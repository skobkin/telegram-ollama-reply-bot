package interactivity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/systemone"
)

// InteractionAction is the action the policy selected for one message.
// ActionIgnore is the safe zero value.
type InteractionAction uint8

const (
	ActionIgnore InteractionAction = iota
	ActionReact
	ActionReply
)

// String is the content-free action name for logs.
func (a InteractionAction) String() string {
	switch a {
	case ActionReact:
		return "react"
	case ActionReply:
		return "reply"
	default:
		return "ignore"
	}
}

// DecisionReason is a bounded enum describing why the decision was reached.
// It is never model-generated text, so it is safe for ordinary logs.
type DecisionReason uint8

const (
	// ReasonProfileDisabled: the effective profile is Non-Interactive (or
	// otherwise unknown), decided before any resolution or HTTP.
	ReasonProfileDisabled DecisionReason = iota
	// ReasonUnconfigured: the scope has no effective System One model.
	ReasonUnconfigured
	// ReasonEvidenceInvalid: the answers could not be mapped to usable
	// evidence (missing, mistyped, or out of range).
	ReasonEvidenceInvalid
	// ReasonIntrusionVeto: the hard intrusion veto fired.
	ReasonIntrusionVeto
	// ReasonBelowThresholds: no action gate passed.
	ReasonBelowThresholds
	// ReasonReact: the reaction gate passed.
	ReasonReact
	// ReasonReply: the reply gate passed.
	ReasonReply
)

// String is the content-free reason name for logs.
func (r DecisionReason) String() string {
	switch r {
	case ReasonUnconfigured:
		return "unconfigured"
	case ReasonEvidenceInvalid:
		return "evidence_invalid"
	case ReasonIntrusionVeto:
		return "intrusion_veto"
	case ReasonBelowThresholds:
		return "below_thresholds"
	case ReasonReact:
		return "react"
	case ReasonReply:
		return "reply"
	default:
		return "profile_disabled"
	}
}

// ActionDecision is one engine verdict. Scores are zero when the decision
// short-circuits before inference.
type ActionDecision struct {
	Action InteractionAction
	Scores DecisionScores
	Reason DecisionReason
}

// PersonaSettings are the persona-defining fields of the operation
// snapshot, carried into the decision so the persona is rendered from the
// snapshot instead of a fresh settings read: the decision and its resulting
// action must describe the same character even if settings change
// mid-decision. TemplateBody is the chat prompt template body captured at
// operation start — the same captured body the action turn renders.
type PersonaSettings struct {
	Language      string
	Gender        string
	CharacterName string
	TemplateBody  string
}

// DecisionInput is the domain input of one decision. Profile and
// Instructions come from the caller's settings snapshot, taken once when the
// operation begins: the engine never re-reads settings mid-decision, so a
// runtime change affects only operations that start later.
type DecisionInput struct {
	ChatID  int64
	TopicID int
	// TargetID is the triggering message's ID. Together with ChatID it
	// identifies the message for history exclusion — the middleware has
	// already saved it into history — without preparing it first.
	TargetID int
	// HydrateTarget lazily prepares the triggering message for the context:
	// sender and reply attribution, and image recognition for photos. The
	// engine invokes it only after the profile and decision-model gates
	// pass, so an opted-out or unconfigured scope never pays for vision
	// inference or downloads; implementations memoize, so the resulting
	// action reuses the work. A nil provider fails the decision once the
	// gates pass — the input contract requires it.
	HydrateTarget func(context.Context) (state.Message, error)
	// Profile is the effective interaction profile of the operation snapshot.
	Profile adminconfig.InteractionProfile
	// Instructions is the effective decision-instructions text ("" = none).
	Instructions string
	// BotUserID is the bot's own Telegram user ID. Reaction attribution in
	// the context depends on it: bot-authored reactions are the record of
	// its previous participation. Zero disables bot attribution.
	BotUserID int64
	// Persona carries the snapshot's persona fields for the context builder.
	Persona PersonaSettings
}

// Typed engine errors. They are content-free: nothing about the message,
// history, persona, or request bodies ever appears in their text. Handlers
// treat every error as Ignore and never interpret one as an action.
var (
	// ErrUnconfigured means the scope has no effective System One model.
	ErrUnconfigured = errors.New("decision backend is not configured for this scope")
	// ErrModelResolution wraps settings-resolution failures (database
	// errors); unset and failed are distinct states.
	ErrModelResolution = errors.New("decision model resolution failed")
	// ErrBackendUnavailable wraps transport, timeout, and HTTP error
	// classes of the decision backend.
	ErrBackendUnavailable = errors.New("decision backend is unavailable")
	// ErrHistoryUnavailable wraps conversation-history read failures.
	ErrHistoryUnavailable = errors.New("conversation history is unavailable")
	// ErrTargetUnavailable wraps target-preparation failures (hydration,
	// image recognition, downloads).
	ErrTargetUnavailable = errors.New("decision target preparation failed")
	// ErrInvalidResponse marks protocol or semantic validation anomalies of
	// a response that passed transport but is not usable evidence.
	ErrInvalidResponse = errors.New("decision backend returned an invalid response")
	// ErrContextTooLarge is the fail-closed budget outcome: the mandatory
	// input alone cannot fit the composition budget.
	ErrContextTooLarge = errors.New("decision input exceeds the composition budget")
)

// HistorySnapshot is the bounded conversation history at execution start.
type HistorySnapshot struct {
	// Messages are ordered oldest to newest and already hydrated the same
	// way the ordinary turn path hydrates them.
	Messages []state.Message
	// EarlierSummary is the committed summary of the trimmed prefix ("" when
	// the full history is present).
	EarlierSummary string
}

// HistorySource returns the history snapshot of one conversation. Production
// implementations read through the existing volatile history store and bound
// and hydrate exactly like the ordinary turn path; tasks that waited see
// messages that arrived meanwhile as ordinary context.
type HistorySource interface {
	DecisionHistory(ctx context.Context, scope state.ConversationScope) (HistorySnapshot, error)
}

// PersonaSource renders the full effective character prompt used by ordinary
// generation, from the operation snapshot's captured persona. The generation
// model passed in is the operation snapshot's resolved generation model: any
// model-identity reference in the persona must describe the generation
// character, never the decision model. CapturePersonaTemplate resolves the
// chat prompt template body once at operation start — the same captured body
// the resulting action turn renders — so a template change mid-operation
// cannot split the persona between decision and action.
type PersonaSource interface {
	DecisionPersona(ctx context.Context, chatID int64, generationModel string, settings PersonaSettings) (string, error)
	CapturePersonaTemplate(ctx context.Context, chatID int64) (string, error)
}

// ModelResolver resolves the effective models of one operation. Production
// implementations memoize into the operation snapshot so the decision and
// its resulting action share one resolution. Empty strings are unset (an
// environment route or an unconfigured feature); errors are resolution
// failures — the two are distinct and never substituted for each other.
type ModelResolver interface {
	// GenerationModel resolves the chat generation model of the scope.
	GenerationModel(ctx context.Context, chatID int64) (string, error)
	// DecisionModel resolves the System One decision model of the scope; ""
	// means the decision backend is unconfigured for the scope.
	DecisionModel(ctx context.Context, chatID int64) (string, error)
}

// DecisionClient is the System One inference boundary. The production
// implementation is *systemone.Client.
type DecisionClient interface {
	Evaluate(ctx context.Context, req systemone.Request) (map[string]systemone.Answer, error)
}

// EngineDeps wire the engine's boundaries.
type EngineDeps struct {
	Client  DecisionClient
	Models  ModelResolver
	Persona PersonaSource
	History HistorySource
	// InputBudgetBytes caps the whole serialized decision request (persona,
	// instructions, history, target, rubric, protocol overhead). It is a
	// composition limit, not a fit guarantee for the backend's context
	// window; overflow errors there fail silent to Ignore as well.
	InputBudgetBytes int64
}

// Engine is the production InteractionDecisionEngine.
type Engine struct {
	deps   EngineDeps
	logger *slog.Logger
}

// NewEngine builds the engine. Dependencies must be non-nil.
func NewEngine(deps EngineDeps, logger *slog.Logger) (*Engine, error) {
	if deps.Client == nil || deps.Models == nil || deps.Persona == nil || deps.History == nil {
		return nil, errors.New("interactivity engine requires client, model resolver, persona source, and history source")
	}
	if deps.InputBudgetBytes <= 0 {
		return nil, errors.New("interactivity engine requires a positive input budget")
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &Engine{deps: deps, logger: logger}, nil
}

// Decide evaluates one eligible message. A Non-Interactive profile is a NoOp
// decided before any model resolution, persona rendering, history retrieval,
// target preparation, or HTTP, and an unset decision model skips everything
// expensive too — target preparation may run vision inference, so both
// gates pass before it starts. Every failure path returns Ignore plus a
// typed content-free error; callers must not interpret errors as actions.
func (e *Engine) Decide(ctx context.Context, input DecisionInput) (ActionDecision, error) {
	profile, known := ProfileByID(input.Profile)
	if !known || !profile.Enabled {
		// NoOp: zero HTTP, zero resolution — handlers need no separate
		// enable check.
		return ignoreDecision(ReasonProfileDisabled, DecisionScores{}), nil
	}

	// Resolve the operation's models once. Unset and failed are distinct:
	// an unset decision model makes the scope unconfigured (Ignore), a
	// resolution failure propagates as a typed error (Ignore, no fallback).
	decisionModel, err := e.deps.Models.DecisionModel(ctx, input.ChatID)
	if err != nil {
		return ignoreDecision(ReasonUnconfigured, DecisionScores{}), fmt.Errorf("%w: %w", ErrModelResolution, err)
	}
	if decisionModel == "" {
		return ignoreDecision(ReasonUnconfigured, DecisionScores{}), ErrUnconfigured
	}
	generationModel, err := e.deps.Models.GenerationModel(ctx, input.ChatID)
	if err != nil {
		return ignoreDecision(ReasonUnconfigured, DecisionScores{}), fmt.Errorf("%w: %w", ErrModelResolution, err)
	}

	// Both gates passed: the scope can act, so target preparation is
	// justified. Everything before this point cost settings reads only.
	target, err := e.hydrateTarget(ctx, input)
	if err != nil {
		return ignoreDecision(ReasonEvidenceInvalid, DecisionScores{}), err
	}

	request, err := e.buildRequest(ctx, input, target, generationModel, decisionModel)
	if err != nil {
		return ignoreDecision(ReasonEvidenceInvalid, DecisionScores{}), err
	}

	answers, err := e.deps.Client.Evaluate(ctx, request)
	if err != nil {
		// The client returns sanitized, content-free errors already; keep
		// the transport class distinct from protocol anomalies.
		if errors.Is(err, systemone.ErrMalformed) || errors.Is(err, systemone.ErrInvalidRequest) {
			return ignoreDecision(ReasonEvidenceInvalid, DecisionScores{}), fmt.Errorf("%w: %w", ErrInvalidResponse, err)
		}

		return ignoreDecision(ReasonEvidenceInvalid, DecisionScores{}), fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}

	scores, err := scoresFromAnswers(answers)
	if err != nil {
		return ignoreDecision(ReasonEvidenceInvalid, DecisionScores{}), err
	}

	return EvaluatePolicy(profile, scores), nil
}

// hydrateTarget invokes the input's lazy provider. A nil provider is an
// input-contract violation that fails the decision once the gates passed.
func (e *Engine) hydrateTarget(ctx context.Context, input DecisionInput) (state.Message, error) {
	if input.HydrateTarget == nil {
		return state.Message{}, fmt.Errorf("%w: no target provider", ErrTargetUnavailable)
	}

	target, err := input.HydrateTarget(ctx)
	if err != nil {
		// Content-free class only: the provider's error can quote provider
		// bodies and file identifiers, none of which may reach diagnostics.
		return state.Message{}, fmt.Errorf("%w: target preparation failed", ErrTargetUnavailable)
	}

	return target, nil
}

// buildRequest composes the System One request within the input budget:
// mandatory sections first, then the summary, then history dropped oldest
// first at message boundaries. Instructions and the target are never
// truncated; if the mandatory input alone cannot fit, the request fails
// closed.
func (e *Engine) buildRequest(ctx context.Context, input DecisionInput, target state.Message, generationModel, decisionModel string) (systemone.Request, error) {
	persona, err := e.deps.Persona.DecisionPersona(ctx, input.ChatID, generationModel, input.Persona)
	if err != nil {
		// Content-free class only: the renderer's error can quote template
		// source (parse/exec failures) and store internals, none of which
		// may reach diagnostics (see the package error contract above).
		return systemone.Request{}, fmt.Errorf("%w: persona rendering failed", ErrModelResolution)
	}

	history, err := e.deps.History.DecisionHistory(ctx, state.ConversationScope{ChatID: input.ChatID, TopicID: input.TopicID})
	if err != nil {
		return systemone.Request{}, fmt.Errorf("%w: %w", ErrHistoryUnavailable, err)
	}

	composer := requestComposer{
		budget:    e.deps.InputBudgetBytes,
		persona:   persona,
		state:     buildDecisionState(input, target, persona, history),
		questions: rubricQuestions(),
	}

	return composer.request(decisionModel)
}
