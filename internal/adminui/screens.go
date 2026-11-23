package adminui

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

// ButtonStyleDanger marks destructive buttons for the presentation layer.
const ButtonStyleDanger = "danger"

// Button is a UI-logic button: a plain label plus already-encoded callback
// data. Styles are hints; the adapter maps them to Telegram button styles.
type Button struct {
	Label string
	Data  string
	Style string
}

// ButtonRow is one row of buttons.
type ButtonRow []Button

// DocumentPayload carries a screen payload that is better delivered as a
// document (large prompt bodies).
type DocumentPayload struct {
	Filename string
	Content  []byte
}

// PhotoPayload carries a payload delivered as an inline photo (statistics
// PNG reports). Telegram re-encodes bot-sent photos, so the filename is
// only the upload's multipart name, not a preserved attachment name.
type PhotoPayload struct {
	Filename string
	Caption  string
	Content  []byte
}

// Screen is a renderable snapshot: plain text (no parse mode — titles,
// usernames, and values are third-party controlled) plus a button grid.
type Screen struct {
	Text       string
	Rows       []ButtonRow
	AsDocument *DocumentPayload
}

// PromptViewDocumentThreshold is the body size above which "View" sends a
// document instead of a plain message.
const PromptViewDocumentThreshold = 3500

// screenBuilder carries the sequence number every button must encode. It is
// constructed with the seq the screen's buttons will carry, i.e. the session
// Seq AFTER the commit that renders this screen.
type screenBuilder struct {
	seq uint32
}

func (b screenBuilder) btn(label, verb string, args ...string) Button {
	return Button{Label: label, Data: EncodeAction(verb, b.seq, args...)}
}

func (b screenBuilder) danger(label, verb string, args ...string) Button {
	button := b.btn(label, verb, args...)
	button.Style = ButtonStyleDanger

	return button
}

// footerRow is the standard bottom row: back in the navigation stack, then
// the dashboard shortcut.
func (b screenBuilder) footerRow() ButtonRow {
	return ButtonRow{b.btn("‹ Back", VerbBack), b.btn("⌂ Home", VerbHome)}
}

// DashboardData feeds the dashboard screen.
type DashboardData struct {
	Summary     adminconfig.AccessSummary
	RecentChats []adminconfig.ChatCatalogEntry
	Uptime      time.Duration
	// Stickers shows the sticker catalog entry (a catalog is wired).
	Stickers bool
}

func (b screenBuilder) dashboard(data DashboardData) *Screen {
	var text strings.Builder
	text.WriteString("🛠 Admin\n\n")
	fmt.Fprintf(&text, "Access policy: %s — %d allowlisted chat(s)\n", data.Summary.Policy, data.Summary.AllowlistCount)
	fmt.Fprintf(&text, "Uptime: %s\n", formatDuration(data.Uptime))

	rows := make([]ButtonRow, 0, 5)
	if len(data.RecentChats) > 0 {
		row := make(ButtonRow, 0, len(data.RecentChats))
		for _, chat := range data.RecentChats {
			row = append(row, b.btn(truncateRunes(ChatEntryName(chat), 20), VerbChat, strconv.FormatInt(chat.ChatID, 10)))
		}
		rows = append(rows, row)
	}

	rows = append(rows,
		ButtonRow{b.btn("Chats", VerbCatalog, "1"), b.btn("Global settings", VerbGlobalSet)},
		ButtonRow{b.btn("Prompts", VerbPrompts), b.btn("Access", VerbAccess)},
		ButtonRow{b.btn("Statistics", VerbStats), b.btn("Refresh", VerbHome)},
	)
	if data.Stickers {
		rows = append(rows, ButtonRow{b.btn("🎟 Stickers", VerbStickers)})
	}

	return &Screen{Text: text.String(), Rows: rows}
}

// CatalogData feeds the chat catalog screen.
type CatalogData struct {
	Page   adminconfig.ChatPage
	Filter adminconfig.ChatFilter
	Search string
}

func (b screenBuilder) catalog(data CatalogData) *Screen {
	var text strings.Builder
	title := "💬 Chats"
	if data.Search != "" {
		title += fmt.Sprintf(" — search %q", truncateRunes(data.Search, 48))
	}
	text.WriteString(title + "\n")
	fmt.Fprintf(&text, "Filter: %s — %d chat(s) — page %d/%d\n\n", catalogFilterLabel(data.Filter), data.Page.Total, data.Page.Page, data.Page.Pages)

	if len(data.Page.Entries) == 0 {
		text.WriteString("No chats found.\n")
	}
	for i, chat := range data.Page.Entries {
		number := (data.Page.Page-1)*adminconfig.DefaultChatPageSize + i + 1
		fmt.Fprintf(&text, "%d. %s — %s — %d\n", number,
			truncateRunes(ChatEntryName(chat), 32), chatTypeShort(chat.ChatType), chat.ChatID)
	}

	rows := make([]ButtonRow, 0, 7)

	// Two chat buttons per row.
	for i := 0; i < len(data.Page.Entries); i += 2 {
		row := ButtonRow{b.chatEntryButton(data.Page.Entries[i])}
		if i+1 < len(data.Page.Entries) {
			row = append(row, b.chatEntryButton(data.Page.Entries[i+1]))
		}
		rows = append(rows, row)
	}

	nav := ButtonRow{}
	if data.Page.Page > 1 {
		nav = append(nav, b.btn("< Prev", VerbCatalog, strconv.Itoa(data.Page.Page-1)))
	}
	nav = append(nav, b.btn(filterToggleLabel(data.Filter, adminconfig.ChatFilterAll), VerbCatalog, string(adminconfig.ChatFilterAll)))
	nav = append(nav, b.btn(filterToggleLabel(data.Filter, adminconfig.ChatFilterGroups), VerbCatalog, string(adminconfig.ChatFilterGroups)))
	nav = append(nav, b.btn(filterToggleLabel(data.Filter, adminconfig.ChatFilterPrivate), VerbCatalog, string(adminconfig.ChatFilterPrivate)))
	if data.Page.Page < data.Page.Pages {
		nav = append(nav, b.btn("Next >", VerbCatalog, strconv.Itoa(data.Page.Page+1)))
	}
	rows = append(rows, nav)

	rows = append(rows,
		ButtonRow{b.btn("🔍 Search", VerbCatalog, "s"), b.btn("➕ Add chat", VerbCatalog, "a")},
		b.footerRow(),
	)

	return &Screen{Text: text.String(), Rows: rows}
}

func (b screenBuilder) chatEntryButton(chat adminconfig.ChatCatalogEntry) Button {
	return b.btn(truncateRunes(ChatEntryName(chat), 18), VerbChat, strconv.FormatInt(chat.ChatID, 10))
}

// ChatCardData feeds the chat card screen. AccessLine is preformatted by the
// controller (four policy × membership states).
type ChatCardData struct {
	Info           adminconfig.ChatCatalogEntry
	Found          bool
	AccessLine     string
	View           adminconfig.EffectiveChatConfig
	Interactivity  string
	ModeOverridden bool
	ModeLabels     []string
	ModeValues     []string
	// HidePreviewFields hides the preview-governed fields from the settings
	// list while the global preview master switch is off. Hiding is
	// render-time only: stored chat overrides stay untouched.
	HidePreviewFields bool
	// HideWebSearch hides the web search row while no search backend is
	// configured. Render-time only: stored overrides stay untouched.
	HideWebSearch bool
	// HideImageGeneration hides the image generation row while no generation
	// backend is configured. Render-time only: stored overrides stay untouched.
	HideImageGeneration bool
}

// previewFieldNames lists the chat fields governed by the preview master
// switch; they disappear from chat screens while previews are globally off.
var previewFieldNames = map[string]bool{
	"preview_mode":      true,
	"keep_tool_history": true,
}

// toolsSubmenuFields names the fields grouped into the Tools submenu instead
// of the settings screen button grid. They stay listed in the settings text so
// the effective values remain visible at a glance.
var toolsSubmenuFields = map[string]bool{
	"keep_tool_history":        true,
	"web_search_enabled":       true,
	"image_generation_enabled": true,
}

// filterPreviewFields drops the preview-governed specs from a chat spec list.
func filterPreviewFields(specs []adminconfig.FieldSpec) []adminconfig.FieldSpec {
	filtered := make([]adminconfig.FieldSpec, 0, len(specs))
	for _, spec := range specs {
		if previewFieldNames[spec.Name] {
			continue
		}
		filtered = append(filtered, spec)
	}

	return filtered
}

func (b screenBuilder) chatCard(data ChatCardData) *Screen {
	name := ChatEntryName(data.Info)
	if !data.Found {
		name = fmt.Sprintf("chat %d", data.Info.ChatID)
	}

	var text strings.Builder
	fmt.Fprintf(&text, "💬 %s — %s — ID: %d\n\n", truncateRunes(name, 48), chatTypeShort(data.Info.ChatType), data.Info.ChatID)
	fmt.Fprintf(&text, "Access: %s\n", data.AccessLine)
	fmt.Fprintf(&text, "Interactivity: %s", adminconfig.InteractivityModeLabel(adminconfig.InteractivityMode(data.Interactivity)))
	if data.ModeOverridden {
		text.WriteString(" (chat override)")
	}
	text.WriteString("\n\nSettings:\n")
	for _, field := range data.View.Fields {
		if field.Spec.Name == "interactivity_mode" {
			continue // shown in the dedicated line above
		}
		if data.HidePreviewFields && previewFieldNames[field.Spec.Name] {
			continue // hidden while the preview master switch is off
		}
		if data.HideWebSearch && field.Spec.Name == "web_search_enabled" {
			continue // no search backend is configured
		}
		if data.HideImageGeneration && field.Spec.Name == "image_generation_enabled" {
			continue // no image generation backend is configured
		}
		fmt.Fprintf(&text, "• %s: %s%s\n", field.Spec.Label, displayFieldValue(field), sourceMark(field.Overridden))
	}

	rows := make([]ButtonRow, 0, 5)

	modeRow := make(ButtonRow, 0, len(data.ModeValues))
	for i, mode := range data.ModeValues {
		label := "○ " + data.ModeLabels[i]
		if mode == data.Interactivity {
			label = "● " + data.ModeLabels[i]
		}
		modeRow = append(modeRow, b.btn(label, VerbValue, "im", mode))
	}
	rows = append(rows, modeRow)

	if data.ModeOverridden {
		rows = append(rows, ButtonRow{b.btn("Inherit global", VerbClear, "im")})
	}

	allowlistVerb := VerbAllowAdd
	allowlistLabel := "✅ Allowlist: add"
	if data.View.Allowlisted {
		allowlistVerb = VerbAllowRem
		allowlistLabel = "🚫 Allowlist: remove"
	}
	rows = append(rows,
		ButtonRow{
			b.btn(allowlistLabel, allowlistVerb, strconv.FormatInt(data.Info.ChatID, 10)),
			b.btn("Edit settings", VerbChatSet),
		},
		ButtonRow{
			b.btn("🧠 Interactivity", VerbInteractivity),
			b.btn("🛠 Tools", VerbTools),
		},
	)

	rows = append(rows,
		ButtonRow{b.btn("Prompts", VerbPrompts), b.danger("Reset overrides", VerbResetChat)},
		b.footerRow(),
	)

	return &Screen{Text: text.String(), Rows: rows}
}

// SettingsData feeds the global/chat settings screens.
type SettingsData struct {
	Global   bool
	View     adminconfig.EffectiveGlobalConfig
	ChatView adminconfig.EffectiveChatConfig
	ChatName string
	// HidePreviewFields hides the preview-governed rows from the chat
	// settings screen while the global preview master switch is off. The
	// global screen always keeps every field (the master switch itself must
	// stay operable), and stored chat overrides persist untouched.
	HidePreviewFields bool
	// HideImageModel hides the image_recognition_model setting while the
	// active image recognizer takes no model selection (MiniMax). Hiding is
	// render-time only: a stored global value stays untouched.
	HideImageModel bool
	// HideWebSearch hides the web search setting while no search backend is
	// configured in the environment. Render-time only.
	HideWebSearch bool
	// HideImageGeneration hides the image generation settings (permission and
	// model) while no generation backend is configured in the environment.
	// Render-time only: stored values stay untouched.
	HideImageGeneration bool
}

// interactivitySubmenuFields names the fields grouped into the interactivity
// submenu instead of the settings screen button grid.
var interactivitySubmenuFields = map[string]bool{
	"interaction_profile":         true,
	"decision_instructions":       true,
	"default_interaction_profile": true,
}

func (b screenBuilder) settings(data SettingsData) *Screen {
	var text strings.Builder
	if data.Global {
		text.WriteString("🌍 Global settings\n\n")
	} else {
		fmt.Fprintf(&text, "⚙️ Chat settings — %s\n\n", truncateRunes(data.ChatName, 48))
	}

	specs := adminconfig.GlobalFieldSpecs()
	if !data.Global {
		specs = adminconfig.ChatFieldSpecs()
		if data.HidePreviewFields {
			specs = filterPreviewFields(specs)
		}
	}
	visible := make([]adminconfig.FieldSpec, 0, len(specs))
	for _, spec := range specs {
		if data.HideImageModel && spec.Name == "image_recognition_model" {
			continue
		}
		if data.HideWebSearch && spec.Name == "web_search_enabled" {
			continue
		}
		if data.HideImageGeneration && (spec.Name == "image_generation_enabled" || spec.Name == "image_generation_model") {
			continue
		}
		visible = append(visible, spec)
	}

	for _, spec := range visible {
		value, overridden := data.valueOf(spec)
		line := fmt.Sprintf("• %s: %s", spec.Label, truncateRunes(value, 60))
		if overridden {
			line += " [chat override]"
		}
		text.WriteString(line + "\n")
	}

	grid := make([]adminconfig.FieldSpec, 0, len(visible))
	for _, spec := range visible {
		if interactivitySubmenuFields[spec.Name] {
			continue // edited in the interactivity submenu
		}
		if toolsSubmenuFields[spec.Name] {
			continue // edited in the tools submenu
		}
		grid = append(grid, spec)
	}

	rows := make([]ButtonRow, 0, 5)
	for i := 0; i < len(grid); i += 2 {
		row := ButtonRow{b.settingsFieldButton(grid[i])}
		if i+1 < len(grid) {
			row = append(row, b.settingsFieldButton(grid[i+1]))
		}
		rows = append(rows, row)
	}

	rows = append(rows, ButtonRow{
		b.btn("🧠 Interactivity", VerbInteractivity),
		b.btn("🛠 Tools", VerbTools),
	})

	if !data.Global {
		rows = append(rows, ButtonRow{b.danger("Reset overrides", VerbResetChat)})
	}

	rows = append(rows, b.footerRow())

	return &Screen{Text: text.String(), Rows: rows}
}

// valueOf resolves the display value and override flag of one field for the
// settings screens. Global values are raw (unlabeled) storage values; chat
// values are the effective ones with their override flag.
func (data SettingsData) valueOf(spec adminconfig.FieldSpec) (string, bool) {
	if data.Global {
		return globalSettingsValue(data.View.Settings, spec), false
	}

	for _, field := range data.ChatView.Fields {
		if field.Spec.Name == spec.Name {
			return field.Effective, field.Overridden
		}
	}

	return "", false
}

func globalSettingsValue(settings adminconfig.GlobalSettings, spec adminconfig.FieldSpec) string {
	switch spec.Name {
	case "character_name":
		return settings.CharacterName
	case "language":
		return settings.Language
	case "gender":
		return settings.Gender
	case "default_interactivity_mode":
		return string(settings.DefaultInteractivityMode)
	case "default_auto_summary_enabled":
		return strconv.FormatBool(settings.DefaultAutoSummaryEnabled)
	case "show_reply_previews":
		return strconv.FormatBool(settings.ShowReplyPreviews)
	case "default_preview_mode":
		return string(settings.DefaultPreviewMode)
	case "keep_tool_history":
		return strconv.FormatBool(settings.KeepToolHistory)
	case "web_search_enabled":
		return strconv.FormatBool(settings.WebSearchEnabled)
	case "image_generation_enabled":
		return strconv.FormatBool(settings.ImageGenerationEnabled)
	case "image_generation_model":
		// No environment route exists for this model (#139): unset means the
		// generate_image tool stays unavailable.
		if settings.ImageGenerationModel == "" {
			return unsetPlaceholder
		}

		return settings.ImageGenerationModel
	case "chat_model", "summarize_model", "image_recognition_model":
		if settings.ChatModel == "" && spec.Name == "chat_model" {
			return envRoutePlaceholder
		}
		if value := globalModelValue(settings, spec.Name); value != "" {
			return value
		}

		return envRoutePlaceholder
	case "systemone_model":
		if settings.SystemOneModel == "" {
			return unsetPlaceholder
		}

		return settings.SystemOneModel
	case "default_interaction_profile":
		if settings.DefaultInteractionProfile == "" {
			return string(adminconfig.ProfileNonInteractive)
		}

		return string(settings.DefaultInteractionProfile)
	case "decision_instructions":
		if settings.DecisionInstructions == "" {
			return unsetPlaceholder
		}

		return settings.DecisionInstructions
	default:
		return ""
	}
}

const (
	// envRoutePlaceholder and unsetPlaceholder render empty global values in
	// the settings screens: a blank line would read as a broken screen.
	envRoutePlaceholder = "(environment route)"
	unsetPlaceholder    = "(unset)"
)

func globalModelValue(settings adminconfig.GlobalSettings, name string) string {
	switch name {
	case "chat_model":
		return settings.ChatModel
	case "summarize_model":
		return settings.SummarizeModel
	case "image_recognition_model":
		return settings.ImageRecognitionModel
	default:
		return ""
	}
}

func (b screenBuilder) settingsFieldButton(spec adminconfig.FieldSpec) Button {
	code, ok := FieldCode(spec.Name)
	if !ok {
		code = spec.Name
	}

	return b.btn("✎ "+truncateRunes(spec.Label, 16), VerbField, code)
}

// FieldEditorData feeds a single field editor screen.
type FieldEditorData struct {
	Global     bool
	ScopeChat  int64
	Spec       adminconfig.FieldSpec
	Value      string
	Overridden bool
	// HasDiscovery reports that the backend serving this field's feature can
	// list its models, enabling the picker. Discovery establishes
	// availability only, never capability: a listed model is not guaranteed
	// to serve the feature well, so manual entry stays available and the
	// picker text says so.
	HasDiscovery bool
	// Models is the cached backend list (empty until fetched); ModelPage is
	// the shown 1-based page of that list.
	Models    []string
	ModelPage int
}

// modelPickerPages is the page count of a cached model list.
func modelPickerPages(models []string) int {
	if len(models) == 0 {
		return 0
	}

	return (len(models) + ModelPickerPageSize - 1) / ModelPickerPageSize
}

func (b screenBuilder) fieldEditor(data FieldEditorData) *Screen {
	var text strings.Builder
	scope := "Global"
	if !data.Global {
		scope = "Chat"
	}
	fmt.Fprintf(&text, "✎ %s — %s\n\n", data.Spec.Label, scope)
	fmt.Fprintf(&text, "Current: %s\n", truncateRunes(data.Value, 80))
	if data.Overridden {
		text.WriteString("(chat override)\n")
	}

	var rows []ButtonRow
	code, _ := FieldCode(data.Spec.Name)
	switch data.Spec.Kind {
	case adminconfig.KindEnum:
		for _, option := range data.Spec.Enum {
			label := "○ " + option.Label
			if option.Value == data.Value {
				label = "● " + option.Label
			}
			rows = append(rows, ButtonRow{b.btn(label, VerbValue, code, option.Value)})
		}
		// Chat-scope enum overrides inherit by clearing the override row —
		// restoring the global default without touching unrelated settings.
		if !data.Global && data.Overridden {
			rows = append(rows, ButtonRow{b.btn("Inherit global", VerbClear, code)})
		}
	case adminconfig.KindBool:
		rows = append(rows, ButtonRow{
			b.btn(boolMark(data.Value == "true")+" On", VerbValue, code, "on"),
			b.btn(boolMark(data.Value == "false")+" Off", VerbValue, code, "off"),
		})
	case adminconfig.KindBoolTriState:
		rows = append(rows, ButtonRow{
			b.btn("Inherit", VerbValue, code, "inh"),
			b.btn(boolMark(data.Value == "true")+" On", VerbValue, code, "on"),
			b.btn(boolMark(data.Value == "false")+" Off", VerbValue, code, "off"),
		})
	case adminconfig.KindString:
		if data.HasDiscovery {
			if len(data.Models) == 0 {
				rows = append(rows, ButtonRow{b.btn("📋 Backend models", VerbModelList, code)})
			} else {
				text.WriteString("\nBackend-served models (availability, not capability — manual entry always works):\n")
				rows = append(rows, b.modelPickerRows(code, data.Models, data.ModelPage)...)
			}
		}
		rows = append(rows, ButtonRow{b.btn("✏️ Enter model ID…", VerbString, code)})
		// Chat-scope string overrides inherit by clearing the override row —
		// empty input is a rejected value, so the button is the only way
		// back to the global value.
		if !data.Global && data.Overridden {
			rows = append(rows, ButtonRow{b.btn("Inherit global", VerbClear, code)})
		}
		if data.Global && data.Spec.Clearable && isClearableModelField(data.Spec.Name) && hasConcreteValue(data.Value) {
			rows = append(rows, ButtonRow{b.danger(clearGlobalModelLabel(data.Spec.Name), VerbClearGlobal, code)})
		}
	}

	rows = append(rows, b.footerRow())

	return &Screen{Text: text.String(), Rows: rows}
}

// modelPickerRows renders one page of the cached backend list as two model
// buttons per row plus pagination; selection carries the list INDEX because
// model IDs overflow the 64-byte callback data limit.
func (b screenBuilder) modelPickerRows(code string, models []string, page int) []ButtonRow {
	pages := modelPickerPages(models)
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}

	start := (page - 1) * ModelPickerPageSize
	end := start + ModelPickerPageSize
	if end > len(models) {
		end = len(models)
	}

	rows := make([]ButtonRow, 0, 5)
	for i := start; i < end; i += 2 {
		row := ButtonRow{b.btn(truncateRunes(models[i], 24), VerbModelPick, code, strconv.Itoa(i))}
		if i+1 < end {
			row = append(row, b.btn(truncateRunes(models[i+1], 24), VerbModelPick, code, strconv.Itoa(i+1)))
		}
		rows = append(rows, row)
	}

	if pages > 1 {
		nav := ButtonRow{}
		if page > 1 {
			nav = append(nav, b.btn("< Prev", VerbModelPage, strconv.Itoa(page-1)))
		}
		nav = append(nav, b.btn(fmt.Sprintf("%d/%d", page, pages), VerbModelPage, strconv.Itoa(page)))
		if page < pages {
			nav = append(nav, b.btn("Next >", VerbModelPage, strconv.Itoa(page+1)))
		}
		rows = append(rows, nav)
	}

	return rows
}

func isClearableModelField(name string) bool {
	switch name {
	case "chat_model", "summarize_model", "image_recognition_model", "image_generation_model", "systemone_model":
		return true
	default:
		return false
	}
}

// profileButton renders one profile option with the selection marker.
func (b screenBuilder) profileButton(code string, option adminconfig.EnumOption, current string) Button {
	label := "○ " + option.Label
	if option.Value == current {
		label = "● " + option.Label
	}

	return b.btn(label, VerbValue, code, option.Value)
}

// hasConcreteValue reports whether the shown editor value is a stored
// override rather than a placeholder (environment route / unset).
func hasConcreteValue(value string) bool {
	return value != "" && value != envRoutePlaceholder && value != unsetPlaceholder
}

// clearGlobalModelLabel distinguishes the two clear semantics: generation
// models fall back to their environment route, the System One model has no
// environment fallback and per-chat overrides survive the clear.
func clearGlobalModelLabel(name string) string {
	switch name {
	case "systemone_model":
		return "Clear global default model"
	case "image_generation_model":
		return "Clear model"
	}

	return "Reset to ENV default"
}

// InteractivityData feeds the interactivity submenu screen, which groups the
// interaction profile, the effective System One decision model, and the
// decision instructions for one scope (global or chat).
type InteractivityData struct {
	Global   bool
	ChatName string
	// Profile is the effective profile in this scope: the raw global value
	// (empty renders as the effective non_interactive default) or the chat's
	// effective value, with Overridden marking a chat override.
	Profile           string
	ProfileOverridden bool
	// Model is the effective System One decision model in this scope; empty
	// means the decision backend is unconfigured for the scope. HasDiscovery
	// reports whether the decision backend answers model discovery at all.
	Model           string
	ModelOverridden bool
	HasDiscovery    bool
	// Instructions is the effective decision-engine guidance in this scope.
	Instructions           string
	InstructionsOverridden bool
}

func (b screenBuilder) interactivity(data InteractivityData) *Screen {
	var text strings.Builder
	if data.Global {
		text.WriteString("🧠 Interactivity — global\n\n")
	} else {
		fmt.Fprintf(&text, "🧠 Interactivity — %s\n\n", truncateRunes(data.ChatName, 48))
	}

	profile := data.Profile
	if profile == "" {
		// An empty effective profile means the non_interactive default in
		// every scope (matching the engine's consumption semantics).
		profile = string(adminconfig.ProfileNonInteractive)
	}
	fmt.Fprintf(&text, "Interaction profile: %s%s\n", adminconfig.InteractionProfileLabel(adminconfig.InteractionProfile(profile)), sourceMark(data.ProfileOverridden))
	if data.Model == "" {
		text.WriteString("Decision model: (unconfigured — decision features stay off in this scope)\n")
	} else {
		fmt.Fprintf(&text, "Decision model: %s%s\n", data.Model, sourceMark(data.ModelOverridden))
	}
	if data.Instructions == "" {
		text.WriteString("Decision instructions: (unset)\n")
	} else {
		fmt.Fprintf(&text, "Decision instructions: %s\n", truncateRunes(data.Instructions, 120))
	}

	rows := make([]ButtonRow, 0, 8)

	// Profile buttons, two per row. Enabling any profile as the global
	// default routes through a scope confirmation (controller); returning to
	// non_interactive applies directly.
	profileCode, _ := FieldCode("interaction_profile")
	profileOptions := adminconfig.InteractionProfileEnumOptions()
	for i := 0; i < len(profileOptions); i += 2 {
		row := ButtonRow{b.profileButton(profileCode, profileOptions[i], profile)}
		if i+1 < len(profileOptions) {
			row = append(row, b.profileButton(profileCode, profileOptions[i+1], profile))
		}
		rows = append(rows, row)
	}
	if !data.Global && data.ProfileOverridden {
		rows = append(rows, ButtonRow{b.btn("Inherit global", VerbClear, profileCode)})
	}

	modelCode, _ := FieldCode("systemone_model")
	modelRow := ButtonRow{b.btn("Select model…", VerbField, modelCode)}
	if data.Global && hasConcreteValue(data.Model) {
		modelRow = append(modelRow, b.danger("Clear global default model", VerbClearGlobal, modelCode))
	}
	rows = append(rows, modelRow)

	instructionsCode, _ := FieldCode("decision_instructions")
	rows = append(rows, ButtonRow{b.btn("✏️ Edit instructions…", VerbString, instructionsCode)})
	// Instructions edit through a dedicated input, never the field editor,
	// so the chat-scope inherit row lives here: blank input is a rejected
	// value, and this button is the only way back to the global text.
	if !data.Global && data.InstructionsOverridden {
		rows = append(rows, ButtonRow{b.btn("Inherit global", VerbClear, instructionsCode)})
	}
	if data.Global && data.Instructions != "" {
		rows = append(rows, ButtonRow{b.danger("Clear instructions", VerbClearGlobal, instructionsCode)})
	}

	rows = append(rows, b.footerRow())

	return &Screen{Text: text.String(), Rows: rows}
}

// ToolsField is one row of the Tools submenu: a boolean setting with its
// effective value in the scope. In chat scope Overridden marks a chat
// override and GlobalValue is the value an inheriting chat would use.
type ToolsField struct {
	Name        string
	Value       string // "true" | "false"
	Overridden  bool
	GlobalValue string
}

// ToolsData feeds the Tools submenu, which groups the settings that decide
// which tools the model may use and how tool activity is presented.
type ToolsData struct {
	Global   bool
	ChatName string
	Fields   []ToolsField
}

// toolsFieldNotes explains settings whose effect is not obvious from the label.
var toolsFieldNotes = map[string]string{
	"web_search_enabled":       "External search can consume paid provider resources. It applies only while a search backend is configured on the server.",
	"image_generation_enabled": "Image generation is billed per image. It applies only while a generation backend and a generation model are configured.",
	"keep_tool_history":        "Appends completed tool calls to replies while live previews are enabled.",
}

func onOffLabel(value string) string {
	if value == "true" {
		return "On"
	}

	return "Off"
}

func (b screenBuilder) tools(data ToolsData) *Screen {
	var text strings.Builder
	if data.Global {
		text.WriteString("🛠 Tools — global\n\n")
	} else {
		fmt.Fprintf(&text, "🛠 Tools — %s\n\n", truncateRunes(data.ChatName, 48))
	}

	rows := make([]ButtonRow, 0, 4)
	if len(data.Fields) == 0 {
		text.WriteString("No tool settings are available right now.\n")
	}

	specs := adminconfig.ChatFieldSpecs()
	if data.Global {
		specs = adminconfig.GlobalFieldSpecs()
	}
	for _, field := range data.Fields {
		var spec adminconfig.FieldSpec
		for _, candidate := range specs {
			if candidate.Name == field.Name {
				spec = candidate
			}
		}
		if spec.Name == "" {
			continue
		}

		line := fmt.Sprintf("%s: %s", spec.Label, onOffLabel(field.Value))
		switch {
		case !data.Global && field.Overridden:
			line += fmt.Sprintf(" [chat override] (global: %s)", onOffLabel(field.GlobalValue))
		case !data.Global:
			line += " (inherited)"
		}
		text.WriteString(line + "\n")
		if note := toolsFieldNotes[field.Name]; note != "" {
			text.WriteString("  " + note + "\n")
		}
		text.WriteString("\n")

		rows = append(rows, ButtonRow{b.settingsFieldButton(spec)})
	}

	rows = append(rows, b.footerRow())

	return &Screen{Text: strings.TrimRight(text.String(), "\n") + "\n", Rows: rows}
}

type PromptsMenuData struct {
	ChatScope int64 // 0 = global scope
	ChatName  string
	Overview  []adminconfig.PromptStatus
}

func (b screenBuilder) promptsMenu(data PromptsMenuData) *Screen {
	var text strings.Builder
	if data.ChatScope != 0 {
		fmt.Fprintf(&text, "📝 Prompts — chat %s\n\n", truncateRunes(data.ChatName, 48))
	} else {
		text.WriteString("📝 Prompts — global\n\n")
	}

	for _, status := range data.Overview {
		line := fmt.Sprintf("• %s — effective: %s", promptFeatureLabel(status.Feature), status.Effective)
		if data.ChatScope != 0 && status.HasChatStored {
			line += " [chat override]"
		}
		text.WriteString(line + "\n")
	}

	rows := []ButtonRow{}
	for _, feature := range []adminconfig.PromptFeature{
		adminconfig.PromptFeatureChat,
		adminconfig.PromptFeatureSummarize,
		adminconfig.PromptFeatureImageRecognition,
	} {
		code, _ := PromptFeatureCode(feature)
		rows = append(rows, ButtonRow{b.btn(promptFeatureLabel(feature), VerbPromptFeat, code)})
	}
	rows = append(rows, b.footerRow())

	return &Screen{Text: text.String(), Rows: rows}
}

// PromptDetailData feeds one prompt's detail screen.
type PromptDetailData struct {
	Feature      adminconfig.PromptFeature
	ChatScope    int64
	ChatName     string
	Status       adminconfig.PromptStatus
	ActiveBody   string
	ActiveSource adminconfig.PromptSource
	BuiltinBody  string
	HasBuiltin   bool
}

func (b screenBuilder) promptDetail(data PromptDetailData) *Screen {
	code, _ := PromptFeatureCode(data.Feature)

	var text strings.Builder
	scope := "global"
	if data.ChatScope != 0 {
		scope = "chat " + truncateRunes(data.ChatName, 40)
	}
	fmt.Fprintf(&text, "📝 Prompt: %s — %s\n\n", promptFeatureLabel(data.Feature), scope)
	fmt.Fprintf(&text, "Effective source: %s\n", data.ActiveSource)
	fmt.Fprintf(&text, "Active body: %d chars\n", len(data.ActiveBody))
	if data.ChatScope != 0 && data.Status.HasChatStored {
		text.WriteString("Chat override stored.\n")
	}
	if data.Status.HasGlobalStored {
		text.WriteString("Global template stored.\n")
	}

	rows := []ButtonRow{
		{b.btn("View", VerbPromptView, code)},
		{b.btn("⬇️ Download active", VerbPromptDL, code)},
	}
	if data.HasBuiltin {
		rows = append(rows, ButtonRow{b.btn("⬇️ Download builtin", VerbPromptBL, code)})
	}
	rows = append(rows,
		ButtonRow{b.btn("✏️ Replace via text", VerbPromptText, code)},
		ButtonRow{b.btn("📎 Upload file", VerbPromptUp, code)},
	)

	if data.ChatScope != 0 {
		if data.Status.HasChatStored {
			rows = append(rows, ButtonRow{b.danger("Reset to global", VerbPromptRstC, code)})
		}
	} else if data.Status.HasGlobalStored {
		rows = append(rows, ButtonRow{b.danger("Restore builtin", VerbPromptRstG, code)})
	}

	rows = append(rows, b.footerRow())

	return &Screen{Text: text.String(), Rows: rows}
}

// AccessData feeds the access policy screen. Entries is ONE page of the
// allowlist (paginated like the catalog so the screen always fits Telegram's
// message and keyboard limits); Total is the full entry count.
type AccessData struct {
	Summary adminconfig.AccessSummary
	Entries []adminconfig.AllowlistEntry
	Total   int
	Page    int
	Pages   int
}

func (b screenBuilder) access(data AccessData) *Screen {
	var text strings.Builder
	text.WriteString("🔐 Access\n\n")
	fmt.Fprintf(&text, "Policy: %s\n", data.Summary.Policy)
	if data.Summary.Policy == adminconfig.AccessPolicyAllowlist {
		text.WriteString("Only allowlisted chats are served (admin DMs are always exempt).\n")
	} else {
		text.WriteString("Every chat is served; allowlist membership is kept for reference.\n")
	}
	fmt.Fprintf(&text, "\nAllowlist (%d", data.Total)
	if data.Pages > 1 {
		fmt.Fprintf(&text, ", page %d of %d", data.Page, data.Pages)
	}
	text.WriteString("):\n")
	if len(data.Entries) == 0 {
		text.WriteString("(empty — no ordinary chats are allowed under the allowlist policy)\n")
	}
	first := (data.Page - 1) * adminconfig.DefaultChatPageSize
	for i, entry := range data.Entries {
		fmt.Fprintf(&text, "%d. %s — %d%s\n", first+i+1,
			truncateRunes(allowlistEntryName(entry), 32), entry.ChatID, allowlistAddedMark(entry))
	}

	rows := make([]ButtonRow, 0, 8)
	for i := 0; i < len(data.Entries); i += 2 {
		row := ButtonRow{b.danger("✕ "+strconv.FormatInt(data.Entries[i].ChatID, 10), VerbAllowRem, strconv.FormatInt(data.Entries[i].ChatID, 10))}
		if i+1 < len(data.Entries) {
			row = append(row, b.danger("✕ "+strconv.FormatInt(data.Entries[i+1].ChatID, 10), VerbAllowRem, strconv.FormatInt(data.Entries[i+1].ChatID, 10)))
		}
		rows = append(rows, row)
	}

	if data.Pages > 1 {
		nav := ButtonRow{}
		if data.Page > 1 {
			nav = append(nav, b.btn("< Prev", VerbAccess, strconv.Itoa(data.Page-1)))
		}
		if data.Page < data.Pages {
			nav = append(nav, b.btn("Next >", VerbAccess, strconv.Itoa(data.Page+1)))
		}
		rows = append(rows, nav)
	}

	rows = append(rows, ButtonRow{b.btn("➕ Add chat", VerbCatalog, "a")})

	if data.Summary.Policy == adminconfig.AccessPolicyAllowlist {
		rows = append(rows, ButtonRow{b.danger("Switch to allow_all", VerbAccessPol, string(adminconfig.AccessPolicyAllowAll))})
	} else {
		rows = append(rows, ButtonRow{b.btn("Switch to allowlist", VerbAccessPol, string(adminconfig.AccessPolicyAllowlist))})
	}

	rows = append(rows, b.footerRow())

	return &Screen{Text: text.String(), Rows: rows}
}

// StatisticsData feeds the statistics screen: the general JSON blob plus,
// when the collector is wired, the interaction view.
type StatisticsData struct {
	General     string
	Interaction *InteractionStatsView
}

// InteractionStatsView is the rendered interaction-statistics section. The
// summary lines are pre-formatted; the screen shows scope-level totals only
// (per-bucket detail lives in the JSON export).
type InteractionStatsView struct {
	Mode            string
	RubricVersion   int
	PeriodStartedAt time.Time
	PeriodResets    uint64
	ScopeLabel      string
	ProfileLabel    string
	ModelLabel      string
	Summary         []string
	Warning         string
	HasData         bool
	// Narrowed marks a scope/filter-narrowed view: its empty state says
	// "no observations match" instead of claiming an empty dataset.
	Narrowed bool
}

func (b screenBuilder) statistics(data StatisticsData) *Screen {
	text := "📊 Statistics\n\n" + data.General

	rows := []ButtonRow{
		{b.btn("Refresh", VerbStats), b.btn("‹ Back", VerbBack)},
	}

	if data.Interaction != nil {
		v := data.Interaction
		interaction := "\n\n🔗 Interaction (" + v.Mode + ", rubric v" + strconv.Itoa(v.RubricVersion) + ")\n"
		interaction += "Period since " + v.PeriodStartedAt.Format("2006-01-02 15:04") +
			" · resets: " + strconv.FormatUint(v.PeriodResets, 10) + "\n"
		interaction += "Scope: " + v.ScopeLabel + " · profile: " + v.ProfileLabel + " · model: " + v.ModelLabel + "\n\n"
		switch {
		case v.HasData:
			for _, line := range v.Summary {
				interaction += line + "\n"
			}
		case v.Narrowed:
			interaction += "No observations match this selection.\n"
		default:
			interaction += "No interaction observations yet.\n"
		}
		if v.Warning != "" {
			interaction += "\n⚠️ " + v.Warning + "\n"
		}
		text += interaction

		rows = append(rows,
			ButtonRow{
				b.btn("Scope ▸ "+truncateRunes(v.ScopeLabel, 16), VerbStatsScope),
				b.btn("Profile ▸ "+truncateRunes(v.ProfileLabel, 16), VerbStatsProfile),
			},
			ButtonRow{
				b.btn("Model ▸ "+truncateRunes(v.ModelLabel, 16), VerbStatsModel),
				b.btn("⬇ Export JSON", VerbStatsExport),
			},
			ButtonRow{
				b.danger("Reset interaction", VerbStatsResetI),
				b.danger("Reset general", VerbStatsResetG),
			},
			ButtonRow{b.btn("🛠 Tool statistics", VerbToolStats)},
		)
	}

	rows = append(rows, ButtonRow{b.btn("⌂ Home", VerbHome)})

	return &Screen{Text: truncateRunes(text, 3800), Rows: rows}
}

// ToolStatsData feeds the tool statistics screen.
type ToolStatsData struct {
	View *ToolStatsView
}

// ToolStatsView is the rendered tool-usage section. The summary lines are
// pre-formatted; the full per-tool detail lives in the JSON export and PNG.
type ToolStatsView struct {
	ScopeLabel      string
	PeriodStartedAt time.Time
	PeriodResets    uint64
	Persistence     string
	Summary         []string
	Warning         string
	HasData         bool
	// Narrowed marks a chat-scoped view: its empty state says "no
	// observations match" instead of claiming an empty dataset.
	Narrowed bool
}

func (b screenBuilder) toolStats(data ToolStatsData) *Screen {
	rows := []ButtonRow{{b.btn("Refresh", VerbToolStats), b.btn("‹ Back", VerbBack)}}
	if data.View == nil {
		rows = append(rows, ButtonRow{b.btn("⌂ Home", VerbHome)})

		return &Screen{Text: "🛠 Tool statistics\n\nStatistics are not available.", Rows: rows}
	}

	v := data.View
	text := "🛠 Tool statistics\n\n"
	text += "Period since " + v.PeriodStartedAt.Format("2006-01-02 15:04") + " · resets: " + strconv.FormatUint(v.PeriodResets, 10) + "\n"
	text += "Scope: " + v.ScopeLabel + "\n" + v.Persistence + "\n\n"
	switch {
	case v.HasData:
		for _, line := range v.Summary {
			text += line + "\n"
		}
	case v.Narrowed:
		text += "No observations match this selection.\n"
	default:
		text += "No tool statistics yet.\n"
	}
	if v.Warning != "" {
		text += "\n⚠️ " + v.Warning + "\n"
	}

	rows = append(rows,
		ButtonRow{
			b.btn("Scope ▸ "+truncateRunes(v.ScopeLabel, 16), VerbStatsScope),
			b.btn("⬇ Export JSON + PNG", VerbToolExport),
		},
		ButtonRow{b.danger("Reset tool statistics", VerbStatsResetT)},
		ButtonRow{b.btn("⌂ Home", VerbHome)},
	)

	return &Screen{Text: truncateRunes(text, 3800), Rows: rows}
}

// StatsScopeData feeds the statistics scope picker.
type StatsScopeData struct {
	Chats   []adminconfig.ChatCatalogEntry
	Current int64
	Total   int
	Page    int
	Pages   int
}

func (b screenBuilder) statsScope(data StatsScopeData) *Screen {
	var text strings.Builder
	text.WriteString("🎯 Statistics scope\n\nPick the chat whose interaction or tool statistics to view, or stay global.\n")
	if data.Total > len(data.Chats) {
		fmt.Fprintf(&text, "Showing %d of %d chats (page %d/%d)\n\n", len(data.Chats), data.Total, data.Page, data.Pages)
	}

	globalLabel := "⌂ Global"
	if data.Current == 0 {
		globalLabel = "✓ Global"
	}
	rows := []ButtonRow{{b.btn(globalLabel, VerbStatsScopeSet, "g")}}
	for i := 0; i < len(data.Chats); i += 2 {
		row := ButtonRow{b.statsScopeChatButton(data.Chats[i], data.Current)}
		if i+1 < len(data.Chats) {
			row = append(row, b.statsScopeChatButton(data.Chats[i+1], data.Current))
		}
		rows = append(rows, row)
	}

	if data.Pages > 1 {
		nav := ButtonRow{}
		if data.Page > 1 {
			nav = append(nav, b.btn("< Prev", VerbStatsScope, strconv.Itoa(data.Page-1)))
		}
		if data.Page < data.Pages {
			nav = append(nav, b.btn("Next >", VerbStatsScope, strconv.Itoa(data.Page+1)))
		}
		rows = append(rows, nav)
	}

	rows = append(rows, b.footerRow())

	return &Screen{Text: truncateRunes(text.String(), 3800), Rows: rows}
}

func (b screenBuilder) statsScopeChatButton(chat adminconfig.ChatCatalogEntry, current int64) Button {
	label := truncateRunes(ChatEntryName(chat), 22)
	if chat.ChatID == current {
		label = "✓ " + label
	}

	return b.btn(label, VerbStatsScopeSet, strconv.FormatInt(chat.ChatID, 10))
}

// ConfirmData feeds the confirmation screen.
type ConfirmData struct {
	Description string
	Danger      bool
}

func (b screenBuilder) confirm(data ConfirmData) *Screen {
	text := "⚠️ Confirm\n\n" + data.Description + "\n"

	confirmButton := b.btn("Confirm", VerbConfirmYes)
	if data.Danger {
		confirmButton = b.danger("Confirm (danger)", VerbConfirmYes)
	}

	rows := []ButtonRow{{confirmButton, b.btn("Cancel", VerbConfirmNo)}}

	return &Screen{Text: text, Rows: rows}
}

// ChatEntryName picks the best display name of a catalog entry.
func ChatEntryName(chat adminconfig.ChatCatalogEntry) string {
	switch {
	case chat.Title != "":
		return chat.Title
	case chat.DisplayName != "":
		return chat.DisplayName
	case chat.Username != "":
		return "@" + chat.Username
	default:
		return strconv.FormatInt(chat.ChatID, 10)
	}
}

func allowlistEntryName(entry adminconfig.AllowlistEntry) string {
	switch {
	case entry.Title != "":
		return entry.Title
	case entry.DisplayName != "":
		return entry.DisplayName
	case entry.Username != "":
		return "@" + entry.Username
	default:
		return strconv.FormatInt(entry.ChatID, 10)
	}
}

func allowlistAddedMark(entry adminconfig.AllowlistEntry) string {
	if entry.AddedBy == 0 {
		return ""
	}

	return fmt.Sprintf(" — added by %d", entry.AddedBy)
}

func chatTypeShort(chatType string) string {
	return chatType
}

func catalogFilterLabel(filter adminconfig.ChatFilter) string {
	switch filter {
	case adminconfig.ChatFilterGroups:
		return "groups"
	case adminconfig.ChatFilterPrivate:
		return "private"
	default:
		return "all"
	}
}

func filterToggleLabel(current, target adminconfig.ChatFilter) string {
	label := catalogFilterLabel(target)
	if current == target {
		return "● " + label
	}

	return "○ " + label
}

func promptFeatureLabel(feature adminconfig.PromptFeature) string {
	switch feature {
	case adminconfig.PromptFeatureChat:
		return "Chat"
	case adminconfig.PromptFeatureSummarize:
		return "Summarize"
	case adminconfig.PromptFeatureImageRecognition:
		return "Image recognition"
	default:
		return string(feature)
	}
}

func displayFieldValue(field adminconfig.EffectiveField) string {
	return field.Spec.EnumLabel(field.Effective)
}

func sourceMark(overridden bool) string {
	if overridden {
		return " [chat override]"
	}

	return ""
}

func boolMark(on bool) string {
	if on {
		return "●"
	}

	return "○"
}

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}

	runes := []rune(text)

	return string(runes[:limit]) + "…"
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}

	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60

	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}
