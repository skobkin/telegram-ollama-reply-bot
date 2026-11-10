package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"
	"telegram-ollama-reply-bot/internal/vision"
)

type stubAnalyzer struct {
	calls      int
	lastMeta   state.ImageMeta
	lastPrompt string
	analysis   string
	err        error
}

func (a *stubAnalyzer) AnalyzeImage(_ context.Context, meta state.ImageMeta, prompt string) (string, error) {
	a.calls++
	a.lastMeta = meta
	a.lastPrompt = prompt

	return a.analysis, a.err
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newImageToolsRuntime(t *testing.T) (*Runtime, *stubAnalyzer, state.ConversationStore) {
	t.Helper()

	store := memory.New(memory.Config{}, testLogger()).Conversations()
	runtime := New(nil, store, nil, nil, nil, nil, testLogger(), Config{MaxIterations: 6})
	analyzer := &stubAnalyzer{analysis: "fresh analysis"}
	runtime.AttachImageAnalyzer(analyzer)

	return runtime, analyzer, store
}

func imageMessage(id int, chatID int64, topicID int, fileID string) state.Message {
	return state.Message{
		MessageID: id,
		ChatID:    chatID,
		TopicID:   topicID,
		Name:      "alice",
		HasImage:  true,
		ImageMeta: &state.ImageMeta{FileID: fileID, FileSize: 10},
	}
}

func imageCallContext(scope state.ConversationScope, requester state.Message) CallContext {
	return CallContext{
		Scope:     scope,
		Requester: requester,
		Logger:    testLogger().With("tool_name", "test"),
	}
}

func runListImages(t *testing.T, r *Runtime, callCtx CallContext, args string) toolResult {
	t.Helper()

	result, err := r.listRecentImages(context.Background(), callCtx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("list_recent_images: %v", err)
	}

	return result
}

func runAnalyzeImage(t *testing.T, r *Runtime, callCtx CallContext, args string) toolResult {
	t.Helper()

	result, err := r.analyzeImage(context.Background(), callCtx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("analyze_image handler error: %v", err)
	}

	return result
}

func analyzeResultCode(t *testing.T, result toolResult) string {
	t.Helper()

	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected data map, got %#v", result.Data)
	}
	payload, ok := data["error"].(toolErrorPayload)
	if !ok {
		t.Fatalf("expected error payload, got %#v", data["error"])
	}

	return payload.Code
}

func listedImageIDs(t *testing.T, result toolResult) []int {
	t.Helper()

	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected data map, got %#v", result.Data)
	}
	rawImages, ok := data["images"].([]map[string]any)
	if !ok {
		t.Fatalf("expected images list, got %#v", data["images"])
	}

	ids := make([]int, 0, len(rawImages))
	for _, entry := range rawImages {
		id, ok := entry["message_id"].(int)
		if !ok {
			t.Fatalf("expected int message_id, got %#v", entry["message_id"])
		}
		ids = append(ids, id)
	}

	return ids
}

func TestImageToolsAbsentWithoutAnalyzer(t *testing.T) {
	t.Parallel()

	runtime := New(nil, memory.New(memory.Config{}, testLogger()).Conversations(), nil, nil, nil, nil, testLogger(), Config{})

	// Before attach: neither tool is registered.
	if _, ok := runtime.registry.Lookup("list_recent_images"); ok {
		t.Fatal("list_recent_images must be absent before attach")
	}
	if _, ok := runtime.registry.Lookup("analyze_image"); ok {
		t.Fatal("analyze_image must be absent before attach")
	}

	// nil attach stays a no-op.
	runtime.AttachImageAnalyzer(nil)
	if _, ok := runtime.registry.Lookup("analyze_image"); ok {
		t.Fatal("nil attach must not register tools")
	}
}

func TestImageToolsRegisteredWithCorrectMetadata(t *testing.T) {
	t.Parallel()

	runtime, _, _ := newImageToolsRuntime(t)

	list, ok := runtime.registry.Lookup("list_recent_images")
	if !ok {
		t.Fatal("expected list_recent_images registered")
	}
	if list.InvocationPolicy != InvocationPolicyDiscretionary || list.SideEffecting {
		t.Fatalf("unexpected list policy: %q side-effecting=%v", list.InvocationPolicy, list.SideEffecting)
	}

	analyze, ok := runtime.registry.Lookup("analyze_image")
	if !ok {
		t.Fatal("expected analyze_image registered")
	}
	if analyze.InvocationPolicy != InvocationPolicyDiscretionaryPaid || analyze.SideEffecting {
		t.Fatalf("unexpected analyze policy: %q side-effecting=%v", analyze.InvocationPolicy, analyze.SideEffecting)
	}
	if analyze.ResultCharBudget != analyzeImageResultCharBudget || list.ResultCharBudget != listImagesResultCharBudget {
		t.Fatalf("unexpected budgets: list=%d analyze=%d", list.ResultCharBudget, analyze.ResultCharBudget)
	}
}

func TestListRecentImages_SnapshotOrderAndFields(t *testing.T) {
	t.Parallel()

	runtime, _, store := newImageToolsRuntime(t)
	scope := state.ConversationScope{ChatID: 7}

	oldDesc := imageMessage(11, 7, 0, "f1")
	oldDesc.Image = "a cached baseline description"
	oldDesc.Text = "old caption"
	store.AppendMessage(scope, oldDesc)
	store.AppendMessage(scope, state.Message{MessageID: 12, ChatID: 7, Text: "text only", Name: "bob"})
	noDesc := imageMessage(13, 7, 0, "f2")
	store.AppendMessage(scope, noDesc)

	result := runListImages(t, runtime, imageCallContext(scope, state.Message{MessageID: 99, ChatID: 7}), `{}`)

	if result.Status != "ok" {
		t.Fatalf("expected ok, got %q", result.Status)
	}
	if ids := listedImageIDs(t, result); fmt.Sprint(ids) != "[13 11]" {
		t.Fatalf("expected newest-first [13 11], got %v", ids)
	}

	images := result.Data.(map[string]any)["images"].([]map[string]any)
	withDesc := images[1]
	if withDesc["has_description"] != true || withDesc["description_preview"] != "a cached baseline description" {
		t.Fatalf("unexpected description fields: %#v", withDesc)
	}
	if withDesc["text_preview"] != "old caption" {
		t.Fatalf("expected caption text_preview, got %#v", withDesc["text_preview"])
	}
	withoutDesc := images[0]
	if withoutDesc["has_description"] != false {
		t.Fatalf("expected has_description false for undescribed stored message: %#v", withoutDesc)
	}
	if _, has := withoutDesc["description_preview"]; has {
		t.Fatalf("description_preview must be omitted when empty: %#v", withoutDesc)
	}
	if withoutDesc["name"] != "alice" || withoutDesc["reply_to_message_id"] != 0 {
		t.Fatalf("unexpected entry fields: %#v", withoutDesc)
	}
}

func TestListRecentImages_RequesterAndSameScopeReplyChain(t *testing.T) {
	t.Parallel()

	runtime, _, _ := newImageToolsRuntime(t)
	scope := state.ConversationScope{ChatID: 7, TopicID: 3}

	// Nested same-scope reply chain: requester -> 21 -> 22.
	ancestor := imageMessage(22, 7, 3, "f-grand")
	parent := imageMessage(21, 7, 3, "f-parent")
	parent.ReplyTo = &ancestor
	requester := imageMessage(20, 7, 3, "f-cur")
	requester.ReplyTo = &parent

	result := runListImages(t, runtime, imageCallContext(scope, requester), `{}`)

	if ids := listedImageIDs(t, result); fmt.Sprint(ids) != "[20 21 22]" {
		t.Fatalf("expected requester-first chain [20 21 22], got %v", ids)
	}
}

func TestListRecentImages_ScopeMatrixExcludesForeignNodes(t *testing.T) {
	t.Parallel()

	runtime, _, store := newImageToolsRuntime(t)
	scope := state.ConversationScope{ChatID: 7, TopicID: 3}

	// In-scope snapshot image.
	store.AppendMessage(scope, imageMessage(31, 7, 3, "f-in"))

	// Other-topic and other-chat snapshot images stay invisible.
	store.AppendMessage(state.ConversationScope{ChatID: 7, TopicID: 9}, imageMessage(32, 7, 9, "f-topic"))
	store.AppendMessage(state.ConversationScope{ChatID: 8, TopicID: 3}, imageMessage(33, 8, 3, "f-chat"))

	// Requester from another chat with a same-scope chain node beneath it.
	foreignChild := imageMessage(34, 8, 3, "f-foreign")
	foreignChild.ReplyTo = &[]state.Message{imageMessage(31, 8, 3, "f-collide")}[0]
	result := runListImages(t, runtime, imageCallContext(scope, foreignChild), `{}`)

	if ids := listedImageIDs(t, result); fmt.Sprint(ids) != "[31]" {
		t.Fatalf("expected only the in-scope snapshot image [31], got %v", ids)
	}

	// topic0-vs-nonzero both directions.
	zeroScope := state.ConversationScope{ChatID: 7}
	requester := imageMessage(40, 7, 5, "f-topic-nonzero")
	result = runListImages(t, runtime, imageCallContext(zeroScope, requester), `{}`)
	if result.Status != "empty" {
		t.Fatalf("topic0 scope must not adopt a nonzero-topic requester, got %#v", result)
	}
}

func TestListRecentImages_MismatchedAncestorStopsTraversal(t *testing.T) {
	t.Parallel()

	runtime, _, _ := newImageToolsRuntime(t)
	scope := state.ConversationScope{ChatID: 7, TopicID: 3}

	// requester(same scope) -> parent(foreign topic) -> ancestor(same scope id 42).
	inScopeAncestor := imageMessage(42, 7, 3, "f-deep")
	foreignParent := imageMessage(41, 7, 9, "f-foreign-mid")
	foreignParent.ReplyTo = &inScopeAncestor
	requester := imageMessage(40, 7, 3, "f-cur")
	requester.ReplyTo = &foreignParent

	result := runListImages(t, runtime, imageCallContext(scope, requester), `{}`)

	// The requester is listed; the foreign parent is skipped AND stops the
	// traversal, so the same-scope ancestor beyond it never surfaces.
	if ids := listedImageIDs(t, result); fmt.Sprint(ids) != "[40]" {
		t.Fatalf("expected traversal to stop at the mismatched parent: [40], got %v", ids)
	}
}

func TestListRecentImages_ChainBoundedAndCycleSafe(t *testing.T) {
	t.Parallel()

	runtime, _, _ := newImageToolsRuntime(t)
	scope := state.ConversationScope{ChatID: 7}

	// A chain deeper than the limit plus a cycle: only bounded traversal.
	requester := imageMessage(1, 7, 0, "f-root")
	node := &requester
	for i := 2; i <= 30; i++ {
		next := imageMessage(i, 7, 0, fmt.Sprintf("f-%d", i))
		node.ReplyTo = &next
		node = &next
	}
	node.ReplyTo = &requester // cycle back to the root

	result := runListImages(t, runtime, imageCallContext(scope, requester), `{}`)

	ids := listedImageIDs(t, result)
	if len(ids) > messageThreadChainLimit+1 {
		t.Fatalf("chain traversal must stay bounded, got %d entries", len(ids))
	}
	for i, id := range ids {
		if id != i+1 {
			t.Fatalf("unexpected chain entry %d at position %d", id, i)
		}
	}
}

func TestListRecentImages_LimitClampAndEmpty(t *testing.T) {
	t.Parallel()

	runtime, _, store := newImageToolsRuntime(t)
	scope := state.ConversationScope{ChatID: 7}

	store.AppendMessage(scope, imageMessage(1, 7, 0, "f1"))
	result := runListImages(t, runtime, imageCallContext(scope, state.Message{ChatID: 7}), `{"limit":99}`)
	if len(listedImageIDs(t, result)) != 1 {
		t.Fatalf("clamp must cap at available entries")
	}

	empty := runListImages(t, runtime, imageCallContext(state.ConversationScope{ChatID: 42}, state.Message{ChatID: 42}), `{}`)
	if empty.Status != "empty" {
		t.Fatalf("expected empty status, got %q", empty.Status)
	}
}

func TestAnalyzeImage_ResolutionMatrix(t *testing.T) {
	t.Parallel()

	runtime, analyzer, store := newImageToolsRuntime(t)
	scope := state.ConversationScope{ChatID: 7, TopicID: 3}

	store.AppendMessage(scope, imageMessage(31, 7, 3, "f-snapshot"))
	store.AppendMessage(scope, state.Message{MessageID: 35, ChatID: 7, TopicID: 3, Text: "text only"})

	ancestor := imageMessage(22, 7, 3, "f-chain")
	parent := state.Message{MessageID: 21, ChatID: 7, TopicID: 3, Text: "no image", ReplyTo: &ancestor}
	requester := imageMessage(20, 7, 3, "f-requester")
	requester.ReplyTo = &parent

	callCtx := imageCallContext(scope, requester)

	// Requester hit: identity match resolves even though earlier chain
	// candidates exist.
	result := runAnalyzeImage(t, runtime, callCtx, `{"message_id":20,"prompt":"look"}`)
	if result.Status != "ok" || analyzer.lastMeta.FileID != "f-requester" {
		t.Fatalf("expected requester resolution, got %#v meta=%+v", result.Status, analyzer.lastMeta)
	}

	// Reply-chain hit below a text-only node.
	result = runAnalyzeImage(t, runtime, callCtx, `{"message_id":22,"prompt":"look"}`)
	if result.Status != "ok" || analyzer.lastMeta.FileID != "f-chain" {
		t.Fatalf("expected chain resolution, got %#v meta=%+v", result.Status, analyzer.lastMeta)
	}

	// Snapshot hit.
	result = runAnalyzeImage(t, runtime, callCtx, `{"message_id":31,"prompt":"look"}`)
	if result.Status != "ok" || analyzer.lastMeta.FileID != "f-snapshot" {
		t.Fatalf("expected snapshot resolution, got %#v meta=%+v", result.Status, analyzer.lastMeta)
	}

	// Prompt is passed trimmed.
	if analyzer.lastPrompt != "look" {
		t.Fatalf("expected trimmed prompt, got %q", analyzer.lastPrompt)
	}
}

func TestAnalyzeImage_MissCasesAreIndistinguishable(t *testing.T) {
	t.Parallel()

	runtime, analyzer, store := newImageToolsRuntime(t)
	scope := state.ConversationScope{ChatID: 7, TopicID: 3}

	store.AppendMessage(scope, imageMessage(31, 7, 3, "f-snapshot"))
	store.AppendMessage(state.ConversationScope{ChatID: 7, TopicID: 9}, imageMessage(32, 7, 9, "f-other-topic"))
	store.AppendMessage(scope, state.Message{MessageID: 35, ChatID: 7, TopicID: 3, Text: "text only"})

	cases := map[string]int{
		"unknown id":      999,
		"other topic":     32,
		"text-only id":    35,
		"nonpositive was": 0,
	}
	for name, id := range cases {
		if id == 0 {
			continue // nonpositive ids are argument errors, not results
		}

		result := runAnalyzeImage(t, runtime, imageCallContext(scope, state.Message{ChatID: 7, TopicID: 3}), fmt.Sprintf(`{"message_id":%d,"prompt":"look"}`, id))
		if result.Status != "error" || analyzeResultCode(t, result) != analyzeErrorCodeImageNotFound {
			t.Fatalf("%s: expected image_not_found, got %#v", name, result)
		}
	}
	if analyzer.calls != 0 {
		t.Fatalf("misses must not reach the analyzer, got %d calls", analyzer.calls)
	}
}

func TestAnalyzeImage_MismatchedRequesterCollisionResolvesViaSnapshot(t *testing.T) {
	t.Parallel()

	runtime, analyzer, store := newImageToolsRuntime(t)
	scope := state.ConversationScope{ChatID: 7, TopicID: 3}

	store.AppendMessage(scope, imageMessage(31, 7, 3, "f-snapshot"))

	// A foreign requester carrying the same message_id as a valid scoped
	// snapshot node: the requester is skipped, the snapshot node resolves.
	foreignRequester := imageMessage(31, 8, 3, "f-forged")

	result := runAnalyzeImage(t, runtime, imageCallContext(scope, foreignRequester), `{"message_id":31,"prompt":"look"}`)
	if result.Status != "ok" || analyzer.lastMeta.FileID != "f-snapshot" {
		t.Fatalf("expected snapshot resolution despite forged requester, got %#v meta=%+v", result.Status, analyzer.lastMeta)
	}
}

func TestAnalyzeImage_PromptValidation(t *testing.T) {
	t.Parallel()

	runtime, analyzer, _ := newImageToolsRuntime(t)
	callCtx := imageCallContext(state.ConversationScope{ChatID: 7}, state.Message{ChatID: 7})

	result := runAnalyzeImage(t, runtime, callCtx, `{"message_id":1,"prompt":"   "}`)
	if result.Status != "error" || analyzeResultCode(t, result) != analyzeErrorCodePromptInvalid {
		t.Fatalf("expected prompt_invalid for blank prompt, got %#v", result)
	}

	long := strings.Repeat("x", analyzeImagePromptMaxChars+1)
	result = runAnalyzeImage(t, runtime, callCtx, fmt.Sprintf(`{"message_id":1,"prompt":%q}`, long))
	if result.Status != "error" || analyzeResultCode(t, result) != analyzeErrorCodePromptInvalid {
		t.Fatalf("expected prompt_invalid for over-long prompt, got %#v", result)
	}

	if analyzer.calls != 0 {
		t.Fatalf("invalid prompts must not reach the analyzer, got %d calls", analyzer.calls)
	}
}

func TestAnalyzeImage_ErrorCodeMapping(t *testing.T) {
	t.Parallel()

	runtime, analyzer, _ := newImageToolsRuntime(t)
	callCtx := imageCallContext(state.ConversationScope{ChatID: 7}, imageMessage(1, 7, 0, "f"))

	cases := []struct {
		name string
		err  error
		code string
	}{
		{"too large", errors.Join(errImageRecognitionStub, fmt.Errorf("%w: file reports 99 bytes", vision.ErrImageTooLarge)), analyzeErrorCodeImageTooLarge},
		{"unsupported", errors.Join(errImageRecognitionStub, fmt.Errorf("%w: image/gif", vision.ErrUnsupportedImageType)), analyzeErrorCodeUnsupportedType},
		{"transport-kind", errors.Join(errImageRecognitionStub, errors.New("telegram file metadata request failed: request failed")), analyzeErrorCodeRecognitionFailed},
	}
	for _, tc := range cases {
		analyzer.err = tc.err
		result := runAnalyzeImage(t, runtime, callCtx, `{"message_id":1,"prompt":"look"}`)
		if result.Status != "error" || analyzeResultCode(t, result) != tc.code {
			t.Fatalf("%s: expected %s, got %#v code=%q", tc.name, tc.code, result, analyzeResultCode(t, result))
		}
	}
}

// errImageRecognitionStub mirrors the bot's joined error shape without
// importing the bot package (which would create a cycle in tests).
var errImageRecognitionStub = errors.New("image recognition error")

func TestAnalyzeImage_ResultTruncatedToBudget(t *testing.T) {
	t.Parallel()

	runtime, analyzer, _ := newImageToolsRuntime(t)
	analyzer.analysis = strings.Repeat("д", analyzeImageAnalysisCharLimit+500)

	result := runAnalyzeImage(t, runtime, imageCallContext(state.ConversationScope{ChatID: 7}, imageMessage(1, 7, 0, "f")), `{"message_id":1,"prompt":"look"}`)

	marshaled, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := len([]rune(string(marshaled))); got > analyzeImageResultCharBudget+1 {
		t.Fatalf("result %d runes exceeds budget %d", got, analyzeImageResultCharBudget)
	}
}

// TestReplyWithTools_AccountsUsageAcrossMultipleToolCalls proves usage is
// accumulated across several tool calls inside one loop (two calls in one
// model response, then a final round), including the paid image analysis.
func TestReplyWithTools_AccountsUsageAcrossMultipleToolCalls(t *testing.T) {
	t.Parallel()

	store := memory.New(memory.Config{}, testLogger()).Conversations()
	scope := state.ConversationScope{ChatID: 7}
	store.AppendMessage(scope, imageMessage(31, 7, 0, "f1"))

	analyzer := &stubAnalyzer{analysis: "first analysis"}
	runtime := New(nil, store, nil, nil, nil, nil, testLogger(), Config{MaxIterations: 6})
	runtime.AttachImageAnalyzer(analyzer)

	llmStub := &stubLLM{responses: []llm.Response{
		{
			Message: llm.Message{ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "list_recent_images", Arguments: json.RawMessage(`{}`)},
				{ID: "c2", Name: "analyze_image", Arguments: json.RawMessage(`{"message_id":31,"prompt":"look closely"}`)},
			}},
			Usage: llm.TokenUsage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14},
		},
		{
			Message: llm.TextMessage(llm.RoleAssistant, "done"),
			Usage:   llm.TokenUsage{PromptTokens: 20, CompletionTokens: 6, TotalTokens: 26},
		},
	}}
	runtime.llm = llmStub

	reply, usage, err := runtime.ReplyWithTools(context.Background(), ChatRequest{
		Scope:          scope,
		RequestMessage: state.Message{MessageID: 99, ChatID: 7},
	})
	if err != nil {
		t.Fatalf("reply with tools: %v", err)
	}
	if reply != "done" {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if usage == nil || usage.PromptTokens != 30 || usage.CompletionTokens != 10 || usage.TotalTokens != 40 {
		t.Fatalf("usage must accumulate across rounds and calls, got %+v", usage)
	}
	if analyzer.calls != 1 {
		t.Fatalf("expected one analyzer call, got %d", analyzer.calls)
	}
}

// wireTextFor runs a tool through executeToolCall and returns the exact text
// bound for the model message.
func wireTextFor(t *testing.T, r *Runtime, req ChatRequest, call llm.ToolCall) string {
	t.Helper()

	message, _ := r.executeToolCall(context.Background(), req, call)

	return message.Text()
}

func decodeWireResult(t *testing.T, wire string) (topLevel, data map[string]any) {
	t.Helper()

	if !json.Valid([]byte(wire)) {
		t.Fatalf("wire text is not complete JSON: %.120s…", wire)
	}
	if err := json.Unmarshal([]byte(wire), &topLevel); err != nil {
		t.Fatalf("wire text does not decode: %v", err)
	}
	data, _ = topLevel["data"].(map[string]any)
	if data == nil {
		t.Fatalf("wire result carries no data object: %#v", topLevel)
	}

	return topLevel, data
}

// TestImageToolsWireResultsAreCompleteJSON drives both tools through
// executeToolCall with escaping-hostile content (HTML-style characters,
// quotes, backslashes, newlines) and asserts the model-bound text is always
// complete JSON within budget, keeps usable identities, and reports
// truncation/omission truthfully.
func TestImageToolsWireResultsAreCompleteJSON(t *testing.T) {
	t.Parallel()

	runtime, analyzer, store := newImageToolsRuntime(t)
	scope := state.ConversationScope{ChatID: 7}

	for i := 1; i <= 5; i++ {
		msg := imageMessage(i, 7, 0, "file")
		msg.Text = strings.Repeat(`<&>"\`+"\n", 20)
		msg.Image = strings.Repeat("<&>", 70)
		store.AppendMessage(scope, msg)
	}
	analyzer.analysis = strings.Repeat("<\\>\n\"quote\"", 400)

	req := ChatRequest{Scope: scope, RequestMessage: state.Message{ChatID: 7, MessageID: 99}}

	listWire := wireTextFor(t, runtime, req, llm.ToolCall{ID: "l", Name: "list_recent_images", Arguments: json.RawMessage(`{}`)})
	if got := len([]rune(listWire)); got > listImagesResultCharBudget {
		t.Fatalf("list wire %d runes exceeds budget %d", got, listImagesResultCharBudget)
	}
	listTop, listData := decodeWireResult(t, listWire)
	if listTop["status"] != "ok" {
		t.Fatalf("unexpected list status: %#v", listTop["status"])
	}
	images, ok := listData["images"].([]any)
	if !ok || len(images) == 0 {
		t.Fatalf("expected usable image identities, got %#v", listData["images"])
	}
	returnedIDs := make([]int, 0, len(images))
	for _, raw := range images {
		entry, _ := raw.(map[string]any)
		id, _ := entry["message_id"].(float64)
		if id <= 0 {
			t.Fatalf("entry lost its identity: %#v", entry)
		}
		returnedIDs = append(returnedIDs, int(id))
	}
	// Truthful counts: whatever was dropped is reported.
	if omitted, has := listData["omitted_count"]; has {
		omittedCount, _ := omitted.(float64)
		total, _ := listData["total_found"].(float64)
		if int(omittedCount)+len(images) != int(total) || int(total) != 5 {
			t.Fatalf("untruthful counts: returned=%d omitted=%v total=%v", len(images), omittedCount, total)
		}
		// Newest identities survive.
		if returnedIDs[0] != 5 {
			t.Fatalf("expected newest identity first, got %v", returnedIDs)
		}
	}

	analyzeWire := wireTextFor(t, runtime, req, llm.ToolCall{ID: "a", Name: "analyze_image", Arguments: json.RawMessage(`{"message_id":1,"prompt":"read"}`)})
	if got := len([]rune(analyzeWire)); got > analyzeImageResultCharBudget {
		t.Fatalf("analyze wire %d runes exceeds budget %d", got, analyzeImageResultCharBudget)
	}
	analyzeTop, analyzeData := decodeWireResult(t, analyzeWire)
	if analyzeTop["status"] != "ok" || analyzeData["message_id"] != float64(1) {
		t.Fatalf("unexpected analyze wire: %#v", analyzeData)
	}
	if analysis, _ := analyzeData["analysis"].(string); analysis == "" {
		t.Fatal("expected non-empty analysis on the wire")
	}
}

// TestAnalyzeImage_WireBudgetSurvivesEscapingTorture shrinks below the floor
// gracefully: heavily escaped output stays complete JSON within budget with
// a non-empty analysis.
func TestAnalyzeImage_WireBudgetSurvivesEscapingTorture(t *testing.T) {
	t.Parallel()

	runtime, analyzer, store := newImageToolsRuntime(t)
	scope := state.ConversationScope{ChatID: 7}
	store.AppendMessage(scope, imageMessage(1, 7, 0, "f"))
	analyzer.analysis = strings.Repeat("<\\>\n\"&", 800) // every char escapes to ~6

	req := ChatRequest{Scope: scope, RequestMessage: state.Message{ChatID: 7, MessageID: 99}}
	wire := wireTextFor(t, runtime, req, llm.ToolCall{ID: "a", Name: "analyze_image", Arguments: json.RawMessage(`{"message_id":1,"prompt":"read"}`)})

	if got := len([]rune(wire)); got > analyzeImageResultCharBudget {
		t.Fatalf("wire %d runes exceeds budget %d", got, analyzeImageResultCharBudget)
	}
	_, data := decodeWireResult(t, wire)
	if data["truncated"] != true {
		t.Fatalf("expected truncated flag for shrunken analysis: %#v", data)
	}
	if analysis, _ := data["analysis"].(string); utf8.RuneCountInString(analysis) < analyzeImageMinAnalysisChars-1 {
		t.Fatalf("analysis shrank below the floor: %d runes", utf8.RuneCountInString(analysis))
	}
}

// TestAnalyzeImage_WireFitUsesAvailableBudget proves the binary-search fitter
// uses the budget rather than collapsing to the floor: escaped runes cost
// several wire characters each, and the longest fitting prefix must keep the
// bulk of the analysis.
func TestAnalyzeImage_WireFitUsesAvailableBudget(t *testing.T) {
	t.Parallel()

	runtime, analyzer, store := newImageToolsRuntime(t)
	scope := state.ConversationScope{ChatID: 7}
	store.AppendMessage(scope, imageMessage(1, 7, 0, "f"))

	// 500 runes of '<' serialize to ~3000 wire characters; more than 300
	// escaped runes fit the 2600 budget.
	analyzer.analysis = strings.Repeat("<", 500)

	result := runAnalyzeImage(t, runtime, imageCallContext(scope, state.Message{ChatID: 7}), `{"message_id":1,"prompt":"read"}`)

	analysis, ok := result.Data.(map[string]any)["analysis"].(string)
	if !ok {
		t.Fatalf("expected analysis string, got %#v", result.Data)
	}
	if got := utf8.RuneCountInString(analysis); got < 300 {
		t.Fatalf("only %d analysis runes retained although >300 escaped runes fit the budget", got)
	}
	if result.Data.(map[string]any)["truncated"] != true {
		t.Fatal("expected truncated flag for a shrunken analysis")
	}
}

// TestListRecentImages_RetainsAllIdentitiesUnderBudget proves hostile preview
// content never makes identities undiscoverable: the fitter shrinks previews
// and strips optional metadata (compact mode) before any entry is dropped, so
// every requested message_id stays reachable for analyze_image.
func TestListRecentImages_RetainsAllIdentitiesUnderBudget(t *testing.T) {
	t.Parallel()

	runtime, _, store := newImageToolsRuntime(t)
	scope := state.ConversationScope{ChatID: 7}

	for i := 1; i <= 5; i++ {
		msg := imageMessage(i, 7, 0, "f")
		msg.Text = strings.Repeat("<", 180)
		msg.Image = strings.Repeat("<", 200)
		store.AppendMessage(scope, msg)
	}

	req := ChatRequest{Scope: scope, RequestMessage: state.Message{ChatID: 7, MessageID: 99}}
	wire := wireTextFor(t, runtime, req, llm.ToolCall{ID: "l", Name: "list_recent_images", Arguments: json.RawMessage(`{"limit":10}`)})

	if got := len([]rune(wire)); got > listImagesResultCharBudget {
		t.Fatalf("wire %d runes exceeds budget %d", got, listImagesResultCharBudget)
	}
	top, data := decodeWireResult(t, wire)
	if top["status"] != "ok" {
		t.Fatalf("unexpected status: %#v", top["status"])
	}

	images, ok := data["images"].([]any)
	if !ok || len(images) != 5 {
		t.Fatalf("expected all 5 identities retained, got %#v", data["images"])
	}
	seen := map[int]bool{}
	for _, raw := range images {
		entry, _ := raw.(map[string]any)
		id, _ := entry["message_id"].(float64)
		if id <= 0 || seen[int(id)] {
			t.Fatalf("unusable or duplicated identity: %#v", entry)
		}
		seen[int(id)] = true
	}
	for i := 1; i <= 5; i++ {
		if !seen[i] {
			t.Fatalf("identity %d became undiscoverable; got %v", i, seen)
		}
	}
	if _, has := data["omitted_count"]; has {
		t.Fatalf("identities must be retained instead of omitted: %#v", data)
	}
}

// TestSnapshotCandidateScopeContract is the EVERY-candidate helper contract:
// snapshot nodes are scope-checked exactly like requester and reply-chain
// nodes — a foreign node never supplies an image and never shadows an
// in-scope duplicate, including topic0 mismatches.
func TestSnapshotCandidateScopeContract(t *testing.T) {
	t.Parallel()

	scope := state.ConversationScope{ChatID: 7, TopicID: 3}

	foreign := imageMessage(1, 8, 3, "foreign")
	if meta := resolveScopeImage(scope, state.Message{}, []state.Message{foreign}, 1); meta != nil {
		t.Fatal("foreign snapshot node must not resolve")
	}

	topicMismatch := imageMessage(1, 7, 0, "topic0")
	if meta := resolveScopeImage(scope, state.Message{}, []state.Message{topicMismatch}, 1); meta != nil {
		t.Fatal("topic0 node must not resolve for a nonzero-topic scope")
	}
	if meta := resolveScopeImage(state.ConversationScope{ChatID: 7}, state.Message{}, []state.Message{imageMessage(1, 7, 3, "nz")}, 1); meta != nil {
		t.Fatal("nonzero-topic node must not resolve for a topic0 scope")
	}

	// Collision: a foreign node sharing the message_id of an in-scope node
	// must be skipped, not shadow it.
	inScope := imageMessage(5, 7, 3, "f-in")
	inScope.Image = "described"
	foreignCollision := imageMessage(5, 8, 3, "f-forged")

	meta := resolveScopeImage(scope, state.Message{}, []state.Message{inScope, foreignCollision}, 5)
	if meta == nil || meta.FileID != "f-in" {
		t.Fatalf("in-scope node must survive a foreign collision, got %+v", meta)
	}

	candidates := scopeMessageCandidates(scope, state.Message{ChatID: 7, TopicID: 3}, []state.Message{inScope, foreignCollision})
	for _, msg := range candidates {
		if msg.ChatID != 7 || msg.TopicID != 3 {
			t.Fatalf("candidate leaked out of scope: %+v", msg)
		}
	}
}
