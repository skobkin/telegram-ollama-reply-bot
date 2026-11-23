package adminui

import (
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

func TestCallbackCodecRoundTrip(t *testing.T) {
	cases := []struct {
		verb string
		args []string
		seq  uint32
	}{
		{VerbHome, nil, 7},
		{VerbCatalog, []string{"2"}, 7},
		{VerbCatalog, []string{"gr"}, 7},
		{VerbChat, []string{"-1001234567890"}, 4294967295},
		{VerbValue, []string{"tm", "mentions_only"}, 99},
		{VerbValue, []string{"at", "off"}, 99},
		{VerbPromptFeat, []string{"ch"}, 12},
		{VerbPromptRstG, []string{"su"}, 12},
		{VerbAllowRem, []string{"-1001234567890"}, 4294967295},
		{VerbAccessPol, []string{"allow_all"}, 1000},
		{VerbConfirmYes, nil, 3},
		{VerbInteractivity, nil, 12},
		{VerbTools, nil, 12},
		{VerbValue, []string{"ws", "inh"}, 12},
		{VerbField, []string{"so"}, 12},
		{VerbValue, []string{"ip", "talk_high"}, 12},
		{VerbModelList, []string{"cm"}, 99},
		{VerbModelPage, []string{"2"}, 99},
		{VerbModelPick, []string{"so", "0"}, 99},
		{VerbClearGlobal, []string{"so"}, 99},
	}

	for _, testCase := range cases {
		data := EncodeAction(testCase.verb, testCase.seq, testCase.args...)
		if len(data) > MaxCallbackBytes {
			t.Fatalf("%s exceeds %d bytes: %q (%d)", testCase.verb, MaxCallbackBytes, data, len(data))
		}

		action, err := DecodeAction(data)
		if err != nil {
			t.Fatalf("decode %q: %v", data, err)
		}
		if action.Verb != testCase.verb || action.Seq != testCase.seq {
			t.Fatalf("round trip mismatch: got %+v want verb %s seq %d", action, testCase.verb, testCase.seq)
		}
		if strings.Join(action.Args, ",") != strings.Join(testCase.args, ",") {
			t.Fatalf("args mismatch: got %v want %v", action.Args, testCase.args)
		}
	}
}

func TestCallbackWorstCaseFitsLimit(t *testing.T) {
	worst := EncodeAction(VerbChat, 4294967295, "-1001234567890")
	if len(worst) > MaxCallbackBytes {
		t.Fatalf("worst case chat callback too long: %q (%d bytes)", worst, len(worst))
	}

	worstValue := EncodeAction(VerbValue, 4294967295, "im", "mentions_or_replies")
	if len(worstValue) > MaxCallbackBytes {
		t.Fatalf("worst case value callback too long: %q (%d bytes)", worstValue, len(worstValue))
	}

	// Model picker picks carry an index into the cached list, never the
	// model ID itself — the ID overflows the limit. A five-digit index is
	// already far beyond any realistic backend list (8 per page).
	worstPick := EncodeAction(VerbModelPick, 4294967295, "so", "99999")
	if len(worstPick) > MaxCallbackBytes {
		t.Fatalf("worst case model pick too long: %q (%d bytes)", worstPick, len(worstPick))
	}

	// Enabling the longest profile name through the scope confirmation.
	worstProfile := EncodeAction(VerbValue, 4294967295, "ip", "non_interactive")
	if len(worstProfile) > MaxCallbackBytes {
		t.Fatalf("worst case profile value too long: %q (%d bytes)", worstProfile, len(worstProfile))
	}
}

func TestDecodeActionRejectsMalformed(t *testing.T) {
	bad := []string{
		"",
		"home:7",         // wrong prefix
		"a:",             // nothing
		"a:home",         // no sequence
		"a:home:x",       // non-numeric sequence
		"a:nosuchverb:7", // unknown verb
		"a:home:-1",      // negative sequence
		"a::7",           // empty verb
		"a:cat:gr",       // last part not a number
	}
	for _, data := range bad {
		if _, err := DecodeAction(data); err == nil {
			t.Fatalf("expected %q to be rejected", data)
		}
	}

	// Extra middle segments stay in Args — only the last part is the seq.
	action, err := DecodeAction("a:cat:gr:s:9")
	if err != nil || action.Verb != VerbCatalog || len(action.Args) != 2 || action.Seq != 9 {
		t.Fatalf("unexpected parse: %+v err %v", action, err)
	}
}

func TestFieldCodesCoverRegistry(t *testing.T) {
	for _, spec := range adminconfig.GlobalFieldSpecs() {
		if _, ok := FieldCode(spec.Name); !ok {
			t.Fatalf("global field %q has no callback code", spec.Name)
		}
	}
	for _, spec := range adminconfig.ChatFieldSpecs() {
		if _, ok := FieldCode(spec.Name); !ok {
			t.Fatalf("chat field %q has no callback code", spec.Name)
		}
	}
}

func TestFieldNameScopeDisambiguation(t *testing.T) {
	global, ok := FieldName("im", true)
	if !ok || global != "default_interactivity_mode" {
		t.Fatalf("global im should map to default_interactivity_mode, got %q", global)
	}

	chat, ok := FieldName("im", false)
	if !ok || chat != "interactivity_mode" {
		t.Fatalf("chat im should map to interactivity_mode, got %q", chat)
	}

	summaryGlobal, ok := FieldName("as", true)
	if !ok || summaryGlobal != "default_auto_summary_enabled" {
		t.Fatalf("global as should map to default_auto_summary_enabled, got %q", summaryGlobal)
	}

	summaryChat, ok := FieldName("as", false)
	if !ok || summaryChat != "auto_summary_enabled" {
		t.Fatalf("chat as should map to auto_summary_enabled, got %q", summaryChat)
	}

	name, ok := FieldName("cn", false)
	if !ok || name != "character_name" {
		t.Fatalf("cn should map to character_name, got %q", name)
	}

	// Model/interactivity codes resolve per scope; cm/so/di share one name.
	for _, testCase := range []struct {
		code          string
		global, chat  string
		sameNameScope bool // code maps to the same field in both scopes
	}{
		{"cm", "chat_model", "chat_model", true},
		{"so", "systemone_model", "systemone_model", true},
		{"ip", "default_interaction_profile", "interaction_profile", false},
		{"di", "decision_instructions", "decision_instructions", true},
	} {
		gotGlobal, ok := FieldName(testCase.code, true)
		if !ok || gotGlobal != testCase.global {
			t.Fatalf("global %s should map to %s, got %q", testCase.code, testCase.global, gotGlobal)
		}
		gotChat, ok := FieldName(testCase.code, false)
		if !ok || gotChat != testCase.chat {
			t.Fatalf("chat %s should map to %s, got %q", testCase.code, testCase.chat, gotChat)
		}
		if (gotGlobal == gotChat) != testCase.sameNameScope {
			t.Fatalf("%s scope mapping unexpected: global %q chat %q", testCase.code, gotGlobal, gotChat)
		}
	}

	if _, ok := FieldName("zz", false); ok {
		t.Fatalf("unknown code must fail")
	}
}

func TestPromptFeatureCodes(t *testing.T) {
	for _, feature := range []adminconfig.PromptFeature{
		adminconfig.PromptFeatureChat,
		adminconfig.PromptFeatureSummarize,
		adminconfig.PromptFeatureImageRecognition,
	} {
		code, ok := PromptFeatureCode(feature)
		if !ok {
			t.Fatalf("feature %q has no code", feature)
		}
		resolved, ok := PromptFeatureFromCode(code)
		if !ok || resolved != feature {
			t.Fatalf("code %q should resolve to %q, got %q", code, feature, resolved)
		}
	}

	if _, ok := PromptFeatureFromCode("zz"); ok {
		t.Fatalf("unknown prompt code must fail")
	}
}
