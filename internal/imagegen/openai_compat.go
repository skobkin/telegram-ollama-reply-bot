package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"telegram-ollama-reply-bot/internal/logging"
	"telegram-ollama-reply-bot/internal/provider"
	"telegram-ollama-reply-bot/internal/support/safeerr"
	"telegram-ollama-reply-bot/internal/support/urlredact"
)

// BackendOpenAICompat names the OpenAI-compatible images backend.
const BackendOpenAICompat = "openai_compat"

// ErrMisconfigured reports an unusable backend configuration.
var ErrMisconfigured = errors.New("image generator is misconfigured")

const (
	imagesPath = "images/generations"

	// defaultMaxImageBytes bounds the response read when the caller passes no
	// image bound: a response is never read unbounded.
	defaultMaxImageBytes = 32 << 20

	// responseOverheadBytes is the JSON envelope allowance added on top of the
	// base64 expansion of the largest accepted image.
	responseOverheadBytes = 1 << 20
)

// Doer is the HTTP transport the backend uses. *http.Client and the secret
// scrubbing wrapper both satisfy it.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// sizes maps an Aspect to the OpenAI-style size string. These three sizes are
// accepted by OpenAI's gpt-image models and by MiniMax-H3-Image alike (area
// within MiniMax's 655,360–20,000,000 px window, ratio within 1:3–3:1).
var sizes = map[Aspect]string{
	AspectSquare:    "1024x1024",
	AspectLandscape: "1536x1024",
	AspectPortrait:  "1024x1536",
}

// safeCodePattern admits short machine identifiers only, so a provider-
// controlled string can never smuggle prose (and with it prompts or secrets)
// into logs.
var safeCodePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// OpenAICompat generates images through an OpenAI-compatible
// POST {base}/images/generations endpoint (OpenAI itself, MiniMax-H3-Image,
// and gateways that mirror the API). It is not tied to any model family: the
// model name is passed through from the request.
//
// The client always asks for response_format b64_json and sends no
// provider-specific parameters. A url-only answer is treated as malformed:
// fetching provider-chosen URLs would need the SSRF-hardened content fetcher
// and is deliberately out of scope.
//
// Generation is billed and not idempotent, so there are no retries. Error
// and log text carries only the typed kind, the HTTP status and a short
// machine code: provider messages can echo prompts and are never surfaced.
// Transport errors are sanitized through safeerr with the cause discarded.
type OpenAICompat struct {
	endpoint     string
	token        string
	client       Doer
	maxBodyBytes int64
	logger       *slog.Logger
	// omitResponseFormat leaves response_format out of the request, for
	// providers (OpenAI's gpt-image models) that reject it and always return
	// base64.
	omitResponseFormat bool
}

// OmitResponseFormat stops the backend from sending response_format. The
// default stays on because providers such as MiniMax answer with a url unless
// b64_json is requested, and url-only responses are not supported.
func (b *OpenAICompat) OmitResponseFormat() *OpenAICompat {
	b.omitResponseFormat = true

	return b
}

// NewOpenAICompat builds the backend. baseURL is the OpenAI-style base
// including the version segment (for example https://api.minimax.io/v1); the
// images path is appended. maxImageBytes bounds the accepted response; zero
// or negative falls back to a conservative default.
func NewOpenAICompat(baseURL, apiToken string, client Doer, maxImageBytes int64, logger *slog.Logger) (*OpenAICompat, error) {
	trimmed := strings.TrimSpace(baseURL)

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("%w: base url is not a valid url: %s", ErrMisconfigured, urlredact.RedactOne(trimmed))
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("%w: base url scheme must be http or https: %s", ErrMisconfigured, urlredact.RedactOne(trimmed))
	}
	if strings.TrimSpace(apiToken) == "" {
		return nil, fmt.Errorf("%w: api token is required", ErrMisconfigured)
	}
	if client == nil {
		return nil, fmt.Errorf("%w: http client is required", ErrMisconfigured)
	}
	if maxImageBytes <= 0 {
		maxImageBytes = defaultMaxImageBytes
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &OpenAICompat{
		endpoint: parsed.JoinPath(imagesPath).String(),
		token:    apiToken,
		client:   client,
		// base64 expands 3 bytes to 4; add the JSON envelope allowance.
		maxBodyBytes: maxImageBytes/3*4 + 4 + responseOverheadBytes,
		logger:       logger,
	}, nil
}

// Name returns the backend name.
func (b *OpenAICompat) Name() string { return BackendOpenAICompat }

// Generate renders one image.
func (b *OpenAICompat) Generate(ctx context.Context, req Request) (Image, error) {
	logger := logging.FromContext(ctx, b.logger)

	size, ok := sizes[req.Aspect]
	if !ok {
		return Image{}, ErrInvalidAspect
	}

	payload := struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              int    `json:"n"`
		Size           string `json:"size"`
		ResponseFormat string `json:"response_format,omitempty"`
	}{
		Model:          req.Model,
		Prompt:         req.Prompt,
		N:              1,
		Size:           size,
		ResponseFormat: "b64_json",
	}
	if b.omitResponseFormat {
		payload.ResponseFormat = ""
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Image{}, provider.NewError(b.Name(), provider.ErrorKindUnknown)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(body))
	if err != nil {
		return Image{}, provider.NewError(b.Name(), provider.ErrorKindMisconfigured)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+b.token)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return Image{}, safeerr.WrapTransport(provider.NewError(b.Name(), provider.ErrorKindUpstreamUnavailable), err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bound the read before looking at the status: a huge error body must not
	// be buffered whatever the status code is.
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, b.maxBodyBytes+1))
	if readErr != nil {
		return Image{}, safeerr.WrapTransport(provider.NewError(b.Name(), provider.ErrorKindUpstreamUnavailable), readErr)
	}
	if int64(len(raw)) > b.maxBodyBytes {
		return Image{}, fmt.Errorf("%w: response exceeds %d bytes", ErrMalformedResponse, b.maxBodyBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		errType, errCode := parseErrorBody(raw)
		kind := classifyFailure(resp.StatusCode, errType, errCode)
		logger.Warn(
			"image generation request failed",
			"backend", b.Name(),
			"http_status", resp.StatusCode,
			"provider_error_type", safeCode(errType),
			"provider_error_code", safeCode(errCode),
			"kind", kind,
		)

		return Image{}, provider.NewError(b.Name(), kind)
	}

	return b.parseSuccess(logger, raw)
}

// parseSuccess extracts the first image from a 2xx body. HTTP 200 does not
// imply success for the item: a provider may report a per-image error beside
// a 200, so the item is checked before its payload.
func (b *OpenAICompat) parseSuccess(logger *slog.Logger, raw []byte) (Image, error) {
	var body struct {
		Data []struct {
			B64JSON       string          `json:"b64_json"`
			URL           string          `json:"url"`
			RevisedPrompt string          `json:"revised_prompt"`
			Error         json.RawMessage `json:"error"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || len(body.Data) == 0 {
		return Image{}, fmt.Errorf("%w: no image in response", ErrMalformedResponse)
	}

	item := body.Data[0]
	if hasValue(item.Error) {
		errType, errCode := parseErrorObject(item.Error)
		kind := classifyFailure(http.StatusOK, errType, errCode)
		logger.Warn(
			"image generation item failed",
			"backend", b.Name(),
			"provider_error_type", safeCode(errType),
			"provider_error_code", safeCode(errCode),
			"kind", kind,
		)

		return Image{}, provider.NewError(b.Name(), kind)
	}

	if strings.TrimSpace(item.B64JSON) == "" {
		// A url-only answer lands here too; see the type comment.
		return Image{}, fmt.Errorf("%w: response has no b64_json image", ErrMalformedResponse)
	}
	data, err := base64.StdEncoding.DecodeString(item.B64JSON)
	if err != nil {
		return Image{}, fmt.Errorf("%w: image is not valid base64", ErrMalformedResponse)
	}

	return Image{Data: data, RevisedPrompt: item.RevisedPrompt}, nil
}

// classifyFailure maps an HTTP status plus the provider's machine-readable
// error type/code to a provider error kind. Message text is never consulted.
// MiniMax reports content-safety rejections as HTTP 422 (codes 1026/1027) and
// OpenAI as 400 with code content_policy_violation or moderation_blocked.
func classifyFailure(status int, errType, errCode string) provider.ErrorKind {
	switch strings.ToLower(errCode) {
	case "content_policy_violation", "moderation_blocked", "1026", "1027":
		return provider.ErrorKindContentRejected
	}

	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return provider.ErrorKindAuthFailed
	case status == http.StatusPaymentRequired:
		return provider.ErrorKindCreditsExhausted
	case status == http.StatusUnprocessableEntity:
		return provider.ErrorKindContentRejected
	case status == http.StatusTooManyRequests:
		return provider.ErrorKindRateLimited
	case status == http.StatusBadRequest:
		return provider.ErrorKindBadRequest
	case status == http.StatusRequestTimeout, status >= 500:
		return provider.ErrorKindUpstreamUnavailable
	case status >= 400:
		return provider.ErrorKindBadRequest
	default:
		// A per-item error beside HTTP 200 (or a status we cannot place):
		// a timeout-like item code is transient, anything else unknown.
		if strings.EqualFold(errType, "server_error") || errCode == "1000" || errCode == "1001" {
			return provider.ErrorKindUpstreamUnavailable
		}

		return provider.ErrorKindUnknown
	}
}

// parseErrorBody reads error.type and error.code from a non-2xx body, both
// OpenAI-shaped ({"error":{"type","code"}}) and MiniMax-shaped
// ({"type":"error","error":{"type","message","http_code"}}). An undecodable
// body yields empty strings; classification then rests on the status alone.
func parseErrorBody(raw []byte) (errType, errCode string) {
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", ""
	}

	return parseErrorObject(body.Error)
}

func parseErrorObject(raw json.RawMessage) (errType, errCode string) {
	if !hasValue(raw) {
		return "", ""
	}
	var obj struct {
		Type json.RawMessage `json:"type"`
		Code json.RawMessage `json:"code"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", ""
	}

	return scalarString(obj.Type), scalarString(obj.Code)
}

// scalarString renders a JSON string or number as text and anything else as
// empty. Provider codes arrive as either (MiniMax uses integers, OpenAI
// strings).
func scalarString(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	if _, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return trimmed
	}

	return ""
}

func hasValue(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))

	return trimmed != "" && trimmed != "null"
}

// safeCode returns s when it is a short machine identifier and "" otherwise.
func safeCode(s string) string {
	if safeCodePattern.MatchString(s) {
		return s
	}

	return ""
}
