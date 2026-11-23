package tooluse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/imagegen"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/provider"
	"telegram-ollama-reply-bot/internal/state"

	tg "github.com/mymmrac/telego"
)

func testPNG(t testing.TB) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatalf("encode png: %v", err)
	}

	return buf.Bytes()
}

// stubImageGenerator records requests and replies with a fixed image or error.
type stubImageGenerator struct {
	mu       sync.Mutex
	timeout  time.Duration
	image    imagegen.Image
	err      error
	requests []imagegen.Request
	// remaining records the context deadline distance seen by each call
	// (zero when the context had no deadline).
	remaining []time.Duration
	// hook runs inside Generate, before it returns.
	hook func(ctx context.Context)
}

func (g *stubImageGenerator) Name() string           { return "stub" }
func (g *stubImageGenerator) Timeout() time.Duration { return g.timeout }

func (g *stubImageGenerator) Generate(ctx context.Context, req imagegen.Request) (imagegen.Image, error) {
	g.mu.Lock()
	g.requests = append(g.requests, req)
	var left time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		left = time.Until(deadline)
	}
	g.remaining = append(g.remaining, left)
	hook := g.hook
	g.mu.Unlock()

	if hook != nil {
		hook(ctx)
	}

	return g.image, g.err
}

func (g *stubImageGenerator) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return len(g.requests)
}

type imageHarness struct {
	t        *testing.T
	runtime  *Runtime
	sender   *stubActionSender
	gen      *stubImageGenerator
	recorder *fakeRecorder
	clock    time.Time
}

func newImageHarness(t *testing.T, cooldown time.Duration) *imageHarness {
	t.Helper()

	h := &imageHarness{
		t:        t,
		sender:   &stubActionSender{message: &tg.Message{MessageID: 88, Date: sentDate}},
		gen:      &stubImageGenerator{timeout: time.Minute, image: imagegen.Image{Data: testPNG(t), MIME: "image/png", RevisedPrompt: "a revised prompt"}},
		recorder: &fakeRecorder{},
		clock:    time.Unix(sentDate, 0),
	}
	h.runtime = &Runtime{actions: h.sender, recorder: h.recorder}
	h.runtime.imageGenerator = h.gen
	h.runtime.imageCooldownWindow = cooldown
	h.runtime.imageNow = func() time.Time { return h.clock }

	return h
}

func imageRun() *runState {
	return &runState{imageGenerationEnabled: true, imageGenerationModel: "MiniMax-H3-Image"}
}

func imageCall(chat int64, topic int, run *runState) CallContext {
	return CallContext{Scope: state.ConversationScope{ChatID: chat, TopicID: topic}, run: run}
}

func (h *imageHarness) call(ctx context.Context, call CallContext, args string) toolResult {
	h.t.Helper()

	result, err := h.runtime.generateImage(ctx, call, json.RawMessage(args))
	if err != nil {
		h.t.Fatalf("generateImage(%s): unexpected error %v", args, err)
	}

	return result
}

func requireImageError(t *testing.T, result toolResult, code string, class ErrorClass) {
	t.Helper()

	if result.Error == nil || result.Error.Code != code || result.Error.Class != class {
		t.Fatalf("expected error %s/%s, got %+v", code, class, result.Error)
	}
}

func TestGenerateImageSendsPhotoWithCaptionToTopic(t *testing.T) {
	h := newImageHarness(t, 0)

	result := h.call(context.Background(), imageCall(-1001, 9, imageRun()),
		`{"prompt":"  a cat in a hat  ","aspect":"landscape","caption":"  Meet Whiskers  "}`)
	if result.Status != "ok" {
		t.Fatalf("unexpected failure: %+v", result.Error)
	}

	if h.sender.photoCalls != 1 {
		t.Fatalf("photo calls = %d", h.sender.photoCalls)
	}
	params := h.sender.lastPhotoParams
	if params.ChatID.ID != -1001 || params.MessageThreadID != 9 {
		t.Fatalf("target = chat %d topic %d", params.ChatID.ID, params.MessageThreadID)
	}
	if params.Caption != "Meet Whiskers" || params.ParseMode != "" {
		t.Fatalf("caption must be sent trimmed and as plain text: %q parse=%q", params.Caption, params.ParseMode)
	}
	if _, err := png.Decode(params.Photo.File); err != nil {
		t.Fatalf("uploaded file is not the generated PNG: %v", err)
	}
	if name := params.Photo.File.(interface{ Name() string }).Name(); name != "generated.png" {
		t.Fatalf("unexpected upload name %q", name)
	}

	if h.gen.calls() != 1 {
		t.Fatalf("generator calls = %d", h.gen.calls())
	}
	req := h.gen.requests[0]
	if req.Model != "MiniMax-H3-Image" || req.Prompt != "a cat in a hat" || req.Aspect != imagegen.AspectLandscape {
		t.Fatalf("unexpected generation request: %+v", req)
	}

	data := result.Data.(map[string]any)
	if data["message_id"] != 88 || data["chat_id"] != int64(-1001) || data["topic_id"] != 9 || data["aspect"] != "landscape" || data["revised_prompt"] != "a revised prompt" {
		t.Fatalf("unexpected result data: %+v", data)
	}
	wire, preparation := prepareResult(result, generateImageResultCharBudget)
	if preparation != ResultPreparationComplete || utf8.RuneCountInString(wire) > generateImageResultCharBudget {
		t.Fatalf("result must fit its budget: %s %s", preparation, wire)
	}
	if strings.Contains(wire, "iVBOR") || strings.Contains(wire, "base64") {
		t.Fatalf("the result must never carry image bytes: %s", wire)
	}
}

func TestGenerateImageDefaultsAspectAndOmitsEmptyCaption(t *testing.T) {
	h := newImageHarness(t, 0)

	result := h.call(context.Background(), imageCall(5, 0, imageRun()), `{"prompt":"a cat"}`)
	if result.Status != "ok" {
		t.Fatalf("unexpected failure: %+v", result.Error)
	}
	if got := h.gen.requests[0].Aspect; got != imagegen.AspectSquare {
		t.Fatalf("expected the default aspect square, got %q", got)
	}
	params := h.sender.lastPhotoParams
	if params.MessageThreadID != 0 || params.Caption != "" {
		t.Fatalf("no topic and no caption must stay unset: %+v", params)
	}
}

func TestGenerateImageRecordsTheSendWithoutImageMetadata(t *testing.T) {
	h := newImageHarness(t, 0)

	h.call(context.Background(), imageCall(-100500, 7, imageRun()), `{"prompt":"a cat in a hat","caption":"Meet Whiskers"}`)

	if len(h.recorder.calls) != 1 {
		t.Fatalf("recorder calls = %d, want 1", len(h.recorder.calls))
	}
	got := h.recorder.calls[0].msg
	if !got.IsMe || got.MessageID != 88 || got.Attachment == nil {
		t.Fatalf("recorded message = %+v", got)
	}
	if got.Attachment.Kind != state.AttachmentGeneratedImage || got.Attachment.Label != "Meet Whiskers" {
		t.Fatalf("recorded attachment = %+v", got.Attachment)
	}
	if got.HasImage || got.Text != "" {
		t.Fatalf("no image metadata or text may be stored for a generated image: %+v", got)
	}
}

func TestGenerateImageRejectsInvalidArgumentsWithoutAnyCall(t *testing.T) {
	cases := map[string]string{
		"not json":       `nope`,
		"unknown field":  `{"prompt":"a cat","size":"1024x1024"}`,
		"missing prompt": `{}`,
		"blank prompt":   `{"prompt":"  \n "}`,
		"long prompt":    `{"prompt":"` + strings.Repeat("p", imageGenerationPromptMaxRunes+1) + `"}`,
		"bad aspect":     `{"prompt":"a cat","aspect":"wide"}`,
		"upper aspect":   `{"prompt":"a cat","aspect":"Square"}`,
		"long caption":   `{"prompt":"a cat","caption":"` + strings.Repeat("c", imageGenerationCaptionMaxRunes+1) + `"}`,
		"wrong type":     `{"prompt":5}`,
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			h := newImageHarness(t, time.Minute)
			run := imageRun()

			_, err := h.runtime.generateImage(context.Background(), imageCall(5, 0, run), json.RawMessage(args))
			toolErr := ClassifyError(err, true)
			if err == nil || toolErr.Class != ErrorArguments || !toolErr.Retryable {
				t.Fatalf("err=%v classified=%+v", err, toolErr)
			}
			if h.gen.calls() != 0 || h.sender.photoCalls != 0 {
				t.Fatalf("nothing may be generated or sent: gen=%d photos=%d", h.gen.calls(), h.sender.photoCalls)
			}
			if run.imageGenerationAttempted || h.runtime.imageCooldown.cooling(5, h.clock, time.Minute) {
				t.Fatal("an invalid call must not use up the attempt or the cooldown")
			}
		})
	}
}

func TestGenerateImageAcceptsPromptAndCaptionAtTheLimit(t *testing.T) {
	h := newImageHarness(t, 0)
	args, _ := json.Marshal(map[string]string{
		"prompt":  strings.Repeat("я", imageGenerationPromptMaxRunes),
		"caption": strings.Repeat("я", imageGenerationCaptionMaxRunes),
	})

	if result := h.call(context.Background(), imageCall(5, 0, imageRun()), string(args)); result.Status != "ok" {
		t.Fatalf("limits are measured in characters, not bytes: %+v", result.Error)
	}
}

func TestGenerateImageUnavailableStatesNeverGenerate(t *testing.T) {
	cases := map[string]func(h *imageHarness) CallContext{
		"permission off": func(*imageHarness) CallContext {
			run := imageRun()
			run.imageGenerationEnabled = false

			return imageCall(5, 0, run)
		},
		"no model": func(*imageHarness) CallContext {
			run := imageRun()
			run.imageGenerationModel = ""

			return imageCall(5, 0, run)
		},
		"proactive turn": func(*imageHarness) CallContext {
			run := imageRun()
			run.proactive = true

			return imageCall(5, 0, run)
		},
		"no run state": func(*imageHarness) CallContext { return imageCall(5, 0, nil) },
		"no generator": func(h *imageHarness) CallContext {
			h.runtime.imageGenerator = nil

			return imageCall(5, 0, imageRun())
		},
		"no sender": func(h *imageHarness) CallContext {
			h.runtime.actions = nil

			return imageCall(5, 0, imageRun())
		},
	}

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			h := newImageHarness(t, time.Minute)
			call := build(h)

			result := h.call(context.Background(), call, `{"prompt":"a cat"}`)
			requireImageError(t, result, "tool_unavailable", ErrorUnavailable)
			if h.gen.calls() != 0 || h.sender.photoCalls != 0 {
				t.Fatalf("an unavailable tool must not generate or send")
			}
			if h.runtime.imageCooldown.cooling(5, h.clock, time.Minute) {
				t.Fatal("an unavailable tool must not reserve the cooldown")
			}
		})
	}
}

func TestGenerateImageAllowsOneAttemptPerTurn(t *testing.T) {
	h := newImageHarness(t, 0)
	run := imageRun()

	if result := h.call(context.Background(), imageCall(5, 0, run), `{"prompt":"a cat"}`); result.Status != "ok" {
		t.Fatalf("first attempt failed: %+v", result.Error)
	}
	result := h.call(context.Background(), imageCall(5, 0, run), `{"prompt":"another cat"}`)
	requireImageError(t, result, "image_limit_reached", ErrorPermanent)
	if h.gen.calls() != 1 || h.sender.photoCalls != 1 {
		t.Fatalf("the second attempt must not reach the provider: gen=%d photos=%d", h.gen.calls(), h.sender.photoCalls)
	}
}

func TestGenerateImageFailedAttemptAlsoUsesUpTheTurn(t *testing.T) {
	h := newImageHarness(t, 0)
	h.gen.err = provider.NewError("stub", provider.ErrorKindUpstreamUnavailable)
	run := imageRun()

	first := h.call(context.Background(), imageCall(5, 0, run), `{"prompt":"a cat"}`)
	requireImageError(t, first, "provider_unavailable", ErrorTransient)

	// A failed request may still have been billed: no second try this turn.
	second := h.call(context.Background(), imageCall(5, 0, run), `{"prompt":"a cat"}`)
	requireImageError(t, second, "image_limit_reached", ErrorPermanent)
	if h.gen.calls() != 1 {
		t.Fatalf("generator calls = %d, want 1", h.gen.calls())
	}
}

func TestGenerateImageCooldownIsPerChatAndExpires(t *testing.T) {
	h := newImageHarness(t, time.Minute)

	if result := h.call(context.Background(), imageCall(5, 0, imageRun()), `{"prompt":"a cat"}`); result.Status != "ok" {
		t.Fatalf("first generation failed: %+v", result.Error)
	}

	// Same chat, a new turn, inside the window: declined before any provider call.
	h.clock = h.clock.Add(20 * time.Second)
	result := h.call(context.Background(), imageCall(5, 0, imageRun()), `{"prompt":"a dog"}`)
	requireImageError(t, result, "image_cooldown", ErrorRateLimited)
	if result.Error.Retryable || result.Error.RetryAfterSeconds != 40 {
		t.Fatalf("expected a non-retryable decline with 40s to wait, got %+v", result.Error)
	}
	if h.gen.calls() != 1 {
		t.Fatalf("a cooling chat must not reach the provider, calls=%d", h.gen.calls())
	}

	// A different chat is unaffected.
	if other := h.call(context.Background(), imageCall(6, 0, imageRun()), `{"prompt":"a dog"}`); other.Status != "ok" {
		t.Fatalf("another chat must not be throttled: %+v", other.Error)
	}

	// After the window the chat may generate again.
	h.clock = h.clock.Add(41 * time.Second)
	if later := h.call(context.Background(), imageCall(5, 0, imageRun()), `{"prompt":"a bird"}`); later.Status != "ok" {
		t.Fatalf("the cooldown must expire: %+v", later.Error)
	}
}

func TestGenerateImageFailedAttemptStillCostsTheCooldown(t *testing.T) {
	h := newImageHarness(t, time.Minute)
	h.gen.err = provider.NewError("stub", provider.ErrorKindUpstreamUnavailable)

	h.call(context.Background(), imageCall(5, 0, imageRun()), `{"prompt":"a cat"}`)
	h.gen.err = nil

	result := h.call(context.Background(), imageCall(5, 0, imageRun()), `{"prompt":"a cat"}`)
	requireImageError(t, result, "image_cooldown", ErrorRateLimited)
}

func TestGenerateImageZeroCooldownDisablesSpacing(t *testing.T) {
	h := newImageHarness(t, 0)

	for i := 0; i < 3; i++ {
		if result := h.call(context.Background(), imageCall(5, 0, imageRun()), `{"prompt":"a cat"}`); result.Status != "ok" {
			t.Fatalf("attempt %d: %+v", i, result.Error)
		}
	}
}

func TestGenerateImageRefusesWhenTheTurnHasTooLittleTimeLeft(t *testing.T) {
	h := newImageHarness(t, time.Minute)
	run := imageRun()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second) // 12s - 10s reserve < 15s minimum
	defer cancel()

	result := h.call(ctx, imageCall(5, 0, run), `{"prompt":"a cat"}`)
	requireImageError(t, result, "insufficient_time", ErrorCancelled)
	if h.gen.calls() != 0 || h.sender.photoCalls != 0 {
		t.Fatal("no billed request may start without enough time")
	}
	if run.imageGenerationAttempted || h.runtime.imageCooldown.cooling(5, h.clock, time.Minute) {
		t.Fatal("a refusal for lack of time must not use up the attempt or the cooldown")
	}

	// The same chat can still generate with a healthy deadline.
	long, cancelLong := context.WithTimeout(context.Background(), time.Minute)
	defer cancelLong()
	if again := h.call(long, imageCall(5, 0, imageRun()), `{"prompt":"a cat"}`); again.Status != "ok" {
		t.Fatalf("a later turn with enough time must work: %+v", again.Error)
	}
}

func TestGenerateImageCapsGenerationAtTheTurnBudget(t *testing.T) {
	h := newImageHarness(t, 0)
	h.gen.timeout = 5 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	if result := h.call(ctx, imageCall(5, 0, imageRun()), `{"prompt":"a cat"}`); result.Status != "ok" {
		t.Fatalf("unexpected failure: %+v", result.Error)
	}
	left := h.gen.remaining[0]
	if left <= 0 || left > 30*time.Second || left < 28*time.Second {
		t.Fatalf("expected a generation deadline near 30s (40s turn minus the 10s reply reserve), got %s", left)
	}
}

func TestGenerateImageUsesConfiguredTimeoutWhenShorterThanTheTurn(t *testing.T) {
	h := newImageHarness(t, 0)
	h.gen.timeout = 20 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h.call(ctx, imageCall(5, 0, imageRun()), `{"prompt":"a cat"}`)
	if left := h.gen.remaining[0]; left <= 0 || left > 20*time.Second {
		t.Fatalf("expected the configured 20s cap, got %s", left)
	}
}

func TestGenerateImageWithoutTurnDeadlineUsesTheConfiguredTimeout(t *testing.T) {
	h := newImageHarness(t, 0)

	h.call(context.Background(), imageCall(5, 0, imageRun()), `{"prompt":"a cat"}`)
	if left := h.gen.remaining[0]; left <= 0 || left > time.Minute || left < 58*time.Second {
		t.Fatalf("without a turn deadline the generator's own 1m cap applies, got %s", left)
	}

	// An uncapped generator without a turn deadline gets no deadline at all.
	uncapped := newImageHarness(t, 0)
	uncapped.gen.timeout = 0
	uncapped.call(context.Background(), imageCall(5, 0, imageRun()), `{"prompt":"a cat"}`)
	if got := uncapped.gen.remaining[0]; got != 0 {
		t.Fatalf("no turn deadline and no cap must add no deadline, got %s", got)
	}
}

func TestImageGenerationBudget(t *testing.T) {
	within := func(got, want time.Duration) bool { return got <= want && got > want-2*time.Second }
	timeoutCtx := func(d time.Duration) context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		t.Cleanup(cancel)

		return ctx
	}

	if budget, ok := imageGenerationBudget(context.Background(), time.Minute); !ok || budget != time.Minute {
		t.Fatalf("no deadline: budget=%s ok=%v", budget, ok)
	}
	if budget, ok := imageGenerationBudget(context.Background(), 0); !ok || budget != 0 {
		t.Fatalf("no deadline and no cap: budget=%s ok=%v", budget, ok)
	}
	if budget, ok := imageGenerationBudget(timeoutCtx(100*time.Second), time.Minute); !ok || budget != time.Minute {
		t.Fatalf("configured cap shorter than the turn: budget=%s ok=%v", budget, ok)
	}
	if budget, ok := imageGenerationBudget(timeoutCtx(40*time.Second), time.Minute); !ok || !within(budget, 30*time.Second) {
		t.Fatalf("turn shorter than the cap: budget=%s ok=%v", budget, ok)
	}
	if budget, ok := imageGenerationBudget(timeoutCtx(40*time.Second), 0); !ok || !within(budget, 30*time.Second) {
		t.Fatalf("turn deadline with an uncapped generator: budget=%s ok=%v", budget, ok)
	}
	if budget, ok := imageGenerationBudget(timeoutCtx(100*time.Second), 5*time.Second); !ok || budget != 5*time.Second {
		t.Fatalf("a short configured cap is honored even below the minimum: budget=%s ok=%v", budget, ok)
	}
	if _, ok := imageGenerationBudget(timeoutCtx(20*time.Second), time.Minute); ok {
		t.Fatal("20s left minus the reserve is below the minimum and must be refused")
	}
	// Boundary: 24s - 10s reserve = 14s is still below the 15s minimum.
	if _, ok := imageGenerationBudget(timeoutCtx(24*time.Second), time.Minute); ok {
		t.Fatal("24s left leaves 14s after the reserve, below the minimum, and must be refused")
	}
	if _, ok := imageGenerationBudget(timeoutCtx(27*time.Second), time.Minute); !ok {
		t.Fatal("27s left leaves about 17s after the reserve and must be accepted")
	}
}

func TestGenerateImageCancelledBeforeGenerationStartsNothing(t *testing.T) {
	h := newImageHarness(t, time.Minute)
	run := imageRun()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := h.call(ctx, imageCall(5, 0, run), `{"prompt":"a cat"}`)
	requireImageError(t, result, "cancelled", ErrorCancelled)
	if h.gen.calls() != 0 || h.sender.photoCalls != 0 {
		t.Fatal("an expired turn must not start a billed request")
	}
	if run.imageGenerationAttempted || h.runtime.imageCooldown.cooling(5, h.clock, time.Minute) {
		t.Fatal("a cancelled call must not use up the attempt or the cooldown")
	}
}

func TestGenerateImageTurnExpiringDuringGenerationSendsNothing(t *testing.T) {
	h := newImageHarness(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.gen.hook = func(context.Context) { cancel() } // the turn dies while the provider works

	result := h.call(ctx, imageCall(5, 0, imageRun()), `{"prompt":"a cat"}`)
	requireImageError(t, result, "cancelled", ErrorCancelled)
	if h.sender.photoCalls != 0 {
		t.Fatal("an expired turn must not start the Telegram send")
	}
}

func TestGenerateImageTurnExpiringAfterGenerationSendsNothing(t *testing.T) {
	h := newImageHarness(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The provider returns a good image, but the turn ended meanwhile.
	h.gen.hook = func(context.Context) { cancel() }
	h.gen.err = nil

	result := h.call(ctx, imageCall(5, 0, imageRun()), `{"prompt":"a cat"}`)
	requireImageError(t, result, "cancelled", ErrorCancelled)
	if !strings.Contains(result.Error.Message, "ended") && !strings.Contains(result.Error.Message, "ready") {
		t.Fatalf("the model must be told the turn ended: %q", result.Error.Message)
	}
	if h.sender.photoCalls != 0 {
		t.Fatal("no photo may be sent after the turn expired")
	}
}

func TestGenerateImageSendFailureIsAmbiguous(t *testing.T) {
	h := newImageHarness(t, 0)
	h.sender.err = errors.New("connection reset by peer")

	_, err := h.runtime.generateImage(context.Background(), imageCall(5, 0, imageRun()), json.RawMessage(`{"prompt":"a cat"}`))
	if err == nil {
		t.Fatal("a send failure must surface as an error for classification")
	}
	if toolErr := ClassifyError(err, true); toolErr.Class != ErrorAmbiguous || toolErr.Retryable {
		t.Fatalf("a failed send may have reached Telegram: %+v", toolErr)
	}
	if len(h.recorder.calls) != 0 {
		t.Fatal("an unconfirmed send must not be recorded in history")
	}
}

func TestGenerateImageMapsFailuresToSafeResults(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		code  string
		class ErrorClass
	}{
		{"content rejected", provider.NewError("stub", provider.ErrorKindContentRejected), "content_rejected", ErrorPermanent},
		{"bad request", provider.NewError("stub", provider.ErrorKindBadRequest), "invalid_request", ErrorPermanent},
		{"rate limited", provider.NewError("stub", provider.ErrorKindRateLimited), "rate_limited", ErrorRateLimited},
		{"upstream", provider.NewError("stub", provider.ErrorKindUpstreamUnavailable), "provider_unavailable", ErrorTransient},
		{"auth", provider.NewError("stub", provider.ErrorKindAuthFailed), "provider_unavailable", ErrorPermanent},
		{"credits", provider.NewError("stub", provider.ErrorKindCreditsExhausted), "provider_unavailable", ErrorPermanent},
		{"misconfigured", provider.NewError("stub", provider.ErrorKindMisconfigured), "provider_unavailable", ErrorPermanent},
		{"too large", errors.Join(imagegen.ErrImageTooLarge, errors.New("12 bytes")), "image_too_large", ErrorPermanent},
		{"malformed", imagegen.ErrMalformedResponse, "invalid_response", ErrorTransient},
		{"generation timeout", context.DeadlineExceeded, "generation_timeout", ErrorTransient},
		{"canceled by the generator", context.Canceled, "cancelled", ErrorCancelled},
		{"unknown", errors.New("kaboom with SECRET-PROMPT-ECHO"), "generation_failed", ErrorPermanent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newImageHarness(t, 0)
			h.gen.err = tc.err

			result := h.call(context.Background(), imageCall(5, 0, imageRun()), `{"prompt":"a cat"}`)
			requireImageError(t, result, tc.code, tc.class)
			if result.Error.Retryable {
				t.Fatal("a billed generation must never invite a blind retry")
			}
			if strings.Contains(result.Error.Message+result.Error.RecoveryHint, "SECRET-PROMPT-ECHO") {
				t.Fatalf("raw error text leaked to the model: %+v", result.Error)
			}
			if h.sender.photoCalls != 0 {
				t.Fatal("a failed generation must not send anything")
			}
		})
	}
}

func TestGenerateImageTurnExpiryOutranksAGenerationTimeout(t *testing.T) {
	h := newImageHarness(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.gen.err = context.DeadlineExceeded
	h.gen.hook = func(context.Context) { cancel() }

	result := h.call(ctx, imageCall(5, 0, imageRun()), `{"prompt":"a cat"}`)
	requireImageError(t, result, "cancelled", ErrorCancelled)
}

func TestImageFileExtension(t *testing.T) {
	for mime, want := range map[string]string{
		"image/png":  ".png",
		"image/jpeg": ".jpg",
		"image/webp": ".webp",
		"":           ".png",
		"image/gif":  ".png",
	} {
		if got := imageFileExtension(mime); got != want {
			t.Errorf("imageFileExtension(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestGenerateImagePresentationGating(t *testing.T) {
	type tweak func(h *imageHarness, run *runState)
	cases := map[string]struct {
		tweak tweak
		want  bool
	}{
		"fully available": {func(*imageHarness, *runState) {}, true},
		"permission off":  {func(_ *imageHarness, run *runState) { run.imageGenerationEnabled = false }, false},
		"no model":        {func(_ *imageHarness, run *runState) { run.imageGenerationModel = "" }, false},
		"attempt used":    {func(_ *imageHarness, run *runState) { run.imageGenerationAttempted = true }, false},
		"no generator":    {func(h *imageHarness, _ *runState) { h.runtime.imageGenerator = nil }, false},
		"chat cooling": {func(h *imageHarness, _ *runState) {
			h.runtime.imageCooldown.stamp(5, h.clock, time.Minute)
		}, false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newImageHarness(t, time.Minute)
			run := imageRun()
			tc.tweak(h, run)

			_, available := h.runtime.generateImagePresentation(imageCall(5, 0, run), Presentation{})
			if available != tc.want {
				t.Fatalf("available = %v, want %v", available, tc.want)
			}
		})
	}

	h := newImageHarness(t, time.Minute)
	if _, available := h.runtime.generateImagePresentation(imageCall(5, 0, nil), Presentation{}); available {
		t.Fatal("a call without run state must be hidden")
	}

	// A cooldown that has elapsed no longer hides the tool.
	h.runtime.imageCooldown.stamp(5, h.clock, time.Minute)
	h.clock = h.clock.Add(2 * time.Minute)
	if _, available := h.runtime.generateImagePresentation(imageCall(5, 0, imageRun()), Presentation{}); !available {
		t.Fatal("an elapsed cooldown must not hide the tool")
	}
}

func TestAttachImageGeneratorNilIsANoOp(t *testing.T) {
	r := New(&stubLLM{}, nil, &stubExtractor{}, nil, &stubActionSender{}, nil, nil, Config{})
	r.AttachImageGenerator(nil, time.Minute)

	if r.imageGenerator != nil {
		t.Fatal("a nil generator must stay detached")
	}
	if _, registered := r.registry.Lookup(generateImageToolName); registered {
		t.Fatal("generate_image must be absent without a generator")
	}
}

func TestAttachImageGeneratorRegistersAPaidSideEffectingExplicitTool(t *testing.T) {
	r := New(&stubLLM{}, nil, &stubExtractor{}, nil, &stubActionSender{}, nil, nil, Config{})
	r.AttachImageGenerator(&stubImageGenerator{}, 30*time.Second)

	def, registered := r.registry.Lookup(generateImageToolName)
	if !registered {
		t.Fatal("generate_image must be registered once a generator is attached")
	}
	found := &def
	if !found.SideEffecting || found.InvocationPolicy != InvocationPolicyExplicitRequestOnly || found.Resolve == nil || found.Visibility != VisibilityStandard {
		t.Fatalf("unexpected definition: %+v", found)
	}
	if r.imageCooldownWindow != 30*time.Second {
		t.Fatalf("cooldown window = %s", r.imageCooldownWindow)
	}

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
		Additional bool                       `json:"additionalProperties"`
	}
	if err := json.Unmarshal(found.Parameters, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if len(schema.Properties) != 3 || len(schema.Required) != 1 || schema.Required[0] != "prompt" || schema.Additional {
		t.Fatalf("unexpected schema: %s", found.Parameters)
	}
}

// ---- end-to-end through the tool loop ----

func imageLoopRuntime(t *testing.T, stub *stubLLM, gen ImageGenerator, sender *stubActionSender, cooldown time.Duration) *Runtime {
	t.Helper()

	r := newSearchGateRuntime(stub, nil)
	r.actions = sender
	if gen != nil {
		r.AttachImageGenerator(gen, cooldown)
	}

	return r
}

func imageLoopRequest(enabled bool, model string) ChatRequest {
	req := eventRequest()
	req.TurnConfig = llm.TurnConfig{ImageGenerationEnabled: enabled, ImageGenerationModel: model}

	return req
}

func advertises(stub *stubLLM, result Result, name string) (schemas, advertised, policy bool) {
	for _, tool := range toolDefinitionNames(stub.lastBuildTools) {
		schemas = schemas || tool == name
	}
	for _, tool := range result.AdvertisedTools {
		advertised = advertised || tool == name
	}

	return schemas, advertised, strings.Contains(stub.lastBuildPolicy, name)
}

func TestGenerateImageAdvertisedOnlyWhenFullyPermitted(t *testing.T) {
	cases := []struct {
		name    string
		attach  bool
		enabled bool
		model   string
		visible bool
	}{
		{"permitted", true, true, "m", true},
		{"permission off", true, false, "m", false},
		{"no model selected", true, true, "", false},
		{"no backend attached", false, true, "m", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubLLM{responses: []llm.Response{{Message: llm.TextMessage(llm.RoleAssistant, "done")}}}
			var gen ImageGenerator
			if tc.attach {
				gen = &stubImageGenerator{timeout: time.Minute, image: imagegen.Image{Data: testPNG(t)}}
			}
			r := imageLoopRuntime(t, stub, gen, &stubActionSender{}, 0)

			result, err := r.ReplyWithTools(context.Background(), imageLoopRequest(tc.enabled, tc.model))
			if err != nil {
				t.Fatal(err)
			}
			schemas, advertised, policy := advertises(stub, result, generateImageToolName)
			for label, got := range map[string]bool{"tool schemas": schemas, "advertised set": advertised, "tool policy": policy} {
				if got != tc.visible {
					t.Errorf("generate_image present in %s = %v, want %v", label, got, tc.visible)
				}
			}
		})
	}
}

func imageToolCallResponses(args string) []llm.Response {
	return []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: generateImageToolName, Arguments: json.RawMessage(args)},
		}}},
		{Message: llm.TextMessage(llm.RoleAssistant, "done")},
	}
}

func TestGenerateImageCallRejectedWhenNotPermitted(t *testing.T) {
	stub := &stubLLM{responses: imageToolCallResponses(`{"prompt":"a cat"}`)}
	gen := &stubImageGenerator{timeout: time.Minute, image: imagegen.Image{Data: testPNG(t)}}
	sender := &stubActionSender{message: &tg.Message{MessageID: 9}}
	r := imageLoopRuntime(t, stub, gen, sender, 0)

	// A model that names the tool anyway (stale instructions, prompt
	// injection) must be rejected without touching the paid backend.
	result, err := r.ReplyWithTools(context.Background(), imageLoopRequest(false, "m"))
	if err != nil {
		t.Fatal(err)
	}
	if gen.calls() != 0 || sender.photoCalls != 0 {
		t.Fatalf("a disabled chat must never reach the provider: gen=%d photos=%d", gen.calls(), sender.photoCalls)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("records = %+v", result.ToolCalls)
	}
	record := result.ToolCalls[0]
	if record.Name != generateImageToolName || record.Executed || record.Rejection != RejectionNotAdvertised {
		t.Fatalf("record = %+v", record)
	}
}

func TestGenerateImageCallExecutesAndSendsThePhoto(t *testing.T) {
	stub := &stubLLM{responses: imageToolCallResponses(`{"prompt":"a cat in a hat","aspect":"portrait","caption":"Whiskers"}`)}
	gen := &stubImageGenerator{timeout: time.Minute, image: imagegen.Image{Data: testPNG(t)}}
	sender := &stubActionSender{message: &tg.Message{MessageID: 9}}
	r := imageLoopRuntime(t, stub, gen, sender, time.Minute)

	result, err := r.ReplyWithTools(context.Background(), imageLoopRequest(true, "MiniMax-H3-Image"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 1 || !result.ToolCalls[0].Executed || result.ToolCalls[0].Status != "ok" || !result.ToolCalls[0].SideEffecting {
		t.Fatalf("records = %+v", result.ToolCalls)
	}
	if sender.photoCalls != 1 || sender.lastPhotoParams.Caption != "Whiskers" {
		t.Fatalf("photo calls = %d params = %+v", sender.photoCalls, sender.lastPhotoParams)
	}
	if req := gen.requests[0]; req.Model != "MiniMax-H3-Image" || req.Aspect != imagegen.AspectPortrait {
		t.Fatalf("the model and aspect must come from the turn snapshot and the call: %+v", req)
	}
}

// After a generation the chat is cooling: a later turn in the same chat no
// longer sees the tool at all.
func TestGenerateImageIsHiddenFromTheNextTurnInACoolingChat(t *testing.T) {
	stub := &stubLLM{responses: imageToolCallResponses(`{"prompt":"a cat"}`)}
	gen := &stubImageGenerator{timeout: time.Minute, image: imagegen.Image{Data: testPNG(t)}}
	r := imageLoopRuntime(t, stub, gen, &stubActionSender{message: &tg.Message{MessageID: 9}}, time.Minute)

	if _, err := r.ReplyWithTools(context.Background(), imageLoopRequest(true, "m")); err != nil {
		t.Fatal(err)
	}

	stub.responses = []llm.Response{{Message: llm.TextMessage(llm.RoleAssistant, "done")}}
	result, err := r.ReplyWithTools(context.Background(), imageLoopRequest(true, "m"))
	if err != nil {
		t.Fatal(err)
	}
	if schemas, advertised, _ := advertises(stub, result, generateImageToolName); schemas || advertised {
		t.Fatalf("a cooling chat must not be offered the tool: schemas=%v advertised=%v", schemas, advertised)
	}
}
