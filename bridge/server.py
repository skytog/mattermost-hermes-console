"""Loopback HTTP API for the Hermes Console Bridge (stdlib only, threaded).

Read-only endpoints for Step 2. The Mattermost Server Plugin is the only client.
Auth is a shared service token (Bearer) compared in constant time.
"""

from __future__ import annotations

import hmac
import json
import logging
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Optional
from urllib.parse import parse_qs, urlparse

from . import actions as act
from . import session as sess
from . import state as st

logger = logging.getLogger(__name__)

AGENT_ID = "hermes"
AGENT_DISPLAY_NAME = "Hermes"

_runtime: dict[str, Any] = {"gateway": None, "session_store": None, "loop": None}
_server: Optional[ThreadingHTTPServer] = None
_thread: Optional[threading.Thread] = None
_lock = threading.Lock()


def bind_runtime(gateway: Any = None, session_store: Any = None, loop: Any = None) -> None:
    """Bind the live runtime refs. Unconditional assign, so None clears."""
    with _lock:
        _runtime["gateway"] = gateway
        _runtime["session_store"] = session_store
        _runtime["loop"] = loop


def _bind_host() -> str:
    return os.getenv("HERMES_CONSOLE_BRIDGE_HOST", "127.0.0.1")


def _bind_port() -> int:
    return int(os.getenv("HERMES_CONSOLE_BRIDGE_PORT", "18066"))


def _token() -> str:
    return os.getenv("HERMES_CONSOLE_BRIDGE_TOKEN", "")


def _ensure_runtime_bound() -> None:
    """Bind the live gateway runner on first use.

    register() starts the HTTP server before the gateway runner exists, so a
    panel opened right after a gateway restart would otherwise get 503 until the
    first inbound message fired `pre_gateway_dispatch`. Inside the gateway
    `gateway.run` is already imported, so this is a cheap pointer read; in tests
    the ref is None and the explicit bind stays authoritative.
    """
    if _runtime["gateway"] is not None and _runtime["session_store"] is not None:
        return
    import sys

    mod = sys.modules.get("gateway.run")
    if mod is None:
        # Not inside the gateway (a CLI/test process), or the runner module has
        # not loaded yet: the explicit bind from the hook stays authoritative.
        return
    try:
        runner = getattr(mod, "_gateway_runner_ref", lambda: None)()
    except Exception:
        return
    if runner is None:
        return
    store = getattr(runner, "session_store", None)
    if store is None:
        return
    with _lock:
        if _runtime["gateway"] is None:
            _runtime["gateway"] = runner
        if _runtime["session_store"] is None:
            _runtime["session_store"] = store


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    # ---- plumbing -------------------------------------------------------

    def log_message(self, format, *args):  # quieter default
        logger.debug("bridge: " + format, *args)

    def _send(self, code: int, payload: dict) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _error(self, code: int, error_code: str, message: str = "") -> None:
        self._send(code, {"error": error_code, "message": message})

    def _authorized(self) -> bool:
        expected = _token()
        if not expected:
            return True  # unset token = loopback-only trust; warn at startup
        header = self.headers.get("Authorization", "")
        parts = header.split(None, 1)
        if len(parts) != 2 or parts[0].lower() != "bearer":
            return False
        return hmac.compare_digest(parts[1].strip(), expected)

    # ---- routing --------------------------------------------------------

    def do_GET(self) -> None:
        if not self._authorized():
            self._error(401, "unauthenticated")
            return

        url = urlparse(self.path)
        path = url.path.rstrip("/")
        query = parse_qs(url.query)
        _ensure_runtime_bound()

        if path == "/health":
            self._send(200, {"status": "ok", "agent_id": AGENT_ID})
            return

        if path == "/agents":
            self._send(
                200,
                {
                    "agents": [
                        {
                            "agent_id": AGENT_ID,
                            "display_name": AGENT_DISPLAY_NAME,
                            "capabilities": {
                                "switch_model": True,
                                "reset_session": True,
                                "compress_session": True,
                            },
                        }
                    ]
                },
            )
            return

        if path == f"/agents/{AGENT_ID}/models":
            self._send(200, {"models": sess.model_candidates()})
            return

        if path == f"/agents/{AGENT_ID}/language":
            # The console renders in the gateway's display language so it never
            # contradicts the gateway's own /status text.
            try:
                from agent.i18n import get_language

                lang = get_language() or "en"
            except Exception:
                logger.debug("bridge: language lookup failed", exc_info=True)
                lang = "en"
            self._send(200, {"language": lang})
            return

        if path == f"/agents/{AGENT_ID}/status":
            gateway = _runtime["gateway"]
            loop = _runtime["loop"]
            if gateway is None or loop is None:
                self._error(503, "hermes_unavailable", "gateway runtime not bound yet")
                return
            channel_id = (query.get("channel_id") or [""])[0]
            if not channel_id:
                self._error(404, "target_not_found", "channel_id is required")
                return
            try:
                payload = act.run_status(
                    gateway,
                    loop,
                    channel_id,
                    (query.get("root_post_id") or [None])[0] or None,
                    (query.get("user_id") or [None])[0] or None,
                    (query.get("chat_type") or ["channel"])[0],
                )
            except Exception as exc:
                logger.warning("bridge: status failed", exc_info=True)
                self._error(500, "internal_error", str(exc))
                return
            if "error" in payload:
                code = {"timeout": 504}.get(payload["error"], 500)
                self._send(code, payload)
                return
            self._send(200, payload)
            return

        if path == f"/agents/{AGENT_ID}/session":
            gateway = _runtime["gateway"]
            session_store = _runtime["session_store"]
            if gateway is None or session_store is None:
                self._error(503, "hermes_unavailable", "gateway runtime not bound yet")
                return
            channel_id = (query.get("channel_id") or [""])[0]
            if not channel_id:
                self._error(404, "target_not_found", "channel_id is required")
                return
            root_post_id = (query.get("root_post_id") or [None])[0] or None
            user_id = (query.get("user_id") or [None])[0] or None
            try:
                payload = st.build_state(
                    gateway, session_store, channel_id, root_post_id, user_id,
                    (query.get("chat_type") or ["channel"])[0],
                )
            except Exception as exc:
                logger.warning("bridge: session state failed", exc_info=True)
                self._error(500, "internal_error", str(exc))
                return
            self._send(200, payload)
            return

        self._error(404, "not_found", path)

    def do_POST(self) -> None:  # Step 3 operations
        if not self._authorized():
            self._error(401, "unauthenticated")
            return

        url = urlparse(self.path)
        path = url.path.rstrip("/")
        _ensure_runtime_bound()

        if path == f"/agents/{AGENT_ID}/session/actions":
            gateway = _runtime["gateway"]
            loop = _runtime["loop"]
            if gateway is None or loop is None:
                self._error(503, "hermes_unavailable", "gateway runtime not bound yet")
                return
            length = int(self.headers.get("Content-Length") or 0)
            raw = self.rfile.read(length) if length else b"{}"
            try:
                body = json.loads(raw or b"{}")
            except Exception:
                self._error(400, "bad_request", "invalid JSON body")
                return
            channel_id = str(body.get("channel_id") or "")
            if not channel_id:
                self._error(404, "target_not_found", "channel_id is required")
                return
            action = str(body.get("action") or "")
            if action not in act.ACTIONS:
                self._error(400, "unknown_action", f"unknown action: {action}")
                return
            try:
                payload = act.run_action(
                    gateway,
                    loop,
                    action,
                    str(body.get("args") or ""),
                    channel_id,
                    body.get("root_post_id") or None,
                    body.get("user_id") or None,
                    body.get("idempotency_key") or None,
                    str(body.get("chat_type") or "channel"),
                )
            except Exception as exc:
                logger.warning("bridge: action failed", exc_info=True)
                self._error(500, "internal_error", str(exc))
                return
            if "error" in payload:
                # busy / in_progress -> 409, timeout -> 504 (unknown outcome,
                # never auto-retried).
                code = {
                    "busy": 409,
                    "in_progress": 409,
                    "timeout": 504,
                    "unknown_action": 400,
                }.get(payload["error"], 400)
                self._send(code, payload)
                return
            self._send(200, payload)
            return

        self._error(404, "not_found", path)


def start() -> None:
    """Start the server thread once."""
    global _server, _thread
    with _lock:
        if _server is not None:
            return
        _server = ThreadingHTTPServer((_bind_host(), _bind_port()), _Handler)
        _thread = threading.Thread(target=_server.serve_forever, name="hermes-console-bridge", daemon=True)
        _thread.start()
        logger.info(
            "hermes-console-bridge listening on http://%s:%s%s",
            _bind_host(),
            _bind_port(),
            "" if _token() else " (token unset)",
        )


def stop() -> None:
    global _server, _thread
    with _lock:
        if _server is None:
            return
        _server.shutdown()
        _server.server_close()
        _server = None
        _thread = None
