package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	testUser    = "user-1"
	testChannel = "chan-1"
	testRoot    = "root-1"
)

// newTestPlugin returns a plugin wired to a fake Mattermost API and a stub bridge.
func newTestPlugin(t *testing.T, bridgeURL string, cfg configuration) (*Plugin, *plugintest.API) {
	t.Helper()
	api := &plugintest.API{}
	p := &Plugin{client: &http.Client{Timeout: 5 * time.Second}}
	p.API = api
	p.cfg = cfg
	// Tests assert the Japanese copy: pin the language so a bridge-less
	// gatewayLanguage() call does not fall back to English mid-test.
	p.lang = "ja"
	p.langAt = time.Now()
	setLanguage("ja")
	// Every authenticated route resolves the actor through GetUser.
	api.On("GetUser", testUser).Return(&model.User{Id: testUser}, nil)
	// The bridge needs the channel TYPE to build the same session key the adapter
	// does; tests use a private channel (type "P" -> chat_type "group").
	api.On("GetChannel", mock.Anything).Return(&model.Channel{Id: testChannel, Type: model.ChannelTypePrivate}, nil)
	// Logging is never asserted; accept every shape the plugin emits (msg + 0..8 kv).
	for _, method := range []string{"LogInfo", "LogWarn", "LogError"} {
		for n := 1; n <= 9; n++ {
			args := make([]any, n)
			for i := range args {
				args[i] = mock.Anything
			}
			api.On(method, args...).Return()
		}
	}
	if bridgeURL != "" {
		p.cfg.BridgeURL = bridgeURL
	}
	return p, api
}

func doRequest(p *Plugin, path, userID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if userID != "" {
		req.Header.Set("Mattermost-User-ID", userID)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(nil, rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func TestUnauthenticatedWithoutHeader(t *testing.T) {
	p, api := newTestPlugin(t, "", configuration{})
	api.On("GetUser", testUser).Return(&model.User{Id: testUser}, nil)

	rec := doRequest(p, "/api/v1/agents/hermes/session?channel_id="+testChannel, "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "unauthenticated", decode(t, rec)["error"])
}

func TestUnknownUserRejected(t *testing.T) {
	p, api := newTestPlugin(t, "", configuration{})
	api.ExpectedCalls = nil
	api.On("GetUser", testUser).Return(nil, &model.AppError{Id: "boom"})

	rec := doRequest(p, "/api/v1/agents/hermes/session?channel_id="+testChannel, testUser)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestMissingChannelIsTargetNotFound(t *testing.T) {
	p, api := newTestPlugin(t, "", configuration{})
	api.On("GetUser", testUser).Return(&model.User{Id: testUser}, nil)

	rec := doRequest(p, "/api/v1/agents/hermes/session", testUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "target_not_found", decode(t, rec)["error"])
}

func TestNoChannelAccessIsForbidden(t *testing.T) {
	p, api := newTestPlugin(t, "", configuration{})
	api.On("GetUser", testUser).Return(&model.User{Id: testUser}, nil)
	api.On("GetChannelMember", testChannel, testUser).Return(nil, &model.AppError{Id: "boom"})

	rec := doRequest(p, "/api/v1/agents/hermes/session?channel_id="+testChannel, testUser)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "forbidden", decode(t, rec)["error"])
}

func TestUnallowlistedUserIsForbidden(t *testing.T) {
	p, api := newTestPlugin(t, "", configuration{AllowedUsers: "someone-else"})
	api.On("GetUser", testUser).Return(&model.User{Id: testUser}, nil)
	api.On("GetChannelMember", testChannel, testUser).Return(&model.ChannelMember{}, nil)

	rec := doRequest(p, "/api/v1/agents/hermes/session?channel_id="+testChannel, testUser)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestUnallowlistedChannelIsForbidden(t *testing.T) {
	p, api := newTestPlugin(t, "", configuration{AllowedChannels: "other-channel"})
	api.On("GetUser", testUser).Return(&model.User{Id: testUser}, nil)
	api.On("GetChannelMember", testChannel, testUser).Return(&model.ChannelMember{}, nil)

	rec := doRequest(p, "/api/v1/agents/hermes/session?channel_id="+testChannel, testUser)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// A root post from a different channel must be rejected (target substitution, A-10).
func TestRootPostFromOtherChannelRejected(t *testing.T) {
	p, api := newTestPlugin(t, "", configuration{})
	api.On("GetUser", testUser).Return(&model.User{Id: testUser}, nil)
	api.On("GetChannelMember", testChannel, testUser).Return(&model.ChannelMember{}, nil)
	api.On("GetPost", testRoot).Return(&model.Post{Id: testRoot, ChannelId: "elsewhere"}, nil)

	rec := doRequest(p, "/api/v1/agents/hermes/session?channel_id="+testChannel+"&root_post_id="+testRoot, testUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "target_not_found", decode(t, rec)["error"])
}

// Happy path: the bridge receives the actor and the target, and the service token.
func TestRelayPassesActorAndToken(t *testing.T) {
	var gotPath, gotActor, gotAuth string
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotActor = r.URL.Query().Get("user_id")
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"session_exists": true, "activity": "idle"})
	}))
	defer bridge.Close()

	p, api := newTestPlugin(t, bridge.URL, configuration{BridgeToken: "svc-token"})
	api.On("GetUser", testUser).Return(&model.User{Id: testUser}, nil)
	api.On("GetChannelMember", testChannel, testUser).Return(&model.ChannelMember{}, nil)
	api.On("GetPost", testRoot).Return(&model.Post{Id: testRoot, ChannelId: testChannel}, nil)

	rec := doRequest(p, "/api/v1/agents/hermes/session?channel_id="+testChannel+"&root_post_id="+testRoot, testUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "/agents/hermes/session", gotPath)
	assert.Equal(t, testUser, gotActor, "actor must come from the Mattermost session")
	assert.Equal(t, "Bearer svc-token", gotAuth)
}

func TestBridgeDownIsUnavailable(t *testing.T) {
	p, _ := newTestPlugin(t, "http://127.0.0.1:1", configuration{})

	rec := doRequest(p, "/api/v1/agents", testUser)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "hermes_unavailable", decode(t, rec)["error"])
}

func TestHealthAndRouting(t *testing.T) {
	p, _ := newTestPlugin(t, "", configuration{})

	rec := doRequest(p, "/api/v1/health", "")
	assert.Equal(t, http.StatusOK, rec.Code)

	// Tolerates a prefixed path (direct testing / proxy rewrite).
	rec = doRequest(p, "/plugins/com.nicox.hermes-console/api/v1/health", "")
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = doRequest(p, "/api/v1/nope", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// Unset allowlists must permit (loopback dev); set-but-empty must still deny others.
func TestEmptyAllowlistsPermit(t *testing.T) {
	p, api := newTestPlugin(t, "", configuration{})
	api.On("GetUser", testUser).Return(&model.User{Id: testUser}, nil)
	api.On("GetChannelMember", testChannel, testUser).Return(&model.ChannelMember{}, nil)

	users, channels := p.allowlists()
	assert.Empty(t, users)
	assert.Empty(t, channels)
}
