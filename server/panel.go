package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// Panel is the in-chat control surface (SPEC §5.3). It replaces the RHS: the
// Mattermost right sidebar is a single slot and collides with the thread view.
//
// The panel is one post per thread, edited in place on every interaction.

const panelTitle = "Hermes Console"

// action ids used inside the panel.
const (
	actSelectProvider = "select_provider"
	actSelectModel    = "select_model"
	// The model select's action id carries the provider, because Mattermost keys
	// <ActionMenu> by action.id and builds its option providers in the constructor:
	// a changed id remounts the menu, which is the ONLY way to swap its options
	// without re-posting. (ActionMenu.getDerivedStateFromProps syncs `selected`
	// only; `this.providers` is never rebuilt.)
	actSelectModelPrefix = "selectmodel"
	actApplyModel        = "apply_model"
	actReset             = "reset_session"
	actCompress          = "compress_session"
	actRefresh           = "refresh"
)

// Panel views. `/hermes model` shows ONLY the model picker and `/hermes status`
// only the session block; no argument keeps the full console. nicox asked for
// the command to be able to narrow the surface instead of always posting every
// attachment.
const (
	viewAll     = ""
	viewModel   = "model"
	viewSession = "session"
)

// isStatusWord reports whether the word after `/hermes` asks for the read-only
// status view (`/hermes status`), which posts text instead of a panel.
func isStatusWord(arg string) bool {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "status", "state", "session", "状態":
		return true
	}
	return false
}

// scopeLabel says which session the panel targets and who shares it. Thread
// sessions are shared (thread_sessions_per_user defaults false), while a
// channel-root session is PER-USER (group_sessions_per_user: true) — so the old
// "チャンネルで共有" label claimed sharing that does not exist.
func scopeLabel(rootPostID string) string {
	if rootPostID != "" {
		return tr("スレッド（スレッド内で共有）")
	}
	return tr("チャンネル直下（ユーザーごと）")
}

// viewFromArg maps the word after `/hermes` to a panel view. Unknown words fall
// back to the full panel, so a typo still leaves a usable surface.
func viewFromArg(arg string) string {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "model", "models", "モデル":
		return viewModel
	case "status", "state", "session", "reset", "compress", "リセット", "圧縮", "状態":
		return viewSession
	}
	return viewAll
}

func actionURL() string { return "/plugins/com.nicox.hermes-console/api/v1/actions" }

// modelActionID is the model select's per-provider action id (letters+digits
// only: the id travels in the integration URL path).
func modelActionID(providerKey string) string {
	var b strings.Builder
	b.WriteString(actSelectModelPrefix)
	for _, r := range strings.ToLower(providerKey) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// registerCommands declares the slash commands (autocomplete, no manual typing).
// Registered per team: an unset team_id leaves the client's cached command list
// without a team entry, so typing `/hermes` never reaches the server.
func (p *Plugin) registerCommands() error {
	defs := []struct {
		trigger string
		hint    string
		desc    string
	}{
		{"hermes", "[status|model|reset|compress]", tr("Hermes Console を開く（status は /status と同じ本文）")},
		{"hermes-status", "", tr("状態を表示（/status と同じ本文）")},
		{"hermes-model", "", tr("モデルを選んで切り替える")},
		{"hermes-reset", "", tr("この会話をリセット")},
		{"hermes-compress", "", tr("この会話のコンテキストを圧縮")},
	}

	teams, appErr := p.API.GetTeams()
	if appErr != nil {
		return appErr
	}

	for _, team := range teams {
		for _, d := range defs {
			cmd := &model.Command{
				TeamId:           team.Id,
				Trigger:          d.trigger,
				Method:           model.CommandMethodPost,
				AutoComplete:     true,
				AutoCompleteHint: d.hint,
				AutoCompleteDesc: d.desc,
				DisplayName:      "Hermes Console",
				Description:      d.desc,
			}
			if appErr := p.API.RegisterCommand(cmd); appErr != nil {
				return appErr
			}
		}
	}
	return nil
}

// ExecuteCommand routes the slash commands. The panel is posted first so every
// command has a live control surface, then the requested operation runs on it.
func (p *Plugin) ExecuteCommand(_ *plugin.Context, args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	// CommandArgs carries the WHOLE typed line ("/hermes model"), not a separate
	// argument field, so split it: first word = trigger, second = panel view.
	fields := strings.Fields(strings.TrimPrefix(args.Command, "/"))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "hermes") {
		return &model.CommandResponse{}, nil
	}
	trigger := fields[0]
	arg := ""
	if len(fields) > 1 {
		arg = fields[1]
	}

	p.API.LogInfo("hermes-console: command", "trigger", trigger, "channel", args.ChannelId, "user", args.UserId, "root", args.RootId)

	userID := args.UserId
	p.gatewayLanguage()
	if !p.authorizeCommand(userID, args.ChannelId) {
		p.API.LogWarn("hermes-console: command denied", "user", userID, "channel", args.ChannelId)
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         tr("Hermes Console: このユーザーまたはチャンネルは許可されていません。"),
		}, nil
	}

	// The target is exactly where the command was typed: RootId is set inside a
	// thread, empty at the channel root. Do NOT guess "the user's active thread":
	// a channel-level /hermes used to land in the thread above it (nicox).
	rootPostID := args.RootId
	// `/hermes status` is read-only: it posts the gateway's OWN /status text plus
	// the console's scope/session facts, and posts no panel (nicox does not want
	// reset/compress controls there). Everything else opens a panel.
	actionArg := ""
	if trigger == "hermes" {
		switch strings.ToLower(arg) {
		case "reset", "リセット":
			actionArg = actReset
		case "compress", "圧縮":
			actionArg = actCompress
		}
	}
	isStatus := trigger == "hermes-status" ||
		(trigger == "hermes" && actionArg == "" && isStatusWord(arg))

	view := viewAll
	switch {
	case trigger == "hermes":
		view = viewFromArg(arg)
	case trigger == "hermes-model":
		view = viewModel
	case trigger == "hermes-status" || trigger == "hermes-reset" || trigger == "hermes-compress":
		view = viewSession
	}
	if actionArg != "" {
		view = viewSession
	}
	if isStatus {
		if text := p.statusMessage(args.ChannelId, rootPostID, userID); text != "" {
			p.postResult(args.ChannelId, rootPostID, text)
			return &model.CommandResponse{}, nil
		}
		// Say WHY there is no status instead of silently posting a panel: the
		// panel looked like the command's normal output (nicox hit this).
		p.postResult(args.ChannelId, rootPostID, tr("⚠️ statusを取得できませんでした（ブリッジが応答しません）。"))
		return &model.CommandResponse{}, nil
	}

	// Compress/reset are fire-and-forget operations: a control panel around them
	// carries no information (nicox: "これなんの意味ある？"). Post ONE line and
	// turn that same post into the result when the operation finishes.
	standalone := actionArg
	switch trigger {
	case "hermes-compress":
		standalone = actCompress
	case "hermes-reset":
		standalone = actReset
	}
	if standalone != "" {
		p.runStandaloneAction(args.ChannelId, rootPostID, userID, standalone)
		return &model.CommandResponse{}, nil
	}

	panelID, appErr := p.postPanel(args.ChannelId, rootPostID, userID, view)
	if appErr != nil {
		p.API.LogError("hermes-console: panel post failed", "err", appErr.Error())
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         fmt.Sprintf(tr("Hermes Console: パネルを投稿できませんでした (%s)"), appErr.Error()),
		}, nil
	}

	switch trigger {
	case "hermes-model":
		p.setSelectedModel(panelID, "")
	case "hermes-reset":
		p.runPanelAction(panelID, args.ChannelId, rootPostID, userID, actReset, "", "")
	case "hermes-compress":
		p.runPanelAction(panelID, args.ChannelId, rootPostID, userID, actCompress, "", "")
	case "hermes-status":
		// read-only: handled above, no panel
	case "hermes":
		// `/hermes reset` / `/hermes compress` run straight away; the session
		// block they posted stays as the control surface.
		switch actionArg {
		case actReset:
			p.runPanelAction(panelID, args.ChannelId, rootPostID, userID, actReset, "", "")
		case actCompress:
			p.runPanelAction(panelID, args.ChannelId, rootPostID, userID, actCompress, "", "")
		}
	}
	return &model.CommandResponse{}, nil
}

// authorizeCommand mirrors the HTTP allowlist gate for slash commands.
func (p *Plugin) authorizeCommand(userID, channelID string) bool {
	if _, appErr := p.API.GetChannelMember(channelID, userID); appErr != nil {
		return false
	}
	users, channels := p.allowlists()
	if len(users) > 0 && !users[userID] {
		return false
	}
	if len(channels) > 0 && !channels[channelID] {
		return false
	}
	return true
}

// postPanel creates the panel post and returns its id. A provider switch posts a
// NEW panel (see handleAction): Mattermost freezes a select's options on mount, so
// only a fresh post can show a different provider's model list.
func (p *Plugin) postPanel(channelID, rootPostID, userID, view string) (string, *model.AppError) {
	return p.postPanelFor(channelID, rootPostID, userID, "", "", view)
}

// postPanelFor creates a panel already scoped to a provider/model selection.
func (p *Plugin) postPanelFor(channelID, rootPostID, userID, selProvider, selModel, view string) (string, *model.AppError) {
	post := &model.Post{
		ChannelId: channelID,
		UserId:    p.botUserID(),
		RootId:    rootPostID,
		Props: map[string]any{
			"hermes_console":     true,
			"target_channel":    channelID,
			"target_root":       rootPostID,
			"target_user":       userID,
			"selected_provider": selProvider,
			"selected_model":    selModel,
			"view":              view,
			"last_result":       "",
			"attachments":       p.panelAttachments(channelID, rootPostID, userID, selProvider, selModel, view, false),
		},
	}
	created, appErr := p.API.CreatePost(post)
	if appErr != nil {
		return "", appErr
	}
	if created == nil {
		return "", nil
	}
	// The bridge comes up ~20s after a gateway restart, so a panel opened in that
	// window renders empty. Self-heal instead of making the user hit 状態を更新.
	if _, err := p.fetchModels(); err != nil {
		p.scheduleModelRetry(created.Id, 0)
	}
	return created.Id, nil
}

// scheduleModelRetry re-renders a panel whose model fetch failed, every few
// seconds, until the bridge answers. Bounded so a dead bridge cannot loop.
func (p *Plugin) scheduleModelRetry(postID string, attempt int) {
	const (
		// A gateway restart keeps the bridge down for tens of seconds; 12 attempts
		// (36s) expired before it returned. 30 attempts = 90s.
		maxAttempts = 30
		interval    = 3 * time.Second
	)
	if attempt >= maxAttempts {
		return
	}
	time.AfterFunc(interval, func() {
		post, appErr := p.API.GetPost(postID)
		if appErr != nil || post.DeleteAt != 0 {
			return
		}
		if _, err := p.fetchModels(); err != nil {
			p.scheduleModelRetry(postID, attempt+1)
			return
		}
		p.API.LogInfo("hermes-console: bridge recovered", "post", postID, "attempt", attempt+1)
		p.rerender(post)
	})
}

// providerGroup collapses the flat model list into one entry per provider,
// preserving the order declared in config.yaml.
type providerGroup struct {
	Key     string
	Display string
	Models  []apiModel
}

// providerKeyOf extracts the provider key from a "provider:model" key.
func providerKeyOf(m apiModel) string {
	if key, _, ok := strings.Cut(m.Key, ":"); ok {
		return key
	}
	return m.ProviderDisplayName
}

func groupProviders(models []apiModel) []providerGroup {
	index := map[string]int{}
	groups := []providerGroup{}
	for _, m := range models {
		key := providerKeyOf(m)
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, providerGroup{Key: key, Display: m.ProviderDisplayName})
		}
		groups[i].Models = append(groups[i].Models, m)
	}
	return groups
}

// groupFor picks the provider the model select should list: the explicit
// selection first, else the session's current provider, else the first group.
func groupFor(groups []providerGroup, selected, current string) (providerGroup, bool) {
	for _, g := range groups {
		if selected != "" && g.Key == selected {
			return g, true
		}
	}
	for _, g := range groups {
		if current != "" && g.Key == current {
			return g, true
		}
	}
	if len(groups) > 0 {
		return groups[0], true
	}
	return providerGroup{}, false
}

// providerOptions lists providers, current one first. Every option carries a
// NON-EMPTY value: an empty value makes Mattermost reject the option.
func providerOptions(groups []providerGroup, current, selected string) []*model.PostActionOptions {
	if len(groups) == 0 {
		return []*model.PostActionOptions{{Text: tr("(プロバイダーを取得できません)"), Value: "none"}}
	}
	lead := current
	if selected != "" {
		lead = selected
	}
	out := []*model.PostActionOptions{}
	appendGroup := func(g providerGroup) {
		text := fmt.Sprintf(tr("%s（%dモデル）"), g.Display, len(g.Models))
		switch {
		case selected != "" && g.Key == selected:
			text += tr("（選択中）")
		case selected == "" && g.Key == current:
			text += tr("（現在）")
		}
		out = append(out, &model.PostActionOptions{Text: text, Value: g.Key})
	}
	for _, g := range groups {
		if g.Key == lead {
			appendGroup(g)
		}
	}
	for _, g := range groups {
		if g.Key != lead {
			appendGroup(g)
		}
	}
	return out
}

// modelOptions lists the models of ONE provider, current model first so the
// default pick stays actionable. Provider is already chosen one step above, so
// the labels stay short.
// modelOptions lists ONE provider's models, current first. The provider step
// posts a FRESH panel for the chosen provider, because Mattermost freezes a
// select's options on mount: re-rendering the same post never swaps them, so a
// narrowed list has to come from a new post, not an update.
func modelOptions(group providerGroup, haveGroup bool, current string) []*model.PostActionOptions {
	if !haveGroup || len(group.Models) == 0 {
		return []*model.PostActionOptions{{Text: tr("(モデルを取得できません)"), Value: "none"}}
	}
	out := []*model.PostActionOptions{}
	for _, m := range group.Models {
		if m.Key == current {
			out = append(out, &model.PostActionOptions{Text: m.DisplayName + tr("（現在）"), Value: m.Key})
		}
	}
	for _, m := range group.Models {
		if m.Key != current {
			out = append(out, &model.PostActionOptions{Text: m.DisplayName, Value: m.Key})
		}
	}
	return out
}

// panelAttachments renders the control surface for the post's target.
//
// The model picker is a two-step flow (provider, then model, then apply), so it
// needs more actions than one attachment allows: a Mattermost attachment holds
// at most 5 PostActions and the panel has 6. Hence two attachments.
func (p *Plugin) panelAttachments(channelID, rootPostID, userID, selProvider, selModel, view string, busy bool) []*model.MessageAttachment {
	// Render in the gateway's display language, not the Mattermost user's.
	p.gatewayLanguage()
	st, stateErr := p.fetchState(channelID, rootPostID, userID)
	models, modelsErr := p.fetchModels()
	// The bridge can be briefly down (gateway restart) right when a panel is
	// posted; one short retry keeps the picker from rendering empty.
	if modelsErr != nil {
		time.Sleep(300 * time.Millisecond)
		if again, err := p.fetchModels(); err == nil {
			models, modelsErr = again, nil
		}
	}
	groups := groupProviders(models)

	currentKey, currentProvider := "", ""
	if stateErr == nil {
		currentKey = st.Model.Key
		currentProvider = providerKeyOf(apiModel{Key: st.Model.Key, ProviderDisplayName: st.Model.ProviderDisplayName})
	}

	scope := scopeLabel(rootPostID)

	currentLabel := "—"
	activity := tr("未確認")
	session := tr("未作成")
	if stateErr == nil {
		currentLabel = fmt.Sprintf("%s / %s", st.Model.ProviderDisplayName, st.Model.DisplayName)
		activity = activityJP(st.Activity)
		session = presence(st.SessionExists)
	}

	group, haveGroup := groupFor(groups, selProvider, currentProvider)

	// Show the pick so far, so a re-render (selects reset visually) keeps state.
	// Nothing picked yet = no text: the selects and 適用 button already say what
	// to do, and nicox does not want the step-by-step ①②③ line repeated.
	pick := ""
	if selProvider != "" && haveGroup {
		pick = fmt.Sprintf("Provider: %s", group.Display)
	}
	if selModel != "" {
		for _, m := range group.Models {
			if m.Key == selModel {
				pick = fmt.Sprintf(tr("選択中: %s / %s"), group.Display, m.DisplayName)
			}
		}
	}

	modelText := pick
	if modelsErr != nil {
		errLine := fmt.Sprintf(tr("モデル一覧の取得に失敗: %s\n→「状態を更新」で再取得できます"), modelsErr.Error())
		if pick != "" {
			modelText = pick + "\n" + errLine
		} else {
			modelText = errLine
		}
	}

	integration := func(id string) *model.PostActionIntegration {
		return &model.PostActionIntegration{
			URL:     actionURL(),
			Context: map[string]any{"action_id": id},
		}
	}
	button := func(id, name string, disabled bool) *model.PostAction {
		return &model.PostAction{
			Id:          id,
			Name:        name,
			Type:        model.PostActionTypeButton,
			Integration: integration(id),
			Disabled:    disabled,
		}
	}

	modelAtt := &model.MessageAttachment{
		Fallback: panelTitle,
		Title:    tr("モデル"),
		Text:     modelText,
		Fields: []*model.MessageAttachmentField{
			{Title: tr("現在モデル"), Value: currentLabel, Short: true},
			{Title: tr("範囲"), Value: scope, Short: true},
		},
		Actions: []*model.PostAction{
			{
				Id:          actSelectProvider,
				Name:        tr("Provider を選ぶ"),
				Type:        model.PostActionTypeSelect,
				Integration: integration(actSelectProvider),
				Options:     providerOptions(groups, currentProvider, selProvider),
				Disabled:    busy,
			},
			{
				// Id carries the provider so the client remounts the menu and
				// rebuilds its options (see actSelectModelPrefix).
				Id:          modelActionID(group.Key),
				Name:        tr("モデルを選ぶ"),
				Type:        model.PostActionTypeSelect,
				Integration: integration(modelActionID(group.Key)),
				Options:     modelOptions(group, haveGroup, currentKey),
				Disabled:    busy,
			},
			button(actApplyModel, tr("適用"), busy || selModel == ""),
		},
	}

	sessionAtt := &model.MessageAttachment{
		Fallback: panelTitle,
		Title:    tr("セッション"),
		Text:     fmt.Sprintf(tr("対象: %s"), scope),
		Fields: []*model.MessageAttachmentField{
			{Title: tr("稼働状態"), Value: activity, Short: true},
			{Title: tr("会話"), Value: session, Short: true},
		},
		// Read-only: nicox does not want reset/compress controls in the panel.
		// They stay available as `/hermes reset` / `/hermes compress`.
		Actions: []*model.PostAction{
			button(actRefresh, tr("状態を更新"), busy),
		},
	}

	switch view {
	case viewModel:
		return []*model.MessageAttachment{modelAtt}
	case viewSession:
		return []*model.MessageAttachment{sessionAtt}
	}
	return []*model.MessageAttachment{modelAtt, sessionAtt}
}

// setSelectedModel stores the picked model in the post props and re-renders.
func (p *Plugin) setSelectedModel(postID, value string) {
	if value == "none" {
		value = ""
	}
	p.mutatePost(postID, func(post *model.Post) {
		post.Props["selected_model"] = value
		p.renderPanel(post)
	})
}

// rerender rewrites the panel body from its stored target + props.
func (p *Plugin) rerender(post *model.Post) {
	if appErr := p.updatePost(p.renderPanel(post)); appErr != nil {
		p.API.LogError("hermes-console: panel update failed", "err", appErr.Error())
	}
}

// renderPanel writes the attachments for the post's current props and returns
// the post. Selects return it in the integration response's `update` field:
// updating out-of-band with UpdatePost races the client's own post state, which
// made the first provider pick render the PREVIOUS provider's model list.
func (p *Plugin) renderPanel(post *model.Post) *model.Post {
	channelID, _ := post.Props["target_channel"].(string)
	rootPostID, _ := post.Props["target_root"].(string)
	targetUser, _ := post.Props["target_user"].(string)
	selProvider, _ := post.Props["selected_provider"].(string)
	selModel, _ := post.Props["selected_model"].(string)
	last, _ := post.Props["last_result"].(string)
	busy, _ := post.Props["busy"].(bool)
	view, _ := post.Props["view"].(string)

	atts := p.panelAttachments(channelID, rootPostID, targetUser, selProvider, selModel, view, busy)
	if last != "" {
		atts = append(atts, &model.MessageAttachment{
			Fallback: panelTitle,
			Title:    tr("結果"),
			Text:     last,
		})
	}
	post.Props["attachments"] = atts
	return post
}

// bridgeAction maps a panel action id to the bridge's operation name.
var bridgeAction = map[string]string{
	actApplyModel: "switch_model",
	actReset:      "reset_session",
	actCompress:   "compress_session",
}

// applyModelArgs turns the select's "provider:model" value into the argument
// form /model expects. A bare "provider:model" is rewritten by the model
// resolver into a vendor slug and applied to the CURRENT provider, which is wrong
// for a built-in provider (openrouter, openai-codex); "--provider" pins it.
func applyModelArgs(selected string) string {
	if provider, modelName, ok := strings.Cut(selected, ":"); ok && provider != "" && modelName != "" {
		return fmt.Sprintf("%s --provider %s", modelName, provider)
	}
	return selected
}

// postResult leaves a durable record of the operation in the thread. The
// gateway's own /model, /reset and /compress replies stay in history, so a
// panel operation must too — otherwise the "what did I just do?" trail is gone.
func (p *Plugin) postResult(channelID, rootPostID, text string) {
	post := &model.Post{
		ChannelId: channelID,
		UserId:    p.botUserID(),
		RootId:    rootPostID,
		Message:   text,
	}
	if _, appErr := p.API.CreatePost(post); appErr != nil {
		p.API.LogError("hermes-console: result post failed", "err", appErr.Error())
	}
}

// statusMessage returns EXACTLY the gateway's own /status text — no console
// footer: `/hermes-status` is meant to be the same output as typing /status
// (nicox). One retry covers a transient empty answer.
func (p *Plugin) statusMessage(channelID, rootPostID, userID string) string {
	p.gatewayLanguage()
	for attempt := 0; attempt < 2; attempt++ {
		statusText, err := p.fetchStatus(channelID, rootPostID, userID)
		if err == nil && statusText != "" {
			return statusText
		}
		if attempt == 0 {
			time.Sleep(300 * time.Millisecond)
		}
	}
	return ""
}

// opKey identifies one target session for server-side exclusivity.
func opKey(channelID, rootPostID, userID string) string {
	return channelID + "\x00" + rootPostID + "\x00" + userID
}

// reserveOp atomically claims a target session for ONE in-flight operation.
// Two clicks can arrive before the first render lands, so the UI's Disabled is
// only a hint; the server must refuse the second one (review M4).
func (p *Plugin) reserveOp(key string) bool {
	p.opsMu.Lock()
	defer p.opsMu.Unlock()
	if p.ops == nil {
		p.ops = map[string]bool{}
	}
	if p.ops[key] {
		return false
	}
	p.ops[key] = true
	return true
}

func (p *Plugin) releaseOp(key string) {
	p.opsMu.Lock()
	defer p.opsMu.Unlock()
	delete(p.ops, key)
}

// postLock serialises read-modify-write on one panel post. A select, a refresh
// and an operation completion all rewrite the same post; without the lock a late
// write rolled busy/selection back (review M4).
func (p *Plugin) postLock(postID string) *sync.Mutex {
	p.postMu.Lock()
	defer p.postMu.Unlock()
	if p.postLocks == nil {
		p.postLocks = map[string]*sync.Mutex{}
	}
	m, ok := p.postLocks[postID]
	if !ok {
		m = &sync.Mutex{}
		p.postLocks[postID] = m
	}
	return m
}

// updatePost retries a panel write a few times; an API blip must not leave
// "実行中" or a stale selection behind (review m1).
func (p *Plugin) updatePost(post *model.Post) *model.AppError {
	var lastErr *model.AppError
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := p.API.UpdatePost(post); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(150 * time.Millisecond)
	}
	return lastErr
}

// mutatePost re-reads, mutates and writes one panel post under its lock.
func (p *Plugin) mutatePost(postID string, fn func(*model.Post)) *model.Post {
	mu := p.postLock(postID)
	mu.Lock()
	defer mu.Unlock()
	post, appErr := p.API.GetPost(postID)
	if appErr != nil || post == nil {
		p.API.LogError("hermes-console: panel read failed", "post", postID)
		return nil
	}
	if post.Props == nil {
		post.Props = map[string]any{}
	}
	fn(post)
	if appErr := p.updatePost(post); appErr != nil {
		p.API.LogError("hermes-console: panel update failed", "err", appErr.Error())
	}
	return post
}

// runPanelAction relays one operation to the bridge and reflects the result.
// It returns immediately and finishes in the background: compress can take
// minutes, and a synchronous relay left the panel "busy" while Mattermost's own
// request timed out (nicox saw "失敗: context deadline exceeded" while the
// compression was in fact still running).
// The panel locks while the operation runs (no double fire), stays in place
// afterwards (it is the thread's control surface), and every attempt is
// recorded as a separate result post so the thread keeps a readable trail.
func (p *Plugin) runPanelAction(postID, channelID, rootPostID, userID, action, args, idem string) {
	op, ok := bridgeAction[action]
	if !ok {
		return
	}
	key := opKey(channelID, rootPostID, userID)
	if !p.reserveOp(key) {
		// Refuse BEFORE spawning: the previous operation is still in flight.
		p.mutatePost(postID, func(post *model.Post) {
			post.Props["last_result"] = tr("実行中です。少し待ってから再度押してください。")
			p.renderPanel(post)
		})
		return
	}
	// Lock the UI synchronously, not inside the goroutine: a second click can
	// arrive before the goroutine's render lands.
	p.setBusy(postID, true)
	go p.runPanelActionAsync(key, postID, channelID, rootPostID, userID, action, op, args, idem)
}

func (p *Plugin) runPanelActionAsync(key, postID, channelID, rootPostID, userID, action, op, args, idem string) {
	defer p.releaseOp(key)
	payload, status, err := p.callBridgeAction(channelID, rootPostID, userID, op, args, idem)
	p.API.LogInfo("hermes-console: action", "action", action, "op", op, "status", status, "err", err != nil)

	succeeded := true
	result := ""
	switch {
	case err != nil && errors.Is(err, errOutcomeUnknown):
		// The request reached the bridge but the answer was unreadable: the
		// operation may have run, so never auto-retry (review m2).
		result = tr("結果不明（応答を解釈できませんでした）。自動再送はしません。")
		succeeded = false
	case err != nil && errors.Is(err, errChatTypeUnknown):
		result = tr("失敗: チャンネル種別を取得できませんでした（操作は送信していません）")
		succeeded = false
	case err != nil:
		result = fmt.Sprintf(tr("失敗: %s"), err.Error())
		succeeded = false
	case status == http.StatusConflict:
		result = tr("実行中です。少し待ってから再度押してください。")
		succeeded = false
	case status == http.StatusGatewayTimeout:
		result = tr("結果不明（タイムアウト）。自動再送はしません。")
		succeeded = false
	case status >= 400:
		result = fmt.Sprintf(tr("失敗 (%d): %s"), status, payload.Message)
		succeeded = false
	default:
		result = payloadText(action, payload.Text)
		// A 200 only means the bridge answered; the gateway's business outcome
		// can still be a failure ("会話が不十分" etc.), so read the text.
		succeeded = !payloadFailed(result)
	}

	post := p.mutatePost(postID, func(post *model.Post) {
		post.Props["last_result"] = result
		post.Props["busy"] = false
		if action == actApplyModel && args != "" && succeeded {
			post.Props["selected_model"] = ""
		}
		p.renderPanel(post)
	})
	if post == nil {
		// The panel vanished; the trail must still land in the thread (review m1).
		p.postResult(channelID, rootPostID, resultIcon(succeeded)+result)
		return
	}
	p.postResult(channelID, rootPostID, resultIcon(succeeded)+result)
}

// resultIcon prefixes exactly one icon: the gateway's own text often already
// starts with ⚠️/ℹ️, and doubling it looks broken.
func resultIcon(ok bool) string {
	if ok {
		return "✅ "
	}
	return "⚠️ "
}

// runStandaloneAction posts a single "running" line for an operation that has no
// control surface (compress/reset) and rewrites it in place with the outcome.
// One post in, one post out — no panel, no second result post.
func (p *Plugin) runStandaloneAction(channelID, rootPostID, userID, action string) {
	op, ok := bridgeAction[action]
	if !ok {
		return
	}
	label := map[string]string{
		actCompress: tr("🗜️ 会話を圧縮しています…"),
		actReset:    tr("♻️ 会話をリセットしています…"),
	}[action]

	key := opKey(channelID, rootPostID, userID)
	if !p.reserveOp(key) {
		p.postResult(channelID, rootPostID, tr("⚠️ 実行中です。少し待ってから再度実行してください。"))
		return
	}

	post := &model.Post{
		ChannelId: channelID,
		UserId:    p.botUserID(),
		RootId:    rootPostID,
		Message:   label,
		Props:     model.StringInterface{"hermes_console_action": true},
	}
	created, appErr := p.API.CreatePost(post)
	if appErr != nil {
		p.releaseOp(key)
		p.API.LogError("hermes-console: action post failed", "err", appErr.Error())
		return
	}

	go func() {
		defer p.releaseOp(key)
		payload, status, err := p.callBridgeAction(channelID, rootPostID, userID, op, "", "")
		p.API.LogInfo("hermes-console: action", "action", action, "op", op, "status", status, "err", err != nil)

		message := ""
		switch {
		case err != nil && errors.Is(err, errOutcomeUnknown):
			message = tr("⚠️ 結果不明（応答を解釈できませんでした）。自動再送はしません。")
		case err != nil && errors.Is(err, errChatTypeUnknown):
			message = tr("⚠️ 失敗: チャンネル種別を取得できませんでした（操作は送信していません）")
		case err != nil:
			message = fmt.Sprintf(tr("⚠️ 失敗: %s"), err.Error())
		case status == http.StatusConflict:
			message = tr("⚠️ 実行中です。少し待ってから再度実行してください。")
		case status == http.StatusGatewayTimeout:
			message = tr("⚠️ 結果不明（タイムアウト）。自動再送はしません。")
		case status >= 400:
			message = fmt.Sprintf(tr("⚠️ 失敗 (%d): %s"), status, payload.Message)
		default:
			text := payloadText(action, payload.Text)
			if payloadFailed(text) {
				message = "⚠️ " + text
			} else {
				message = "✅ " + text
			}
		}

		update := func() *model.AppError {
			current, getErr := p.API.GetPost(created.Id)
			if getErr != nil {
				return getErr
			}
			current.Message = message
			_, updErr := p.API.UpdatePost(current)
			return updErr
		}
		if err := update(); err != nil {
			// Fall back to a separate post so the outcome is never lost.
			p.API.LogError("hermes-console: action post update failed", "err", err.Error())
			p.postResult(channelID, rootPostID, message)
		}
	}()
}

// setBusy toggles the panel's locked state and re-renders.
func (p *Plugin) setBusy(postID string, busy bool) {
	p.mutatePost(postID, func(post *model.Post) {
		post.Props["busy"] = busy
		p.renderPanel(post)
	})
}

func payloadText(action, text string) string {
	label := map[string]string{
		actReset:      tr("会話をリセットしました"),
		actCompress:   tr("会話を圧縮しました"),
		actApplyModel: tr("モデルを適用しました"),
		actRefresh:    tr("状態を更新しました"),
	}[action]
	if text == "" {
		return label
	}
	// The gateway's reply is the authoritative outcome; prefixing it with a
	// success label made "✅ 会話を圧縮しました" sit above "会話が不十分です"
	// (review m2).
	return text
}

// payloadFailed reads the gateway's business outcome from its own text. The
// bridge has no structured success flag, so this is the only honest signal.
func payloadFailed(text string) bool {
	for _, marker := range []string{
		"⚠️", "❌", "✗", "失敗", "不十分", "できません", "できませんでした",
		"failed", "Failed", "not enough", "Not enough", "no provider",
		"プロバイダーが構成されていません", "何も", "Nothing to",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func presence(b bool) string {
	if b {
		return tr("あり")
	}
	return tr("未作成")
}

func activityJP(a string) string {
	m := map[string]string{
		"idle":    tr("待機中"),
		"running": tr("応答中"),
		"unknown": tr("未確認"),
	}
	if v, ok := m[a]; ok {
		return v
	}
	return a
}

// ---- HTTP entry points ----------------------------------------------------

// handlePanel posts a panel for the browser (App Bar).
func (p *Plugin) handlePanel(w http.ResponseWriter, r *http.Request) {
	userID, ok := p.resolveActor(w, r)
	if !ok {
		return
	}
	var body struct {
		ChannelID  string `json:"channel_id"`
		RootPostID string `json:"root_post_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if !p.authorizeChannel(w, userID, body.ChannelID) {
		return
	}
	if !p.authorizeThread(w, body.ChannelID, body.RootPostID) {
		return
	}
	postID, appErr := p.postPanel(body.ChannelID, body.RootPostID, userID, viewAll)
	if appErr != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", appErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"post_id": postID})
}

// handleAction is the button/select integration endpoint.
func (p *Plugin) handleAction(w http.ResponseWriter, r *http.Request) {
	var req model.PostActionIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid integration request")
		return
	}

	post, appErr := p.API.GetPost(req.PostId)
	if appErr != nil {
		writeError(w, http.StatusNotFound, "not_found", "panel post not found")
		return
	}
	channelID, _ := post.Props["target_channel"].(string)
	rootPostID, _ := post.Props["target_root"].(string)
	targetUser, _ := post.Props["target_user"].(string)
	if targetUser == "" {
		targetUser = req.UserId
	}

	// The actor is the clicker, but the panel keeps its original target.
	if _, appErr := p.API.GetUser(req.UserId); appErr != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "unknown user")
		return
	}

	actionID, _ := req.Context["action_id"].(string)
	if actionID == "" {
		// Mattermost passes the action id in the integration context/url path too.
		actionID = strings.TrimPrefix(r.URL.Path, "/plugins/com.nicox.hermes-console/api/v1/actions/")
		actionID = strings.TrimSuffix(actionID, "/")
	}

	// Selection updates the post without touching the session. The updated post
	// rides back in the response's `update` field: a bare UpdatePost races the
	// client's own post state, so the first pick after a re-render showed the
	// previous provider's model list until the user picked again.
	// Both selects update the SAME post (no re-post: a new post would notify the
	// user's other devices). The model select survives the swap because its action
	// id embeds the provider, and Mattermost keys the menu by action.id.
	if actionID == actSelectProvider {
		value, _ := req.Context["selected_option"].(string)
		if value == "none" {
			value = ""
		}
		p.API.LogInfo("hermes-console: select", "action", actionID, "value", value, "post", req.PostId)
		updated := p.mutatePost(req.PostId, func(post *model.Post) {
			post.Props["selected_provider"] = value
			post.Props["selected_model"] = ""
			p.renderPanel(post)
		})
		writeJSON(w, http.StatusOK, model.PostActionIntegrationResponse{Update: updated})
		return
	}
	if actionID == actSelectModel || strings.HasPrefix(actionID, actSelectModelPrefix) {
		value, _ := req.Context["selected_option"].(string)
		if value == "none" {
			value = ""
		}
		p.API.LogInfo("hermes-console: select", "action", actionID, "value", value, "post", req.PostId)
		updated := p.mutatePost(req.PostId, func(post *model.Post) {
			post.Props["selected_model"] = value
			p.renderPanel(post)
		})
		writeJSON(w, http.StatusOK, model.PostActionIntegrationResponse{Update: updated})
		return
	}

	args := ""
	if actionID == actApplyModel {
		selected, _ := post.Props["selected_model"].(string)
		if selected == "" {
			post.Props["last_result"] = tr("モデルが未選択です。モデルを選んでから適用してください。")
			p.rerender(post)
			writeJSON(w, http.StatusOK, model.PostActionIntegrationResponse{})
			return
		}
		args = applyModelArgs(selected)
	}

	// A unique key per ACCEPTED operation: the server-side reservation already
	// rejects duplicates, so tying the key to post.UpdateAt (which every render
	// changes) made two clicks look like different operations (review M4).
	idem := fmt.Sprintf("%s:%s:%s:%d", req.PostId, actionID, args, time.Now().UnixNano())

	if actionID == actRefresh {
		p.mutatePost(req.PostId, func(post *model.Post) {
			post.Props["last_result"] = tr("状態を更新しました")
			p.renderPanel(post)
		})
		writeJSON(w, http.StatusOK, model.PostActionIntegrationResponse{})
		return
	}

	p.runPanelAction(req.PostId, channelID, rootPostID, targetUser, actionID, args, idem)
	writeJSON(w, http.StatusOK, model.PostActionIntegrationResponse{})
}
