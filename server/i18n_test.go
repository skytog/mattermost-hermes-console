package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeLanguage(t *testing.T) {
	cases := map[string]string{
		"ja":     "ja",
		"ja-JP":  "ja",
		"JA_jp":  "ja",
		"en":     "en",
		"en-US":  "en",
		"de-DE":  "en", // unsupported pack -> agreed fallback
		"":       "en",
		"  ja  ": "ja",
	}
	for raw, want := range cases {
		assert.Equal(t, want, normalizeLanguage(raw), "raw=%q", raw)
	}
}

func TestTranslationFollowsActiveLanguage(t *testing.T) {
	t.Cleanup(func() { setLanguage("en") })

	setLanguage("ja")
	assert.Equal(t, "モデル", tr("モデル"))
	assert.Equal(t, "適用", tr("適用"))

	setLanguage("en")
	assert.Equal(t, "Model", tr("モデル"))
	assert.Equal(t, "Apply", tr("適用"))
	assert.Equal(t, "Refresh", tr("状態を更新"))
	// A string with no English entry still renders (Japanese is the key).
	assert.Equal(t, "未登録の文言", tr("未登録の文言"))
}

// Every user-facing Japanese message in the panel has an English entry; a miss
// shows Japanese to an English user, which is the whole point of the table.
func TestAllPanelStringsAreTranslated(t *testing.T) {
	setLanguage("ja")
	required := []string{
		"モデル", "セッション", "現在モデル", "範囲", "稼働状態", "会話",
		"Provider を選ぶ", "モデルを選ぶ", "適用", "状態を更新", "結果",
		"スレッド（スレッド内で共有）", "チャンネル直下（ユーザーごと）",
		"未確認", "未作成", "あり", "待機中", "応答中",
		"実行中です。少し待ってから再度押してください。",
		"結果不明（タイムアウト）。自動再送はしません。",
		"会話をリセットしました", "会話を圧縮しました", "モデルを適用しました",
		"🗜️ 会話を圧縮しています…", "♻️ 会話をリセットしています…",
	}
	for _, ja := range required {
		en, ok := englishText[ja]
		assert.True(t, ok, "missing English entry: %q", ja)
		assert.NotEqual(t, ja, en, "English entry must differ: %q", ja)
	}
}

// The panel renders in the gateway's language, not the Mattermost user's.
func TestPanelRendersInTheGatewayLanguage(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agents/hermes/language":
			_ = json.NewEncoder(w).Encode(map[string]any{"language": "en-US"})
		case "/agents/hermes/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"key": "qwen38:m-a", "display_name": "M A", "provider_display_name": "PGX"}},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"session_exists": true, "activity": "idle"})
		}
	}))
	defer bridge.Close()

	p, _ := newTestPlugin(t, bridge.URL, configuration{})
	p.lang, p.langAt = "", time.Time{}
	atts := p.panelAttachments(testChannel, testRoot, testUser, "", "", viewModel, false)
	require.Len(t, atts, 1)
	assert.Equal(t, "Model", atts[0].Title)
	assert.Equal(t, "en", p.gatewayLanguage())
}
