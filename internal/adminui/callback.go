// Package adminui holds the UI-logic layer of the interactive admin
// interface: sessions, the callback codec, screen models, and the controller
// that orchestrates adminconfig operations into screens and effects. The
// package is Telegram-free by design — only the adapter in
// internal/telegram/adminui talks to the Bot API — so a future Widgets
// frontend can reuse the controller unchanged.
package adminui

import (
	"fmt"
	"strconv"
	"strings"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

// CallbackPrefix marks admin UI callback data.
const CallbackPrefix = "a:"

// Action verbs of the admin UI callback protocol. Format:
//
//	a:<verb>[:<arg>...]:<seq>
//
// The sequence number is always last and echoes the session Seq at render
// time, so stale button presses are detectable.
const (
	VerbHome       = "home" // → dashboard
	VerbCatalog    = "cat"  // args: <page> | <all|gr|pr> filter | s (open search prompt)
	VerbChat       = "chat" // args: <chat_id> → chat card
	VerbGlobalSet  = "gs"   // → global settings screen
	VerbChatSet    = "cs"   // → chat settings screen (target chat in state)
	VerbField      = "fld"  // args: <code> → field editor
	VerbValue      = "val"  // args: <code>,<value> → apply enum/tri-state value
	VerbString     = "str"  // args: <code> → request string input via ForceReply
	VerbClear      = "clr"  // args: <code> → clear override / reset to global value
	VerbPrompts    = "pr"   // → prompts menu
	VerbPromptFeat = "pf"   // args: <ch|su|ir> → prompt detail
	VerbPromptView = "pv"   // args: <feat> → view active prompt body
	VerbPromptDL   = "pd"   // args: <feat> → download active prompt
	VerbPromptBL   = "pb"   // args: <feat> → download builtin prompt
	VerbPromptText = "pt"   // args: <feat> → replace via text input
	VerbPromptUp   = "pu"   // args: <feat> → upload document
	VerbPromptRstC = "prc"  // args: <feat> → reset chat prompt (confirm)
	VerbPromptRstG = "pbg"  // args: <feat> → restore builtin global (confirm)
	VerbAccess     = "ac"   // → access screen
	VerbAllowAdd   = "aw"   // args: <chat_id> → allowlist add
	VerbAllowRem   = "ar"   // args: <chat_id> → allowlist remove (confirm)
	VerbAccessPol  = "ap"   // args: <allowlist|allow_all> → switch policy (confirm)
	VerbResetChat  = "rov"  // → reset ALL chat overrides (confirm)
	VerbBack       = "back" // pop the navigation stack
	VerbConfirmYes = "ok"   // confirm the pending confirmation
	VerbConfirmNo  = "no"   // cancel the pending confirmation
	VerbStats      = "st"   // → statistics screen

	// Statistics screen actions (#161): export, filter cycling, scope
	// picking, and confirmation-gated resets.
	VerbStatsExport   = "sx"  // export the current scope+filters as a JSON document
	VerbStatsProfile  = "sp"  // cycle the profile filter (All → profiles → All)
	VerbStatsModel    = "sm"  // cycle the decision-model filter (All → observed → All)
	VerbStatsScope    = "sk"  // → scope picker (catalog chats + global)
	VerbStatsScopeSet = "sc"  // args: g | <chat_id> → set the statistics scope
	VerbStatsResetI   = "sri" // reset interaction statistics (confirm, global)
	VerbStatsResetG   = "srg" // reset general statistics (confirm, global)

	// Tool usage statistics (#178): own screen, export and reset.
	VerbToolStats   = "tl"  // → tool statistics screen (scope from the statistics screen)
	VerbToolExport  = "tx"  // export tool statistics (JSON + PNG)
	VerbStatsResetT = "srt" // reset tool usage statistics (confirm, global)

	// Model selection and interactivity (#128 / System One).
	VerbInteractivity = "in" // → interactivity submenu (scope from screen)
	VerbModelList     = "ml" // args: <code> → fetch and cache the backend model list
	VerbModelPage     = "mg" // args: <page> → paginate the cached model list
	VerbModelPick     = "mo" // args: <code>,<index> → apply cached model at index
	VerbClearGlobal   = "cg" // args: <code> → clear a clearable global field

	// Sticker catalog management.
	VerbStickers         = "stk" // [args: <page>] → stickers screen
	VerbStickerSet       = "sto" // args: <index> → set card (index into the screen's set snapshot)
	VerbStickerExportNew = "stx" // → prompt for a set name or link to export
	VerbStickerExportCur = "ste" // → re-export the set card's set
	VerbStickerImport    = "sti" // → prompt for an edited template upload
	VerbStickerToggle    = "stt" // → enable/disable the set card's set
	VerbStickerRemove    = "sdl" // → remove the set card's set (confirm)
	VerbStickerZipNew    = "stz" // → prompt for a set name or link to export as ZIP
	VerbStickerZipCur    = "stc" // → export the set card's set as ZIP (YAML + sticker files)
)

// knownVerbs is the closed verb set accepted by Decode.
var knownVerbs = map[string]bool{
	VerbHome: true, VerbCatalog: true, VerbChat: true, VerbGlobalSet: true,
	VerbChatSet: true, VerbField: true, VerbValue: true, VerbString: true,
	VerbClear: true, VerbPrompts: true, VerbPromptFeat: true, VerbPromptView: true,
	VerbPromptDL: true, VerbPromptBL: true, VerbPromptText: true, VerbPromptUp: true,
	VerbPromptRstC: true, VerbPromptRstG: true, VerbAccess: true, VerbAllowAdd: true,
	VerbAllowRem: true, VerbAccessPol: true, VerbResetChat: true, VerbBack: true,
	VerbConfirmYes: true, VerbConfirmNo: true, VerbStats: true,
	VerbInteractivity: true, VerbModelList: true, VerbModelPage: true,
	VerbModelPick: true, VerbClearGlobal: true,
	VerbStatsExport: true, VerbStatsProfile: true, VerbStatsModel: true,
	VerbStatsScope: true, VerbStatsScopeSet: true,
	VerbStatsResetI: true, VerbStatsResetG: true,
	VerbToolStats: true, VerbToolExport: true, VerbStatsResetT: true,
	VerbStickers: true, VerbStickerSet: true, VerbStickerExportNew: true, VerbStickerExportCur: true,
	VerbStickerImport: true, VerbStickerToggle: true, VerbStickerRemove: true,
	VerbStickerZipNew: true, VerbStickerZipCur: true,
}

// MaxCallbackBytes is the Telegram callback data limit.
const MaxCallbackBytes = 64

// Action is a decoded admin UI callback.
type Action struct {
	Verb string
	Args []string
	Seq  uint32
}

// EncodeAction serializes an action into Telegram callback data. Every
// emitted button must stay within MaxCallbackBytes; the codec test enforces
// the worst cases.
func EncodeAction(verb string, seq uint32, args ...string) string {
	parts := make([]string, 0, len(args)+3)
	parts = append(parts, "a", verb)
	parts = append(parts, args...)
	parts = append(parts, strconv.FormatUint(uint64(seq), 10))

	return strings.Join(parts, ":")
}

// DecodeAction parses callback data back into an Action. Anything malformed
// (wrong prefix, missing sequence, non-numeric sequence, unknown verb)
// returns an error the adapter maps to an "Unknown action" toast.
func DecodeAction(data string) (Action, error) {
	if !strings.HasPrefix(data, CallbackPrefix) {
		return Action{}, fmt.Errorf("callback data must start with %q", CallbackPrefix)
	}

	parts := strings.Split(strings.TrimPrefix(data, CallbackPrefix), ":")
	if len(parts) < 2 {
		return Action{}, fmt.Errorf("callback data needs a verb and a sequence")
	}

	seqRaw := parts[len(parts)-1]
	seq, err := strconv.ParseUint(seqRaw, 10, 32)
	if err != nil {
		return Action{}, fmt.Errorf("callback sequence %q is not a number", seqRaw)
	}

	verb := parts[0]
	if !knownVerbs[verb] {
		return Action{}, fmt.Errorf("unknown callback verb %q", verb)
	}

	return Action{Verb: verb, Args: parts[1 : len(parts)-1], Seq: uint32(seq)}, nil
}

// fieldCodes maps domain field names to short callback codes. The mapping
// lives here (not in adminconfig) to keep the domain layer free of callback
// encoding concerns.
var fieldCodes = map[string]string{
	"character_name":               "cn",
	"language":                     "lg",
	"gender":                       "gd",
	"interactivity_mode":           "im",
	"default_interactivity_mode":   "im",
	"auto_summary_enabled":         "as",
	"default_auto_summary_enabled": "as",
	"show_reply_previews":          "rp",
	"preview_mode":                 "pv",
	"default_preview_mode":         "pv",
	"keep_tool_history":            "kh",
	"web_search_enabled":           "ws",
	"chat_model":                   "cm",
	"summarize_model":              "sm",
	"image_recognition_model":      "iv",
	"systemone_model":              "so",
	"interaction_profile":          "ip",
	"default_interaction_profile":  "ip",
	"decision_instructions":        "di",
}

var codeFields = func() map[string]string {
	reversed := make(map[string]string, len(fieldCodes))
	for name, code := range fieldCodes {
		if existing, ok := reversed[code]; ok && existing != name {
			// The interactivity pair shares "im" and the auto-summary pair
			// shares "as": the scope (global vs chat screen) disambiguates
			// them, exactly like the interactivity fields.
			continue
		}
		reversed[code] = name
	}

	return reversed
}()

// scopedFieldNames maps a shared code to its per-scope field names.
var scopedFieldNames = map[string]struct {
	global string
	chat   string
}{
	"im": {global: "default_interactivity_mode", chat: "interactivity_mode"},
	"as": {global: "default_auto_summary_enabled", chat: "auto_summary_enabled"},
	"pv": {global: "default_preview_mode", chat: "preview_mode"},
	"kh": {global: "keep_tool_history", chat: "keep_tool_history"},
	"ws": {global: "web_search_enabled", chat: "web_search_enabled"},
	"cm": {global: "chat_model", chat: "chat_model"},
	"so": {global: "systemone_model", chat: "systemone_model"},
	"ip": {global: "default_interaction_profile", chat: "interaction_profile"},
	"di": {global: "decision_instructions", chat: "decision_instructions"},
}

// FieldCode returns the short callback code for a settings field name.
func FieldCode(field string) (string, bool) {
	code, ok := fieldCodes[field]

	return code, ok
}

// FieldName resolves a short callback code back to a field name. Codes
// shared by a global/chat pair ("im", "as") resolve per scope: global maps
// to the default_* field, chat maps to the per-chat field.
func FieldName(code string, global bool) (string, bool) {
	if scoped, ok := scopedFieldNames[code]; ok {
		if global {
			return scoped.global, true
		}

		return scoped.chat, true
	}

	name, ok := codeFields[code]

	return name, ok
}

// promptFeatureCodes maps prompt features to short codes.
var promptFeatureCodes = map[string]adminconfig.PromptFeature{
	"ch": adminconfig.PromptFeatureChat,
	"su": adminconfig.PromptFeatureSummarize,
	"ir": adminconfig.PromptFeatureImageRecognition,
}

var promptCodeFeatures = func() map[adminconfig.PromptFeature]string {
	reversed := make(map[adminconfig.PromptFeature]string, len(promptFeatureCodes))
	for code, feature := range promptFeatureCodes {
		reversed[feature] = code
	}

	return reversed
}()

// PromptFeatureCode returns the short callback code for a prompt feature.
func PromptFeatureCode(feature adminconfig.PromptFeature) (string, bool) {
	code, ok := promptCodeFeatures[feature]

	return code, ok
}

// PromptFeatureFromCode resolves a short code back to a prompt feature.
func PromptFeatureFromCode(code string) (adminconfig.PromptFeature, bool) {
	feature, ok := promptFeatureCodes[code]

	return feature, ok
}
