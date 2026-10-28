package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// testModels mirrors the bridge payload shape: flat list, provider embedded.
var testModels = []apiModel{
	{Key: "qwen38:model-a", DisplayName: "Model A", ProviderDisplayName: "ThinkStation PGX"},
	{Key: "qwen38:model-b", DisplayName: "Model B", ProviderDisplayName: "ThinkStation PGX"},
	{Key: "spark:model-c", DisplayName: "Model C", ProviderDisplayName: "DGX Spark"},
}

func TestGroupProvidersKeepsOrderAndKeys(t *testing.T) {
	groups := groupProviders(testModels)
	require.Len(t, groups, 2)
	assert.Equal(t, "qwen38", groups[0].Key)
	assert.Equal(t, "ThinkStation PGX", groups[0].Display)
	assert.Len(t, groups[0].Models, 2)
	assert.Equal(t, "spark", groups[1].Key)
	assert.Len(t, groups[1].Models, 1)
}

func TestGroupProvidersEmpty(t *testing.T) {
	assert.Empty(t, groupProviders(nil))
}

func TestGroupForPrefersSelectionThenCurrentThenFirst(t *testing.T) {
	groups := groupProviders(testModels)

	got, ok := groupFor(groups, "spark", "qwen38")
	require.True(t, ok)
	assert.Equal(t, "spark", got.Key, "explicit selection wins over current")

	got, ok = groupFor(groups, "", "spark")
	require.True(t, ok)
	assert.Equal(t, "spark", got.Key, "falls back to the session's provider")

	got, ok = groupFor(groups, "", "")
	require.True(t, ok)
	assert.Equal(t, "qwen38", got.Key, "falls back to the first provider")

	_, ok = groupFor(nil, "", "")
	assert.False(t, ok)
}

func TestProviderOptionsAreNonEmptyAndMarkCurrent(t *testing.T) {
	setLanguage("ja")
	groups := groupProviders(testModels)
	opts := providerOptions(groups, "spark", "")
	require.Len(t, opts, 2)

	// Current provider first, every value non-empty (Mattermost rejects "").
	assert.Equal(t, "spark", opts[0].Value)
	assert.Contains(t, opts[0].Text, "（現在）")
	assert.Equal(t, "qwen38", opts[1].Value)
	assert.NotContains(t, opts[1].Text, "（現在）")
	for _, o := range opts {
		assert.NotEmpty(t, o.Value, "select option value must be non-empty")
	}
	assert.Contains(t, opts[1].Text, "2モデル")
}

func TestProviderOptionsEmptyStillUsable(t *testing.T) {
	opts := providerOptions(nil, "", "")
	require.Len(t, opts, 1)
	assert.NotEmpty(t, opts[0].Value)
}

// The model select lists ONLY the panel's provider: a provider switch posts a
// fresh panel, so the options never have to be swapped in place.
func TestModelOptionsListOnlyThePanelProvider(t *testing.T) {
	groups := groupProviders(testModels)
	group, ok := groupFor(groups, "spark", "qwen38")
	require.True(t, ok)

	opts := modelOptions(group, ok, "qwen38:model-a")
	require.Len(t, opts, 1)
	assert.Equal(t, "spark:model-c", opts[0].Value)
	assert.Equal(t, "Model C", opts[0].Text)
}

func TestModelOptionsPutsCurrentFirst(t *testing.T) {
	setLanguage("ja")
	groups := groupProviders(testModels)
	group, ok := groupFor(groups, "", "qwen38")
	require.True(t, ok)

	opts := modelOptions(group, ok, "qwen38:model-b")
	require.Len(t, opts, 2)
	assert.Equal(t, "qwen38:model-b", opts[0].Value, "current model first overall")
	assert.Contains(t, opts[0].Text, "（現在）")
	assert.Equal(t, "qwen38:model-a", opts[1].Value)
	assert.NotContains(t, opts[1].Text, "（現在）")
}

func TestModelOptionsWithoutProvider(t *testing.T) {
	opts := modelOptions(providerGroup{}, false, "")
	require.Len(t, opts, 1)
	assert.NotEmpty(t, opts[0].Value)
}

func TestProviderKeyOf(t *testing.T) {
	assert.Equal(t, "qwen38", providerKeyOf(apiModel{Key: "qwen38:model-a"}))
	assert.Equal(t, "ThinkStation PGX", providerKeyOf(apiModel{Key: "model-a", ProviderDisplayName: "ThinkStation PGX"}))
}

func TestPanelActionsRespectTheFiveActionLimit(t *testing.T) {
	p, _ := newTestPlugin(t, "", configuration{})
	atts := p.panelAttachments(testChannel, "", testUser, "", "", viewAll, false)
	require.Len(t, atts, 2)

	total := 0
	for _, a := range atts {
		assert.LessOrEqual(t, len(a.Actions), 5, "an attachment holds at most 5 PostActions")
		total += len(a.Actions)
	}
	assert.Equal(t, 4, total, "provider select + model select + apply + refresh (no reset/compress)")
}

// `/hermes compress` posts ONE plain line (no panel): nicox pointed out that a
// session panel around a fire-and-forget operation carries no information.
func TestCompressPostsOnePlainLine(t *testing.T) {
	p, api := newTestPlugin(t, "", configuration{})
	api.On("GetChannelMember", testChannel, testUser).Return(&model.ChannelMember{}, nil)
	var mu sync.Mutex
	var posts []*model.Post
	api.On("CreatePost", mock.Anything).Run(func(args mock.Arguments) {
		mu.Lock()
		posts = append(posts, args.Get(0).(*model.Post))
		mu.Unlock()
	}).Return(&model.Post{Id: "panel-1"}, nil)
	api.On("GetPost", mock.Anything).Return(&model.Post{Id: "panel-1", Props: map[string]any{}}, nil)
	api.On("UpdatePost", mock.Anything).Return(&model.Post{Id: "panel-1"}, nil)

	_, appErr := p.ExecuteCommand(nil, &model.CommandArgs{
		UserId: testUser, ChannelId: testChannel, RootId: testRoot, Command: "/hermes compress",
	})
	require.Nil(t, appErr)

	mu.Lock()
	defer mu.Unlock()
	var action *model.Post
	for _, post := range posts {
		if ok, _ := post.Props["hermes_console_action"].(bool); ok {
			action = post
		}
		if ok, _ := post.Props["hermes_console"].(bool); ok {
			t.Fatalf("compress must not post a panel: %v", post.Props["view"])
		}
	}
	require.NotNil(t, action, "compress posts one line that becomes its result")
	_, hasAtts := action.Props["attachments"]
	assert.False(t, hasAtts, "the line carries no attachments")
	assert.Contains(t, action.Message, "圧縮")
}

// The scope label must match the session config: threads are shared, channel
// roots are per-user (group_sessions_per_user: true).
func TestScopeLabelMatchesSessionConfig(t *testing.T) {
	assert.Equal(t, "スレッド（スレッド内で共有）", scopeLabel("root-1"))
	assert.Equal(t, "チャンネル直下（ユーザーごと）", scopeLabel(""))
}

// `/hermes status` is read-only: reset/compress are gone from the panel, and the
// status word posts the gateway's /status text instead of a panel.
func TestStatusViewIsReadOnly(t *testing.T) {
	p, _ := newTestPlugin(t, "", configuration{})
	atts := p.panelAttachments(testChannel, testRoot, testUser, "", "", viewSession, false)
	require.Len(t, atts, 1)
	for _, act := range atts[0].Actions {
		assert.NotEqual(t, actReset, act.Id, "reset is not needed in the panel")
		assert.NotEqual(t, actCompress, act.Id, "compress is not needed in the panel")
	}

	for _, arg := range []string{"status", "状態", "session"} {
		assert.True(t, isStatusWord(arg), "arg %q", arg)
	}
	for _, arg := range []string{"", "model", "reset", "nonsense"} {
		assert.False(t, isStatusWord(arg), "arg %q", arg)
	}
}

func TestApplyDisabledUntilModelPicked(t *testing.T) {
	p, _ := newTestPlugin(t, "", configuration{})

	findApply := func(atts []*model.MessageAttachment) *model.PostAction {
		for _, a := range atts {
			for _, act := range a.Actions {
				if act.Id == actApplyModel {
					return act
				}
			}
		}
		return nil
	}

	// Without a bridge the model list is empty, so no pick is possible: apply
	// stays disabled rather than silently applying a stale value.
	atts := p.panelAttachments(testChannel, "", testUser, "", "", viewAll, false)
	apply := findApply(atts)
	require.NotNil(t, apply)
	assert.True(t, apply.Disabled)
}

// postResult must land in the same thread as the panel, so the operation trail
// stays readable after the panel re-renders.
func TestPostResultPostsIntoThePanelThread(t *testing.T) {
	p, api := newTestPlugin(t, "", configuration{HermesBotID: "bot-1"})
	api.On("CreatePost", mock.Anything).Return(&model.Post{Id: "result-1"}, nil)

	p.postResult(testChannel, testRoot, "✅ モデルを適用しました")

	found := false
	for _, c := range api.Calls {
		if c.Method == "CreatePost" {
			post := c.Arguments.Get(0).(*model.Post)
			assert.Equal(t, testChannel, post.ChannelId)
			assert.Equal(t, testRoot, post.RootId, "result must stay in the panel's thread")
			assert.Equal(t, "bot-1", post.UserId)
			assert.Contains(t, post.Message, "モデルを適用しました")
			found = true
		}
	}
	assert.True(t, found, "expected a CreatePost call")
}

func TestApplyModelArgsPinsTheProvider(t *testing.T) {
	// Custom endpoint: --provider is still correct and keeps routing explicit.
	assert.Equal(t, "gemma-4-26B-A4B-it --provider qwen38", applyModelArgs("qwen38:gemma-4-26B-A4B-it"))
	// Built-in providers must not fall through to the current provider.
	assert.Equal(t, "gpt-6-sol --provider openai-codex", applyModelArgs("openai-codex:gpt-6-sol"))
	// Aggregator slugs keep their own colons intact.
	assert.Equal(t, "anthropic/claude-sonnet-5.5 --provider openrouter",
		applyModelArgs("openrouter:anthropic/claude-sonnet-5.5"))
	assert.Equal(t, "hermes3:70b --provider openrouter", applyModelArgs("openrouter:hermes3:70b"))
	// A bare model (no provider) is passed through untouched.
	assert.Equal(t, "gpt-6-sol", applyModelArgs("gpt-6-sol"))
	assert.Equal(t, "", applyModelArgs(""))
}

// A provider switch must POST A NEW PANEL and delete the old one: Mattermost
// builds a select's options on mount, so editing the post leaves the previous
// provider's model list on screen.
func TestProviderSwitchSwapsModelOptionsInPlace(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"key": "qwen38:m-a", "model": "m-a", "display_name": "M A", "provider": "qwen38", "provider_display_name": "PGX"},
				{"key": "openrouter:m-b", "model": "m-b", "display_name": "M B", "provider": "openrouter", "provider_display_name": "OpenRouter"},
			},
		})
	}))
	defer bridge.Close()

	p, api := newTestPlugin(t, bridge.URL, configuration{HermesBotID: "bot-1"})
	post := &model.Post{
		Id:        "panel-1",
		ChannelId: testChannel,
		Props: map[string]any{
			"hermes_console":     true,
			"target_channel":    testChannel,
			"target_root":       "",
			"target_user":       testUser,
			"selected_provider": "",
			"selected_model":    "",
		},
	}
	api.On("GetPost", "panel-1").Return(post, nil)
	api.On("GetUser", testUser).Return(&model.User{Id: testUser}, nil)
	api.On("UpdatePost", mock.Anything).Return(post, nil)
	api.On("LogInfo", mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, mock.Anything).Return()

	body, err := json.Marshal(model.PostActionIntegrationRequest{
		PostId:  "panel-1",
		UserId:  testUser,
		Context: map[string]any{"action_id": actSelectProvider, "selected_option": "openrouter"},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/plugins/com.nicox.hermes-console/api/v1/actions", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	p.handleAction(rec, req)

	// No re-post: a new post would notify the user's other devices.
	for _, c := range api.Calls {
		assert.NotEqual(t, "CreatePost", c.Method, "provider switch must not re-post")
	}

	var updated *model.Post
	for _, c := range api.Calls {
		if c.Method == "UpdatePost" {
			updated = c.Arguments.Get(0).(*model.Post)
		}
	}
	require.NotNil(t, updated)
	assert.Equal(t, "openrouter", updated.Props["selected_provider"])
	atts := updated.Props["attachments"].([]*model.MessageAttachment)
	menu := atts[0].Actions[1]
	assert.Equal(t, modelActionID("openrouter"), menu.Id, "model menu id must change so the client remounts it")
	require.Len(t, menu.Options, 1, "only the chosen provider's models")
	assert.Equal(t, "openrouter:m-b", menu.Options[0].Value)
}

func TestModelActionIDIsPathSafeAndPerProvider(t *testing.T) {
	assert.Equal(t, "selectmodelqwen38", modelActionID("qwen38"))
	assert.Equal(t, "selectmodelopenaicodex", modelActionID("openai-codex"))
	assert.NotEqual(t, modelActionID("openrouter"), modelActionID("qwen38"))
}

// `/hermes model` must show ONLY the model picker: nicox asked for the word after
// /hermes to narrow the surface instead of always posting every attachment.
func TestAgentArgumentNarrowsThePanel(t *testing.T) {
	cases := map[string]string{
		"":         viewAll,
		"model":    viewModel,
		"モデル":      viewModel,
		"status":   viewSession,
		"状態":       viewSession,
		"reset":    viewSession,
		"compress": viewSession,
		"nonsense": viewAll,
	}
	for arg, want := range cases {
		assert.Equal(t, want, viewFromArg(arg), "arg %q", arg)
	}

	p, _ := newTestPlugin(t, "", configuration{})

	modelAtts := p.panelAttachments(testChannel, "", testUser, "", "", viewModel, false)
	require.Len(t, modelAtts, 1, "model view keeps a single attachment")
	assert.Equal(t, "モデル", modelAtts[0].Title)

	sessionAtts := p.panelAttachments(testChannel, "", testUser, "", "", viewSession, false)
	require.Len(t, sessionAtts, 1, "session view keeps a single attachment")
	assert.Equal(t, "セッション", sessionAtts[0].Title)

	assert.Len(t, p.panelAttachments(testChannel, "", testUser, "", "", viewAll, false), 2,
		"no argument keeps the full console")
}

// A view must survive the provider pick (the panel is edited in place, not
// re-posted), otherwise the narrowed panel would balloon back to two blocks.
func TestViewSurvivesProviderPick(t *testing.T) {
	p, api := newTestPlugin(t, "", configuration{})
	post := &model.Post{
		Id: "panel-1",
		Props: map[string]any{
			"target_channel": testChannel,
			"target_root":    "",
			"selected_model": "",
			"view":           viewModel,
		},
	}
	api.On("UpdatePost", mock.Anything).Return(post, nil)

	updated := p.renderPanel(post)
	atts, _ := updated.Props["attachments"].([]*model.MessageAttachment)
	require.Len(t, atts, 1)
	assert.Equal(t, "モデル", atts[0].Title)
}
