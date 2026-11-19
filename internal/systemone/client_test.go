package systemone

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient builds a client against an httptest backend. Zero-value
// Config fields get the production defaults.
func newTestClient(t *testing.T, handler http.Handler, mutate func(*Config)) *Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	cfg := Config{
		BaseURL:          server.URL,
		Timeout:          5 * time.Second,
		MaxResponseBytes: 1 << 20,
		InputBudgetBytes: 96 << 10,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	client, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	return client
}

// referenceQuestions is the mixed batch used across the tests, matching the
// llama.cpp README example: fractional score, noul, and choice.
func referenceQuestions() map[string]Question {
	return map[string]Question{
		"relevance": Score("How relevant is this?", []string{"irrelevant", "slightly", "relevant", "highly"}),
		"angry":     Noul("Is the customer angry?"),
		"route":     Choice("Which team handles this?", map[string]any{"billing": nil, "shipping": nil, "technical": nil}),
	}
}

// referenceQuestionNames lists the ids of referenceQuestions for assertions
// that must hold regardless of the concrete fixture.
func referenceQuestionNames() []string {
	return []string{"relevance", "angry", "route"}
}

// referenceResponse is a valid success body for referenceQuestions, with
// distributions that sum to 1 and a score consistent with Σ(i·p_i).
const referenceResponse = `{
  "model": "laya",
  "answers": {
    "relevance": {"type":"score","score":2.0858,"legend":{"0":"irrelevant","1":"slightly","2":"relevant","3":"highly"},"probabilities":{"0":0.0023,"1":0.116,"2":0.6753,"3":0.2064},"confidence":0.673},
    "angry": {"type":"noul","noul":0.6328},
    "route": {"type":"choice","choice":"billing","probabilities":{"billing":0.9998,"shipping":0.0001,"technical":0.0001},"confidence":0.9997}
  },
  "usage": {"input_tokens": 239, "output_tokens": 0}
}`

func TestEvaluateMixedBatch(t *testing.T) {
	var gotAuth string

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")

		if r.URL.Path != "/v1/systemone" {
			t.Errorf("unexpected request path %q", r.URL.Path)
		}

		var decoded map[string]any
		if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if decoded["model"] != "laya" {
			t.Errorf("request must carry the model field, got %v", decoded["model"])
		}
		if _, ok := decoded["questions"].(map[string]any); !ok {
			t.Errorf("questions must be an object map, got %T", decoded["questions"])
		}

		w.Header().Set("Content-Type", contentTypeJSON)
		_, _ = w.Write([]byte(referenceResponse))
	}), nil)

	answers, err := client.Evaluate(context.Background(), Request{
		Model:     "laya",
		State:     "Customer message: I was charged twice.",
		Questions: referenceQuestions(),
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if gotAuth != "" {
		t.Fatalf("client without api key must not send Authorization, got %q", gotAuth)
	}

	if answers["angry"].Type != TypeNoul || answers["angry"].Noul != 0.6328 {
		t.Fatalf("unexpected noul answer: %+v", answers["angry"])
	}
	if answers["route"].Choice != "billing" || answers["route"].Probabilities["billing"] != 0.9998 {
		t.Fatalf("unexpected choice answer: %+v", answers["route"])
	}
	// Fractional scores stay fractional: no rounding or quantization.
	if answers["relevance"].Score != 2.0858 {
		t.Fatalf("unexpected score answer: %+v", answers["relevance"])
	}
	if len(answers["relevance"].Legend) != 4 {
		t.Fatalf("expected legend to pass through, got %+v", answers["relevance"].Legend)
	}
}

func TestEvaluateSendsBearerAuth(t *testing.T) {
	var gotAuth string

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(referenceResponse))
	}), func(cfg *Config) {
		cfg.APIKey = "decision-token"
	})

	_, err := client.Evaluate(context.Background(), Request{Model: "laya", State: "s", Questions: referenceQuestions()})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if gotAuth != "Bearer decision-token" {
		t.Fatalf("unexpected authorization header: %q", gotAuth)
	}
}

// presenceFixtures are otherwise-successful 200 responses whose required
// fields are missing or null. Every one of them must be rejected: a missing
// or null numeric answer never becomes a legitimate zero.
func presenceFixtures() map[string]string {
	base := func(relevance, angry, route string) string {
		return `{"model":"laya","answers":{"relevance":` + relevance + `,"angry":` + angry + `,"route":` + route + `}}`
	}
	fullScore := `{"type":"score","score":2.0858,"probabilities":{"0":0.0023,"1":0.116,"2":0.6753,"3":0.2064}}`
	nullScore := `{"type":"score","score":null,"probabilities":{"0":0.0023,"1":0.116,"2":0.6753,"3":0.2064}}`
	noScore := `{"type":"score","probabilities":{"0":0.0023,"1":0.116,"2":0.6753,"3":0.2064}}`
	fullNoul := `{"type":"noul","noul":0.6328}`
	nullNoul := `{"type":"noul","noul":null}`
	noNoul := `{"type":"noul"}`
	fullChoice := `{"type":"choice","choice":"billing","probabilities":{"billing":0.9998,"shipping":0.0001,"technical":0.0001}}`
	noChoice := `{"type":"choice","probabilities":{"billing":0.9998,"shipping":0.0001,"technical":0.0001}}`

	return map[string]string{
		"missing noul":           base(fullScore, noNoul, fullChoice),
		"null noul":              base(fullScore, nullNoul, fullChoice),
		"missing score":          base(noScore, fullNoul, fullChoice),
		"null score":             base(nullScore, fullNoul, fullChoice),
		"missing choice":         base(fullScore, fullNoul, noChoice),
		"null probability entry": base(`{"type":"score","score":2.0858,"probabilities":{"0":0.0023,"1":0.116,"2":0.6753,"3":null}}`, fullNoul, fullChoice),
		"null probability map":   base(`{"type":"score","score":2.0858,"probabilities":null}`, fullNoul, fullChoice),
	}
}

func TestEvaluateRejectsMissingAndNullRequiredFields(t *testing.T) {
	for name, response := range presenceFixtures() {
		t.Run(name, func(t *testing.T) {
			client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(response))
			}), nil)

			_, err := client.Evaluate(context.Background(), Request{Model: "laya", State: "s", Questions: referenceQuestions()})
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("expected ErrMalformed, got %v", err)
			}
		})
	}
}

func malformedFixtures() map[string]string {
	choice := `{"type":"choice","choice":"billing","probabilities":{"billing":0.9998,"shipping":0.0001,"technical":0.0001}}`
	noul := `{"type":"noul","noul":0.6328}`

	return map[string]string{
		"unexpected answer id":                 `{"model":"laya","answers":{"angry":` + noul + `,"route":` + choice + `,"extra":` + noul + `}}`,
		"missing answer":                       `{"model":"laya","answers":{"angry":` + noul + `}}`,
		"wrong answer type":                    `{"model":"laya","answers":{"angry":{"type":"choice","choice":"yes","probabilities":{"yes":1.0}},"relevance":{"type":"noul","noul":0.5},"route":` + choice + `}}`,
		"out of range noul":                    `{"model":"laya","answers":{"angry":{"type":"noul","noul":1.5},"relevance":{"type":"score","score":2.0858,"probabilities":{"0":0.0023,"1":0.116,"2":0.6753,"3":0.2064}},"route":` + choice + `}}`,
		"negative noul":                        `{"model":"laya","answers":{"angry":{"type":"noul","noul":-0.1},"relevance":{"type":"score","score":2.0858,"probabilities":{"0":0.0023,"1":0.116,"2":0.6753,"3":0.2064}},"route":` + choice + `}}`,
		"short distribution":                   `{"model":"laya","answers":{"angry":` + noul + `,"relevance":{"type":"score","score":1.0,"probabilities":{"0":0.5,"1":0.5}},"route":` + choice + `}}`,
		"extra distribution key":               `{"model":"laya","answers":{"angry":` + noul + `,"relevance":{"type":"score","score":2.0858,"probabilities":{"0":0.0023,"1":0.116,"2":0.6753,"3":0.2064,"4":0.0}},"route":` + choice + `}}`,
		"distribution sums off":                `{"model":"laya","answers":{"angry":` + noul + `,"relevance":{"type":"score","score":1.0,"probabilities":{"0":0.4,"1":0.5,"2":0.05,"3":0.03}},"route":` + choice + `}}`,
		"choice is not the maximum":            `{"model":"laya","answers":{"angry":` + noul + `,"relevance":{"type":"score","score":2.0858,"probabilities":{"0":0.0023,"1":0.116,"2":0.6753,"3":0.2064}},"route":{"type":"choice","choice":"shipping","probabilities":{"billing":0.9998,"shipping":0.0001,"technical":0.0001}}}}`,
		"choice outside distribution":          `{"model":"laya","answers":{"angry":` + noul + `,"relevance":{"type":"score","score":2.0858,"probabilities":{"0":0.0023,"1":0.116,"2":0.6753,"3":0.2064}},"route":{"type":"choice","choice":"unknown","probabilities":{"billing":0.9998,"shipping":0.0001,"technical":0.0001}}}}`,
		"score inconsistent with distribution": `{"model":"laya","answers":{"angry":` + noul + `,"relevance":{"type":"score","score":2.5,"probabilities":{"0":0.0023,"1":0.116,"2":0.6753,"3":0.2064}},"route":` + choice + `}}`,
		"score out of range":                   `{"model":"laya","answers":{"angry":` + noul + `,"relevance":{"type":"score","score":4.0,"probabilities":{"0":0.0023,"1":0.116,"2":0.6753,"3":0.2064}},"route":` + choice + `}}`,
		"legend level mismatch":                `{"model":"laya","answers":{"angry":` + noul + `,"relevance":{"type":"score","score":2.0858,"legend":{"0":"a","1":"b"},"probabilities":{"0":0.0023,"1":0.116,"2":0.6753,"3":0.2064}},"route":` + choice + `}}`,
		"out of range float encoding":          `{"model":"laya","answers":{"angry":{"type":"noul","noul":1e400},"relevance":{"type":"score","score":2.0858,"probabilities":{"0":0.0023,"1":0.116,"2":0.6753,"3":0.2064}},"route":` + choice + `}}`,
		"not json":                             `{"model":`,
	}
}

func TestEvaluateRejectsMalformedResponses(t *testing.T) {
	for name, response := range malformedFixtures() {
		t.Run(name, func(t *testing.T) {
			client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(response))
			}), nil)

			_, err := client.Evaluate(context.Background(), Request{Model: "laya", State: "s", Questions: referenceQuestions()})
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("expected ErrMalformed, got %v", err)
			}
		})
	}
}

// TestEvaluateTypeMismatchErrorIsContentFree pins the privacy rule on the
// type-mismatch path: the backend-controlled reported type never reaches the
// error text, even when it carries echoed input.
func TestEvaluateTypeMismatchErrorIsContentFree(t *testing.T) {
	const leaked = "SECRET-ECHOED-INPUT"
	response := `{"model":"laya","answers":{"angry":{"type":"` + leaked + `"},"relevance":{"type":"noul","noul":0.5},"route":{"type":"choice","choice":"billing","probabilities":{"billing":1.0,"shipping":0.0,"technical":0.0}}}}`

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(response))
	}), nil)

	_, err := client.Evaluate(context.Background(), Request{Model: "laya", State: "s", Questions: referenceQuestions()})
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("expected ErrMalformed, got %v", err)
	}
	if strings.Contains(err.Error(), leaked) {
		t.Fatalf("error must not carry the backend-reported type, got %q", err.Error())
	}
}

// TestEvaluateValidationErrorsNeverNameQuestions pins the privacy rule on
// the question-name path: decision-tool question ids are caller-supplied
// arguments that may echo conversation content, and the client logs
// validation errors verbatim — so no validation failure may carry the id.
func TestEvaluateValidationErrorsNeverNameQuestions(t *testing.T) {
	const private = "PRIVATE_CASE_123"

	cases := []struct {
		name     string
		response string
	}{
		{
			// Same answer count, different id set: isolates the
			// missing-answer branch from the count check before it.
			name:     "missing answer",
			response: `{"model":"laya","answers":{"` + private + `":{"type":"noul","noul":0.5},"relevance":{"type":"noul","noul":0.5},"route":{"type":"choice","choice":"billing","probabilities":{"billing":1.0,"shipping":0.0,"technical":0.0}}}}`,
		},
		{
			name:     "undecodable answer",
			response: `{"model":"laya","answers":{"` + private + `":42,"relevance":{"type":"noul","noul":0.5},"route":{"type":"choice","choice":"billing","probabilities":{"billing":1.0,"shipping":0.0,"technical":0.0}}}}`,
		},
		{
			name:     "invalid answer",
			response: `{"model":"laya","answers":{"` + private + `":{"type":"noul","noul":9.9},"relevance":{"type":"noul","noul":0.5},"route":{"type":"choice","choice":"billing","probabilities":{"billing":1.0,"shipping":0.0,"technical":0.0}}}}`,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.response))
			}), nil)

			_, err := client.Evaluate(context.Background(), Request{Model: "laya", State: "s", Questions: referenceQuestions()})
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("expected ErrMalformed, got %v", err)
			}
			// None of the caller-chosen ids may appear: the private one the
			// fixtures exercise, nor any reference question's own name.
			for _, named := range append([]string{private}, referenceQuestionNames()...) {
				if strings.Contains(err.Error(), named) {
					t.Fatalf("validation error must not name the question %q, got %q", named, err.Error())
				}
			}
		})
	}
}

func TestEvaluateAcceptsTiedMaximaAndRoundingTolerances(t *testing.T) {
	// Two of the three options tie at 0.5; either reported choice is valid.
	// The score drifts from Σ(i·p_i) by 0.004, inside the accumulated-rounding
	// tolerance. Confidence and legend are absent: optional fields stay
	// optional.
	response := `{"model":"laya","answers":{
		"route":{"type":"choice","choice":"billing","probabilities":{"billing":0.5,"shipping":0.5,"technical":0.0}},
		"relevance":{"type":"score","score":1.504,"probabilities":{"0":0.0,"1":0.5,"2":0.5,"3":0.0}},
		"angry":{"type":"noul","noul":0.0}
	}}`

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(response))
	}), nil)

	answers, err := client.Evaluate(context.Background(), Request{Model: "laya", State: "s", Questions: referenceQuestions()})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if answers["route"].Choice != "billing" {
		t.Fatalf("expected tied maximum to be accepted, got %+v", answers["route"])
	}
	if answers["angry"].Noul != 0.0 {
		t.Fatalf("a legitimate zero answer must pass, got %+v", answers["angry"])
	}
}

func TestEvaluateRequestValidation(t *testing.T) {
	cases := []struct {
		name    string
		request Request
	}{
		{"blank model", Request{Model: "  ", State: "s", Questions: referenceQuestions()}},
		{"nil state", Request{Model: "laya", State: nil, Questions: referenceQuestions()}},
		{"no questions", Request{Model: "laya", State: "s", Questions: map[string]Question{}}},
		{"unknown type", Request{Model: "laya", State: "s", Questions: map[string]Question{"q": {Type: "box", Instructions: "x"}}}},
		{"blank instructions", Request{Model: "laya", State: "s", Questions: map[string]Question{"q": Noul("   ")}}},
		{"empty choice criteria", Request{Model: "laya", State: "s", Questions: map[string]Question{"q": Choice("pick", map[string]any{})}}},
		{"too many choice options", Request{Model: "laya", State: "s", Questions: map[string]Question{
			"q": Choice("pick", oversizedOptions(MaxChoiceOptions+1)),
		}}},
		{"one score level", Request{Model: "laya", State: "s", Questions: map[string]Question{"q": Score("rate", []string{"only"})}}},
		{"eleven score levels", Request{Model: "laya", State: "s", Questions: map[string]Question{"q": Score("rate", oversizedLevels(11))}}},
		{"blank score level", Request{Model: "laya", State: "s", Questions: map[string]Question{"q": Score("rate", []string{"low", "  "})}}},
		{"bad noul criteria keys", Request{Model: "laya", State: "s", Questions: map[string]Question{
			"q": {Type: TypeNoul, Instructions: "yes?", Criteria: map[string]any{"maybe": nil}},
		}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requested := false
			client := newTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				requested = true
			}), nil)

			_, err := client.Evaluate(context.Background(), tc.request)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("expected ErrInvalidRequest, got %v", err)
			}
			if requested {
				t.Fatal("a request that fails boundary validation must never reach the network")
			}
		})
	}
}

func TestEvaluateEnforcesInputBudget(t *testing.T) {
	requested := false
	client := newTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requested = true
	}), func(cfg *Config) {
		// The reference batch serializes to well under 1 KiB; 64 bytes
		// refuses it before the network is touched.
		cfg.InputBudgetBytes = 64
	})

	_, err := client.Evaluate(context.Background(), Request{Model: "laya", State: strings.Repeat("x", 4096), Questions: referenceQuestions()})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
	if requested {
		t.Fatal("an over-budget request must never reach the network")
	}
}

func TestEvaluateClassifiesErrorStatuses(t *testing.T) {
	cases := []struct {
		status   int
		expected error
	}{
		{http.StatusUnauthorized, ErrAuth},
		{http.StatusForbidden, ErrAuth},
		{http.StatusTooManyRequests, ErrUnavailable},
		{http.StatusInternalServerError, ErrUnavailable},
		{http.StatusNotImplemented, ErrUnavailable},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			sensitive := "SENSITIVE-ECHOED-CONTENT"
			client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"message":"invalid state: ` + sensitive + `"}}`))
			}), nil)

			_, err := client.Evaluate(context.Background(), Request{Model: "laya", State: "s", Questions: referenceQuestions()})
			if !errors.Is(err, tc.expected) {
				t.Fatalf("expected %v, got %v", tc.expected, err)
			}
			if strings.Contains(err.Error(), sensitive) {
				t.Fatalf("error must not echo the response body, got %q", err.Error())
			}
		})
	}
}

func TestEvaluateRefusesRedirects(t *testing.T) {
	// Server B records anything it receives; the contract is that a 307 from
	// server A must never be followed, so B stays untouched and neither the
	// decision input nor the bearer token leaves for another endpoint.
	var hitsAtB atomic.Int32
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitsAtB.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer serverB.Close()

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, serverB.URL+"/v1/systemone", http.StatusTemporaryRedirect)
	}), func(cfg *Config) {
		cfg.APIKey = "decision-token"
	})

	_, err := client.Evaluate(context.Background(), Request{Model: "laya", State: "s", Questions: referenceQuestions()})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable for a refused redirect, got %v", err)
	}
	if hitsAtB.Load() != 0 {
		t.Fatalf("redirect target must receive no request, got %d", hitsAtB.Load())
	}
}

func TestEvaluateBoundedByClientTimeout(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}), func(cfg *Config) {
		cfg.Timeout = 20 * time.Millisecond
	})

	_, err := client.Evaluate(context.Background(), Request{Model: "laya", State: "s", Questions: referenceQuestions()})
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected sanitized timeout error, got %v", err)
	}
}

func TestEvaluateRespectsContextCancellation(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a cancelled request must not be sent")
	}), nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Evaluate(ctx, Request{Model: "laya", State: "s", Questions: referenceQuestions()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled to survive sanitization, got %v", err)
	}
}

func TestEvaluateCapsResponseBody(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 2<<20))
	}), func(cfg *Config) {
		cfg.MaxResponseBytes = 1 << 20
	})

	_, err := client.Evaluate(context.Background(), Request{Model: "laya", State: "s", Questions: referenceQuestions()})
	if !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected oversized-body rejection, got %v", err)
	}
}

func TestListModels(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"laya"},{"id":"julia-1"},{"id":"gemma4:e4b"}]}`))
	}), nil)

	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(models) != 3 || models[0] != "laya" {
		t.Fatalf("unexpected model list: %v", models)
	}
}

func TestListModelsRejectsMalformedLists(t *testing.T) {
	cases := map[string]string{
		"not json":    `{"data":`,
		"no data":     `{"object":"list"}`,
		"empty list":  `{"object":"list","data":[]}`,
		"blank model": `{"object":"list","data":[{"id":""},{"id":"laya"}]}`,
	}

	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(response))
			}), nil)

			if _, err := client.ListModels(context.Background()); !errors.Is(err, ErrMalformed) {
				t.Fatalf("expected ErrMalformed, got %v", err)
			}
		})
	}
}

func TestUnconfiguredClient(t *testing.T) {
	client, err := New(Config{}, nil)
	if err != nil {
		t.Fatalf("unconfigured client must construct without error, got %v", err)
	}
	if client.Configured() {
		t.Fatal("expected unconfigured client")
	}

	if _, err := client.Evaluate(context.Background(), Request{Model: "m", State: "s", Questions: referenceQuestions()}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
	if _, err := client.ListModels(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
}

func TestNewRejectsInvalidBaseURL(t *testing.T) {
	_, err := New(Config{BaseURL: "x://user:supersecret@host/path", Timeout: time.Second, MaxResponseBytes: 1, InputBudgetBytes: 1}, nil)
	if err == nil {
		t.Fatal("expected non-http scheme to be rejected")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("base url errors must stay redacted, got %q", err.Error())
	}
}

func TestTransportErrorsNeverCarryCredentials(t *testing.T) {
	client, err := New(Config{
		BaseURL:          "http://127.0.0.1:1", // nothing listens here
		APIKey:           "decision-token",
		Timeout:          time.Second,
		MaxResponseBytes: 1 << 20,
		InputBudgetBytes: 96 << 10,
	}, nil)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	_, evalErr := client.Evaluate(context.Background(), Request{Model: "laya", State: "s", Questions: referenceQuestions()})
	if !errors.Is(evalErr, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", evalErr)
	}
	if strings.Contains(evalErr.Error(), "decision-token") || strings.Contains(evalErr.Error(), "127.0.0.1:1") {
		t.Fatalf("transport error must stay sanitized, got %q", evalErr.Error())
	}
}

func oversizedOptions(n int) map[string]any {
	options := make(map[string]any, n)
	for i := range n {
		options["opt"+strconv.Itoa(i)] = nil
	}

	return options
}

func oversizedLevels(n int) []string {
	levels := make([]string, 0, n)
	for range n {
		levels = append(levels, "level")
	}

	return levels
}
