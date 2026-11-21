package adminui

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mymmrac/telego/telegoapi"
	th "github.com/mymmrac/telego/telegohandler"

	core "telegram-ollama-reply-bot/internal/adminui"
	"telegram-ollama-reply-bot/internal/support/safeerr"
)

// archiveUploadTimeout bounds the sendDocument phase on its own; the archive
// construction deadline (service) ends before the upload starts and does not
// cover it. A variable so tests can shorten it.
var archiveUploadTimeout = 5 * time.Minute

const msgArchiveBusy = "Another ZIP export is still being prepared or delivered — try again once it has arrived."

// runStickerArchive runs one requested ZIP export outside every session lock
// and delivers its result. The gate is held from the first download to the
// end of the document upload and is never waited on: a second request is
// refused at once, so no second large export starts while one is in flight.
func (a *Adapter) runStickerArchive(ctx *th.Context, adminID int64, job core.StickerArchiveJob) {
	select {
	case a.archiveGate <- struct{}{}:
		defer func() { <-a.archiveGate }()
	default:
		a.sendPlain(ctx, adminID, msgArchiveBusy)

		return
	}

	outcome := a.controller.RunStickerArchive(ctx, adminID, job)
	if outcome.Document == nil {
		a.applyOutcome(ctx, adminID, outcome)

		return
	}
	a.deliverArchive(ctx, adminID, job.Set, outcome.Document)
}

// deliverArchive uploads the archive and, unlike other documents, reports a
// failed upload to the administrator. Causes are never shown: transport
// errors can embed token-bearing URLs. A definite Telegram rejection says
// nothing was delivered; anything else is ambiguous (the upload may have
// completed), so the admin is told to check the chat and the archive is
// never resent automatically.
func (a *Adapter) deliverArchive(ctx *th.Context, adminID int64, set string, payload *core.DocumentPayload) {
	uploadCtx, cancel := context.WithTimeout(ctx, archiveUploadTimeout)
	defer cancel()

	message, err := a.sendDocument(uploadCtx, adminID, payload)
	if err == nil && message != nil && message.MessageID > 0 {
		return // confirmed: Telegram returned the sent message
	}
	if err == nil {
		// No error but no sent message either: not a confirmation.
		a.logger.Warn("admin ui archive send unconfirmed", "admin_id", adminID, "set", set)
		a.sendPlain(ctx, adminID, unconfirmedArchiveText(set))

		return
	}
	a.logger.Warn("admin ui archive send failed", "admin_id", adminID, "set", set,
		"timed_out", errors.Is(uploadCtx.Err(), context.DeadlineExceeded),
		"error", safeerr.WrapTransport(errAdminDocumentSend, err))

	var apiErr *telegoapi.Error
	if errors.As(err, &apiErr) && apiErr.ErrorCode >= 400 && apiErr.ErrorCode < 500 {
		a.sendPlain(ctx, adminID, fmt.Sprintf(
			"ZIP export of %s was not delivered: Telegram rejected the upload (error %d). Nothing was sent; you can try again.",
			set, apiErr.ErrorCode))

		return
	}
	a.sendPlain(ctx, adminID, unconfirmedArchiveText(set))
}

func unconfirmedArchiveText(set string) string {
	return fmt.Sprintf("Delivery of the ZIP of %s could not be confirmed. Check this chat for the archive before retrying — it was not resent automatically.", set)
}
