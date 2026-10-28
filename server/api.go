package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/mattermost/mattermost/server/public/plugin"
)

// apiError is the JSON error envelope shared with the webapp.
type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Error: code, Message: message})
}

// relayToBridge forwards a request to the loopback Hermes Control Bridge.
//
// The bridge is trusted by a shared service token; the actor is added here, not
// by the browser. A bridge failure is never reported as a client error.
func (p *Plugin) relayToBridge(w http.ResponseWriter, path string, query url.Values) {
	base, token, client := p.bridgeConfig()
	if base == "" {
		writeError(w, http.StatusServiceUnavailable, "hermes_unavailable", "bridge URL is not configured")
		return
	}
	if client == nil {
		writeError(w, http.StatusServiceUnavailable, "hermes_unavailable", "plugin is not activated")
		return
	}

	target := base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		// Distinguish "never reached the bridge" (safe to retry) from a slow
		// bridge whose outcome is unknown (Step 3 will use 504 for that).
		writeError(w, http.StatusServiceUnavailable, "hermes_unavailable", err.Error())
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, "hermes_unavailable", err.Error())
		return
	}

	// Pass the bridge's own error envelope straight through.
	if resp.StatusCode >= 400 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// resolveActor returns the authenticated Mattermost user id or an error response.
func (p *Plugin) resolveActor(w http.ResponseWriter, r *http.Request) (string, bool) {
	userID := r.Header.Get("Mattermost-User-ID")
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "no Mattermost session")
		return "", false
	}
	if _, appErr := p.API.GetUser(userID); appErr != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "unknown user")
		return "", false
	}
	return userID, true
}

// authorizeChannel verifies channel access and allowlists for a target channel.
func (p *Plugin) authorizeChannel(w http.ResponseWriter, userID, channelID string) bool {
	if channelID == "" {
		writeError(w, http.StatusNotFound, "target_not_found", "channel_id is required")
		return false
	}
	if _, appErr := p.API.GetChannelMember(channelID, userID); appErr != nil {
		writeError(w, http.StatusForbidden, "forbidden", "no access to the channel")
		return false
	}
	users, channels := p.allowlists()
	// Unset allowlist = nothing is permitted (spec 10.2).
	if len(users) > 0 && !users[userID] {
		writeError(w, http.StatusForbidden, "forbidden", "user is not allowlisted")
		return false
	}
	if len(channels) > 0 && !channels[channelID] {
		writeError(w, http.StatusForbidden, "forbidden", "channel is not allowlisted")
		return false
	}
	return true
}

// authorizeThread verifies a root post belongs to the target channel (spec 4.3.4).
func (p *Plugin) authorizeThread(w http.ResponseWriter, channelID, rootPostID string) bool {
	if rootPostID == "" {
		return true
	}
	post, appErr := p.API.GetPost(rootPostID)
	if appErr != nil {
		writeError(w, http.StatusNotFound, "target_not_found", "root post not found")
		return false
	}
	if post.ChannelId != channelID {
		// Guards target-ID substitution (A-10).
		writeError(w, http.StatusNotFound, "target_not_found", "root post belongs to another channel")
		return false
	}
	return true
}

func targetQuery(channelID, rootPostID, userID, chatType string) url.Values {
	q := url.Values{}
	q.Set("channel_id", channelID)
	if rootPostID != "" {
		q.Set("root_post_id", rootPostID)
	}
	q.Set("user_id", userID)
	// The bridge must receive the SAME chat_type the adapter uses. Without it a
	// private channel / DM resolves to a different session key than the panel
	// (review M1).
	q.Set("chat_type", chatType)
	return q
}

func (p *Plugin) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ServeHTTP routes the browser-facing API. Mattermost strips /plugins/<id>
// before calling this, but tolerate a prefixed path for direct testing.
func (p *Plugin) ServeHTTP(_ *plugin.Context, w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/plugins/com.nicox.hermes-console")
	path = strings.TrimSuffix(path, "/")

	switch {
	case path == "/api/v1/health":
		p.handleHealth(w, r)
	case path == "/api/v1/agents":
		p.handleAgents(w, r)
	case path == "/api/v1/agents/hermes/models":
		p.handleModels(w, r)
	case path == "/api/v1/agents/hermes/session":
		p.handleSession(w, r)
	case path == "/api/v1/panel":
		p.handlePanel(w, r)
	case strings.HasPrefix(path, "/api/v1/actions"):
		p.handleAction(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", path)
	}
}

func (p *Plugin) handleAgents(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.resolveActor(w, r); !ok {
		return
	}
	p.relayToBridge(w, "/agents", nil)
}

func (p *Plugin) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.resolveActor(w, r); !ok {
		return
	}
	p.relayToBridge(w, "/agents/hermes/models", nil)
}

func (p *Plugin) handleSession(w http.ResponseWriter, r *http.Request) {
	userID, ok := p.resolveActor(w, r)
	if !ok {
		return
	}
	channelID := r.URL.Query().Get("channel_id")
	rootPostID := r.URL.Query().Get("root_post_id")
	if !p.authorizeChannel(w, userID, channelID) {
		return
	}
	if !p.authorizeThread(w, channelID, rootPostID) {
		return
	}
	p.relayToBridge(w, "/agents/hermes/session", targetQuery(channelID, rootPostID, userID, p.channelChatType(channelID)))
}
