package logging

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		wantErr bool
		want    slog.Level
	}{
		{name: "debug", input: "debug", want: slog.LevelDebug},
		{name: "info", input: "info", want: slog.LevelInfo},
		{name: "empty defaults to info", input: "", want: slog.LevelInfo},
		{name: "warn", input: "warn", want: slog.LevelWarn},
		{name: "warning alias", input: "warning", want: slog.LevelWarn},
		{name: "error", input: "error", want: slog.LevelError},
		{name: "invalid", input: "verbose", wantErr: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseLevel(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q", tc.input)
				}

				return
			}
			if err != nil {
				t.Fatalf("parseLevel(%q) unexpected error: %v", tc.input, err)
			}

			gotLevel := got.Level()
			if gotLevel != tc.want {
				t.Fatalf("parseLevel(%q) = %v, want %v", tc.input, gotLevel, tc.want)
			}
		})
	}
}

func TestManagerLoggerAddsScopedAttributes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	mgr, err := NewManager(Options{Writer: &buf, Level: "debug"})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	mgr.Logger("telegram/bot").InfoContext(context.Background(), "hello", "chat_id", int64(42))

	out := buf.String()
	if !strings.Contains(out, `"pkg":"telegram/bot"`) {
		t.Fatalf("expected pkg attribute in output: %q", out)
	}
	if !strings.Contains(out, `"chat_id":42`) {
		t.Fatalf("expected contextual attribute in output: %q", out)
	}
}

func TestManagerConfigureReplacesLoggerBehavior(t *testing.T) {
	t.Parallel()

	var initial bytes.Buffer
	mgr, err := NewManager(Options{Writer: &initial, Level: "info"})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	mgr.Logger("internal/test").Debug("hidden")
	if initial.Len() != 0 {
		t.Fatalf("expected debug log to be filtered before reconfigure, got %q", initial.String())
	}

	var updated bytes.Buffer
	if err := mgr.Configure(Options{Writer: &updated, Level: "debug"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	mgr.Logger("internal/test").Debug("visible")
	if !strings.Contains(updated.String(), `"msg":"visible"`) {
		t.Fatalf("expected reconfigured logger output, got %q", updated.String())
	}
}

func TestManagerConfigureSetDefaultIsExplicit(t *testing.T) {
	t.Parallel()

	original := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(original)
	})

	var buf bytes.Buffer
	mgr, err := NewManager(Options{Writer: &buf, Level: "info"})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if slog.Default() == mgr.Logger("internal/test") {
		t.Fatal("expected scoped logger to differ from global default")
	}

	if err := mgr.Configure(Options{Writer: &buf, Level: "debug"}); err != nil {
		t.Fatalf("Configure without SetDefault: %v", err)
	}

	before := slog.Default()
	if err := mgr.Configure(Options{Writer: &buf, Level: "debug", SetDefault: true}); err != nil {
		t.Fatalf("Configure with SetDefault: %v", err)
	}
	if slog.Default() == before {
		t.Fatal("expected global default logger to be replaced")
	}
}

func TestWithRequestLoggerStoresRequestID(t *testing.T) {
	t.Parallel()

	ctx := WithRequestLogger(context.Background(), slog.Default(), "abcd1234efgh5678")
	if got := RequestIDFromContext(ctx); got != "abcd1234efgh5678" {
		t.Fatalf("RequestIDFromContext() = %q, want %q", got, "abcd1234efgh5678")
	}

	logger := FromContext(ctx, nil)
	if logger == nil {
		t.Fatal("expected logger in context")
	}
}

func TestNewRequestIDFormat(t *testing.T) {
	t.Parallel()

	requestID, err := NewRequestID()
	if err != nil {
		t.Fatalf("NewRequestID: %v", err)
	}

	matched, err := regexp.MatchString("^[0-9a-f]{16}$", requestID)
	if err != nil {
		t.Fatalf("regexp.MatchString: %v", err)
	}
	if !matched {
		t.Fatalf("unexpected request ID format: %q", requestID)
	}
}
