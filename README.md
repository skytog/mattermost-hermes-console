# Mattermost Hermes Console

**[English](README.en.md) | 日本語**

Mattermost の**投稿パネル**から Hermes の会話設定と状態を操作するプラグイン。
モデル切替、会話リセット、コンテキスト圧縮、状態確認を、コマンドを手入力せずに行える。

- plugin_id: `com.nicox.hermes-console`
- バージョン: `0.3.3`（Mattermost v11.11.0 で動作確認）
- 対応: linux-amd64（Mattermost Server Plugin）＋ Web App Plugin

## できること

| 操作 | 入口 |
| --- | --- |
| モデル切替（Provider → モデル → 適用） | パネル / `/hermes-model` |
| 会話リセット | パネル / `/hermes-reset` |
| コンテキスト圧縮 | パネル / `/hermes-compress` |
| 状態を `/status` と同じ本文で表示 | `/hermes-status`（gateway の `/status` をそのまま投稿） |
| パネルを開く | App Bar / 投稿メニュー「Open Hermes Console」 / `/hermes` |

パネルは RHS ではなく、チャンネルやスレッドへの通常の投稿として置く。操作すると同じ投稿が書き換わる。

## 構成

```
docs/     仕様書・レビュー記録
server/   Mattermost Server Plugin (Go)   ← 認証・権限・中継・パネル描画
webapp/   Mattermost Web App Plugin (React/TS) ← App Bar・投稿メニューの入口
bridge/   Hermes Control Bridge (Python, in-process) ← 状態取得・排他・冪等
```

- bridge は Hermes gateway プロセス内で動く（`~/.hermes/plugins/hermes-console-bridge`）。
- gateway プロセスでは `register()` の時点で HTTP bridge を起動する（CLI では遅延起動）。
- bridge を変更したときは gateway の再起動が必要。

## 設定（System Console → Plugins → Hermes Console）

| キー | 内容 |
| --- | --- |
| `BridgeURL` | bridge の接続先（既定 `http://127.0.0.1:18066`） |
| `BridgeToken` | 共有サービス認証。サーバー側のみで保持 |
| `AllowedUsers` | カンマ区切りの user_id。未設定時は操作不可 |
| `AllowedChannels` | カンマ区切りの channel_id。未設定時は操作不可 |
| `HermesBotID` | 既存 Hermes 連携の bot user_id |

`~/.hermes/.env` の `HERMES_CONSOLE_BRIDGE_TOKEN` と `BridgeToken` を一致させる。

## 言語

パネルとコマンド説明は gateway の `display.language` に追従する（ja / en、fallback `en`）。
gateway の `/status` と同じ言語になるので、表示が食い違わない。

```bash
hermes config set display.language ja   # 日本語にする
```

## ビルドと導入

```bash
cd server && GOOS=linux GOARCH=amd64 go build -trimpath -o dist/plugin-linux-amd64 .
# plugin.json + server/dist/plugin-linux-amd64 + webapp/dist/main.js を tar.gz にまとめて
# /api/v4/plugins?force=true へアップロードし、enable する
```

webapp を変更したときは、`webapp` で esbuild を実行して `webapp/dist/main.js` を作り直す。

## テスト

```bash
cd server && go test ./...          # 48 pass
pytest bridge/tests                 # 36 pass
```

## ライセンス

MIT（[LICENSE](LICENSE)）。

## 仕様・レビュー

- [仕様書](docs/SPEC.md)（v0.3、実装に合わせて更新）
- [レビュー記録](docs/REVIEW.md)
