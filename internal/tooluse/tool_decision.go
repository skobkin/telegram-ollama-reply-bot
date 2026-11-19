package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/systemone"
)

// Decision tools (#158): decision_choose and decision_assess delegate small
// semantic decisions to the configured System One model. The tools are
// statically registered at startup when the backend is configured; their
// per-turn availability is the intersection of that registration with the
// operation snapshot's effective System One model, the run allowlist, and
// the existing permission machinery (see Runtime.turnDefinitions and the
// rejection path in executeToolCall). Availability is independent of the
// interaction profile: Non-Interactive never disables explicit tool calls.

const (
	decisionChooseToolName = "decision_choose"
	decisionAssessToolName = "decision_assess"

	decisionChooseResultCharBudget = 1200
	decisionAssessResultCharBudget = 2000

	// Input limits. Text limits are character (rune) counts; identifiers are
	// ASCII-restricted (see decisionIDPattern) so they stay short and
	// escaping-free as JSON keys — labels, descriptions, and state remain
	// fully multilingual.
	maxDecisionStateChars        = 8000
	maxDecisionQuestionChars     = 1000
	maxDecisionInstructionsChars = 1000
	minDecisionOptions           = 2
	maxDecisionOptions           = 12
	maxDecisionOptionLabelChars  = 200
	maxDecisionQuestions         = 5
	maxDecisionLevelDescChars    = 200
	// maxDecisionRequestBytes caps the whole serialized System One request
	// (state, question text, criteria, protocol overhead) — comfortably
	// inside the engine's composition budget.
	maxDecisionRequestBytes = 24 * 1024
)

// decisionChooseQuestionID is the fixed question key of the single choice
// question decision_choose poses. It never reaches the model as content.
const decisionChooseQuestionID = "decision"

// decisionIDPattern constrains option and question identifiers.
var decisionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// Sanitized, content-free handler errors: arguments, backend response
// bodies, endpoints, and credentials never surface through any of them.
var (
	errDecisionUnavailable      = errors.New("decision backend is not configured for this scope")
	errDecisionInvalidArguments = errors.New("invalid arguments for the decision tool")
	errDecisionRequestTooLarge  = errors.New("decision request exceeds the size budget")
	errDecisionBackendFailed    = errors.New("decision backend request failed")
	errDecisionInvalidResponse  = errors.New("decision backend returned an invalid response")
	errDecisionResultTooLarge   = errors.New("decision result exceeds the result budget")
)

// decisionExecutor is the System One inference boundary for the decision
// tools; *systemone.Client is the production implementation. Recursion is
// structurally impossible: the decision backend cannot call tools.
type decisionExecutor interface {
	Evaluate(ctx context.Context, req systemone.Request) (map[string]systemone.Answer, error)
}

// isDecisionTool reports whether the named tool requires an effective
// System One model to be advertised and executable.
func isDecisionTool(name string) bool {
	return name == decisionChooseToolName || name == decisionAssessToolName
}

// AttachDecisionTools registers both decision tools against the shared
// System One client. It is a no-op when `client` is nil so the application
// layer can gate it on the backend configuration; while detached the tools
// are absent from the registry and never advertised. Model and endpoint are
// never overridable from tool arguments: the model comes from the run's
// turn configuration snapshot.
func (r *Runtime) AttachDecisionTools(client decisionExecutor) {
	if client == nil {
		return
	}
	r.decisionClient = client

	r.registry.Register(Definition{
		Name:             decisionChooseToolName,
		Description:      "Delegate one small semantic decision to the decision model: given the self-contained state you provide, it picks the best-fitting option and reports its validated probability distribution. The backend sees only the arguments you pass. Option identifiers must match [A-Za-z0-9_-]{1,32}; labels may be any text.",
		Parameters:       json.RawMessage(`{"type":"object","properties":{"state":{"type":"string","minLength":1,"maxLength":8000,"description":"Self-contained state and context for the decision."},"question":{"type":"string","minLength":1,"maxLength":1000,"description":"The decision question."},"instructions":{"type":"string","maxLength":1000,"description":"Optional additional decision instructions."},"options":{"type":"object","minProperties":2,"maxProperties":12,"additionalProperties":{"type":"string","maxLength":200},"description":"Option identifier to short label. Identifiers must match [A-Za-z0-9_-]{1,32}."}},"required":["state","question","options"],"additionalProperties":false}`),
		InvocationPolicy: InvocationPolicyDiscretionary,
		ResultCharBudget: decisionChooseResultCharBudget,
		Visibility:       VisibilityStandard,
		Handler:          r.decisionChoose,
	})
	r.registry.Register(Definition{
		Name:             decisionAssessToolName,
		Description:      "Delegate a small batch of semantic assessments to the decision model over the self-contained state you provide: noul yes/no probabilities or ordered score questions. Returns typed answers keyed by question id. The backend sees only the arguments you pass; question identifiers must match [A-Za-z0-9_-]{1,32}.",
		Parameters:       json.RawMessage(`{"type":"object","properties":{"state":{"type":"string","minLength":1,"maxLength":8000,"description":"Self-contained state and context for the assessments."},"questions":{"type":"array","minItems":1,"maxItems":5,"items":{"type":"object","properties":{"id":{"type":"string","pattern":"^[A-Za-z0-9_-]{1,32}$","description":"Question identifier, returned with the answer."},"type":{"type":"string","enum":["noul","score"]},"instructions":{"type":"string","minLength":1,"maxLength":1000,"description":"What this question asks."},"levels":{"type":"array","minItems":2,"maxItems":10,"items":{"type":"string","maxLength":200},"description":"Ordered level descriptions, lowest first; required for score questions."}},"required":["id","type","instructions"],"additionalProperties":false}}},"required":["state","questions"],"additionalProperties":false}`),
		InvocationPolicy: InvocationPolicyDiscretionary,
		ResultCharBudget: decisionAssessResultCharBudget,
		Visibility:       VisibilityStandard,
		Handler:          r.decisionAssess,
	})
}

// decisionChooseArgs are the validated inputs of decision_choose.
type decisionChooseArgs struct {
	State        string            `json:"state"`
	Question     string            `json:"question"`
	Instructions string            `json:"instructions,omitempty"`
	Options      map[string]string `json:"options"`
}

// decisionChooseResult reports the selected option id and the validated
// distribution. Labels never appear: ids and numbers only, so the result
// size is bounded by the identifier syntax.
type decisionChooseResult struct {
	Selected      string             `json:"selected"`
	Probabilities map[string]float64 `json:"probabilities"`
}

func (r *Runtime) decisionChoose(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var parsed decisionChooseArgs
	if err := json.Unmarshal(args, &parsed); err != nil {
		return toolResult{}, fmt.Errorf("%w: malformed arguments", errDecisionInvalidArguments)
	}

	if utf8.RuneCountInString(parsed.State) < 1 || utf8.RuneCountInString(parsed.State) > maxDecisionStateChars {
		return toolResult{}, fmt.Errorf("%w: state must be 1-%d characters", errDecisionInvalidArguments, maxDecisionStateChars)
	}
	question := strings.TrimSpace(parsed.Question)
	if utf8.RuneCountInString(question) < 1 || utf8.RuneCountInString(question) > maxDecisionQuestionChars {
		return toolResult{}, fmt.Errorf("%w: question must be 1-%d characters", errDecisionInvalidArguments, maxDecisionQuestionChars)
	}
	instructions := strings.TrimSpace(parsed.Instructions)
	if utf8.RuneCountInString(instructions) > maxDecisionInstructionsChars {
		return toolResult{}, fmt.Errorf("%w: instructions must be at most %d characters", errDecisionInvalidArguments, maxDecisionInstructionsChars)
	}
	if len(parsed.Options) < minDecisionOptions || len(parsed.Options) > maxDecisionOptions {
		return toolResult{}, fmt.Errorf("%w: options must number %d-%d", errDecisionInvalidArguments, minDecisionOptions, maxDecisionOptions)
	}

	criteria := make(map[string]any, len(parsed.Options))
	for id, label := range parsed.Options {
		if !decisionIDPattern.MatchString(id) {
			return toolResult{}, fmt.Errorf("%w: option identifiers must match [A-Za-z0-9_-]{1,32}", errDecisionInvalidArguments)
		}
		if utf8.RuneCountInString(label) < 1 || utf8.RuneCountInString(label) > maxDecisionOptionLabelChars {
			return toolResult{}, fmt.Errorf("%w: option labels must be 1-%d characters", errDecisionInvalidArguments, maxDecisionOptionLabelChars)
		}
		criteria[id] = label
	}

	if instructions != "" {
		question = question + "\n\n" + instructions
	}

	answers, err := r.evaluateDecision(ctx, callCtx, parsed.State, map[string]systemone.Question{
		decisionChooseQuestionID: systemone.Choice(question, criteria),
	})
	if err != nil {
		return toolResult{}, err
	}

	answer, ok := answers[decisionChooseQuestionID]
	if !ok || answer.Type != systemone.TypeChoice {
		return toolResult{}, errDecisionInvalidResponse
	}

	return decisionResult(decisionChooseResultCharBudget, "decision made", decisionChooseResult{
		Selected:      answer.Choice,
		Probabilities: roundedProbabilities(answer.Probabilities),
	})
}

// decisionAssessArgs are the validated inputs of decision_assess.
type decisionAssessArgs struct {
	State     string                       `json:"state"`
	Questions []decisionAssessQuestionArgs `json:"questions"`
}

type decisionAssessQuestionArgs struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	Instructions string   `json:"instructions"`
	Levels       []string `json:"levels,omitempty"`
}

// decisionAnswerReport is one typed answer in the decision_assess result,
// ordered by the input question order for determinism.
type decisionAnswerReport struct {
	ID            string             `json:"id"`
	Type          string             `json:"type"`
	Value         float64            `json:"value,omitempty"`
	Score         float64            `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

func (r *Runtime) decisionAssess(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var parsed decisionAssessArgs
	if err := json.Unmarshal(args, &parsed); err != nil {
		return toolResult{}, fmt.Errorf("%w: malformed arguments", errDecisionInvalidArguments)
	}

	if utf8.RuneCountInString(parsed.State) < 1 || utf8.RuneCountInString(parsed.State) > maxDecisionStateChars {
		return toolResult{}, fmt.Errorf("%w: state must be 1-%d characters", errDecisionInvalidArguments, maxDecisionStateChars)
	}
	if len(parsed.Questions) < 1 || len(parsed.Questions) > maxDecisionQuestions {
		return toolResult{}, fmt.Errorf("%w: questions must number 1-%d", errDecisionInvalidArguments, maxDecisionQuestions)
	}

	questions := make(map[string]systemone.Question, len(parsed.Questions))
	reports := make([]decisionAnswerReport, 0, len(parsed.Questions))
	for _, question := range parsed.Questions {
		if !decisionIDPattern.MatchString(question.ID) {
			return toolResult{}, fmt.Errorf("%w: question identifiers must match [A-Za-z0-9_-]{1,32}", errDecisionInvalidArguments)
		}
		if _, duplicate := questions[question.ID]; duplicate {
			return toolResult{}, fmt.Errorf("%w: duplicate question identifier", errDecisionInvalidArguments)
		}
		instructions := strings.TrimSpace(question.Instructions)
		if utf8.RuneCountInString(instructions) < 1 || utf8.RuneCountInString(instructions) > maxDecisionInstructionsChars {
			return toolResult{}, fmt.Errorf("%w: question instructions must be 1-%d characters", errDecisionInvalidArguments, maxDecisionInstructionsChars)
		}

		switch question.Type {
		case systemone.TypeNoul:
			if len(question.Levels) > 0 {
				return toolResult{}, fmt.Errorf("%w: levels are only valid for score questions", errDecisionInvalidArguments)
			}
			questions[question.ID] = systemone.Noul(instructions)
		case systemone.TypeScore:
			if len(question.Levels) < systemone.MinScoreLevels || len(question.Levels) > systemone.MaxScoreLevels {
				return toolResult{}, fmt.Errorf("%w: score questions need %d-%d level descriptions", errDecisionInvalidArguments, systemone.MinScoreLevels, systemone.MaxScoreLevels)
			}
			for _, level := range question.Levels {
				if utf8.RuneCountInString(level) < 1 || utf8.RuneCountInString(level) > maxDecisionLevelDescChars {
					return toolResult{}, fmt.Errorf("%w: level descriptions must be 1-%d characters", errDecisionInvalidArguments, maxDecisionLevelDescChars)
				}
			}
			levels := append([]string(nil), question.Levels...)
			questions[question.ID] = systemone.Score(instructions, levels)
		default:
			return toolResult{}, fmt.Errorf("%w: question type must be noul or score", errDecisionInvalidArguments)
		}

		reports = append(reports, decisionAnswerReport{ID: question.ID, Type: question.Type})
	}

	answers, err := r.evaluateDecision(ctx, callCtx, parsed.State, questions)
	if err != nil {
		return toolResult{}, err
	}

	// Fill the reports in input order; every requested id is answered (the
	// client guarantees it) and unexpected ids are ignored.
	for i, question := range parsed.Questions {
		answer, ok := answers[question.ID]
		if !ok || answer.Type != question.Type {
			return toolResult{}, errDecisionInvalidResponse
		}
		switch question.Type {
		case systemone.TypeNoul:
			reports[i].Value = round4(answer.Noul)
		case systemone.TypeScore:
			reports[i].Score = round4(answer.Score)
			reports[i].Probabilities = roundedProbabilities(answer.Probabilities)
		}
	}

	return decisionResult(decisionAssessResultCharBudget, "assessments complete", reports)
}

// evaluateDecision composes, caps, and evaluates one System One request.
// The decision model comes from the run's turn configuration snapshot; a
// call reaching here without one is rejected (unadvertised tools can only
// be hallucinated). Nested requests run under the tool-call context, which
// is bounded by the remaining turn deadline — a decision call never extends
// it.
func (r *Runtime) evaluateDecision(ctx context.Context, callCtx CallContext, decisionState any, questions map[string]systemone.Question) (map[string]systemone.Answer, error) {
	model := ""
	if callCtx.run != nil {
		model = callCtx.run.systemOneModel
	}
	if model == "" {
		return nil, errDecisionUnavailable
	}

	request := systemone.Request{Model: model, State: decisionState, Questions: questions}
	serialized, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("%w: request cannot be serialized", errDecisionRequestTooLarge)
	}
	if len(serialized) > maxDecisionRequestBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d byte budget", errDecisionRequestTooLarge, len(serialized), maxDecisionRequestBytes)
	}

	answers, err := r.decisionClient.Evaluate(ctx, request)
	if err != nil {
		// Defense in depth: the shared client already sanitizes its errors,
		// but the handler never embeds client error text into tool results —
		// only the class distinction (transport vs protocol) reaches the
		// model.
		if errors.Is(err, systemone.ErrMalformed) || errors.Is(err, systemone.ErrInvalidRequest) {
			return nil, errDecisionInvalidResponse
		}

		return nil, errDecisionBackendFailed
	}

	return answers, nil
}

// decisionResult applies the decision tools' no-truncation rule: a complete
// valid result within the budget is returned; anything larger becomes a
// sanitized structured error, never a byte-truncated JSON string.
func decisionResult(budget int, summary string, data any) (toolResult, error) {
	encoded, err := json.Marshal(toolResult{Status: "ok", Summary: summary, Data: data})
	if err != nil || utf8.RuneCount(encoded) > budget {
		return toolResult{}, errDecisionResultTooLarge
	}

	return toolResult{Status: "ok", Summary: summary, Data: data}, nil
}

// roundedProbabilities rounds distribution values to 4 decimals for compact
// reporting. Keys are identifiers only.
func roundedProbabilities(distribution map[string]float64) map[string]float64 {
	rounded := make(map[string]float64, len(distribution))
	for id, probability := range distribution {
		rounded[id] = round4(probability)
	}

	return rounded
}

func round4(value float64) float64 {
	return math.Round(value*10000) / 10000
}
