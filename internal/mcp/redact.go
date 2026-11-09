package mcp

import "telegram-ollama-reply-bot/internal/support/urlredact"

// redactURLCredentials strips the userinfo, query string, and fragment from
// every http(s) URL embedded in s, so transport errors that quote the
// request URL never carry credentials into WARN logs or the LLM-visible
// tool response. The implementation lives in the shared urlredact package
// so config validation errors get the same treatment.
func redactURLCredentials(s string) string {
	return urlredact.Redact(s)
}
