package transport

import (
	"net/http"
	"telegram-ollama-reply-bot/internal/support/httpclient"
)

func NewHTTPClient() *http.Client {
	return httpclient.New()
}
