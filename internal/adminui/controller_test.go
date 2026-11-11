package adminui

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

const (
	testAdminID = int64(7)
	testGroupID = int64(-100200300400)
)

// recordingStore is an in-memory adminconfig.Store that records every write.
type recordingStore struct {
	mu        sync.Mutex
	global    adminconfig.GlobalSettings
	chatRows  map[int64]map[string]string
	prompts   map[promptKey]string
	catalog   []adminconfig.ChatCatalogEntry
	policy    adminconfig.AccessPolicy
	whitelist map[int64]adminconfig.WhitelistEntry
	failNext  map[string]error // first call to the named method fails once
	calls     []string
}

type promptKey struct {
	feature adminconfig.PromptFeature
	chat    int64
}

func newRecordingStore() *recordingStore {
	store := &recordingStore{
		chatRows:  make(map[int64]map[string]string),
		prompts:   make(map[promptKey]string),
		policy:    adminconfig.AccessPolicyAllowlist,
		whitelist: make(map[int64]adminconfig.WhitelistEntry),
		failNext:  make(map[string]error),
	}
	store.global = adminconfig.GlobalSettings{
		CharacterName:            "Ada",
		Language:                 "English",
		Gender:                   "female",
		ToneMode:                 "friendly",
		AllowTeasing:             true,
		DefaultInteractivityMode: adminconfig.InteractivityMentionsOnly,
	}
	store.catalog = []adminconfig.ChatCatalogEntry{
		{ChatID: testGroupID, ChatType: "supergroup", Title: "Group Foo", Username: "groupfoo"},
	}

	return store
}

func (s *recordingStore) record(call string) {
	s.calls = append(s.calls, call)
}

func (s *recordingStore) writeCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.calls...)
}

// failOnce arms a one-shot persistence failure: the first call to the named
// store method returns err, later calls behave normally (a transient SQLite
// outage).
func (s *recordingStore) failOnce(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[method] = err
}

func (s *recordingStore) takeFailure(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err, ok := s.failNext[method]
	if ok {
		delete(s.failNext, method)
	}

	return err
}

func (s *recordingStore) GetGlobalSettings(context.Context) (adminconfig.GlobalSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.global, nil
}

func (s *recordingStore) SetGlobalField(_ context.Context, field, value string, _ int64) error {
	if err := s.takeFailure("SetGlobalField"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("SetGlobalField:" + field + "=" + value)

	switch field {
	case "character_name":
		s.global.CharacterName = value
	case "language":
		s.global.Language = value
	case "gender":
		s.global.Gender = value
	case "tone_mode":
		s.global.ToneMode = value
	case "allow_teasing":
		s.global.AllowTeasing = value == "true"
	case "default_interactivity_mode":
		s.global.DefaultInteractivityMode = adminconfig.InteractivityMode(value)
	}

	return nil
}

func (s *recordingStore) GetChatSettings(_ context.Context, chatID int64) (adminconfig.ChatSettings, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fields, found := s.chatRows[chatID]
	settings := adminconfig.ChatSettings{ChatID: chatID}
	if !found {
		return settings, false, nil
	}

	settings.CharacterName = fields["character_name"]
	settings.Language = fields["language"]
	settings.Gender = fields["gender"]
	settings.ToneMode = fields["tone_mode"]
	if value, ok := fields["allow_teasing"]; ok {
		settings.AllowTeasing = adminconfig.BoolPointer(value == "true")
	}
	if value, ok := fields["interactivity_mode"]; ok && value != "" {
		settings.HasInteractivity = true
		settings.InteractivityMode = adminconfig.InteractivityMode(value)
	}

	return settings, true, nil
}

func (s *recordingStore) SetChatField(_ context.Context, chatID int64, field, value string, _ int64) error {
	if err := s.takeFailure("SetChatField"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("SetChatField:" + strconv.FormatInt(chatID, 10) + ":" + field + "=" + value)

	row := s.chatRows[chatID]
	if row == nil {
		row = make(map[string]string)
		s.chatRows[chatID] = row
	}
	row[field] = value

	return nil
}

func (s *recordingStore) ClearChatField(_ context.Context, chatID int64, field string, _ int64) error {
	if err := s.takeFailure("ClearChatField"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("ClearChatField:" + strconv.FormatInt(chatID, 10) + ":" + field)

	if field == "all" {
		delete(s.chatRows, chatID) // atomic reset: the whole override row goes

		return nil
	}

	if row := s.chatRows[chatID]; row != nil {
		delete(row, field)
		if len(row) == 0 {
			delete(s.chatRows, chatID)
		}
	}

	return nil
}

func (s *recordingStore) GetPromptTemplate(_ context.Context, feature adminconfig.PromptFeature, chatID int64) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	body, ok := s.prompts[promptKey{feature: feature, chat: chatID}]

	return body, ok, nil
}

func (s *recordingStore) SetPromptTemplate(_ context.Context, feature adminconfig.PromptFeature, chatID int64, body string, _ int64) error {
	if err := s.takeFailure("SetPromptTemplate"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("SetPromptTemplate:" + strconv.FormatInt(chatID, 10) + ":" + string(feature))

	s.prompts[promptKey{feature: feature, chat: chatID}] = body

	return nil
}

func (s *recordingStore) ClearPromptTemplate(_ context.Context, feature adminconfig.PromptFeature, chatID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("ClearPromptTemplate:" + strconv.FormatInt(chatID, 10) + ":" + string(feature))

	delete(s.prompts, promptKey{feature: feature, chat: chatID})

	return nil
}

func (s *recordingStore) UpsertChatCatalog(_ context.Context, entry adminconfig.ChatCatalogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("UpsertChatCatalog:" + strconv.FormatInt(entry.ChatID, 10))

	for i, existing := range s.catalog {
		if existing.ChatID == entry.ChatID {
			s.catalog[i] = entry

			return nil
		}
	}
	s.catalog = append(s.catalog, entry)

	return nil
}

func (s *recordingStore) ListChats(context.Context) ([]adminconfig.ChatCatalogEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]adminconfig.ChatCatalogEntry(nil), s.catalog...), nil
}

func (s *recordingStore) GetAccessPolicy(context.Context) (adminconfig.AccessPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.policy, nil
}

func (s *recordingStore) SetAccessPolicy(_ context.Context, policy adminconfig.AccessPolicy, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("SetAccessPolicy:" + string(policy))
	s.policy = policy

	return nil
}

func (s *recordingStore) AddWhitelistChat(_ context.Context, chatID int64, actorID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("AddWhitelistChat:" + strconv.FormatInt(chatID, 10))

	s.whitelist[chatID] = adminconfig.WhitelistEntry{ChatID: chatID, AddedBy: actorID}

	return nil
}

func (s *recordingStore) RemoveWhitelistChat(_ context.Context, chatID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("RemoveWhitelistChat:" + strconv.FormatInt(chatID, 10))
	delete(s.whitelist, chatID)

	return nil
}

func (s *recordingStore) ListWhitelistChats(context.Context) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]int64, 0, len(s.whitelist))
	for id := range s.whitelist {
		ids = append(ids, id)
	}

	return ids, nil
}

func (s *recordingStore) ListWhitelistEntries(context.Context) ([]adminconfig.WhitelistEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries := make([]adminconfig.WhitelistEntry, 0, len(s.whitelist))
	for _, entry := range s.whitelist {
		entries = append(entries, entry)
	}

	return entries, nil
}

func (s *recordingStore) IsChatWhitelisted(_ context.Context, chatID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.whitelist[chatID]

	return ok, nil
}

func newTestController(t *testing.T) (*Controller, *recordingStore) {
	t.Helper()

	store := newRecordingStore()
	builtins := adminconfig.BuiltinTemplates{
		adminconfig.PromptFeatureChat:             "builtin chat {{.Language}}",
		adminconfig.PromptFeatureSummarize:        "builtin summarize {{.Language}}",
		adminconfig.PromptFeatureImageRecognition: "builtin image {{.Language}}",
	}
	validator := func(_ adminconfig.PromptFeature, body string) error {
		if strings.Contains(body, "{{.Bogus}}") {
			return errors.New("template references unknown field .Bogus")
		}
		if _, err := template.New("t").Parse(body); err != nil {
			return err
		}

		return nil
	}
	service := adminconfig.NewService(store, builtins, validator)

	return NewController(NewManager(nil), service, []int64{testAdminID}, func() string {
		return "stats-snapshot"
	}), store
}

// mustOpen opens the dashboard and adopts a message ID the way the adapter
// does after the first send.
func mustOpen(t *testing.T, c *Controller) *Session {
	t.Helper()

	outcome, err := c.OpenDashboard(context.Background(), testAdminID)
	if err != nil {
		t.Fatalf("open dashboard: %v", err)
	}
	if outcome.Render == nil || outcome.Render.Built == nil {
		t.Fatalf("open dashboard must produce a built render")
	}
	if !strings.Contains(outcome.Render.Built.Text, "🛠 Admin") {
		t.Fatalf("dashboard screen unexpected: %q", outcome.Render.Built.Text)
	}

	c.manager.AdoptMsgID(outcome.Session, 100)

	return outcome.Session
}

// tap fires a callback built from the session's current sequence.
func tap(t *testing.T, c *Controller, session *Session, verb string, args ...string) Outcome {
	t.Helper()

	session.mu.Lock()
	seq, msgID := session.Seq, session.MsgID
	session.mu.Unlock()

	outcome, err := c.HandleCallback(context.Background(), testAdminID, msgID, EncodeAction(verb, seq, args...))
	if err != nil {
		t.Fatalf("tap %s: %v", verb, err)
	}

	return outcome
}

// armPending simulates the adapter recording a sent input prompt message.
func armPending(t *testing.T, c *Controller, msgID int) {
	t.Helper()

	session := c.manager.Session(testAdminID)
	if session == nil || session.Pending == nil {
		t.Fatalf("arm pending: no live pending")
	}

	if err := c.manager.SetPendingPromptMsgID(session, session.Pending.ID, msgID); err != nil {
		t.Fatalf("arm pending: %v", err)
	}
}

func (s *recordingStore) assertCalls(t *testing.T, want ...string) {
	t.Helper()

	got := s.writeCalls()
	if len(got) != len(want) {
		t.Fatalf("store calls mismatch:\ngot:  %v\nwant: %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("store call %d mismatch:\ngot:  %q\nwant: %q", i, got[i], want[i])
		}
	}
}

func (s *recordingStore) assertNoCallsAfter(t *testing.T, count int) {
	t.Helper()

	got := s.writeCalls()
	if len(got) != count {
		t.Fatalf("expected no new store writes (had %d), got %v", count, got[count:])
	}
}

// screenButtons flattens all button labels for containment checks.
func screenButtons(screen *Screen) string {
	labels := make([]string, 0)
	for _, row := range screen.Rows {
		for _, button := range row {
			labels = append(labels, button.Label)
		}
	}

	return strings.Join(labels, "\n")
}

func TestOpenDashboardAndNavigation(t *testing.T) {
	c, _ := newTestController(t)
	session := mustOpen(t, c)

	outcome := tap(t, c, session, VerbCatalog, "1")
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenCatalog {
		t.Fatalf("catalog tap must render the catalog: %+v", outcome.Render)
	}
	if !strings.Contains(outcome.Render.Built.Text, "Group Foo") {
		t.Fatalf("catalog must list seeded chat: %q", outcome.Render.Built.Text)
	}
	if outcome.Render.Built.Rows == nil {
		t.Fatalf("catalog must carry buttons")
	}

	outcome = tap(t, c, session, VerbChat, strconv.FormatInt(testGroupID, 10))
	if outcome.Render.Screen.Kind != ScreenChatCard || outcome.Render.Screen.ChatID != testGroupID {
		t.Fatalf("chat tap must open the card: %+v", outcome.Render.Screen)
	}
	if !strings.Contains(outcome.Render.Built.Text, "supergroup") {
		t.Fatalf("card must show chat type: %q", outcome.Render.Built.Text)
	}

	outcome = tap(t, c, session, VerbBack)
	if outcome.Render.Screen.Kind != ScreenCatalog {
		t.Fatalf("back must return to the catalog: %+v", outcome.Render.Screen)
	}

	outcome = tap(t, c, session, VerbHome)
	if outcome.Render.Screen.Kind != ScreenDashboard {
		t.Fatalf("home must return to the dashboard: %+v", outcome.Render.Screen)
	}
}

func TestOnCardInteractivityOverrideAndInherit(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	tap(t, c, session, VerbCatalog, "1")
	tap(t, c, session, VerbChat, strconv.FormatInt(testGroupID, 10))

	// Global default is mentions_only; override on the card itself.
	outcome := tap(t, c, session, VerbValue, "im", "mentions_or_replies")
	store.assertCalls(t, "SetChatField:-100200300400:interactivity_mode=mentions_or_replies")

	session.mu.Lock()
	pending := session.Pending
	session.mu.Unlock()
	if pending != nil {
		t.Fatalf("value tap must not leave a pending input")
	}
	if !strings.Contains(outcome.Render.Built.Text, "(chat override)") {
		t.Fatalf("card must flag the override: %q", outcome.Render.Built.Text)
	}
	if !strings.Contains(screenButtons(outcome.Render.Built), "● Mentions/Replies") {
		t.Fatalf("card must mark the active mode: %q", screenButtons(outcome.Render.Built))
	}

	// Inherit clears the override.
	outcome = tap(t, c, session, VerbClear, "im")
	store.assertCalls(t,
		"SetChatField:-100200300400:interactivity_mode=mentions_or_replies",
		"ClearChatField:-100200300400:interactivity_mode",
	)
	if strings.Contains(outcome.Render.Built.Text, "(chat override)") {
		t.Fatalf("inherit must clear the override flag: %q", outcome.Render.Built.Text)
	}
}

func TestEqualValuedOverrideStillFlagged(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	store.mu.Lock()
	store.chatRows[testGroupID] = map[string]string{"language": "English"} // equals the global value
	store.mu.Unlock()

	tap(t, c, session, VerbCatalog, "1")
	outcome := tap(t, c, session, VerbChat, strconv.FormatInt(testGroupID, 10))

	if !strings.Contains(outcome.Render.Built.Text, "[chat override]") {
		t.Fatalf("equal-valued override must still be flagged: %q", outcome.Render.Built.Text)
	}
}

func TestTriStateBoolEditor(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	tap(t, c, session, VerbCatalog, "1")
	tap(t, c, session, VerbChat, strconv.FormatInt(testGroupID, 10))
	tap(t, c, session, VerbChatSet)

	outcome := tap(t, c, session, VerbField, "at")
	if outcome.Render.Screen.Kind != ScreenFieldEditor || outcome.Render.Screen.Field != "allow_teasing" {
		t.Fatalf("field tap must open the editor: %+v", outcome.Render.Screen)
	}
	if !strings.Contains(screenButtons(outcome.Render.Built), "Inherit") {
		t.Fatalf("tri-state editor must offer inherit: %q", screenButtons(outcome.Render.Built))
	}

	tap(t, c, session, VerbValue, "at", "on")
	store.assertCalls(t, "SetChatField:-100200300400:allow_teasing=true")

	tap(t, c, session, VerbValue, "at", "inh")
	store.assertCalls(t,
		"SetChatField:-100200300400:allow_teasing=true",
		"ClearChatField:-100200300400:allow_teasing",
	)
}

func TestStringInputFlowSaveAndAttemptsCancel(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	tap(t, c, session, VerbGlobalSet)
	tap(t, c, session, VerbField, "lg")

	outcome := tap(t, c, session, VerbString, "lg")
	if outcome.Input == nil || !strings.HasPrefix(outcome.Input.Text, AdminInputMarker) {
		t.Fatalf("string tap must request input: %+v", outcome.Input)
	}

	// Valid reply saves, cleans up, and refreshes with a notice.
	armPending(t, c, 60)
	outcome, err := c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 60, TextAvailable: true, Text: "  Русский  ", ReplyMsgID: 201,
	})
	if err != nil {
		t.Fatalf("handle reply: %v", err)
	}
	store.assertCalls(t, "SetGlobalField:language=Русский")
	if outcome.Render == nil || !strings.Contains(outcome.Render.Built.Text, "Saved.") {
		t.Fatalf("successful input must re-render with a notice: %+v", outcome.Render)
	}
	found := false
	for _, id := range outcome.DeleteMsgIDs {
		if id == 60 || id == 201 {
			found = true
		}
	}
	if !found {
		t.Fatalf("prompt and reply messages must be deleted: %v", outcome.DeleteMsgIDs)
	}

	// Invalid replies retry with a warning, then cancel after three tries.
	tap(t, c, session, VerbField, "tm")
	tap(t, c, session, VerbString, "tm")

	tooLong := strings.Repeat("x", 129) // tone_mode max is 128
	for attempt, msgID := range []int{61, 62} {
		armPending(t, c, msgID)
		outcome, err = c.HandleReply(context.Background(), testAdminID, ReplyInput{
			ReplyToMsgID: msgID, TextAvailable: true, Text: tooLong, ReplyMsgID: 300 + attempt,
		})
		if err != nil {
			t.Fatalf("handle invalid reply %d: %v", attempt+1, err)
		}
		if outcome.Input == nil {
			t.Fatalf("invalid reply %d must trigger a fresh prompt, got %+v", attempt+1, outcome)
		}
		if outcome.Input.PendingID == 0 {
			t.Fatalf("retry prompt must carry the pending identity (its message must correlate)")
		}
		if !strings.Contains(outcome.Input.Text, "⚠️") || !strings.Contains(outcome.Input.Text, "exceeds") {
			t.Fatalf("retry prompt must carry the error: %q", outcome.Input.Text)
		}
	}

	armPending(t, c, 63)
	outcome, err = c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 63, TextAvailable: true, Text: tooLong, ReplyMsgID: 303,
	})
	if err != nil {
		t.Fatalf("handle third invalid reply: %v", err)
	}
	if outcome.Input != nil {
		t.Fatalf("third invalid reply must cancel, not retry: %+v", outcome.Input)
	}
	session.mu.Lock()
	pending := session.Pending
	session.mu.Unlock()
	if pending != nil {
		t.Fatalf("cancelled input must clear the pending")
	}
	if outcome.Render == nil || !strings.Contains(outcome.Render.Built.Text, "cancelled") {
		t.Fatalf("cancelled input must show a notice: %+v", outcome.Render)
	}

	// Only the successful language write happened; no tone_mode write.
	store.assertCalls(t, "SetGlobalField:language=Русский")
}

// TestFailedSettingSaveKeepsPendingCorrelated pins persistence-failure
// recovery: a failed store write must leave the pending input matching its
// ORIGINAL prompt message — no attempt consumed, no cleanup — so the admin
// simply replies to the same prompt again instead of restarting the flow.
func TestFailedSettingSaveKeepsPendingCorrelated(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	tap(t, c, session, VerbGlobalSet)
	tap(t, c, session, VerbField, "lg")
	tap(t, c, session, VerbString, "lg")

	armPending(t, c, 70)
	store.failOnce("SetGlobalField", errors.New("sqlite busy"))
	outcome, err := c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 70, TextAvailable: true, Text: "  Русский  ", ReplyMsgID: 401,
	})
	if err != nil {
		t.Fatalf("handle reply: %v", err)
	}
	if outcome.Message == nil || !strings.Contains(outcome.Message.Text, "Failed:") {
		t.Fatalf("persistence failure must be surfaced: %+v", outcome.Message)
	}
	if outcome.Render != nil || len(outcome.DeleteMsgIDs) > 0 {
		t.Fatalf("a failed save must not re-render or delete anything: %+v", outcome)
	}
	if calls := store.writeCalls(); len(calls) != 0 {
		t.Fatalf("a failed write must not record a store write, got %v", calls)
	}

	session.mu.Lock()
	if pending := session.Pending; pending == nil {
		session.mu.Unlock()
		t.Fatalf("a failed save must keep the pending input alive")
	} else {
		if pending.PromptMsgID != 70 {
			t.Fatalf("failed save must keep the original prompt correlation, got %d", pending.PromptMsgID)
		}
		if pending.Attempts != 0 {
			t.Fatalf("a transient failure must not consume an attempt, got %d", pending.Attempts)
		}
		session.mu.Unlock()
	}

	// The retry: same prompt message, same value — now the write succeeds.
	outcome, err = c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 70, TextAvailable: true, Text: "  Русский  ", ReplyMsgID: 402,
	})
	if err != nil {
		t.Fatalf("retry reply: %v", err)
	}
	store.assertCalls(t, "SetGlobalField:language=Русский")
	if outcome.Render == nil || !strings.Contains(outcome.Render.Built.Text, "Saved.") {
		t.Fatalf("the retried save must succeed with a notice: %+v", outcome.Render)
	}
	session.mu.Lock()
	pending := session.Pending
	session.mu.Unlock()
	if pending != nil {
		t.Fatalf("the successful retry must clear the pending")
	}

	// Chat-scoped writes take the same recovery path through SetChatField.
	tap(t, c, session, VerbCatalog, "1")
	tap(t, c, session, VerbChat, strconv.FormatInt(testGroupID, 10))
	tap(t, c, session, VerbChatSet)
	tap(t, c, session, VerbField, "lg")
	tap(t, c, session, VerbString, "lg")

	armPending(t, c, 75)
	store.failOnce("SetChatField", errors.New("sqlite busy"))
	outcome, err = c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 75, TextAvailable: true, Text: "Deutsch", ReplyMsgID: 403,
	})
	if err != nil {
		t.Fatalf("chat-scoped reply: %v", err)
	}
	if outcome.Message == nil || !strings.Contains(outcome.Message.Text, "Failed:") {
		t.Fatalf("chat-scope persistence failure must be surfaced: %+v", outcome.Message)
	}
	session.mu.Lock()
	if chatPending := session.Pending; chatPending == nil || chatPending.PromptMsgID != 75 {
		session.mu.Unlock()
		t.Fatalf("chat-scoped failure must keep the pending correlated, got %+v", chatPending)
	}
	session.mu.Unlock()

	outcome, err = c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 75, TextAvailable: true, Text: "Deutsch", ReplyMsgID: 404,
	})
	if err != nil {
		t.Fatalf("chat-scoped retry: %v", err)
	}
	store.assertCalls(t,
		"SetGlobalField:language=Русский",
		"SetChatField:-100200300400:language=Deutsch",
	)
}

// TestFailedPromptSaveKeepsBodyVerbatimAndCorrelated covers the same
// persistence-failure recovery for prompt templates, including that the
// retried body is persisted byte-for-byte.
func TestFailedPromptSaveKeepsBodyVerbatimAndCorrelated(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	tap(t, c, session, VerbPrompts)
	tap(t, c, session, VerbPromptFeat, "ch")
	tap(t, c, session, VerbPromptText, "ch")

	body := "\nYou are {{.CharacterName}}.\n\nStay playful.  \n"
	armPending(t, c, 80)
	store.failOnce("SetPromptTemplate", errors.New("disk I/O error"))
	outcome, err := c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 80, TextAvailable: true, Text: body, ReplyMsgID: 410,
	})
	if err != nil {
		t.Fatalf("handle reply: %v", err)
	}
	if outcome.Message == nil || !strings.Contains(outcome.Message.Text, "Failed:") {
		t.Fatalf("persistence failure must be surfaced: %+v", outcome.Message)
	}

	session.mu.Lock()
	pending := session.Pending
	if pending == nil {
		session.mu.Unlock()
		t.Fatalf("a failed save must keep the pending input alive")
	}
	if pending.PromptMsgID != 80 {
		t.Fatalf("failed save must keep the original prompt correlation, got %d", pending.PromptMsgID)
	}
	session.mu.Unlock()

	outcome, err = c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 80, TextAvailable: true, Text: body, ReplyMsgID: 411,
	})
	if err != nil {
		t.Fatalf("retry reply: %v", err)
	}
	store.assertCalls(t, "SetPromptTemplate:0:chat")
	store.mu.Lock()
	saved := store.prompts[promptKey{feature: adminconfig.PromptFeatureChat, chat: 0}]
	store.mu.Unlock()
	if saved != body {
		t.Fatalf("prompt body must be stored verbatim:\ngot:  %q\nwant: %q", saved, body)
	}
}

// TestPromptTextStoredVerbatim pins exact prompt-body preservation: the body
// is stored byte-for-byte — trimming is validation only — and a
// whitespace-only body is still rejected as empty.
func TestPromptTextStoredVerbatim(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	tap(t, c, session, VerbPrompts)
	tap(t, c, session, VerbPromptFeat, "ch")
	tap(t, c, session, VerbPromptText, "ch")

	body := "\n  You are {{.CharacterName}}.\n\nStay playful.  \n"
	armPending(t, c, 85)
	if _, err := c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 85, TextAvailable: true, Text: body, ReplyMsgID: 420,
	}); err != nil {
		t.Fatalf("handle reply: %v", err)
	}
	store.assertCalls(t, "SetPromptTemplate:0:chat")
	store.mu.Lock()
	saved := store.prompts[promptKey{feature: adminconfig.PromptFeatureChat, chat: 0}]
	store.mu.Unlock()
	if saved != body {
		t.Fatalf("prompt body must be stored verbatim:\ngot:  %q\nwant: %q", saved, body)
	}

	// Whitespace-only remains an empty template: retry prompt, no new write.
	tap(t, c, session, VerbPromptText, "ch")
	armPending(t, c, 86)
	outcome, err := c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 86, TextAvailable: true, Text: "   \n\t", ReplyMsgID: 421,
	})
	if err != nil {
		t.Fatalf("handle empty reply: %v", err)
	}
	if outcome.Input == nil || !strings.Contains(outcome.Input.Text, "must not be empty") {
		t.Fatalf("whitespace-only body must be rejected as empty: %+v", outcome.Input)
	}
	store.assertCalls(t, "SetPromptTemplate:0:chat")
}

func TestPromptSourcesDownloadsAndDistinctResets(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	store.mu.Lock()
	store.prompts[promptKey{feature: adminconfig.PromptFeatureChat, chat: 0}] = "stored global chat"
	store.prompts[promptKey{feature: adminconfig.PromptFeatureChat, chat: testGroupID}] = "stored chat override"
	store.mu.Unlock()

	// Global scope: menu → detail shows stored global + builtin download.
	tap(t, c, session, VerbPrompts)
	outcome := tap(t, c, session, VerbPromptFeat, "ch")
	if !strings.Contains(outcome.Render.Built.Text, "Effective source: global") ||
		!strings.Contains(outcome.Render.Built.Text, "Global template stored.") {
		t.Fatalf("global detail must show the stored template: %q", outcome.Render.Built.Text)
	}

	outcome = tap(t, c, session, VerbPromptBL, "ch")
	if outcome.Document == nil || string(outcome.Document.Content) != "builtin chat {{.Language}}" {
		t.Fatalf("builtin download must be verbatim: %+v", outcome.Document)
	}
	if outcome.Render != nil {
		t.Fatalf("download is an observable no-screen action: %+v", outcome.Render)
	}

	// Restore builtin is confirmation-gated and clears the GLOBAL row only.
	outcome = tap(t, c, session, VerbPromptRstG, "ch")
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenConfirm {
		t.Fatalf("restore must render a confirmation: %+v", outcome.Render)
	}
	if strings.Contains(strings.Join(store.writeCalls(), ";"), "ClearPromptTemplate") {
		t.Fatalf("confirmation must run before any write")
	}
	tap(t, c, session, VerbConfirmYes)
	store.assertCalls(t, "ClearPromptTemplate:0:chat")

	// Chat scope: reset-to-global clears the CHAT row.
	session = mustOpen(t, c)
	tap(t, c, session, VerbCatalog, "1")
	tap(t, c, session, VerbChat, strconv.FormatInt(testGroupID, 10))
	tap(t, c, session, VerbPrompts)

	outcome = tap(t, c, session, VerbPromptFeat, "ch")
	if !strings.Contains(outcome.Render.Built.Text, "Chat override stored.") {
		t.Fatalf("chat detail must show the override: %q", outcome.Render.Built.Text)
	}

	tap(t, c, session, VerbPromptRstC, "ch")
	tap(t, c, session, VerbConfirmYes)
	store.assertCalls(t,
		"ClearPromptTemplate:0:chat",
		"ClearPromptTemplate:-100200300400:chat",
	)
}

// TestPromptOutputActionsKeepVisibleKeyboardLive pins the download/view fix:
// pure-output verbs advance neither the screen nor the sequence, so the
// keyboard already rendered keeps working — the press AFTER a download must
// apply, not bounce as stale with a "Menu updated" notice.
func TestPromptOutputActionsKeepVisibleKeyboardLive(t *testing.T) {
	c, _ := newTestController(t)
	session := mustOpen(t, c)

	tap(t, c, session, VerbPrompts)
	tap(t, c, session, VerbPromptFeat, "ch")

	session.mu.Lock()
	seq, msgID := session.Seq, session.MsgID
	session.mu.Unlock()

	// View, download active, download builtin — all with the same sequence,
	// exactly like the keyboard still on screen.
	for _, verb := range []string{VerbPromptView, VerbPromptDL, VerbPromptBL} {
		outcome, err := c.HandleCallback(context.Background(), testAdminID, msgID, EncodeAction(verb, seq, "ch"))
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		if outcome.Document == nil && outcome.Message == nil {
			t.Fatalf("%s must produce output, got %+v", verb, outcome)
		}
	}

	session.mu.Lock()
	after := session.Seq
	session.mu.Unlock()
	if after != seq {
		t.Fatalf("output actions must not advance the sequence: %d → %d", seq, after)
	}

	// The immediately-following press on that same keyboard applies.
	outcome, err := c.HandleCallback(context.Background(), testAdminID, msgID, EncodeAction(VerbBack, seq))
	if err != nil {
		t.Fatalf("back after downloads: %v", err)
	}
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenPrompts {
		t.Fatalf("back must navigate to the prompts menu: %+v", outcome.Render)
	}
	if strings.Contains(outcome.Render.Built.Text, "Menu updated") {
		t.Fatalf("press after download must apply, not refresh as stale: %q", outcome.Render.Built.Text)
	}
}

func TestPromptReplaceViaTextAndUpload(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	tap(t, c, session, VerbPrompts)
	tap(t, c, session, VerbPromptFeat, "su")
	tap(t, c, session, VerbPromptText, "su")

	armPending(t, c, 70)
	outcome, err := c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 70, TextAvailable: true, Text: "new {{.Language}} summarize", ReplyMsgID: 401,
	})
	if err != nil {
		t.Fatalf("prompt text reply: %v", err)
	}
	store.assertCalls(t, "SetPromptTemplate:0:summarize")
	if outcome.Render == nil || !strings.Contains(outcome.Render.Built.Text, "Template saved.") {
		t.Fatalf("saved template must refresh the screen: %+v", outcome.Render)
	}

	// Upload path: wrong extension and invalid template retry; good saves.
	tap(t, c, session, VerbPromptUp, "su")

	armPending(t, c, 71)
	outcome, err = c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 71, TextAvailable: true, ReplyMsgID: 402,
		Document: &DocumentInput{Filename: "template.pdf", Content: []byte("x")},
	})
	if err != nil {
		t.Fatalf("upload reply: %v", err)
	}
	if outcome.Input == nil || !strings.Contains(outcome.Input.Text, ".txt") {
		t.Fatalf("wrong extension must retry with guidance: %+v", outcome.Input)
	}

	armPending(t, c, 72)
	outcome, _ = c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 72, TextAvailable: true, ReplyMsgID: 403,
		Document: &DocumentInput{Filename: "bad.md", Content: []byte("{{.Bogus}}")},
	})
	if outcome.Input == nil {
		t.Fatalf("invalid template must retry, got %+v", outcome)
	}
	if strings.Contains(strings.Join(store.writeCalls(), ";"), "SetPromptTemplate:0:summarize=") {
		t.Fatalf("invalid template must not be stored")
	}

	armPending(t, c, 73)
	outcome, _ = c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 73, TextAvailable: true, ReplyMsgID: 404,
		Document: &DocumentInput{Filename: "good.md", Content: []byte("final {{.Language}}")},
	})
	// The text replace earlier plus this upload are the only two writes.
	store.assertCalls(t, "SetPromptTemplate:0:summarize", "SetPromptTemplate:0:summarize")
	if outcome.Render == nil {
		t.Fatalf("valid upload must commit and refresh")
	}

	// Oversized and non-UTF-8 documents are rejected on actual bytes.
	oversized := make([]byte, PromptBodyLimit+1)
	for i := range oversized {
		oversized[i] = 'a'
	}

	tap(t, c, session, VerbPromptUp, "su")
	armPending(t, c, 74)
	outcome, _ = c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 74, TextAvailable: true, ReplyMsgID: 405,
		Document: &DocumentInput{Filename: "big.txt", Content: oversized},
	})
	if outcome.Input == nil {
		t.Fatalf("oversized document must be rejected")
	}

	tap(t, c, session, VerbPromptUp, "su")
	armPending(t, c, 75)
	outcome, _ = c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 75, TextAvailable: true, ReplyMsgID: 406,
		Document: &DocumentInput{Filename: "bin.txt", Content: []byte{0xff, 0xfe, 0xfd}},
	})
	if outcome.Input == nil {
		t.Fatalf("non-UTF-8 document must be rejected")
	}
}

func TestPolicySwitchOnlyViaConfirmationAndMembershipUntouched(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	store.mu.Lock()
	store.whitelist[testGroupID] = adminconfig.WhitelistEntry{ChatID: testGroupID, AddedBy: 1}
	store.mu.Unlock()

	tap(t, c, session, VerbAccess)

	outcome := tap(t, c, session, VerbAccessPol, "allow_all")
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenConfirm {
		t.Fatalf("policy switch must render a confirmation: %+v", outcome.Render)
	}
	if !strings.Contains(outcome.Render.Built.Text, "allow_all") {
		t.Fatalf("confirmation must state the consequence: %q", outcome.Render.Built.Text)
	}
	store.assertNoCallsAfter(t, 0)

	tap(t, c, session, VerbConfirmYes)
	store.assertCalls(t, "SetAccessPolicy:allow_all")
	// Switching never touches membership.
	if strings.Contains(strings.Join(store.writeCalls(), ";"), "WhitelistChat") {
		t.Fatalf("policy switch must not alter allowlist membership: %v", store.writeCalls())
	}

	// Switching back with an empty allowlist warns about it.
	tap(t, c, session, VerbAccessPol, "allowlist")
	store.mu.Lock()
	store.calls = nil
	delete(store.whitelist, testGroupID)
	store.mu.Unlock()

	// Re-issue on the current (confirm) screen is a stale seq; re-render.
	outcome = tap(t, c, session, VerbAccessPol, "allowlist")
	if outcome.Render == nil || !strings.Contains(outcome.Render.Built.Text, "EMPTY") {
		t.Fatalf("empty-allowlist switch must warn: %q", outcome.Render.Built.Text)
	}
	tap(t, c, session, VerbConfirmYes)
	store.assertCalls(t, "SetAccessPolicy:allowlist")
}

func TestStaleSeqReRenderWithoutMutation(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	session.mu.Lock()
	staleSeq := session.Seq
	session.mu.Unlock()

	tap(t, c, session, VerbCatalog, "1") // bumps the sequence

	outcome, err := c.HandleCallback(context.Background(), testAdminID, 100, EncodeAction(VerbAccess, staleSeq))
	if err != nil {
		t.Fatalf("stale callback: %v", err)
	}
	if outcome.Render == nil || !strings.Contains(outcome.Render.Built.Text, noticeMenuUpdated) {
		t.Fatalf("stale callback must re-render with a notice: %+v", outcome.Render)
	}
	store.assertNoCallsAfter(t, 0)

	session.mu.Lock()
	kind := session.Screen.Kind
	session.mu.Unlock()
	if kind != ScreenCatalog {
		t.Fatalf("stale callback must not navigate: %q", kind)
	}
}

func TestAccessStatusFourStates(t *testing.T) {
	c, store := newTestController(t)

	cases := []struct {
		policy      adminconfig.AccessPolicy
		allowlisted bool
		want        string
	}{
		{adminconfig.AccessPolicyAllowlist, true, "allowlisted"},
		{adminconfig.AccessPolicyAllowlist, false, "blocked (allowlist policy)"},
		{adminconfig.AccessPolicyAllowAll, true, "allowlisted (policy already allows all)"},
		{adminconfig.AccessPolicyAllowAll, false, "allowed via allow_all (not allowlisted)"},
	}
	for _, testCase := range cases {
		store.mu.Lock()
		store.policy = testCase.policy
		store.whitelist = map[int64]adminconfig.WhitelistEntry{}
		if testCase.allowlisted {
			store.whitelist[testGroupID] = adminconfig.WhitelistEntry{ChatID: testGroupID}
		}
		store.mu.Unlock()

		screen, err := c.buildScreen(context.Background(), 5, ScreenState{Kind: ScreenChatCard, ChatID: testGroupID})
		if err != nil {
			t.Fatalf("build card: %v", err)
		}
		if !strings.Contains(screen.Text, "Access: "+testCase.want) {
			t.Fatalf("policy=%s member=%t: want %q, got %q", testCase.policy, testCase.allowlisted, testCase.want, screen.Text)
		}
	}
}

func TestCallbackGuards(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	// Non-admin sender is denied before anything else.
	outcome, err := c.HandleCallback(context.Background(), 8, 100, EncodeAction(VerbHome, session.Seq))
	if err != nil || outcome.Answer != AnswerDenied {
		t.Fatalf("non-admin callback must be denied: %+v err=%v", outcome, err)
	}

	// Malformed data answers "Unknown action" without writes.
	outcome, err = c.HandleCallback(context.Background(), testAdminID, 100, "a:bogus:1")
	if err != nil || outcome.Answer != AnswerUnknown {
		t.Fatalf("malformed callback: %+v err=%v", outcome, err)
	}

	// Well-formed but invalid arguments also answer without writes.
	outcome, err = c.HandleCallback(context.Background(), testAdminID, 100, EncodeAction(VerbChat, session.Seq, "notanumber"))
	if err != nil || outcome.Answer != AnswerUnknown {
		t.Fatalf("invalid argument callback: %+v err=%v", outcome, err)
	}
	store.assertNoCallsAfter(t, 0)

	// A callback for a different message than the active menu is refused.
	session.mu.Lock()
	seq := session.Seq
	session.mu.Unlock()
	outcome, err = c.HandleCallback(context.Background(), testAdminID, 999, EncodeAction(VerbHome, seq))
	if err != nil || outcome.Answer != AnswerMenuGone {
		t.Fatalf("message mismatch callback: %+v err=%v", outcome, err)
	}

	// A forged field access is rejected (access_policy never a settings field).
	outcome, err = c.HandleCallback(context.Background(), testAdminID, 100, EncodeAction(VerbValue, session.Seq, "ap", "on"))
	if err != nil || outcome.Answer != AnswerUnknown {
		t.Fatalf("unknown field code must be rejected: %+v err=%v", outcome, err)
	}
}

func TestChatPickerFlowVerifiesAndCompletes(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	outcome := tap(t, c, session, VerbCatalog, "a")
	if outcome.Picker == nil || outcome.Picker.RequestID == 0 {
		t.Fatalf("add chat must send a picker: %+v", outcome.Picker)
	}
	armPending(t, c, 80) // the picker helper message is correlatable too

	shared := ChatSharedInput{RequestID: outcome.Picker.RequestID, ChatID: -100999, Title: "New Group"}
	result, err := c.HandleChatShared(context.Background(), testAdminID, shared)
	if err != nil {
		t.Fatalf("chat shared: %v", err)
	}
	if result.Verify == nil || result.Verify.ChatID != -100999 {
		t.Fatalf("accepted selection must request verification: %+v", result.Verify)
	}
	if result.DropPicker != MsgPickerReceived {
		t.Fatalf("accepted selection must remove the reply keyboard: %q", result.DropPicker)
	}

	// A duplicate of the same request is consumed with a distinct message and
	// no second keyboard removal.
	duplicate, _ := c.HandleChatShared(context.Background(), testAdminID, shared)
	if duplicate.Message == nil || duplicate.Message.Text != msgChatSharedDone {
		t.Fatalf("duplicate selection must be reported as processed: %+v", duplicate.Message)
	}
	if duplicate.Verify != nil {
		t.Fatalf("duplicate selection must not verify again")
	}
	if duplicate.DropPicker != "" {
		t.Fatalf("duplicate selection must not remove a keyboard again: %q", duplicate.DropPicker)
	}

	// Completion with the wrong chat fails closed without writes.
	wrong, _ := c.CompleteChatAdd(context.Background(), testAdminID, adminconfig.ChatCatalogEntry{ChatID: 12345}, true)
	if wrong.Message == nil || wrong.Message.Text != msgChatSharedInactive {
		t.Fatalf("wrong completion must fail closed: %+v", wrong.Message)
	}
	store.assertNoCallsAfter(t, 0)

	done, err := c.CompleteChatAdd(context.Background(), testAdminID, adminconfig.ChatCatalogEntry{
		ChatID: -100999, ChatType: "supergroup", Title: "New Group",
	}, true)
	if err != nil {
		t.Fatalf("complete chat add: %v", err)
	}
	store.assertCalls(t, "UpsertChatCatalog:-100999", "AddWhitelistChat:-100999")
	if done.Render == nil || done.Render.Screen.Kind != ScreenChatCard {
		t.Fatalf("completion must land on the chat card: %+v", done.Render)
	}
	if !strings.Contains(done.Render.Built.Text, "Added to allowlist.") {
		t.Fatalf("completion must show the notice: %q", done.Render.Built.Text)
	}
}

func TestManualChatIDUnverifiedAdd(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	tap(t, c, session, VerbCatalog, "a")
	armPending(t, c, 90)

	outcome, err := c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 90, TextAvailable: true, Text: "-100888", ReplyMsgID: 500,
	})
	if err != nil {
		t.Fatalf("manual id reply: %v", err)
	}
	if outcome.Verify == nil || outcome.Verify.ChatID != -100888 {
		t.Fatalf("manual id must request verification: %+v", outcome.Verify)
	}
	if outcome.DropPicker != MsgPickerClosed {
		t.Fatalf("manual chat-ID completion must remove the picker keyboard: %q", outcome.DropPicker)
	}

	done, _ := c.CompleteChatAdd(context.Background(), testAdminID, adminconfig.ChatCatalogEntry{
		ChatID: -100888, ChatType: "unknown",
	}, false)
	store.assertCalls(t, "UpsertChatCatalog:-100888", "AddWhitelistChat:-100888")
	if !strings.Contains(done.Render.Built.Text, "unverified") {
		t.Fatalf("unverified add must be labeled: %q", done.Render.Built.Text)
	}
}

// TestPickerKeyboardLifecycleTransitions pins every transition that must
// remove the picker reply keyboard: session replacement (/admin), /cancel,
// abandonment by navigation or another input, and picker supersession (which
// must NOT remove — the fresh keyboard send replaces it in place).
func TestPickerKeyboardLifecycleTransitions(t *testing.T) {
	armPicker := func(t *testing.T, c *Controller) {
		t.Helper()

		session := c.manager.Session(testAdminID)
		outcome := tap(t, c, session, VerbCatalog, "a")
		if outcome.Picker == nil {
			t.Fatalf("add chat must arm a picker")
		}
		armPending(t, c, 80)
	}

	t.Run("replaced by /admin", func(t *testing.T) {
		c, _ := newTestController(t)
		mustOpen(t, c)
		armPicker(t, c)

		reopen, err := c.OpenDashboard(context.Background(), testAdminID)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if reopen.DropPicker != MsgPickerClosed {
			t.Fatalf("/admin must remove the replaced session's picker keyboard: %q", reopen.DropPicker)
		}

		// A reopen with no live picker must not send a removal.
		clean, err := c.OpenDashboard(context.Background(), testAdminID)
		if err != nil {
			t.Fatalf("clean reopen: %v", err)
		}
		if clean.DropPicker != "" {
			t.Fatalf("reopen without a picker must keep the keyboard alone: %q", clean.DropPicker)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		c, _ := newTestController(t)
		mustOpen(t, c)
		armPicker(t, c)

		cancel, err := c.Cancel(context.Background(), testAdminID)
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if cancel.DropPicker != "Cancelled." {
			t.Fatalf("cancel must remove the picker keyboard, ack = %q", cancel.DropPicker)
		}
		if cancel.Message != nil {
			t.Fatalf("the removal acknowledgement doubles as the cancel line, got an extra message: %+v", cancel.Message)
		}
	})

	t.Run("abandoned by navigation", func(t *testing.T) {
		c, _ := newTestController(t)
		session := mustOpen(t, c)
		tap(t, c, session, VerbCatalog, "1") // the Add chat button lives on the catalog
		armPicker(t, c)

		abandon := tap(t, c, c.manager.Session(testAdminID), VerbBack)
		if abandon.DropPicker != MsgPickerClosed {
			t.Fatalf("navigation away must remove the picker keyboard: %q", abandon.DropPicker)
		}

		// The forward direction too: opening a chat card is a screen push and
		// drops the pending the same way.
		armPicker(t, c)
		card := tap(t, c, c.manager.Session(testAdminID), VerbChat, "-100200300400")
		if card.DropPicker != MsgPickerClosed {
			t.Fatalf("pushing another screen must remove the picker keyboard: %q", card.DropPicker)
		}
	})

	t.Run("abandoned by another input", func(t *testing.T) {
		c, _ := newTestController(t)
		session := mustOpen(t, c)
		tap(t, c, session, VerbCatalog, "1")
		armPicker(t, c)

		search := tap(t, c, c.manager.Session(testAdminID), VerbCatalog, "s")
		if search.Input == nil {
			t.Fatalf("search must arm its input")
		}
		if search.DropPicker != MsgPickerClosed {
			t.Fatalf("starting another input must remove the picker keyboard: %q", search.DropPicker)
		}
	})

	t.Run("superseded by a newer picker", func(t *testing.T) {
		c, _ := newTestController(t)
		session := mustOpen(t, c)
		tap(t, c, session, VerbCatalog, "1")
		armPicker(t, c)

		fresh := tap(t, c, c.manager.Session(testAdminID), VerbCatalog, "a")
		if fresh.Picker == nil {
			t.Fatalf("second add chat must arm a picker")
		}
		if fresh.DropPicker != "" {
			t.Fatalf("a new picker keyboard supersedes the old one in place; must not send a removal: %q", fresh.DropPicker)
		}
	})
}

// TestStaleChatSharedKeepsNewPickerKeyboard pins the dismissal guard: a chat
// selection with a mismatched RequestID is consumed with a notice, but the
// live picker's keyboard survives — and the matching selection still
// completes afterwards.
func TestStaleChatSharedKeepsNewPickerKeyboard(t *testing.T) {
	c, _ := newTestController(t)
	session := mustOpen(t, c)

	outcome := tap(t, c, session, VerbCatalog, "a")
	liveRequestID := outcome.Picker.RequestID
	armPending(t, c, 82)

	stale, err := c.HandleChatShared(context.Background(), testAdminID, ChatSharedInput{
		RequestID: liveRequestID + 1, ChatID: -100777, Title: "Stale",
	})
	if err != nil {
		t.Fatalf("stale selection: %v", err)
	}
	if stale.Message == nil || stale.Message.Text != msgChatSharedInactive {
		t.Fatalf("stale selection must be consumed with a notice: %+v", stale.Message)
	}
	if stale.DropPicker != "" {
		t.Fatalf("a stale selection must not dismiss the live picker's keyboard: %q", stale.DropPicker)
	}
	if !c.manager.PickerKeyboardLive(testAdminID) {
		t.Fatalf("the live picker must remain keyboard-armed after a stale selection")
	}

	accepted, err := c.HandleChatShared(context.Background(), testAdminID, ChatSharedInput{
		RequestID: liveRequestID, ChatID: -100999, Title: "New Group",
	})
	if err != nil {
		t.Fatalf("live selection: %v", err)
	}
	if accepted.Verify == nil || accepted.DropPicker != MsgPickerReceived {
		t.Fatalf("the matching selection must still complete: verify=%+v drop=%q", accepted.Verify, accepted.DropPicker)
	}
}

// TestChatSharedExpiredPendingBranch pins the pending-expiry branch of
// HandleChatShared: an expired picker pending consumes the selection with the
// keyboard-removal acknowledgement, while an expired non-picker pending gets
// the plain inactive notice without any keyboard removal.
func TestChatSharedExpiredPendingBranch(t *testing.T) {
	t.Run("expired picker removes the keyboard", func(t *testing.T) {
		c, _ := newTestController(t)
		session := mustOpen(t, c)
		tap(t, c, session, VerbCatalog, "a")
		armPending(t, c, 80)
		session = c.manager.Session(testAdminID)
		session.Pending.ExpiresAt = time.Now().Add(-time.Second)

		expired, err := c.HandleChatShared(context.Background(), testAdminID, ChatSharedInput{
			RequestID: 12345, ChatID: -100555, Title: "Late",
		})
		if err != nil {
			t.Fatalf("expired selection: %v", err)
		}
		if expired.DropPicker != msgChatSharedInactive {
			t.Fatalf("expired picker must fold the notice into the keyboard removal: %q", expired.DropPicker)
		}
		if expired.Message != nil {
			t.Fatalf("the notice must be the removal ack, not a separate message: %+v", expired.Message)
		}
		if expired.Verify != nil {
			t.Fatalf("an expired pending must not start verification: %+v", expired.Verify)
		}
		if c.manager.PickerKeyboardLive(testAdminID) {
			t.Fatalf("the expired pending must be gone after the selection")
		}
	})

	t.Run("expired non-picker pending sends no removal", func(t *testing.T) {
		c, _ := newTestController(t)
		session := mustOpen(t, c)
		tap(t, c, session, VerbCatalog, "s") // arms a search input, no picker keyboard
		session = c.manager.Session(testAdminID)
		session.Pending.ExpiresAt = time.Now().Add(-time.Second)

		expired, err := c.HandleChatShared(context.Background(), testAdminID, ChatSharedInput{
			RequestID: 12345, ChatID: -100555, Title: "Late",
		})
		if err != nil {
			t.Fatalf("expired selection: %v", err)
		}
		if expired.DropPicker != "" {
			t.Fatalf("no picker keyboard exists, must not send a removal: %q", expired.DropPicker)
		}
		if expired.Message == nil || expired.Message.Text != msgChatSharedInactive {
			t.Fatalf("expired pending must be consumed with a notice: %+v", expired.Message)
		}
	})
}

// TestResetAllOverridesIsAtomic pins the confirm execution for
// chat_overrides_reset: exactly one "all" store operation, and the whole
// override row disappears.
func TestResetAllOverridesIsAtomic(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	// Two overrides seeded directly in the store (the recording store persists
	// them as the chat's override row).
	if err := store.SetChatField(context.Background(), testGroupID, "character_name", "fox", 1); err != nil {
		t.Fatalf("seed override: %v", err)
	}
	if err := store.SetChatField(context.Background(), testGroupID, "language", "Deutsch", 1); err != nil {
		t.Fatalf("seed override: %v", err)
	}
	tap(t, c, session, VerbChat, "-100200300400")
	store.calls = nil

	outcome := tap(t, c, session, VerbResetChat)
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenConfirm {
		t.Fatalf("reset must confirm first: %+v", outcome.Render)
	}

	done := tap(t, c, c.manager.Session(testAdminID), VerbConfirmYes)
	if !strings.Contains(done.Render.Built.Text, "All overrides cleared") {
		t.Fatalf("reset must confirm success: %q", done.Render.Built.Text)
	}
	store.assertCalls(t, "ClearChatField:-100200300400:all")

	if _, found, err := store.GetChatSettings(context.Background(), testGroupID); err != nil || found {
		t.Fatalf("reset-all must delete the override row: found=%v err=%v", found, err)
	}
}

// TestResetAllOverridesFailureLeavesChatIntact verifies a transient store
// failure cannot partially reset the chat: the atomic operation fails as a
// whole, the failure is surfaced, and both overrides survive.
func TestResetAllOverridesFailureLeavesChatIntact(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	for _, seed := range []struct{ field, value string }{
		{"character_name", "fox"}, {"language", "Deutsch"},
	} {
		if err := store.SetChatField(context.Background(), testGroupID, seed.field, seed.value, 1); err != nil {
			t.Fatalf("seed override %s: %v", seed.field, err)
		}
	}

	tap(t, c, session, VerbChat, "-100200300400")
	tap(t, c, session, VerbResetChat)
	store.failOnce("ClearChatField", context.DeadlineExceeded)

	done := tap(t, c, c.manager.Session(testAdminID), VerbConfirmYes)
	if !strings.Contains(done.Render.Built.Text, "Failed:") {
		t.Fatalf("the failed reset must be surfaced: %q", done.Render.Built.Text)
	}

	row, found, err := store.GetChatSettings(context.Background(), testGroupID)
	if err != nil || !found {
		t.Fatalf("a failed reset must leave the overrides readable: found=%v err=%v", found, err)
	}
	if row.CharacterName != "fox" || row.Language != "Deutsch" {
		t.Fatalf("a failed reset must not touch any override: %+v", row)
	}
}

func TestSearchFlow(t *testing.T) {
	c, _ := newTestController(t)
	session := mustOpen(t, c)

	tap(t, c, session, VerbCatalog, "1")

	outcome := tap(t, c, session, VerbCatalog, "s")
	if outcome.Input == nil || !strings.HasPrefix(outcome.Input.Text, AdminInputMarker) {
		t.Fatalf("search must request input: %+v", outcome.Input)
	}

	armPending(t, c, 95)
	outcome, err := c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 95, TextAvailable: true, Text: "group foo", ReplyMsgID: 600,
	})
	if err != nil {
		t.Fatalf("search reply: %v", err)
	}
	session.mu.Lock()
	state := session.Screen
	session.mu.Unlock()
	if state.Kind != ScreenCatalog || state.Search != "group foo" || state.Page != 1 {
		t.Fatalf("search reply must update the catalog state: %+v", state)
	}
	if !strings.Contains(outcome.Render.Built.Text, "search") {
		t.Fatalf("catalog must show the active search: %q", outcome.Render.Built.Text)
	}
}

func TestCancelCancelsPendingOrReopensDashboard(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	tap(t, c, session, VerbGlobalSet)
	tap(t, c, session, VerbField, "lg")
	tap(t, c, session, VerbString, "lg")
	armPending(t, c, 96)

	outcome, err := c.Cancel(context.Background(), testAdminID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	session.mu.Lock()
	pending, confirm := session.Pending, session.Confirm
	session.mu.Unlock()
	if pending != nil || confirm != nil {
		t.Fatalf("cancel must clear pendings and confirms")
	}
	found := false
	for _, id := range outcome.DeleteMsgIDs {
		if id == 96 {
			found = true
		}
	}
	if !found {
		t.Fatalf("cancel must delete the input prompt: %v", outcome.DeleteMsgIDs)
	}
	if outcome.Render == nil || !strings.Contains(outcome.Render.Built.Text, "Cancelled.") {
		t.Fatalf("cancel must refresh the screen: %+v", outcome.Render)
	}

	// With nothing pending, /cancel reopens the dashboard (fresh session).
	before := store.writeCalls()
	outcome, err = c.Cancel(context.Background(), testAdminID)
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenDashboard {
		t.Fatalf("cancel with nothing pending must reopen the dashboard: %+v", outcome.Render)
	}
	store.assertNoCallsAfter(t, len(before))
}

func TestReplyClassificationFailsClosed(t *testing.T) {
	c, store := newTestController(t)
	mustOpen(t, c)

	// No session state matches: marker text present → inactive notice.
	outcome, err := c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 123, TextAvailable: true, Text: "hello", ReplyMsgID: 700,
	})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if outcome.Message == nil || outcome.Message.Text != msgInputInactive {
		t.Fatalf("stale marker reply must fail closed: %+v", outcome.Message)
	}

	// Unclassifiable (no text) reply gets the distinct unmatched notice.
	outcome, _ = c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID: 123, TextAvailable: false, ReplyMsgID: 701,
	})
	if outcome.Message == nil || outcome.Message.Text != msgUnmatchedReply {
		t.Fatalf("unclassifiable reply must fail closed: %+v", outcome.Message)
	}
	store.assertNoCallsAfter(t, 0)
}

func TestNonAdminInputsAreIgnored(t *testing.T) {
	c, store := newTestController(t)
	mustOpen(t, c)

	outcome, err := c.HandleReply(context.Background(), 8, ReplyInput{
		ReplyToMsgID: 1, TextAvailable: true, Text: "x", ReplyMsgID: 2,
	})
	if err != nil {
		t.Fatalf("non-admin reply: %v", err)
	}
	if outcome.Message != nil || outcome.Render != nil {
		t.Fatalf("non-admin reply must be ignored: %+v", outcome)
	}
	store.assertNoCallsAfter(t, 0)
}

// TestOpenDashboardConcurrentWithCommits pins the locking discipline of
// OpenDashboard under the race detector: /admin publishes a fresh session
// that is immediately visible to concurrent commits (stale button presses
// landing as notices), so every field the dashboard render reads must come
// from a locked snapshot.
func TestOpenDashboardConcurrentWithCommits(t *testing.T) {
	c, _ := newTestController(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			outcome, err := c.OpenDashboard(context.Background(), testAdminID)
			if err != nil {
				t.Errorf("open dashboard: %v", err)

				return
			}
			if outcome.Render == nil || outcome.Render.Built == nil {
				t.Errorf("dashboard render missing")

				return
			}
		}
	}()

	for i := 0; i < 50; i++ {
		// Stale/random sequence callbacks commit notice re-renders on the
		// live session while OpenDashboard reads it.
		if _, err := c.HandleCallback(context.Background(), testAdminID, 100, EncodeAction(VerbHome, uint32(i+1))); err != nil {
			t.Errorf("callback: %v", err)
		}
	}
	<-done
}
