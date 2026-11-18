// Package systemone is a typed client for the System One decision backend
// (the Jev protocol, POST /v1/systemone). The wire contract is the common
// core of the two supported dialects: llama.cpp ≥ b11361 / llama-swap ≥ v262
// (OpenAI-style error envelopes, optional bearer auth) and the hosted
// TypeSafe API (FastAPI-style errors, required bearer auth). It was verified
// against llama.cpp's tools/server/server-decision.cpp, its unit tests, and
// the TypeSafe OpenAPI schema.
//
// The client is deliberately narrow and free of application business logic:
// no Telegram types, no interactivity policy, no settings resolution. The
// InteractionDecisionEngine and the decision tools share one instance.
// Endpoint and credentials arrive through Config from deployment
// configuration; the client reads no environment variables.
//
// Privacy: request and response bodies are never logged; error values carry
// only typed sentinels, HTTP status classes, and redacted URLs. Transport
// errors are sanitized through safeerr so Sentry breadcrumbs cannot retain
// raw causes. Redirects are refused outright: Go would follow a 307/308 and
// replay the request body — decision input and credentials must never reach
// another endpoint.
package systemone

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Question types of the /v1/systemone contract.
const (
	TypeNoul   = "noul"
	TypeChoice = "choice"
	TypeScore  = "score"
)

// Protocol limits of the common core. The engine and the decision tools work
// well inside them; per-model caps below these values (e.g. openjev's 52
// choice options) are backend concerns that surface as request errors.
const (
	// MaxChoiceOptions is the protocol-wide choice criteria cap (255).
	MaxChoiceOptions = 255
	// MinScoreLevels and MaxScoreLevels bound the score level count
	// (llama.cpp: 2–10; the hosted API: 2–10).
	MinScoreLevels = 2
	MaxScoreLevels = 10
)

// Client-side validation errors. They never carry request content: a
// serialized request that fails validation is described, not quoted.
var (
	ErrInvalidRequest = errors.New("system one request is invalid")
)

// Question is one typed question of a decision request. Instructions and
// criteria are typed any because the contract accepts strings, objects, and
// arrays there; the builders below produce the canonical shapes this
// application uses.
type Question struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	// Criteria is required for choice (option → description, null allowed)
	// and score (ordered level descriptions, lowest first), and optional for
	// noul ({true,false} descriptions).
	Criteria any `json:"criteria,omitempty"`
}

// Noul builds a yes/no probability question.
func Noul(instructions string) Question {
	return Question{Type: TypeNoul, Instructions: instructions}
}

// Choice builds a single-select question over an option → description map.
// Descriptions may be nil.
func Choice(instructions string, criteria map[string]any) Question {
	return Question{Type: TypeChoice, Instructions: instructions, Criteria: criteria}
}

// Score builds an ordinal question over ordered level descriptions (lowest
// level first). The answer is a probability-weighted mean of level numbers,
// so fractional scores are normal.
func Score(instructions string, levels []string) Question {
	return Question{Type: TypeScore, Instructions: instructions, Criteria: levels}
}

// Request is one decision batch: a shared state plus independent typed
// questions. The model field is always sent — llama-swap routes /v1/systemone
// by the body model field exactly like /v1/chat/completions, and a
// single-model llama.cpp server ignores it.
type Request struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// ScoreLevels returns the ordered level descriptions of a score question.
// For non-score questions it returns nil.
func (q Question) ScoreLevels() []string {
	if q.Type != TypeScore {
		return nil
	}
	levels, _ := q.Criteria.([]string)

	return levels
}

// ChoiceOptions returns the option names of a choice question. For
// non-choice questions it returns nil.
func (q Question) ChoiceOptions() []string {
	if q.Type != TypeChoice {
		return nil
	}
	criteria, ok := q.Criteria.(map[string]any)
	if !ok {
		return nil
	}
	options := make([]string, 0, len(criteria))
	for name := range criteria {
		options = append(options, name)
	}

	return options
}

// validate checks an outgoing request at the client boundary. Malformed
// requests never leave the process: the strictness here protects the engine
// and every decision tool at once, so handlers cannot drift from the wire
// contract.
func (r Request) validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("%w: model is required", ErrInvalidRequest)
	}

	if r.State == nil {
		return fmt.Errorf("%w: state is required", ErrInvalidRequest)
	}

	if len(r.Questions) == 0 {
		return fmt.Errorf("%w: at least one question is required", ErrInvalidRequest)
	}

	for name, q := range r.Questions {
		if err := q.validate(name); err != nil {
			return err
		}
	}

	return nil
}

func (q Question) validate(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: question names must be non-empty", ErrInvalidRequest)
	}

	if !isBlankString(q.Instructions) {
		return fmt.Errorf("%w: question %q instructions must be a non-blank string", ErrInvalidRequest, name)
	}

	switch q.Type {
	case TypeNoul:
		if q.Criteria == nil {
			return nil
		}
		criteria, ok := q.Criteria.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: question %q noul criteria must be an object", ErrInvalidRequest, name)
		}
		for key := range criteria {
			if key != "true" && key != "false" {
				return fmt.Errorf("%w: question %q noul criteria keys must be true/false", ErrInvalidRequest, name)
			}
		}

		return nil
	case TypeChoice:
		criteria, ok := q.Criteria.(map[string]any)
		if !ok || len(criteria) == 0 {
			return fmt.Errorf("%w: question %q choice criteria must be a non-empty option map", ErrInvalidRequest, name)
		}
		if len(criteria) > MaxChoiceOptions {
			return fmt.Errorf("%w: question %q has %d choice options, max is %d", ErrInvalidRequest, name, len(criteria), MaxChoiceOptions)
		}

		return nil
	case TypeScore:
		levels := q.ScoreLevels()
		if len(levels) < MinScoreLevels || len(levels) > MaxScoreLevels {
			return fmt.Errorf(
				"%w: question %q score needs %d-%d level descriptions, got %d",
				ErrInvalidRequest, name, MinScoreLevels, MaxScoreLevels, len(levels),
			)
		}
		for i, level := range levels {
			if strings.TrimSpace(level) == "" {
				return fmt.Errorf("%w: question %q score level %d is blank", ErrInvalidRequest, name, i)
			}
		}

		return nil
	default:
		return fmt.Errorf("%w: question %q has unknown type %q", ErrInvalidRequest, name, q.Type)
	}
}

// isBlankString reports whether v is a non-blank string. The contract also
// allows object/array instructions, but this application always sends
// strings; the boundary accepts exactly what it produces.
func isBlankString(v any) bool {
	s, ok := v.(string)

	return ok && strings.TrimSpace(s) != ""
}

// finite01 reports whether f is finite and inside [0,1].
func finite01(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f <= 1
}
