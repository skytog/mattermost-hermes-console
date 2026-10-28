"""Session key resolution and state reading.

Never hand-build the key: `gateway.session.build_session_key()` is the single
source of truth (Enhancer convention). Actor (`user_id`) is ALWAYS passed —
channel-root sessions are per-user under group_sessions_per_user, and omitting
the actor silently targets a different session there.
"""

from __future__ import annotations

import json
import os
import time
from typing import Any, Optional

# Built-in providers Hermes knows but that are NOT declared under `providers:`
# (OpenRouter, the ChatGPT/Codex subscription). They are switchable with
# `/model <model> --provider <id>`, so the panel must offer them too — otherwise
# the picker only shows custom endpoints and they look "unregistered".
BUILTIN_PROVIDERS: dict[str, tuple[str, str]] = {
    "openrouter": ("OpenRouter", "openrouter"),
    "openai-codex": ("ChatGPT / Codex", "codex"),
}
BUILTIN_MODEL_LIMIT = 20
_BUILTIN_TTL = 300.0
_BUILTIN_EMPTY_TTL = 60.0
_builtin_cache: dict[str, tuple[float, list[tuple[str, str]]]] = {}


def _credentials_present(provider_id: str) -> bool:
    """Whether Hermes holds a usable credential for a built-in provider."""
    try:
        from hermes_cli.auth import get_auth_status

        status = get_auth_status(provider_id) or {}
        if status.get("logged_in") or status.get("configured"):
            return True
    except Exception:
        pass
    # The api-key status path ignores pool-only credentials (OpenRouter added via
    # `hermes auth add`), so fall back to the credential pool itself.
    try:
        home = os.environ.get("HERMES_HOME") or os.path.expanduser("~/.hermes")
        with open(os.path.join(home, "auth.json"), encoding="utf-8") as fh:
            data = json.load(fh)
        return bool((data.get("credential_pool") or {}).get(provider_id))
    except Exception:
        return False


def _builtin_model_rows(source: str) -> list[tuple[str, str]]:
    if source == "openrouter":
        from hermes_cli.model_catalog import get_curated_openrouter_models

        return list(get_curated_openrouter_models() or [])
    if source == "codex":
        from hermes_cli.codex_models import get_codex_model_ids

        return [(mid, "") for mid in (get_codex_model_ids() or [])]
    return []


def _builtin_candidates(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """Model rows for built-in providers with credentials (cached, capped)."""
    configured = {str(k) for k in (cfg.get("providers") or {})}
    out: list[dict[str, Any]] = []
    now = time.time()
    for provider_id, (label, source) in BUILTIN_PROVIDERS.items():
        if provider_id in configured:
            continue
        cached = _builtin_cache.get(provider_id)
        ttl = _BUILTIN_TTL if cached and cached[1] else _BUILTIN_EMPTY_TTL
        if cached and now - cached[0] < ttl:
            rows = cached[1]
        else:
            rows: list[tuple[str, str]] = []
            if _credentials_present(provider_id):
                try:
                    rows = _builtin_model_rows(source)[:BUILTIN_MODEL_LIMIT]
                except Exception:
                    rows = []
            _builtin_cache[provider_id] = (now, rows)
        for model_id, _desc in rows:
            out.append(
                {
                    "key": f"{provider_id}:{model_id}",
                    "model": model_id,
                    "display_name": model_id,
                    "provider": provider_id,
                    "provider_display_name": label,
                    "context_length": None,
                }
            )
    return out


def _gateway_session_module():
    import gateway.session as gs  # lazy: keeps plugin import cheap

    return gs


def _session_cfg() -> tuple[bool, bool]:
    from hermes_cli.config import load_config

    cfg = load_config()
    return (
        bool(cfg.get("group_sessions_per_user", True)),
        bool(cfg.get("thread_sessions_per_user", False)),
    )


# The Mattermost adapter derives chat_type from the channel type
# (`O` -> channel, `G`/`P` -> group, `D` -> dm) and leaves the THREAD id in
# `thread_id`; `build_session_key` keeps chat_type in the key whenever thread_id
# is real. So the chat_type MUST come from the channel, never be assumed: passing
# "thread" here produced `...:thread:<ch>:<root>`, a phantom session that the
# real conversation never writes to (compress said "not enough conversation").
CHAT_TYPES = {"channel", "group", "dm"}


def session_source(
    channel_id: str,
    root_post_id: Optional[str],
    user_id: Optional[str],
    chat_type: str = "channel",
):
    """The Mattermost SessionSource for a target (+actor), never hand-built elsewhere."""
    gs = _gateway_session_module()
    if chat_type not in CHAT_TYPES:
        chat_type = "channel"
    return gs.SessionSource(
        platform=gs.Platform.MATTERMOST,
        chat_id=channel_id,
        chat_type=chat_type,
        thread_id=root_post_id or None,
        user_id=user_id,
    )


def resolve_session_key(
    channel_id: str,
    root_post_id: Optional[str],
    user_id: Optional[str],
    chat_type: str = "channel",
) -> str:
    """Map a Mattermost target (+actor) to the canonical Hermes session key."""
    gs = _gateway_session_module()
    group_per_user, thread_per_user = _session_cfg()
    source = session_source(channel_id, root_post_id, user_id, chat_type)
    return gs.build_session_key(
        source,
        group_sessions_per_user=group_per_user,
        thread_sessions_per_user=thread_per_user,
        profile=None,
    )


def shared_scope(root_post_id: Optional[str]) -> str:
    """Human-facing label for who shares the session."""
    return "thread" if root_post_id else "channel"


def model_candidates() -> list[dict[str, Any]]:
    """Provider:Model candidates (opaque keys, no secrets).

    Custom endpoints come from `providers:`; built-in providers with credentials
    (OpenRouter, ChatGPT/Codex) are appended so they are selectable too.
    """
    from hermes_cli.config import load_config

    cfg = load_config()
    out: list[dict[str, Any]] = []
    for provider_name, provider in (cfg.get("providers") or {}).items():
        if not isinstance(provider, dict):
            continue
        display_provider = provider.get("name") or provider_name
        models = provider.get("models") or {}
        if isinstance(models, str):
            models = {models: {}}
        for model_name, meta in models.items():
            meta = meta if isinstance(meta, dict) else {}
            out.append(
                {
                    "key": f"{provider_name}:{model_name}",
                    "model": model_name,
                    "display_name": meta.get("name") or model_name,
                    "provider": provider_name,
                    "provider_display_name": display_provider,
                    "context_length": meta.get("context_length"),
                }
            )
    out.extend(_builtin_candidates(cfg))
    return out


def default_model() -> dict[str, Any]:
    from hermes_cli.config import load_config

    cfg = load_config()
    model_cfg = cfg.get("model") or {}
    provider_name = model_cfg.get("provider") or ""
    model_name = model_cfg.get("default") or ""
    display_provider = provider_name
    providers = cfg.get("providers") or {}
    if provider_name in providers and isinstance(providers[provider_name], dict):
        display_provider = providers[provider_name].get("name") or provider_name
    return {
        "key": f"{provider_name}:{model_name}",
        "display_name": model_name,
        "provider_display_name": display_provider,
        "source": "default",
    }
