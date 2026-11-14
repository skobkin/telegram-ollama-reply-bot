package vision

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
	"strconv"
	"strings"
	"time"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/logging"
	"telegram-ollama-reply-bot/internal/support/httpclient"
	"telegram-ollama-reply-bot/internal/support/safeerr"
	"telegram-ollama-reply-bot/internal/support/urlredact"
)

// MiniMax recognizer errors.
var (
	ErrRecognizerConfigured  = errors.New("image recognizer is misconfigured")
	ErrRecognizerUnavailable = errors.New("image recognizer is unavailable")
	ErrRecognizerAuth        = errors.New("image recognizer rejected credentials")
	ErrRecognizerStatus      = errors.New("image recognizer returned an error status")
	ErrRecognizerMalformed   = errors.New("image recognizer returned a malformed response")
	ErrRecognizerEmpty       = errors.New("image recognizer returned an empty response")
)

const (
	// minimaxVLMPath is the MiniMax Coding Plan vision endpoint appended to
	// the configured base URL. The API takes no model: the M Plan
	// subscription tied to the API key selects it.
	minimaxVLMPath = "/v1/coding_plan/vlm"

	// maxResponseBytes caps the VLM response body read. Descriptions are short
	// texts; a larger body is rejected as malformed instead of buffered.
	maxResponseBytes = 1 << 20 // 1 MiB

	// minimaxAuthStatus is base_resp.status_code 1004: the API key is invalid
	// or does not match the endpoint's region.
	minimaxAuthStatus = 1004

	// minimaxAuthProbePNGBase64 is a 1x1 transparent PNG used as the smallest
	// possible image payload for the startup credential probe.
	minimaxAuthProbePNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
)

// MiniMaxRecognizer describes images through the MiniMax Coding Plan VLM API.
//
// The wire format was verified against the official
// MiniMax-AI/MiniMax-Coding-Plan-MCP client (client.py / server.py /
// utils.py): POST {base}/v1/coding_plan/vlm with a JSON body of {"prompt",
// "image_url": data URL} and Bearer auth. Success requires a 2xx HTTP status
// AND a present, integer base_resp.status_code == 0 AND non-empty content;
// a missing, null, or mistyped status_code must never default to success.
// The response carries no usage fields, so Recognition.Usage is always nil.
//
// Error and log text carries only the typed kind, the HTTP status, and the
// numeric provider status: provider messages can echo prompts or secrets and
// are never surfaced. Transport errors are sanitized via safeerr with the
// cause discarded — Sentry traverses error chains, so no raw SDK error is
// retained anywhere.
type MiniMaxRecognizer struct {
	endpoint *url.URL
	apiKey   string
	client   *http.Client
	logger   *slog.Logger
}

// NewMiniMaxRecognizer builds a recognizer for the MiniMax VLM endpoint.
// baseURL must be an http(s) URL — https://api.minimax.io (global) or
// https://api.minimaxi.com (China); the region must match the key. timeout is
// the per-request budget.
func NewMiniMaxRecognizer(baseURL string, apiKey string, timeout time.Duration, logger *slog.Logger) (*MiniMaxRecognizer, error) {
	trimmed := strings.TrimSpace(baseURL)

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("%w: base url is not a valid url: %s", ErrRecognizerConfigured, urlredact.RedactOne(trimmed))
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("%w: base url scheme must be http or https: %s", ErrRecognizerConfigured, urlredact.RedactOne(trimmed))
	}

	if timeout <= 0 {
		return nil, fmt.Errorf("%w: timeout must be positive", ErrRecognizerConfigured)
	}

	client := httpclient.New()
	client.Timeout = timeout

	return &MiniMaxRecognizer{
		endpoint: parsed.JoinPath(minimaxVLMPath),
		apiKey:   apiKey,
		client:   client,
		logger:   logger,
	}, nil
}

func (r *MiniMaxRecognizer) Name() string { return config.ImageRecognizerMiniMaxVLM }

// ValidateAuth probes the configured API key with a minimal recognition call
// carrying a 1x1 PNG. Only ErrRecognizerAuth (provider status 1004) proves
// the key is rejected: every other outcome — an image rejected for its size,
// a malformed response, a network failure, a rate limit — is inconclusive
// about the credentials and treated as transient by startup validation. The
// returned errors are the recognizer's regular sanitized errors and never
// carry the key.
func (r *MiniMaxRecognizer) ValidateAuth(ctx context.Context) error {
	probe, err := minimaxAuthProbeImage()
	if err != nil {
		return err
	}

	_, err = r.Recognize(ctx, probe, "ping")

	return err
}

// minimaxAuthProbeImage decodes the embedded probe PNG. The constant is
// compile-time data, so an error here means the shipped constant is broken.
func minimaxAuthProbeImage() (Image, error) {
	data, err := base64.StdEncoding.DecodeString(minimaxAuthProbePNGBase64)
	if err != nil {
		return Image{}, fmt.Errorf("%w: cannot decode auth probe image", ErrRecognizerConfigured)
	}

	return Image{Data: data, MIME: "image/png"}, nil
}

func (r *MiniMaxRecognizer) Supports(mime string) bool {
	_, ok := supportedMIME[mime]

	return ok
}

func (r *MiniMaxRecognizer) RequiresModel() bool { return false }

func (r *MiniMaxRecognizer) Recognize(ctx context.Context, image Image, prompt string) (Recognition, error) {
	logger := logging.FromContext(ctx, r.logger)

	if strings.TrimSpace(prompt) == "" {
		return Recognition{}, ErrPromptEmpty
	}

	payload := struct {
		Prompt   string `json:"prompt"`
		ImageURL string `json:"image_url"`
	}{
		Prompt:   prompt,
		ImageURL: "data:" + image.MIME + ";base64," + base64.StdEncoding.EncodeToString(image.Data),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return Recognition{}, fmt.Errorf("%w: cannot encode request", ErrRecognizerUnavailable)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Recognition{}, fmt.Errorf("%w: cannot build request", ErrRecognizerUnavailable)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.apiKey)

	resp, err := r.client.Do(req)
	if err != nil {
		return Recognition{}, safeerr.WrapTransport(ErrRecognizerUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bound the read before touching the status: a huge error body must not
	// be buffered regardless of the status code.
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if readErr != nil {
		return Recognition{}, safeerr.WrapTransport(ErrRecognizerUnavailable, readErr)
	}
	if len(raw) > maxResponseBytes {
		return Recognition{}, fmt.Errorf("%w: response exceeds %d bytes", ErrRecognizerMalformed, maxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// An explicit credential rejection can arrive on any HTTP status: the
		// provider body is checked before the status verdict, so a 401/403
		// carrying base_resp status_code 1004 classifies as auth — startup
		// validation must fail a rejected key instead of waving it through as
		// a transient outage. Only a body without the rejection marker stays
		// transient (unavailable).
		if code, ok := minimaxProviderStatusCode(raw); ok && code == minimaxAuthStatus {
			logger.Warn("minimax vlm request rejected", "http_status", resp.StatusCode, "provider_status", code)

			return Recognition{}, fmt.Errorf("%w: provider status %d", ErrRecognizerAuth, code)
		}

		logger.Warn("minimax vlm request failed", "http_status", resp.StatusCode)

		return Recognition{}, fmt.Errorf("%w: http status %d", ErrRecognizerUnavailable, resp.StatusCode)
	}

	text, err := parseVLMResponse(raw)
	if err != nil {
		return Recognition{}, err
	}

	return Recognition{Text: text}, nil
}

// parseVLMResponse validates a MiniMax VLM success body and returns the
// content. Presence and type are checked strictly through json.RawMessage so
// absent, null, and wrongly-typed fields are distinguishable: a missing
// base_resp, a missing/null/non-integer status_code, missing or blank
// content, or an undecodable body are all errors — none may default to
// success.
// minimaxProviderStatusCode extracts base_resp.status_code from a VLM body,
// reporting false when the body is undecodable or the field is absent, null,
// or not an integer. Shared by the 2xx success parse and the non-2xx
// credential classification, so both read the same field with the same
// strictness: nothing here may default an absent status to zero.
func minimaxProviderStatusCode(raw []byte) (int, bool) {
	var body struct {
		BaseResp json.RawMessage `json:"base_resp"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0, false
	}

	baseResp := strings.TrimSpace(string(body.BaseResp))
	if baseResp == "" || baseResp == "null" || !strings.HasPrefix(baseResp, "{") {
		return 0, false
	}

	var status struct {
		StatusCode json.RawMessage `json:"status_code"`
	}
	if err := json.Unmarshal(body.BaseResp, &status); err != nil {
		return 0, false
	}

	codeRaw := strings.TrimSpace(string(status.StatusCode))
	if codeRaw == "" || codeRaw == "null" {
		return 0, false
	}

	code, err := strconv.Atoi(codeRaw)
	if err != nil {
		return 0, false
	}

	return code, true
}

func parseVLMResponse(raw []byte) (string, error) {
	var body struct {
		Content  json.RawMessage `json:"content"`
		BaseResp json.RawMessage `json:"base_resp"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", ErrRecognizerMalformed
	}

	code, ok := minimaxProviderStatusCode(raw)
	if !ok {
		return "", ErrRecognizerMalformed
	}

	if code != 0 {
		if code == minimaxAuthStatus {
			return "", fmt.Errorf("%w: provider status %d", ErrRecognizerAuth, code)
		}

		return "", fmt.Errorf("%w: provider status %d", ErrRecognizerStatus, code)
	}

	if len(body.Content) == 0 || strings.TrimSpace(string(body.Content)) == "null" {
		return "", ErrRecognizerEmpty
	}

	var content string
	if err := json.Unmarshal(body.Content, &content); err != nil {
		return "", ErrRecognizerMalformed
	}

	if strings.TrimSpace(content) == "" {
		return "", ErrRecognizerEmpty
	}

	return content, nil
}
