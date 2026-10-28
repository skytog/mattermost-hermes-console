"""Gateway command execution for Hermes Console operations (Step 3).

Reuse the gateway's OWN slash-command dispatch instead of re-implementing
switch/reset/compress: `_hm_dispatch_canonical_command` is the exact entry the
platform adapters use, so behaviour matches typing the command by hand.

Nothing is hand-copied from the Enhancer.
"""

from __future__ import annotations

import asyncio
import threading
import time
from typing import Any, Optional

from . import session as sess
from . import state as st

# action id -> gateway command word
ACTIONS: dict[str, str] = {
    "switch_model": "model",
    "reset_session": "reset",
    "compress_session": "compress",
}

# compress may run while the agent is mid-turn; switch/reset require idle.
ALLOW_WHILE_RUNNING = {"compress_session"}

# Compress summarizes the whole transcript with the aux model, so it needs
# minutes, not the 90s default. Kept below the plugin's 10-minute client bound.
COMPRESS_TIMEOUT = 540.0

_idempotency: dict[str, tuple[float, dict[str, Any]]] = {}
_IDEM_TTL = 24 * 60 * 60

# Operations that are still running. A timeout only ends the WAIT, not the
# coroutine: without this registry a second request could re-inject the same
# operation and the late result was never recorded (review M5).
_running: dict[str, float] = {}
_lock = threading.Lock()


def _prune() -> None:
    now = time.time()
    with _lock:
        for key in [k for k, (ts, _) in _idempotency.items() if now - ts > _IDEM_TTL]:
            _idempotency.pop(key, None)
        for key in [k for k, ts in _running.items() if now - ts > _IDEM_TTL]:
            _running.pop(key, None)


def _op_key(idempotency_key: Optional[str], session_key: str, action: str) -> str:
    """Stable identity for one operation: the caller's key, else the session+action."""
    return idempotency_key or f"{session_key}:{action}"


def _source(channel_id: str, root_post_id: Optional[str], user_id: Optional[str],
            chat_type: str = "channel"):
    from gateway.session import Platform, SessionSource

    return SessionSource(
        platform=Platform.MATTERMOST,
        chat_id=channel_id,
        chat_type=chat_type if chat_type in sess.CHAT_TYPES else "channel",
        thread_id=root_post_id or None,
        user_id=user_id,
    )


def _event(source, text: str, user_id: Optional[str]):
    from gateway.platforms.event import MessageEvent

    return MessageEvent(text=text, source=source, user_id=user_id)


def run_status(
    gateway: Any,
    loop: Any,
    channel_id: str,
    root_post_id: Optional[str],
    user_id: Optional[str],
    chat_type: str = "channel",
    timeout: float = 20.0,
) -> dict[str, Any]:
    """Return the gateway's OWN /status text for a target. Read-only.

    Dispatched through the same canonical command entry the adapters use, so the
    text is exactly what typing `/status` produces (model, provider, context,
    tokens, running state) — never a re-implementation.
    """
    session_key = sess.resolve_session_key(channel_id, root_post_id, user_id, chat_type)
    source = _source(channel_id, root_post_id, user_id, chat_type)
    event = _event(source, "/status", user_id)
    coro = gateway._hm_dispatch_canonical_command(event, source, session_key, "status")
    future = asyncio.run_coroutine_threadsafe(coro, loop)
    try:
        handled, result = future.result(timeout=timeout)
    except TimeoutError:
        return {"error": "timeout", "message": "status timed out"}
    return {
        "handled": bool(handled),
        "text": (result or "").strip(),
        "session_key": session_key,
    }


def run_action(
    gateway: Any,
    loop: Any,
    action: str,
    args: str,
    channel_id: str,
    root_post_id: Optional[str],
    user_id: Optional[str],
    idempotency_key: Optional[str] = None,
    chat_type: str = "channel",
    timeout: float = 90.0,
) -> dict[str, Any]:
    """Run one operation through the gateway. Blocking (called from a thread)."""
    if action not in ACTIONS:
        return {"error": "unknown_action", "message": f"unknown action: {action}"}

    if action == "compress_session" and timeout < COMPRESS_TIMEOUT:
        timeout = COMPRESS_TIMEOUT

    _prune()
    session_key = sess.resolve_session_key(channel_id, root_post_id, user_id, chat_type)
    key = _op_key(idempotency_key, session_key, action)

    with _lock:
        hit = _idempotency.get(key)
        if hit is not None:
            return {**hit[1], "replayed": True}
        if key in _running:
            # The previous call timed out but its coroutine is still running.
            # Refer the caller to that operation instead of injecting another.
            return {
                "error": "in_progress",
                "message": "operation is still running",
                "operation_id": key,
            }
        _running[key] = time.time()

    # Exclusivity: never mutate a session mid-turn (compress is the exception).
    activity = st.activity(gateway, session_key)
    if activity == "running" and action not in ALLOW_WHILE_RUNNING:
        with _lock:
            _running.pop(key, None)
        return {"error": "busy", "message": "session is running", "activity": activity}

    command = ACTIONS[action]
    source = _source(channel_id, root_post_id, user_id, chat_type)
    text = f"/{command} {args}".strip()
    event = _event(source, text, user_id)

    coro = gateway._hm_dispatch_canonical_command(event, source, session_key, command)
    future = asyncio.run_coroutine_threadsafe(coro, loop)
    try:
        handled, result = future.result(timeout=timeout)
    except TimeoutError:
        # Outcome unknown: do NOT auto-retry (spec 7.3). Keep the reservation and
        # record the result when the coroutine actually finishes.
        def _record(fut: Any) -> None:
            try:
                done_handled, done_result = fut.result()
            except Exception as exc:  # noqa: BLE001 - recorded, never raised
                late = {"error": "internal_failure", "message": str(exc)}
            else:
                late = {
                    "action": action,
                    "handled": bool(done_handled),
                    "text": (done_result or "").strip(),
                    "session_key": session_key,
                }
            with _lock:
                _idempotency[key] = (time.time(), late)
                _running.pop(key, None)

        future.add_done_callback(_record)
        return {"error": "timeout", "message": "operation timed out", "operation_id": key}

    payload = {
        "action": action,
        "handled": bool(handled),
        "text": (result or "").strip(),
        "session_key": session_key,
    }
    with _lock:
        _idempotency[key] = (time.time(), payload)
        _running.pop(key, None)
    return payload
