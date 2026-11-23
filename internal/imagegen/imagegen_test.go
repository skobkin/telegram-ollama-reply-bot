package imagegen

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/provider"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// stubBackend is a Backend driven by a function.
type stubBackend struct {
	generate func(ctx context.Context, req Request) (Image, error)
}

func (stubBackend) Name() string { return "stub" }

func (b stubBackend) Generate(ctx context.Context, req Request) (Image, error) {
	return b.generate(ctx, req)
}

func okBackend(t testing.TB) stubBackend {
	t.Helper()

	data := pngBytes(t)

	return stubBackend{generate: func(context.Context, Request) (Image, error) {
		return Image{Data: data, MIME: "image/x-lying", RevisedPrompt: "revised"}, nil
	}}
}

func validRequest() Request {
	return Request{Model: "m", Prompt: "a cat", Aspect: AspectSquare}
}

func TestServiceGenerateSniffsMIMEFromBytes(t *testing.T) {
	svc := NewService(okBackend(t), Options{}, discardLogger)

	got, err := svc.Generate(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got.MIME != "image/png" {
		t.Fatalf("expected the MIME detected from bytes, got %q", got.MIME)
	}
	if got.RevisedPrompt != "revised" {
		t.Fatalf("revised prompt must pass through, got %q", got.RevisedPrompt)
	}
}

func TestServiceValidatesRequestBeforeDispatch(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		want error
	}{
		{"blank prompt", Request{Model: "m", Prompt: "  \n", Aspect: AspectSquare}, ErrPromptEmpty},
		{"blank model", Request{Model: " ", Prompt: "p", Aspect: AspectSquare}, ErrModelEmpty},
		{"unknown aspect", Request{Model: "m", Prompt: "p", Aspect: "wide"}, ErrInvalidAspect},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			svc := NewService(stubBackend{generate: func(context.Context, Request) (Image, error) {
				called = true

				return Image{}, nil
			}}, Options{}, discardLogger)

			if _, err := svc.Generate(context.Background(), tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			if called {
				t.Fatal("the backend must not be called for an invalid request")
			}
		})
	}
}

func TestServiceDefaultsAspectToSquare(t *testing.T) {
	var seen Aspect
	data := pngBytes(t)
	svc := NewService(stubBackend{generate: func(_ context.Context, req Request) (Image, error) {
		seen = req.Aspect

		return Image{Data: data}, nil
	}}, Options{}, discardLogger)

	if _, err := svc.Generate(context.Background(), Request{Model: "m", Prompt: "p"}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if seen != AspectSquare {
		t.Fatalf("expected the default aspect square, got %q", seen)
	}
}

func TestServiceRejectsUnusableImages(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		opts Options
		want error
	}{
		{"empty", nil, Options{}, ErrMalformedResponse},
		{"not an image", []byte("<html>definitely not an image</html>"), Options{}, ErrMalformedResponse},
		{"unsupported gif", []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), Options{}, ErrMalformedResponse},
		{"over size bound", pngBytes(t), Options{MaxImageBytes: 8}, ErrImageTooLarge},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewService(stubBackend{generate: func(context.Context, Request) (Image, error) {
				return Image{Data: tc.data}, nil
			}}, tc.opts, discardLogger)

			if _, err := svc.Generate(context.Background(), validRequest()); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
		})
	}
}

func TestServicePassesBackendErrorsThrough(t *testing.T) {
	want := provider.NewError("stub", provider.ErrorKindContentRejected)
	svc := NewService(stubBackend{generate: func(context.Context, Request) (Image, error) {
		return Image{}, want
	}}, Options{}, discardLogger)

	_, err := svc.Generate(context.Background(), validRequest())
	if got := provider.KindOf(err); got != provider.ErrorKindContentRejected {
		t.Fatalf("expected the backend error to pass through, got %v", err)
	}
}

func TestServiceConcurrencyCapFailsFastWithoutQueueing(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	data := pngBytes(t)
	svc := NewService(stubBackend{generate: func(context.Context, Request) (Image, error) {
		started <- struct{}{}
		<-release

		return Image{Data: data}, nil
	}}, Options{MaxConcurrent: 1}, discardLogger)

	done := make(chan error, 1)
	go func() {
		_, err := svc.Generate(context.Background(), validRequest())
		done <- err
	}()
	<-started

	begin := time.Now()
	_, err := svc.Generate(context.Background(), validRequest())
	if got := provider.KindOf(err); got != provider.ErrorKindRateLimited {
		t.Fatalf("expected rate_limited while the cap is reached, got %v", err)
	}
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("a local cap refusal must be identifiable as ErrBusy, got %v", err)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("the capped call must fail fast, took %s", elapsed)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first generation: %v", err)
	}

	// The slot is released: a later call goes through.
	svc2 := NewService(okBackend(t), Options{MaxConcurrent: 1}, discardLogger)
	for range 3 {
		if _, err := svc2.Generate(context.Background(), validRequest()); err != nil {
			t.Fatalf("sequential generation after release: %v", err)
		}
	}
}

func TestServiceReleasesSlotAfterBackendFailure(t *testing.T) {
	calls := 0
	data := pngBytes(t)
	svc := NewService(stubBackend{generate: func(context.Context, Request) (Image, error) {
		calls++
		if calls == 1 {
			return Image{}, provider.NewError("stub", provider.ErrorKindUpstreamUnavailable)
		}

		return Image{Data: data}, nil
	}}, Options{MaxConcurrent: 1}, discardLogger)

	if _, err := svc.Generate(context.Background(), validRequest()); err == nil {
		t.Fatal("expected the first call to fail")
	}
	if _, err := svc.Generate(context.Background(), validRequest()); err != nil {
		t.Fatalf("the slot must be released after a failure: %v", err)
	}
}

func TestServiceAppliesTimeout(t *testing.T) {
	svc := NewService(stubBackend{generate: func(ctx context.Context, _ Request) (Image, error) {
		<-ctx.Done()

		return Image{}, ctx.Err()
	}}, Options{Timeout: 20 * time.Millisecond}, discardLogger)

	begin := time.Now()
	_, err := svc.Generate(context.Background(), validRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the service timeout to cancel the backend, got %v", err)
	}
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Fatalf("timeout was not applied promptly: %s", elapsed)
	}
	if svc.Timeout() != 20*time.Millisecond {
		t.Fatalf("unexpected Timeout(): %s", svc.Timeout())
	}
}

func TestServiceExposesBackendName(t *testing.T) {
	if got := NewService(okBackend(t), Options{}, nil).Name(); got != "stub" {
		t.Fatalf("unexpected name: %q", got)
	}
}

func TestAspectValid(t *testing.T) {
	for _, a := range Aspects {
		if !a.Valid() {
			t.Fatalf("%q must be valid", a)
		}
	}
	for _, bad := range []Aspect{"", "wide", "SQUARE"} {
		if bad.Valid() {
			t.Fatalf("%q must be invalid", bad)
		}
	}
}

func TestNewFromConfigDisabledReturnsNil(t *testing.T) {
	svc, err := NewFromConfig(config.ImageGenerationConfig{}, http.DefaultClient, nil)
	if err != nil || svc != nil {
		t.Fatalf("expected (nil, nil) for a disabled feature, got (%v, %v)", svc, err)
	}
}

func TestNewFromConfigBuildsOpenAICompat(t *testing.T) {
	svc, err := NewFromConfig(config.ImageGenerationConfig{
		Backend:       config.ImageGenerationBackendOpenAICompat,
		OpenAICompat:  config.ImageGenerationOpenAICompatConfig{BaseURL: "https://images.example/v1", APIToken: testToken},
		Timeout:       45 * time.Second,
		MaxImageBytes: 1 << 20,
		MaxConcurrent: 3,
	}, http.DefaultClient, nil)
	if err != nil {
		t.Fatalf("new from config: %v", err)
	}
	if svc == nil || svc.Name() != BackendOpenAICompat {
		t.Fatalf("unexpected service: %+v", svc)
	}
	if svc.Timeout() != 45*time.Second {
		t.Fatalf("unexpected timeout: %s", svc.Timeout())
	}
	if cap(svc.slots) != 3 {
		t.Fatalf("expected a concurrency cap of 3, got %d", cap(svc.slots))
	}
}

func TestNewFromConfigRejectsBadConfiguration(t *testing.T) {
	cases := map[string]config.ImageGenerationConfig{
		"unknown backend": {Backend: "dalle9"},
		"missing url": {
			Backend:      config.ImageGenerationBackendOpenAICompat,
			OpenAICompat: config.ImageGenerationOpenAICompatConfig{APIToken: testToken},
		},
		"missing token": {
			Backend:      config.ImageGenerationBackendOpenAICompat,
			OpenAICompat: config.ImageGenerationOpenAICompatConfig{BaseURL: "https://images.example/v1"},
		},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			svc, err := NewFromConfig(cfg, http.DefaultClient, nil)
			if !errors.Is(err, ErrMisconfigured) || svc != nil {
				t.Fatalf("expected ErrMisconfigured and no service, got (%v, %v)", svc, err)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("error leaked the token: %v", err)
			}
		})
	}
}
