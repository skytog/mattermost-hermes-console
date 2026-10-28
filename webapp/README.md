# Mattermost Web App Plugin

パネルの入口だけを提供する（React / TypeScript）。パネル自体は Server Plugin が投稿として作る。

- App Bar: 現在のチャンネル、スレッドを開いていればその root を対象にパネルを投稿する
- 投稿メニュー「Open Hermes Console」: 返信投稿なら root に解決してパネルを投稿する
- 右サイドバーは使わない（スレッド表示と枠を奪い合うため）

## ビルド

```bash
npm install
npm run build      # -> dist/main.js (esbuild, IIFE)
npm run typecheck
```

`server/Makefile` の `package` が webapp ビルドも呼ぶため、通常は `make package` だけでよい。

## 現状

Step 3 まで完了。パネルの描画・操作・結果表示は Server Plugin 側にある。
