# Hermes Control Bridge

Hermes Gateway プロセス内で動く薄い拡張（Python）。Mattermost Server Plugin だけが接続する
ループバック HTTP API を提供する。

責務（SPEC §7.1）は次のとおり。

- セッション解決（`gateway.session.build_session_key()` を唯一の真源として使う。Actor 込み）
- 状態取得（`_peek_session_state` / `_is_session_running` / モデル上書き）
- `/status` の中継（gateway のコマンドディスパッチをそのまま呼ぶ）
- 表示言語の取得（`agent.i18n.get_language()`）
- 排他制御（`_running` レジストリ、`_session_has_compression_in_flight` を含む）
- 冪等記録（メモリ内、保持24時間）

`gateway.session` と `hermes_cli` を直接参照する。Enhancer は使わない。

## エンドポイント

| Method | Path | 内容 |
| --- | --- | --- |
| GET | `/health` | `{"status":"ok","agent_id":"hermes"}` |
| GET | `/agents` | Agent 一覧と Capabilities |
| GET | `/agents/{id}/models` | モデル候補（不透明なキー） |
| GET | `/agents/{id}/language` | gateway の `display.language` |
| GET | `/agents/{id}/status` | `/status` の本文（`channel_id` / `root_post_id` / `user_id` / `chat_type`） |
| GET | `/agents/{id}/session` | セッションの存在・稼働状態・現在モデル |
| POST | `/agents/{id}/session/actions` | `switch_model` / `reset_session` / `compress_session` |

認証は共有サービス認証（Bearer）。トークン未設定ならループバックからの接続だけを信頼する。

## 起動

gateway プロセスでは `register()` の時点で HTTP サーバーを起動する（再起動直後にパネルを
開いても接続できるようにするため）。CLI プロセス（`hermes plugins list` など）では遅延起動し、
18066 を先に取らないようにする。

## 必須条件：`plugins.isolation: in_process`

`pre_gateway_dispatch` フックは live な gateway runner と session store を受け取る。
`plugins.isolation: host` ではこれらがプレースホルダになり、状態が読めなくなる
（Hermes 側 `HOST_DEGRADED_HOOKS`）。本プラグインは in-process 必須。

```
hermes config set plugins.isolation in_process
```

## 導入

```bash
ln -sfn "$(pwd)/bridge" ~/.hermes/plugins/hermes-console-bridge
```

`.env` に共有サービス認証を置く（`openssl rand -hex 32`）。

```
HERMES_CONSOLE_BRIDGE_TOKEN=<32byte hex>
```

任意で次を設定する。

```
HERMES_CONSOLE_BRIDGE_HOST=127.0.0.1   # 既定
HERMES_CONSOLE_BRIDGE_PORT=18066       # 既定
```

Mattermost が Docker・Hermes がホストの場合は `HOST=0.0.0.0` にし、Server Plugin 側からは
`host.docker.internal:18066` を使う。

gateway 再起動後に有効化される（gateway 内から `hermes gateway restart` は不可なので別シェルで）。

## 設計上の約束

- セッションキーは手で組まない。`gateway.session.build_session_key()` を唯一の真源にする
- Actor（`user_id`）は常に渡す。`group_sessions_per_user: true` のためチャンネル直下は
  per-user セッションで、Actor を省くと別セッションに流れる
- 状態取得は何も作らない（`_peek_session_state` / `lookup_by_session_key`）
- 秘密情報（api_key / base_url の token）はブラウザーに返さない

## テスト

```bash
python -m pytest bridge/tests -q -o 'addopts='
```

テストは Hermes ソースを `sys.path` に載せる。場所は環境変数 `HERMES_SRC` で指定でき、
未指定なら `~/.hermes/hermes-agent` を使う。
