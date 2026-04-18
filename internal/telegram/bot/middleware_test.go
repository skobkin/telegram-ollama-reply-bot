package bot

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/logging"

	tg "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
)

func TestRequestLoggerMiddlewareAddsRequestIDAndMetadata(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logManager, err := logging.NewManager(logging.Options{
		Level:  "debug",
		Writer: &buf,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	update := tg.Update{
		UpdateID: 42,
		Message: &tg.Message{
			MessageID: 11,
			Chat: tg.Chat{
				ID:   100500,
				Type: tg.ChatTypePrivate,
			},
			From: &tg.User{
				ID: 777,
			},
		},
	}

	updates := make(chan tg.Update, 1)
	updates <- update
	close(updates)

	bh, err := th.NewBotHandler(nil, updates)
	if err != nil {
		t.Fatalf("NewBotHandler: %v", err)
	}

	testBot := &Bot{
		ctx:    context.Background(),
		logger: logManager.Logger("telegram/bot"),
	}

	done := make(chan struct{})
	bh.Use(testBot.requestLogger)
	bh.Handle(func(ctx *th.Context, _ tg.Update) error {
		requestID := logging.RequestIDFromContext(ctx.Context())
		if requestID == "" {
			t.Fatal("expected request ID in context")
		}

		logger := logging.FromContext(ctx.Context(), nil)
		logger.Debug("handler reached")
		close(done)

		return nil
	})

	if err := bh.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	<-done

	output := buf.String()
	if !strings.Contains(output, `"request_id":"`) {
		t.Fatalf("expected request_id in logs: %q", output)
	}
	if !strings.Contains(output, `"pkg":"telegram/bot"`) {
		t.Fatalf("expected pkg in logs: %q", output)
	}
	if !strings.Contains(output, `"update_id":42`) {
		t.Fatalf("expected update_id in logs: %q", output)
	}
	if !strings.Contains(output, `"chat_id":100500`) {
		t.Fatalf("expected chat_id in logs: %q", output)
	}
	if !strings.Contains(output, `"from_id":777`) {
		t.Fatalf("expected from_id in logs: %q", output)
	}
}
