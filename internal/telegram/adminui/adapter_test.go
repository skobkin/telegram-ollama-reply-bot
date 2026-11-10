package adminui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tg "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"

	"telegram-ollama-reply-bot/internal/adminconfig"
	core "telegram-ollama-reply-bot/internal/adminui"
)

const (
	testBotID = int64(999)
	testAdmin = int64(7)
)

// A syntactically valid bot token (telego validates the <id>:<35 chars>
// shape); all requests hit the fake server anyway.
var testToken = "1:" + strings.Repeat("A", 35)

// testClock gives tests deterministic control over session and pending TTLs.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// memStore is a behavioral in-memory adminconfig.Store: writes are readable
// back, and write calls are recorded for isolation assertions.
type memStore struct {
	mu        sync.Mutex
	global    adminconfig.GlobalSettings
	chatRows  map[int64]map[string]string
	prompts   map[string]string
	catalog   []adminconfig.ChatCatalogEntry
	policy    adminconfig.AccessPolicy
	whitelist map[int64]bool
	failNext  map[string]error // first call to the named method fails once
	calls     []string

	// failPolicy makes GetAccessPolicy fail, to exercise error feedback.
	failPolicy bool
}

func newMemStore() *memStore {
	return &memStore{
		chatRows:  make(map[int64]map[string]string),
		prompts:   make(map[string]string),
		policy:    adminconfig.AccessPolicyAllowlist,
		whitelist: make(map[int64]bool),
		failNext:  make(map[string]error),
	}
}

// failOnce arms a one-shot persistence failure: the first call to the named
// store method returns err, later calls behave normally (a transient SQLite
// outage).
func (s *memStore) failOnce(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[method] = err
}

func (s *memStore) takeFailure(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err, ok := s.failNext[method]
	if ok {
		delete(s.failNext, method)
	}

	return err
}

func (s *memStore) record(call string) {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
}

func (s *memStore) writeCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.calls...)
}

func (s *memStore) GetGlobalSettings(context.Context) (adminconfig.GlobalSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.global, nil
}

func (s *memStore) SetGlobalField(_ context.Context, field, value string, _ int64) error {
	if err := s.takeFailure("SetGlobalField"); err != nil {
		return err
	}
	s.record("SetGlobalField:" + field + "=" + value)
	s.mu.Lock()
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
	s.mu.Unlock()

	return nil
}

func (s *memStore) GetChatSettings(_ context.Context, chatID int64) (adminconfig.ChatSettings, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row, ok := s.chatRows[chatID]
	if !ok {
		return adminconfig.ChatSettings{}, false, nil
	}

	settings := adminconfig.ChatSettings{ChatID: chatID}
	settings.CharacterName = row["character_name"]
	settings.Language = row["language"]
	settings.Gender = row["gender"]
	settings.ToneMode = row["tone_mode"]
	if value, ok := row["allow_teasing"]; ok {
		teasing := value == "true"
		settings.AllowTeasing = &teasing
	}
	if value, ok := row["interactivity_mode"]; ok {
		settings.InteractivityMode = adminconfig.InteractivityMode(value)
		settings.HasInteractivity = true
	}

	return settings, true, nil
}

func (s *memStore) SetChatField(_ context.Context, chatID int64, field, value string, _ int64) error {
	s.record("SetChatField:" + field + "=" + value)
	s.mu.Lock()
	if s.chatRows[chatID] == nil {
		s.chatRows[chatID] = make(map[string]string)
	}
	s.chatRows[chatID][field] = value
	s.mu.Unlock()

	return nil
}

func (s *memStore) ClearChatField(_ context.Context, chatID int64, field string, _ int64) error {
	s.record("ClearChatField:" + field)
	s.mu.Lock()
	delete(s.chatRows[chatID], field)
	s.mu.Unlock()

	return nil
}

func promptKey(feature adminconfig.PromptFeature, chatID int64) string {
	return string(feature) + ":" + strconv.FormatInt(chatID, 10)
}

func (s *memStore) GetPromptTemplate(_ context.Context, feature adminconfig.PromptFeature, chatID int64) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	body, ok := s.prompts[promptKey(feature, chatID)]

	return body, ok, nil
}

func (s *memStore) SetPromptTemplate(_ context.Context, feature adminconfig.PromptFeature, chatID int64, body string, _ int64) error {
	s.record("SetPromptTemplate:" + promptKey(feature, chatID))
	s.mu.Lock()
	s.prompts[promptKey(feature, chatID)] = body
	s.mu.Unlock()

	return nil
}

func (s *memStore) ClearPromptTemplate(_ context.Context, feature adminconfig.PromptFeature, chatID int64) error {
	s.record("ClearPromptTemplate:" + promptKey(feature, chatID))
	s.mu.Lock()
	delete(s.prompts, promptKey(feature, chatID))
	s.mu.Unlock()

	return nil
}

func (s *memStore) UpsertChatCatalog(_ context.Context, entry adminconfig.ChatCatalogEntry) error {
	s.record("UpsertChatCatalog:" + strconv.FormatInt(entry.ChatID, 10))
	s.mu.Lock()
	for i, existing := range s.catalog {
		if existing.ChatID == entry.ChatID {
			s.catalog[i] = entry
			s.mu.Unlock()

			return nil
		}
	}
	s.catalog = append(s.catalog, entry)
	s.mu.Unlock()

	return nil
}

func (s *memStore) ListChats(context.Context) ([]adminconfig.ChatCatalogEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]adminconfig.ChatCatalogEntry(nil), s.catalog...), nil
}

func (s *memStore) GetAccessPolicy(context.Context) (adminconfig.AccessPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failPolicy {
		return "", errors.New("policy lookup failed")
	}

	return s.policy, nil
}

func (s *memStore) SetAccessPolicy(_ context.Context, policy adminconfig.AccessPolicy, _ int64) error {
	s.record("SetAccessPolicy:" + string(policy))
	s.mu.Lock()
	s.policy = policy
	s.mu.Unlock()

	return nil
}

func (s *memStore) AddWhitelistChat(_ context.Context, chatID int64, _ int64) error {
	s.record("AddWhitelistChat:" + strconv.FormatInt(chatID, 10))
	s.mu.Lock()
	s.whitelist[chatID] = true
	s.mu.Unlock()

	return nil
}

func (s *memStore) RemoveWhitelistChat(_ context.Context, chatID int64) error {
	s.record("RemoveWhitelistChat:" + strconv.FormatInt(chatID, 10))
	s.mu.Lock()
	delete(s.whitelist, chatID)
	s.mu.Unlock()

	return nil
}

func (s *memStore) ListWhitelistChats(context.Context) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]int64, 0, len(s.whitelist))
	for id := range s.whitelist {
		ids = append(ids, id)
	}

	return ids, nil
}

func (s *memStore) ListWhitelistEntries(context.Context) ([]adminconfig.WhitelistEntry, error) {
	ids, _ := s.ListWhitelistChats(context.Background())
	entries := make([]adminconfig.WhitelistEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, adminconfig.WhitelistEntry{ChatID: id})
	}

	return entries, nil
}

func (s *memStore) IsChatWhitelisted(_ context.Context, chatID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.whitelist[chatID], nil
}

// fakeAdminAPI is a Bot API test double: it records every call (parsing
// multipart bodies), hands out real message IDs, answers getChat/getFile, and
// serves bounded file downloads from /file/bot<token>/.
type fakeAdminAPI struct {
	server *httptest.Server

	mu             sync.Mutex
	requests       []fakeRequest
	nextMessageID  atomic.Int64
	files          map[string][]byte
	chats          map[int64]*tg.ChatFullInfo
	failMethods    map[string]error
	failChatScopes bool // setMyCommands fails only for per-chat scopes

	// One-shot request gate: the first request of the armed method parks
	// inside serve until released — an in-flight Telegram API call.
	gateMethod  string
	gateEntered chan struct{}
	gateRelease chan struct{}
}

type fakeRequest struct {
	Method string
	Body   string
}

func newFakeAdminAPI(t *testing.T) *fakeAdminAPI {
	t.Helper()

	fake := &fakeAdminAPI{
		files:       make(map[string][]byte),
		chats:       make(map[int64]*tg.ChatFullInfo),
		failMethods: make(map[string]error),
	}
	fake.nextMessageID.Store(100)
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)

	return fake
}

func (f *fakeAdminAPI) serve(w http.ResponseWriter, r *http.Request) {
	if prefix := "/file/bot" + testToken + "/"; strings.HasPrefix(r.URL.Path, prefix) {
		path := strings.TrimPrefix(r.URL.Path, prefix)
		f.mu.Lock()
		content, ok := f.files[path]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}
		_, _ = w.Write(content)

		return
	}

	method := strings.TrimPrefix(r.URL.Path, "/bot"+testToken+"/")

	f.parkIfGated(method) // no-op unless a test armed the gate

	// Multipart bodies (sendDocument): capture filename and content.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		document := readMultipartDocument(r)
		f.mu.Lock()
		f.requests = append(f.requests, fakeRequest{
			Method: method,
			Body:   "document:" + document.filename + "\n" + document.content,
		})
		f.mu.Unlock()
		writeJSON(w, map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": f.nextMessageID.Add(1),
				"chat":       map[string]any{"id": testAdmin, "type": "private"},
			},
		})

		return
	}

	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{Method: method, Body: string(body)})
	failure := f.failMethods[method]
	chatScopeFailure := f.failChatScopes && strings.Contains(string(body), `"type":"chat"`)
	f.mu.Unlock()

	if failure != nil || chatScopeFailure {
		description := "forced failure"
		if failure != nil {
			description = failure.Error()
		}
		writeJSON(w, map[string]any{"ok": false, "description": description})

		return
	}

	switch method {
	case "sendMessage", "editMessageText":
		writeJSON(w, map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": f.nextMessageID.Add(1),
				"chat":       map[string]any{"id": testAdmin, "type": "private"},
			},
		})
	case "getChat":
		f.mu.Lock()
		info := f.chats[chatIDFromBody(body)]
		f.mu.Unlock()
		if info == nil {
			writeJSON(w, map[string]any{"ok": false, "description": "chat not found"})

			return
		}
		writeJSON(w, map[string]any{"ok": true, "result": info})
	case "getFile":
		// file_size 0 on purpose: the download bound must never trust it.
		writeJSON(w, map[string]any{
			"ok": true,
			"result": map[string]any{
				"file_id":   "file-1",
				"file_size": 0,
				"file_path": "uploads/prompt.md",
			},
		})
	case "answerCallbackQuery", "deleteMessage", "setMyCommands":
		writeJSON(w, map[string]any{"ok": true, "result": true})
	default:
		writeJSON(w, map[string]any{"ok": true, "result": map[string]any{}})
	}
}

func writeJSON(w http.ResponseWriter, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func chatIDFromBody(body []byte) int64 {
	var params struct {
		ChatID int64 `json:"chat_id"`
	}
	_ = json.Unmarshal(body, &params)

	return params.ChatID
}

type capturedDocument struct {
	filename string
	content  string
}

func readMultipartDocument(request *http.Request) capturedDocument {
	contentType := request.Header.Get("Content-Type")
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return capturedDocument{}
	}

	part, err := multipart.NewReader(request.Body, params["boundary"]).NextPart()
	if err != nil {
		return capturedDocument{}
	}
	defer part.Close() //nolint:errcheck // read-side cleanup

	content, _ := io.ReadAll(part)

	return capturedDocument{filename: part.FileName(), content: string(content)}
}

// armGate arms the one-shot request gate for a method. The FIRST request of
// that method parks inside the fake server; entered is signaled when it
// parks, and closing release lets it complete. Later requests flow normally.
func (f *fakeAdminAPI) armGate(method string) (entered, release chan struct{}) {
	entered = make(chan struct{}, 1)
	release = make(chan struct{})

	f.mu.Lock()
	f.gateMethod = method
	f.gateEntered = entered
	f.gateRelease = release
	f.mu.Unlock()

	return entered, release
}

// parkIfGated blocks the calling request until the test releases the gate —
// an in-flight Telegram API call. No-op for unmatched or unarmed methods.
func (f *fakeAdminAPI) parkIfGated(method string) {
	f.mu.Lock()
	entered, release := f.gateEntered, f.gateRelease
	match := f.gateMethod == method
	if match {
		f.gateMethod, f.gateEntered, f.gateRelease = "", nil, nil // one-shot
	}
	f.mu.Unlock()

	if !match {
		return
	}

	entered <- struct{}{}
	<-release
}

func (f *fakeAdminAPI) requestsOf(method string) []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	matched := make([]fakeRequest, 0)
	for _, request := range f.requests {
		if request.Method == method {
			matched = append(matched, request)
		}
	}

	return matched
}

func (f *fakeAdminAPI) callCount(method string) int {
	return len(f.requestsOf(method))
}

// lastBodyJSON returns the last captured body of a method parsed as JSON.
func (f *fakeAdminAPI) lastBodyJSON(method string) map[string]any {
	requests := f.requestsOf(method)
	if len(requests) == 0 {
		return nil
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(requests[len(requests)-1].Body), &payload); err != nil {
		return nil
	}

	return payload
}

func (f *fakeAdminAPI) lastSentText(method string) string {
	payload := f.lastBodyJSON(method)
	if payload == nil {
		return ""
	}
	text, _ := payload["text"].(string)

	return text
}

func (f *fakeAdminAPI) lastSendMessageID() int64 {
	return f.nextMessageID.Load()
}

func (f *fakeAdminAPI) chatScopedMenuSyncs() []fakeRequest {
	matched := make([]fakeRequest, 0)
	for _, request := range f.requestsOf("setMyCommands") {
		if strings.Contains(request.Body, `"type":"chat"`) {
			matched = append(matched, request)
		}
	}

	return matched
}

// harness wires the real controller + manager + adapter against the fake
// API. logs captures the adapter's structured log output for leak
// assertions.
type harness struct {
	adapter    *Adapter
	manager    *core.Manager
	controller *core.Controller
	store      *memStore
	api        *fakeAdminAPI
	bh         *th.BotHandler
	botAPI     *tg.Bot
	clock      *testClock
	logs       *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	return newHarnessWithAdmins(t, testAdmin)
}

func newHarnessWithAdmins(t *testing.T, adminIDs ...int64) *harness {
	t.Helper()

	store := newMemStore()
	builtins := adminconfig.BuiltinTemplates{
		adminconfig.PromptFeatureChat:             "builtin chat {{.Language}}",
		adminconfig.PromptFeatureSummarize:        "builtin summarize {{.Language}}",
		adminconfig.PromptFeatureImageRecognition: "builtin image {{.Language}}",
	}
	validator := func(_ adminconfig.PromptFeature, body string) error {
		if strings.Contains(body, "{{.Bogus}}") {
			return errors.New("template references unknown field .Bogus")
		}

		return nil
	}
	service := adminconfig.NewService(store, builtins, validator)

	clock := &testClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	manager := core.NewManager(clock.Now)
	controller := core.NewController(manager, service, adminIDs, func() string { return "stats-snapshot" })

	api := newFakeAdminAPI(t)
	botAPI, err := tg.NewBot(testToken, tg.WithAPIServer(api.server.URL), tg.WithDiscardLogger())
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	logs := &bytes.Buffer{}
	adapter := NewAdapter(controller, manager, adminIDs, slog.New(slog.NewTextHandler(logs, nil)))

	bh, err := th.NewBotHandler(botAPI, make(chan tg.Update))
	if err != nil {
		t.Fatalf("NewBotHandler: %v", err)
	}
	t.Cleanup(func() { _ = bh.Stop() })

	adapter.RegisterRoutes(bh, botAPI, testBotID, anyCommand)

	return &harness{
		adapter:    adapter,
		manager:    manager,
		controller: controller,
		store:      store,
		api:        api,
		bh:         bh,
		botAPI:     botAPI,
		clock:      clock,
		logs:       logs,
	}
}

func anyCommand(_ context.Context, _ tg.Update) bool { return true }

func (h *harness) deliver(t *testing.T, update tg.Update) {
	t.Helper()

	if err := h.bh.BaseGroup().HandleUpdate(context.Background(), h.botAPI, update); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
}

// open sends /admin and returns the rendered menu message ID.
func (h *harness) open(t *testing.T) int64 {
	t.Helper()

	h.deliver(t, privateUpdate(testAdmin, "/admin"))

	return h.api.lastSendMessageID()
}

// tap presses a button with the session's current sequence number.
func (h *harness) tap(t *testing.T, verb string, args ...string) {
	t.Helper()

	session := h.manager.Session(testAdmin)
	if session == nil {
		t.Fatalf("no session to tap")
	}
	_, _, msgID, _ := session.DetachedSnapshot()

	h.deliver(t, callbackUpdate(testAdmin, msgID, core.EncodeAction(verb, session.Seq, args...)))
}

// Update builders.

func privateUpdate(fromID int64, text string) tg.Update {
	return tg.Update{UpdateID: 42, Message: &tg.Message{
		MessageID: 50,
		Chat:      tg.Chat{ID: testAdmin, Type: tg.ChatTypePrivate},
		From:      &tg.User{ID: fromID},
		Text:      text,
	}}
}

func callbackUpdate(fromID int64, msgID int, data string) tg.Update {
	return tg.Update{UpdateID: 43, CallbackQuery: &tg.CallbackQuery{
		ID:   "cb-1",
		From: tg.User{ID: fromID},
		Message: &tg.Message{
			MessageID: msgID,
			Chat:      tg.Chat{ID: fromID, Type: tg.ChatTypePrivate},
		},
		Data: data,
	}}
}

func replyToBotUpdate(text string, replyTo tg.Message) tg.Update {
	return tg.Update{UpdateID: 44, Message: &tg.Message{
		MessageID:      51,
		Chat:           tg.Chat{ID: testAdmin, Type: tg.ChatTypePrivate},
		From:           &tg.User{ID: testAdmin},
		Text:           text,
		ReplyToMessage: &replyTo,
	}}
}

func documentReplyToBotUpdate(replyTo tg.Message) tg.Update {
	return tg.Update{UpdateID: 46, Message: &tg.Message{
		MessageID:      53,
		Chat:           tg.Chat{ID: testAdmin, Type: tg.ChatTypePrivate},
		From:           &tg.User{ID: testAdmin},
		ReplyToMessage: &replyTo,
		Document:       &tg.Document{FileID: "file-1", FileName: "prompt.md"},
	}}
}

func botPromptMessage(msgID int, text string) tg.Message {
	return tg.Message{
		MessageID: msgID,
		Chat:      tg.Chat{ID: testAdmin, Type: tg.ChatTypePrivate},
		From:      &tg.User{ID: testBotID, IsBot: true},
		Text:      text,
	}
}

// ---- tests ----

func TestAdminCommandRendersDashboardWithKeyboard(t *testing.T) {
	h := newHarness(t)

	menuMsgID := h.open(t)

	if got := h.api.callCount("sendMessage"); got != 1 {
		t.Fatalf("expected one sendMessage, got %d", got)
	}
	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "Admin") {
		t.Fatalf("dashboard text missing admin header: %q", text)
	}

	payload := h.api.lastBodyJSON("sendMessage")
	markup, _ := payload["reply_markup"].(map[string]any)
	keyboard, _ := markup["inline_keyboard"].([]any)
	if len(keyboard) == 0 {
		t.Fatalf("dashboard must render an inline keyboard, got %v", payload["reply_markup"])
	}

	if menuMsgID == 0 {
		t.Fatalf("menu message ID must be recorded")
	}
}

func TestCallbackEditsMenuAndAnswersExactlyOnce(t *testing.T) {
	h := newHarness(t)
	h.open(t)

	h.tap(t, core.VerbCatalog, "1")

	if got := h.api.callCount("answerCallbackQuery"); got != 1 {
		t.Fatalf("callback must be answered exactly once, got %d", got)
	}
	answer := h.api.lastBodyJSON("answerCallbackQuery")
	if text, _ := answer["text"].(string); text != "" {
		t.Fatalf("successful callback answer must be silent, got %q", text)
	}

	if got := h.api.callCount("editMessageText"); got != 1 {
		t.Fatalf("navigation must edit the menu message, got %d edits", got)
	}
	edit := h.api.lastBodyJSON("editMessageText")
	if id, _ := edit["message_id"].(float64); int64(id) != h.api.lastSendMessageID()-1 {
		t.Fatalf("edit must target the menu message: %v", edit["message_id"])
	}
	if text := lastJSONText(edit); !strings.Contains(text, "Chats") {
		t.Fatalf("catalog screen expected, got %q", text)
	}
}

func lastJSONText(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	text, _ := payload["text"].(string)

	return text
}

func TestIdenticalRerenderSkipsEditAndSeqBump(t *testing.T) {
	h := newHarness(t)
	h.open(t)

	session := h.manager.Session(testAdmin)
	seq := session.Seq

	h.tap(t, core.VerbHome)

	if got := h.api.callCount("editMessageText"); got != 0 {
		t.Fatalf("byte-identical re-render must not edit, got %d", got)
	}
	if got := h.api.callCount("sendMessage"); got != 1 {
		t.Fatalf("byte-identical re-render must not send, got %d", got)
	}
	if h.manager.Session(testAdmin).Seq != seq {
		t.Fatalf("byte-identical re-render must not bump the sequence")
	}
}

func TestCallbackAcknowledgementMatrix(t *testing.T) {
	t.Run("malformed data", func(t *testing.T) {
		h := newHarness(t)
		h.open(t)

		h.deliver(t, callbackUpdate(testAdmin, 100, "a:zzz:1"))

		if got := h.api.callCount("answerCallbackQuery"); got != 1 {
			t.Fatalf("exactly one answer, got %d", got)
		}
		if text := h.api.lastSentText("answerCallbackQuery"); text != core.AnswerUnknown {
			t.Fatalf("malformed callback answer = %q", text)
		}
		if got := h.api.callCount("editMessageText"); got != 0 {
			t.Fatalf("malformed callback must not edit, got %d", got)
		}
		if calls := h.store.writeCalls(); len(calls) != 0 {
			t.Fatalf("malformed callback must not write, got %v", calls)
		}
	})

	t.Run("non-admin sender", func(t *testing.T) {
		h := newHarness(t)
		h.open(t)

		h.deliver(t, callbackUpdate(42, 100, core.EncodeAction(core.VerbHome, 1)))

		if got := h.api.callCount("answerCallbackQuery"); got != 1 {
			t.Fatalf("exactly one answer, got %d", got)
		}
		if text := h.api.lastSentText("answerCallbackQuery"); text != core.AnswerDenied {
			t.Fatalf("non-admin answer = %q", text)
		}
		if got := h.api.callCount("editMessageText"); got != 0 {
			t.Fatalf("non-admin callback must not edit, got %d", got)
		}
	})

	t.Run("dead session", func(t *testing.T) {
		h := newHarness(t) // no /admin ever

		h.deliver(t, callbackUpdate(testAdmin, 100, core.EncodeAction(core.VerbHome, 1)))

		if got := h.api.callCount("answerCallbackQuery"); got != 1 {
			t.Fatalf("exactly one answer, got %d", got)
		}
		if text := h.api.lastSentText("answerCallbackQuery"); text != core.AnswerMenuGone {
			t.Fatalf("dead session answer = %q", text)
		}
	})

	t.Run("stale sequence", func(t *testing.T) {
		h := newHarness(t)
		h.open(t)

		session := h.manager.Session(testAdmin)
		_, _, msgID, _ := session.DetachedSnapshot()
		staleSeq := session.Seq

		h.tap(t, core.VerbCatalog, "1") // bumps the sequence

		h.deliver(t, callbackUpdate(testAdmin, msgID, core.EncodeAction(core.VerbCatalog, staleSeq, "1")))

		if got := h.api.callCount("answerCallbackQuery"); got != 2 {
			t.Fatalf("each callback answered exactly once, got %d", got)
		}
		if text := h.api.lastSentText("answerCallbackQuery"); text != "" {
			t.Fatalf("stale callback answer must be silent, got %q", text)
		}
		edits := h.api.requestsOf("editMessageText")
		if len(edits) != 2 {
			t.Fatalf("stale callback must re-render the current screen, got %d edits", len(edits))
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(edits[len(edits)-1].Body), &payload); err != nil {
			t.Fatalf("unmarshal edit body: %v", err)
		}
		if text := lastJSONText(payload); !strings.Contains(text, "Menu updated") {
			t.Fatalf("stale callback must show the Menu updated notice, got %q", text)
		}
	})
}

func TestCancelReopensDashboard(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbCatalog, "1")

	h.deliver(t, privateUpdate(testAdmin, "/cancel"))

	if text := h.api.lastSentText("editMessageText"); !strings.Contains(text, "Admin") {
		t.Fatalf("cancel must reopen the dashboard, got %q", text)
	}
}

func TestStringInputFlowEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbGlobalSet)
	h.tap(t, core.VerbField, "lg")
	h.tap(t, core.VerbString, "lg")

	prompts := h.api.requestsOf("sendMessage")
	if len(prompts) != 2 { // dashboard + input prompt
		t.Fatalf("input prompt expected, got %d sends", len(prompts))
	}
	var promptBody map[string]any
	if err := json.Unmarshal([]byte(prompts[1].Body), &promptBody); err != nil {
		t.Fatalf("unmarshal prompt body: %v", err)
	}
	if text := lastJSONText(promptBody); !strings.HasPrefix(text, core.AdminInputMarker) {
		t.Fatalf("input prompt must carry the marker, got %q", text)
	}
	markup, _ := promptBody["reply_markup"].(map[string]any)
	if force, _ := markup["force_reply"].(bool); !force {
		t.Fatalf("input prompt must use ForceReply, got %v", promptBody["reply_markup"])
	}

	prompt := botPromptMessage(int(h.api.lastSendMessageID()), lastJSONText(promptBody))

	if pending := h.manager.Session(testAdmin).Pending; pending == nil || pending.PromptMsgID == 0 {
		t.Fatalf("pending input must be correlated before the reply can match")
	}

	h.deliver(t, replyToBotUpdate("Русский", prompt))

	calls := h.store.writeCalls()
	found := false
	for _, call := range calls {
		if strings.Contains(call, "SetGlobalField:language=Русский") {
			found = true
		}
	}
	if !found {
		t.Fatalf("language write missing, calls: %v", calls)
	}
	if got := h.api.callCount("deleteMessage"); got != 2 {
		t.Fatalf("prompt and reply must be deleted, got %d", got)
	}
	if text := h.api.lastSentText("editMessageText"); !strings.Contains(text, "Saved") {
		t.Fatalf("screen must confirm the save, got %q", text)
	}
}

func TestRetryPromptCorrelatesEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbGlobalSet)
	h.tap(t, core.VerbField, "tm")
	h.tap(t, core.VerbString, "tm")

	// First reply is invalid (tone_mode max is 128 bytes): the controller
	// keeps the pending and sends a retry prompt for it.
	first := botPromptMessage(int(h.api.lastSendMessageID()), core.AdminInputMarker+" — tone mode")
	h.deliver(t, replyToBotUpdate(strings.Repeat("x", 129), first))

	prompts := h.api.requestsOf("sendMessage")
	if len(prompts) != 3 { // dashboard + first prompt + retry prompt
		t.Fatalf("retry prompt expected, got %d sends", len(prompts))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(prompts[2].Body), &body); err != nil {
		t.Fatalf("unmarshal retry body: %v", err)
	}
	retry := botPromptMessage(int(h.api.lastSendMessageID()), lastJSONText(body))
	if !strings.HasPrefix(retry.Text, core.AdminInputMarker) {
		t.Fatalf("retry prompt must carry the marker, got %q", retry.Text)
	}

	// A valid reply to the RETRY message must apply: before the correlation
	// fix the retry carried no pending identity, never stamped its message,
	// and this reply was rejected as no-longer-active.
	h.deliver(t, replyToBotUpdate("playful", retry))

	found := false
	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "SetGlobalField:tone_mode=playful") {
			found = true
		}
	}
	if !found {
		t.Fatalf("reply to the retry prompt must save the value, calls: %v", h.store.writeCalls())
	}
}

func TestDownloadKeepsKeyboardLiveEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbPrompts)
	h.tap(t, core.VerbPromptFeat, "ch")

	session := h.manager.Session(testAdmin)
	_, _, msgID, _ := session.DetachedSnapshot()
	seq := session.Seq // the sequence the visible keyboard encodes

	h.deliver(t, callbackUpdate(testAdmin, msgID, core.EncodeAction(core.VerbPromptBL, seq, "ch")))
	if got := h.api.callCount("sendDocument"); got != 1 {
		t.Fatalf("builtin download must send a document, got %d", got)
	}

	// The keyboard still on screen was built with `seq`: the next press must
	// APPLY, not bounce as stale with a "Menu updated" notice.
	h.deliver(t, callbackUpdate(testAdmin, msgID, core.EncodeAction(core.VerbBack, seq)))

	edits := h.api.requestsOf("editMessageText")
	var payload map[string]any
	if err := json.Unmarshal([]byte(edits[len(edits)-1].Body), &payload); err != nil {
		t.Fatalf("unmarshal edit body: %v", err)
	}
	if text := lastJSONText(payload); strings.Contains(text, "Menu updated") {
		t.Fatalf("press after download must apply, got %q", text)
	}
}

// TestInFlightResendNeverAdoptsOntoReplacement pins the menu-adoption race:
// the callback's edit finds the menu gone and resends; /admin replaces the
// session while the resend is in flight. The resent message belongs to the
// detached session — the replacement must not adopt it.
func TestInFlightResendNeverAdoptsOntoReplacement(t *testing.T) {
	h := newHarness(t)
	h.open(t) // live menu, first session

	h.api.mu.Lock()
	h.api.failMethods["editMessageText"] = errors.New("message to edit not found")
	h.api.mu.Unlock()
	entered, release := h.api.armGate("sendMessage")

	// A Fatalf while a request is parked must not hang server.Close in
	// cleanup — always release the gate on the way out, exactly once.
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseGate)

	session := h.manager.Session(testAdmin)
	_, _, msgID, _ := session.DetachedSnapshot()
	data := core.EncodeAction(core.VerbCatalog, session.Seq, "1")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := h.bh.BaseGroup().HandleUpdate(context.Background(), h.botAPI, callbackUpdate(testAdmin, msgID, data)); err != nil {
			t.Errorf("handle update: %v", err)
		}
	}()
	<-entered // the resend is in flight

	// /admin replaces the session while the Telegram request is parked (its
	// own dashboard render is deliberately not applied here).
	if _, err := h.controller.OpenDashboard(context.Background(), testAdmin); err != nil {
		t.Fatalf("open dashboard: %v", err)
	}

	releaseGate()
	wg.Wait()

	fresh := h.manager.Session(testAdmin)
	_, _, freshMsgID, _ := fresh.DetachedSnapshot()
	if freshMsgID != 0 {
		t.Fatalf("replacement must not adopt the detached session's resent menu, got %d", freshMsgID)
	}

	// The admin is not stranded: a fresh /admin sends and adopts its own menu.
	h.deliver(t, privateUpdate(testAdmin, "/admin"))
	fresh = h.manager.Session(testAdmin)
	_, _, freshMsgID, _ = fresh.DetachedSnapshot()
	if freshMsgID == 0 {
		t.Fatalf("fresh /admin must adopt its own menu message")
	}
	if got := h.api.callCount("answerCallbackQuery"); got != 1 {
		t.Fatalf("the parked callback must still be answered exactly once, got %d", got)
	}
}

// TestDelayedPromptSendNeverStampsReplacementPending pins the cross-session
// prompt-correlation race: both sessions number their first pending input 1,
// so a ForceReply send from a replaced session collides with the replacement
// session's own pending. The late send must be dropped.
func TestDelayedPromptSendNeverStampsReplacementPending(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbGlobalSet)
	h.tap(t, core.VerbField, "lg")

	// The string-edit prompt send parks inside the fake API.
	entered, release := h.api.armGate("sendMessage")

	// Same anti-hang guard as the resend test: release exactly once even on
	// a Fatalf path, so server.Close in cleanup never blocks on a parked
	// request.
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseGate)

	session := h.manager.Session(testAdmin)
	_, _, msgID, _ := session.DetachedSnapshot()
	data := core.EncodeAction(core.VerbString, session.Seq, "lg")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := h.bh.BaseGroup().HandleUpdate(context.Background(), h.botAPI, callbackUpdate(testAdmin, msgID, data)); err != nil {
			t.Errorf("handle update: %v", err)
		}
	}()
	<-entered // the old session's prompt send is in flight

	// Safe unlocked read: the pending committed before the send parked, and
	// the parked goroutine touches no session state until released.
	old := h.manager.Session(testAdmin)
	oldPendingID := old.Pending.ID

	// /admin replaces the session; the replacement arms its own string
	// pending (controller-side — its prompt is not sent) while the old send
	// is still parked.
	if _, err := h.controller.OpenDashboard(context.Background(), testAdmin); err != nil {
		t.Fatalf("open dashboard: %v", err)
	}
	freshSeq, _, _, _ := h.manager.Session(testAdmin).Snapshot()
	freshOutcome, err := h.controller.HandleCallback(context.Background(), testAdmin, 0, core.EncodeAction(core.VerbString, freshSeq, "lg"))
	if err != nil {
		t.Fatalf("arm replacement pending: %v", err)
	}
	if freshOutcome.Input == nil {
		t.Fatalf("replacement string tap must arm a pending input")
	}
	if freshOutcome.Input.PendingID != oldPendingID {
		t.Fatalf("test premise: pending IDs must collide across sessions, got %d and %d",
			oldPendingID, freshOutcome.Input.PendingID)
	}

	releaseGate()
	wg.Wait()

	// Safe unlocked reads: wg.Wait above ordered the goroutine's last touches.
	fresh := h.manager.Session(testAdmin)
	stamped := fresh.Pending.PromptMsgID
	freshID := fresh.Pending.ID
	if stamped != 0 {
		t.Fatalf("replacement pending must stay unstamped by the old session's late send, got %d", stamped)
	}

	// The replacement's own prompt still correlates normally.
	if err := h.manager.SetPendingPromptMsgID(fresh, freshID, 900); err != nil {
		t.Fatalf("own send must correlate: %v", err)
	}
}

func TestExpiredInputReplyConsumedNeverFallsThrough(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbGlobalSet)
	h.tap(t, core.VerbField, "lg")
	h.tap(t, core.VerbString, "lg")

	prompt := botPromptMessage(int(h.api.lastSendMessageID()), core.AdminInputMarker+" — Language")

	h.clock.Advance(core.PendingTTL + time.Minute)

	h.deliver(t, replyToBotUpdate("Русский", prompt))

	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "no longer active") {
		t.Fatalf("expired input must be consumed with a notice, got %q", text)
	}
	if calls := h.store.writeCalls(); len(calls) != 0 {
		t.Fatalf("expired input must not write, got %v", calls)
	}
}

func TestReplyWithoutMarkerResumesChain(t *testing.T) {
	h := newHarness(t)

	ordinary := botPromptMessage(77, "ordinary bot answer")
	h.deliver(t, replyToBotUpdate("thanks!", ordinary))

	// No admin session exists and the reply is ordinary: no admin IO at all.
	if got := h.api.callCount("sendMessage"); got != 0 {
		t.Fatalf("ordinary reply must not trigger admin IO, got %d sends", got)
	}
}

func TestUnclassifiableReplyFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.open(t)

	mediaMessage := botPromptMessage(78, "") // bot media message: no text
	h.deliver(t, replyToBotUpdate("nice pic", mediaMessage))

	if got := h.api.callCount("sendMessage"); got != 2 { // dashboard + notice
		t.Fatalf("fail-closed notice expected, got %d sends", got)
	}
	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "could not be matched") {
		t.Fatalf("fail-closed notice expected, got %q", text)
	}
	if calls := h.store.writeCalls(); len(calls) != 0 {
		t.Fatalf("fail-closed reply must not write, got %v", calls)
	}
}

func TestDocumentUploadEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbPrompts)
	h.tap(t, core.VerbPromptFeat, "ch")
	h.tap(t, core.VerbPromptUp, "ch")

	h.api.files["uploads/prompt.md"] = []byte("custom chat {{.Language}}")

	prompt := botPromptMessage(int(h.api.lastSendMessageID()), core.AdminInputMarker+" — upload")
	h.deliver(t, documentReplyToBotUpdate(prompt))

	calls := h.store.writeCalls()
	found := false
	for _, call := range calls {
		if strings.Contains(call, "SetPromptTemplate:chat:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("prompt upload write missing, calls: %v", calls)
	}
}

func TestDocumentUploadOversizedRejected(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbPrompts)
	h.tap(t, core.VerbPromptFeat, "ch")
	h.tap(t, core.VerbPromptUp, "ch")

	// The download bound is 32 KiB + 1: a full 32769-byte read is oversized,
	// regardless of reported FileSize (the fake always reports 0).
	h.api.files["uploads/prompt.md"] = make([]byte, core.PromptBodyLimit+1)

	prompt := botPromptMessage(int(h.api.lastSendMessageID()), core.AdminInputMarker+" — upload")
	h.deliver(t, documentReplyToBotUpdate(prompt))

	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "SetPromptTemplate") {
			t.Fatalf("oversized upload must never be persisted: %v", call)
		}
	}
	if got := h.api.callCount("sendMessage"); got != 3 { // dashboard + prompt + retry feedback
		t.Fatalf("oversized upload must answer with feedback, got %d sends", got)
	}
}

func TestBootstrapDiagnosticOnEmptyAdminIDs(t *testing.T) {
	h := newHarnessWithAdmins(t) // no admins at all

	h.deliver(t, privateUpdate(5, "/admin"))

	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "Admin controls are disabled") ||
		!strings.Contains(text, "5") {
		t.Fatalf("bootstrap diagnostic expected, got %q", text)
	}
}

func TestNonAdminCommandDenied(t *testing.T) {
	h := newHarnessWithAdmins(t, testAdmin, 8)

	h.deliver(t, privateUpdate(42, "/cancel"))

	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "only to administrators") {
		t.Fatalf("non-admin denial expected, got %q", text)
	}
}

func TestCommandMenusSync(t *testing.T) {
	h := newHarnessWithAdmins(t, testAdmin, 8)

	h.adapter.SyncCommandMenus(context.Background())

	if got := h.api.callCount("setMyCommands"); got != 4 { // default + groups + two admins
		t.Fatalf("expected 4 menu syncs, got %d", got)
	}

	scopes := make([]string, 0)
	for _, request := range h.api.requestsOf("setMyCommands") {
		var body map[string]any
		if err := json.Unmarshal([]byte(request.Body), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		scope, _ := body["scope"].(map[string]any)
		kind, _ := scope["type"].(string)
		scopes = append(scopes, kind)
	}
	if got, want := strings.Join(scopes, ","), "default,all_group_chats,chat,chat"; got != want {
		t.Fatalf("scopes = %q, want %q", got, want)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(h.api.requestsOf("setMyCommands")[0].Body), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	commands, _ := body["commands"].([]any)
	if len(commands) != 3 { // start, help, summarize in the default scope
		t.Fatalf("default scope must list user commands only, got %v", body["commands"])
	}
}

func TestAdminMenuFailureRetriedOnAdminOpen(t *testing.T) {
	h := newHarness(t)

	h.api.mu.Lock()
	h.api.failChatScopes = true
	h.api.mu.Unlock()
	h.adapter.SyncCommandMenus(context.Background())
	if got := len(h.api.chatScopedMenuSyncs()); got != 1 {
		t.Fatalf("one failed chat-scoped sync expected, got %d", got)
	}

	h.api.mu.Lock()
	h.api.failChatScopes = false
	h.api.mu.Unlock()

	h.open(t)

	if got := len(h.api.chatScopedMenuSyncs()); got != 2 {
		t.Fatalf("the failed admin menu must be retried on /admin, got %d", got)
	}
}

// TestReopenEditsLiveMenuWithoutDuplicate pins adopt-on-edit: after a second
// /admin edits the live menu message, the replacement session must record
// that message ID so the next button press edits again instead of sending a
// duplicate menu.
func TestReopenEditsLiveMenuWithoutDuplicate(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbCatalog, "1")

	sendsBefore := h.api.callCount("sendMessage")
	editsBefore := h.api.callCount("editMessageText")

	h.deliver(t, privateUpdate(testAdmin, "/admin"))
	if got := h.api.callCount("sendMessage"); got != sendsBefore {
		t.Fatalf("reopen must edit the live menu, sent %d extra messages", got-sendsBefore)
	}

	h.tap(t, core.VerbCatalog, "1")
	if got := h.api.callCount("sendMessage"); got != sendsBefore {
		t.Fatalf("post-reopen tap must edit, got %d extra sends", got-sendsBefore)
	}
	if got := h.api.callCount("editMessageText"); got != editsBefore+2 {
		t.Fatalf("expected two edits (reopen + tap), got %d", got-editsBefore)
	}
}

// TestUppercaseCancelStillCancels pins case-insensitive command matching
// (th.CommandEqual matches case-insensitively; the handler must too): an
// uppercase /CANCEL with a live pending deletes the prompt and cancels.
func TestUppercaseCancelStillCancels(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbGlobalSet)
	h.tap(t, core.VerbField, "lg")
	h.tap(t, core.VerbString, "lg")

	h.deliver(t, privateUpdate(testAdmin, "/CANCEL"))

	if got := h.api.callCount("deleteMessage"); got != 1 {
		t.Fatalf("/CANCEL must cancel (delete the pending prompt), got %d deletes", got)
	}
	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "Cancelled") {
		t.Fatalf("cancellation feedback expected, got %q", text)
	}
}

// TestAdminCommandFailureSendsFeedback pins the error path: a failing
// OpenDashboard answers the admin instead of silently doing nothing.
func TestAdminCommandFailureSendsFeedback(t *testing.T) {
	h := newHarness(t)
	h.store.mu.Lock()
	h.store.failPolicy = true
	h.store.mu.Unlock()

	h.deliver(t, privateUpdate(testAdmin, "/admin"))

	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "Failed:") {
		t.Fatalf("failure feedback expected, got %q", text)
	}
}

// TestAccessScreenPaginatesLargeAllowlist pins the bounded access screen:
// with more entries than one page the screen paginates instead of exceeding
// Telegram's message and keyboard limits.
func TestAccessScreenPaginatesLargeAllowlist(t *testing.T) {
	h := newHarness(t)
	h.store.mu.Lock()
	for i := int64(1); i <= 10; i++ {
		h.store.whitelist[-1000000000-i] = true
	}
	h.store.mu.Unlock()

	h.open(t)
	h.tap(t, core.VerbAccess)

	text := h.api.lastSentText("editMessageText")
	if !strings.Contains(text, "page 1 of 2") {
		t.Fatalf("first page expected, got %q", text)
	}
	if !strings.Contains(text, "Allowlist (10") {
		t.Fatalf("total count expected, got %q", text)
	}

	h.tap(t, core.VerbAccess, "2")
	if text = h.api.lastSentText("editMessageText"); !strings.Contains(text, "page 2 of 2") {
		t.Fatalf("second page expected, got %q", text)
	}
}

// TestDocumentDownloadFailuresNeverLeakToken pins the sanitizer: metadata
// and transport failures log typed errors only — the raw transport error
// embeds the token-bearing file URL and must never reach the logs.
func TestDocumentDownloadFailuresNeverLeakToken(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbPrompts)
	h.tap(t, core.VerbPromptFeat, "ch")
	h.tap(t, core.VerbPromptUp, "ch")

	prompt := botPromptMessage(int(h.api.lastSendMessageID()), core.AdminInputMarker+" — upload")

	h.api.mu.Lock()
	h.api.failMethods["getFile"] = errors.New("http do request: connection refused")
	h.api.mu.Unlock()
	h.deliver(t, documentReplyToBotUpdate(prompt))

	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "Failed to download the document") {
		t.Fatalf("download failure feedback expected, got %q", text)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "telegram file metadata request failed") {
		t.Fatalf("the typed sentinel must be logged, got %q", logs)
	}
	if strings.Contains(logs, testToken) || strings.Contains(logs, "/file/bot") {
		t.Fatalf("the bot token or file URL must never appear in logs: %q", logs)
	}
	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "SetPromptTemplate") {
			t.Fatalf("a failed download must not persist, got %v", call)
		}
	}
}

// TestEmptyPromptUploadRejected pins the central empty-body rejection: a
// zero-byte upload (or whitespace-only text) can no longer silently wipe the
// active template with a success notice.
func TestEmptyPromptUploadRejected(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbPrompts)
	h.tap(t, core.VerbPromptFeat, "ch")
	h.tap(t, core.VerbPromptUp, "ch")

	h.api.files["uploads/prompt.md"] = []byte("   ")

	prompt := botPromptMessage(int(h.api.lastSendMessageID()), core.AdminInputMarker+" — upload")
	h.deliver(t, documentReplyToBotUpdate(prompt))

	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "SetPromptTemplate") {
			t.Fatalf("empty prompt bodies must never be persisted, got %v", call)
		}
	}
	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "must not be empty") {
		t.Fatalf("actionable rejection expected, got %q", text)
	}
}

// TestAdminAPITransportErrorsNeverLeakToken pins the log sanitizer across the
// adapter's Telegram IO: raw transport errors carry the token-bearing Bot
// API URL, and only the typed sentinel may reach the logs.
func TestAdminAPITransportErrorsNeverLeakToken(t *testing.T) {
	tokenErr := func(method string) error {
		return errors.New(`Post "https://api.telegram.org/bot` + testToken + `/` + method + `": dial tcp 1.2.3.4:443: i/o timeout`)
	}
	assertSanitized := func(t *testing.T, h *harness, sentinel string) {
		t.Helper()
		logs := h.logs.String()
		if !strings.Contains(logs, sentinel) {
			t.Fatalf("the typed sentinel must be logged, got %q", logs)
		}
		if strings.Contains(logs, testToken) || strings.Contains(logs, "api.telegram.org") {
			t.Fatalf("the token-bearing transport error must never appear in logs: %q", logs)
		}
	}

	t.Run("screen update", func(t *testing.T) {
		h := newHarness(t)
		h.open(t)
		h.api.mu.Lock()
		h.api.failMethods["editMessageText"] = tokenErr("editMessageText")
		h.api.mu.Unlock()

		h.tap(t, core.VerbCatalog, "1")

		assertSanitized(t, h, "admin ui screen update failed")
	})

	t.Run("input prompt send", func(t *testing.T) {
		h := newHarness(t)
		h.open(t)
		h.tap(t, core.VerbGlobalSet)
		h.tap(t, core.VerbField, "lg")
		h.api.mu.Lock()
		h.api.failMethods["sendMessage"] = tokenErr("sendMessage")
		h.api.mu.Unlock()

		h.tap(t, core.VerbString, "lg")

		assertSanitized(t, h, "admin ui input prompt send failed")
	})

	t.Run("callback acknowledgement", func(t *testing.T) {
		h := newHarness(t)
		h.open(t)
		h.api.mu.Lock()
		h.api.failMethods["answerCallbackQuery"] = tokenErr("answerCallbackQuery")
		h.api.mu.Unlock()

		h.tap(t, core.VerbCatalog, "1")

		assertSanitized(t, h, "admin ui callback acknowledgement failed")
	})
}

// TestFailedSaveKeepsPromptReplyableEndToEnd drives the persistence-failure
// recovery through the real handler chain: the failed save is surfaced as
// feedback, the prompt and its reply stay in the chat, and a retry reply to
// the SAME still-visible prompt message applies.
func TestFailedSaveKeepsPromptReplyableEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbGlobalSet)
	h.tap(t, core.VerbField, "lg")
	h.tap(t, core.VerbString, "lg")

	prompt := botPromptMessage(int(h.api.lastSendMessageID()), core.AdminInputMarker+" — language")
	h.store.failOnce("SetGlobalField", errors.New("sqlite busy"))

	h.deliver(t, replyToBotUpdate("  Русский  ", prompt))

	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "Failed:") {
		t.Fatalf("failure feedback expected, got %q", text)
	}
	if got := h.api.callCount("deleteMessage"); got != 0 {
		t.Fatalf("a failed save must not clean up the prompt or reply, got %d deletes", got)
	}
	if calls := h.store.writeCalls(); len(calls) != 0 {
		t.Fatalf("a failed save must not write, got %v", calls)
	}

	// Retry: reply to the SAME prompt message — it must still correlate.
	h.deliver(t, replyToBotUpdate("  Русский  ", prompt))

	found := false
	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "SetGlobalField:language=Русский") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the retried reply must save the value, calls: %v", h.store.writeCalls())
	}
	if text := h.api.lastSentText("editMessageText"); !strings.Contains(text, "Saved.") {
		t.Fatalf("the retried save must refresh the screen, got %q", text)
	}
}
