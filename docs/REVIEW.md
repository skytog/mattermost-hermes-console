# Mattermost Hermes Console 仕様書 — レビュー記録（v0.1 に対する指摘）

- レビュー日：2026-10-11（JST）
- 対象：mattermost-hermes-console-spec.md v0.1
- 結論：方向は妥当。ただし前提が3つズレていたため、§7の層構成を1枚減らし、
  セッション扱いを実設定に合わせ、パッチ適用を前提化した。→ SPEC.md v0.2 に反映済み。

## 実環境で確認した事実（v0.1の「未確認」を埋めた）

| 項目 | 実測値 |
| --- | --- |
| Mattermost | mattermost/mattermost-team-edition:latest、稼働中・healthy、v11.11.0、image created 2026-09-07、127.0.0.1:8065、SiteURL https://<your-mattermost-host> |
| Hermes | v0.21.6+509.gf925b01 (2026.9.24)、commit f925b01791 (2026-10-10)、adapter は plugins/platforms/mattermost/adapter.py |
| Enhancer導入状況 | 未導入（~/.hermes/plugins は homeassistant のみ） |
| セッション分離 | group_sessions_per_user: true |
| CPU | x86_64 / 16core（Mattermostホスト） |
| SDK | registerChannelHeaderButtonAction / registerRightHandSidebarComponent / registerPostDropdownMenuAction / registerAppBarComponent は実在 |

参照検証：［S1］は GitHub rename で［S2］［S3］と同一リポジトリ（a-nomad-studio/hermes-plugin-mattermost-enhancer、MIT）。
［S4］は Python の Hermes プラグインで中に mattermost-plugin/（Go、plugin id com.hermes-agent.slash-bridge、min_server_version 9.0.0）を持つ。ライセンスは NOASSERTION。
［S5］Agents Plugin は Go / 254 stars。

## ブロッカー（v0.1 の欠陥）

- **B-1**：§4.2 の「共有会話」前提はチャンネル直下で不成立（group_sessions_per_user=true → per-user）。
  実測で確認。Actor を経路全体で必須化する必要がある。
- **B-2**：Bridge 新設の必然性が薄い。Enhancer が同じ機能を既に持つ。
  callback はローカルHTTP + MATTERMOST_CALLBACK_SECRET の HMAC署名、slash はコマンド別トークンで認証も実装済み。
  さらに Enhancer 自身が「session key を手で組むな、build_session_key() を使え」という規約を持つ。
  → Server Plugin → Enhancer callback に接続し、Bridge は不足分のみに絞る。
- **B-3**：Enhancer は「インストールするだけ」ではない。register_platform() は last-writer-wins で
  bundled adapter の apply_yaml_config_fn / standalone_sender_fn / validate_config / is_connected / メッセージ長を
  引き継がないと機能が気づかないうちに動かなくなる。scripts/hermes-mattermost-enhancer.sh は
  gateway/run_inbound.py（Clarifyセッション分裂）と gateway/run_startup.py（再起動時セッション串台）を直接パッチする。
  → Step1で old_string の一意マッチを実測確認（E-P2 PartA/PartB、E-P4 すべて1回）。適用可能。
  → Hermes 更新ごとに再検証必須（実機は検証済みコミットより新しい）。
- **B-4**：Mattermost は Docker、Hermes はホスト。callback 到達に host.docker.internal が必要。
  compose に MM_PLUGINSETTINGS_ENABLEUPLOADS が無く、System Console で有効化が必要。latest タグは digest pin 推奨。
- **B-5**：registerRightHandSidebarComponent はグローバルRHSでスレッドRHSと枠を奪い合う。
  入口は registerAppBarComponent の rhsComponent を使う方が素直。

## 中程度

- M-1 モデル候補の取得元が未定義 → providers:/custom_providers: を固定（実測で取得可能を確認）。
- M-2 「応答中・ツール実行中なら拒否」の判定源が未定義 → _is_session_running / _peek_session_state を新設利用。
- M-3 expected_revision は Hermes に無い概念 → Bridge 側で新設する旨を明記。
- M-4 圧縮の --preview / here N は既存 → MVPで出すか選ぶ。
- M-5 監査ログの SQLite 配置・保持・ローテーション未定義。
- M-6 アンインストール / ロールバック手順が要件化されていない（パッチを当てるので必須）。

## 細かい修正

- SDK 関数名を明記：registerAppBarAction ではなく registerAppBarComponent。
- Mattermost 制約を仕様化：action_id は純字母（-/_不可）、attachment あたり5 actions / メッセージあたり5 attachments、select はデフォルト選択不可。
- ［S4］ライセンス NOASSERTION を §11.2 に反映。
- ［S1］〜［S3］が同一リポジトリである旨を注記。

## 追加発見（実装リスク）

- `_handle_compress_command(event: MessageEvent)` は session_key ではなく MessageEvent を要求する。
  Bridge は最小の合成イベントを組む必要があり、ここが最も難しい。
- `_session_has_compression_in_flight(session_key)` が圧縮ロックとして使える。

## 決定

1. 構成 = A（Enhancer をもとに、不足分のみ新設）。Bridge は `gateway.session` を直接importし、ロジックを写経しない。
2. セッション = Actor込み per-user マッピングを初期から採用。
3. パッチ = 当てる前提、導入時に SKIP/再検証を実施。

## 2026-10-27 外部レビュー（sol）M1〜M5 / m1〜m4 の修正

対象は未コミットの作業ツリー。方針は「入口で対象を一貫させる」「排他はサーバー側」「timeout は操作の終了ではない」。

| 指摘 | 修正 |
| --- | --- |
| M1 ブラウザ API の chat_type 欠落 | `handleSession` がサーバー側で `GetChannel().Type` を読み、`chat_type` を付加 |
| M2 DM 直下の架空スレッド化 | `resolveThreadRoot` は `chat_type=dm` で自動 root を作らない。スレッドは本人が投稿したものだけ候補 |
| M3 fetchState の user_id 欠落 | `fetchState(channel, root, user)` に変更。パネルは `target_user`、status は対象ユーザーを渡す |
| M4 busy が排他にならない | `reserveOp`（channel+root+user）で goroutine 起動前に原子的に予約。パネルは per-post mutex で read-modify-write を直列化。`busy` は同期的に立てる。冪等キーは受付ごとに一意 |
| M5 timeout 後の追跡漏れ | bridge に `_running` レジストリ。timeout 中は 409 `in_progress`、`add_done_callback` で遅延結果を `_idempotency` に記録 |
| m1 投稿更新失敗で「実行中」が残る | `updatePost` は3回再試行。失敗時は結果を別投稿へフォールバック |
| m2 200 を成功と混同 | read/decode エラーは `errOutcomeUnknown`（結果不明）。gateway 本文をそのまま提示し、⚠️/失敗/不十分/failed 等で成否を判定 |
| m3 GetChannel 失敗時に書き込み継続 | 書き込みは `channelTypeOf` が失敗したら送信しない（`errChatTypeUnknown`） |
| m4 status の内外逆転 | plugin の status 用 client を 25s（bridge の 20s より長く） |

検証: `go test ./...` 41 pass / 0 fail、`go vet` clean、`pytest bridge/tests` 33 pass。実機デプロイと gateway 再起動は未実施。

残り（NIT）: `panel.go` の reset/compress 分岐コメント、`SPEC.md:173` のボタン記述は旧仕様のまま。

## 2026-10-27 実機フィードバックと仕上げ（plugin 0.2.7 → 0.3.2）

| 指摘 | 対応 |
| --- | --- |
| モデル一覧取得に失敗する | bridge を gateway の `register()` で起動（CLI は遅延のまま）。`gateway.run` の runner を最初のリクエストで bind。パネルの復帰待ちを 3s×30（90s）に延長。起動直後に `/models` が返ることを実測 |
| パネルの `Provider: … — ① Provider → ② モデル → ③ 適用` が不要 | 手順説明を全削除。未選択時は空、選択時は `Provider: <名前>` / `Selected: <Provider> / <モデル>` だけ |
| 複数言語にしたい。gateway に合わせ、fallback は en | `server/i18n.go` を追加。bridge `GET /agents/hermes/language` で `display.language` を取得し 60s キャッシュ。日本語文をキーにした ja/en 表（78文言）。入力キーワードは訳さない |
| `/hermes-status` は `/status` と同じでは？ | コンソールのフッターを廃止し、gateway の `/status` 本文をそのまま投稿。空応答は1回リトライ、駄目なら明示メッセージ。パネルへのフォールバックをやめた |
| OSS にするのでコミットメッセージを遡って直す | 全16コミットを Conventional Commits（`feat:`/`fix:`/`docs:`/`chore:` + scope）へ書き換え。ツリーが同一であることを確認済み。`backup/pre-conventional` に旧履歴を保持 |
| チャンネル直下に書いたのに上のスレッドに status/パネルが出る | `resolveThreadRoot`（直近の自分のスレッドを推定）を廃止。対象は `CommandArgs.RootId` のみ。直下は空、スレッド内はその root。回帰テスト2件追加 |
| SPEC/README が実装とズレている | 投稿パネル方式・実エンドポイント・検証状況・運用注意へ更新（SPEC v0.3）。コンテキスト使用量が未実装であることも明記 |

検証: Go **48 pass** / bridge **36 pass**。実機で モデル切替・リセット・圧縮・DM パネル・busy 拒否を
post-action API 経由で確認（ブラウザーの App Bar クリックだけ未確認）。
