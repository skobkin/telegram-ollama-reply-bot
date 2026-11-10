// Package vision provides image recognition behind a focused, Telegram-free
// contract.
//
// A Recognizer describes an Image under a caller-supplied prompt. The prompt
// parameter is deliberate: today the bot renders the operator-managed
// image-recognition template, and a future tool can pass a model-authored
// prompt straight through without coupling recognition to Telegram or adding
// new caches. Nothing in this package imports Telegram code or stores image
// data; callers keep full ownership of the bytes.
package vision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/logging"
)

// Recognition errors. Errors returned by this package wrap these sentinels;
// messages carry only sizes, MIME types, and numeric statuses — never image
// bytes, prompts, provider text, or URLs.
var (
	// ErrUnsupportedImageType reports image data whose detected format is not
	// supported. Detection inspects the leading byte signature only; it is
	// not a full image integrity check.
	ErrUnsupportedImageType = errors.New("unsupported image type")
	// ErrImageTooLarge reports image data above the configured client-side bound.
	ErrImageTooLarge = errors.New("image exceeds size bound")
	// ErrPromptEmpty reports an empty recognition prompt.
	ErrPromptEmpty = errors.New("image recognition prompt is empty")
)

// Image is downloaded image data with its MIME type detected from the actual
// bytes (never from Telegram metadata, filenames, or content-type headers).
type Image struct {
	Data []byte
	MIME string // image/jpeg, image/png, or image/webp
}

// Recognition is one successful image description.
type Recognition struct {
	// Text is the recognizer's description of the image.
	Text string
	// Usage reports token usage when the back-end provides it; nil otherwise
	// (the MiniMax VLM response carries no usage fields, so that route
	// contributes nothing to usage accounting rather than guessing numbers).
	Usage *llm.TokenUsage
}

// Recognizer describes an image under a caller-supplied prompt.
type Recognizer interface {
	// Name identifies the recognizer in logs and startup diagnostics.
	Name() string
	// Supports reports whether the recognizer can handle the MIME type.
	Supports(mime string) bool
	// RequiresModel reports whether the recognizer routes through an LLM
	// feature model. Startup model checks consult this: a model-free
	// recognizer must not force an unused image LLM model to exist.
	RequiresModel() bool
	// Recognize describes the image under the prompt.
	Recognize(ctx context.Context, image Image, prompt string) (Recognition, error)
}

// Service renders the operator-managed image-recognition prompt and dispatches
// to the configured Recognizer.
type Service struct {
	recognizer    Recognizer
	prompts       llm.PromptRenderer
	maxImageBytes int64
	logger        *slog.Logger
}

// NewService builds a vision service. maxImageBytes is the client-side bound
// on accepted image data — this bot's own conservative choice, not a
// provider-documented limit. A non-positive bound disables the check
// (production wiring always passes the configured value).
func NewService(recognizer Recognizer, prompts llm.PromptRenderer, maxImageBytes int64, logger *slog.Logger) *Service {
	return &Service{
		recognizer:    recognizer,
		prompts:       prompts,
		maxImageBytes: maxImageBytes,
		logger:        logger,
	}
}

// Name returns the active recognizer's name.
func (s *Service) Name() string { return s.recognizer.Name() }

// RequiresModel reports whether the active recognizer needs an LLM feature
// model.
func (s *Service) RequiresModel() bool { return s.recognizer.RequiresModel() }

// Describe validates the image data, renders the image-recognition prompt,
// and returns the recognizer's description.
func (s *Service) Describe(ctx context.Context, imageData []byte) (Recognition, error) {
	logger := logging.FromContext(ctx, s.logger)

	if s.maxImageBytes > 0 && int64(len(imageData)) > s.maxImageBytes {
		logger.Warn(
			"image exceeds size bound",
			"recognizer", s.recognizer.Name(),
			"image_bytes", len(imageData),
			"max_image_bytes", s.maxImageBytes,
		)

		return Recognition{}, fmt.Errorf("%w: %d bytes exceeds bound %d", ErrImageTooLarge, len(imageData), s.maxImageBytes)
	}

	mime, err := DetectMIME(imageData)
	if err != nil {
		return Recognition{}, err
	}

	if !s.recognizer.Supports(mime) {
		return Recognition{}, fmt.Errorf("%w: %s is not supported by recognizer %s", ErrUnsupportedImageType, mime, s.recognizer.Name())
	}

	prompt, err := s.prompts.RenderImageRecognitionSystemPrompt(ctx, llm.PromptScope{})
	if err != nil {
		logger.Error("image recognition template processing failed", "error", err)

		return Recognition{}, llm.ErrTemplateProcessing
	}

	recognition, err := s.recognizer.Recognize(ctx, Image{Data: imageData, MIME: mime}, prompt)
	if err != nil {
		return Recognition{}, err
	}

	logger.Debug(
		"image recognized",
		"recognizer", s.recognizer.Name(),
		"mime", mime,
		"image_bytes", len(imageData),
		"description_length", len(recognition.Text),
	)

	return recognition, nil
}
