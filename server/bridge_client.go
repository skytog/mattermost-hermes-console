package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// actionTimeout bounds a relayed operation. Compress summarizes the whole
// transcript with the aux model, which is minutes — the 10s read timeout made
// the plugin report "context deadline exceeded" while compression was still
// running (nicox hit exactly this).
const actionTimeout = 10 * time.Minute

var (
	// errChatTypeUnknown means the channel type could not be read, so the
	// session key would be a guess. A write must not proceed (review m3).
	errChatTypeUnknown = errors.New("cannot determine channel type")
	// errOutcomeUnknown means the bridge was reached but its answer could not
	// be parsed: the operation may or may not have run (review m2).
	errOutcomeUnknown = errors.New("bridge response could not be interpreted")
)

// Thin bridge client used by the panel. Reuses the same loopback relay contract
// as the browser API: actor is attached server-side, bridge errors are never
// reported as client errors.

type apiModel struct {
	Key                 string `json:"key"`
	DisplayName         string `json:"display_name"`
	ProviderDisplayName string `json:"provider_display_name"`
}

type apiState struct {
	Activity      string `json:"activity"`
	SessionExists bool   `json:"session_exists"`
	Model         struct {
		Key                 string `json:"key"`
		DisplayName         string `json:"display_name"`
		ProviderDisplayName string `json:"provider_display_name"`
	} `json:"model"`
}

type apiPayload struct {
	Text    string `json:"text"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

// botUserID is the poster identity for panel messages.
func (p *Plugin) botUserID() string {
	p.mu.RLock()
	id := p.cfg.HermesBotID
	p.mu.RUnlock()
	if id == "" {
		id = os.Getenv("MM_HERMES_BOT_ID")
	}
	return id
}

func (p *Plugin) bridgeGet(path string, query url.Values) ([]byte, int, error) {
	return p.bridgeGetWithTimeout(path, query, 0)
}

// bridgeGetWithTimeout is bridgeGet with an explicit client deadline. A zero
// timeout keeps the shared client's default.
func (p *Plugin) bridgeGetWithTimeout(path string, query url.Values, timeout time.Duration) ([]byte, int, error) {
	base, token, client := p.bridgeConfig()
	if base == "" || client == nil {
		return nil, 0, fmt.Errorf("bridge unavailable")
	}
	if timeout > 0 {
		cp := *client
		if cp.Timeout < timeout {
			cp.Timeout = timeout
		}
		client = &cp
	}
	target := base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// channelTypeOf reads the channel type exactly like the adapter does
// ("O" -> channel, "G"/"P" -> group, "D" -> dm). ok=false means the type could
// not be read, so the caller must not guess.
func (p *Plugin) channelTypeOf(channelID string) (string, bool) {
	ch, appErr := p.API.GetChannel(channelID)
	if appErr != nil || ch == nil {
		return "", false
	}
	switch ch.Type {
	case "D":
		return "dm", true
	case "G", "P":
		return "group", true
	case "O":
		return "channel", true
	}
	return "", false
}

// channelChatType mirrors the Mattermost adapter's channel_type mapping.
// Read-only callers may fall back to "channel"; writes use channelTypeOf and
// abort when the type is unknown.
func (p *Plugin) channelChatType(channelID string) string {
	if t, ok := p.channelTypeOf(channelID); ok {
		return t
	}
	return "channel"
}

// fetchState reads the read-only state payload for a target. user_id matters
// when the session is a channel root (per-user): without it the panel shows a
// DIFFERENT session than the one an operation targets (review M3).
func (p *Plugin) fetchState(channelID, rootPostID, userID string) (*apiState, error) {
	q := url.Values{}
	q.Set("channel_id", channelID)
	q.Set("chat_type", p.channelChatType(channelID))
	if rootPostID != "" {
		q.Set("root_post_id", rootPostID)
	}
	if userID != "" {
		q.Set("user_id", userID)
	}
	body, status, err := p.bridgeGet("/agents/hermes/session", q)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("bridge status %d", status)
	}
	var out apiState
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%w: %v", errOutcomeUnknown, err)
	}
	return &out, nil
}

// fetchStatus reads the gateway's own /status text for a target (read-only).
func (p *Plugin) fetchStatus(channelID, rootPostID, userID string) (string, error) {
	q := url.Values{}
	q.Set("channel_id", channelID)
	q.Set("chat_type", p.channelChatType(channelID))
	if rootPostID != "" {
		q.Set("root_post_id", rootPostID)
	}
	if userID != "" {
		q.Set("user_id", userID)
	}
	body, status, err := p.bridgeGetWithTimeout("/agents/hermes/status", q, statusTimeout)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("bridge status %d", status)
	}
	var out apiPayload
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("%w: %v", errOutcomeUnknown, err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("%s: %s", out.Error, out.Message)
	}
	return out.Text, nil
}

// gatewayLanguage returns the gateway's display language (ja/en). The console
// renders in the SAME language as the gateway's own /status text; unknown or
// unreachable -> "en" (the agreed fallback). Cached for 60s.
func (p *Plugin) gatewayLanguage() string {
	p.langMu.Lock()
	defer p.langMu.Unlock()
	if p.lang != "" && time.Since(p.langAt) < 60*time.Second {
		setLanguage(p.lang)
		return p.lang
	}
	// A failed fetch keeps the last known language; "en" only when none is known.
	lang := p.lang
	if lang == "" {
		lang = "en"
	}
	body, status, err := p.bridgeGet("/agents/hermes/language", nil)
	if err == nil && status < 400 {
		var out struct {
			Language string `json:"language"`
		}
		if json.Unmarshal(body, &out) == nil && out.Language != "" {
			lang = normalizeLanguage(out.Language)
		}
	}
	p.lang = lang
	p.langAt = time.Now()
	setLanguage(lang)
	return lang
}

func (p *Plugin) fetchModels() ([]apiModel, error) {
	body, status, err := p.bridgeGet("/agents/hermes/models", nil)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("bridge status %d", status)
	}
	var out struct {
		Models []apiModel `json:"models"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

// callBridgeAction performs a write operation. Returns the parsed payload and
// the HTTP status; a non-nil error means the request never completed.
func (p *Plugin) actionClient() *http.Client {
	_, _, c := p.bridgeConfig()
	if c == nil {
		return nil
	}
	cp := *c
	if cp.Timeout < actionTimeout {
		cp.Timeout = actionTimeout
	}
	return &cp
}

func (p *Plugin) callBridgeAction(channelID, rootPostID, userID, action, args, idem string) (*apiPayload, int, error) {
	base, token, _ := p.bridgeConfig()
	client := p.actionClient()
	if base == "" || client == nil {
		return nil, 0, fmt.Errorf("bridge unavailable")
	}
	// A guessed chat_type targets a different session. Better to refuse than to
	// mutate a phantom one (review m3).
	chatType, ok := p.channelTypeOf(channelID)
	if !ok {
		return nil, 0, errChatTypeUnknown
	}

	reqBody := map[string]any{
		"channel_id":   channelID,
		"root_post_id": rootPostID,
		"user_id":      userID,
		"chat_type":    chatType,
		"action":       action,
		"args":         args,
	}
	if idem != "" {
		reqBody["idempotency_key"] = idem
	}
	raw, _ := json.Marshal(reqBody)

	req, err := http.NewRequest(http.MethodPost, base+"/agents/hermes/session/actions", bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		// The request reached the bridge; the outcome is unknown, not a failure.
		return nil, resp.StatusCode, fmt.Errorf("%w: %v", errOutcomeUnknown, readErr)
	}

	out := &apiPayload{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return nil, resp.StatusCode, fmt.Errorf("%w: %v", errOutcomeUnknown, err)
		}
	}
	return out, resp.StatusCode, nil
}

// actionIDFromPath tolerates both a path suffix and the request data field.
func actionIDFromPath(path, data string) string {
	if data != "" {
		return data
	}
	id := strings.TrimPrefix(path, "/plugins/com.nicox.hermes-console/api/v1/actions/")
	return strings.TrimSuffix(id, "/")
}
