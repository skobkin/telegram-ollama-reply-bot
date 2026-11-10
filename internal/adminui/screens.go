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
	rows = append(rows, ButtonRow{
		b.btn(allowlistLabel, allowlistVerb, strconv.FormatInt(data.Info.ChatID, 10)),
		b.btn("Edit settings", VerbChatSet),
	})

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
	}

	for _, spec := range specs {
		value, overridden := data.valueOf(spec)
		line := fmt.Sprintf("• %s: %s", spec.Label, truncateRunes(value, 60))
		if overridden {
			line += " [chat override]"
		}
		text.WriteString(line + "\n")
	}

	rows := make([]ButtonRow, 0, 5)
	for i := 0; i < len(specs); i += 2 {
		row := ButtonRow{b.settingsFieldButton(specs[i])}
		if i+1 < len(specs) {
			row = append(row, b.settingsFieldButton(specs[i+1]))
		}
		rows = append(rows, row)
	}

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
	case "tone_mode":
		return settings.ToneMode
	case "allow_teasing":
		return strconv.FormatBool(settings.AllowTeasing)
	case "default_interactivity_mode":
		return string(settings.DefaultInteractivityMode)
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
		rows = append(rows, ButtonRow{b.btn("✏️ Enter value…", VerbString, code)})
	}

	rows = append(rows, b.footerRow())

	return &Screen{Text: text.String(), Rows: rows}
}

// PromptsMenuData feeds the prompts menu screen.
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

// StatisticsData feeds the statistics screen.
type StatisticsData struct {
	Snapshot string
}

func (b screenBuilder) statistics(data StatisticsData) *Screen {
	text := "📊 Statistics\n\n" + data.Snapshot

	rows := []ButtonRow{
		{b.btn("Refresh", VerbStats), b.btn("‹ Back", VerbBack)},
		{b.btn("⌂ Home", VerbHome)},
	}

	return &Screen{Text: truncateRunes(text, 3800), Rows: rows}
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
