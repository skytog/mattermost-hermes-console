package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// The browser API must send the SAME chat_type the adapter uses: a private
// channel is "group", not a guessed default (review M1).
func TestBrowserSessionRelaySendsChannelChatType(t *testing.T) {
	var gotType string
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotType = r.URL.Query().Get("chat_type")
		_ = json.NewEncoder(w).Encode(map[string]any{"session_exists": true, "activity": "idle"})
	}))
	defer bridge.Close()

	p, api := newTestPlugin(t, bridge.URL, configuration{})
	api.On("GetChannelMember", testChannel, testUser).Return(&model.ChannelMember{}, nil)
	api.On("GetPost", testRoot).Return(&model.Post{Id: testRoot, ChannelId: testChannel}, nil)

	rec := doRequest(p, "/api/v1/agents/hermes/session?channel_id="+testChannel+"&root_post_id="+testRoot, testUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "group", gotType, "private channel must resolve as group, never a guess")
}

// The panel's state read must carry the target user, otherwise a channel-root
// session shows a different session than the operation targets (review M3).
func TestFetchStateSendsTargetUserAndChatType(t *testing.T) {
	var gotUser, gotType string
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.URL.Query().Get("user_id")
		gotType = r.URL.Query().Get("chat_type")
		_ = json.NewEncoder(w).Encode(map[string]any{"session_exists": true, "activity": "idle"})
	}))
	defer bridge.Close()

	p, _ := newTestPlugin(t, bridge.URL, configuration{})
	_, err := p.fetchState(testChannel, "", testUser)
	require.NoError(t, err)
	assert.Equal(t, testUser, gotUser)
	assert.Equal(t, "group", gotType)
}

// A write must not proceed when the channel type cannot be read: guessing
// "channel" mutates a phantom session (review m3).
func TestActionRefusesUnknownChannelType(t *testing.T) {
	posts := 0
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		_ = json.NewEncoder(w).Encode(map[string]any{"text": "ok"})
	}))
	defer bridge.Close()

	p, api := newTestPlugin(t, bridge.URL, configuration{})
	api.ExpectedCalls = nil
	api.On("GetChannel", mock.Anything).Return(nil, &model.AppError{Id: "boom"})
	for _, method := range []string{"LogInfo", "LogWarn", "LogError"} {
		for n := 1; n <= 9; n++ {
			args := make([]any, n)
			for i := range args {
				args[i] = mock.Anything
			}
			api.On(method, args...).Return()
		}
	}

	_, _, err := p.callBridgeAction(testChannel, testRoot, testUser, "reset_session", "", "idem")
	require.Error(t, err)
	assert.True(t, errors.Is(err, errChatTypeUnknown), "expected errChatTypeUnknown, got %v", err)
	assert.Equal(t, 0, posts, "no operation may reach the bridge without a known chat_type")
}

// A 200 with an unparseable body means "outcome unknown", not success
// (review m2).
func TestActionUnparseableReplyIsOutcomeUnknown(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer bridge.Close()

	p, _ := newTestPlugin(t, bridge.URL, configuration{})
	_, status, err := p.callBridgeAction(testChannel, testRoot, testUser, "reset_session", "", "idem")
	require.Error(t, err)
	assert.True(t, errors.Is(err, errOutcomeUnknown), "expected errOutcomeUnknown, got %v", err)
	assert.Equal(t, http.StatusOK, status)
}

// A gateway business failure arrives as HTTP 200; it must not be reported as
// success (review m2).
func TestPayloadFailureIsDetected(t *testing.T) {
	assert.True(t, payloadFailed("圧縮するための会話が不十分です (少なくとも 4 件のメッセージが必要)。"))
	assert.True(t, payloadFailed("Not enough conversation to compress (need at least 4 messages)."))
	assert.True(t, payloadFailed("⚠️ 圧縮が中止されました"))
	assert.True(t, payloadFailed("Compression failed: boom"))
	assert.False(t, payloadFailed("要約を更新しました (12 messages -> 1 summary)"))
	assert.False(t, payloadFailed("モデルを gpt-6-sol に切り替えました"))
}

// The status client must outlive the bridge's own 20s wait, or the plugin gives
// up first and falls back to a panel (review m4).
func TestStatusClientOutlivesBridgeWait(t *testing.T) {
	p, _ := newTestPlugin(t, "http://127.0.0.1:1", configuration{})
	c := p.statusClient()
	require.NotNil(t, c)
	assert.GreaterOrEqual(t, c.Timeout, 20*time.Second)
}

// DM conversations are not auto-threaded by the adapter, so the plugin must not
// invent a root from the user's own top-level post (review M2).
// A channel-level /hermes must target the CHANNEL session (RootId ""), not the
// thread the user last posted in: that made the panel land in the thread above
// (nicox). RootId from the command args is the only source of truth.
func TestChannelLevelCommandTargetsTheChannelRoot(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/language"):
			_ = json.NewEncoder(w).Encode(map[string]any{"language": "en"})
		case strings.HasSuffix(r.URL.Path, "/models"):
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"key": "p:m", "display_name": "M"}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"session_exists": true, "activity": "idle"})
		}
	}))
	defer bridge.Close()

	p, api := newTestPlugin(t, bridge.URL, configuration{})
	api.On("GetChannelMember", testChannel, testUser).Return(&model.ChannelMember{}, nil)
	api.On("GetChannel", testChannel).Return(&model.Channel{Id: testChannel, Type: model.ChannelTypeOpen}, nil)
	var created *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		created = args.Get(0).(*model.Post)
	}).Return(&model.Post{Id: "p-1"}, nil)

	_, appErr := p.ExecuteCommand(nil, &model.CommandArgs{
		UserId: testUser, ChannelId: testChannel, RootId: "", Command: "/hermes",
	})
	require.Nil(t, appErr)
	require.NotNil(t, created)
	assert.Equal(t, "", created.RootId, "a channel-level panel must stay at the channel root")
	assert.Equal(t, "", created.Props["target_root"])
	api.AssertNotCalled(t, "GetPostsSince", mock.Anything, mock.Anything)
}

// Inside a thread, RootId is passed through unchanged.
func TestThreadCommandTargetsThatThread(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/language"):
			_ = json.NewEncoder(w).Encode(map[string]any{"language": "en"})
		case strings.HasSuffix(r.URL.Path, "/models"):
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"key": "p:m", "display_name": "M"}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"session_exists": true, "activity": "idle"})
		}
	}))
	defer bridge.Close()

	p, api := newTestPlugin(t, bridge.URL, configuration{})
	api.On("GetChannelMember", testChannel, testUser).Return(&model.ChannelMember{}, nil)
	api.On("GetChannel", testChannel).Return(&model.Channel{Id: testChannel, Type: model.ChannelTypeOpen}, nil)
	var created *model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		created = args.Get(0).(*model.Post)
	}).Return(&model.Post{Id: "p-1"}, nil)

	_, appErr := p.ExecuteCommand(nil, &model.CommandArgs{
		UserId: testUser, ChannelId: testChannel, RootId: "root-1", Command: "/hermes",
	})
	require.Nil(t, appErr)
	require.NotNil(t, created)
	assert.Equal(t, "root-1", created.RootId)
	assert.Equal(t, "root-1", created.Props["target_root"])
}

// Two clicks must produce ONE bridge operation: the server reserves the session
// before spawning, not after (review M4).
func TestPanelActionReservesTheSessionBeforeSpawning(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			// Panel rendering reads state/models; those are not operations.
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{}})
			return
		}
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"text": "done"})
	}))
	defer bridge.Close()

	p, api := newTestPlugin(t, bridge.URL, configuration{})
	post := &model.Post{Id: "panel-1", Props: map[string]any{
		"target_channel": testChannel,
		"target_root":    testRoot,
		"target_user":    testUser,
	}}
	api.On("GetPost", "panel-1").Return(post, nil)
	api.On("UpdatePost", mock.Anything).Return(post, nil)
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "result-1"}, nil)

	p.runPanelAction("panel-1", testChannel, testRoot, testUser, actReset, "", "idem-1")
	p.runPanelAction("panel-1", testChannel, testRoot, testUser, actReset, "", "idem-2")
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give the second (refused) click time to write its message, then ensure no
	// second bridge call appeared.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, calls, "second click must be refused, not relayed")

	sawBusy := false
	for _, c := range api.Calls {
		if c.Method != "UpdatePost" {
			continue
		}
		if up, ok := c.Arguments.Get(0).(*model.Post); ok {
			if msg, _ := up.Props["last_result"].(string); msg != "" {
				sawBusy = true
			}
		}
	}
	assert.True(t, sawBusy, "the refused click must say so in the panel")
}

// The model block must not repeat "Provider: X（現在の設定）— ① Provider → ② …":
// the selects and the 適用 button already say what to do (nicox).
func TestModelBlockHasNoStepByStepText(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{
					{"key": "qwen38:m-a", "display_name": "M A", "provider_display_name": "PGX"},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"session_exists": true, "activity": "idle"})
	}))
	defer bridge.Close()

	p, _ := newTestPlugin(t, bridge.URL, configuration{})
	atts := p.panelAttachments(testChannel, testRoot, testUser, "", "", viewModel, false)
	require.Len(t, atts, 1)
	assert.Equal(t, "", atts[0].Text, "nothing picked yet: no instruction text")
	assert.NotContains(t, atts[0].Text, "①")
	assert.NotContains(t, atts[0].Text, "現在の設定")
}

// `/hermes-status` must return the gateway's /status text VERBATIM: no console
// footer, no reformatting (nicox).
func TestStatusMessageIsExactlyTheGatewayText(t *testing.T) {
	const statusText = "📊 **Hermes Gateway Status**\n\n**Model:** `m`"
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/language") {
			_ = json.NewEncoder(w).Encode(map[string]any{"language": "en"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"handled": true, "text": statusText})
	}))
	defer bridge.Close()

	p, _ := newTestPlugin(t, bridge.URL, configuration{})
	got := p.statusMessage(testChannel, testRoot, testUser)
	assert.Equal(t, statusText, got, "must be byte-identical to /status")
	assert.NotContains(t, got, "Hermes Console")
	assert.NotContains(t, got, "Scope:")
}

// When status cannot be fetched, say so instead of silently posting a panel
// that looks like the command's normal output.
func TestStatusFailurePostsAnExplicitMessage(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer bridge.Close()

	p, api := newTestPlugin(t, bridge.URL, configuration{})
	api.On("GetChannelMember", testChannel, testUser).Return(&model.ChannelMember{}, nil)
	var posts []*model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		posts = append(posts, args.Get(0).(*model.Post))
	}).Return(&model.Post{Id: "p-1"}, nil)

	_, appErr := p.ExecuteCommand(nil, &model.CommandArgs{
		UserId: testUser, ChannelId: testChannel, RootId: testRoot, Command: "/hermes-status",
	})
	require.Nil(t, appErr)
	require.Len(t, posts, 1, "one message, no panel")
	assert.Contains(t, posts[0].Message, "statusを取得できませんでした")
	if panel, _ := posts[0].Props["hermes_console"].(bool); panel {
		t.Fatal("a failed status must not post a panel")
	}
}

// The reservation must be released when the operation ends, otherwise the
// session stays locked forever.
func TestOperationReservationIsReleased(t *testing.T) {
	p, _ := newTestPlugin(t, "", configuration{})
	key := opKey(testChannel, testRoot, testUser)
	require.True(t, p.reserveOp(key))
	assert.False(t, p.reserveOp(key))
	p.releaseOp(key)
	assert.True(t, p.reserveOp(key))
}
