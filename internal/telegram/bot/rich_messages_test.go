package bot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/support/markdown"

	tg "github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

func TestBuildRichMessageParams(t *testing.T) {
	message := tg.Message{MessageID: 42, Chat: tg.Chat{ID: 100}}

	params := buildRichMessageParams(tu.ID(100), message, "**hi**")

	if params.RichMessage.Markdown != "**hi**" {
		t.Fatalf("expected markdown payload, got %+v", params.RichMessage)
	}
	if params.RichMessage.HTML != "" || params.RichMessage.Blocks != nil {
		t.Fatalf("expected only markdown set, got %+v", params.RichMessage)
	}
	if params.ChatID.ID != 100 {
		t.Fatalf("expected chat id 100, got %+v", params.ChatID)
	}
	if params.ReplyParameters == nil || params.ReplyParameters.MessageID != 42 {
		t.Fatalf("expected reply parameters quoting message 42, got %+v", params.ReplyParameters)
	}
	if params.MessageThreadID != 0 {
		t.Fatalf("expected no thread id, got %d", params.MessageThreadID)
	}
}

func TestBuildRichMessageParams_PassesThreadID(t *testing.T) {
	message := tg.Message{MessageID: 7, Chat: tg.Chat{ID: 100}, MessageThreadID: 55}

	params := buildRichMessageParams(tu.ID(100), message, "text")

	if params.MessageThreadID != 55 {
		t.Fatalf("expected thread id 55, got %d", params.MessageThreadID)
	}
}

// capturedRequest is a Bot API call recorded by the test server.
type capturedRequest struct {
	path string
	body map[string]any
	at   time.Time // arrival time at the test server
}

// newRichTestBot returns a Bot wired to a test Bot API server that records
// every request. failRich and failMessage make the corresponding methods
// return an API error.
func newRichTestBot(t *testing.T, requests *[]capturedRequest, mu *sync.Mutex, failRich, failMessage bool) *Bot {
	t.Helper()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Errorf("cannot decode request body: %v", err)
		}

		mu.Lock()
		*requests = append(*requests, capturedRequest{path: r.URL.Path, body: decoded})
		mu.Unlock()

		fail := strings.HasSuffix(r.URL.Path, "/sendRichMessage") && failRich ||
			strings.HasSuffix(r.URL.Path, "/sendMessage") && failMessage

		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "error_code": 400, "description": "test failure",
			})

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 1,
				"date":       1,
				"chat":       map[string]any{"id": 100, "type": "private"},
			},
		})
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

func richMessageTestInput() (tg.Message, string) {
	message := tg.Message{MessageID: 42, Chat: tg.Chat{ID: 100}}
	text := "# Hello\n\nSome **bold** and `code` with a [link](https://example.com)."

	return message, text
}

func TestSendRichMarkdownReply_Success(t *testing.T) {
	var requests []capturedRequest
	var mu sync.Mutex
	b := newRichTestBot(t, &requests, &mu, false, false)

	message, text := richMessageTestInput()

	_, err := b.sendRichMarkdownReply(context.Background(), message, text)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(requests) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(requests))
	}
	if !strings.HasSuffix(requests[0].path, "/sendRichMessage") {
		t.Fatalf("expected sendRichMessage call, got %s", requests[0].path)
	}

	rich := requests[0].body["rich_message"].(map[string]any)
	if rich["markdown"] != text {
		t.Fatalf("expected raw markdown %q on the wire, got %q", text, rich["markdown"])
	}

	reply := requests[0].body["reply_parameters"].(map[string]any)
	if reply["message_id"] != float64(42) {
		t.Fatalf("expected reply to message 42, got %v", reply["message_id"])
	}
}

// TestSendRichMarkdownReply_PassesDocumentedMarkupThrough is a review
// regression: documented Telegram inline syntax (tg://emoji, tg://time) and
// markup inside code spans or four-backtick fences must reach the wire
// untouched — rich markdown is sent raw, with no speculative rewriting.
func TestSendRichMarkdownReply_PassesDocumentedMarkupThrough(t *testing.T) {
	var requests []capturedRequest
	var mu sync.Mutex
	b := newRichTestBot(t, &requests, &mu, false, false)

	message := tg.Message{MessageID: 42, Chat: tg.Chat{ID: 100}}
	text := "![Image: 👍](tg://emoji?id=5368324170671202286)\n" +
		"![22:45 tomorrow](tg://time?unix=1647531900&format=wDT)\n" +
		"An example: `![icon](file.png)`\n" +
		"````markdown\n```\n![icon](file.png)\n````"

	_, err := b.sendRichMarkdownReply(context.Background(), message, text)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(requests) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(requests))
	}

	rich := requests[0].body["rich_message"].(map[string]any)
	if rich["markdown"] != text {
		t.Fatalf("documented markup changed on the wire:\nin:  %q\nout: %q", text, rich["markdown"])
	}
}

func TestSendRichMarkdownReply_FallsBackToMarkdownV2(t *testing.T) {
	var requests []capturedRequest
	var mu sync.Mutex
	b := newRichTestBot(t, &requests, &mu, true, false)

	message, text := richMessageTestInput()

	_, err := b.sendRichMarkdownReply(context.Background(), message, text)
	if err != nil {
		t.Fatalf("expected fallback success, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}
	if !strings.HasSuffix(requests[1].path, "/sendMessage") {
		t.Fatalf("expected sendMessage fallback, got %s", requests[1].path)
	}

	body := requests[1].body
	if body["parse_mode"] != "MarkdownV2" {
		t.Fatalf("expected MarkdownV2 parse mode, got %v", body["parse_mode"])
	}

	want := markdown.NewTgMarkdownV2Sanitizer().Sanitize(text)
	if body["text"] != want {
		t.Fatalf("expected sanitized fallback text %q, got %q", want, body["text"])
	}
}

func TestSendRichMarkdownReply_BothFail(t *testing.T) {
	var requests []capturedRequest
	var mu sync.Mutex
	b := newRichTestBot(t, &requests, &mu, true, true)

	message, text := richMessageTestInput()

	_, err := b.sendRichMarkdownReply(context.Background(), message, text)
	if err == nil {
		t.Fatalf("expected error when both sends fail")
	}

	mu.Lock()
	defer mu.Unlock()

	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}
}

func TestSendRichMarkdownReply_CropsOverLimit(t *testing.T) {
	var requests []capturedRequest
	var mu sync.Mutex
	b := newRichTestBot(t, &requests, &mu, false, false)

	message := tg.Message{MessageID: 42, Chat: tg.Chat{ID: 100}}
	text := strings.Repeat("word ", RichMessageCharLimit/3)

	_, err := b.sendRichMarkdownReply(context.Background(), message, text)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(requests) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(requests))
	}

	rich := requests[0].body["rich_message"].(map[string]any)
	sent, _ := rich["markdown"].(string)
	if sent == "" {
		t.Fatalf("expected markdown payload on the wire")
	}

	runes := []rune(sent)
	if len(runes) > RichMessageCharLimit {
		t.Fatalf("wire payload %d runes exceeds limit %d", len(runes), RichMessageCharLimit)
	}
	if !strings.HasSuffix(sent, "…") {
		t.Fatalf("expected ellipsis suffix, got %q", sent[len(sent)-20:])
	}
}

// TestSendRichMarkdownReply_CropsNoSpaceTextUsefully is a review regression:
// over-length text without whitespace (e.g. CJK) must deliver a near-limit
// payload on the wire, not an ellipsis alone.
func TestSendRichMarkdownReply_CropsNoSpaceTextUsefully(t *testing.T) {
	var requests []capturedRequest
	var mu sync.Mutex
	b := newRichTestBot(t, &requests, &mu, false, false)

	message := tg.Message{MessageID: 42, Chat: tg.Chat{ID: 100}}
	text := strings.Repeat("漢", RichMessageCharLimit+1)

	_, err := b.sendRichMarkdownReply(context.Background(), message, text)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(requests) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(requests))
	}

	rich := requests[0].body["rich_message"].(map[string]any)
	sent, _ := rich["markdown"].(string)

	runes := []rune(sent)
	if len(runes) != RichMessageCharLimit {
		t.Fatalf("expected %d runes on the wire, got %d", RichMessageCharLimit, len(runes))
	}
	if !strings.HasSuffix(sent, "漢…") {
		t.Fatalf("expected cropped CJK prefix with ellipsis, got tail %q", sent[len(sent)-10:])
	}
}

func TestSendRichMarkdownReplyWithSuffix_KeepsFooterOnRichPath(t *testing.T) {
	var requests []capturedRequest
	var mu sync.Mutex
	b := newRichTestBot(t, &requests, &mu, false, false)

	message := tg.Message{MessageID: 42, Chat: tg.Chat{ID: 100}}
	body := strings.Repeat("word ", 10000)
	footer := "\n\n[src](https://example.com/article)"

	_, err := b.sendRichMarkdownReplyWithSuffix(context.Background(), message, body, footer)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(requests) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(requests))
	}

	rich := requests[0].body["rich_message"].(map[string]any)
	sent, _ := rich["markdown"].(string)

	if runes := []rune(sent); len(runes) > RichMessageCharLimit {
		t.Fatalf("wire payload %d runes exceeds limit %d", len(runes), RichMessageCharLimit)
	}
	if !strings.HasSuffix(sent, footer) {
		t.Fatalf("expected reserved footer to survive cropping, got tail %q", sent[len(sent)-60:])
	}
}

func TestSendRichMarkdownReplyWithSuffix_KeepsFooterOnLegacyFallback(t *testing.T) {
	var requests []capturedRequest
	var mu sync.Mutex
	b := newRichTestBot(t, &requests, &mu, true, false)

	message := tg.Message{MessageID: 42, Chat: tg.Chat{ID: 100}}
	body := strings.Repeat("word ", 10000)
	footer := "\n\n[src](https://example.com/article)"

	_, err := b.sendRichMarkdownReplyWithSuffix(context.Background(), message, body, footer)
	if err != nil {
		t.Fatalf("expected fallback success, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}
	if !strings.HasSuffix(requests[1].path, "/sendMessage") {
		t.Fatalf("expected sendMessage fallback, got %s", requests[1].path)
	}

	sent, _ := requests[1].body["text"].(string)
	if runes := []rune(sent); len(runes) > TelegramCharLimit {
		t.Fatalf("fallback text %d runes exceeds limit %d", len(runes), TelegramCharLimit)
	}
	if !strings.HasSuffix(sent, footer) {
		t.Fatalf("expected reserved footer to survive fallback crop, got tail %q", sent[len(sent)-60:])
	}
}

// racedDeadlineContext is the state the transport race creates: the
// deadline has passed, the transport has already reported its timeout
// identity, but the context timer has not marked the context done yet —
// ctx.Err is still nil.
type racedDeadlineContext struct{ context.Context }

func (racedDeadlineContext) Deadline() (time.Time, bool) {
	return time.Now().Add(-time.Hour), true
}

func (racedDeadlineContext) Err() error { return nil }

// TestCtxDeadlineReached pins that the send-abort decision compares the
// budget's deadline against the clock instead of racing ctx.Err: a context
// whose deadline passed must read as exhausted even while its timer has not
// flipped Err yet, or an aborted send walks into a doomed fallback and the
// raw transport error escapes instead of the deadline.
func TestCtxDeadlineReached(t *testing.T) {
	if ctxDeadlineReached(context.Background()) {
		t.Fatal("a context without a deadline must not read as exhausted")
	}

	live, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if ctxDeadlineReached(live) {
		t.Fatal("a context with a future deadline must not read as exhausted")
	}

	past, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()
	if !ctxDeadlineReached(past) {
		t.Fatal("a context with a passed deadline must read as exhausted")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if !ctxDeadlineReached(cancelled) {
		t.Fatal("a cancelled context must read as exhausted")
	}

	if !ctxDeadlineReached(racedDeadlineContext{Context: context.Background()}) {
		t.Fatal("a raced context (deadline passed, Err still nil) must read as exhausted")
	}
}

// TestSendRichMarkdownReply_NoFallbackOnSpentDeadline pins that a rich send
// aborted by its deadline does not walk into the legacy fallback: the raced
// window (transport timeout already surfaced, ctx.Err not yet set) must be
// read as aborted, so the original transport error is returned unchanged
// instead of a mismatching fallback error.
func TestSendRichMarkdownReply_NoFallbackOnSpentDeadline(t *testing.T) {
	srv := newPreviewServerState()
	b := newPreviewTestBot(t, srv)
	srv.delayMethod("sendRichMessage", time.Hour)

	message := tg.Message{MessageID: 42, Chat: tg.Chat{ID: 100}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	sent, err := b.sendRichMarkdownReplyWithSuffix(ctx, message, "the answer", "")
	if sent != nil {
		t.Fatalf("an aborted send must not deliver, got %+v", sent)
	}
	if err == nil {
		t.Fatal("the abort must surface the original send error, got nil")
	}
	if isRateLimit(err) {
		t.Fatalf("expected the transport abort, got a rate limit: %v", err)
	}
	if n := srv.callCount("sendMessage"); n != 0 {
		t.Fatalf("a spent budget must not carry the fallback, got %d sendMessage calls", n)
	}
}
