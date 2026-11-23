package app

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/imagegen"
	"telegram-ollama-reply-bot/internal/tooluse"
)

func newImageTestRuntime() *tooluse.Runtime {
	return tooluse.New(nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), tooluse.Config{})
}

func enabledImageGenerationConfig() *config.Config {
	cfg := &config.Config{}
	cfg.ImageGeneration = config.ImageGenerationConfig{
		Backend:       config.ImageGenerationBackendOpenAICompat,
		OpenAICompat:  config.ImageGenerationOpenAICompatConfig{BaseURL: "https://images.example/v1", APIToken: "TOKENFIXTURE"},
		Timeout:       60 * time.Second,
		MaxImageBytes: 10_000_000,
		MaxConcurrent: 2,
		ChatCooldown:  time.Minute,
	}
	cfg.Bot.ProcessingTimeout = 90 * time.Second

	return cfg
}

// A disabled feature yields a nil *imagegen.Service. Passing that nil pointer
// on as the tool's interface would register a tool whose every call crashes,
// so the helper must keep the tool unregistered.
func TestAttachImageGenerationKeepsToolAbsentForANilService(t *testing.T) {
	t.Parallel()

	disabledSvc, err := imagegen.NewFromConfig(config.ImageGenerationConfig{}, http.DefaultClient, nil)
	if err != nil || disabledSvc != nil {
		t.Fatalf("a disabled config must yield no service, got (%v, %v)", disabledSvc, err)
	}

	runtime := newImageTestRuntime()
	attachImageGeneration(runtime, disabledSvc, time.Minute)
	if runtime.HasTool("generate_image") {
		t.Fatal("generate_image must stay unregistered without a generation service")
	}
}

func TestAttachImageGenerationRegistersToolForARealService(t *testing.T) {
	t.Parallel()

	cfg := enabledImageGenerationConfig()
	svc, err := imagegen.NewFromConfig(cfg.ImageGeneration, http.DefaultClient, nil)
	if err != nil || svc == nil {
		t.Fatalf("an enabled config must yield a service, got (%v, %v)", svc, err)
	}

	runtime := newImageTestRuntime()
	attachImageGeneration(runtime, svc, cfg.ImageGeneration.ChatCooldown)
	if !runtime.HasTool("generate_image") {
		t.Fatal("generate_image must be registered once a service exists")
	}
}

func TestLogImageGenerationConfigDisabled(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logImageGenerationConfig(slog.New(slog.NewTextHandler(&logs, nil)), &config.Config{})

	if !strings.Contains(logs.String(), "image generation disabled") || strings.Contains(logs.String(), "WARN") {
		t.Fatalf("unexpected log output: %s", logs.String())
	}
}

func TestLogImageGenerationConfigNeverLogsEndpointOrToken(t *testing.T) {
	t.Parallel()

	cfg := enabledImageGenerationConfig()
	cfg.ImageGeneration.OpenAICompat.BaseURL = "https://ops:URLPASSFIXTURE@images.example/v1"

	var logs bytes.Buffer
	logImageGenerationConfig(slog.New(slog.NewTextHandler(&logs, nil)), cfg)

	out := logs.String()
	if !strings.Contains(out, "image generation configured") || !strings.Contains(out, "openai_compat") {
		t.Fatalf("expected the configured backend to be logged: %s", out)
	}
	for _, secret := range []string{"TOKENFIXTURE", "URLPASSFIXTURE", "images.example"} {
		if strings.Contains(out, secret) {
			t.Fatalf("startup log leaked %q: %s", secret, out)
		}
	}
	if strings.Contains(out, "WARN") {
		t.Fatalf("a 90s processing timeout fits a 60s generation plus the model calls, no warning expected: %s", out)
	}
}

func TestImageGenerationDeadlineShortfall(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		enabled    bool
		processing time.Duration
		timeout    time.Duration
		wantShort  bool
		wantRec    time.Duration
	}{
		{"default 30s turn cannot fit a 60s generation", true, 30 * time.Second, 60 * time.Second, true, 90 * time.Second},
		{"exactly the recommendation fits", true, 90 * time.Second, 60 * time.Second, false, 90 * time.Second},
		{"just below the recommendation", true, 89 * time.Second, 60 * time.Second, true, 90 * time.Second},
		{"a short generation timeout fits the default turn", true, 60 * time.Second, 20 * time.Second, false, 50 * time.Second},
		{"no turn deadline means nothing to warn about", true, 0, 60 * time.Second, false, 0},
		{"disabled feature never warns", false, 10 * time.Second, 60 * time.Second, false, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := enabledImageGenerationConfig()
			if !tc.enabled {
				cfg.ImageGeneration.Backend = ""
			}
			cfg.Bot.ProcessingTimeout = tc.processing
			cfg.ImageGeneration.Timeout = tc.timeout

			recommended, short := imageGenerationDeadlineShortfall(cfg)
			if short != tc.wantShort || recommended != tc.wantRec {
				t.Fatalf("shortfall = (%s, %v), want (%s, %v)", recommended, short, tc.wantRec, tc.wantShort)
			}
		})
	}
}

func TestLogImageGenerationConfigWarnsWhenTheTurnDeadlineIsTooShort(t *testing.T) {
	t.Parallel()

	cfg := enabledImageGenerationConfig()
	cfg.Bot.ProcessingTimeout = 30 * time.Second

	var logs bytes.Buffer
	logImageGenerationConfig(slog.New(slog.NewTextHandler(&logs, nil)), cfg)

	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "BOT__PROCESSING_TIMEOUT") {
		t.Fatalf("expected a processing-timeout warning: %s", out)
	}
	if !strings.Contains(out, "recommended_minimum=1m30s") {
		t.Fatalf("the warning must state the recommended minimum: %s", out)
	}
}
