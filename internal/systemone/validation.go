package systemone

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// Validation tolerances. The wire contract serializes probabilities at
// limited precision (llama.cpp guarantees a distribution sum within 1e-4),
// so structural checks use tolerances that absorb backend rounding without
// accepting structurally wrong answers.
const (
	// distributionSumTolerance bounds how far a probability distribution may
	// drift from summing to exactly 1.
	distributionSumTolerance = 1e-3
	// scoreConsistencyTolerance bounds the absolute gap between a reported
	// score and Σ(i·p_i) computed from its own distribution: up to 10 level
	// terms, each rounded at serialization precision.
	scoreConsistencyTolerance = 0.01
	// tiedMaxEpsilon accepts any tied maximum as the reported choice: the
	// backend picks the argmax before serialization, so rounding can tip a
	// tie by the last emitted digit.
	tiedMaxEpsilon = 1e-6
)

// Answer is one validated answer keyed by question name inside the map
// Evaluate returns. Required fields are guaranteed present and in range;
// Confidence is optional on the wire and zero when absent (the policy
// consumes distributions, never confidence). Only the fields matching Type
// are meaningful.
type Answer struct {
	Type          string
	Noul          float64
	Choice        string
	Score         float64
	Probabilities map[string]float64
	Confidence    float64
	Legend        map[string]string
}

// wireAnswer decodes with pointer fields so missing, null, and wrongly-typed
// values are distinguishable from legitimate zeros: a missing noul must
// never become a valid probability of zero, and a null entry inside a
// probability distribution must never become a zero probability that passes
// range and sum checks.
type wireAnswer struct {
	Type          *string             `json:"type"`
	Noul          *float64            `json:"noul"`
	Choice        *string             `json:"choice"`
	Score         *float64            `json:"score"`
	Probabilities map[string]*float64 `json:"probabilities"`
	Confidence    *float64            `json:"confidence"`
	Legend        map[string]string   `json:"legend"`
}

// decodeAnswers validates a /v1/systemone success body against the request
// that produced it. The answer set must match the request exactly — every
// requested question answered, no unexpected ids — and every answer must
// match its requested type and criteria. Validation failures are malformed
// responses: no partial trust, no best-effort salvage. Question names never
// appear in the error text: for the decision tools they are caller-supplied
// arguments that may echo conversation content, and the client logs these
// errors verbatim.
func decodeAnswers(raw []byte, questions map[string]Question) (map[string]Answer, error) {
	var body struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("%w: cannot decode response", ErrMalformed)
	}

	if len(body.Answers) != len(questions) {
		return nil, fmt.Errorf(
			"%w: expected %d answers, got %d",
			ErrMalformed, len(questions), len(body.Answers),
		)
	}
	for name := range questions {
		if _, ok := body.Answers[name]; !ok {
			return nil, fmt.Errorf("%w: an expected answer is missing", ErrMalformed)
		}
	}

	answers := make(map[string]Answer, len(questions))
	for name, rawAnswer := range body.Answers {
		var wire wireAnswer
		if err := json.Unmarshal(rawAnswer, &wire); err != nil {
			return nil, fmt.Errorf("%w: cannot decode an answer", ErrMalformed)
		}

		answer, err := wire.validate(questions[name])
		if err != nil {
			return nil, err
		}
		answers[name] = answer
	}

	return answers, nil
}

func (w *wireAnswer) validate(question Question) (Answer, error) {
	malformed := func(what string) error {
		return fmt.Errorf("%w: an answer %s", ErrMalformed, what)
	}

	if w.Type == nil || *w.Type == "" {
		return Answer{}, malformed("is missing its type")
	}
	if *w.Type != question.Type {
		// Neither the reported type (backend-controlled) nor the question
		// name (caller-controlled tool argument) may reach the error text:
		// bodies can echo input, and the client logs this error verbatim.
		return Answer{}, fmt.Errorf(
			"%w: an answer has a type that does not match the requested %q",
			ErrMalformed, question.Type,
		)
	}

	answer := Answer{Type: *w.Type}

	if w.Confidence != nil && !finite01(*w.Confidence) {
		return Answer{}, malformed("has an out-of-range confidence")
	}
	if w.Confidence != nil {
		answer.Confidence = *w.Confidence
	}

	switch answer.Type {
	case TypeNoul:
		if w.Noul == nil {
			return Answer{}, malformed("is missing its noul probability")
		}
		if !finite01(*w.Noul) {
			return Answer{}, malformed("has an out-of-range noul probability")
		}
		answer.Noul = *w.Noul

		return answer, nil

	case TypeChoice:
		if w.Choice == nil || *w.Choice == "" {
			return Answer{}, malformed("is missing its choice")
		}
		probs, err := w.validatedDistribution(question.ChoiceOptions(), "choice options")
		if err != nil {
			return Answer{}, err
		}
		if !withinTolerance(probs[*w.Choice], maxProbability(probs), tiedMaxEpsilon) {
			return Answer{}, malformed("reports a choice that is not a maximum of its distribution")
		}
		answer.Choice = *w.Choice
		answer.Probabilities = probs

		return answer, nil

	case TypeScore:
		if w.Score == nil {
			return Answer{}, malformed("is missing its score")
		}
		levels := question.ScoreLevels()
		probs, err := w.validatedDistribution(levelKeys(len(levels)), "score levels")
		if err != nil {
			return Answer{}, err
		}
		expected := weightedLevelMean(probs)
		if math.Abs(*w.Score-expected) > scoreConsistencyTolerance {
			return Answer{}, malformed("reports a score inconsistent with its distribution")
		}
		// NaN fails both comparisons below, so finiteness is checked first:
		// the raw score must lie inside [0, len-1] (the normalization the
		// consumers perform keeps it inside [0,1] by construction).
		if math.IsNaN(*w.Score) || math.IsInf(*w.Score, 0) || *w.Score < 0 || *w.Score > float64(len(levels)-1) {
			return Answer{}, malformed("has an out-of-range score")
		}
		answer.Score = *w.Score
		answer.Probabilities = probs
		if w.Legend != nil && len(w.Legend) != len(levels) {
			return Answer{}, malformed("has a legend that does not match the level count")
		}
		answer.Legend = w.Legend

		return answer, nil

	default:
		// Unreachable: the type equality check above already rejected
		// anything but the three supported types.
		return Answer{}, malformed("has an unsupported type")
	}
}

// validatedDistribution checks a probability distribution against its
// expected key set: exactly the requested options (choice) or exactly the
// level numbers (score), no null entries, all finite and in [0,1], summing
// to 1 within tolerance. Error text names only counts and shapes — never
// option names, question names, or other request content.
func (w *wireAnswer) validatedDistribution(expectedKeys []string, what string) (map[string]float64, error) {
	malformed := func(what string) error {
		return fmt.Errorf("%w: an answer %s", ErrMalformed, what)
	}

	if len(w.Probabilities) != len(expectedKeys) {
		return nil, malformed(fmt.Sprintf("distribution has %d entries, expected %d %s", len(w.Probabilities), len(expectedKeys), what))
	}

	probs := make(map[string]float64, len(w.Probabilities))
	for _, key := range expectedKeys {
		value, ok := w.Probabilities[key]
		if !ok || value == nil {
			return nil, malformed(fmt.Sprintf("distribution is missing an entry for %s", what))
		}
		if !finite01(*value) {
			return nil, malformed(fmt.Sprintf("distribution has an out-of-range entry for %s", what))
		}
		probs[key] = *value
	}

	sum := 0.0
	for _, p := range probs {
		sum += p
	}
	if math.Abs(sum-1) > distributionSumTolerance {
		return nil, malformed("distribution does not sum to one")
	}

	return probs, nil
}

// levelKeys returns the score distribution keys: level numbers as strings,
// "0" through n-1.
func levelKeys(n int) []string {
	keys := make([]string, 0, n)
	for i := range n {
		keys = append(keys, strconv.Itoa(i))
	}

	return keys
}

// weightedLevelMean computes Σ(i·p_i): the probability-weighted mean level
// the score field is expected to report.
func weightedLevelMean(probs map[string]float64) float64 {
	mean := 0.0
	for key, p := range probs {
		level, err := strconv.Atoi(key)
		if err != nil {
			continue
		}
		mean += float64(level) * p
	}

	return mean
}

func maxProbability(probs map[string]float64) float64 {
	highest := 0.0
	for _, p := range probs {
		if p > highest {
			highest = p
		}
	}

	return highest
}

func withinTolerance(value, target, tolerance float64) bool {
	return value >= target-tolerance
}
