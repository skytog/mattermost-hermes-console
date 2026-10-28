// Package main implements the Mattermost Server Plugin for Hermes Console.
//
// It is the only browser-facing layer: it authenticates the Mattermost user,
// checks channel access and allowlists, then relays to the loopback Hermes
// Control Bridge. The actor is attached server-side; the browser never sends it.
package main

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// configuration mirrors settings_schema in plugin.json.
type configuration struct {
	BridgeURL       string
	BridgeToken     string
	AllowedUsers    string
	AllowedChannels string
	HermesBotID     string
}

// Plugin is the server plugin.
type Plugin struct {
	plugin.MattermostPlugin

	mu sync.RWMutex

	cfg    configuration
	client *http.Client

	// ops reserves a target session for ONE in-flight operation. UI-level
	// Disabled/busy is only a hint: two clicks can arrive before the first
	// render lands, so the server must reserve atomically before spawning the
	// goroutine (review M4).
	opsMu sync.Mutex
	ops   map[string]bool

	// postLocks serialises read-modify-write on one panel post: a select, a
	// refresh and an operation completion all rewrite the same post, and a late
	// write used to roll back busy/selection (review M4).
	postMu    sync.Mutex
	postLocks map[string]*sync.Mutex

	// The console's language follows the gateway's display.language; cached
	// briefly so a panel render does not fetch it on every action.
	langMu sync.Mutex
	lang   string
	langAt time.Time
}

// OnActivate is called when the plugin is activated (and on config change).
func (p *Plugin) OnActivate() error {
	if err := p.loadConfig(); err != nil {
		return err
	}
	p.mu.Lock()
	p.client = &http.Client{Timeout: 10 * time.Second}
	p.mu.Unlock()

	return p.registerCommands()
}

func (p *Plugin) loadConfig() error {
	var cfg configuration
	if err := p.API.LoadPluginConfiguration(&cfg); err != nil {
		return err
	}
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
	return nil
}

// ConfigurationWillBeSaved reloads on System Console save.
func (p *Plugin) ConfigurationWillBeSaved(_ *model.Config) bool {
	_ = p.loadConfig()
	return true
}

func (p *Plugin) bridgeConfig() (string, string, *http.Client) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	url := strings.TrimRight(p.cfg.BridgeURL, "/")
	token := p.cfg.BridgeToken
	if url == "" {
		url = strings.TrimRight(os.Getenv("MM_BRIDGE_URL"), "/")
	}
	if token == "" {
		token = os.Getenv("MM_BRIDGE_TOKEN")
	}
	return url, token, p.client
}

func splitList(raw string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out[v] = true
		}
	}
	return out
}

// statusTimeout must be LONGER than the bridge's own status wait (20s), or the
// plugin gives up first and `/hermes status` falls back to a panel even though
// the bridge would have answered (review m4: inner 20s > outer 10s).
const statusTimeout = 25 * time.Second

// statusClient returns a client whose deadline outlives the bridge's status wait.
func (p *Plugin) statusClient() *http.Client {
	_, _, c := p.bridgeConfig()
	if c == nil {
		return nil
	}
	cp := *c
	if cp.Timeout < statusTimeout {
		cp.Timeout = statusTimeout
	}
	return &cp
}

func (p *Plugin) allowlists() (map[string]bool, map[string]bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	usersRaw, channelsRaw := p.cfg.AllowedUsers, p.cfg.AllowedChannels
	if usersRaw == "" {
		usersRaw = os.Getenv("MM_ALLOWED_USERS")
	}
	if channelsRaw == "" {
		channelsRaw = os.Getenv("MM_ALLOWED_CHANNELS")
	}
	return splitList(usersRaw), splitList(channelsRaw)
}
