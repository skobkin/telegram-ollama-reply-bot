// Package imagegen generates images behind a focused, Telegram-free contract.
//
// A Backend turns a text prompt into one image. Service wraps a Backend with
// the bounds every backend needs — prompt/model validation, a global
// concurrency cap, a per-request timeout, a decoded-size limit and MIME
// sniffing — so a new backend only implements the wire format. Nothing in
// this package imports Telegram code or stores image data; callers own the
// bytes, and generated images are never cached.
//
// Adding a backend: implement Backend in its own file, add a config
// subsection and a case in NewFromConfig, and register its credentials in the
// app's secret scrubber.
package imagegen

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"telegram-ollama-reply-bot/internal/logging"
	"telegram-ollama-reply-bot/internal/provider"
	"telegram-ollama-reply-bot/internal/vision"
)

// Generation errors. Errors returned by this package wrap these sentinels or
// are *provider.Error values; messages carry only kinds, sizes and numeric
// statuses — never prompts, provider text, image bytes or URLs.
var (
	// ErrPromptEmpty reports a blank generation prompt.
	ErrPromptEmpty = errors.New("image generation prompt is empty")
	// ErrModelEmpty reports that no generation model is configured.
	ErrModelEmpty = errors.New("image generation model is empty")
	// ErrInvalidAspect reports an aspect outside the supported set.
	ErrInvalidAspect = errors.New("image generation aspect is not supported")
	// ErrMalformedResponse reports a backend answer that is not a usable
	// image: undecodable, empty, an unsupported format, or a per-image error.
	ErrMalformedResponse = errors.New("image generator returned a malformed response")
	// ErrImageTooLarge reports a generated image above the configured bound.
	ErrImageTooLarge = errors.New("generated image exceeds size bound")
)

// Aspect is the coarse shape of the requested image. Each backend maps it to
// whatever size or ratio parameters its API takes.
type Aspect string

const (
	AspectSquare    Aspect = "square"
	AspectLandscape Aspect = "landscape"
	AspectPortrait  Aspect = "portrait"
)

// Aspects lists every supported aspect, in presentation order.
var Aspects = []Aspect{AspectSquare, AspectLandscape, AspectPortrait}

// Valid reports whether a is a supported aspect.
func (a Aspect) Valid() bool {
	for _, known := range Aspects {
		if a == known {
			return true
		}
	}

	return false
}

// Request is one generation request.
type Request struct {
	Model  string
	Prompt string
	// Aspect defaults to AspectSquare when empty.
	Aspect Aspect
}

// Image is one generated image. MIME is detected from the bytes by Service;
// backend-reported types are never trusted.
type Image struct {
	Data []byte
	MIME string // image/jpeg, image/png, or image/webp
	// RevisedPrompt is the prompt the backend actually used, when it reports
	// one. It is advisory text from the provider; callers must bound it.
	RevisedPrompt string
}

// Backend generates one image per call. Implementations must not retry:
// generation is billed and not idempotent. They return *provider.Error
// values for provider-side failures and must never put provider message
// text, prompts or credentials in an error.
type Backend interface {
	// Name identifies the backend in logs and errors.
	Name() string
	// Generate renders req. The returned Image's MIME may be empty.
	Generate(ctx context.Context, req Request) (Image, error)
}

// Options bounds a Service.
type Options struct {
	// Timeout caps one Backend.Generate call. Zero disables the cap (the
	// caller's context still applies).
	Timeout time.Duration
	// MaxImageBytes caps the decoded image size. Zero disables the check.
	MaxImageBytes int64
	// MaxConcurrent caps simultaneous Backend.Generate calls. Zero means 1.
	MaxConcurrent int
}

// Service validates requests and dispatches them to a Backend.
type Service struct {
	backend Backend
	opts    Options
	slots   chan struct{}
	logger  *slog.Logger
}

// NewService builds a Service around backend.
func NewService(backend Backend, opts Options, logger *slog.Logger) *Service {
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 1
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{
		backend: backend,
		opts:    opts,
		slots:   make(chan struct{}, opts.MaxConcurrent),
		logger:  logger,
	}
}

// Name returns the active backend's name.
func (s *Service) Name() string { return s.backend.Name() }

// Timeout returns the per-request cap, or zero when uncapped.
func (s *Service) Timeout() time.Duration { return s.opts.Timeout }

// Generate renders one image. It never waits for a free slot: when the
// concurrency cap is reached it fails immediately with a rate-limited
// provider error, so queueing can never eat into the caller's turn budget.
func (s *Service) Generate(ctx context.Context, req Request) (Image, error) {
	logger := logging.FromContext(ctx, s.logger)

	if strings.TrimSpace(req.Prompt) == "" {
		return Image{}, ErrPromptEmpty
	}
	if strings.TrimSpace(req.Model) == "" {
		return Image{}, ErrModelEmpty
	}
	if req.Aspect == "" {
		req.Aspect = AspectSquare
	}
	if !req.Aspect.Valid() {
		return Image{}, ErrInvalidAspect
	}

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		logger.Warn("image generation concurrency cap reached", "backend", s.backend.Name(), "max_concurrent", s.opts.MaxConcurrent)

		return Image{}, provider.NewError(s.backend.Name(), provider.ErrorKindRateLimited)
	}

	if s.opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.opts.Timeout)
		defer cancel()
	}

	image, err := s.backend.Generate(ctx, req)
	if err != nil {
		return Image{}, err
	}

	if len(image.Data) == 0 {
		return Image{}, fmt.Errorf("%w: empty image", ErrMalformedResponse)
	}
	if s.opts.MaxImageBytes > 0 && int64(len(image.Data)) > s.opts.MaxImageBytes {
		logger.Warn("generated image exceeds size bound", "backend", s.backend.Name(), "image_bytes", len(image.Data), "max_image_bytes", s.opts.MaxImageBytes)

		return Image{}, fmt.Errorf("%w: %d bytes exceeds bound %d", ErrImageTooLarge, len(image.Data), s.opts.MaxImageBytes)
	}

	mime, err := vision.DetectMIME(image.Data)
	if err != nil {
		return Image{}, fmt.Errorf("%w: unsupported image format", ErrMalformedResponse)
	}
	image.MIME = mime

	logger.Debug(
		"image generated",
		"backend", s.backend.Name(),
		"aspect", req.Aspect,
		"mime", mime,
		"image_bytes", len(image.Data),
		"prompt_length", len(req.Prompt),
	)

	return image, nil
}
