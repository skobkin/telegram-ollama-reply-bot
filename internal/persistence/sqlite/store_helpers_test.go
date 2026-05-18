package sqlite

import (
	"strings"
	"testing"
)

func TestGlobalFieldAssignmentUnsupportedInteractivityModeListsSupportedValues(t *testing.T) {
	t.Parallel()

	_, _, err := globalFieldAssignment("default_interactivity_mode", "unknown_mode")
	if err == nil {
		t.Fatal("expected error")
	}

	message := err.Error()
	if !strings.Contains(message, `unsupported interactivity mode "unknown_mode"`) {
		t.Fatalf("unexpected error: %q", message)
	}
	if !strings.Contains(message, "disabled, mentions_only, mentions_or_replies") {
		t.Fatalf("expected supported values in error, got %q", message)
	}
}

func TestChatFieldAssignmentUnsupportedInteractivityModeListsSupportedValues(t *testing.T) {
	t.Parallel()

	_, _, err := chatFieldAssignment("interactivity_mode", "unknown_mode")
	if err == nil {
		t.Fatal("expected error")
	}

	message := err.Error()
	if !strings.Contains(message, `unsupported interactivity mode "unknown_mode"`) {
		t.Fatalf("unexpected error: %q", message)
	}
	if !strings.Contains(message, "disabled, mentions_only, mentions_or_replies") {
		t.Fatalf("expected supported values in error, got %q", message)
	}
}
