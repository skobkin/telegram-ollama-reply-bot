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

	err := b.sendRichMarkdownReply(context.Background(), message, text)
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
		t.Fatalf("expected sanitized markdown %q on the wire, got %q", text, rich["markdown"])
	}

	reply := requests[0].body["reply_parameters"].(map[string]any)
	if reply["message_id"] != float64(42) {
		t.Fatalf("expected reply to message 42, got %v", reply["message_id"])
	}
}

func TestSendRichMarkdownReply_FallsBackToMarkdownV2(t *testing.T) {
	var requests []capturedRequest
	var mu sync.Mutex
	b := newRichTestBot(t, &requests, &mu, true, false)

	message, text := richMessageTestInput()

	err := b.sendRichMarkdownReply(context.Background(), message, text)
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

	err := b.sendRichMarkdownReply(context.Background(), message, text)
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

	err := b.sendRichMarkdownReply(context.Background(), message, text)
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
