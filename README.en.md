# Mattermost Hermes Console

**English | [日本語](README.md)**

A Mattermost plugin that puts Hermes conversation settings and status into a **post panel** inside a
channel or thread — switch models, reset the conversation, compress context, and check status without
typing commands.

- plugin_id: `com.nicox.hermes-console`
- Version: `0.3.3` (tested on Mattermost v11.11.0)
- Targets: linux-amd64 (Mattermost Server Plugin) + Web App Plugin

## What it does

| Action | Entry point |
| --- | --- |
| Switch model (provider → model → apply) | Panel / `/hermes-model` |
| Reset the conversation | Panel / `/hermes-reset` |
| Compress the conversation context | Panel / `/hermes-compress` |
| Show status, exactly as `/status` | `/hermes-status` (posts the gateway's `/status` text verbatim) |
| Open the panel | App Bar / post menu "Open Hermes Console" / `/hermes` |

The panel is a **normal post** in the channel/thread, not an RHS pane. Actions rewrite that same post.

## Layout

```
docs/     spec and review record
server/   Mattermost Server Plugin (Go)   ← auth, allowlist, relay, panel rendering
webapp/   Mattermost Web App Plugin (React/TS) ← App Bar and post-menu entries
bridge/   Hermes Control Bridge (Python, in-process) ← state, exclusivity, idempotency
```

- The bridge runs inside the Hermes gateway process (`~/.hermes/plugins/hermes-console-bridge`).
- In the gateway process it starts its HTTP server at `register()`; CLI processes stay lazy.
- Changing the bridge requires **a gateway restart**.

## Configuration (System Console → Plugins → Hermes Console)

| Key | Meaning |
| --- | --- |
| `BridgeURL` | Bridge endpoint (default `http://127.0.0.1:18066`) |
| `BridgeToken` | Shared-service token, server-side only |
| `AllowedUsers` | Comma-separated user IDs. Empty = nobody can operate |
| `AllowedChannels` | Comma-separated channel IDs. Empty = nobody can operate |
| `HermesBotID` | Bot user ID of the existing Hermes integration |

Keep `HERMES_CONSOLE_BRIDGE_TOKEN` in `~/.hermes/.env` equal to `BridgeToken`.

## Language

The panel and command descriptions follow the gateway's **`display.language`** (ja / en, fallback `en`),
so the console never contradicts the gateway's own `/status` text.

```bash
hermes config set display.language ja   # switch the console to Japanese
```

The value is cached in the gateway process: **restart the gateway** after changing it.

## Build and deploy

```bash
cd server && GOOS=linux GOARCH=amd64 go build -trimpath -o dist/plugin-linux-amd64 .
# bundle plugin.json + server/dist/plugin-linux-amd64 + webapp/dist/main.js into a tar.gz,
# upload it to /api/v4/plugins?force=true and enable it
```

Rebuild `webapp/dist/main.js` with esbuild after changing the webapp.

## Tests

```bash
cd server && go test ./...          # 48 pass
pytest bridge/tests                 # 36 pass
```

## License

MIT (see [LICENSE](LICENSE)).

## Spec and review

- [Spec](docs/SPEC.md), v0.3 (aligned with the implementation)
- [Review record](docs/REVIEW.md)
