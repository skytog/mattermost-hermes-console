"""Gateway state reading (read-only).

Reads only what exists: `_peek_session_state`, `_is_session_running`,
`_session_model_overrides`, and the session store. Nothing is created by a read.
"""

from __future__ import annotations

from typing import Any, Optional

from . import session as sess


def current_model(gateway: Any, session_key: str) -> dict[str, Any]:
    """Session override if present, else the configured default."""
    overrides = getattr(gateway, "_session_model_overrides", None) or {}
    override = overrides.get(session_key)
    if override:
        provider_name = override.get("provider", "")
        model_name = override.get("model", "")
        display_provider = provider_name
        try:
            from hermes_cli.config import load_config

            providers = (load_config().get("providers") or {})
            if provider_name in providers and isinstance(providers[provider_name], dict):
                display_provider = providers[provider_name].get("name") or provider_name
        except Exception:
            pass
        return {
            "key": f"{provider_name}:{model_name}",
            "display_name": model_name,
            "provider_display_name": display_provider,
            "source": "session_override",
        }
    return sess.default_model()


def activity(gateway: Any, session_key: str) -> str:
    """idle / running / unknown. Unknown never permits a change operation."""
    try:
        if gateway._is_session_running(session_key):
            return "running"
    except Exception:
        return "unknown"
    return "idle"


def session_exists(session_store: Any, session_key: str) -> bool:
    try:
        return session_store.lookup_by_session_key(session_key) is not None
    except Exception:
        return False


def revision(gateway: Any, session_key: str, exists: bool) -> int:
    """Cheap content-derived revision: changes when model or existence changes.

    Deliberately NOT tied to timestamps or polling counters.
    """
    model = current_model(gateway, session_key)
    payload = f"{model.get('key')}|{int(exists)}|{activity(gateway, session_key)}"
    return abs(hash(payload)) % 100000


def build_state(
    gateway: Any,
    session_store: Any,
    channel_id: str,
    root_post_id: Optional[str],
    user_id: Optional[str],
    chat_type: str = "channel",
) -> dict[str, Any]:
    """Full read-only state payload for one target."""
    session_key = sess.resolve_session_key(channel_id, root_post_id, user_id, chat_type)
    exists = session_exists(session_store, session_key)
    act = activity(gateway, session_key)
    allowed = ["switch_model", "reset_session", "compress_session"] if act == "idle" else []

    return {
        "target": {"channel_id": channel_id, "root_post_id": root_post_id},
        "session_exists": exists,
        "generation": 0,
        "revision": revision(gateway, session_key, exists),
        "agent_connectivity": "reachable",
        "activity": act,
        "shared_scope": sess.shared_scope(root_post_id),
        "model": current_model(gateway, session_key),
        "context": {
            "used_tokens": None,
            "limit_tokens": None,
            "percent": None,
            "measurement": "unavailable",
        },
        "capabilities": {
            "switch_model": True,
            "reset_session": True,
            "compress_session": True,
        },
        "allowed_actions": allowed,
        "session_key": session_key,
    }
