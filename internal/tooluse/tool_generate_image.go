package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/imagegen"
	"telegram-ollama-reply-bot/internal/provider"
	"telegram-ollama-reply-bot/internal/telegram/msgdesc"
)

const (
	generateImageToolName = "generate_image"

	// imageGenerationReplyReserve is the part of the turn's remaining time
	// kept back for the model's final reply after the image is sent: the
	// generation budget never eats it.
	imageGenerationReplyReserve = 10 * time.Second
	// imageGenerationMinBudget is the least generation time worth starting a
	// billed request for. A request that cannot finish is still charged, so
	// below this the tool refuses without calling the provider.
	imageGenerationMinBudget = 15 * time.Second

	imageGenerationPromptMaxRunes        = 2000
	imageGenerationCaptionMaxRunes       = 1024 // Telegram's caption limit
	imageGenerationRevisedPromptMaxRunes = 200
)

// ImageGenerator generates one image per call. It is implemented by
// *imagegen.Service, which owns validation, bounds and MIME checks; the
// interface keeps the backend out of tooluse and lets tests substitute a fake.
type ImageGenerator interface {
	// Name identifies the active backend for logs.
	Name() string
	// Timeout is the configured per-request cap (zero = uncapped).
	Timeout() time.Duration
	Generate(ctx context.Context, req imagegen.Request) (imagegen.Image, error)
}

// AttachImageGenerator wires the image generator and registers generate_image.
// It is a no-op when g is nil, so the application layer can gate it on the
// configured backend; while detached the tool is absent from the registry and
// never advertised. chatCooldown spaces generation attempts per chat (zero
// disables it). Like AttachImageAnalyzer it runs after runtime construction.
func (r *Runtime) AttachImageGenerator(g ImageGenerator, chatCooldown time.Duration) {
	if g == nil {
		return
	}
	r.imageGenerator = g
	r.imageCooldownWindow = chatCooldown

	r.registry.Register(Definition{
		Name:             generateImageToolName,
		Description:      "Generate a new image from a text prompt and send it to the current chat as a photo. Use only when the user explicitly asks for a picture, drawing, illustration or image to be created. Each call is billed and slow (it can take up to a minute), and a turn gets one attempt, so write one good, self-contained, detailed prompt instead of retrying. The image goes out as its own message; do not paste or describe a link, and do not repeat the prompt back at length afterwards.",
		Parameters:       json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","minLength":1,"maxLength":2000,"description":"Detailed description of the image to generate: subject, style, composition, mood."},"aspect":{"type":"string","enum":["square","landscape","portrait"],"description":"Image shape. Defaults to square."},"caption":{"type":"string","maxLength":1024,"description":"Optional short plain-text caption sent with the image. Omit it unless it adds something."}},"required":["prompt"],"additionalProperties":false}`),
		InvocationPolicy: InvocationPolicyExplicitRequestOnly,
		Visibility:       VisibilityStandard,
		ResultCharBudget: generateImageResultCharBudget,
		SideEffecting:    true,
		Handler:          r.generateImage,
		Resolve:          r.generateImagePresentation,
	})
}

func (r *Runtime) imageClock() time.Time {
	if r.imageNow != nil {
		return r.imageNow()
	}

	return time.Now()
}

// generateImagePresentation hides generate_image whenever calling it cannot
// succeed: no generator, the chat's permission off, no model selected, the
// turn's single attempt used up, or the chat inside its cooldown. The handler
// re-checks every condition; this only decides advertisement.
func (r *Runtime) generateImagePresentation(call CallContext, p Presentation) (Presentation, bool) {
	run := call.run
	if r.imageGenerator == nil || run == nil || !run.canGenerateImage() {
		return p, false
	}
	if r.imageCooldownWindow > 0 && r.imageCooldown.cooling(call.Scope.ChatID, r.imageClock(), r.imageCooldownWindow) {
		return p, false
	}

	return p, true
}

// canGenerateImage reports whether the run may still start a generation: the
// permission is on, a model is selected, the turn's attempt is unused, and the
// turn answers a user. Proactive turns never generate: nobody asked for the
// image and every call is billed.
func (s *runState) canGenerateImage() bool {
	return s != nil && !s.proactive && s.imageGenerationEnabled && s.imageGenerationModel != "" && !s.imageGenerationAttempted
}

// imageToolError builds a model-facing failure. Nothing here is retryable:
// generation is billed and a turn gets one attempt, so no failure may invite
// the model to try again on its own.
func imageToolError(code, message, hint string, class ErrorClass) toolResult {
	return ErrorResult(NewToolError(code, class, message, false, hint))
}

// imageCancelledResult reports an expired turn. Nothing reached Telegram, so
// the outcome is not ambiguous.
func imageCancelledResult(message string) toolResult {
	return imageToolError("cancelled", message, "", ErrorCancelled)
}

// imageGenerationBudget derives how long one generation may run. configured is
// the generator's own cap (zero = none). With a turn deadline the budget is
// what is left after the reply reserve; below the minimum a billed request is
// not worth starting. Zero with ok means "no cap beyond the context".
func imageGenerationBudget(ctx context.Context, configured time.Duration) (time.Duration, bool) {
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		return configured, true
	}

	available := time.Until(deadline) - imageGenerationReplyReserve
	if available < imageGenerationMinBudget {
		return 0, false
	}
	if configured > 0 && configured < available {
		return configured, true
	}

	return available, true
}

func (r *Runtime) generateImage(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Prompt  string `json:"prompt"`
		Aspect  string `json:"aspect"`
		Caption string `json:"caption"`
	}
	if !decodeToolJSON(args, &payload) {
		return toolResult{}, invalidArguments("Provide prompt, and optionally aspect and caption, according to the schema.")
	}

	prompt := strings.TrimSpace(payload.Prompt)
	if prompt == "" {
		return toolResult{}, invalidArguments("prompt is required.")
	}
	if utf8.RuneCountInString(prompt) > imageGenerationPromptMaxRunes {
		return toolResult{}, invalidArguments("prompt is too long; keep it under 2000 characters.")
	}
	aspect := imagegen.Aspect(payload.Aspect)
	if payload.Aspect == "" {
		aspect = imagegen.AspectSquare
	}
	if !aspect.Valid() {
		return toolResult{}, invalidArguments("aspect must be square, landscape or portrait.")
	}
	caption := strings.TrimSpace(payload.Caption)
	if utf8.RuneCountInString(caption) > imageGenerationCaptionMaxRunes {
		return toolResult{}, invalidArguments("caption is too long; keep it under 1024 characters.")
	}

	run := callCtx.run
	if r.imageGenerator == nil || r.actions == nil || run == nil || run.proactive || !run.imageGenerationEnabled || run.imageGenerationModel == "" {
		return imageToolError("tool_unavailable", "Image generation is unavailable for this chat.", "", ErrorUnavailable), nil
	}
	// The turn's single attempt is checked first: a blocked turn must not be
	// coaxed into a second billed request.
	if run.imageGenerationAttempted {
		return imageToolError("image_limit_reached", "This turn already used its image generation.", "Reply with text instead.", ErrorPermanent), nil
	}

	if contextDone(ctx) {
		return imageCancelledResult("The image was cancelled before it was generated."), nil
	}
	budget, enoughTime := imageGenerationBudget(ctx, r.imageGenerator.Timeout())
	if !enoughTime {
		// Decided before the cooldown slot is reserved: a refusal for lack of
		// time must not also cost the chat its cooldown.
		return imageToolError("insufficient_time", "Not enough time is left in this turn to generate an image.", "Tell the user it could not be done this time and to ask again.", ErrorCancelled), nil
	}

	// Nothing that can decline or cancel the call may sit between the
	// admission below and the generation: a reserved cooldown slot must always
	// be followed by a real attempt.
	if r.imageCooldownWindow > 0 {
		if admitted, wait := r.imageCooldown.admit(callCtx.Scope.ChatID, true, r.imageClock(), r.imageCooldownWindow); !admitted {
			declined := NewToolError("image_cooldown", ErrorRateLimited, "An image was generated in this chat moments ago.", false, "Tell the user to ask again in a little while.")
			declined.RetryAfterSeconds = int((wait + time.Second - 1) / time.Second)

			return ErrorResult(declined), nil
		}
	}
	run.imageGenerationAttempted = true

	genCtx := ctx
	if budget > 0 {
		var cancel context.CancelFunc
		genCtx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	image, err := r.imageGenerator.Generate(genCtx, imagegen.Request{Model: run.imageGenerationModel, Prompt: prompt, Aspect: aspect})
	if err != nil {
		result := imageGenerationFailure(ctx, err)
		logToolResult(callCtx, result, "backend", r.imageGenerator.Name(), "aspect", string(aspect))

		return result, nil
	}

	// An expired turn must not start the Telegram side effect, even though
	// the image was generated (and billed).
	if contextDone(ctx) {
		result := imageCancelledResult("The image was generated but the turn ended before it could be sent.")
		logToolResult(callCtx, result, "backend", r.imageGenerator.Name(), "aspect", string(aspect))

		return result, nil
	}

	message, err := r.sendPhotoToScope(ctx, callCtx, image.Data, "generated"+imageFileExtension(image.MIME), caption)
	if err != nil {
		return toolResult{}, err
	}

	messageID := 0
	if message != nil {
		messageID = message.MessageID
	}
	// Described from the caption or prompt, not the pixels: no image metadata
	// is stored, so the vision model is never asked to describe the bot's own
	// picture and the generated bytes are never retained.
	r.recordSent(ctx, callCtx, message, msgdesc.GeneratedImage(caption, prompt))

	data := map[string]any{
		"chat_id":    callCtx.Scope.ChatID,
		"topic_id":   callCtx.Scope.TopicID,
		"message_id": messageID,
		"aspect":     string(aspect),
	}
	if revised := truncateUTF8(normalizeWhitespace(image.RevisedPrompt), imageGenerationRevisedPromptMaxRunes); revised != "" {
		data["revised_prompt"] = revised
	}
	result := toolResult{Status: "ok", Summary: "Image generated and sent", Data: data}
	logToolResult(callCtx, result, "message_id", messageID, "backend", r.imageGenerator.Name(), "aspect", string(aspect), "image_bytes", len(image.Data))

	return result, nil
}

// imageFileExtension names the upload by its detected type. Telegram sniffs
// the content; the extension only keeps the multipart filename honest.
func imageFileExtension(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

// imageGenerationFailure maps a generation error to a model-safe result.
// Provider text never reaches the model (the backend never puts any in an
// error), and no failure invites a blind retry: generation is billed and a
// turn gets one attempt.
func imageGenerationFailure(turnCtx context.Context, err error) toolResult {
	if contextDone(turnCtx) {
		return imageCancelledResult("The turn ended before the image was ready.")
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return imageToolError("generation_timeout", "Image generation took too long and was stopped.", "Tell the user it timed out and to try again later.", ErrorTransient)
	case errors.Is(err, context.Canceled):
		return imageCancelledResult("The image was cancelled before it was ready.")
	case errors.Is(err, imagegen.ErrImageTooLarge):
		return imageToolError("image_too_large", "The generated image is too large to send.", "Tell the user and offer to try a simpler image.", ErrorPermanent)
	case errors.Is(err, imagegen.ErrMalformedResponse):
		return imageToolError("invalid_response", "The image provider returned an unusable result.", "Tell the user it failed and to try again later.", ErrorTransient)
	}

	switch provider.KindOf(err) {
	case provider.ErrorKindContentRejected:
		return imageToolError("content_rejected", "The image provider refused this prompt.", "Tell the user the request was rejected and offer to try a different description.", ErrorPermanent)
	case provider.ErrorKindBadRequest:
		return imageToolError("invalid_request", "The image provider could not process this request.", "Tell the user and offer to try a simpler or shorter description.", ErrorPermanent)
	case provider.ErrorKindRateLimited:
		return imageToolError("rate_limited", "Image generation is busy or rate-limited right now.", "Tell the user to try again in a little while.", ErrorRateLimited)
	case provider.ErrorKindUpstreamUnavailable:
		return imageToolError("provider_unavailable", "The image provider is temporarily unavailable.", "Tell the user to try again later.", ErrorTransient)
	case provider.ErrorKindAuthFailed, provider.ErrorKindCreditsExhausted, provider.ErrorKindMisconfigured, provider.ErrorKindUnsupported:
		return imageToolError("provider_unavailable", "Image generation is not available right now.", "Tell the user it is unavailable.", ErrorPermanent)
	default:
		return imageToolError("generation_failed", "The image could not be generated.", "Tell the user it failed.", ErrorPermanent)
	}
}
