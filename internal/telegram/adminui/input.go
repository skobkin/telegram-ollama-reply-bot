package adminui

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"

	core "telegram-ollama-reply-bot/internal/adminui"
)

// downloadClient fetches uploaded prompt documents. The bound is enforced on
// actual downloaded bytes (32 KiB + 1), never on reported metadata.
var downloadClient = &http.Client{Timeout: 30 * time.Second}

// handleAdminInput consumes replies to the bot's messages in admin DMs. The
// anti-leak contract: a reply to an admin input prompt never falls through to
// the conversation pipeline, whatever happened to its pending state; an
// ordinary reply (replied-to text present, no input marker) is positively
// distinguished and resumes the ordinary chain unchanged; anything
// unclassifiable fails closed.
func (a *Adapter) handleAdminInput(ctx *th.Context, message t.Message) error {
	adminID := message.From.ID
	reply := message.ReplyToMessage

	switch {
	case strings.HasPrefix(reply.Text, core.AdminInputMarker):
		input := core.ReplyInput{
			ReplyToMsgID:  reply.MessageID,
			TextAvailable: true,
			Text:          message.Text,
			ReplyMsgID:    message.MessageID,
		}
		if message.Document != nil {
			document, err := a.downloadDocument(ctx, message.Document)
			if err != nil {
				a.logger.Warn("admin ui document download failed", "admin_id", adminID, "error", err)
				a.sendPlain(ctx, adminID, "Failed to download the document. Try again.")

				return nil
			}
			input.Document = document
			input.Text = ""
		}

		outcome, err := a.controller.HandleReply(ctx, adminID, input)
		if err != nil {
			a.logger.Error("admin ui input failed", "admin_id", adminID, "error", err)

			return nil
		}
		a.applyOutcome(ctx, adminID, outcome)

		return nil
	case reply.Text != "":
		// Ordinary conversation: resume the chain (history, stats, LLM).
		return ctx.Next(t.Update{Message: &message})
	default:
		// The replied-to message's text is unavailable (stripped content,
		// bot media/document messages): fail closed — never the LLM.
		outcome, err := a.controller.HandleReply(ctx, adminID, core.ReplyInput{
			ReplyToMsgID:  reply.MessageID,
			TextAvailable: false,
			ReplyMsgID:    message.MessageID,
		})
		if err != nil {
			a.logger.Error("admin ui input classification failed", "admin_id", adminID, "error", err)

			return nil
		}
		a.applyOutcome(ctx, adminID, outcome)

		return nil
	}
}

// downloadDocument fetches a prompt upload bounded to PromptBodyLimit+1
// actual bytes: a full 32769-byte read means oversized content, immune to
// absent or understated FileSize metadata. Validation (.txt/.md, UTF-8) is
// the controller's job; oversized bytes never get that far.
func (a *Adapter) downloadDocument(ctx *th.Context, document *t.Document) (*core.DocumentInput, error) {
	file, err := a.api.GetFile(ctx, &t.GetFileParams{FileID: document.FileID})
	if err != nil {
		return nil, fmt.Errorf("get file: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.api.FileDownloadURL(file.FilePath), nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}

	response, err := downloadClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck // read-side cleanup

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download status %d", response.StatusCode)
	}

	content, err := io.ReadAll(io.LimitReader(response.Body, core.PromptBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	return &core.DocumentInput{
		Filename: document.FileName,
		Content:  content,
	}, nil
}
