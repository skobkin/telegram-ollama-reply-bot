package llm

import (
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

func TestValidatePromptTemplateAcceptsBuiltins(t *testing.T) {
	for _, feature := range []struct {
		name  adminconfig.PromptFeature
		body  string
		label string
	}{
		{adminconfig.PromptFeatureChat, DefaultChatPromptTemplate, "chat"},
		{adminconfig.PromptFeatureSummarize, DefaultSummarizePromptTemplate, "summarize"},
		{adminconfig.PromptFeatureImageRecognition, DefaultImageRecognitionPromptTemplate, "image_recognition"},
	} {
		if err := ValidatePromptTemplate(feature.name, feature.body); err != nil {
			t.Fatalf("builtin %s must self-validate: %v", feature.label, err)
		}
	}
}

func TestValidatePromptTemplateAcceptsAllLegitimatePlaceholders(t *testing.T) {
	body := "{{.Language}} {{.Model}} {{.Context}} {{.Gender}} {{.CharacterName}} {{.ToneMode}} " +
		"{{.AllowTeasing}} {{.ImageRecognitionEnabled}} {{.ToolPolicy}}"

	if err := ValidatePromptTemplate(adminconfig.PromptFeatureChat, body); err != nil {
		t.Fatalf("all chat placeholders must validate: %v", err)
	}

	if err := ValidatePromptTemplate(adminconfig.PromptFeatureSummarize, "{{.Language}} {{.MaxLength}}"); err != nil {
		t.Fatalf("summarize placeholders must validate: %v", err)
	}

	if err := ValidatePromptTemplate(adminconfig.PromptFeatureImageRecognition, "{{.Language}}"); err != nil {
		t.Fatalf("image recognition placeholders must validate: %v", err)
	}
}

func TestValidatePromptTemplateRejectsUnknownPlaceholder(t *testing.T) {
	err := ValidatePromptTemplate(adminconfig.PromptFeatureChat, "Hello {{.Bogus}}")
	if err == nil {
		t.Fatalf("unknown placeholder must be rejected")
	}
	if !strings.Contains(err.Error(), "chat") || !strings.Contains(err.Error(), "Bogus") {
		t.Fatalf("error should name the feature and field, got: %v", err)
	}
}

func TestValidatePromptTemplateRejectsBranchConditionalDefects(t *testing.T) {
	cases := []struct {
		label string
		body  string
	}{
		// The false branch references an unknown field: parse-only checks
		// pass, execution on the allow_teasing=false dataset must fail.
		{"false-branch", "{{if .AllowTeasing}}ok{{else}}{{.Bogus}}{{end}}"},
		// Symmetric case: only the true branch is broken.
		{"true-branch", "{{if .ImageRecognitionEnabled}}{{.Bogus}}{{else}}ok{{end}}"},
		// Unknown field inside a boolean-driven range of either flag.
		{"recognition-gated", "{{if .ImageRecognitionEnabled}}{{.Nope}}{{end}}"},
	}

	for _, testCase := range cases {
		if err := ValidatePromptTemplate(adminconfig.PromptFeatureChat, testCase.body); err == nil {
			t.Fatalf("%s: expected branch-conditional defect to be rejected", testCase.label)
		}
	}
}

func TestValidatePromptTemplateReportsFailingDataset(t *testing.T) {
	err := ValidatePromptTemplate(adminconfig.PromptFeatureChat, "{{if .AllowTeasing}}ok{{else}}{{.Bogus}}{{end}}")
	if err == nil {
		t.Fatalf("expected rejection")
	}
	if !strings.Contains(err.Error(), "allow_teasing=false") {
		t.Fatalf("error should name the failing dataset, got: %v", err)
	}
}

func TestValidatePromptTemplateRejectsSyntaxErrors(t *testing.T) {
	if err := ValidatePromptTemplate(adminconfig.PromptFeatureSummarize, "{{.Language"); err == nil {
		t.Fatalf("malformed template must be rejected")
	}
}

func TestValidatePromptTemplateRejectsUnknownFeature(t *testing.T) {
	if err := ValidatePromptTemplate(adminconfig.PromptFeature("nope"), "x"); err == nil {
		t.Fatalf("unknown feature must be rejected")
	}
}

// TestValidatePromptTemplatePlaceholdersNotRequired proves a minimal body
// using no placeholders at all is still valid.
func TestValidatePromptTemplatePlaceholdersNotRequired(t *testing.T) {
	if err := ValidatePromptTemplate(adminconfig.PromptFeatureChat, "You are a fox."); err != nil {
		t.Fatalf("plain body must validate: %v", err)
	}
}
