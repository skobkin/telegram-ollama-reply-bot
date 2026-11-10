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
	"character_name":             "cn",
	"language":                   "lg",
	"gender":                     "gd",
	"tone_mode":                  "tm",
	"allow_teasing":              "at",
	"interactivity_mode":         "im",
	"default_interactivity_mode": "im",
}

var codeFields = func() map[string]string {
	reversed := make(map[string]string, len(fieldCodes))
	for name, code := range fieldCodes {
		if existing, ok := reversed[code]; ok && existing != name {
			// Both interactivity fields intentionally share "im": the scope
			// (global vs chat screen) disambiguates them.
			continue
		}
		reversed[code] = name
	}

	return reversed
}()

// FieldCode returns the short callback code for a settings field name.
func FieldCode(field string) (string, bool) {
	code, ok := fieldCodes[field]

	return code, ok
}

// FieldName resolves a short callback code back to a field name. The "im"
// code resolves per scope: global maps to default_interactivity_mode, chat
// maps to interactivity_mode.
func FieldName(code string, global bool) (string, bool) {
	if code == "im" {
		if global {
			return "default_interactivity_mode", true
		}

		return "interactivity_mode", true
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
