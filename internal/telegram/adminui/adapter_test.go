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
	"runtime"
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
	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/support/httpclient"
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
	if err := s.takeFailure("ClearChatField"); err != nil {
		return err
	}
	s.record("ClearChatField:" + field)
	s.mu.Lock()
	if field == "all" {
		delete(s.chatRows, chatID) // atomic reset: the whole override row goes
	} else {
		delete(s.chatRows[chatID], field)
	}
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

// HasCatalogChat reports whether the chat is registered in the catalog.
func (s *memStore) HasCatalogChat(chatID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, entry := range s.catalog {
		if entry.ChatID == chatID {
			return true
		}
	}

	return false
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
	dispatcher execution.Dispatcher
	// stopRecreate stops the default (10ms) re-creation watcher, so a test
	// can run its own with a different quiet window.
	stopRecreate func()
	// commandBlock, when set, parks the next command delivery inside the
	// route predicate (before the /admin handler runs) until released.
	commandBlock atomic.Pointer[commandBlock]
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
	// The HTTP caller matches production (app.Run wires WithHTTPClient):
	// telego's default fasthttp caller cannot abort an in-flight request on
	// context cancellation, so shutdown cancellation of the worker's menu IO
	// is not observable through it.
	botAPI, err := tg.NewBot(testToken, tg.WithAPIServer(api.server.URL), tg.WithHTTPClient(httpclient.New()), tg.WithDiscardLogger())
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	logs := &bytes.Buffer{}
	adapter := NewAdapter(controller, manager, adminIDs, slog.New(slog.NewTextHandler(logs, nil)), botAPI)

	bh, err := th.NewBotHandler(botAPI, make(chan tg.Update))
	if err != nil {
		t.Fatalf("NewBotHandler: %v", err)
	}
	t.Cleanup(func() { _ = bh.Stop() })

	h := &harness{
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

	// The command predicate doubles as a determinism lever: a test can park
	// the /admin delivery inside route matching — before the command handler
	// runs at all — to pin the ingress-event-before-handler interleaving the
	// production middleware produces.
	adapter.RegisterRoutes(bh, testBotID, func(_ context.Context, update tg.Update) bool {
		if blocked := h.commandBlock.Load(); blocked != nil {
			blocked.entered <- struct{}{}
			<-blocked.release
		}

		return anyCommand(context.Background(), update)
	})

	// The menu re-creation rig: a live dispatcher the tests publish
	// MessageSent into, and the adapter watching it with a short debounce.
	// No publisher is wired into the harness routes, so tests without
	// explicit publishes see no re-creations.
	dispatcher := execution.NewDispatcher(execution.DispatcherConfig{QueueSize: 64}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	dispatcher.Start(context.Background())
	t.Cleanup(dispatcher.Stop)
	h.dispatcher = dispatcher

	stopRecreate := adapter.startWatching(context.Background(), dispatcher, 10*time.Millisecond)
	t.Cleanup(stopRecreate)
	h.stopRecreate = stopRecreate

	return h
}

func anyCommand(_ context.Context, _ tg.Update) bool { return true }

// commandBlock parks one delivery inside the harness's command predicate:
// entered fires when the update reached the predicate (no handler has run
// yet), release lets it proceed.
type commandBlock struct {
	entered chan struct{}
	release chan struct{}
}

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

// replyKeyboards splits the sent messages by reply-markup kind: picker
// keyboards (with a request-chat button) versus keyboard removals.
func replyKeyboards(h *harness) (pickers, removals []map[string]any) {
	for _, request := range h.api.requestsOf("sendMessage") {
		var payload map[string]any
		if err := json.Unmarshal([]byte(request.Body), &payload); err != nil {
			continue
		}
		markup, _ := payload["reply_markup"].(map[string]any)
		if markup == nil {
			continue
		}
		if _, ok := markup["keyboard"]; ok {
			pickers = append(pickers, payload)
		}
		if _, ok := markup["remove_keyboard"]; ok {
			removals = append(removals, payload)
		}
	}

	return pickers, removals
}

// keyboardRemovalSequence reports the sendMessage order positions of the
// picker keyboard and of every keyboard removal, so tests can assert that a
// removal lands AFTER a late keyboard delivery.
func keyboardRemovalSequence(h *harness) (pickerIdx int, removalIdxs []int) {
	for i, request := range h.api.requestsOf("sendMessage") {
		var payload map[string]any
		if err := json.Unmarshal([]byte(request.Body), &payload); err != nil {
			continue
		}
		markup, _ := payload["reply_markup"].(map[string]any)
		if markup == nil {
			continue
		}
		if _, ok := markup["keyboard"]; ok && pickerIdx == 0 {
			pickerIdx = i + 1 // 1-based; 0 = no keyboard seen
		}
		if _, ok := markup["remove_keyboard"]; ok {
			removalIdxs = append(removalIdxs, i+1)
		}
	}

	return pickerIdx, removalIdxs
}

// requestChatID extracts the request-chat button's request_id from a picker
// keyboard payload.
func requestChatID(payload map[string]any) int64 {
	markup := payload["reply_markup"].(map[string]any)
	rows := markup["keyboard"].([]any)
	buttons := rows[0].([]any)
	button := buttons[0].(map[string]any)
	requestChat := button["request_chat"].(map[string]any)
	id, _ := requestChat["request_id"].(float64)

	return int64(id)
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

// TestPickerKeyboardLifecycleEndToEnd drives the chat-picker reply keyboard
// through the real handler chain: every transition that must remove it
// (acceptance, cancel, /admin reopen, manual chat-ID completion, delayed
// send completing after a session replacement) and the one that must not
// (a stale or mismatched selection).
func TestPickerKeyboardLifecycleEndToEnd(t *testing.T) {
	armPicker := func(t *testing.T, h *harness) int {
		t.Helper()

		requestID := h.beginPicker(t)

		pickers, removals := replyKeyboards(h)
		if len(pickers) != 1 || len(removals) != 0 {
			t.Fatalf("picker keyboard expected, pickers=%d removals=%d", len(pickers), len(removals))
		}
		if got := requestChatID(pickers[0]); got != int64(requestID)&0x7fffffff {
			t.Fatalf("request id mismatch: wire %d pending %d", got, requestID)
		}

		return requestID
	}

	t.Run("acceptance removes the keyboard before verification", func(t *testing.T) {
		h := newHarness(t)
		requestID := armPicker(t, h)
		h.api.chats[sharedGroupID] = sharedGroupInfo()

		h.deliver(t, chatSharedUpdate(requestID))

		_, removals := replyKeyboards(h)
		if len(removals) != 1 {
			t.Fatalf("exactly one keyboard removal expected, got %d", len(removals))
		}
		if text := lastJSONText(removals[0]); text != core.MsgPickerReceived {
			t.Fatalf("acceptance acknowledgement expected, got %q", text)
		}
		if got := h.api.callCount("getChat"); got != 1 {
			t.Fatalf("verification must run after the removal, got %d getChat calls", got)
		}
		if !h.store.HasCatalogChat(sharedGroupID) {
			t.Fatalf("the accepted chat must be registered, calls: %v", h.store.writeCalls())
		}

		// A duplicate selection is consumed without a second removal.
		h.deliver(t, chatSharedUpdate(requestID))
		if _, removals = replyKeyboards(h); len(removals) != 1 {
			t.Fatalf("duplicate selection must not remove a keyboard again, got %d", len(removals))
		}
		if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "no longer active") {
			t.Fatalf("duplicate must be consumed with a notice, got %q", text)
		}
	})

	t.Run("stale selection never dismisses the live keyboard", func(t *testing.T) {
		h := newHarness(t)
		requestID := armPicker(t, h)

		h.deliver(t, chatSharedUpdate(requestIDOther()))

		_, removals := replyKeyboards(h)
		if len(removals) != 0 {
			t.Fatalf("a stale selection must not dismiss the newer picker's keyboard, got %d removals", len(removals))
		}
		if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "no longer active") {
			t.Fatalf("the stale selection must be consumed with a notice, got %q", text)
		}
		if !h.manager.PickerKeyboardLive(testAdmin) {
			t.Fatalf("the live picker must survive the stale selection")
		}

		// The matching selection still completes normally afterwards.
		h.api.chats[sharedGroupID] = sharedGroupInfo()
		h.deliver(t, chatSharedUpdate(requestID))
		if _, removals = replyKeyboards(h); len(removals) != 1 {
			t.Fatalf("the matching selection must remove the keyboard, got %d removals", len(removals))
		}
	})

	t.Run("cancel removes the keyboard", func(t *testing.T) {
		h := newHarness(t)
		armPicker(t, h)

		h.deliver(t, privateUpdate(testAdmin, "/cancel"))

		_, removals := replyKeyboards(h)
		if len(removals) != 1 {
			t.Fatalf("cancel must remove the picker keyboard, got %d removals", len(removals))
		}
		if text := lastJSONText(removals[0]); !strings.Contains(text, "Cancelled") {
			t.Fatalf("the removal acknowledgement doubles as the cancel line, got %q", text)
		}
		if got := h.api.callCount("deleteMessage"); got != 1 {
			t.Fatalf("cancel must delete the picker prompt, got %d deletes", got)
		}
	})

	t.Run("reopen removes the keyboard", func(t *testing.T) {
		h := newHarness(t)
		armPicker(t, h)

		h.deliver(t, privateUpdate(testAdmin, "/admin"))

		_, removals := replyKeyboards(h)
		if len(removals) != 1 {
			t.Fatalf("reopen must remove the replaced session's picker keyboard, got %d removals", len(removals))
		}
		if text := lastJSONText(removals[0]); text != core.MsgPickerClosed {
			t.Fatalf("closure acknowledgement expected, got %q", text)
		}
	})

	t.Run("manual chat id completes and removes the keyboard", func(t *testing.T) {
		h := newHarness(t)
		armPicker(t, h)
		h.api.chats[sharedGroupID] = sharedGroupInfo()

		prompt := botPromptMessage(int(h.api.lastSendMessageID()), core.AdminInputMarker+" — Add chat")
		h.deliver(t, replyToBotUpdate(strconv.FormatInt(sharedGroupID, 10), prompt))

		_, removals := replyKeyboards(h)
		if len(removals) != 1 {
			t.Fatalf("manual completion must remove the picker keyboard, got %d removals", len(removals))
		}
		if got := h.api.callCount("getChat"); got != 1 {
			t.Fatalf("manual completion must verify the chat, got %d getChat calls", got)
		}
		if !h.store.HasCatalogChat(sharedGroupID) {
			t.Fatalf("the manual chat must be registered, calls: %v", h.store.writeCalls())
		}
	})

	t.Run("delayed send after replacement removes the keyboard", func(t *testing.T) {
		h := newHarness(t)
		h.open(t)
		h.tap(t, core.VerbCatalog, "1")

		// The picker keyboard send parks inside the fake API.
		entered, release := h.api.armGate("sendMessage")
		var releaseOnce sync.Once
		releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseGate)

		session := h.manager.Session(testAdmin)
		_, _, msgID, _ := session.DetachedSnapshot()
		data := core.EncodeAction(core.VerbCatalog, session.Seq, "a")

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.bh.BaseGroup().HandleUpdate(context.Background(), h.botAPI, callbackUpdate(testAdmin, msgID, data)); err != nil {
				t.Errorf("handle update: %v", err)
			}
		}()
		<-entered // the picker send is in flight

		// /admin replaces the session while the keyboard is being delivered.
		// The reopen runs on its own goroutine: the picker send holds the
		// render gate while parked, so the reopen's dashboard render waits
		// for the gate and must not block the main goroutine that will
		// release it.
		var reopen sync.WaitGroup
		reopen.Add(1)
		go func() {
			defer reopen.Done()
			h.deliver(t, privateUpdate(testAdmin, "/admin"))
		}()
		releaseGate()
		wg.Wait()
		reopen.Wait()

		pickers, removals := replyKeyboards(h)
		if len(pickers) != 1 {
			t.Fatalf("the keyboard must have been delivered, got %d", len(pickers))
		}
		// The keyboard must never linger: the delayed send's stamp is always
		// refused (its session was replaced), so its cleanup removal always
		// fires after the delivery. The reopen's own cleanup may then find no
		// live picker and skip — behind the render gate that ordering is a
		// legitimate single-removal resolution, so only a floor is asserted.
		if len(removals) == 0 {
			t.Fatalf("the delivered keyboard must be removed after the replacement, got none")
		}
		for _, removal := range removals {
			if text := lastJSONText(removal); text != core.MsgPickerClosed {
				t.Fatalf("closure acknowledgement expected, got %q", text)
			}
		}
		pickerIdx, removalIdxs := keyboardRemovalSequence(h)
		if pickerIdx == 0 || len(removalIdxs) == 0 || removalIdxs[len(removalIdxs)-1] < pickerIdx {
			t.Fatalf("a removal must land after the late keyboard delivery: picker=%d removals=%v",
				pickerIdx, removalIdxs)
		}
		if pending := h.manager.Session(testAdmin).Pending; pending != nil {
			t.Fatalf("the replacement session must not inherit a picker pending, got %+v", pending)
		}
	})

	t.Run("superseding picker sends no removal", func(t *testing.T) {
		h := newHarness(t)
		armPicker(t, h)

		h.tap(t, core.VerbCatalog, "a")

		pickers, removals := replyKeyboards(h)
		if len(pickers) != 2 || len(removals) != 0 {
			t.Fatalf("a new picker supersedes the keyboard in place: pickers=%d removals=%d", len(pickers), len(removals))
		}
	})
}

// TestUnmarkedReplyToPendingIsInputEndToEnd pins the classification order:
// pending correlation beats marker classification, so a reply to the live
// pending is consumed as input even when the marker text is absent or
// altered — it can never slip into the ordinary conversation pipeline.
func TestUnmarkedReplyToPendingIsInputEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbGlobalSet)
	h.tap(t, core.VerbField, "lg")
	h.tap(t, core.VerbString, "lg")

	// The replied-to message's marker text has been altered beyond recognition.
	prompt := botPromptMessage(int(h.api.lastSendMessageID()), "definitely not an admin input marker")
	h.deliver(t, replyToBotUpdate("Русский", prompt))

	found := false
	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "SetGlobalField:language=Русский") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unmarked reply must be applied as admin input, calls: %v", h.store.writeCalls())
	}
	if got := h.api.callCount("deleteMessage"); got != 2 {
		t.Fatalf("processed input cleans up prompt and reply, got %d deletes", got)
	}
	if text := h.api.lastSentText("editMessageText"); !strings.Contains(text, "Saved.") {
		t.Fatalf("the save must refresh the screen, got %q", text)
	}
}

// TestDocumentReplyWithAlteredMarkerUploadsEndToEnd extends the pending-first
// classification to document uploads: a document reply matched by the pending
// is downloaded and applied although the marker text is gone.
func TestDocumentReplyWithAlteredMarkerUploadsEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbPrompts)
	h.tap(t, core.VerbPromptFeat, "ch")
	h.tap(t, core.VerbPromptUp, "ch")

	h.api.files["uploads/prompt.md"] = []byte("custom chat {{.Language}}")

	prompt := botPromptMessage(int(h.api.lastSendMessageID()), "") // text stripped entirely
	h.deliver(t, documentReplyToBotUpdate(prompt))

	found := false
	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "SetPromptTemplate:chat:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unmarked document reply must be uploaded as admin input, calls: %v", h.store.writeCalls())
	}
}

// ---- menu re-creation (MessageSent watching) ----

// publishMessageSent feeds a MessageSent for chatID into the harness
// dispatcher, standing in for the bot package's send-path publishers.
func (h *harness) publishMessageSent(chatID int64) {
	h.dispatcher.Publish(execution.MessageSent{
		RunContext: execution.RunContext{ChatID: chatID},
		At:         time.Now(),
		MessageID:  77,
	})
}

// publishRunStarted feeds a conversation run start for chatID into the
// harness dispatcher, standing in for the tool-use runtime's publishers.
func (h *harness) publishRunStarted(chatID int64) {
	h.dispatcher.Publish(execution.RunStarted{
		RunContext: execution.RunContext{ChatID: chatID},
		At:         time.Now(),
	})
}

// publishRunCompleted feeds a conversation run completion for chatID into
// the harness dispatcher.
func (h *harness) publishRunCompleted(chatID int64) {
	h.dispatcher.Publish(execution.RunCompleted{
		RunContext: execution.RunContext{ChatID: chatID},
		At:         time.Now(),
	})
}

// publishRunFailed feeds a conversation run failure for chatID into the
// harness dispatcher.
func (h *harness) publishRunFailed(chatID int64) {
	h.dispatcher.Publish(execution.RunFailed{
		RunContext: execution.RunContext{ChatID: chatID},
		At:         time.Now(),
		Reason:     execution.ReasonBackend,
	})
}

// slowWatcher swaps the default 10ms re-creation watcher for a 300ms one —
// the default window is too fast for witness-based interleavings. The
// replacement is stopped with the test's cleanup.
func (h *harness) slowWatcher(t *testing.T) {
	t.Helper()

	h.stopRecreate()
	stopSlow := h.adapter.startWatching(context.Background(), h.dispatcher.(execution.Subscriber), 300*time.Millisecond)
	t.Cleanup(stopSlow)
}

// waitFor polls cond until it holds or the deadline passes.
func (h *harness) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s did not settle in time", what)
}

// keyboardSends counts sendMessage requests carrying an inline keyboard —
// menu sends and re-created menus, as opposed to prompts, pickers, and
// notices. The admin UI's own sends never publish MessageSent, so only an
// actual re-creation can grow this counter past its baseline.
func (h *harness) keyboardSends() int {
	count := 0
	for _, request := range h.api.requestsOf("sendMessage") {
		if strings.Contains(request.Body, `"inline_keyboard"`) {
			count++
		}
	}

	return count
}

// keyboardSendsFor counts sendMessage requests carrying an inline keyboard
// for ONE chat — its menu sends and re-creations, distinguished per admin in
// multi-admin scenarios.
func (h *harness) keyboardSendsFor(chatID int64) int {
	count := 0
	for _, request := range h.api.requestsOf("sendMessage") {
		var payload struct {
			ChatID      int64 `json:"chat_id"`
			ReplyMarkup struct {
				InlineKeyboard []json.RawMessage `json:"inline_keyboard"`
			} `json:"reply_markup"`
		}
		if err := json.Unmarshal([]byte(request.Body), &payload); err != nil {
			continue
		}
		if payload.ChatID == chatID && len(payload.ReplyMarkup.InlineKeyboard) > 0 {
			count++
		}
	}

	return count
}

// sessionMsgID reports the message ID currently adopted by the admin's session.
func (h *harness) sessionMsgID(t *testing.T) int {
	t.Helper()

	session := h.manager.Session(testAdmin)
	if session == nil {
		t.Fatalf("no session for admin %d", testAdmin)
	}
	_, _, msgID, _ := session.DetachedSnapshot()

	return msgID
}

// quietElapse covers several harness quiet windows (10ms each): long enough
// for any re-creation a misbehaving watcher would fire, short enough to keep
// the suite fast.
const quietElapse = 100 * time.Millisecond

// waitForRecreate waits until one settled re-creation is visible past the
// opening menu. Settled means all three steps of resendMenu landed: the
// fresh keyboard send, the delete of the buried menu, and the adoption of
// the new message ID — asserting right after the send would race the tail.
func (h *harness) waitForRecreate(t *testing.T) {
	t.Helper()

	h.waitFor(t, "settled menu re-creation", func() bool {
		return h.keyboardSends() == 2 &&
			h.api.callCount("deleteMessage") == 1 &&
			h.sessionMsgID(t) == int(h.api.lastSendMessageID())
	})
}

// coreRecreateFrame is the stack symbol of the core's gated re-creation
// entry — the only place a parked re-creation is observable while it waits
// on a held render gate.
const coreRecreateFrame = "internal/adminui.(*Manager).RecreateRender"

// waitForStackFrame polls all goroutine stacks until the frame appears
// (want) or disappears (!want). A positive, timing-free observation of
// where the worker actually is: a held render gate parks the re-creation
// inside RecreateRender, and once it is seen there, every following stage
// of the interleaving is order-locked.
func waitForStackFrame(t *testing.T, what, frame string, want bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	buf := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), frame) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s did not settle in time (frame %q presence=%v)", what, frame, want)
}

// TestMenuRecreatedAfterChatMessage is the headline flow: a chat message in
// the admin DM re-creates the buried menu — fresh keyboard send, best-effort
// delete of the buried one, new message ID adopted.
func TestMenuRecreatedAfterChatMessage(t *testing.T) {
	h := newHarness(t)

	initialMsgID := h.open(t)
	if got := h.keyboardSends(); got != 1 {
		t.Fatalf("expected the opened menu to carry a keyboard, got %d keyboard sends", got)
	}

	h.publishMessageSent(testAdmin)

	h.waitForRecreate(t)

	deletes := h.api.requestsOf("deleteMessage")
	if len(deletes) != 1 {
		t.Fatalf("expected exactly one deleteMessage, got %d", len(deletes))
	}
	var params struct {
		ChatID    int64 `json:"chat_id"`
		MessageID int   `json:"message_id"`
	}
	if err := json.Unmarshal([]byte(deletes[0].Body), &params); err != nil {
		t.Fatalf("deleteMessage body: %v", err)
	}
	if params.ChatID != testAdmin || params.MessageID != int(initialMsgID) {
		t.Fatalf("expected delete of menu message %d in chat %d, got %+v", initialMsgID, testAdmin, params)
	}

	if got := h.sessionMsgID(t); got != int(h.api.lastSendMessageID()) {
		t.Fatalf("session must adopt the re-created menu: session %d, last send %d", got, h.api.lastSendMessageID())
	}

	payload := h.api.lastBodyJSON("sendMessage")
	markup, _ := payload["reply_markup"].(map[string]any)
	if _, ok := markup["inline_keyboard"]; !ok {
		t.Fatalf("re-created menu must carry a fresh inline keyboard, got %v", payload["reply_markup"])
	}
}

// TestMenuRecreatePreservesSessionExpiry proves the automatic re-creation is
// TTL-neutral: recurring chat activity cannot keep an unused admin session
// alive past its original inactivity window.
func TestMenuRecreatePreservesSessionExpiry(t *testing.T) {
	h := newHarness(t)
	h.open(t)

	h.publishMessageSent(testAdmin)
	h.waitForRecreate(t)

	// One full session TTL past the opening: the re-creation must not have
	// extended the window.
	h.clock.Advance(core.SessionTTL + time.Minute)
	if _, live := h.manager.ActiveSession(testAdmin); live {
		t.Fatal("automatic re-creation must not slide the session TTL")
	}
}

// TestMenuRecreateDebouncesBurst proves the quiet window coalesces: five
// messages in the admin DM cost exactly one re-creation.
func TestMenuRecreateDebouncesBurst(t *testing.T) {
	h := newHarness(t)
	h.open(t)

	for range 5 {
		h.publishMessageSent(testAdmin)
	}

	h.waitForRecreate(t)

	time.Sleep(quietElapse)
	if got := h.keyboardSends(); got != 2 {
		t.Fatalf("a burst must cost one re-creation, got %d keyboard sends", got)
	}
	if got := h.api.callCount("deleteMessage"); got != 1 {
		t.Fatalf("a burst must delete the buried menu once, got %d deletes", got)
	}
}

// TestMenuRecreateSuppressedWhilePendingInput walks into a ForceReply flow and
// proves chat activity during the flow neither sends nor deletes anything.
func TestMenuRecreateSuppressedWhilePendingInput(t *testing.T) {
	h := newHarness(t)
	h.open(t)

	h.tap(t, core.VerbGlobalSet)
	h.tap(t, core.VerbField, "lg")
	h.tap(t, core.VerbString, "lg")

	session := h.manager.Session(testAdmin)
	if !session.HasPendingInput() {
		t.Fatal("test setup: the string flow must leave a pending input")
	}

	baseline := h.keyboardSends()
	h.publishMessageSent(testAdmin)

	time.Sleep(quietElapse)
	if got := h.keyboardSends(); got != baseline {
		t.Fatalf("re-creation must be suppressed while an input is pending: %d keyboard sends after publish (baseline %d)", got, baseline)
	}
	if got := h.api.callCount("deleteMessage"); got != 0 {
		t.Fatalf("a suppressed re-creation must not delete anything, got %d deletes", got)
	}
}

// TestMenuRecreateSkipsWithoutSessionOrExpired covers the two dead-chat
// cases: an admin DM with no session at all (plus a non-admin chat as
// negative control), and a session that expired before the quiet window
// elapsed.
func TestMenuRecreateSkipsWithoutSessionOrExpired(t *testing.T) {
	h := newHarnessWithAdmins(t, testAdmin, 8)

	h.publishMessageSent(testAdmin) // admin, but no session
	h.publishMessageSent(4242)      // not an admin chat at all

	time.Sleep(quietElapse)
	if got := h.api.callCount("sendMessage"); got != 0 {
		t.Fatalf("no session, no re-creation: %d sends already", got)
	}

	h.open(t)
	base := h.keyboardSends()

	h.clock.Advance(core.SessionTTL + time.Minute)
	h.publishMessageSent(testAdmin)

	time.Sleep(quietElapse)
	if got := h.keyboardSends(); got != base {
		t.Fatalf("an expired session must not re-create: %d keyboard sends after publish (base %d)", got, base)
	}
	if got := h.api.callCount("deleteMessage"); got != 0 {
		t.Fatalf("an expired session must not delete anything, got %d deletes", got)
	}
}

// TestPromptSendSurvivesInFlightRecreation pins the burial fix: a
// string-input callback committing while a menu re-creation's send is still
// in flight must wait for it — VerbString produces no render, so the prompt
// would otherwise bypass the render gate entirely and land BEFORE the
// re-created menu, which then buries it.
func TestPromptSendSurvivesInFlightRecreation(t *testing.T) {
	h := newHarness(t)
	h.open(t)

	// Navigate to a string field so the input flow is one tap away.
	h.tap(t, core.VerbGlobalSet)
	h.tap(t, core.VerbField, "lg")

	// Park the re-creation's send inside the API: RecreateRender holds the
	// render gate for the whole send.
	entered, release := h.api.armGate("sendMessage")
	h.publishMessageSent(testAdmin)

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("recreation send never reached the API")
	}

	// The string-input callback commits its pending mid-re-creation and its
	// prompt send blocks on the render gate. Wait for the commit (the
	// origin check precedes it, so the callback is safely accepted against
	// the not-yet-replaced menu) before letting the re-creation finish.
	promptDone := make(chan struct{})
	go func() {
		defer close(promptDone)
		h.tap(t, core.VerbString, "lg")
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !h.manager.Session(testAdmin).HasPendingInput() {
		time.Sleep(time.Millisecond)
	}
	if !h.manager.Session(testAdmin).HasPendingInput() {
		t.Fatal("the string callback never committed its pending input")
	}

	close(release)
	<-promptDone

	// The re-created menu must precede the prompt: find the last inline
	// keyboard send and the first ForceReply send after it.
	sends := h.api.requestsOf("sendMessage")
	menuIdx, promptIdx := -1, -1
	for i, request := range sends {
		switch {
		case strings.Contains(request.Body, `"inline_keyboard"`):
			menuIdx = i
		case promptIdx == -1 && strings.Contains(request.Body, `"force_reply"`):
			promptIdx = i
		}
	}
	if menuIdx == -1 || promptIdx == -1 {
		t.Fatalf("expected a re-created menu and an input prompt, menu=%d prompt=%d (%d sends)", menuIdx, promptIdx, len(sends))
	}
	if menuIdx > promptIdx {
		t.Fatalf("the re-created menu buried the input prompt: menu at send %d, prompt at send %d", menuIdx, promptIdx)
	}

	// The prompt is a live, correlated pending input anchored at the bottom.
	if !h.manager.MatchesPendingInput(testAdmin, int(h.api.lastSendMessageID())) {
		t.Fatal("the prompt must be correlated to the pending input")
	}
	if got := h.api.callCount("deleteMessage"); got != 1 {
		t.Fatalf("expected exactly one buried-menu delete, got %d", got)
	}
}

// TestMenuRecreateSettlesWithInFlightEdit parks a navigation edit mid-API and
// fires a re-creation while it still holds the render gate. The re-creation
// must wait for the gate, land on the post-edit state, and cost exactly one
// send + one delete.
func TestMenuRecreateSettlesWithInFlightEdit(t *testing.T) {
	h := newHarness(t)
	h.open(t)

	entered, release := h.api.armGate("editMessageText")
	tapDone := make(chan struct{})
	go func() {
		defer close(tapDone)
		h.tap(t, core.VerbCatalog, "1")
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("navigation edit never reached the API")
	}

	h.publishMessageSent(testAdmin)

	close(release)
	<-tapDone

	// The navigation edit itself allocates a fake message ID in its
	// response, so the settled state is: exactly one extra keyboard send
	// (the re-created menu), one delete, and the session on the last send.
	h.waitForRecreate(t)

	if got := h.api.callCount("deleteMessage"); got != 1 {
		t.Fatalf("expected exactly one buried-menu delete, got %d", got)
	}
	if got := h.sessionMsgID(t); got != int(h.api.lastSendMessageID()) {
		t.Fatalf("session must adopt the re-created message: session %d, last send %d", got, h.api.lastSendMessageID())
	}
}

// TestMenuRecreateGateReadyBeforeRoutesRegistered pins the startup-window
// fix: the re-creation worker watches the event dispatcher from BEFORE the
// bot's routes are registered (that happens inside Bot.Run), so a reminder
// delivered to an admin during the startup interval reaches canRecreate
// while RegisterRoutes has not run yet. A lazily-bound API client made the
// gate blind there — and the unsynchronized bind raced the worker's reads
// (confirmed under -race). The client is bound at construction instead.
func TestMenuRecreateGateReadyBeforeRoutesRegistered(t *testing.T) {
	manager := core.NewManager(nil)
	controller := core.NewController(manager, adminconfig.NewService(newMemStore(), nil, nil), []int64{testAdmin}, nil)

	api := newFakeAdminAPI(t)
	// The HTTP caller matches production (app.Run wires WithHTTPClient):
	// telego's default fasthttp caller cannot abort an in-flight request on
	// context cancellation, so shutdown cancellation of the worker's menu IO
	// is not observable through it.
	botAPI, err := tg.NewBot(testToken, tg.WithAPIServer(api.server.URL), tg.WithHTTPClient(httpclient.New()), tg.WithDiscardLogger())
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	// Constructed; routes deliberately NOT registered — the startup window.
	adapter := NewAdapter(controller, manager, []int64{testAdmin}, slog.New(slog.NewTextHandler(io.Discard, nil)), botAPI)

	manager.Replace(testAdmin, core.ScreenState{Kind: core.ScreenDashboard})
	if !adapter.canRecreate(testAdmin) {
		t.Fatal("the re-creation gate must be functional before routes are registered: a reminder delivered during startup reaches it first")
	}
}

// TestMenuRecreateSurfacesAfterAdminDuringPendingInput pins the
// lost-activity fix: /admin arriving while an input is pending publishes its
// activity event BEFORE the command handler clears the pending. The old
// arm-time pending check discarded exactly that event, and /admin's
// dashboard render edits the buried menu IN PLACE — admin UI sends publish
// no further events, so the menu never surfaced. Now the event arms
// regardless of the pending, the pending is validated when the window
// elapses, and a suppressed fire re-arms so the menu surfaces as soon as the
// input resolves.
func TestMenuRecreateSurfacesAfterAdminDuringPendingInput(t *testing.T) {
	h := newHarnessWithAdmins(t, testAdmin, 8)

	h.open(t)                                // admin 7's menu
	h.deliver(t, privateUpdate(8, "/admin")) // admin 8's menu — the FIFO witness

	// Walk admin 7 into a string input so a pending input is live.
	h.tap(t, core.VerbGlobalSet)
	h.tap(t, core.VerbField, "lg")
	h.tap(t, core.VerbString, "lg")

	// The /admin-like activity lands while the pending is still set, and
	// admin 8's event acts as the witness: one dispatcher queue, one worker
	// — once 8's re-creation has landed, admin 7's event has provably been
	// consumed, and the /admin command below has provably NOT run yet, so
	// the consumption happened with the pending still set.
	h.publishMessageSent(testAdmin)
	h.publishMessageSent(8)

	h.waitFor(t, "witness re-creation for admin 8", func() bool {
		return h.keyboardSendsFor(8) == 2
	})

	// Suppression while the input is genuinely pending still holds.
	if got := h.keyboardSendsFor(testAdmin); got != 1 {
		t.Fatalf("no re-creation may happen while the input is pending, got %d keyboard sends for admin 7", got)
	}

	// NOW the handler runs: the fresh dashboard replaces the session (the
	// pending dies with it) and edits the buried menu in place.
	h.deliver(t, privateUpdate(testAdmin, "/admin"))

	// The preserved activity must surface the menu at the bottom.
	h.waitFor(t, "menu re-creation after /admin cleared the pending", func() bool {
		return h.keyboardSendsFor(testAdmin) == 2
	})

	// Exactly one re-creation: the re-arm loop ends, nothing else fires.
	time.Sleep(quietElapse)
	if got := h.keyboardSendsFor(testAdmin); got != 2 {
		t.Fatalf("exactly one re-creation expected after /admin, got %d keyboard sends", got)
	}
	if got := h.keyboardSendsFor(8); got != 2 {
		t.Fatalf("admin 8 must stay untouched by admin 7's flow, got %d keyboard sends", got)
	}
}

// TestMenuRecreateDefersWhileRunActiveAndSurfacesAfterRunEnd pins the issue
// #148 fix: in an admin DM the live reply preview is a rich-message draft,
// and any other bot send — the fresh menu included — dismisses it. A
// re-creation firing while a conversation run is active would flash-remove
// the draft right after it appears. The re-creation must instead defer
// until the run ends, then surface exactly once.
func TestMenuRecreateDefersWhileRunActiveAndSurfacesAfterRunEnd(t *testing.T) {
	h := newHarnessWithAdmins(t, testAdmin, 8)
	h.slowWatcher(t)

	h.open(t)                                // admin 7's menu
	h.deliver(t, privateUpdate(8, "/admin")) // admin 8's menu — the FIFO witness

	// Arm admin 7's window, then start a conversation run before it elapses.
	// Admin 8's event acts as the witness: one dispatcher queue, one
	// subscriber goroutine — once 8's re-creation has landed, admin 7's run
	// start has provably been recorded, so the deferral below is
	// order-locked, not a timing assumption.
	h.publishMessageSent(testAdmin)
	h.publishRunStarted(testAdmin)
	h.publishMessageSent(8)

	h.waitFor(t, "witness re-creation for admin 8", func() bool {
		return h.keyboardSendsFor(8) == 2 &&
			h.api.callCount("deleteMessage") == 1 // admin 8's buried menu
	})

	// The run is active: past the armed deadline the re-creation must be
	// deferred — no fresh menu send for admin 7, and no delete of its
	// buried menu on top of admin 8's.
	time.Sleep(400 * time.Millisecond)
	if got := h.keyboardSendsFor(testAdmin); got != 1 {
		t.Fatalf("no re-creation may happen while the run is active, got %d keyboard sends for admin 7", got)
	}
	if got := h.api.callCount("deleteMessage"); got != 1 {
		t.Fatalf("no delete may happen while the run is active, got %d deletes", got)
	}

	// The run completes: the re-armed schedule surfaces the menu — all three
	// settled steps of one re-creation.
	h.publishRunCompleted(testAdmin)
	h.waitFor(t, "settled menu re-creation after the run ended", func() bool {
		return h.keyboardSendsFor(testAdmin) == 2 &&
			h.api.callCount("deleteMessage") == 2 &&
			h.sessionMsgID(t) == int(h.api.lastSendMessageID())
	})

	// Exactly one: the re-arm loop ends once the run is gone.
	time.Sleep(quietElapse)
	if got := h.keyboardSendsFor(testAdmin); got != 2 {
		t.Fatalf("exactly one re-creation expected after the run ended, got %d keyboard sends", got)
	}
	if got := h.keyboardSendsFor(8); got != 2 {
		t.Fatalf("admin 8 must stay untouched by admin 7's flow, got %d keyboard sends", got)
	}
}

// TestMenuRecreateRunFailedReleasesDeferral is the failure twin of the
// headline deferral test: a failed run releases the deferral exactly like a
// completed one.
func TestMenuRecreateRunFailedReleasesDeferral(t *testing.T) {
	h := newHarnessWithAdmins(t, testAdmin, 8)
	h.slowWatcher(t)

	h.open(t)                                // admin 7's menu
	h.deliver(t, privateUpdate(8, "/admin")) // admin 8's menu — the FIFO witness

	h.publishMessageSent(testAdmin)
	h.publishRunStarted(testAdmin)
	h.publishMessageSent(8)

	h.waitFor(t, "witness re-creation for admin 8", func() bool {
		return h.keyboardSendsFor(8) == 2
	})

	time.Sleep(400 * time.Millisecond)
	if got := h.keyboardSendsFor(testAdmin); got != 1 {
		t.Fatalf("no re-creation may happen while the run is active, got %d keyboard sends for admin 7", got)
	}

	h.publishRunFailed(testAdmin)
	h.waitFor(t, "settled menu re-creation after the run failed", func() bool {
		return h.keyboardSendsFor(testAdmin) == 2 &&
			h.api.callCount("deleteMessage") == 2 &&
			h.sessionMsgID(t) == int(h.api.lastSendMessageID())
	})

	time.Sleep(quietElapse)
	if got := h.keyboardSendsFor(testAdmin); got != 2 {
		t.Fatalf("exactly one re-creation expected after the run failed, got %d keyboard sends", got)
	}
}

// TestMenuRecreateSpuriousRunEndNeverWedges pins the clamp: a run end
// without a matching start (subscription began mid-run) must be ignored —
// never treated as activity, never left as a negative or sticky count. The
// chat must keep re-creating normally.
func TestMenuRecreateSpuriousRunEndNeverWedges(t *testing.T) {
	h := newHarness(t)

	h.open(t)
	if got := h.keyboardSends(); got != 1 {
		t.Fatalf("expected the opened menu to carry a keyboard, got %d keyboard sends", got)
	}

	h.publishRunCompleted(testAdmin)
	h.publishMessageSent(testAdmin)

	h.waitFor(t, "menu re-creation after a spurious run end", func() bool {
		return h.keyboardSends() == 2 &&
			h.api.callCount("deleteMessage") == 1 &&
			h.sessionMsgID(t) == int(h.api.lastSendMessageID())
	})
}

// TestMenuRecreateDeferredUntilLastConcurrentRunEnds pins the counter: two
// concurrent runs in the chat keep the re-creation deferred until the LAST
// one ends.
func TestMenuRecreateDeferredUntilLastConcurrentRunEnds(t *testing.T) {
	h := newHarnessWithAdmins(t, testAdmin, 8)
	h.slowWatcher(t)

	h.open(t)                                // admin 7's menu
	h.deliver(t, privateUpdate(8, "/admin")) // admin 8's menu — the FIFO witness

	h.publishMessageSent(testAdmin)
	h.publishRunStarted(testAdmin)
	h.publishRunStarted(testAdmin)
	h.publishMessageSent(8)

	h.waitFor(t, "witness re-creation for admin 8", func() bool {
		return h.keyboardSendsFor(8) == 2
	})

	// Both runs active: deferred past the armed deadline and one re-armed
	// window.
	time.Sleep(400 * time.Millisecond)
	if got := h.keyboardSendsFor(testAdmin); got != 1 {
		t.Fatalf("no re-creation may happen while both runs are active, got %d keyboard sends for admin 7", got)
	}

	// One run completes: the other still owns the bottom of the chat.
	h.publishRunCompleted(testAdmin)
	time.Sleep(400 * time.Millisecond)
	if got := h.keyboardSendsFor(testAdmin); got != 1 {
		t.Fatalf("no re-creation may happen while one run is still active, got %d keyboard sends for admin 7", got)
	}

	// The last run completes: exactly one re-creation.
	h.publishRunCompleted(testAdmin)
	h.waitFor(t, "settled menu re-creation after the last run ended", func() bool {
		return h.keyboardSendsFor(testAdmin) == 2 &&
			h.api.callCount("deleteMessage") == 2 &&
			h.sessionMsgID(t) == int(h.api.lastSendMessageID())
	})

	time.Sleep(quietElapse)
	if got := h.keyboardSendsFor(testAdmin); got != 2 {
		t.Fatalf("exactly one re-creation expected after the last run ended, got %d keyboard sends", got)
	}
}

// TestMenuRecreateIgnoresNonAdminRunEvents pins the chat filter: run
// lifecycle events of non-admin chats must neither create a deferral for
// admin chats nor leak into their counts.
func TestMenuRecreateIgnoresNonAdminRunEvents(t *testing.T) {
	h := newHarness(t)

	h.open(t)
	if got := h.keyboardSends(); got != 1 {
		t.Fatalf("expected the opened menu to carry a keyboard, got %d keyboard sends", got)
	}

	h.publishRunStarted(4242)
	h.publishRunCompleted(4242)
	h.publishMessageSent(testAdmin)

	// The re-creation proceeds promptly: no global deferral, no cross-chat
	// counter pollution — and no run-end event for admin 7 is needed to
	// release anything.
	h.waitFor(t, "menu re-creation despite non-admin run events", func() bool {
		return h.keyboardSends() == 2 &&
			h.api.callCount("deleteMessage") == 1 &&
			h.sessionMsgID(t) == int(h.api.lastSendMessageID())
	})
}

// TestMenuRecreateDefersAdminWakeDuringRun documents the accepted /admin
// trade-off: the command itself still edits the buried menu in place
// immediately (its handler runs synchronously), but the resurfacing wake is
// deferred while a run is active, surfacing the menu once the run ends.
func TestMenuRecreateDefersAdminWakeDuringRun(t *testing.T) {
	h := newHarnessWithAdmins(t, testAdmin, 8)
	h.slowWatcher(t)

	h.open(t)                                // admin 7's menu
	h.deliver(t, privateUpdate(8, "/admin")) // admin 8's menu — the FIFO witness

	h.publishMessageSent(testAdmin)
	h.publishRunStarted(testAdmin)
	h.publishMessageSent(8)

	h.waitFor(t, "witness re-creation for admin 8", func() bool {
		return h.keyboardSendsFor(8) == 2
	})

	// The reopen wake lands mid-run: deferred, no re-creation.
	h.adapter.notifyRecreate(testAdmin)
	time.Sleep(400 * time.Millisecond)
	if got := h.keyboardSendsFor(testAdmin); got != 1 {
		t.Fatalf("the wake must be deferred while the run is active, got %d keyboard sends for admin 7", got)
	}

	// The run ends: the (kept-alive) schedule surfaces the menu.
	h.publishRunCompleted(testAdmin)
	h.waitFor(t, "settled menu re-creation after the run released the wake", func() bool {
		return h.keyboardSendsFor(testAdmin) == 2 &&
			h.api.callCount("deleteMessage") == 2 &&
			h.sessionMsgID(t) == int(h.api.lastSendMessageID())
	})

	time.Sleep(quietElapse)
	if got := h.keyboardSendsFor(testAdmin); got != 2 {
		t.Fatalf("exactly one re-creation expected after the run ended, got %d keyboard sends", got)
	}
}

// TestMenuRecreateIgnoresSupersededExpiry pins the stale-notification fix:
// a debounce notification of a superseded schedule must be discarded when it
// finally arrives, without disturbing the newer schedule. (The original
// interleaving — the timer firing while the worker's re-arm sat blocked on
// an in-flight session commit — ceased to be constructable once the arm-time
// gate stopped touching session state; the supersession-vs-fire race now
// lives in the worker's select between the events and due queues, and the
// same generation check covers it.) This shape also pins the fire-time
// re-arm: a fire landing while an input is pending keeps the activity alive
// until the input resolves, then surfaces the menu exactly once.
func TestMenuRecreateIgnoresSupersededExpiry(t *testing.T) {
	h := newHarness(t)

	// The default 10ms harness window is too fast for the deterministic
	// interleaving below; run a slow one instead.
	h.stopRecreate()
	stopSlow := h.adapter.startWatching(context.Background(), h.dispatcher.(execution.Subscriber), 300*time.Millisecond)
	t.Cleanup(stopSlow)

	h.open(t)
	if got := h.keyboardSends(); got != 1 {
		t.Fatalf("expected the opened menu to carry a keyboard, got %d keyboard sends", got)
	}

	// Walk into a string input so a pending input owns the bottom of the chat.
	h.tap(t, core.VerbGlobalSet)
	h.tap(t, core.VerbField, "lg")
	h.tap(t, core.VerbString, "lg")

	// Arm the first schedule, then supersede it before its deadline elapses.
	// Arming succeeds despite the pending input (arm-time session state is
	// stale by construction) — which is what makes the stale fire
	// deterministic: T1(gen1) fires into T2(gen2)'s schedule and must be
	// discarded there.
	h.publishMessageSent(testAdmin)
	time.Sleep(150 * time.Millisecond) // T1(gen1) armed, deadline +300ms
	h.publishMessageSent(testAdmin)    // supersedes: T2(gen2), deadline +450ms

	// T1's deadline elapses; its notification must be dropped as stale.
	time.Sleep(200 * time.Millisecond) // now ~350ms: T1 fired at 300ms
	if got := h.keyboardSends(); got != 1 {
		t.Fatalf("the superseded expiry must be discarded: %d keyboard sends", got)
	}

	// T2 then fires into the live pending input: suppressed, and the
	// activity is kept alive by re-arming rather than dropped.
	time.Sleep(150 * time.Millisecond) // now ~500ms: T2 fired at 450ms, re-armed
	if got := h.keyboardSends(); got != 1 {
		t.Fatalf("no re-creation while the input is pending, got %d keyboard sends", got)
	}

	// The input resolves: /admin replaces the session (the pending dies with
	// it) and edits the buried menu in place. The re-armed schedule then
	// surfaces the menu at the bottom — exactly once.
	editsBefore := h.api.callCount("editMessageText")
	h.deliver(t, privateUpdate(testAdmin, "/admin"))
	if got := h.api.callCount("editMessageText"); got != editsBefore+1 {
		t.Fatalf("expected the reopen to edit the buried menu in place once, edits went %d → %d", editsBefore, got)
	}

	h.waitForRecreate(t)

	time.Sleep(quietElapse)
	if got := h.keyboardSends(); got != 2 {
		t.Fatalf("exactly one re-creation expected after the input resolved, got %d keyboard sends", got)
	}
}

// TestMenuRecreateSurvivesReopenAfterSessionExpiry pins the reopen fix:
// /admin arriving after the previous session expired publishes its activity
// event BEFORE the command handler replaces that session. An arm-time
// liveness check discarded exactly that event, and the reopen then only
// edits the buried menu in place (the fresh render carries the old message
// ID) — admin-UI sends publish no events, so the menu never resurfaced
// without another message.
func TestMenuRecreateSurvivesReopenAfterSessionExpiry(t *testing.T) {
	h := newHarness(t)
	h.open(t)

	// Expire the session; the reopen must still be able to surface the menu.
	h.clock.Advance(core.SessionTTL + time.Minute)

	// The harness default window (10ms) would fire while the parked handler
	// is still waiting; run a slow watcher so the fire lands after the
	// reopen's replacement exists.
	h.stopRecreate()
	stopSlow := h.adapter.startWatching(context.Background(), h.dispatcher.(execution.Subscriber), 300*time.Millisecond)
	t.Cleanup(stopSlow)

	// Park the /admin delivery inside the route predicate: no handler has
	// run yet, the expired session is still in place — the exact shape of an
	// ingress event consumed before its command handler.
	block := &commandBlock{entered: make(chan struct{}, 1), release: make(chan struct{})}
	h.commandBlock.Store(block)

	reopenDone := make(chan struct{})
	go func() {
		defer close(reopenDone)
		h.deliver(t, privateUpdate(testAdmin, "/admin"))
	}()

	select {
	case <-block.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the /admin delivery never reached the route predicate")
	}

	// The reopen's activity event lands while the expired session is still
	// the live one — the handler is parked before it can replace anything.
	h.publishMessageSent(testAdmin)

	// Let the handler through: the fresh dashboard replaces the expired
	// session and edits the buried menu in place.
	close(block.release)
	<-reopenDone

	if got := h.api.callCount("editMessageText"); got != 1 {
		t.Fatalf("expected the reopen to edit the buried menu in place, got %d edits", got)
	}

	// The preserved activity must surface the menu at the bottom once the
	// quiet window elapses past the reopen.
	h.waitForRecreate(t)

	time.Sleep(quietElapse)
	if got := h.keyboardSends(); got != 2 {
		t.Fatalf("exactly one re-creation expected after the reopen, got %d keyboard sends", got)
	}
}

// TestMenuRecreateResurfacesAfterSkippedFireBeforeReopen pins the
// delayed-handler fix: the quiet window can elapse while the /admin handler is
// still stuck behind the route middlewares, and the fire then consumes the
// reopen's only activity event against the session the command is about to
// replace — the core silently skips a dead (expired) session, and the reopen
// afterwards just edits the buried menu in place, because admin-UI sends
// publish no events of their own. A successful reopen of a dead session that
// carried a rendered menu must re-anchor the quiet window itself.
func TestMenuRecreateResurfacesAfterSkippedFireBeforeReopen(t *testing.T) {
	h := newHarness(t)
	h.open(t)

	// Expire the session the command is about to replace.
	h.clock.Advance(core.SessionTTL + time.Minute)

	// Park the /admin delivery inside the route predicate — no handler has
	// run, the expired session is still in place. The default 10ms window
	// fires against it long before the handler gets to run.
	block := &commandBlock{entered: make(chan struct{}, 1), release: make(chan struct{})}
	h.commandBlock.Store(block)

	reopenDone := make(chan struct{})
	go func() {
		defer close(reopenDone)
		h.deliver(t, privateUpdate(testAdmin, "/admin"))
	}()

	select {
	case <-block.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the /admin delivery never reached the route predicate")
	}

	// The ingress event is consumed while the handler is parked: the window
	// elapses, the fire sees the expired session, and the core skips
	// silently — the event is gone.
	h.publishMessageSent(testAdmin)
	time.Sleep(quietElapse)
	if got := h.keyboardSends(); got != 1 {
		t.Fatalf("the fire against the expired session must be a silent skip, got %d keyboard sends", got)
	}
	if got := h.api.callCount("deleteMessage"); got != 0 {
		t.Fatalf("the skipped fire must not delete anything, got %d deletes", got)
	}

	// The handler finally runs and edits the buried menu in place.
	close(block.release)
	<-reopenDone

	if got := h.api.callCount("editMessageText"); got != 1 {
		t.Fatalf("expected the reopen to edit the buried menu in place, got %d edits", got)
	}

	// The reopen itself must resurface the menu at the bottom.
	h.waitForRecreate(t)

	time.Sleep(quietElapse)
	if got := h.keyboardSends(); got != 2 {
		t.Fatalf("exactly one re-creation expected after the delayed reopen, got %d keyboard sends", got)
	}
}

// TestMenuRecreateDrainsQueuedActivityBeforeHonoringExpiry pins the
// select-race fix: while the worker is parked inside one admin's re-creation,
// another admin's debounce expiry and newer activity can both queue up.
// select hands out one ready case at random, and honoring the expiry first
// re-creates before the queued activity's quiet window has elapsed — then the
// event schedules a second re-creation. Queued activity must re-anchor its
// window first, superseding the expiry by generation. The defect loses a coin
// flip per cycle, so the test runs enough cycles for a regression to surface
// with overwhelming probability while the fixed behavior is deterministic.
func TestMenuRecreateDrainsQueuedActivityBeforeHonoringExpiry(t *testing.T) {
	h := newHarnessWithAdmins(t, testAdmin, 8)

	h.open(t)                                // admin 7's menu
	h.deliver(t, privateUpdate(8, "/admin")) // admin 8's menu

	const cycles = 16
	for cycle := 1; cycle <= cycles; cycle++ {
		// Arm admin 7's window and let it get a firm head start before
		// admin 8's: the 5ms gap makes admin 7's timer fire strictly first,
		// so admin 7's re-creation is the one that parks on the gate —
		// regardless of AfterFunc scheduling.
		h.publishMessageSent(testAdmin)
		time.Sleep(5 * time.Millisecond)
		entered, release := h.api.armGate("sendMessage")
		h.publishMessageSent(8)

		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("admin 7's re-creation send never reached the API")
		}

		// While the worker is parked: admin 8's expiry queues (its timer
		// fires 5ms behind admin 7's), then newer admin 8 activity is
		// published and settles — the subscriber records its timestamp and
		// the event queues behind the expiry. Neither can be consumed until
		// the release.
		h.publishMessageSent(8)
		time.Sleep(quietElapse)

		// Released: the recorded activity belongs to a later window, so the
		// expiry re-anchors from its timestamp instead of re-creating under
		// it — one re-creation per admin. Honoring the expiry would send
		// twice for admin 8.
		close(release)

		want := cycle + 1
		h.waitFor(t, "both re-creations to settle", func() bool {
			return h.keyboardSendsFor(testAdmin) == want && h.keyboardSendsFor(8) == want
		})
		time.Sleep(quietElapse)
		if got := h.keyboardSendsFor(8); got != want {
			t.Fatalf("cycle %d: the expiry must not be honored before the queued activity's window elapsed: want %d keyboard sends for admin 8, got %d", cycle, want, got)
		}
		if got := h.keyboardSendsFor(testAdmin); got != want {
			t.Fatalf("cycle %d: admin 7 must cost one re-creation per cycle, want %d, got %d", cycle, want, got)
		}
	}
}

// TestMenuRecreateAdoptsFreshMenuBeforeCleanup pins the adoption fix: the
// fresh menu is already visible when its buried predecessor's best-effort
// delete starts, and callbacks on it are validated against the session's
// message ID — deferring adoption behind the delete bounced every press on
// the new menu as "no longer active" until the delete finished or the
// shared re-creation timeout expired.
func TestMenuRecreateAdoptsFreshMenuBeforeCleanup(t *testing.T) {
	h := newHarness(t)
	h.open(t) // the buried menu

	// Park the delete: the fresh send has landed, cleanup is in flight.
	entered, release := h.api.armGate("deleteMessage")
	h.publishMessageSent(testAdmin)

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the re-created menu's delete never reached the API")
	}

	// The session must already identify the fresh menu.
	fresh := int(h.api.lastSendMessageID())
	if got := h.sessionMsgID(t); got != fresh {
		t.Fatalf("session must adopt the fresh menu before cleanup: session %d, fresh menu %d", got, fresh)
	}

	// A press on the fresh menu while the delete is still parked must be
	// accepted (silent answer), not bounced as a stale menu. The callback is
	// answered before it is processed, so the acceptance is observable while
	// the delete parks — the render itself waits on the re-creation's render
	// gate and only completes after the release.
	session := h.manager.Session(testAdmin)
	seq, _, _, _ := session.Snapshot()

	callbackDone := make(chan struct{})
	go func() {
		defer close(callbackDone)
		h.deliver(t, callbackUpdate(testAdmin, fresh, core.EncodeAction(core.VerbCatalog, seq, "1")))
	}()

	h.waitFor(t, "callback answer while the delete parks", func() bool {
		return len(h.api.requestsOf("answerCallbackQuery")) == 1
	})
	if body := h.api.requestsOf("answerCallbackQuery")[0].Body; strings.Contains(body, "no longer active") {
		t.Fatalf("a press on the fresh menu must be accepted while cleanup is in flight, got %q", body)
	}

	close(release)
	<-callbackDone

	if got := h.api.callCount("deleteMessage"); got != 1 {
		t.Fatalf("expected exactly one buried-menu delete, got %d", got)
	}
	if got := h.sessionMsgID(t); got != fresh {
		t.Fatalf("session must stay on the fresh menu: session %d, fresh menu %d", got, fresh)
	}
}

// TestMenuRecreateResurfacesAfterDelayedCancel pins the /cancel fallback
// fix: with the session expired, the command's ingress event is consumed by
// a fire against the dead session while the /cancel handler sits behind the
// route middlewares, and Cancel then falls back to reopening the dashboard —
// editing the buried menu in place. Only the /admin branch used to wake the
// worker, so /cancel left the menu buried until another unrelated message
// arrived. The fallback must re-anchor the quiet window exactly like the
// /admin reopen.
func TestMenuRecreateResurfacesAfterDelayedCancel(t *testing.T) {
	h := newHarness(t)
	h.open(t)

	// Expire the session the command is about to fall back over.
	h.clock.Advance(core.SessionTTL + time.Minute)

	// Park the /cancel delivery inside the route predicate — no handler has
	// run, the expired session is still in place, and the default 10ms
	// window fires against it long before the handler gets to run.
	block := &commandBlock{entered: make(chan struct{}, 1), release: make(chan struct{})}
	h.commandBlock.Store(block)

	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		h.deliver(t, privateUpdate(testAdmin, "/cancel"))
	}()

	select {
	case <-block.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the /cancel delivery never reached the route predicate")
	}

	// The ingress event is consumed while the handler is parked: the fire
	// sees the expired session and the core skips silently — the event is
	// gone, and nothing else will publish.
	h.publishMessageSent(testAdmin)
	time.Sleep(quietElapse)
	if got := h.keyboardSends(); got != 1 {
		t.Fatalf("the fire against the expired session must be a silent skip, got %d keyboard sends", got)
	}
	if got := h.api.callCount("deleteMessage"); got != 0 {
		t.Fatalf("the skipped fire must not delete anything, got %d deletes", got)
	}

	// The handler finally runs: nothing pending to cancel, so Cancel falls
	// back to the dashboard reopen and edits the buried menu in place.
	close(block.release)
	<-cancelDone

	if got := h.api.callCount("editMessageText"); got != 1 {
		t.Fatalf("expected the cancellation fallback to edit the buried menu in place, got %d edits", got)
	}

	// The fallback itself must resurface the menu at the bottom.
	h.waitForRecreate(t)

	time.Sleep(quietElapse)
	if got := h.keyboardSends(); got != 2 {
		t.Fatalf("exactly one re-creation expected after the delayed /cancel, got %d keyboard sends", got)
	}
}

// TestMenuRecreateRearmsAfterGateSuppression pins the lost-wakeup fix: the
// fire's inputPending check passes a moment before an input callback commits
// its pending, and the re-creation then waits behind another render on the
// render gate — the core's own pending check runs at gate time and
// suppresses the send. That suppression is the authoritative state, and the
// input resolves through inline navigation, which publishes no chat
// activity: a dropped schedule would leave the menu buried forever, so the
// suppressed fire must re-arm instead.
func TestMenuRecreateRearmsAfterGateSuppression(t *testing.T) {
	h := newHarness(t)
	h.open(t)

	// Hold the render gate so the re-creation parks in front of it.
	releaseGate := make(chan struct{})
	gateHeld := make(chan struct{})
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		_ = h.manager.DeliverPendingInput(testAdmin, func() error {
			close(gateHeld)
			<-releaseGate

			return nil
		})
	}()

	select {
	case <-gateHeld:
	case <-time.After(2 * time.Second):
		t.Fatal("the render gate never became held")
	}

	// The fire lands against the held gate: the input has not committed yet,
	// so inputPending is false, the timer is deleted, and the re-creation
	// parks on the gate. The park is detected positively on the worker's
	// stack — no timing assumption, so dispatcher hop lag cannot re-order
	// the staging (an arrive-after-commit would take the inputPending re-arm
	// branch and pass for the wrong reason).
	h.publishMessageSent(testAdmin)
	waitForStackFrame(t, "the re-creation to park on the held render gate", coreRecreateFrame, true)

	// The input commits while the re-creation waits on the gate.
	if _, err := h.manager.Apply(testAdmin, 0, func(s *core.Session) error {
		s.Pending = &core.Pending{
			ID:        s.NextPendingID(),
			Kind:      core.PendingStringInput,
			ExpiresAt: h.clock.Now().Add(core.PendingTTL),
		}

		return nil
	}); err != nil {
		t.Fatalf("commit pending input: %v", err)
	}

	// Release: the re-creation reaches the gate, sees the pending input, and
	// is suppressed — the authoritative state at gate time. The worker must
	// have fully returned from the suppressed call before the input
	// resolves, or the resolution would race the gate-time check.
	close(releaseGate)
	<-holderDone
	waitForStackFrame(t, "the worker to leave the suppressed re-creation", coreRecreateFrame, false)

	// The input resolves through inline navigation: no MessageSent event is
	// published — the re-armed schedule is the only thing left to surface
	// the menu.
	if _, err := h.manager.Apply(testAdmin, 0, func(s *core.Session) error {
		s.Pending = nil

		return nil
	}); err != nil {
		t.Fatalf("resolve pending input: %v", err)
	}

	h.waitForRecreate(t)

	time.Sleep(quietElapse)
	if got := h.keyboardSends(); got != 2 {
		t.Fatalf("exactly one re-creation expected after the suppressed fire re-armed, got %d keyboard sends", got)
	}
}

// TestMenuRecreateStopCancelsInFlightSend pins the shutdown fix: the worker
// executes re-creations on its own loop goroutine, so stopping the watcher
// must cancel the in-flight menu IO instead of waiting out the send timeout —
// otherwise a stalled Telegram API holds dispatcher shutdown and the deferred
// tool/store cleanup past the deployment's stop grace (Docker's default 10s
// vs the 15s send timeout).
func TestMenuRecreateStopCancelsInFlightSend(t *testing.T) {
	h := newHarness(t)
	h.open(t)

	// Park the re-creation's send inside the fake API.
	entered, release := h.api.armGate("sendMessage")
	h.publishMessageSent(testAdmin)

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the re-creation send never reached the API")
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		h.stopRecreate()
	}()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("stopping the watcher must cancel the in-flight send, not wait out its timeout")
	}

	// The canceled send surfaces as a quiet abort, not a transport failure.
	time.Sleep(50 * time.Millisecond)
	if logs := h.logs.String(); strings.Contains(logs, "admin ui menu recreate failed") {
		t.Fatalf("a shutdown-aborted re-creation must not log a transport failure: %s", logs)
	}

	close(release)
}

// TestMenuRecreateParentCancelAbortsInFlightSend pins the shutdown-ordering
// fix: the watcher's stop function is only reachable after Bot.Run returns,
// and Bot.Run joins handlers first — a handler waiting on the render gate
// behind a stalled re-creation send cannot be interrupted by canceling its
// own context, so with the worker bound to context.Background() the stalled
// send holds the gate (and the handler join) for the whole send timeout,
// exceeding the container's stop grace. The worker's IO must abort through
// the runtime context alone, while stop() has not been called.
func TestMenuRecreateParentCancelAbortsInFlightSend(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	h := newHarness(t)
	h.stopRecreate() // the harness watcher is bound to Background; run our own
	stop := h.adapter.startWatching(parent, h.dispatcher.(execution.Subscriber), 10*time.Millisecond)
	t.Cleanup(stop)

	h.open(t)

	// Park the re-creation's send inside the fake API.
	entered, release := h.api.armGate("sendMessage")
	defer close(release)
	h.publishMessageSent(testAdmin)

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the re-creation send never reached the API")
	}

	// Shutdown begins: the runtime context is canceled while the handler
	// join is still pending — the watcher's stop function has NOT run.
	cancelParent()

	// The stalled send must abort through the parent cancellation alone:
	// the worker must leave the gated re-creation — freeing the gate for any
	// handler waiting on it — without the stop function being involved.
	waitForStackFrame(t, "the stalled send to abort through the parent cancellation", coreRecreateFrame, false)
}
