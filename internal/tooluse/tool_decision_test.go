package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/systemone"
)

// stubDecisionClient records outgoing System One requests and scripts
// answers or failures.
type stubDecisionClient struct {
	requests     []systemone.Request
	answers      map[string]systemone.Answer
	err          error
	failOnCtxErr bool
}

func (s *stubDecisionClient) Evaluate(ctx context.Context, req systemone.Request) (map[string]systemone.Answer, error) {
	s.requests = append(s.requests, req)
	if s.failOnCtxErr && ctx.Err() != nil {
		return nil, ctx.Err()
	}

	return s.answers, s.err
}

// decisionTestRuntime wires the standard registry with the decision tools
// attached to the stub client.
func decisionTestRuntime(t *testing.T, client *stubDecisionClient) *Runtime {
	t.Helper()

	runtime := New(
		&stubLLM{},
		memoryConversations(t),
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6, BotIdentity: testBotIdentity},
	)
	runtime.AttachDecisionTools(client)

	return runtime
}

// decisionCallContext is the direct-handler CallContext with the run's
// decision model pinned, mirroring what ReplyWithTools builds.
func decisionCallContext(model string) CallContext {
	return CallContext{run: &runState{systemOneModel: model}}
}

// TestDecisionToolsAvailabilityIntersection pins the per-turn availability:
// statically registered with the backend, advertised and executable only
// with an effective System One model, independent of the allowlist
// machinery.
func TestDecisionToolsAvailabilityIntersection(t *testing.T) {
	client := &stubDecisionClient{}
	runtime := decisionTestRuntime(t, client)

	names := namesOf(runtime.registry.DefaultDefinitions())
	if !containsName(names, "decision_choose") || !containsName(names, "decision_assess") {
		t.Fatalf("decision tools must be statically registered, got %v", names)
	}

	if withModel := namesOf(runtime.turnDefinitions("decision-model")); !containsName(withModel, "decision_choose") {
		t.Fatalf("effective model must advertise the decision tools, got %v", withModel)
	}
	withoutModel := namesOf(runtime.turnDefinitions(""))
	if containsName(withoutModel, "decision_choose") || containsName(withoutModel, "decision_assess") {
		t.Fatalf("unset decision model must hide the decision tools, got %v", withoutModel)
	}
	if !containsName(withoutModel, "get_current_time") {
		t.Fatalf("other tools must remain advertised, got %v", withoutModel)
	}

	if names := namesOf(runtime.turnDefinitions("decision-model")); containsName(names, "decision_choose") && !containsName(names, "decision_assess") {
		t.Fatalf("both decision tools must advertise together, got %v", names)
	}
}

// TestDecisionToolsDetachedWithoutClient pins that a nil client registers
// nothing.
func TestDecisionToolsDetachedWithoutClient(t *testing.T) {
	runtime := New(
		&stubLLM{},
		memoryConversations(t),
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6, BotIdentity: testBotIdentity},
	)
	runtime.AttachDecisionTools(nil)

	for _, name := range namesOf(runtime.registry.DefaultDefinitions()) {
		if isDecisionTool(name) {
			t.Fatalf("nil client must register nothing, found %s in %v", name, namesOf(runtime.registry.DefaultDefinitions()))
		}
	}
}

// TestDecisionChooseHappyPath pins the request composition and the result
// shape: the snapshot's model, the raw state string, one choice question
// with the option map as criteria — ids and numbers only in the result.
func TestDecisionChooseHappyPath(t *testing.T) {
	client := &stubDecisionClient{answers: map[string]systemone.Answer{
		decisionChooseQuestionID: {
			Type:          systemone.TypeChoice,
			Choice:        "migration",
			Probabilities: map[string]float64{"bug": 0.123456, "migration": 0.654321, "feature": 0.222223},
		},
	}}
	runtime := decisionTestRuntime(t, client)

	result, err := runtime.decisionChoose(context.Background(), decisionCallContext("decision-model"), json.RawMessage(`{
		"state": "The release note describes a database migration.",
		"question": "Which category fits best?",
		"options": {"bug": "Bug fix", "migration": "Data migration", "feature": "New feature"}
	}`))
	if err != nil {
		t.Fatalf("decision_choose: %v", err)
	}

	if len(client.requests) != 1 {
		t.Fatalf("exactly one backend request expected, got %d", len(client.requests))
	}
	request := client.requests[0]
	if request.Model != "decision-model" {
		t.Fatalf("request must use the snapshot model, got %q", request.Model)
	}
	if request.State != "The release note describes a database migration." {
		t.Fatalf("state must pass through verbatim, got %v", request.State)
	}
	question := request.Questions[decisionChooseQuestionID]
	if question.Type != systemone.TypeChoice || len(question.Criteria.(map[string]any)) != 3 {
		t.Fatalf("choice question shape mismatch: %+v", question)
	}
	if !strings.Contains(question.Instructions.(string), "Which category fits best?") {
		t.Fatalf("question text must reach the instructions, got %v", question.Instructions)
	}

	if result.Status != "ok" {
		t.Fatalf("expected ok result, got %+v", result)
	}
	data, err := json.Marshal(result.Data)
	if err != nil {
		t.Fatalf("result data: %v", err)
	}
	var parsed decisionChooseResult
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("result data shape: %v", err)
	}
	if parsed.Selected != "migration" {
		t.Fatalf("selected = %q, want migration", parsed.Selected)
	}
	if parsed.Probabilities["bug"] != 0.1235 || parsed.Probabilities["migration"] != 0.6543 {
		t.Fatalf("probabilities must be rounded to 4 decimals, got %v", parsed.Probabilities)
	}
}

// TestDecisionAssessHappyPath pins the batch composition: per-type
// questions, ordered typed reports, rounded values.
func TestDecisionAssessHappyPath(t *testing.T) {
	client := &stubDecisionClient{answers: map[string]systemone.Answer{
		"relevant": {Type: systemone.TypeNoul, Noul: 0.987654},
		"quality":  {Type: systemone.TypeScore, Score: 2.345678, Probabilities: map[string]float64{"1": 0.1, "2": 0.2, "3": 0.7}},
	}}
	runtime := decisionTestRuntime(t, client)

	result, err := runtime.decisionAssess(context.Background(), decisionCallContext("decision-model"), json.RawMessage(`{
		"state": "A short excerpt provided by the caller.",
		"questions": [
			{"id": "relevant", "type": "noul", "instructions": "Is the excerpt relevant?"},
			{"id": "quality", "type": "score", "instructions": "Rate the writing quality.", "levels": ["poor", "fair", "good"]}
		]
	}`))
	if err != nil {
		t.Fatalf("decision_assess: %v", err)
	}

	request := client.requests[0]
	if request.Model != "decision-model" {
		t.Fatalf("request must use the snapshot model, got %q", request.Model)
	}
	noulQuestion := request.Questions["relevant"]
	if noulQuestion.Type != systemone.TypeNoul || noulQuestion.Criteria != nil {
		t.Fatalf("noul question must carry no criteria, got %+v", noulQuestion)
	}
	scoreQuestion := request.Questions["quality"]
	if levels := scoreQuestion.ScoreLevels(); len(levels) != 3 || levels[0] != "poor" {
		t.Fatalf("score levels must pass through ordered, got %v", levels)
	}

	data, err := json.Marshal(result.Data)
	if err != nil {
		t.Fatalf("result data: %v", err)
	}
	var reports []decisionAnswerReport
	if err := json.Unmarshal(data, &reports); err != nil {
		t.Fatalf("result data shape: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("expected two reports, got %d", len(reports))
	}
	if reports[0].ID != "relevant" || reports[0].Type != systemone.TypeNoul || reports[0].Value == nil || *reports[0].Value != 0.9877 {
		t.Fatalf("noul report mismatch: %+v", reports[0])
	}
	if reports[1].ID != "quality" || reports[1].Score == nil || *reports[1].Score != 2.3457 || reports[1].Probabilities["3"] != 0.7 {
		t.Fatalf("score report mismatch: %+v", reports[1])
	}
}

// TestDecisionAssessPreservesZeroAnswers pins that legitimate zero answers
// serialize: omitempty on plain floats would withhold a 0 answer from the
// generation model entirely.
func TestDecisionAssessPreservesZeroAnswers(t *testing.T) {
	client := &stubDecisionClient{answers: map[string]systemone.Answer{
		"relevant": {Type: systemone.TypeNoul, Noul: 0},
		"quality":  {Type: systemone.TypeScore, Score: 0, Probabilities: map[string]float64{"1": 1}},
	}}
	runtime := decisionTestRuntime(t, client)

	result, err := runtime.decisionAssess(context.Background(), decisionCallContext("decision-model"), json.RawMessage(`{
		"state": "s",
		"questions": [
			{"id": "relevant", "type": "noul", "instructions": "yes?"},
			{"id": "quality", "type": "score", "instructions": "rate.", "levels": ["low", "high"]}
		]
	}`))
	if err != nil {
		t.Fatalf("decision_assess: %v", err)
	}

	data, err := json.Marshal(result.Data)
	if err != nil {
		t.Fatalf("result data: %v", err)
	}
	if !strings.Contains(string(data), `"value":0`) {
		t.Fatalf("zero noul value must serialize, got %s", data)
	}
	if !strings.Contains(string(data), `"score":0`) {
		t.Fatalf("zero score must serialize, got %s", data)
	}
}

// TestDecisionArgsNotOverridable pins that model or endpoint hints in the
// arguments are ignored: the snapshot model is the only one used.
func TestDecisionArgsNotOverridable(t *testing.T) {
	client := &stubDecisionClient{answers: map[string]systemone.Answer{
		decisionChooseQuestionID: {Type: systemone.TypeChoice, Choice: "a", Probabilities: map[string]float64{"a": 1.0, "b": 0.0}},
	}}
	runtime := decisionTestRuntime(t, client)

	_, err := runtime.decisionChoose(context.Background(), decisionCallContext("decision-model"), json.RawMessage(`{
		"state": "s", "model": "evil-model", "endpoint": "http://evil.example",
		"question": "q?", "options": {"a": "A", "b": "B"}
	}`))
	if err != nil {
		t.Fatalf("unknown argument keys must be ignored: %v", err)
	}
	if got := client.requests[0].Model; got != "decision-model" {
		t.Fatalf("model must come from the snapshot only, got %q", got)
	}
}

// TestDecisionToolValidationMatrix walks the input limits: every invalid
// case fails with a sanitized error and never reaches the backend.
func TestDecisionToolValidationMatrix(t *testing.T) {
	client := &stubDecisionClient{}
	runtime := decisionTestRuntime(t, client)

	cases := []struct {
		name    string
		handler func(ctx context.Context) error
	}{
		{"choose state empty", func(ctx context.Context) error {
			_, err := runtime.decisionChoose(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"","question":"q","options":{"a":"A","b":"B"}}`))

			return err
		}},
		{"choose state too long", func(ctx context.Context) error {
			_, err := runtime.decisionChoose(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"`+strings.Repeat("长", 8001)+`","question":"q","options":{"a":"A","b":"B"}}`))

			return err
		}},
		{"choose question missing", func(ctx context.Context) error {
			_, err := runtime.decisionChoose(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","options":{"a":"A","b":"B"}}`))

			return err
		}},
		{"choose question too long", func(ctx context.Context) error {
			_, err := runtime.decisionChoose(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","question":"`+strings.Repeat("x", 1001)+`","options":{"a":"A","b":"B"}}`))

			return err
		}},
		{"choose instructions too long", func(ctx context.Context) error {
			_, err := runtime.decisionChoose(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","question":"q","instructions":"`+strings.Repeat("x", 1001)+`","options":{"a":"A","b":"B"}}`))

			return err
		}},
		{"choose one option", func(ctx context.Context) error {
			_, err := runtime.decisionChoose(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","question":"q","options":{"a":"A"}}`))

			return err
		}},
		{"choose thirteen options", func(ctx context.Context) error {
			options := map[string]string{}
			for i := 0; i < 13; i++ {
				options[runeID(i)] = "label"
			}
			encoded, _ := json.Marshal(map[string]any{"state": "s", "question": "q", "options": options})
			_, err := runtime.decisionChoose(ctx, decisionCallContext("m"), encoded)

			return err
		}},
		{"choose invalid option id", func(ctx context.Context) error {
			_, err := runtime.decisionChoose(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","question":"q","options":{"bad id!":"A","b":"B"}}`))

			return err
		}},
		{"choose option id too long", func(ctx context.Context) error {
			_, err := runtime.decisionChoose(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","question":"q","options":{"`+strings.Repeat("a", 33)+`":"A","b":"B"}}`))

			return err
		}},
		{"choose label too long", func(ctx context.Context) error {
			_, err := runtime.decisionChoose(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","question":"q","options":{"a":"`+strings.Repeat("L", 201)+`","b":"B"}}`))

			return err
		}},
		{"choose malformed json", func(ctx context.Context) error {
			_, err := runtime.decisionChoose(ctx, decisionCallContext("m"), json.RawMessage(`{"state":`))

			return err
		}},
		{"assess state too long", func(ctx context.Context) error {
			_, err := runtime.decisionAssess(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"`+strings.Repeat("x", 8001)+`","questions":[{"id":"a","type":"noul","instructions":"i"}]}`))

			return err
		}},
		{"assess no questions", func(ctx context.Context) error {
			_, err := runtime.decisionAssess(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","questions":[]}`))

			return err
		}},
		{"assess six questions", func(ctx context.Context) error {
			questions := []string{}
			for i := 0; i < 6; i++ {
				questions = append(questions, `{"id":"`+runeID(i)+`","type":"noul","instructions":"i"}`)
			}
			_, err := runtime.decisionAssess(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","questions":[`+strings.Join(questions, ",")+`]}`))

			return err
		}},
		{"assess invalid id", func(ctx context.Context) error {
			_, err := runtime.decisionAssess(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","questions":[{"id":"bad.id","type":"noul","instructions":"i"}]}`))

			return err
		}},
		{"assess duplicate id", func(ctx context.Context) error {
			_, err := runtime.decisionAssess(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","questions":[{"id":"a","type":"noul","instructions":"i"},{"id":"a","type":"score","instructions":"i","levels":["x","y"]}]}`))

			return err
		}},
		{"assess unknown type", func(ctx context.Context) error {
			_, err := runtime.decisionAssess(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","questions":[{"id":"a","type":"choice","instructions":"i"}]}`))

			return err
		}},
		{"assess score without levels", func(ctx context.Context) error {
			_, err := runtime.decisionAssess(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","questions":[{"id":"a","type":"score","instructions":"i"}]}`))

			return err
		}},
		{"assess single level", func(ctx context.Context) error {
			_, err := runtime.decisionAssess(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","questions":[{"id":"a","type":"score","instructions":"i","levels":["only"]}]}`))

			return err
		}},
		{"assess eleven levels", func(ctx context.Context) error {
			levels := []string{}
			for i := 0; i < 11; i++ {
				levels = append(levels, "L"+runeID(i))
			}
			encoded, _ := json.Marshal(map[string]any{"state": "s", "questions": []map[string]any{
				{"id": "a", "type": "score", "instructions": "i", "levels": levels},
			}})
			_, err := runtime.decisionAssess(ctx, decisionCallContext("m"), encoded)

			return err
		}},
		{"assess level description too long", func(ctx context.Context) error {
			_, err := runtime.decisionAssess(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","questions":[{"id":"a","type":"score","instructions":"i","levels":["`+strings.Repeat("L", 201)+`","ok"]}]}`))

			return err
		}},
		{"assess noul with levels", func(ctx context.Context) error {
			_, err := runtime.decisionAssess(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","questions":[{"id":"a","type":"noul","instructions":"i","levels":["x","y"]}]}`))

			return err
		}},
		{"assess instructions missing", func(ctx context.Context) error {
			_, err := runtime.decisionAssess(ctx, decisionCallContext("m"), json.RawMessage(`{"state":"s","questions":[{"id":"a","type":"noul"}]}`))

			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.handler(context.Background())
			if err == nil {
				t.Fatalf("expected a validation error")
			}
			if !errors.Is(err, errDecisionInvalidArguments) && !errors.Is(err, errDecisionRequestTooLarge) {
				t.Fatalf("error must be the sanitized validation class, got %v", err)
			}
		})
	}

	if len(client.requests) != 0 {
		t.Fatalf("invalid arguments must never reach the backend, got %d requests", len(client.requests))
	}
}

// TestDecisionRequestSizeBudget pins the 24 KiB whole-request cap with an
// escaping-heavy worst case that stays under it only through the cap: every
// text field at its maximum, filled with characters that JSON-escape.
func TestDecisionRequestSizeBudget(t *testing.T) {
	client := &stubDecisionClient{}
	runtime := decisionTestRuntime(t, client)

	// Every text field at its maximum, filled with quote-characters that
	// serialize to double length: state 8000 + question 1000 + instructions
	// 1000 + twelve 200-character option labels push the serialized request
	// past the 24 KiB cap.
	options := make(map[string]string, maxDecisionOptions)
	for i := 0; i < maxDecisionOptions; i++ {
		options[strings.Repeat(string(rune('a'+i)), 2)] = strings.Repeat(`"`, maxDecisionOptionLabelChars)
	}
	args, _ := json.Marshal(map[string]any{
		"state":        strings.Repeat(`"`, maxDecisionStateChars),
		"question":     strings.Repeat(`"`, maxDecisionQuestionChars),
		"instructions": strings.Repeat(`"`, maxDecisionInstructionsChars),
		"options":      options,
	})

	_, err := runtime.decisionChoose(context.Background(), decisionCallContext("decision-model"), args)
	if !errors.Is(err, errDecisionRequestTooLarge) {
		t.Fatalf("escaping-heavy maximum input must hit the request budget, got %v", err)
	}
	if len(client.requests) != 0 {
		t.Fatalf("over-budget requests must never reach the backend")
	}

	// A same-shape request within the budget goes through.
	client.answers = map[string]systemone.Answer{
		decisionChooseQuestionID: {Type: systemone.TypeChoice, Choice: "aa", Probabilities: map[string]float64{"aa": 1}},
	}
	args, _ = json.Marshal(map[string]any{
		"state":    strings.Repeat(`"`, maxDecisionStateChars),
		"question": "q",
		"options":  map[string]string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "first", "b": "second"},
	})
	if _, err := runtime.decisionChoose(context.Background(), decisionCallContext("decision-model"), args); err != nil {
		t.Fatalf("within-budget escaping-heavy input must pass: %v", err)
	}
}

// TestDecisionResultBudgetsWorstCase runs the worst-case valid results
// through the handlers: maximum identifiers, maximum question and option
// counts, maximum level counts — complete valid JSON must fit the budgets.
func TestDecisionResultBudgetsWorstCase(t *testing.T) {
	t.Run("choose", func(t *testing.T) {
		options := map[string]string{}
		probabilities := map[string]float64{}
		for i := 0; i < maxDecisionOptions; i++ {
			id := maxID(i)
			options[id] = "label"
			probabilities[id] = 1.0 / float64(maxDecisionOptions)
		}
		client := &stubDecisionClient{answers: map[string]systemone.Answer{
			decisionChooseQuestionID: {Type: systemone.TypeChoice, Choice: maxID(0), Probabilities: probabilities},
		}}
		runtime := decisionTestRuntime(t, client)

		args, _ := json.Marshal(map[string]any{"state": "s", "question": "q", "options": options})
		result, err := runtime.decisionChoose(context.Background(), decisionCallContext("decision-model"), args)
		if err != nil {
			t.Fatalf("worst-case choose result must fit the budget: %v", err)
		}
		encoded, _ := json.Marshal(result)
		if strings.Count(string(encoded), maxID(0)) == 0 {
			t.Fatalf("sanity: expected the selected id in the result")
		}
	})

	t.Run("assess", func(t *testing.T) {
		answers := map[string]systemone.Answer{}
		questions := []map[string]any{}
		for i := 0; i < maxDecisionQuestions; i++ {
			id := maxID(i)
			levels := []string{}
			probabilities := map[string]float64{}
			for level := 0; level < systemone.MaxScoreLevels; level++ {
				levels = append(levels, strings.Repeat("L", maxDecisionLevelDescChars))
				probabilities[runeID(level)] = 1.0 / float64(systemone.MaxScoreLevels)
			}
			questions = append(questions, map[string]any{
				"id": id, "type": "score", "instructions": strings.Repeat("i", maxDecisionInstructionsChars), "levels": levels,
			})
			answers[id] = systemone.Answer{Type: systemone.TypeScore, Score: 4.5, Probabilities: probabilities}
		}
		client := &stubDecisionClient{answers: answers}
		runtime := decisionTestRuntime(t, client)

		args, _ := json.Marshal(map[string]any{"state": "s", "questions": questions})
		result, err := runtime.decisionAssess(context.Background(), decisionCallContext("decision-model"), args)
		if err != nil {
			t.Fatalf("worst-case assess result must fit the budget: %v", err)
		}
		encoded, _ := json.Marshal(result)
		if len(encoded) == 0 {
			t.Fatalf("sanity: expected a serialized result")
		}
	})
}

// TestDecisionResultNoTruncation pins the no-truncation rule at the helper
// level: an oversized result becomes the sanitized structured error, never
// a truncated JSON string.
func TestDecisionResultNoTruncation(t *testing.T) {
	huge := map[string]string{"blob": strings.Repeat("x", decisionChooseResultCharBudget*2)}
	result, err := decisionResult(decisionChooseResultCharBudget, "decision made", huge)
	if err == nil {
		t.Fatalf("oversized result must error")
	}
	if !errors.Is(err, errDecisionResultTooLarge) || result.Status != "" {
		t.Fatalf("oversized result must produce the sanitized error class, got %+v / %v", result, err)
	}
}

// TestDecisionUnconfiguredAndBackendErrors pins the sanitized failure
// classes: no effective model rejects before inference, backend failures
// stay content-free, and protocol anomalies stay distinct.
func TestDecisionUnconfiguredAndBackendErrors(t *testing.T) {
	client := &stubDecisionClient{}
	runtime := decisionTestRuntime(t, client)
	args := json.RawMessage(`{"state":"s","question":"q","options":{"a":"A","b":"B"}}`)

	// No model in the run state (and a nil run): rejected before inference.
	for _, callCtx := range []CallContext{{}, decisionCallContext("")} {
		if _, err := runtime.decisionChoose(context.Background(), callCtx, args); !errors.Is(err, errDecisionUnavailable) {
			t.Fatalf("unconfigured scope must reject with the availability error, got %v", err)
		}
	}
	if len(client.requests) != 0 {
		t.Fatalf("unconfigured scope must not reach the backend")
	}

	backend := decisionTestRuntime(t, &stubDecisionClient{err: errors.New("dial tcp 10.0.0.1:8080: connect: connection refused")})
	_, err := backend.decisionChoose(context.Background(), decisionCallContext("decision-model"), args)
	if !errors.Is(err, errDecisionBackendFailed) {
		t.Fatalf("transport failure must map to the backend class, got %v", err)
	}
	if strings.Contains(err.Error(), "10.0.0.1") {
		t.Fatalf("transport details must not surface, got %q", err.Error())
	}

	malformed := decisionTestRuntime(t, &stubDecisionClient{err: systemone.ErrMalformed})
	_, err = malformed.decisionChoose(context.Background(), decisionCallContext("decision-model"), args)
	if !errors.Is(err, errDecisionInvalidResponse) {
		t.Fatalf("protocol anomaly must map to the response class, got %v", err)
	}

	wrongType := decisionTestRuntime(t, &stubDecisionClient{answers: map[string]systemone.Answer{
		decisionChooseQuestionID: {Type: systemone.TypeNoul, Noul: 0.5},
	}})
	_, err = wrongType.decisionChoose(context.Background(), decisionCallContext("decision-model"), args)
	if !errors.Is(err, errDecisionInvalidResponse) {
		t.Fatalf("mistyped answer must map to the response class, got %v", err)
	}
}

// TestDecisionDeadlineContainment pins that nested requests inherit the
// tool-call context: an exhausted turn context surfaces as the sanitized
// backend error, and never extends the deadline.
func TestDecisionDeadlineContainment(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := &stubDecisionClient{failOnCtxErr: true, answers: map[string]systemone.Answer{
		decisionChooseQuestionID: {Type: systemone.TypeChoice, Choice: "a", Probabilities: map[string]float64{"a": 1.0, "b": 0.0}},
	}}
	runtime := decisionTestRuntime(t, client)

	_, err := runtime.decisionChoose(ctx, decisionCallContext("decision-model"), json.RawMessage(`{"state":"s","question":"q","options":{"a":"A","b":"B"}}`))
	if !errors.Is(err, errDecisionBackendFailed) {
		t.Fatalf("canceled turn context must surface as the sanitized backend error, got %v", err)
	}
	if !errors.Is(context.Cause(ctx), context.Canceled) {
		t.Fatalf("sanity: context must be canceled")
	}
}

// TestDecisionToolsNotAdvertisedWithoutModel pins the advertisement side of
// the intersection through the real ReplyWithTools build path.
func TestDecisionToolsNotAdvertisedWithoutModel(t *testing.T) {
	stub := &stubLLM{}
	runtime := New(
		stub,
		memoryConversations(t),
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6, BotIdentity: testBotIdentity},
	)
	runtime.AttachDecisionTools(&stubDecisionClient{})
	stub.responses = []llm.Response{{Message: llm.TextMessage(llm.RoleAssistant, "done")}}

	request := ChatRequest{
		Scope:        state.ConversationScope{ChatID: 1},
		PromptScope:  llm.PromptScope{ChatID: 1},
		ReplyContext: llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "hi")},
	}
	if _, err := runtime.ReplyWithTools(context.Background(), request); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, tool := range stub.lastBuildTools {
		if isDecisionTool(tool.Name) {
			t.Fatalf("decision tool must not be advertised without an effective model, got %s", tool.Name)
		}
	}
	if strings.Contains(stub.lastBuildPolicy, "decision_") {
		t.Fatalf("tool policy must not list the decision tools, got %q", stub.lastBuildPolicy)
	}

	// With the model set they are advertised again — settings changes
	// between turns never rebuild the registry.
	request.TurnConfig = llm.TurnConfig{SystemOneModel: "decision-model"}
	stub.responses = []llm.Response{{Message: llm.TextMessage(llm.RoleAssistant, "done")}}
	if _, err := runtime.ReplyWithTools(context.Background(), request); err != nil {
		t.Fatalf("run with model: %v", err)
	}
	advertised := make([]string, 0, len(stub.lastBuildTools))
	for _, tool := range stub.lastBuildTools {
		advertised = append(advertised, tool.Name)
	}
	if !containsName(advertised, decisionChooseToolName) {
		t.Fatalf("decision tools must be advertised with an effective model, got %v", advertised)
	}
}

// TestDecisionToolExecutionRejectsUnconfiguredTurn pins the execution side:
// a hallucinated decision call in an unconfigured turn takes the rejection
// path with no inference attempt.
func TestDecisionToolExecutionRejectsUnconfiguredTurn(t *testing.T) {
	client := &stubDecisionClient{}
	stub := &stubLLM{}
	runtime := New(
		stub,
		memoryConversations(t),
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6, BotIdentity: testBotIdentity},
	)
	runtime.AttachDecisionTools(client)
	stub.responses = []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "decision_choose", Arguments: json.RawMessage(`{"state":"s","question":"q","options":{"a":"A","b":"B"}}`)},
		}}},
		{Message: llm.TextMessage(llm.RoleAssistant, "giving up")},
	}

	request := ChatRequest{
		Scope:        state.ConversationScope{ChatID: 1},
		PromptScope:  llm.PromptScope{ChatID: 1},
		ReplyContext: llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "hi")},
		// TurnConfig zero: no effective System One model.
	}
	result, err := runtime.ReplyWithTools(context.Background(), request)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(client.requests) != 0 {
		t.Fatalf("unconfigured decision call must not reach inference, got %d requests", len(client.requests))
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Status != execution.StatusError {
		t.Fatalf("the rejected call must record an error, got %+v", result.ToolCalls)
	}
}

// --- small helpers ---

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}

	return false
}

// maxID builds the lexicographically-largest 32-character identifier of its
// index, for worst-case serialization fixtures.
func maxID(index int) string {
	return strings.Repeat("z", 30) + runeID(index*2) + runeID(index*2+1)
}

// runeID maps an index to a short valid identifier.
func runeID(index int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

	return string(alphabet[index%len(alphabet)])
}
