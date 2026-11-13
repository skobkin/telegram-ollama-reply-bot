package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/support/markdown"
	"telegram-ollama-reply-bot/internal/tooluse"

	tg "github.com/mymmrac/telego"
)

// previewServerState is the mutable state of the preview test Bot API
// server: every request is recorded, per-method failure budgets and rate
// limits are configurable, and sendMessage responses carry fresh message IDs.
type previewServerState struct {
	mu       sync.Mutex
	requests []capturedRequest
	failures map[string]int           // method path suffix -> remaining 400 responses
	rates    map[string]*rateLimit    // method path suffix -> remaining 429 responses
	descs    map[string]string        // method path suffix -> armed failure description
	delays   map[string]time.Duration // method path suffix -> response delay
	messageN int
}

type rateLimit struct {
	remaining  int
	retryAfter int
}

// newPreviewServerState returns a fresh recording server state.
func newPreviewServerState() *previewServerState {
	return &previewServerState{
		failures: make(map[string]int),
		rates:    make(map[string]*rateLimit),
		descs:    make(map[string]string),
		delays:   make(map[string]time.Duration),
	}
}

func (s *previewServerState) record(path string, body map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, capturedRequest{path: path, body: body})
}

// calls returns the recorded request paths for the named method suffix.
func (s *previewServerState) calls(method string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []map[string]any
	for _, r := range s.requests {
		if strings.HasSuffix(r.path, "/"+method) {
			out = append(out, r.body)
		}
	}

	return out
}

func (s *previewServerState) callCount(method string) int {
	return len(s.calls(method))
}

func (s *previewServerState) armFailure(method string, times int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[method] = times
}

// armError arms `times` 400 failures carrying a specific Bot API
// description, e.g. the "message is not modified" no-op rejection.
func (s *previewServerState) armError(method string, times int, description string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[method] = times
	s.descs[method] = description
}

// takeFailureDescription returns the description armed for the method,
// falling back to the generic failure text.
func (s *previewServerState) takeFailureDescription(method string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if d := s.descs[method]; d != "" {
		return d
	}

	return "test failure"
}

// delayMethod makes every response for the method wait d before answering,
// so tests can hold a wire call in flight.
func (s *previewServerState) delayMethod(method string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delays[method] = d
}

func (s *previewServerState) delayFor(method string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.delays[method]
}

func (s *previewServerState) armRateLimit(method string, times, retryAfter int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rates[method] = &rateLimit{remaining: times, retryAfter: retryAfter}
}

func (s *previewServerState) takeRateLimit(method string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spec := s.rates[method]
	if spec == nil || spec.remaining <= 0 {
		return 0, false
	}
	spec.remaining--

	return spec.retryAfter, true
}

func (s *previewServerState) takeFailure(method string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures[method] <= 0 {
		return false
	}
	s.failures[method]--

	return true
}

func (s *previewServerState) nextMessageID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messageN++

	return s.messageN
}

// newPreviewTestBot wires a Bot to a recording Bot API server with preview
// failures/rate limits controlled by the shared state.
func newPreviewTestBot(t *testing.T, state *previewServerState) *Bot {
	t.Helper()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Errorf("cannot decode request body: %v", err)
		}
		state.record(r.URL.Path, decoded)

		w.Header().Set("Content-Type", "application/json")

		if retryAfter, limited := state.takeRateLimit(methodSuffix(r.URL.Path)); limited {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":          false,
				"error_code":  429,
				"description": "Too Many Requests: retry after " + strconv.Itoa(retryAfter),
				"parameters":  map[string]any{"retry_after": retryAfter},
			})

			return
		}
		if state.takeFailure(methodSuffix(r.URL.Path)) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "error_code": 400,
				"description": state.takeFailureDescription(methodSuffix(r.URL.Path)),
			})

			return
		}
		if d := state.delayFor(methodSuffix(r.URL.Path)); d > 0 {
			time.Sleep(d)
		}

		result := map[string]any{"date": 1, "chat": map[string]any{"id": 1, "type": "private"}}
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"), strings.HasSuffix(r.URL.Path, "/editMessageText"),
			strings.HasSuffix(r.URL.Path, "/sendRichMessage"):
			result["message_id"] = state.nextMessageID()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	t.Cleanup(ts.Close)

	api, err := tg.NewBot("1:"+strings.Repeat("A", 35), tg.WithAPIServer(ts.URL), tg.WithDiscardLogger())
	if err != nil {
		t.Fatalf("cannot create test bot: %v", err)
	}

	return &Bot{
		api:       api,
		sanitizer: markdown.NewTgMarkdownV2Sanitizer(),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func methodSuffix(path string) string {
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}

	return path
}

// newPreviewTestManager builds a manager over the test bot with shortened
// timings so suites stay fast.
func newPreviewTestManager(t *testing.T, b *Bot) *previewManager {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	m := newPreviewManager(ctx, b.api, b.logger, b.sendTypingUntil)
	m.timings = previewTimings{
		debounce:     30 * time.Millisecond,
		minInterval:  40 * time.Millisecond,
		flushFloor:   15 * time.Millisecond,
		draftRefresh: 120 * time.Millisecond,
		ratePad:      10 * time.Millisecond,
	}
	t.Cleanup(func() {
		m.Stop()
		cancel()
	})

	return m
}

// waitForCondition polls cond until it holds or the deadline passes.
func waitForCondition(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met in %s: %s", timeout, what)
}

func groupRunInfo(mode adminconfig.PreviewMode) RunInfo {
	return RunInfo{
		RunID:            execution.NewRunID(),
		ChatID:           -100900700600,
		TopicID:          77,
		TriggerMessageID: 4242,
		ChatType:         tg.ChatTypeSupergroup,
		Mode:             mode,
	}
}

func toolDone(run *previewRun, name string, internal bool, ok bool, d time.Duration) {
	status := execution.StatusOK
	if !ok {
		status = execution.StatusError
	}
	run.applyEvent(execution.ToolStarted{RunContext: execution.RunContext{RunID: run.ID()}, CallID: "c-" + name, ToolName: name, Internal: internal})
	run.applyEvent(execution.ToolCompleted{RunContext: execution.RunContext{RunID: run.ID()}, CallID: "c-" + name, ToolName: name, Internal: internal, Duration: d, Status: status})
}

func TestPreviewManagerRunForInactive(t *testing.T) {
	var nilManager *previewManager
	if run := nilManager.RunFor(groupRunInfo(adminconfig.PreviewModeTools)); run != nil {
		t.Fatal("nil manager must not start runs")
	}
	nilManager.HandleExecutionEvent(context.Background(), execution.RunStarted{RunContext: execution.RunContext{RunID: "x"}})
	nilManager.Stop()

	var nilRun *previewRun
	nilRun.FinalDelivered()
	nilRun.CompleteNoText(nil)
	nilRun.Abort()
	nilRun.Close()
	if nilRun.ID() != "" {
		t.Fatal("nil run ID must be empty")
	}

	m := newPreviewTestManager(t, newPreviewTestBot(t, newPreviewServerState()))
	if run := m.RunFor(groupRunInfo(adminconfig.PreviewModeOff)); run != nil {
		t.Fatal("mode off must not start runs")
	}
	m.Stop()
	if run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools)); run != nil {
		t.Fatal("stopped manager must not start runs")
	}
	m.Stop() // idempotent
}

func TestPreviewNilRunLifecycleNoops(t *testing.T) {
	var run *previewRun
	run.FinalDelivered()
	run.CompleteNoText(nil)
	run.AwaitChatGate(context.Background())
	run.Abort()
	run.Close()
	if run.ID() != "" {
		t.Fatal("nil run must carry no ID")
	}
}

func TestGroupPreviewDebounceSilencesShortRuns(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	toolDone(run, "get_current_time", false, true, 20*time.Millisecond)
	run.FinalDelivered()

	time.Sleep(3 * m.timings.debounce)
	if n := state.callCount("sendMessage"); n != 0 {
		t.Fatalf("run finished inside the debounce window must not create a preview, got %d sends", n)
	}
}

func TestGroupPreviewCreateReplyEditDelete(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	toolDone(run, "search_web", false, true, time.Second)
	run.applyEvent(execution.ToolStarted{RunContext: execution.RunContext{RunID: run.ID()}, CallID: "c2", ToolName: "fetch_url_content"})

	waitForCondition(t, 2*time.Second, "preview creation", func() bool {
		return state.callCount("sendMessage") == 1
	})

	create := state.calls("sendMessage")[0]
	if create["disable_notification"] != true {
		t.Fatalf("group preview must be silent: %v", create)
	}
	if create["message_thread_id"] != float64(77) {
		t.Fatalf("group preview must carry the topic id: %v", create)
	}
	reply := create["reply_parameters"].(map[string]any)
	if reply["message_id"] != float64(4242) {
		t.Fatalf("group preview must reply to the trigger message: %v", reply)
	}
	if !strings.Contains(create["text"].(string), "✅ search_web()") {
		t.Fatalf("preview text must list the completed tool: %q", create["text"])
	}
	if !strings.Contains(create["text"].(string), "🔄 fetch_url_content()") {
		t.Fatalf("preview text must show the running tool: %q", create["text"])
	}

	// No delete before final delivery.
	if n := state.callCount("deleteMessage"); n != 0 {
		t.Fatalf("preview must not be deleted before delivery: %d deletes", n)
	}

	// A state change after creation becomes an edit of the same message.
	run.applyEvent(execution.ToolCompleted{RunContext: execution.RunContext{RunID: run.ID()}, CallID: "c2", ToolName: "fetch_url_content", Duration: 2 * time.Second, Status: execution.StatusOK})
	waitForCondition(t, 2*time.Second, "preview edit", func() bool {
		return state.callCount("editMessageText") >= 1
	})

	edit := state.calls("editMessageText")[0]
	// The server assigns the first preview message ID 1.
	if edit["message_id"] != float64(1) {
		t.Fatalf("edit must target the created preview message: %v", edit["message_id"])
	}
	if n := state.callCount("sendMessage"); n != 1 {
		t.Fatalf("preview must be created exactly once, got %d sends", n)
	}

	// Internal tools stay hidden in tools mode.
	if strings.Contains(edit["text"].(string), "get_conversation_summary") {
		t.Fatalf("internal tools must be hidden in tools mode: %q", edit["text"])
	}

	run.FinalDelivered()
	waitForCondition(t, 2*time.Second, "preview delete", func() bool {
		return state.callCount("deleteMessage") == 1
	})
	del := state.calls("deleteMessage")[0]
	if del["message_id"] != float64(1) {
		t.Fatalf("delete must target the preview message: %v", del)
	}
}

func TestGroupPreviewModeFiltering(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	info := groupRunInfo(adminconfig.PreviewModeAllTools)
	run := m.RunFor(info)
	toolDone(run, "search_web", false, true, time.Second)
	toolDone(run, "get_conversation_summary", true, true, 1500*time.Millisecond)
	toolDone(run, "fetch_url_content", false, false, 800*time.Millisecond)

	waitForCondition(t, 2*time.Second, "all_tools preview", func() bool {
		return state.callCount("sendMessage") == 1
	})

	text := state.calls("sendMessage")[0]["text"].(string)
	for _, want := range []string{
		"✅ search_web() ⌛ 1s",
		"✅ get_conversation_summary() ⌛ 1.5s",
		"❌ fetch_url_content() ⌛ 0.8s",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("all_tools preview must contain %q: %q", want, text)
		}
	}
}

func TestGroupPreviewLineCompaction(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	for i := 0; i < 25; i++ {
		toolDone(run, "tool_"+strconv.Itoa(i), false, true, 10*time.Millisecond)
	}

	waitForCondition(t, 2*time.Second, "compacted preview", func() bool {
		return state.callCount("sendMessage") == 1
	})

	text := state.calls("sendMessage")[0]["text"].(string)
	if !strings.Contains(text, "… and 5 earlier") {
		t.Fatalf("compaction line missing: %q", text)
	}
	if !strings.Contains(text, "tool_24()") || strings.Contains(text, "tool_4()") {
		t.Fatalf("compaction must keep the newest lines: %q", text)
	}
}

func TestGroupPreviewAbortPreservesPreview(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	toolDone(run, "search_web", false, true, time.Second)
	waitForCondition(t, 2*time.Second, "preview creation", func() bool {
		return state.callCount("sendMessage") == 1
	})

	run.Abort()
	time.Sleep(2 * m.timings.debounce)
	if n := state.callCount("deleteMessage"); n != 0 {
		t.Fatalf("abort must preserve the preview: %d deletes", n)
	}

	m.mu.Lock()
	remaining := len(m.runs)
	m.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("aborted run must leave no manager state, got %d runs", remaining)
	}
}

func TestPreviewRateLimitBacksOffWholeChat(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	// The first preview wire call in the chat is rate limited.
	state.armRateLimit("sendMessage", 1, 1)

	runA := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	toolDone(runA, "search_web", false, true, time.Second)
	waitForCondition(t, 2*time.Second, "rate-limited attempt", func() bool {
		return state.callCount("sendMessage") >= 1
	})

	// A concurrent run in the same chat starts while the gate is closed.
	infoB := groupRunInfo(adminconfig.PreviewModeTools)
	infoB.TriggerMessageID = 4243
	runB := m.RunFor(infoB)
	toolDone(runB, "fetch_url_content", false, true, time.Second)

	// Both runs share the gate: after run A's first (rate-limited) attempt,
	// no further wire call may happen until the gate opens (~1s).
	time.Sleep(150 * time.Millisecond)
	if n := state.callCount("sendMessage"); n != 1 {
		t.Fatalf("runs must back off while the chat gate is closed, got %d sends", n)
	}

	// After the gate opens both deliver their previews.
	waitForCondition(t, 3*time.Second, "both previews delivered", func() bool {
		return state.callCount("sendMessage") >= 3 // first attempt + A retry + B
	})

	runA.FinalDelivered()
	runB.FinalDelivered()
}

func TestGroupPreviewDisablesAfterRepeatedFailures(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	state.armFailure("editMessageText", 100)

	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	toolDone(run, "search_web", false, true, time.Second)
	waitForCondition(t, 2*time.Second, "preview creation", func() bool {
		return state.callCount("sendMessage") == 1
	})

	// Every edit fails: after previewMaxFailures the run disables updates
	// and the worker exits (state unregistered).
	toolDone(run, "fetch_url_content", false, true, time.Second)
	waitForCondition(t, 3*time.Second, "run unregistered after disable", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()

		return len(m.runs) == 0
	})

	edits := state.callCount("editMessageText")
	if edits == 0 || edits > previewMaxFailures {
		t.Fatalf("run must stop after %d failed edits, got %d", previewMaxFailures, edits)
	}

	time.Sleep(2 * m.timings.minInterval)
	if state.callCount("editMessageText") != edits {
		t.Fatal("disabled run must stop editing")
	}

	// The lifecycle safety net still deletes the stale preview.
	run.FinalDelivered()
	waitForCondition(t, 2*time.Second, "stale preview deleted", func() bool {
		return state.callCount("deleteMessage") == 1
	})
}

func TestPrivateDraftStableIDAndRefresh(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	info := RunInfo{
		RunID:            execution.NewRunID(),
		ChatID:           900800700,
		TriggerMessageID: 55,
		ChatType:         tg.ChatTypePrivate,
		Mode:             adminconfig.PreviewModeAllTools,
	}
	run := m.RunFor(info)
	toolDone(run, "search_web", false, true, 1200*time.Millisecond)

	// The draft lifetime is refreshed by re-sending with the same ID.
	waitForCondition(t, 3*time.Second, "draft refresh", func() bool {
		return state.callCount("sendRichMessageDraft") >= 3
	})

	drafts := state.calls("sendRichMessageDraft")
	draftID := drafts[0]["draft_id"].(float64)
	if draftID == 0 {
		t.Fatal("draft id must be non-zero")
	}
	for i, draft := range drafts {
		if draft["draft_id"].(float64) != draftID {
			t.Fatalf("draft %d must reuse the same id: %v != %v", i, draft["draft_id"], draftID)
		}
	}

	text := drafts[0]["rich_message"].(map[string]any)["markdown"].(string)
	if !strings.Contains(text, "✅ `search_web()` ⌛ 1.2s") {
		t.Fatalf("draft must render the tool with duration: %q", text)
	}
}

func TestPrivateDraftEscapesAdversarialToolName(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	info := RunInfo{
		RunID:    execution.NewRunID(),
		ChatID:   900800701,
		ChatType: tg.ChatTypePrivate,
		Mode:     adminconfig.PreviewModeTools,
	}
	run := m.RunFor(info)
	toolDone(run, "ev`il_na me", false, true, time.Second)

	waitForCondition(t, 2*time.Second, "draft with escaped name", func() bool {
		return state.callCount("sendRichMessageDraft") >= 1
	})

	text := state.calls("sendRichMessageDraft")[0]["rich_message"].(map[string]any)["markdown"].(string)
	want := "✅ " + escapeRichMarkdown("ev`il_na me") + "()"
	if !strings.Contains(text, want) {
		t.Fatalf("draft must escape the adversarial name (%q): %q", want, text)
	}
	if strings.Contains(text, "`ev`il_na me()`") {
		t.Fatalf("draft must not build a breakable code span: %q", text)
	}
}

func TestPrivateCompleteNoTextSendsFinalDraft(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	info := RunInfo{
		RunID:    execution.NewRunID(),
		ChatID:   900800702,
		ChatType: tg.ChatTypePrivate,
		Mode:     adminconfig.PreviewModeTools,
	}
	run := m.RunFor(info)
	toolDone(run, "send_dice", false, true, 300*time.Millisecond)
	waitForCondition(t, 2*time.Second, "first draft", func() bool {
		return state.callCount("sendRichMessageDraft") >= 1
	})

	run.CompleteNoText([]tooluse.ToolCallRecord{{
		CallID:     "c1",
		Name:       "send_dice",
		Visibility: tooluse.VisibilityStandard,
		Status:     execution.StatusOK,
		Duration:   300 * time.Millisecond,
	}})
	waitForCondition(t, 2*time.Second, "final done draft", func() bool {
		return state.callCount("sendRichMessageDraft") >= 2
	})

	last := state.calls("sendRichMessageDraft")[1]
	text := last["rich_message"].(map[string]any)["markdown"].(string)
	if !strings.Contains(text, "✔️ Done") || strings.Contains(text, "🔄") {
		t.Fatalf("final draft must drop the active marker: %q", text)
	}
	if !strings.Contains(text, "send_dice") {
		t.Fatalf("final draft must render authoritative records, got: %q", text)
	}
}

func TestPrivateDraftFailureFallsBackToTyping(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	state.armFailure("sendRichMessageDraft", 100)

	info := RunInfo{
		RunID:    execution.NewRunID(),
		ChatID:   900800703,
		ChatType: tg.ChatTypePrivate,
		Mode:     adminconfig.PreviewModeTools,
	}
	run := m.RunFor(info)
	toolDone(run, "search_web", false, true, time.Second)

	// The first failed draft switches to the typing indicator.
	waitForCondition(t, 2*time.Second, "typing fallback", func() bool {
		return state.callCount("sendChatAction") >= 1
	})

	// No more draft attempts after the disable; the worker exits and the
	// manager state is cleaned up.
	time.Sleep(2 * m.timings.minInterval)
	if drafts := state.callCount("sendRichMessageDraft"); drafts != 1 {
		t.Fatalf("drafts must stop after the first failure, got %d", drafts)
	}
	m.mu.Lock()
	remaining := len(m.runs)
	m.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("disabled run must leave no manager state, got %d runs", remaining)
	}

	run.FinalDelivered() // private: nothing beyond finalization
}

func TestFinalizedAndUnknownRunsIgnoreEvents(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	rc := execution.RunContext{RunID: "nonexistent", ChatID: -1}
	m.HandleExecutionEvent(context.Background(), execution.ToolCompleted{RunContext: rc, CallID: "x", ToolName: "t", Status: execution.StatusOK})

	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	run.finalize()
	m.HandleExecutionEvent(context.Background(), execution.ToolCompleted{RunContext: execution.RunContext{RunID: run.ID()}, CallID: "y", ToolName: "t2", Status: execution.StatusOK})
	run.mu.Lock()
	lines := len(run.completed)
	run.mu.Unlock()
	if lines != 0 {
		t.Fatalf("finalized run must ignore events, got %d tool lines", lines)
	}
}

func TestPreviewManagerStateHygiene(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	baseline := runtime.NumGoroutine()

	// Repeated runs across many chats, each finalized a different way.
	for round := 0; round < 3; round++ {
		for chat := 0; chat < 10; chat++ {
			info := groupRunInfo(adminconfig.PreviewModeTools)
			info.ChatID = -100900000000 - int64(chat)
			run := m.RunFor(info)
			toolDone(run, "search_web", false, true, 10*time.Millisecond)

			switch (round + chat) % 3 {
			case 0:
				run.FinalDelivered()
			case 1:
				run.Abort()
			case 2:
				run.CompleteNoText(nil)
			}
		}
	}

	waitForCondition(t, 3*time.Second, "manager state drained", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()

		return len(m.runs) == 0 && len(m.chatGates) == 0
	})

	waitForCondition(t, 3*time.Second, "goroutines back to baseline", func() bool {
		return runtime.NumGoroutine() <= baseline+2
	})
}

func TestPreviewStopClearsStateAndStopsWorkers(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	for chat := 0; chat < 5; chat++ {
		info := groupRunInfo(adminconfig.PreviewModeTools)
		info.ChatID = -100910000000 - int64(chat)
		run := m.RunFor(info)
		toolDone(run, "search_web", false, true, 10*time.Millisecond)
	}

	m.Stop()
	m.Stop() // idempotent

	m.mu.Lock()
	runs, gates, msgs := len(m.runs), len(m.chatGates), len(m.previewMsgs)
	m.mu.Unlock()
	if runs != 0 || gates != 0 || msgs != 0 {
		t.Fatalf("stop must clear manager state, got %d runs / %d gates / %d preview msg chats", runs, gates, msgs)
	}
}

func TestFormatShortDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{200 * time.Millisecond, "0.2s"},
		{1200 * time.Millisecond, "1.2s"},
		{5 * time.Second, "5s"},
		{80 * time.Second, "1m20s"},
		{0, "0.0s"},
	}
	for _, tt := range tests {
		if got := formatShortDuration(tt.in); got != tt.want {
			t.Fatalf("formatShortDuration(%s) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestEscapeRichMarkdown(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"search_web", "search\\_web"},
		{"plain", "plain"},
		{"a*b* c[d]", "a\\*b\\* c\\[d\\]"},
		{"back\\slash", "back\\\\slash"},
		{"ev`il", "ev\\`il"},
	}
	for _, tt := range tests {
		if got := escapeRichMarkdown(tt.in); got != tt.want {
			t.Fatalf("escapeRichMarkdown(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPreviewToolNameRendering(t *testing.T) {
	if got := previewToolName("search_web", true); got != "`search_web()`" {
		t.Fatalf("plain name must be a code span: %q", got)
	}
	if got := previewToolName("ev`il", true); got != escapeRichMarkdown("ev`il")+"()" {
		t.Fatalf("backticked name must be escaped, got %q", got)
	}
	if got := previewToolName("search_web", false); got != "search_web()" {
		t.Fatalf("plain-text rendering must not escape: %q", got)
	}
}

// TestGroupPreviewSkipsIdenticalEdits pins that preview states which render
// identically to the delivered one (internal tools hidden by the mode, no-op
// generation events) never reach the wire as edits: Telegram would reject
// them as "message is not modified" and the retries would disable a healthy
// preview.
func TestGroupPreviewSkipsIdenticalEdits(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))

	run.applyEvent(execution.ToolStarted{RunContext: execution.RunContext{RunID: run.ID()}, CallID: "c1", ToolName: "get_conversation_summary", Internal: true})
	run.applyEvent(execution.ToolCompleted{RunContext: execution.RunContext{RunID: run.ID()}, CallID: "c1", ToolName: "get_conversation_summary", Internal: true, Duration: 300 * time.Millisecond, Status: execution.StatusOK})
	waitForCondition(t, 2*time.Second, "preview created", func() bool {
		return state.callCount("sendMessage") == 1
	})

	// Generation-only churn renders identically to the created preview.
	run.applyEvent(execution.GenerationStarted{RunContext: execution.RunContext{RunID: run.ID()}, Iteration: 1})
	run.applyEvent(execution.GenerationCompleted{RunContext: execution.RunContext{RunID: run.ID()}, Iteration: 1})
	time.Sleep(4 * m.timings.minInterval)
	if edits := state.callCount("editMessageText"); edits != 0 {
		t.Fatalf("identical preview state must not produce edits, got %d", edits)
	}

	// A visible change still goes out as an edit.
	toolDone(run, "search_web", false, true, time.Second)
	waitForCondition(t, 2*time.Second, "visible change edited", func() bool {
		return state.callCount("editMessageText") >= 1
	})
	edit := state.calls("editMessageText")[0]
	if !strings.Contains(edit["text"].(string), "✅ search_web()") {
		t.Fatalf("edit must carry the visible tool: %q", edit["text"])
	}

	run.FinalDelivered()
}

// TestGroupPreviewNotModifiedIsNoOp pins that the "message is not modified"
// rejection is absorbed as a successful no-op: it must not count against
// the run's failure budget or disable the preview.
func TestGroupPreviewNotModifiedIsNoOp(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	state.armError("editMessageText", 2, "Bad Request: message is not modified")

	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	toolDone(run, "search_web", false, true, time.Second)
	waitForCondition(t, 2*time.Second, "preview created", func() bool {
		return state.callCount("sendMessage") == 1
	})

	// Two visible changes are each rejected as not-modified on the wire.
	toolDone(run, "fetch_url_content", false, true, time.Second)
	waitForCondition(t, 2*time.Second, "first not-modified edit", func() bool {
		return state.callCount("editMessageText") >= 1
	})
	toolDone(run, "send_dice", false, true, 100*time.Millisecond)
	waitForCondition(t, 2*time.Second, "second not-modified edit", func() bool {
		return state.callCount("editMessageText") >= 2
	})

	// The run is still healthy: a further change reaches the wire and the
	// preview is still deleted on final delivery.
	toolDone(run, "get_current_time", false, true, 10*time.Millisecond)
	waitForCondition(t, 2*time.Second, "healthy run keeps editing", func() bool {
		return state.callCount("editMessageText") >= 3
	})
	third := state.calls("editMessageText")[2]
	if !strings.Contains(third["text"].(string), "✅ get_current_time()") {
		t.Fatalf("third edit must carry the newest tool: %q", third["text"])
	}

	run.FinalDelivered()
	waitForCondition(t, 2*time.Second, "preview deleted", func() bool {
		return state.callCount("deleteMessage") == 1
	})
}

// TestPreviewNeutralIndicatorCoversSilentPreprocessing pins that the
// neutral preview is delivered at the debounce even before the first
// execution event fires — preprocessing (history summarization) publishes
// no events.
func TestPreviewNeutralIndicatorCoversSilentPreprocessing(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)

	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	waitForCondition(t, 2*time.Second, "neutral group preview", func() bool {
		return state.callCount("sendMessage") == 1
	})
	if text := state.calls("sendMessage")[0]["text"]; text != "🔄 Working…" {
		t.Fatalf("neutral group preview text unexpected: %v", text)
	}
	run.FinalDelivered()

	pstate := newPreviewServerState()
	pb := newPreviewTestBot(t, pstate)
	pm := newPreviewTestManager(t, pb)
	info := RunInfo{
		RunID:    execution.NewRunID(),
		ChatID:   900800711,
		ChatType: tg.ChatTypePrivate,
		Mode:     adminconfig.PreviewModeTools,
	}
	prun := pm.RunFor(info)
	waitForCondition(t, 2*time.Second, "neutral private draft", func() bool {
		return pstate.callCount("sendRichMessageDraft") == 1
	})
	text := pstate.calls("sendRichMessageDraft")[0]["rich_message"].(map[string]any)["markdown"].(string)
	if !strings.HasPrefix(text, "🔄 Working…") {
		t.Fatalf("neutral draft must not wait for the refresh deadline: %q", text)
	}
	prun.CompleteNoText(nil)
}

// TestGroupPreviewFinalDeliveredWaitsForInFlightCreate pins the finalization
// join bound: FinalDelivered waits past an in-flight preview creation (the
// join exceeds the wire-call timeout), so the post-delivery delete targets
// the created message instead of abandoning it.
func TestGroupPreviewFinalDeliveredWaitsForInFlightCreate(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	state.delayMethod("sendMessage", 400*time.Millisecond)

	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	waitForCondition(t, 2*time.Second, "create in flight", func() bool {
		return state.callCount("sendMessage") == 1
	})

	run.FinalDelivered()
	waitForCondition(t, 2*time.Second, "preview deleted after delivery", func() bool {
		return state.callCount("deleteMessage") == 1
	})
	if n := state.callCount("deleteMessage"); n != 1 {
		t.Fatalf("preview must be deleted exactly once, got %d", n)
	}
}

// TestGroupPreviewLateCreateCleansUpAfterFinalization pins the defense for
// a creation that lands after the lifecycle delete found no message ID:
// the late preview removes itself instead of outliving the answer.
func TestGroupPreviewLateCreateCleansUpAfterFinalization(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))

	// The lifecycle delete ran while creation was still in flight and
	// latched with nothing to remove.
	run.mu.Lock()
	run.deleted = true
	run.mu.Unlock()

	if !run.sendUpdate(context.Background(), "🔄 Working…") {
		t.Fatal("late create must succeed")
	}
	waitForCondition(t, 2*time.Second, "late preview deleted", func() bool {
		return state.callCount("deleteMessage") == 1
	})
	if n := state.callCount("sendMessage"); n != 1 {
		t.Fatalf("late create must create the preview once, got %d", n)
	}
	if del := state.calls("deleteMessage")[0]; del["message_id"] != float64(1) {
		t.Fatalf("late delete must target the created preview: %v", del)
	}
}

// TestPreviewQuiesceStopsPrivateDraftsBeforeFinalSend pins that quiescing
// before the final send stops all draft writes: a straggler draft can no
// longer land after the answer and resurrect the working preview.
func TestPreviewQuiesceStopsPrivateDraftsBeforeFinalSend(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	info := RunInfo{
		RunID:    execution.NewRunID(),
		ChatID:   900800710,
		ChatType: tg.ChatTypePrivate,
		Mode:     adminconfig.PreviewModeTools,
	}
	run := m.RunFor(info)
	toolDone(run, "search_web", false, true, time.Second)
	waitForCondition(t, 2*time.Second, "first draft", func() bool {
		return state.callCount("sendRichMessageDraft") >= 1
	})

	run.Quiesce()
	draftsAtQuiesce := state.callCount("sendRichMessageDraft")

	// Events after the quiesce are dropped and no further draft may hit
	// the wire.
	toolDone(run, "fetch_url_content", false, true, time.Second)
	time.Sleep(4 * m.timings.minInterval)
	if n := state.callCount("sendRichMessageDraft"); n != draftsAtQuiesce {
		t.Fatalf("quiesce must stop draft writes, %d -> %d", draftsAtQuiesce, n)
	}

	m.mu.Lock()
	remaining := len(m.runs)
	m.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("quiesced run must leave no manager state, got %d runs", remaining)
	}

	run.FinalDelivered() // idempotent; private: no delete
}

// TestPreviewQuiesceLeavesGroupRunsToFinalDelivery pins that quiescing is a
// no-op for group runs: they keep editing until final delivery deletes the
// preview.
func TestPreviewQuiesceLeavesGroupRunsToFinalDelivery(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	toolDone(run, "search_web", false, true, time.Second)
	waitForCondition(t, 2*time.Second, "preview created", func() bool {
		return state.callCount("sendMessage") == 1
	})

	run.Quiesce() // group runs are not quiesced
	toolDone(run, "fetch_url_content", false, true, time.Second)
	waitForCondition(t, 2*time.Second, "group run still editing", func() bool {
		return state.callCount("editMessageText") >= 1
	})

	run.FinalDelivered()
	waitForCondition(t, 2*time.Second, "preview deleted", func() bool {
		return state.callCount("deleteMessage") == 1
	})
}

// TestPreviewBackoffSurvivesRunExit pins that an unexpired chat backoff
// survives the chat's last run exiting: a new run in the same chat must
// wait out the outstanding retry window instead of drawing a fresh 429.
func TestPreviewBackoffSurvivesRunExit(t *testing.T) {
	const chatID = int64(-100900700600)

	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	state.armRateLimit("sendMessage", 1, 1) // 1s retry_after + rate pad

	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	toolDone(run, "search_web", false, true, time.Second)
	waitForCondition(t, 2*time.Second, "rate-limited attempt", func() bool {
		return state.callCount("sendMessage") >= 1
	})
	run.Abort()

	m.mu.Lock()
	gate := m.chatGates[chatID]
	runs := 0
	if gate != nil {
		runs = gate.runs
	}
	m.mu.Unlock()
	if gate == nil || !gate.earliest.After(time.Now()) || runs != 0 {
		t.Fatalf("unexpired backoff must survive the run exit: gate=%v runs=%d", gate, runs)
	}

	// A new run in the same chat respects the surviving backoff.
	next := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	toolDone(next, "fetch_url_content", false, true, time.Second)
	time.Sleep(300 * time.Millisecond)
	if n := state.callCount("sendMessage"); n != 1 {
		t.Fatalf("new run must not send inside the outstanding retry window, got %d sends", n)
	}
	waitForCondition(t, 3*time.Second, "preview after gate opens", func() bool {
		return state.callCount("sendMessage") >= 2
	})

	next.FinalDelivered()
	m.mu.Lock()
	gates := len(m.chatGates)
	m.mu.Unlock()
	if gates != 0 {
		t.Fatalf("expired backoff must be dropped, got %d gates", gates)
	}
}

// TestPreviewFailureLogCarriesNoTransportURL pins that transport failures
// (whose errors quote the token-bearing request URL) reach the log only in
// their sanitized, typed form.
func TestPreviewFailureLogCarriesNoTransportURL(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)

	var buf safeBuffer
	m.logger = slog.New(slog.NewTextHandler(&buf, nil))

	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	tokenURL := "https://api.telegram.org/bot1:" + strings.Repeat("A", 35) + "/sendMessage"
	transport := &url.Error{Op: "Post", URL: tokenURL, Err: errors.New("connection refused")}
	run.logSendFailure("sendMessage", transport)

	out := buf.String()
	if strings.Contains(out, "api.telegram.org") || strings.Contains(out, strings.Repeat("A", 35)) {
		t.Fatalf("transport URL must not reach the log: %s", out)
	}
	if !strings.Contains(out, "preview wire call failed") {
		t.Fatalf("typed sentinel missing from log: %s", out)
	}
}

// safeBuffer is a concurrency-safe bytes.Buffer for log capture.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// TestPrivateCompleteNoTextRendersFromAuthoritativeRecords pins that the
// terminal no-text draft is rendered from the run's authoritative tool
// records: the dispatcher may still hold an un-applied ToolCompleted when
// finalization lands, and that late event must neither leave a
// forever-running line in the final draft nor resurrect the terminated
// preview with a refresh.
func TestPrivateCompleteNoTextRendersFromAuthoritativeRecords(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)

	info := RunInfo{
		RunID:    execution.NewRunID(),
		ChatID:   900800733,
		ChatType: tg.ChatTypePrivate,
		Mode:     adminconfig.PreviewModeTools,
	}
	run := m.RunFor(info)

	// Only the start is applied: the completion is still in flight when
	// finalization lands.
	run.applyEvent(execution.ToolStarted{RunContext: execution.RunContext{RunID: run.ID()}, CallID: "c1", ToolName: "send_dice", Internal: false})
	waitForCondition(t, 2*time.Second, "working draft", func() bool {
		return state.callCount("sendRichMessageDraft") >= 1
	})

	run.CompleteNoText([]tooluse.ToolCallRecord{{
		CallID:     "c1",
		Name:       "send_dice",
		Visibility: tooluse.VisibilityStandard,
		Status:     execution.StatusOK,
		Duration:   300 * time.Millisecond,
	}})
	waitForCondition(t, 2*time.Second, "final done draft", func() bool {
		return state.callCount("sendRichMessageDraft") >= 2
	})

	// The late completion is dropped by the finalized run and must not
	// trigger another draft refresh.
	run.applyEvent(execution.ToolCompleted{RunContext: execution.RunContext{RunID: run.ID()}, CallID: "c1", ToolName: "send_dice", Internal: false, Duration: 300 * time.Millisecond, Status: execution.StatusOK})
	time.Sleep(3 * m.timings.draftRefresh)
	if n := state.callCount("sendRichMessageDraft"); n != 2 {
		t.Fatalf("terminated run must not refresh the final draft, got %d drafts", n)
	}

	text := state.calls("sendRichMessageDraft")[1]["rich_message"].(map[string]any)["markdown"].(string)
	if strings.Contains(text, "🔄") {
		t.Fatalf("final draft must not trust stale event state, got: %q", text)
	}
	if !strings.Contains(text, "send_dice") {
		t.Fatalf("final draft must render the authoritative record, got: %q", text)
	}
}

// TestGroupPreviewFailureCounterResetsOnSuccess pins that previewMaxFailures
// counts consecutive failures: a successful edit refunds the budget, so
// isolated wire blips interleaved with recoveries never disable the run.
func TestGroupPreviewFailureCounterResetsOnSuccess(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)

	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	toolDone(run, "search_web", false, true, 10*time.Millisecond)
	waitForCondition(t, 2*time.Second, "preview created", func() bool {
		return state.callCount("sendMessage") >= 1
	})

	// Three failing edits, each followed by a recovery: the lifetime count
	// exceeds previewMaxFailures but never consecutively.
	for i := 0; i < 3; i++ {
		state.armFailure("editMessageText", 1)
		toolDone(run, "tool_fail_"+strconv.Itoa(i), false, true, 10*time.Millisecond)
		waitForCondition(t, 2*time.Second, "failing edit attempted", func() bool {
			return state.callCount("editMessageText") >= 2*i+1
		})
		toolDone(run, "tool_ok_"+strconv.Itoa(i), false, true, 10*time.Millisecond)
		waitForCondition(t, 2*time.Second, "recovered edit delivered", func() bool {
			return state.callCount("editMessageText") >= 2*i+2
		})
	}

	run.mu.Lock()
	disabled, failures := run.disabled, run.failures
	run.mu.Unlock()
	if disabled || failures != 0 {
		t.Fatalf("successful edits must refund the failure budget: disabled=%v failures=%d", disabled, failures)
	}

	run.FinalDelivered()
	waitForCondition(t, 2*time.Second, "preview deleted after final delivery", func() bool {
		return state.callCount("deleteMessage") >= 1
	})
}

// TestPreviewAwaitChatGateWaitsForBackoff pins the authoritative-delivery
// gate wait: an open gate returns immediately, an outstanding chat backoff
// is waited out, and a cancelled context releases the wait.
func TestPreviewAwaitChatGateWaitsForBackoff(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)

	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))

	done := make(chan struct{})
	go func() {
		run.AwaitChatGate(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("AwaitChatGate must return immediately on an open gate")
	}

	m.backoffChat(run.info.ChatID, 250*time.Millisecond)
	start := time.Now()
	run.AwaitChatGate(context.Background())
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("AwaitChatGate must wait out the chat backoff, waited %s", elapsed)
	}

	m.backoffChat(run.info.ChatID, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done = make(chan struct{})
	go func() {
		run.AwaitChatGate(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("AwaitChatGate must honor a cancelled context")
	}

	run.Abort()
}

// TestGateSweepReclaimsExpiredGates pins the retained-backoff reclamation:
// an expired gate of a chat that never returns is swept by unrelated
// manager activity, while live backoffs and gates with active runs stay.
func TestGateSweepReclaimsExpiredGates(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)

	m.mu.Lock()
	m.chatGates[-100900700601] = &chatGate{earliest: time.Now().Add(time.Minute), runs: 0}  // unexpired backoff: kept
	m.chatGates[-100900700602] = &chatGate{earliest: time.Now().Add(time.Minute), runs: 1}  // active run refcount: kept
	m.chatGates[-100900700603] = &chatGate{earliest: time.Now().Add(-time.Minute), runs: 0} // expired, chatless: swept
	m.mu.Unlock()

	// An unrelated chat's RunFor sweeps the expired gate; its own run exits
	// and releases its gate.
	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeTools))
	run.Abort()

	waitForCondition(t, 2*time.Second, "expired and released gates reclaimed", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()

		_, expiredLeft := m.chatGates[-100900700603]
		_, runGateLeft := m.chatGates[-100900700600]

		return !expiredLeft && !runGateLeft
	})

	m.mu.Lock()
	_, liveKept := m.chatGates[-100900700601]
	_, activeKept := m.chatGates[-100900700602]
	m.mu.Unlock()
	if !liveKept || !activeKept {
		t.Fatal("sweep must keep unexpired backoffs and gates with active runs")
	}
}

// TestPreviewInitialDebounceSurvivesEarlyEvents pins that early run events
// cannot collapse the initial debounce into the flush floor: a run that
// finishes inside the debounce window never creates a preview at all.
func TestPreviewInitialDebounceSurvivesEarlyEvents(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	m := newPreviewTestManager(t, b)
	m.timings.debounce = 200 * time.Millisecond

	run := m.RunFor(groupRunInfo(adminconfig.PreviewModeAllTools))
	run.applyEvent(execution.RunStarted{RunContext: execution.RunContext{RunID: run.ID()}, At: time.Now()})
	run.applyEvent(execution.GenerationStarted{RunContext: execution.RunContext{RunID: run.ID()}, At: time.Now(), Iteration: 1})

	time.Sleep(100 * time.Millisecond)
	if n := state.callCount("sendMessage"); n != 0 {
		t.Fatalf("early events must not pull the first send inside the debounce, got %d sends", n)
	}
	waitForCondition(t, 2*time.Second, "debounced first send", func() bool {
		return state.callCount("sendMessage") >= 1
	})

	run.FinalDelivered()
	waitForCondition(t, 2*time.Second, "preview deleted", func() bool {
		return state.callCount("deleteMessage") >= 1
	})
}

// TestIngressExcludesPreviewReplies pins that preview presentation text
// never enters conversational context: a message replying to a live group
// preview is stored without ReplyTo, replies to ordinary messages keep
// theirs, and forgetting the preview ID lifts the suppression.
func TestIngressExcludesPreviewReplies(t *testing.T) {
	srv := newPreviewServerState()
	b := newPreviewTestBot(t, srv)
	b.ctx = context.Background()
	b.history = newMemoryConversationStore()
	m := newPreviewTestManager(t, b)
	b.previews = m

	const chatID = int64(-100900700600)
	m.notePreviewMessage(chatID, 777)

	previewReply := tg.Message{
		MessageID: 100,
		Chat:      tg.Chat{ID: chatID, Type: tg.ChatTypeSupergroup},
		From:      &tg.User{ID: 42, Username: "alice", FirstName: "Alice"},
		ReplyToMessage: &tg.Message{
			MessageID: 777,
			Chat:      tg.Chat{ID: chatID, Type: tg.ChatTypeSupergroup},
			From:      &tg.User{ID: b.me.ID, IsBot: true, FirstName: "Bot"},
			Text:      "🔄 Working…",
		},
	}
	msg := b.tgUserMessageToMessageData(previewReply, true)
	if msg.ReplyTo != nil {
		t.Fatalf("reply to a live preview must not carry quotable content: %+v", msg.ReplyTo)
	}

	normalReply := previewReply
	normalReply.MessageID = 101
	normalReply.ReplyToMessage = &tg.Message{
		MessageID: 42,
		Chat:      tg.Chat{ID: chatID, Type: tg.ChatTypeSupergroup},
		From:      &tg.User{ID: 43, Username: "bob", FirstName: "Bob"},
		Text:      "what is this about?",
	}
	msg = b.tgUserMessageToMessageData(normalReply, true)
	if msg.ReplyTo == nil || msg.ReplyTo.Text != "what is this about?" {
		t.Fatalf("reply to an ordinary message must keep its context, got %+v", msg.ReplyTo)
	}

	// Once the preview is gone from the registry, the suppression lifts.
	m.forgetPreviewMessage(chatID, 777)
	msg = b.tgUserMessageToMessageData(previewReply, true)
	if msg.ReplyTo == nil {
		t.Fatal("reply to a forgotten preview message must be recorded like any other reply")
	}

	// The authoritative reply path honors the same exclusion.
	m.notePreviewMessage(chatID, 777)
	b.saveBotReplyToHistory(context.Background(), previewReply, "answer", 999)
	scope := state.ConversationScope{ChatID: chatID, TopicID: previewReply.MessageThreadID}
	snapshot := b.history.Snapshot(scope)
	if len(snapshot.Messages) != 1 {
		t.Fatalf("expected one history entry, got %d", len(snapshot.Messages))
	}
	if snapshot.Messages[0].ReplyTo != nil {
		t.Fatalf("bot reply to a live preview must be saved without ReplyTo, got %+v", snapshot.Messages[0].ReplyTo)
	}
}
