package statistics

import "time"

// ModeLive is the only observation mode recorded in this PR. A future
// shadow-observation mode threads through the same key struct and snapshot
// filtering (see InteractionKeys.Mode).
const ModeLive = "live"

// Overflow-disclosure constants shared with the presenters. Overflow is
// never rendered as an ID-shaped string: the snapshot carries explicit
// boolean tags and null IDs instead.
const (
	// MaxTrackedChats bounds the distinct chats holding per-chat state
	// across the decision, execution, preflight, and submission maps
	// TOGETHER. Further chats fold into the aggregate overflow chat key.
	MaxTrackedChats = 32
	// MaxTrackedModels bounds the distinct model IDs per model dimension
	// (decision, generation). Further values fold into the per-dimension
	// overflow key.
	MaxTrackedModels = 8
	// maxModelIDBytes bounds a retained model identifier. Oversized
	// identifiers fold into the overflow bucket WITHOUT truncation —
	// truncated IDs would silently merge distinct models sharing a prefix.
	maxModelIDBytes = 128
)

// InteractionScope selects the chat slice of an interaction snapshot.
type InteractionScope struct {
	ChatID        int64
	ChatSpecified bool
}

// GlobalScope is the unfiltered, chat-agnostic scope.
func GlobalScope() InteractionScope { return InteractionScope{} }

// ChatScope selects one chat's observations.
func ChatScope(chatID int64) InteractionScope {
	return InteractionScope{ChatID: chatID, ChatSpecified: true}
}

// InteractionFilters narrow an interaction snapshot by bucket dimensions.
// Empty values mean no filter. Model filters never match overflow buckets:
// their attribution is unknown and disclosed instead.
type InteractionFilters struct {
	Profile       string
	DecisionModel string
}

// Narrowed reports whether the filters select a subset of the dataset.
func (f InteractionFilters) Narrowed() bool {
	return f.Profile != "" || f.DecisionModel != ""
}

// InteractionPeriod is the observation period of the interaction dataset.
type InteractionPeriod struct {
	StartedAt time.Time `json:"started_at"`
	Resets    uint64    `json:"resets"`
}

// SubmissionTotals is the submission ledger. Accepted is the eligibility
// denominator; pending decision phases are derivable as
// Accepted - terminated decision-phase classifications (§ invariant).
type SubmissionTotals struct {
	Submitted             uint64 `json:"submitted"`
	SubmitRejected        uint64 `json:"submit_rejected"`
	AcceptedThenCancelled uint64 `json:"accepted_then_cancelled"`
}

// Accepted derives the accepted-submission count. Submit returns false only
// for a closed runner (never launching the task), so SubmitRejected is
// disjoint from every outcome-recording task by construction.
func (s SubmissionTotals) Accepted() uint64 { return s.Submitted - s.SubmitRejected }

// PreflightTotals counts the pre-engine skips, chat-keyed. These explain
// "why didn't it participate" without decision buckets.
type PreflightTotals struct {
	AccessDenied           uint64 `json:"access_denied"`
	ConfigResolutionFailed uint64 `json:"config_resolution_failed"`
	EligibilityLost        uint64 `json:"eligibility_lost"`
	PersonaCaptureFailed   uint64 `json:"persona_capture_failed"`
	EngineWiringFailed     uint64 `json:"engine_wiring_failed"`
}

// Sum returns the total preflight count.
func (p PreflightTotals) Sum() uint64 {
	return p.AccessDenied + p.ConfigResolutionFailed + p.EligibilityLost +
		p.PersonaCaptureFailed + p.EngineWiringFailed
}

// PreflightChatBucket is one chat's preflight counts. A nil ChatID with
// ChatOverflow set is the aggregate overflow chat: observations whose exact
// chat attribution the hard caps folded away.
type PreflightChatBucket struct {
	ChatID       *int64          `json:"chat_id"`
	ChatOverflow bool            `json:"chat_overflow"`
	Counts       PreflightTotals `json:"counts"`
}

// PreflightSection is the preflight dataset. Chats is sorted (overflow last).
type PreflightSection struct {
	Totals PreflightTotals       `json:"totals"`
	Chats  []PreflightChatBucket `json:"chats"`
}

// InteractionOutcomeTotals is the bounded decision-outcome enum, one
// terminal classification per accepted task's decision phase. The addends
// are mutually exclusive: a generic "decision_outcomes" term must never be
// added on top (NoOp, unconfigured and preparation failures ARE outcome-enum
// members).
type InteractionOutcomeTotals struct {
	NoopProfileDisabled uint64 `json:"noop_profile_disabled"`
	Unconfigured        uint64 `json:"unconfigured"`
	PreparationFailed   uint64 `json:"preparation_failed"`
	BackendTimeout      uint64 `json:"backend_timeout"`
	BackendCancelled    uint64 `json:"backend_cancelled"`
	BackendAuth         uint64 `json:"backend_auth"`
	BackendFailed       uint64 `json:"backend_failed"`
	InvalidResponse     uint64 `json:"invalid_response"`
	EvidenceInvalid     uint64 `json:"evidence_invalid"`
	SelectedIgnoreVeto  uint64 `json:"selected_ignore_veto"`
	SelectedIgnoreBelow uint64 `json:"selected_ignore_below"`
	SelectedReact       uint64 `json:"selected_react"`
	SelectedReply       uint64 `json:"selected_reply"`
}

// Sum returns the total terminated decision phases recorded in these totals.
func (o InteractionOutcomeTotals) Sum() uint64 {
	return o.NoopProfileDisabled + o.Unconfigured + o.PreparationFailed +
		o.BackendTimeout + o.BackendCancelled + o.BackendAuth + o.BackendFailed +
		o.InvalidResponse + o.EvidenceInvalid + o.SelectedIgnoreVeto +
		o.SelectedIgnoreBelow + o.SelectedReact + o.SelectedReply
}

// InferenceErrorClassTotals counts Client.Evaluate invocations by
// sentinel-derived error class. Transport failures and non-2xx HTTP statuses
// share the Unavailable class: the client wraps both in one sentinel, and
// they are not distinguishable without parsing error strings (never done).
type InferenceErrorClassTotals struct {
	PretransportRejected uint64 `json:"pretransport_rejected"`
	NotConfigured        uint64 `json:"not_configured"`
	Timeout              uint64 `json:"timeout"`
	Cancelled            uint64 `json:"cancelled"`
	Auth                 uint64 `json:"auth"`
	Unavailable          uint64 `json:"unavailable"`
	InvalidResponse      uint64 `json:"invalid_response"`
}

// HistogramSnapshot is a self-describing histogram: finite edges plus the
// open-ended flag. No +Inf value is ever stored or serialized; latency
// histograms carry OpenEnded for their terminal [lastEdge, ∞) bin, score
// histograms are closed on [0,1].
type HistogramSnapshot struct {
	Edges     []float64 `json:"edges"`
	OpenEnded bool      `json:"open_ended"`
	Counts    []uint64  `json:"counts"`
}

// PercentileValue is one approximate percentile: a histogram
// interpolation, a terminal-bin lower bound, or nil when unavailable (an
// empty histogram reports null, never zero).
type PercentileValue struct {
	ApproxMs  *float64 `json:"approx_ms,omitempty"`
	AtLeastMs *float64 `json:"at_least_ms,omitempty"`
}

// LatencyPercentiles are the p50/p90/p99 approximations of one latency
// histogram.
type LatencyPercentiles struct {
	P50 *PercentileValue `json:"p50"`
	P90 *PercentileValue `json:"p90"`
	P99 *PercentileValue `json:"p99"`
}

// ScoreHistograms holds the base score and the five component score
// distributions.
type ScoreHistograms struct {
	Base             HistogramSnapshot `json:"base"`
	Relevance        HistogramSnapshot `json:"relevance"`
	Contribution     HistogramSnapshot `json:"contribution"`
	Intrusion        HistogramSnapshot `json:"intrusion"`
	ReactSuitability HistogramSnapshot `json:"react_suitability"`
	ReplySuitability HistogramSnapshot `json:"reply_suitability"`
}

// DecisionTotals is the decision-phase dataset at the selected scope.
// EvaluatedAttempts counts Client.Evaluate INVOCATIONS (not necessarily
// HTTP requests); no exact HTTP-request count is claimed — the client also
// folds request-construction failures into its Unavailable sentinel, so
// attempts minus pre-transport rejections is only an upper bound.
type DecisionTotals struct {
	EvaluatedAttempts    uint64                    `json:"evaluated_attempts"`
	PretransportRejected uint64                    `json:"pretransport_rejected"`
	Outcomes             InteractionOutcomeTotals  `json:"outcomes"`
	InferenceErrors      InferenceErrorClassTotals `json:"inference_error_classes"`
	ScoreHistograms      ScoreHistograms           `json:"score_histograms"`
	Latency              LatencySection            `json:"latency_ms"`
}

// LatencySection carries the decision and inference latency histograms
// with their approximate percentiles.
type LatencySection struct {
	Decision    HistogramSnapshot `json:"decision"`
	Inference   HistogramSnapshot `json:"inference"`
	Percentiles struct {
		Decision  LatencyPercentiles `json:"decision"`
		Inference LatencyPercentiles `json:"inference"`
	} `json:"percentiles_approx"`
}

// DecisionBucket is one (chat, profile, decision-model) slice of the
// decision dataset. Nil ChatID / nil DecisionModel with the overflow flags
// set are the aggregate overflow keys, never real identifiers.
type DecisionBucket struct {
	ChatID                *int64                   `json:"chat_id"`
	ChatOverflow          bool                     `json:"chat_overflow"`
	Profile               string                   `json:"profile"`
	DecisionModel         *string                  `json:"decision_model"`
	DecisionModelOverflow bool                     `json:"decision_model_overflow"`
	Counters              InteractionOutcomeTotals `json:"counters"`
	Histograms            ScoreHistograms          `json:"histograms"`
}

// DecisionSection is the decision dataset at the selected scope.
type DecisionSection struct {
	Totals  DecisionTotals   `json:"totals"`
	Buckets []DecisionBucket `json:"buckets"`
}

// ExecutionCounters is the turn-level execution accounting. Effect counters
// count confirmed effects REGARDLESS of completion: a partial success
// (reaction applied, generation failed) lands in BOTH ReactionsConfirmed
// and TurnsFailed — the accurate representation. TurnsSucceededWithoutEffect
// counts successful turns with no confirmed effect (reaction participation,
// text participation, and other-side-effect-only execution remain separately
// observable).
type ExecutionCounters struct {
	DispatchedReact    uint64                 `json:"dispatched_react"`
	DispatchedReply    uint64                 `json:"dispatched_reply"`
	ReactionsConfirmed uint64                 `json:"reactions_confirmed"`
	TextsConfirmed     uint64                 `json:"texts_confirmed"`
	OtherSideEffects   uint64                 `json:"other_side_effects"`
	TurnsWithEffect    uint64                 `json:"turns_with_effect"`
	TurnsWithoutEffect uint64                 `json:"turns_without_effect"`
	TurnsFailed        uint64                 `json:"turns_failed"`
	Failures           ExecutionFailureTotals `json:"failures"`
}

// ExecutionFailureTotals is the bounded execution-failure enum.
// AbandonedTarget is defensive only — unreachable in production wiring.
type ExecutionFailureTotals struct {
	Timeout         uint64 `json:"timeout"`
	Cancelled       uint64 `json:"cancelled"`
	Generation      uint64 `json:"generation"`
	Delivery        uint64 `json:"delivery"`
	AbandonedTarget uint64 `json:"abandoned_target"`
}

// Sum returns the total classified execution failures.
func (f ExecutionFailureTotals) Sum() uint64 {
	return f.Timeout + f.Cancelled + f.Generation + f.Delivery + f.AbandonedTarget
}

// ExecutionTotals is the execution-phase dataset at the selected scope.
type ExecutionTotals struct {
	ExecutionCounters
	Duration    HistogramSnapshot  `json:"duration_ms"`
	Percentiles LatencyPercentiles `json:"duration_percentiles"`
}

// ExecutionBucket is one (chat, profile, decision-model, generation-model)
// slice. BOTH effective model identifiers are retained so execution
// outcomes stay filterable by decision model; the generation model records
// the EFFECTIVE identifier (the ENV route model when the chat override is
// empty), never an empty override string.
type ExecutionBucket struct {
	ChatID                  *int64            `json:"chat_id"`
	ChatOverflow            bool              `json:"chat_overflow"`
	Profile                 string            `json:"profile"`
	DecisionModel           *string           `json:"decision_model"`
	DecisionModelOverflow   bool              `json:"decision_model_overflow"`
	GenerationModel         *string           `json:"generation_model"`
	GenerationModelOverflow bool              `json:"generation_model_overflow"`
	Counters                ExecutionCounters `json:"counters"`
	Duration                HistogramSnapshot `json:"duration_ms"`
}

// ExecutionSection is the execution dataset at the selected scope.
type ExecutionSection struct {
	Totals  ExecutionTotals   `json:"totals"`
	Buckets []ExecutionBucket `json:"buckets"`
}

// InteractionSnapshot is the immutable interaction-statistics view of one
// instant. The JSON presenters consume this struct directly; the PNG
// presenter renders from the same value, so labels and export numbers
// cannot drift apart.
type InteractionSnapshot struct {
	GeneratedAt   time.Time         `json:"generated_at"`
	Mode          string            `json:"mode"`
	RubricVersion int               `json:"rubric_version"`
	Period        InteractionPeriod `json:"observation_period"`

	// Submission and Preflight are chat-keyed only: under profile/model
	// filters they are OMITTED (nil), never misleadingly whole. Chat scope
	// keeps everything.
	Submission *SubmissionTotals `json:"submission"`
	Preflight  *PreflightSection `json:"preflight"`

	Decision  DecisionSection  `json:"decision"`
	Execution ExecutionSection `json:"execution"`

	// FilteredTotalsExact is false when the view is narrowed while overflow
	// folding is active: exact global totals never imply exact filtered
	// totals, because filters sum buckets and overflow buckets cannot be
	// attributed.
	FilteredTotalsExact bool `json:"filtered_totals_exact"`
	// UntrackedSelection is set when a narrowed selection matches no
	// tracked buckets while overflow folding is active: bare zeros are
	// never presented as truth — the selection's activity may live in an
	// overflow bucket.
	UntrackedSelection string `json:"untracked_selection"`
	// OversizedIDFolds counts observations folded into a model overflow key
	// because the identifier exceeded the size bound (>128 bytes, no
	// truncation). It distinguishes size-driven folds from the
	// cardinality-cap folds in the overflow disclosure.
	OversizedIDFolds uint64 `json:"oversized_id_folds"`
}
