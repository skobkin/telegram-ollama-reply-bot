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

	mu    sync.Mutex
	calls []string
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
	s.calls = append(s.calls, "ClearChatField:"+field)
	s.mu.Unlock()

	return s.stubStore.ClearChatField(context.Background(), chatID, field, actorID)
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

	if err := service.SetGlobalSetting(context.Background(), "allow_teasing", " True ", 1); err != nil {
		t.Fatalf("valid boolean: %v", err)
	}
	if err := service.SetGlobalSetting(context.Background(), "language", "  Русский ", 1); err != nil {
		t.Fatalf("valid string: %v", err)
	}
	if err := service.SetGlobalSetting(context.Background(), "default_interactivity_mode", "mentions_only", 1); err != nil {
		t.Fatalf("valid enum: %v", err)
	}

	want := []string{
		"SetGlobalField:allow_teasing=true",
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
		{"default_interactivity_mode", "proactive"}, // unsupported enum
		{"language", "   "},                         // empty after trim
		{"tone_mode", strings.Repeat("x", 129)},     // over MaxLen
		{"access_policy", "allow_all"},              // not a settings field
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

	if err := service.SetChatSetting(context.Background(), -1, "allow_teasing", "false", 1); err != nil {
		t.Fatalf("valid tri-state value: %v", err)
	}
	if err := service.SetChatSetting(context.Background(), -1, "allow_teasing", "maybe", 1); err == nil {
		t.Fatalf("invalid boolean must be rejected")
	}
	if err := service.ClearChatOverride(context.Background(), -1, "interactivity_mode", 1); err != nil {
		t.Fatalf("clear override: %v", err)
	}
	if err := service.ClearChatOverride(context.Background(), -1, "access_policy", 1); err == nil {
		t.Fatalf("access_policy must not be clearable as an override")
	}

	want := []string{"SetChatField:allow_teasing=false", "ClearChatField:interactivity_mode"}
	got := store.writeCalls()
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("calls mismatch:\ngot:  %v\nwant: %v", got, want)
	}
}

func TestClearAllChatOverridesClearsEveryChatField(t *testing.T) {
	service, store := newSettingsTestService(t)

	if err := service.ClearAllChatOverrides(context.Background(), -100, 1); err != nil {
		t.Fatalf("clear all: %v", err)
	}

	got := store.writeCalls()
	if len(got) != len(ChatFieldSpecs()) {
		t.Fatalf("expected %d clears, got %v", len(ChatFieldSpecs()), got)
	}
	cleared := make(map[string]bool)
	for _, call := range got {
		cleared[strings.TrimPrefix(call, "ClearChatField:")] = true
	}
	for _, spec := range ChatFieldSpecs() {
		if !cleared[spec.Name] {
			t.Fatalf("field %s was not cleared", spec.Name)
		}
	}
}
