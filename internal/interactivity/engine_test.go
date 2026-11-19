package interactivity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/systemone"
)

// fakeDecisionClient records the request and serves canned answers.
type fakeDecisionClient struct {
	request systemone.Request
	answers map[string]systemone.Answer
	err     error
	calls   int
}

func (c *fakeDecisionClient) Evaluate(_ context.Context, req systemone.Request) (map[string]systemone.Answer, error) {
	c.calls++
	c.request = req

	return c.answers, c.err
}

// fakeModelResolver counts resolutions; memoization tests rely on the count.
type fakeModelResolver struct {
	generation string
	decision   string
	err        error

	generationCalls int
	decisionCalls   int
}

func (r *fakeModelResolver) GenerationModel(context.Context, int64) (string, error) {
	r.generationCalls++

	return r.generation, r.err
}

func (r *fakeModelResolver) DecisionModel(context.Context, int64) (string, error) {
	r.decisionCalls++

	return r.decision, r.err
}

// fakePersonaSource serves a fixed persona, counting renders and recording
// the settings the engine passed. CapturePersonaTemplate serves a separate
// fixed body so tests can prove the engine forwards it as part of the
// settings instead of resolving anything itself.
type fakePersonaSource struct {
	persona      string
	err          error
	calls        int
	settings     PersonaSettings
	templateBody string
	captureErr   error
	captureCalls int
}

func (p *fakePersonaSource) DecisionPersona(_ context.Context, _ int64, _ string, settings PersonaSettings) (string, error) {
	p.calls++
	p.settings = settings

	return p.persona, p.err
}

func (p *fakePersonaSource) CapturePersonaTemplate(_ context.Context, _ int64) (string, error) {
	p.captureCalls++

	return p.templateBody, p.captureErr
}

// fakeHistorySource serves a fixed snapshot, counting reads.
type fakeHistorySource struct {
	snapshot HistorySnapshot
	err      error
	calls    int
}

func (h *fakeHistorySource) DecisionHistory(context.Context, state.ConversationScope) (HistorySnapshot, error) {
	h.calls++

	return h.snapshot, h.err
}

const (
	testChatID  = int64(-100300400500)
	testTopicID = 11
	markerText  = "the quokka wears a tiny hat"
)

func testTarget() state.Message {
	return state.Message{
		ChatID:    testChatID,
		TopicID:   testTopicID,
		MessageID: 42,
		FromID:    1001,
		Name:      "Dana",
		Text:      markerText,
		CreatedAt: testTime(),
	}
}

func testHistory() HistorySnapshot {
	target := testTarget()

	return HistorySnapshot{
		Messages: []state.Message{
			{ChatID: testChatID, MessageID: 40, Name: "Ivan", Text: "earlier message one", CreatedAt: testTime()},
			{ChatID: testChatID, MessageID: 41, Name: "Marta", Text: "earlier message two", CreatedAt: testTime()},
			target,
		},
	}
}

func testEngineDeps(t *testing.T) (EngineDeps, *fakeDecisionClient, *fakeModelResolver, *fakePersonaSource, *fakeHistorySource) {
	t.Helper()

	client := &fakeDecisionClient{answers: allValidAnswers()}
	resolver := &fakeModelResolver{generation: "gemma4:e4b", decision: "laya"}
	persona := &fakePersonaSource{persona: "You are Ada, a careful assistant."}
	history := &fakeHistorySource{snapshot: testHistory()}

	return EngineDeps{
		Client:           client,
		Models:           resolver,
		Persona:          persona,
		History:          history,
		InputBudgetBytes: 16 * 1024,
	}, client, resolver, persona, history
}

func newTestEngine(t *testing.T) (*Engine, *fakeDecisionClient, *fakeModelResolver, *fakePersonaSource, *fakeHistorySource) {
	t.Helper()

	deps, client, resolver, persona, history := testEngineDeps(t)
	engine, err := NewEngine(deps, discardLogger())
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	return engine, client, resolver, persona, history
}

// allValidAnswers returns five maximal-but-valid answers: everything at 1
// except intrusion.
func allValidAnswers() map[string]systemone.Answer {
	return map[string]systemone.Answer{
		QuestionRelevance:     {Type: systemone.TypeScore, Score: 3},
		QuestionIntrusion:     {Type: systemone.TypeNoul, Noul: 0.1},
		QuestionContribution:  {Type: systemone.TypeNoul, Noul: 1},
		QuestionReactSuitable: {Type: systemone.TypeNoul, Noul: 1},
		QuestionReplySuitable: {Type: systemone.TypeNoul, Noul: 1},
	}
}

func testInput() DecisionInput {
	return DecisionInput{
		ChatID:        testChatID,
		TopicID:       testTopicID,
		TargetID:      testTarget().MessageID,
		HydrateTarget: func(context.Context) (state.Message, error) { return testTarget(), nil },
		Profile:       adminconfig.ProfileTalkLow,
		Instructions:  "Engage with programming topics.",
		Persona: PersonaSettings{
			Language:      "Russian",
			Gender:        "female",
			CharacterName: "Ada",
			TemplateBody:  "captured-template-body",
		},
	}
}

// TestEngineNoOpProfileShortCircuits pins the engine NoOp contract: a
// Non-Interactive (or unknown) profile ignores before any resolution,
// persona rendering, history retrieval, or HTTP.
func TestEngineNoOpProfileShortCircuits(t *testing.T) {
	for _, profile := range []adminconfig.InteractionProfile{
		adminconfig.ProfileNonInteractive,
		adminconfig.InteractionProfile("bogus_profile"),
	} {
		engine, client, resolver, persona, history := newTestEngine(t)

		decision, err := engine.Decide(context.Background(), DecisionInput{Profile: profile})
		if err != nil {
			t.Fatalf("NoOp must not error, got %v", err)
		}
		if decision.Action != ActionIgnore || decision.Reason != ReasonProfileDisabled {
			t.Fatalf("NoOp must ignore as profile-disabled, got %s/%s", decision.Action, decision.Reason)
		}
		if client.calls != 0 || resolver.decisionCalls != 0 || resolver.generationCalls != 0 || persona.calls != 0 || history.calls != 0 {
			t.Fatalf("NoOp must not touch any dependency (client %d, resolver %d/%d, persona %d, history %d)",
				client.calls, resolver.decisionCalls, resolver.generationCalls, persona.calls, history.calls)
		}
	}
}

// TestEngineUnconfiguredScope pins unset-vs-failure: an unset decision model
// ignores as unconfigured without calling the backend; a resolution failure
// propagates wrapped but still ignores.
func TestEngineUnconfiguredScope(t *testing.T) {
	t.Run("unset decision model", func(t *testing.T) {
		engine, client, resolver, _, _ := newTestEngine(t)
		resolver.decision = ""

		decision, err := engine.Decide(context.Background(), testInput())
		if decision.Action != ActionIgnore || decision.Reason != ReasonUnconfigured {
			t.Fatalf("unconfigured must ignore as unconfigured, got %s/%s", decision.Action, decision.Reason)
		}
		if !errors.Is(err, ErrUnconfigured) {
			t.Fatalf("expected ErrUnconfigured, got %v", err)
		}
		if client.calls != 0 || resolver.generationCalls != 0 {
			t.Fatalf("unconfigured must stop before generation resolution and HTTP (gen %d, client %d)",
				resolver.generationCalls, client.calls)
		}
	})

	t.Run("resolution failure", func(t *testing.T) {
		engine, client, resolver, _, _ := newTestEngine(t)
		resolver.err = errors.New("db locked")

		decision, err := engine.Decide(context.Background(), testInput())
		if decision.Action != ActionIgnore {
			t.Fatalf("resolution failure must ignore, got %s", decision.Action)
		}
		if !errors.Is(err, ErrModelResolution) || !errors.Is(err, resolver.err) {
			t.Fatalf("resolution failure must wrap ErrModelResolution and the cause, got %v", err)
		}
		if client.calls != 0 {
			t.Fatalf("resolution failure must not reach the backend")
		}
	})

	t.Run("persona failure stays content-free", func(t *testing.T) {
		engine, client, _, persona, history := newTestEngine(t)
		// Template parse/exec failures quote template source; the renderer's
		// error must never reach the engine's error text.
		persona.err = errors.New(`template: chat:1: unexpected "{{" in operand near "You are Ada, {{.Secret}}"`)

		decision, err := engine.Decide(context.Background(), testInput())
		if decision.Action != ActionIgnore {
			t.Fatalf("persona failure must ignore, got %s", decision.Action)
		}
		if !errors.Is(err, ErrModelResolution) {
			t.Fatalf("persona failure must classify as ErrModelResolution, got %v", err)
		}
		if strings.Contains(err.Error(), "Secret") || strings.Contains(err.Error(), "template") {
			t.Fatalf("persona error must stay content-free, got %q", err.Error())
		}
		if history.calls != 0 || client.calls != 0 {
			t.Fatalf("persona failure must stop before history and backend (history %d, client %d)", history.calls, client.calls)
		}
	})
}

// hydrateSpy counts target preparations and scripts failures.
type hydrateSpy struct {
	calls int
	err   error
}

func (h *hydrateSpy) prepare(context.Context) (state.Message, error) {
	h.calls++

	return testTarget(), h.err
}

// TestEngineTargetPreparationOrdering pins the lazy-target boundary: target
// preparation — the only step that may download files and run vision
// inference — runs strictly after the profile and decision-model gates, and
// its failures are typed, content-free, and ignored.
func TestEngineTargetPreparationOrdering(t *testing.T) {
	t.Run("profile-disabled never prepares the target", func(t *testing.T) {
		engine, _, _, _, _ := newTestEngine(t)
		spy := &hydrateSpy{}

		input := DecisionInput{Profile: adminconfig.ProfileNonInteractive, HydrateTarget: spy.prepare}
		if _, err := engine.Decide(context.Background(), input); err != nil {
			t.Fatalf("NoOp must not error, got %v", err)
		}
		if spy.calls != 0 {
			t.Fatalf("NoOp must not prepare the target, got %d preparations", spy.calls)
		}
	})

	t.Run("unconfigured scope never prepares the target", func(t *testing.T) {
		engine, _, resolver, _, _ := newTestEngine(t)
		resolver.decision = ""
		spy := &hydrateSpy{}

		input := testInput()
		input.HydrateTarget = spy.prepare
		decision, err := engine.Decide(context.Background(), input)
		if decision.Reason != ReasonUnconfigured || !errors.Is(err, ErrUnconfigured) {
			t.Fatalf("expected unconfigured ignore, got %s/%v", decision.Reason, err)
		}
		if spy.calls != 0 {
			t.Fatalf("unconfigured scope must not pay for preparation, got %d", spy.calls)
		}
	})

	t.Run("hydration failure is typed and content-free", func(t *testing.T) {
		engine, _, _, persona, history := newTestEngine(t)
		spy := &hydrateSpy{err: errors.New(`Get "http://vision:9/v1/chat/completions?key=PRIVATE": PRIVATE_PHOTO_BYTES`)}

		input := testInput()
		input.HydrateTarget = spy.prepare
		decision, err := engine.Decide(context.Background(), input)
		if decision.Action != ActionIgnore {
			t.Fatalf("preparation failure must ignore, got %s", decision.Action)
		}
		if !errors.Is(err, ErrTargetUnavailable) {
			t.Fatalf("expected ErrTargetUnavailable, got %v", err)
		}
		if strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "vision:9") {
			t.Fatalf("preparation error must stay content-free, got %q", err.Error())
		}
		if persona.calls != 0 || history.calls != 0 {
			t.Fatalf("preparation failure must stop before persona and history (persona %d, history %d)",
				persona.calls, history.calls)
		}
	})

	t.Run("nil provider fails the decision", func(t *testing.T) {
		engine, _, _, _, _ := newTestEngine(t)

		input := testInput()
		input.HydrateTarget = nil
		decision, err := engine.Decide(context.Background(), input)
		if decision.Action != ActionIgnore || !errors.Is(err, ErrTargetUnavailable) {
			t.Fatalf("nil provider must fail as ErrTargetUnavailable, got %s/%v", decision.Action, err)
		}
	})
}

// TestEngineDecideHappyPath pins the request composition and the answer
// mapping on a full Talk-Low decision.
func TestEngineDecideHappyPath(t *testing.T) {
	engine, client, resolver, persona, history := newTestEngine(t)

	decision, err := engine.Decide(context.Background(), testInput())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.Action != ActionReply {
		t.Fatalf("maximal valid answers must reply on talk-low, got %s/%s", decision.Action, decision.Reason)
	}
	if decision.Scores.Relevance != 1 || decision.Scores.Intrusion != 0.1 {
		t.Fatalf("scores must be mapped and normalized: %+v", decision.Scores)
	}

	// Dependencies engaged exactly once per decision.
	if resolver.decisionCalls != 1 || resolver.generationCalls != 1 || persona.calls != 1 || history.calls != 1 {
		t.Fatalf("one decision must resolve each input once (resolver %d/%d, persona %d, history %d)",
			resolver.decisionCalls, resolver.generationCalls, persona.calls, history.calls)
	}
	// The persona renders from the snapshot's settings, not a fresh read:
	// the captured template body travels with it, so the decision renders
	// the exact body the operation captured.
	if persona.settings != (PersonaSettings{Language: "Russian", Gender: "female", CharacterName: "Ada", TemplateBody: "captured-template-body"}) {
		t.Fatalf("persona must receive the input's snapshot settings incl. the captured template body, got %+v", persona.settings)
	}

	req := client.request
	if req.Model != "laya" {
		t.Fatalf("request must carry the resolved decision model, got %q", req.Model)
	}
	if len(req.Questions) != 5 {
		t.Fatalf("rubric must ask five questions, got %d", len(req.Questions))
	}
	if q := req.Questions[QuestionRelevance]; q.Type != systemone.TypeScore || len(q.ScoreLevels()) != len(relevanceScoreLevels) {
		t.Fatalf("relevance must be the four-level score question, got %+v", q)
	}
	for _, id := range []string{QuestionIntrusion, QuestionContribution, QuestionReactSuitable, QuestionReplySuitable} {
		if q := req.Questions[id]; q.Type != systemone.TypeNoul {
			t.Fatalf("%s must be a noul question, got %+v", id, q)
		}
	}

	// The state payload carries the delineated sections and the untrusted
	// data note; the target renders exactly once.
	stateBody, err := json.Marshal(req.State)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	rendered := string(stateBody)
	if !strings.Contains(rendered, "You are Ada") {
		t.Fatalf("state must carry the persona: %s", rendered)
	}
	if !strings.Contains(rendered, "Engage with programming topics.") {
		t.Fatalf("state must carry the decision instructions: %s", rendered)
	}
	if !strings.Contains(rendered, "untrusted") {
		t.Fatalf("state must state that conversation data is untrusted: %s", rendered)
	}
	if got := strings.Count(rendered, markerText); got != 1 {
		t.Fatalf("target must render exactly once, got %d occurrences", got)
	}
	if !strings.Contains(rendered, "unknown") {
		t.Fatalf("activity without a bot message must read as unknown: %s", rendered)
	}
}

// TestEngineDecisionErrorsFailSilent pins the typed, content-free failure
// paths: every error still returns Ignore, and error text never carries
// message content.
func TestEngineDecisionErrorsFailSilent(t *testing.T) {
	t.Run("backend transport error", func(t *testing.T) {
		engine, client, _, _, _ := newTestEngine(t)
		client.err = errors.New("post \"http://vision-host/v1/systemone\": connection refused")

		decision, err := engine.Decide(context.Background(), testInput())
		if decision.Action != ActionIgnore {
			t.Fatalf("transport failure must ignore, got %s", decision.Action)
		}
		if !errors.Is(err, ErrBackendUnavailable) {
			t.Fatalf("transport failure must wrap ErrBackendUnavailable, got %v", err)
		}
	})

	t.Run("malformed response", func(t *testing.T) {
		engine, client, _, _, _ := newTestEngine(t)
		client.err = systemone.ErrMalformed

		decision, err := engine.Decide(context.Background(), testInput())
		if decision.Action != ActionIgnore {
			t.Fatalf("malformed response must ignore, got %s", decision.Action)
		}
		if !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("malformed response must map to ErrInvalidResponse, got %v", err)
		}
	})

	t.Run("missing answer", func(t *testing.T) {
		engine, client, _, _, _ := newTestEngine(t)
		answers := allValidAnswers()
		delete(answers, QuestionReplySuitable)
		client.answers = answers

		decision, err := engine.Decide(context.Background(), testInput())
		if decision.Action != ActionIgnore || decision.Reason != ReasonEvidenceInvalid {
			t.Fatalf("missing answer must ignore as invalid evidence, got %s/%s", decision.Action, decision.Reason)
		}
		if !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("missing answer must map to ErrInvalidResponse, got %v", err)
		}
	})

	t.Run("persona failure", func(t *testing.T) {
		engine, client, _, persona, _ := newTestEngine(t)
		persona.err = errors.New("db locked")

		decision, err := engine.Decide(context.Background(), testInput())
		if decision.Action != ActionIgnore || !errors.Is(err, ErrModelResolution) {
			t.Fatalf("persona failure must ignore with ErrModelResolution, got %s/%v", decision.Action, err)
		}
		if client.calls != 0 {
			t.Fatalf("persona failure must not reach the backend")
		}
	})

	t.Run("history failure", func(t *testing.T) {
		engine, client, _, _, history := newTestEngine(t)
		history.err = errors.New("store gone")

		decision, err := engine.Decide(context.Background(), testInput())
		if decision.Action != ActionIgnore || !errors.Is(err, ErrHistoryUnavailable) {
			t.Fatalf("history failure must ignore with ErrHistoryUnavailable, got %s/%v", decision.Action, err)
		}
		if client.calls != 0 {
			t.Fatalf("history failure must not reach the backend")
		}
	})

	t.Run("errors never echo content", func(t *testing.T) {
		engine, client, _, _, _ := newTestEngine(t)
		client.err = errors.New("connection refused")

		_, err := engine.Decide(context.Background(), testInput())
		if strings.Contains(err.Error(), markerText) || strings.Contains(err.Error(), "Ada") {
			t.Fatalf("decision errors must stay content-free, got %q", err.Error())
		}
	})
}

// TestEngineNewEngineValidation pins construction-time dependency checks.
func TestEngineNewEngineValidation(t *testing.T) {
	deps, _, _, _, _ := testEngineDeps(t)

	missing := deps
	missing.Client = nil
	if _, err := NewEngine(missing, discardLogger()); err == nil {
		t.Fatalf("nil client must be rejected")
	}

	noBudget := deps
	noBudget.InputBudgetBytes = 0
	if _, err := NewEngine(noBudget, discardLogger()); err == nil {
		t.Fatalf("non-positive budget must be rejected")
	}
}
