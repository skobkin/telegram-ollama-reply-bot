package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// Fixture values are deliberately not credential-shaped to stay out of
// secret-scanning; they stand in for a credential mispasted into the wrong
// configuration variable.
const mispastedFixture = "MISPASTEDSECRET"

// TestLoadFailureDoesNotEchoValues pins the actual startup-failure output: a
// credential mispasted into a numeric variable fails the boot, and the error
// an operator sees names the failure without reproducing the value.
func TestLoadFailureDoesNotEchoValues(t *testing.T) {
	t.Setenv("LLM__BACKENDS__OPENAI_COMPAT__API_TOKEN", mispastedFixture)
	t.Setenv("BOT__UNCOMPRESSED_HISTORY_LIMIT", mispastedFixture)

	_, err := Load()
	if err == nil {
		t.Fatal("expected the malformed numeric variable to fail the load")
	}

	if strings.Contains(err.Error(), mispastedFixture) {
		t.Fatalf("load failure echoed the mispasted value: %v", err)
	}

	if !strings.Contains(err.Error(), "unmarshal") {
		t.Fatalf("load failure lost its classification: %v", err)
	}
}

// TestDecimalHookErrorClassifiesWithoutEchoing pins the decode hook error
// shape: the failure classification survives (invalid syntax / out of
// range), the offending value does not.
func TestDecimalHookErrorClassifiesWithoutEchoing(t *testing.T) {
	hook, ok := decimalIntHook().(func(reflect.Type, reflect.Type, any) (any, error))
	if !ok {
		t.Fatal("decode hook does not have the expected signature")
	}

	_, err := hook(reflect.TypeOf(""), reflect.TypeOf(int64(0)), mispastedFixture)
	if err == nil {
		t.Fatal("expected a parse error")
	}

	if strings.Contains(err.Error(), mispastedFixture) {
		t.Fatalf("hook echoed the value: %v", err)
	}

	if !strings.Contains(err.Error(), "invalid syntax") {
		t.Fatalf("hook lost the failure classification: %v", err)
	}
}

// TestContentValidationErrorsDoNotEchoEntries pins the list-valued
// validation errors: entries are free-text operator input and must never be
// quoted back, while the environment variable keeps the failure actionable.
func TestContentValidationErrorsDoNotEchoEntries(t *testing.T) {
	cfg := ContentConfig{}
	cfg.Fetch.Timeout = time.Second
	cfg.Fetch.MaxBodyBytes = 1
	cfg.Fetch.MaxRedirects = 1
	cfg.Fetch.DenyCIDRs = []string{mispastedFixture}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected a validation error")
	}

	if strings.Contains(err.Error(), mispastedFixture) {
		t.Fatalf("validation echoed the mispasted entry: %v", err)
	}

	if !strings.Contains(err.Error(), "CONTENT__FETCH__DENY_CIDRS") {
		t.Fatalf("validation lost the setting name: %v", err)
	}

	cfg.Fetch.DenyCIDRs = nil
	cfg.Extract.Backends = []string{mispastedFixture}

	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected a validation error")
	}

	if strings.Contains(err.Error(), mispastedFixture) {
		t.Fatalf("validation echoed the mispasted entry: %v", err)
	}

	if !strings.Contains(err.Error(), "CONTENT__EXTRACT__BACKENDS") {
		t.Fatalf("validation lost the setting name: %v", err)
	}
}

// TestRecognizerValidationErrorDoesNotEchoValue pins the recognizer error:
// the free-text value is operator input and must never be quoted back.
func TestRecognizerValidationErrorDoesNotEchoValue(t *testing.T) {
	cfg := LLMConfig{}
	cfg.ImageRecognition.Recognizer = mispastedFixture

	err := cfg.validateImageRecognition()
	if err == nil {
		t.Fatal("expected a validation error")
	}

	if strings.Contains(err.Error(), mispastedFixture) {
		t.Fatalf("validation echoed the mispasted value: %v", err)
	}

	if !strings.Contains(err.Error(), "LLM__IMAGE_RECOGNITION__RECOGNIZER") {
		t.Fatalf("validation lost the setting name: %v", err)
	}
}
