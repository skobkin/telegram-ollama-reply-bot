package adminui

import "errors"

// Typed names for Telegram API failures in the admin adapter. Raw transport
// errors can embed the token-bearing Bot API URL, so every log and report
// site passes its error through safeerr.WrapTransport with one of these
// kinds: only the typed name (plus the context.Canceled /
// context.DeadlineExceeded signals) survives — never the URL, the token, or
// the response body. Controller and session errors never carry transport
// detail and are logged as-is. See internal/support/safeerr and the download
// path in input.go for the established convention.
var (
	errAdminScreenUpdate   = errors.New("admin ui screen update failed")
	errAdminPromptSend     = errors.New("admin ui input prompt send failed")
	errAdminMessageSend    = errors.New("admin ui message send failed")
	errAdminDocumentSend   = errors.New("admin ui document send failed")
	errAdminMessageCleanup = errors.New("admin ui message cleanup failed")
	errAdminCallbackAck    = errors.New("admin ui callback acknowledgement failed")
	errAdminMenuSync       = errors.New("admin ui command menu sync failed")
	errAdminChatVerify     = errors.New("admin ui chat verification failed")
)
