package adminconfig

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// recordingSettingsStore records settings writes on top of the shared stub.
type recordingSettingsStore struct {
	*stubStore

	mu       sync.Mutex
	failNext map[string]error // first call to the named method fails once
	calls    []string
}

func (s *recordingSettingsStore) SetGlobalField(_ context.Context, field, value string, actorID int64) error {
	s.mu.Lock()
	s.calls = append(s.calls, "SetGlobalField:"+field+"="+value)
	s.mu.Unlock()

	return s.stubStore.SetGlobalField(context.Background(), field, value, actorID)
}

func (s *recordingSettingsStore) SetChatField(_ context.Context, chatID int64, field, value string, actorID int64) error {
	s.mu.Lock()
	s.calls = append(s.calls, "SetChatField:"+field+"="+value)
	s.mu.Unlock()

	return s.stubStore.SetChatField(context.Background(), chatID, field, value, actorID)
}

func (s *recordingSettingsStore) ClearChatField(_ context.Context, chatID int64, field string, actorID int64) error {
	s.mu.Lock()
	err, ok := s.failNext["ClearChatField"]
	if ok {
		delete(s.failNext, "ClearChatField")
	} else {
		s.calls = append(s.calls, "ClearChatField:"+field)
	}
	s.mu.Unlock()

	if err != nil {
		return err
	}

	return s.stubStore.ClearChatField(context.Background(), chatID, field, actorID)
}

// failOnce arms a one-shot persistence failure on ClearChatField.
func (s *recordingSettingsStore) failOnce(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failNext == nil {
		s.failNext = make(map[string]error)
	}
	s.failNext[method] = err
}

func (s *recordingSettingsStore) writeCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.calls...)
}

func newSettingsTestService(t *testing.T) (*Service, *recordingSettingsStore) {
	t.Helper()

	store := &recordingSettingsStore{stubStore: &stubStore{}}

	return NewService(store, nil, nil), store
}

func TestSetGlobalSettingValidatesAndCanonicalizes(t *testing.T) {
	service, store := newSettingsTestService(t)

	if err := service.SetGlobalSetting(context.Background(), "default_auto_summary_enabled", " True ", 1); err != nil {
		t.Fatalf("valid boolean: %v", err)
	}
	if err := service.SetGlobalSetting(context.Background(), "language", "  Русский ", 1); err != nil {
		t.Fatalf("valid string: %v", err)
	}
	if err := service.SetGlobalSetting(context.Background(), "default_interactivity_mode", "mentions_only", 1); err != nil {
		t.Fatalf("valid enum: %v", err)
	}

	want := []string{
		"SetGlobalField:default_auto_summary_enabled=true",
		"SetGlobalField:language=Русский",
		"SetGlobalField:default_interactivity_mode=mentions_only",
	}
	got := store.writeCalls()
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("calls mismatch:\ngot:  %v\nwant: %v", got, want)
	}
}

func TestSetGlobalSettingRejectsInvalidWithoutWrite(t *testing.T) {
	service, store := newSettingsTestService(t)

	invalid := []struct {
		field, value string
	}{
		{"default_interactivity_mode", "proactive"},  // unsupported enum
		{"language", "   "},                          // empty after trim
		{"character_name", strings.Repeat("x", 129)}, // over MaxLen
		{"access_policy", "allow_all"},               // not a settings field
		{"no_such_field", "x"},
	}
	for _, testCase := range invalid {
		if err := service.SetGlobalSetting(context.Background(), testCase.field, testCase.value, 1); err == nil {
			t.Fatalf("field %s value %q must be rejected", testCase.field, testCase.value)
		}
	}
	if calls := store.writeCalls(); len(calls) != 0 {
		t.Fatalf("invalid input must never reach the store: %v", calls)
	}
}

func TestSetChatSettingAndClearOverride(t *testing.T) {
	service, store := newSettingsTestService(t)

	if err := service.SetChatSetting(context.Background(), -1, "auto_summary_enabled", "false", 1); err != nil {
		t.Fatalf("valid tri-state value: %v", err)
	}
	if err := service.SetChatSetting(context.Background(), -1, "auto_summary_enabled", "maybe", 1); err == nil {
		t.Fatalf("invalid boolean must be rejected")
	}
	if err := service.ClearChatOverride(context.Background(), -1, "interactivity_mode", 1); err != nil {
		t.Fatalf("clear override: %v", err)
	}
	if err := service.ClearChatOverride(context.Background(), -1, "access_policy", 1); err == nil {
		t.Fatalf("access_policy must not be clearable as an override")
	}

	want := []string{"SetChatField:auto_summary_enabled=false", "ClearChatField:interactivity_mode"}
	got := store.writeCalls()
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("calls mismatch:\ngot:  %v\nwant: %v", got, want)
	}
}

// TestClearAllChatOverridesIsSingleAtomicWrite pins the atomic reset: the
// whole override set goes in ONE store operation backed by a single SQL
// DELETE — never a per-field loop that could leave the chat partially reset
// when a middle write fails.
func TestClearAllChatOverridesIsSingleAtomicWrite(t *testing.T) {
	service, store := newSettingsTestService(t)

	if err := service.ClearAllChatOverrides(context.Background(), -100, 1); err != nil {
		t.Fatalf("clear all: %v", err)
	}

	want := []string{"ClearChatField:all"}
	got := store.writeCalls()
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("reset-all must be one atomic store call, got %v", got)
	}
}

// TestClearAllChatOverridesFailureLeavesNothingWritten verifies the failure
// mode of the atomic reset: the one store call fails, the error surfaces, and
// no other write of any kind is attempted — a transient SQLite outage cannot
// produce a partially reset chat configuration.
func TestClearAllChatOverridesFailureLeavesNothingWritten(t *testing.T) {
	service, store := newSettingsTestService(t)
	store.failOnce("ClearChatField", context.DeadlineExceeded)

	if err := service.ClearAllChatOverrides(context.Background(), -100, 1); err == nil {
		t.Fatalf("the failed reset must surface its error")
	}

	if got := store.writeCalls(); len(got) != 0 {
		t.Fatalf("a failed reset must not write anything, got %v", got)
	}
}

func TestSetGlobalSettingAcceptsModelAndInteractivityFields(t *testing.T) {
	svc, store := newSettingsTestService(t)

	valid := map[string]string{
		"chat_model":                  "gemma4:e4b",
		"summarize_model":             "summarizer-8b",
		"image_recognition_model":     "vision-mini",
		"systemone_model":             "laya",
		"default_interaction_profile": "talk_low",
		"decision_instructions":       "Engage with code.\nSecond line.",
	}
	for field, value := range valid {
		if err := svc.SetGlobalSetting(context.Background(), field, value, 42); err != nil {
			t.Fatalf("set %s: %v", field, err)
		}
	}

	calls := store.writeCalls()
	if len(calls) != len(valid) {
		t.Fatalf("expected %d writes, got %v", len(valid), calls)
	}
	if !containsCall(calls, "SetGlobalField:chat_model=gemma4:e4b") ||
		!containsCall(calls, "SetGlobalField:default_interaction_profile=talk_low") ||
		!containsCall(calls, "SetGlobalField:decision_instructions=Engage with code.\nSecond line.") {
		t.Fatalf("missing expected writes: %v", calls)
	}
}

func TestSetGlobalSettingRejectsInvalidModelAndProfileValues(t *testing.T) {
	svc, store := newSettingsTestService(t)

	invalid := map[string]string{
		"default_interaction_profile": "chaotic",
		"decision_instructions":       strings.Repeat("x", 4001),
		"chat_model":                  "   ",
	}
	for field, value := range invalid {
		if err := svc.SetGlobalSetting(context.Background(), field, value, 42); err == nil {
			t.Fatalf("expected %q to be rejected for %s", value, field)
		}
	}

	if calls := store.writeCalls(); len(calls) != 0 {
		t.Fatalf("invalid values must never reach the store, got %v", calls)
	}
}

func TestClearGlobalSettingClearsClearableFieldsOnly(t *testing.T) {
	svc, store := newSettingsTestService(t)

	for _, field := range []string{"chat_model", "summarize_model", "image_recognition_model", "systemone_model", "decision_instructions"} {
		if err := svc.ClearGlobalSetting(context.Background(), field, 42); err != nil {
			t.Fatalf("clear %s: %v", field, err)
		}
	}

	calls := store.writeCalls()
	if len(calls) != 5 {
		t.Fatalf("expected 5 clear writes, got %v", calls)
	}
	for _, field := range []string{"chat_model", "summarize_model", "image_recognition_model", "systemone_model", "decision_instructions"} {
		if !containsCall(calls, "SetGlobalField:"+field+"=") {
			t.Fatalf("expected empty write for %s, got %v", field, calls)
		}
	}

	// Booleans, enums, and plain identity strings have no unset state.
	for _, field := range []string{"character_name", "language", "default_interaction_profile", "default_auto_summary_enabled"} {
		if err := svc.ClearGlobalSetting(context.Background(), field, 42); err == nil {
			t.Fatalf("expected %s to refuse clearing", field)
		}
	}
	if calls := store.writeCalls(); len(calls) != 5 {
		t.Fatalf("refused clears must not write, got %v", calls)
	}
}

func TestSetChatSettingAcceptsModelAndInteractivityFields(t *testing.T) {
	svc, store := newSettingsTestService(t)

	valid := map[string]string{
		"chat_model":            "chat-override-8b",
		"systemone_model":       "julia-1",
		"interaction_profile":   "reactions_high",
		"decision_instructions": "Never discuss politics.",
	}
	for field, value := range valid {
		if err := svc.SetChatSetting(context.Background(), 555, field, value, 42); err != nil {
			t.Fatalf("set %s: %v", field, err)
		}
	}

	calls := store.writeCalls()
	if len(calls) != len(valid) {
		t.Fatalf("expected %d writes, got %v", len(valid), calls)
	}

	// Clearing restores inherit semantics.
	for _, field := range []string{"chat_model", "systemone_model", "interaction_profile", "decision_instructions"} {
		if err := svc.ClearChatOverride(context.Background(), 555, field, 42); err != nil {
			t.Fatalf("clear %s: %v", field, err)
		}
	}
}

func containsCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}

	return false
}
