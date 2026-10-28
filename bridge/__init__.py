"""Hermes Console Bridge — Hermes plugin entry point.

Runs in-process inside the gateway. Registers one hook to capture the live
gateway runner + session store, and starts the loopback HTTP API.

Read-only (Step 2). Operations land in Step 3.
"""

from __future__ import annotations

import logging
import os
import sys

from . import server

logger = logging.getLogger(__name__)

_started = False


def _is_gateway_process() -> bool:
    """True only in the long-lived gateway, never in a CLI helper.

    `hermes plugins list/validate` also runs register(); letting it bind 18066
    would make the real gateway fail with EADDRINUSE (the original reason the
    server was started lazily from the hook).
    """
    if os.environ.get("HERMES_SUPERVISED_CHILD") == "1":
        return True
    argv = [a.lower() for a in sys.argv[1:]]
    return "gateway" in argv and any(a in argv for a in ("run", "start", "serve"))


def _on_pre_gateway_dispatch(event=None, gateway=None, session_store=None, **_kwargs):
    """Capture the live runtime refs, then start the server (gateway process only)."""
    import asyncio

    try:
        loop = asyncio.get_running_loop()
    except RuntimeError:
        loop = None
    server.bind_runtime(gateway=gateway, session_store=session_store, loop=loop)
    _ensure_started()


def _ensure_started() -> None:
    global _started
    if _started and server._server is not None:
        return
    try:
        server.start()
        _started = True
    except OSError as exc:
        # A short-lived CLI process (`hermes plugins list/validate`) can already hold
        # the port; the gateway keeps it. Harmless — retry on the next hook.
        if getattr(exc, "errno", None) == 98:
            logger.debug("hermes-console-bridge: port busy (another hermes process holds it)")
        else:
            logger.warning("hermes-console-bridge: server start failed", exc_info=True)
    except Exception:
        logger.warning("hermes-console-bridge: server start failed", exc_info=True)


def register(ctx) -> None:
    """Plugin loader entry point.

    In the gateway, start the HTTP server immediately: a bridge that only starts
    on the first inbound message leaves every panel opened after a restart with
    "モデル一覧の取得に失敗" (dial tcp 127.0.0.1:18066). CLI processes stay lazy.
    """
    ctx.register_hook("pre_gateway_dispatch", _on_pre_gateway_dispatch)
    if _is_gateway_process():
        _ensure_started()

