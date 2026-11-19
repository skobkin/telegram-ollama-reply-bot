package systemone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"telegram-ollama-reply-bot/internal/logging"
	"telegram-ollama-reply-bot/internal/support/httpclient"
	"telegram-ollama-reply-bot/internal/support/safeerr"
	"telegram-ollama-reply-bot/internal/support/urlredact"
)

// Client errors. Bodies of failed responses can echo request content (the
// hosted API's 422 validation errors do), so no error ever quotes a body:
// failures are described by typed sentinel, HTTP status, and redacted URL
// only.
var (
	ErrNotConfigured = errors.New("system one backend is not configured")
	ErrUnavailable   = errors.New("system one backend is unavailable")
	ErrAuth          = errors.New("system one backend rejected credentials")
	ErrMalformed     = errors.New("system one backend returned a malformed response")
)

// Wire endpoints appended to the configured base URL.
const (
	evaluatePath    = "/v1/systemone"
	modelsPath      = "/v1/models"
	contentTypeJSON = "application/json"
)

// Config carries the deployment connection settings. It is declared here
// instead of importing internal/config so the package stays free of
// application configuration types; internal/app copies the values over.
type Config struct {
	// BaseURL is the decision backend root. Empty means unconfigured: the
	// client constructs fine but refuses every call with ErrNotConfigured.
	BaseURL string
	// APIKey is the optional bearer token. Empty sends no Authorization
	// header (llama.cpp without --api-key requires none).
	APIKey string
	// Timeout bounds one HTTP request. It applies independently of the
	// caller's context deadline; whichever fires first wins.
	Timeout time.Duration
	// MaxResponseBytes caps the response body read.
	MaxResponseBytes int64
	// InputBudgetBytes caps the serialized request body. It is the final
	// composition bound; callers compose within it, the client refuses
	// anything larger.
	InputBudgetBytes int64
}

// Client evaluates typed decision questions against one configured System
// One backend. Evaluate performs exactly one HTTP attempt per call: bounded
// retries are the caller's policy, and proactive participation prefers
// failing fast to failing late.
type Client struct {
	endpoint  *url.URL
	modelsURL *url.URL
	apiKey    string
	budget    int64
	maxBody   int64
	client    *http.Client
	logger    *slog.Logger
}

// New builds a client. An empty (or whitespace) BaseURL yields an
// unconfigured client: no error, Configured() false, calls refused with
// ErrNotConfigured. A non-empty BaseURL must parse as http(s); errors quote
// the URL only in redacted form because endpoint URLs can carry credentials.
func New(cfg Config, logger *slog.Logger) (*Client, error) {
	trimmed := strings.TrimSpace(cfg.BaseURL)
	if trimmed == "" {
		return &Client{logger: logger}, nil
	}

	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("%w: timeout must be positive", ErrNotConfigured)
	}
	if cfg.MaxResponseBytes <= 0 || cfg.InputBudgetBytes <= 0 {
		return nil, fmt.Errorf("%w: response and input byte limits must be positive", ErrNotConfigured)
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("%w: base url is not a valid url: %s", ErrUnavailable, urlredact.RedactOne(trimmed))
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("%w: base url scheme must be http or https: %s", ErrUnavailable, urlredact.RedactOne(trimmed))
	}

	client := httpclient.New()
	client.Timeout = cfg.Timeout
	// Refuse redirects outright: a 307/308 would replay the request body to
	// the redirect target, leaking decision input and possibly the bearer
	// token. ErrUseLastResponse hands the 3xx back untouched; Evaluate
	// classifies it as a refused redirect.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &Client{
		endpoint:  parsed.JoinPath(evaluatePath),
		modelsURL: parsed.JoinPath(modelsPath),
		apiKey:    cfg.APIKey,
		budget:    cfg.InputBudgetBytes,
		maxBody:   cfg.MaxResponseBytes,
		client:    client,
		logger:    logger,
	}, nil
}

// Configured reports whether the client has a decision backend. The engine
// and the decision tools gate on this before doing any work.
func (c *Client) Configured() bool {
	return c != nil && c.endpoint != nil
}

// Evaluate sends one decision batch and returns validated answers keyed by
// question name. Exactly one attempt is made; the caller's context and the
// configured per-request timeout bound it together.
func (c *Client) Evaluate(ctx context.Context, req Request) (map[string]Answer, error) {
	logger := logging.FromContext(ctx, c.logger)

	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	started := time.Now()

	if err := req.validate(); err != nil {
		return nil, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot encode request", ErrInvalidRequest)
	}
	if int64(len(body)) > c.budget {
		return nil, fmt.Errorf("%w: serialized request of %d bytes exceeds the %d byte input budget", ErrInvalidRequest, len(body), c.budget)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot build request", ErrUnavailable)
	}
	httpReq.Header.Set("Content-Type", contentTypeJSON)
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, safeerr.WrapTransport(ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := readBody(resp.Body, c.maxBody)
	if readErr != nil {
		return nil, readErr
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := statusError(resp.StatusCode)
		logger.Warn("system one request failed",
			"http_status", resp.StatusCode,
			"model", req.Model,
			"questions", len(req.Questions),
			"duration_ms", time.Since(started).Milliseconds(),
		)

		return nil, err
	}

	answers, err := decodeAnswers(raw, req.Questions)
	if err != nil {
		logger.Warn("system one response rejected",
			"model", req.Model,
			"questions", len(req.Questions),
			"duration_ms", time.Since(started).Milliseconds(),
			"error", err,
		)

		return nil, err
	}

	logger.Debug("system one request answered",
		"model", req.Model,
		"questions", len(req.Questions),
		"duration_ms", time.Since(started).Milliseconds(),
	)

	return answers, nil
}

// ListModels fetches the backend's model list for the admin picker.
// Discovery establishes availability only, never capability: a listed model
// is not guaranteed to answer decision questions.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.modelsURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot build request", ErrUnavailable)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, safeerr.WrapTransport(ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := readBody(resp.Body, c.maxBody)
	if readErr != nil {
		return nil, readErr
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, statusError(resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("%w: cannot decode model list", ErrMalformed)
	}

	models := make([]string, 0, len(body.Data))
	for _, entry := range body.Data {
		if strings.TrimSpace(entry.ID) == "" {
			return nil, fmt.Errorf("%w: model list contains an entry without an id", ErrMalformed)
		}
		models = append(models, entry.ID)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: model list is empty", ErrMalformed)
	}

	return models, nil
}

// statusError maps a non-2xx HTTP status to a typed error. 3xx values reach
// this point only because redirects are refused at the client; they are
// reported as refused redirects, not followed.
func statusError(status int) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: http status %d", ErrAuth, status)
	case status >= 300 && status < 400:
		return fmt.Errorf("%w: redirect refused with http status %d", ErrUnavailable, status)
	default:
		return fmt.Errorf("%w: http status %d", ErrUnavailable, status)
	}
}

// readBody reads up to limit+1 bytes and rejects oversized bodies before any
// content is buffered whole. A truncated read (the limit hit) is a malformed
// response, never a partial success.
func readBody(body io.ReadCloser, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, safeerr.WrapTransport(ErrUnavailable, err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%w: response exceeds %d bytes", ErrMalformed, limit)
	}

	return raw, nil
}
