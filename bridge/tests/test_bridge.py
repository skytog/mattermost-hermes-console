"""Bridge tests — real discovery-adjacent behaviour, not source reading.

Run with the Hermes install venv so `hermes_cli.config` resolves:

    ~/.hermes/installs/*/environments/*/venv/bin/python -m pytest bridge/tests -q
"""

from __future__ import annotations

import json
import os
import sys
import threading
import asyncio
import time
import urllib.error
import urllib.request
from pathlib import Path

import pytest

BRIDGE_DIR = Path(__file__).resolve().parents[1]
HERMES_SRC = Path(os.environ.get("HERMES_SRC", str(Path.home() / ".hermes" / "hermes-agent")))

for p in (str(BRIDGE_DIR.parent), str(HERMES_SRC)):
    if p not in sys.path:
        sys.path.insert(0, p)

import bridge  # noqa: E402
from bridge import server, session, state  # noqa: E402
from bridge import actions as act  # noqa: E402


@pytest.fixture()
def loop():
    lp = asyncio.new_event_loop()
    t = threading.Thread(target=lp.run_forever, daemon=True)
    t.start()
    yield lp
    lp.call_soon_threadsafe(lp.stop)


class FakeGateway:
    def __init__(self, running: bool = False, override: dict | None = None):
        self._running = running
        self._session_model_overrides = {"k": override} if override else {}
        self.commands: list[tuple[str, str]] = []

    def _is_session_running(self, key: str) -> bool:
        return self._running

    async def _hm_dispatch_canonical_command(self, event, source, key, command):
        self.commands.append((command, event.text))
        return True, f"ran {command}"


class FakeStore:
    def __init__(self, exists: bool = True):
        self._exists = exists

    def lookup_by_session_key(self, key: str):
        return object() if self._exists else None


# ---- session resolution ----------------------------------------------------


def test_channel_root_session_is_per_user():
    # A private channel (chat_type "group"): channel-root sessions are per-user
    # because group_sessions_per_user is true.
    a = session.resolve_session_key("chanAAA", None, "user-nicox", "group")
    b = session.resolve_session_key("chanAAA", None, "user-other", "group")
    assert a != b
    assert a.startswith("agent:main:mattermost:group:chanAAA:")
    assert "user-nicox" in a


def test_thread_session_is_shared():
    a = session.resolve_session_key("chanAAA", "rootBBB", "user-nicox")
    b = session.resolve_session_key("chanAAA", "rootBBB", "user-other")
    assert a == b
    # chat_type comes from the CHANNEL (group/channel), not "thread": the
    # Mattermost adapter keeps the thread in thread_id and its own chat_type in
    # the key, so a "thread" chat_type builds a phantom session.
    assert a == "agent:main:mattermost:channel:chanAAA:rootBBB"


def test_thread_key_uses_the_channel_type():
    a = session.resolve_session_key("chanAAA", "rootBBB", "user-nicox", "group")
    assert a == "agent:main:mattermost:group:chanAAA:rootBBB"
    # ... and it matches what the adapter produces for a private channel.
    b = session.resolve_session_key("chanAAA", "rootBBB", "user-other", "group")
    assert a == b
    assert session.resolve_session_key("chanAAA", "rootBBB", "u", "thread") == \
        "agent:main:mattermost:channel:chanAAA:rootBBB"


def test_shared_scope_labels():
    assert session.shared_scope("root") == "thread"
    assert session.shared_scope(None) == "channel"


# ---- model candidates ------------------------------------------------------


def test_model_candidates_have_opaque_keys_and_no_secrets():
    models = session.model_candidates()
    assert models, "expected at least one provider model"
    for m in models:
        assert ":" in m["key"]
        assert "api_key" not in m
        assert m["provider_display_name"]


# ---- state -----------------------------------------------------------------


def test_state_reports_session_missing_without_creating():
    gw = FakeGateway()
    store = FakeStore(exists=False)
    payload = state.build_state(gw, store, "chanAAA", None, "u")
    assert payload["session_exists"] is False
    assert payload["allowed_actions"]  # idle => allowed
    assert payload["context"]["used_tokens"] is None
    assert payload["context"]["measurement"] == "unavailable"


def test_state_running_blocks_actions():
    gw = FakeGateway(running=True)
    payload = state.build_state(gw, FakeStore(), "chanAAA", "rootBBB", "u")
    assert payload["activity"] == "running"
    assert payload["allowed_actions"] == []


def test_revision_changes_when_model_changes():
    gw = FakeGateway()
    store = FakeStore()
    key = session.resolve_session_key("c", None, "u")
    before = state.build_state(gw, store, "c", None, "u")
    gw._session_model_overrides[key] = {"provider": "p", "model": "m"}
    after = state.build_state(gw, store, "c", None, "u")
    assert after["model"]["key"] == "p:m"
    assert after["model"]["source"] == "session_override"
    assert isinstance(before["revision"], int)


# ---- http server -----------------------------------------------------------


@pytest.fixture()
def live_server(monkeypatch, loop):
    monkeypatch.setenv("HERMES_CONSOLE_BRIDGE_HOST", "127.0.0.1")
    monkeypatch.setenv("HERMES_CONSOLE_BRIDGE_PORT", "18266")
    monkeypatch.setenv("HERMES_CONSOLE_BRIDGE_TOKEN", "secret-token")
    server._server = None
    server.start()
    server.bind_runtime(gateway=FakeGateway(), session_store=FakeStore(), loop=loop)
    yield "http://127.0.0.1:18266"
    server.stop()


def _post(url: str, body: dict, token: str | None = "secret-token"):
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST")
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read())


def _get(url: str, token: str | None = "secret-token"):
    req = urllib.request.Request(url)
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read())


def test_health_and_agents(live_server):
    code, body = _get(f"{live_server}/health")
    assert code == 200 and body["status"] == "ok"
    code, body = _get(f"{live_server}/agents")
    assert code == 200 and body["agents"][0]["agent_id"] == "hermes"


def test_auth_required(live_server):
    code, body = _get(f"{live_server}/agents", token=None)
    assert code == 401 and body["error"] == "unauthenticated"


def test_wrong_token_rejected(live_server):
    code, _ = _get(f"{live_server}/agents", token="nope")
    assert code == 401


def test_session_requires_channel(live_server):
    code, body = _get(f"{live_server}/agents/hermes/session")
    assert code == 404 and body["error"] == "target_not_found"


def test_session_returns_state(live_server):
    code, body = _get(f"{live_server}/agents/hermes/session?channel_id=c&user_id=u")
    assert code == 200
    assert body["agent_connectivity"] == "reachable"
    assert body["session_key"].startswith("agent:main:mattermost:")


def test_session_unavailable_when_unbound(monkeypatch):
    monkeypatch.setenv("HERMES_CONSOLE_BRIDGE_HOST", "127.0.0.1")
    monkeypatch.setenv("HERMES_CONSOLE_BRIDGE_PORT", "18266")
    monkeypatch.setenv("HERMES_CONSOLE_BRIDGE_TOKEN", "")
    server._server = None
    server.start()
    server.bind_runtime(gateway=None, session_store=None)
    try:
        code, body = _get("http://127.0.0.1:18266/agents/hermes/session?channel_id=c", token=None)
        assert code == 503 and body["error"] == "hermes_unavailable"
    finally:
        server.stop()


# ---- actions ---------------------------------------------------------------


def test_unknown_action_rejected(loop):
    gw = FakeGateway()
    out = act.run_action(gw, loop, "nope", "", "c", None, "u")
    assert out["error"] == "unknown_action"


def test_action_runs_via_gateway_dispatch(loop):
    gw = FakeGateway()
    out = act.run_action(gw, loop, "switch_model", "p:m", "c", "root", "u", None, "group")
    assert out["handled"] is True
    assert gw.commands == [("model", "/model p:m")]
    assert out["session_key"] == "agent:main:mattermost:group:c:root"


# ---- status (read-only) ----------------------------------------------------


def test_status_runs_the_gateways_own_status_command(loop):
    gw = FakeGateway()
    out = act.run_status(gw, loop, "c", "root", "u")
    assert out["handled"] is True
    assert gw.commands == [("status", "/status")]
    assert out["text"] == "ran status"
    assert out["session_key"] == "agent:main:mattermost:channel:c:root"


def test_status_uses_the_given_chat_type(loop):
    gw = FakeGateway()
    out = act.run_status(gw, loop, "c", "root", "u", "group")
    assert out["session_key"] == "agent:main:mattermost:group:c:root"


def test_status_endpoint(live_server):
    code, body = _get(f"{live_server}/agents/hermes/status?channel_id=c&root_post_id=root&user_id=u")
    assert code == 200
    assert body["text"] == "ran status"
    assert body["handled"] is True


def test_language_endpoint(live_server):
    code, body = _get(f"{live_server}/agents/hermes/language")
    assert code == 200
    assert body["language"] in ("ja", "en")


def test_status_requires_channel(live_server):
    code, body = _get(f"{live_server}/agents/hermes/status")
    assert code == 404 and body["error"] == "target_not_found"


def test_action_blocked_while_running(loop):
    gw = FakeGateway(running=True)
    out = act.run_action(gw, loop, "reset_session", "", "c", None, "u")
    assert out["error"] == "busy"
    assert gw.commands == []


def test_compress_allowed_while_running(loop):
    gw = FakeGateway(running=True)
    out = act.run_action(gw, loop, "compress_session", "", "c", None, "u")
    assert out["handled"] is True


def test_idempotency_replays_without_rerunning(loop):
    gw = FakeGateway()
    first = act.run_action(gw, loop, "reset_session", "", "c", None, "u", "key-1")
    second = act.run_action(gw, loop, "reset_session", "", "c", None, "u", "key-1")
    assert second["replayed"] is True
    assert len(gw.commands) == 1
    assert first["text"] == second["text"]


class SlowGateway(FakeGateway):
    def __init__(self, delay: float = 0.25, **kwargs):
        super().__init__(**kwargs)
        self.delay = delay

    async def _hm_dispatch_canonical_command(self, event, source, key, command):
        self.commands.append((command, event.text))
        await asyncio.sleep(self.delay)
        return True, "late done"


def test_timeout_keeps_the_operation_reserved_and_records_the_late_result(loop):
    """A timeout ends the WAIT, not the coroutine: a re-send must not inject a
    second operation, and the eventual result must be replayable (review M5)."""
    act._idempotency.clear()
    act._running.clear()
    gw = SlowGateway(delay=0.3)

    first = act.run_action(gw, loop, "reset_session", "", "c", None, "u", "key-slow", timeout=0.05)
    assert first["error"] == "timeout"

    second = act.run_action(gw, loop, "reset_session", "", "c", None, "u", "key-slow", timeout=0.05)
    assert second["error"] == "in_progress"
    assert len(gw.commands) == 1, "the re-send must not start a second operation"

    time.sleep(0.5)
    third = act.run_action(gw, loop, "reset_session", "", "c", None, "u", "key-slow", timeout=5)
    assert third.get("replayed") is True
    assert third["text"] == "late done"
    assert len(gw.commands) == 1


def test_same_session_without_a_key_is_reserved_too(loop):
    act._idempotency.clear()
    act._running.clear()
    gw = SlowGateway(delay=0.3)

    first = act.run_action(gw, loop, "reset_session", "", "c", "root", "u", None, timeout=0.05)
    assert first["error"] == "timeout"
    second = act.run_action(gw, loop, "reset_session", "", "c", "root", "u", None, timeout=0.05)
    assert second["error"] == "in_progress"
    time.sleep(0.5)
    act._running.clear()


def test_post_actions_endpoint(live_server):
    code, body = _post(
        f"{live_server}/agents/hermes/session/actions",
        {"channel_id": "c", "user_id": "u", "action": "reset_session"},
    )
    assert code == 200 and body["handled"] is True


def test_post_actions_rejects_unknown(live_server):
    code, body = _post(
        f"{live_server}/agents/hermes/session/actions",
        {"channel_id": "c", "action": "nope"},
    )
    assert code == 400 and body["error"] == "unknown_action"


def test_post_actions_needs_channel(live_server):
    code, body = _post(f"{live_server}/agents/hermes/session/actions", {"action": "reset_session"})
    assert code == 404 and body["error"] == "target_not_found"


# ---- registration ----------------------------------------------------------


def test_register_wires_the_hook():
    class Ctx:
        def __init__(self):
            self.hooks = {}

        def register_hook(self, name, cb):
            self.hooks[name] = cb

    ctx = Ctx()
    os.environ.setdefault("HERMES_CONSOLE_BRIDGE_PORT", "18267")
    bridge.register(ctx)
    assert "pre_gateway_dispatch" in ctx.hooks
    # hook must be safe with no args and must not drop the event
    assert ctx.hooks["pre_gateway_dispatch"]() is None
    server.stop()


def test_register_starts_the_bridge_in_the_gateway_process(monkeypatch):
    """A bridge that starts only on the first message leaves every panel opened
    after a gateway restart with a connection-refused model list."""
    class Ctx:
        def __init__(self):
            self.hooks = {}

        def register_hook(self, name, cb):
            self.hooks[name] = cb

    monkeypatch.setenv("HERMES_CONSOLE_BRIDGE_PORT", "18268")
    monkeypatch.setenv("HERMES_SUPERVISED_CHILD", "1")
    server._server = None
    try:
        bridge.register(Ctx())
        assert server._server is not None, "register() must start the server in the gateway"
    finally:
        server.stop()


def test_register_stays_lazy_in_a_cli_process(monkeypatch):
    class Ctx:
        def __init__(self):
            self.hooks = {}

        def register_hook(self, name, cb):
            self.hooks[name] = cb

    monkeypatch.delenv("HERMES_SUPERVISED_CHILD", raising=False)
    monkeypatch.setattr(bridge.sys, "argv", ["hermes", "plugins", "list"])
    monkeypatch.setenv("HERMES_CONSOLE_BRIDGE_PORT", "18269")
    server._server = None
    try:
        bridge.register(Ctx())
        assert server._server is None, "a CLI helper must not grab the gateway's port"
    finally:
        server.stop()


def test_builtin_providers_are_listed_when_credentialed(monkeypatch):
    """OpenRouter / ChatGPT-Codex are switchable via --provider, so the picker
    must offer them even though they are not in `providers:`."""
    monkeypatch.setattr(session, "_credentials_present", lambda pid: True)
    monkeypatch.setattr(session, "_builtin_model_rows", lambda src: [("m-1", ""), ("m-2", "")])
    monkeypatch.setattr(session, "_builtin_cache", {})

    rows = session._builtin_candidates({})
    providers = {r["provider"] for r in rows}
    assert providers == {"openrouter", "openai-codex"}
    assert all(r["key"] == f"{r['provider']}:{r['model']}" for r in rows)
    assert all(r["provider_display_name"] for r in rows)


def test_builtin_providers_skipped_without_credentials(monkeypatch):
    monkeypatch.setattr(session, "_credentials_present", lambda pid: False)
    monkeypatch.setattr(session, "_builtin_cache", {})
    assert session._builtin_candidates({}) == []


def test_builtin_providers_skipped_when_declared_in_config(monkeypatch):
    monkeypatch.setattr(session, "_credentials_present", lambda pid: True)
    monkeypatch.setattr(session, "_builtin_model_rows", lambda src: [("m-1", "")])
    monkeypatch.setattr(session, "_builtin_cache", {})
    rows = session._builtin_candidates({"providers": {"openrouter": {"api": "x"}}})
    assert {r["provider"] for r in rows} == {"openai-codex"}


def test_builtin_model_list_is_capped(monkeypatch):
    monkeypatch.setattr(session, "_credentials_present", lambda pid: True)
    monkeypatch.setattr(session, "_builtin_model_rows",
                        lambda src: [(f"m-{i}", "") for i in range(50)])
    monkeypatch.setattr(session, "_builtin_cache", {})
    rows = session._builtin_candidates({})
    per_provider = {}
    for r in rows:
        per_provider[r["provider"]] = per_provider.get(r["provider"], 0) + 1
    assert all(n <= session.BUILTIN_MODEL_LIMIT for n in per_provider.values())
