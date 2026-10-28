package main

import (
	"strings"
	"sync/atomic"
)

// The console follows the gateway's display language, not the Mattermost user's
// locale: the gateway's own `/status` text is rendered in display.language, so
// a panel in another language would contradict it. `en` is the fallback.
var activeLanguage atomic.Value // string: "ja" | "en"

// normalizeLanguage accepts Mattermost/gateway spellings ("ja", "ja-JP",
// "en-US") and collapses them to a supported pack; unknown -> "en".
func normalizeLanguage(raw string) string {
	lang := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.IndexAny(lang, "-_"); i > 0 {
		lang = lang[:i]
	}
	switch lang {
	case "ja", "en":
		return lang
	}
	return "en"
}

func setLanguage(raw string) {
	activeLanguage.Store(normalizeLanguage(raw))
}

func language() string {
	if v, ok := activeLanguage.Load().(string); ok && v != "" {
		return v
	}
	return "en"
}

// tr translates one user-facing message. The Japanese text is the lookup key,
// so a string without an English entry still reads correctly instead of showing
// a bare key.
func tr(ja string) string {
	if language() == "ja" {
		return ja
	}
	if en, ok := englishText[ja]; ok {
		return en
	}
	return ja
}

// englishText maps every user-facing Japanese message to its English form.
// Input keywords (状態/モデル/リセット/圧縮) are NOT translated: they are
// aliases the user types, not output.
var englishText = map[string]string{
	"スレッド（スレッド内で共有）":                            "Thread (shared within the thread)",
	"チャンネル直下（ユーザーごと）":                           "Channel root (per user)",
	"Hermes Console を開く（status は /status と同じ本文）": "Open Hermes Console (status = the /status text verbatim)",
	"状態を表示（/status と同じ本文）":                      "Show status (the /status text verbatim)",
	"モデルを選んで切り替える":                              "Pick a model and switch to it",
	"この会話をリセット":                                 "Reset this conversation",
	"この会話のコンテキストを圧縮":                            "Compress this conversation's context",
	"Hermes Console: このユーザーまたはチャンネルは許可されていません。":  "Hermes Console: this user or channel is not allowlisted.",
	"Hermes Console: パネルを投稿できませんでした (%s)":        "Hermes Console: could not post the panel (%s)",
	"(プロバイダーを取得できません)":                          "(provider unavailable)",
	"(モデルを取得できません)":                             "(models unavailable)",
	"%s（%dモデル）":                                 "%s (%d models)",
	"（選択中）":                                     " (selected)",
	"（現在）":                                      " (current)",
	"未確認":                                       "unknown",
	"未作成":                                       "none yet",
	"あり":                                        "yes",
	"待機中":                                       "idle",
	"応答中":                                       "running",
	"選択中: %s / %s":                              "Selected: %s / %s",
	"モデル一覧の取得に失敗: %s\n→「状態を更新」で再取得できます": "Could not fetch the model list: %s\n→ Press Refresh to retry",
	"モデル":          "Model",
	"現在モデル":        "Current model",
	"範囲":           "Scope",
	"Provider を選ぶ": "Pick a provider",
	"モデルを選ぶ":       "Pick a model",
	"適用":           "Apply",
	"セッション":        "Session",
	"対象: %s":       "Target: %s",
	"稼働状態":         "Activity",
	"会話":           "Conversation",
	"状態を更新":        "Refresh",
	"結果":           "Result",
	"範囲: %s":       "Scope: %s",
	"会話: %s":       "Conversation: %s",
	"実行中です。少し待ってから再度押してください。":             "An operation is already running. Try again in a moment.",
	"結果不明（応答を解釈できませんでした）。自動再送はしません。":      "Outcome unknown (unreadable response). Not retried automatically.",
	"失敗: チャンネル種別を取得できませんでした（操作は送信していません）": "Failed: could not determine the channel type (nothing was sent)",
	"失敗: %s": "Failed: %s",
	"結果不明（タイムアウト）。自動再送はしません。": "Outcome unknown (timeout). Not retried automatically.",
	"失敗 (%d): %s": "Failed (%d): %s",
	"🗜️ 会話を圧縮しています…":                         "🗜️ Compressing the conversation…",
	"♻️ 会話をリセットしています…":                       "♻️ Resetting the conversation…",
	"⚠️ 実行中です。少し待ってから再度実行してください。":            "⚠️ An operation is already running. Try again in a moment.",
	"⚠️ 結果不明（応答を解釈できませんでした）。自動再送はしません。":      "⚠️ Outcome unknown (unreadable response). Not retried automatically.",
	"⚠️ 失敗: チャンネル種別を取得できませんでした（操作は送信していません）": "⚠️ Failed: could not determine the channel type (nothing was sent)",
	"⚠️ 失敗: %s": "⚠️ Failed: %s",
	"⚠️ 結果不明（タイムアウト）。自動再送はしません。": "⚠️ Outcome unknown (timeout). Not retried automatically.",
	"⚠️ 失敗 (%d): %s": "⚠️ Failed (%d): %s",
	"会話をリセットしました":    "Conversation reset",
	"会話を圧縮しました":      "Conversation compressed",
	"モデルを適用しました":     "Model applied",
	"状態を更新しました":      "Refreshed",
	"モデルが未選択です。モデルを選んでから適用してください。":       "No model selected. Pick a model, then press Apply.",
	"⚠️ statusを取得できませんでした（ブリッジが応答しません）。": "⚠️ Could not fetch status (the bridge did not answer).",
}
