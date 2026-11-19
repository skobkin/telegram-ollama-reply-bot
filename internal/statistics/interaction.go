package statistics

import (
	"time"
)

// InteractionOutcome is the bounded decision-phase outcome enum: one
// terminal classification per accepted task's decision phase (§ invariant —
// the addends are mutually exclusive).
type InteractionOutcome uint8

const (
	OutcomeNoopProfileDisabled InteractionOutcome = iota
	OutcomeUnconfigured
	OutcomePreparationFailed
	OutcomeBackendTimeout
	OutcomeBackendCancelled
	OutcomeBackendAuth
	OutcomeBackendFailed
	OutcomeInvalidResponse
	OutcomeEvidenceInvalid
	OutcomeSelectedIgnoreVeto
	OutcomeSelectedIgnoreBelow
	OutcomeSelectedReact
	OutcomeSelectedReply
	outcomeCount
)

var outcomeSnapshotFields = [...]struct {
	index InteractionOutcome
	apply func(*InteractionOutcomeTotals, uint64)
}{
	{OutcomeNoopProfileDisabled, func(t *InteractionOutcomeTotals, v uint64) { t.NoopProfileDisabled = v }},
	{OutcomeUnconfigured, func(t *InteractionOutcomeTotals, v uint64) { t.Unconfigured = v }},
	{OutcomePreparationFailed, func(t *InteractionOutcomeTotals, v uint64) { t.PreparationFailed = v }},
	{OutcomeBackendTimeout, func(t *InteractionOutcomeTotals, v uint64) { t.BackendTimeout = v }},
	{OutcomeBackendCancelled, func(t *InteractionOutcomeTotals, v uint64) { t.BackendCancelled = v }},
	{OutcomeBackendAuth, func(t *InteractionOutcomeTotals, v uint64) { t.BackendAuth = v }},
	{OutcomeBackendFailed, func(t *InteractionOutcomeTotals, v uint64) { t.BackendFailed = v }},
	{OutcomeInvalidResponse, func(t *InteractionOutcomeTotals, v uint64) { t.InvalidResponse = v }},
	{OutcomeEvidenceInvalid, func(t *InteractionOutcomeTotals, v uint64) { t.EvidenceInvalid = v }},
	{OutcomeSelectedIgnoreVeto, func(t *InteractionOutcomeTotals, v uint64) { t.SelectedIgnoreVeto = v }},
	{OutcomeSelectedIgnoreBelow, func(t *InteractionOutcomeTotals, v uint64) { t.SelectedIgnoreBelow = v }},
	{OutcomeSelectedReact, func(t *InteractionOutcomeTotals, v uint64) { t.SelectedReact = v }},
	{OutcomeSelectedReply, func(t *InteractionOutcomeTotals, v uint64) { t.SelectedReply = v }},
}

// outcomeTotals converts the collector-side array into the snapshot struct.
func outcomeTotals(counts [outcomeCount]uint64) InteractionOutcomeTotals {
	var totals InteractionOutcomeTotals
	for _, f := range outcomeSnapshotFields {
		f.apply(&totals, counts[f.index])
	}

	return totals
}

// InferenceErrClass is the bounded error classification of one
// Client.Evaluate invocation, derived from typed sentinels ONLY — error
// strings are never parsed. Transport failures and non-2xx HTTP statuses
// share ClassUnavailable by client design.
type InferenceErrClass uint8

const (
	InferenceSuccess InferenceErrClass = iota
	InferencePretransportRejected
	InferenceNotConfigured
	InferenceTimeout
	InferenceCancelled
	InferenceAuth
	InferenceUnavailable
	InferenceInvalidResponse
	inferenceClassCount
)

// InteractionAction is the executed proactive action.
type InteractionAction uint8

const (
	ActionReact InteractionAction = iota
	ActionReply
	actionCount
)

// ExecFailure is the bounded execution-failure classification. None marks a
// completed turn.
type ExecFailure uint8

const (
	ExecFailureNone ExecFailure = iota
	ExecFailureTimeout
	ExecFailureCancelled
	ExecFailureGeneration
	ExecFailureDelivery
	ExecFailureAbandonedTarget // defensive only: unreachable in production wiring
	execFailureCount
)

// PreflightKind is the bounded pre-engine skip classification.
type PreflightKind uint8

const (
	PreflightAccessDenied PreflightKind = iota
	PreflightConfigResolutionFailed
	PreflightEligibilityLost
	PreflightPersonaCaptureFailed
	PreflightEngineWiringFailed
	preflightKindCount
)

// ScoreDim indexes the six score distributions (base + five components).
type ScoreDim uint8

const (
	ScoreDimBase ScoreDim = iota
	ScoreDimRelevance
	ScoreDimContribution
	ScoreDimIntrusion
	ScoreDimReactSuitability
	ScoreDimReplySuitability
	scoreDimCount
)

// ScoreSet is one evaluation's score vector: the policy base score plus the
// five components.
type ScoreSet struct {
	Base             float64
	Relevance        float64
	Contribution     float64
	Intrusion        float64
	ReactSuitability float64
	ReplySuitability float64
}

// dim reads one score by dimension.
func (s ScoreSet) dim(d ScoreDim) float64 {
	switch d {
	case ScoreDimBase:
		return s.Base
	case ScoreDimRelevance:
		return s.Relevance
	case ScoreDimContribution:
		return s.Contribution
	case ScoreDimIntrusion:
		return s.Intrusion
	case ScoreDimReactSuitability:
		return s.ReactSuitability
	case ScoreDimReplySuitability:
		return s.ReplySuitability
	}

	return 0
}

// InteractionKeys identifies one interaction operation: the bucket
// dimensions plus the observation mode. ChatID participates in the shared
// per-chat tracking bound; DecisionModel may be "" (unset — empty is not a
// valid configured model).
type InteractionKeys struct {
	ChatID        int64
	Profile       string
	DecisionModel string
	Mode          string
}

// normalize fills the mode default and validates the closed set.
func (k InteractionKeys) normalize() InteractionKeys {
	if k.Mode == "" {
		k.Mode = ModeLive
	}

	return k
}

// DecisionObservation records one terminal decision-phase classification.
// Exactly one observation per accepted task whose callback ran; a selected
// action (SelectedReact/SelectedReply) additionally continues into the
// execution ledger.
type DecisionObservation struct {
	Epoch           uint64 // captured at submission, never at task entry
	Keys            InteractionKeys
	Outcome         InteractionOutcome
	Scores          *ScoreSet // non-nil only when evaluation succeeded
	DecisionLatency time.Duration
}

// InferenceAttempt records one Client.Evaluate INVOCATION observed by the
// instrumentation decorator. An invocation is not necessarily an HTTP
// request: pre-transport rejections are their own class.
type InferenceAttempt struct {
	Epoch    uint64
	Keys     InteractionKeys
	Model    string
	Latency  time.Duration
	ErrClass InferenceErrClass
}

// ExecutionDispatch records the dispatch of one proactive turn BEFORE it
// starts. The completion observation follows when the turn ends.
type ExecutionDispatch struct {
	Epoch           uint64
	Keys            InteractionKeys
	GenerationModel string
	Action          InteractionAction
}

// ExecutionObservation records one completed proactive turn. The three
// effect flags are per-call mutually exclusive at the tool level but MAY
// co-occur at the turn level (a Reply turn with a successful reaction AND
// another side-effecting tool sets both).
type ExecutionObservation struct {
	Epoch                     uint64
	Keys                      InteractionKeys
	GenerationModel           string
	Action                    InteractionAction
	ReactionConfirmed         bool
	TextConfirmed             bool
	OtherSideEffectsConfirmed bool
	Failure                   ExecFailure
	Duration                  time.Duration
}

// PreflightObservation records one pre-engine skip for a chat.
type PreflightObservation struct {
	Epoch  uint64
	ChatID int64
	Kind   PreflightKind
}

// chatKey resolves a chat to its tracked identity or the overflow key.
type chatKey struct {
	id       int64
	overflow bool
}

// modelKey resolves a model ID to its tracked identity, the overflow key,
// or (for oversized identifiers) the overflow key WITHOUT truncation.
type modelKey struct {
	name     string
	overflow bool
}

// decisionBucketKey identifies one decision bucket.
type decisionBucketKey struct {
	chat    chatKey
	profile string
	model   modelKey
}

// executionBucketKey identifies one execution bucket. BOTH model dimensions
// are retained so execution outcomes stay filterable by decision model.
type executionBucketKey struct {
	chat            chatKey
	profile         string
	decisionModel   modelKey
	generationModel modelKey
}

// interactionState is the collector's interaction dataset, guarded by the
// collector mutex. The reset generation (epoch) and the cumulative reset
// count live on the Collector itself: they survive resets by definition.
type interactionState struct {
	periodStartedAt time.Time

	trackedChats     map[int64]struct{}
	decisionModels   map[string]struct{}
	generationModels map[string]struct{}

	submission       SubmissionTotals
	submissionByChat map[chatKey]*SubmissionTotals

	preflightTotal  [preflightKindCount]uint64
	preflightByChat map[chatKey]*[preflightKindCount]uint64

	decision        decisionTotalsState
	decisionBuckets map[decisionBucketKey]*decisionBucketState

	execution        executionTotalsState
	executionBuckets map[executionBucketKey]*executionBucketState

	// oversizedIDFolds counts observations folded into a model overflow key
	// because the identifier exceeded maxModelIDBytes (either dimension).
	// Distinct from the cardinality-cap folds, and disclosed by the exports.
	oversizedIDFolds uint64
}

type scoreHistograms [scoreDimCount]scoreHistogram

type decisionTotalsState struct {
	evaluatedAttempts    uint64
	pretransportRejected uint64
	outcomes             [outcomeCount]uint64
	inferenceClasses     [inferenceClassCount]uint64
	scores               scoreHistograms
	decisionLatency      latencyHistogram
	inferenceLatency     latencyHistogram
}

type decisionBucketState struct {
	outcomes         [outcomeCount]uint64
	inferenceClasses [inferenceClassCount]uint64
	scores           scoreHistograms
	decisionLatency  latencyHistogram
	inferenceLatency latencyHistogram
}

type executionCountersState struct {
	dispatched         [actionCount]uint64
	reactions          uint64
	texts              uint64
	otherSideEffects   uint64
	turnsWithEffect    uint64
	turnsWithoutEffect uint64
	turnsFailed        uint64
	failures           [execFailureCount - 1]uint64 // indexed by ExecFailure-1 (None excluded)
}

type executionTotalsState struct {
	counters executionCountersState
	duration latencyHistogram
}

type executionBucketState struct {
	counters executionCountersState
	duration latencyHistogram
}

// initInteraction prepares the interaction section of the collector; the
// observation period starts at the given instant.
func initInteraction(started time.Time) interactionState {
	return interactionState{
		periodStartedAt:  started,
		trackedChats:     map[int64]struct{}{},
		decisionModels:   map[string]struct{}{},
		generationModels: map[string]struct{}{},
		submissionByChat: map[chatKey]*SubmissionTotals{},
		preflightByChat:  map[chatKey]*[preflightKindCount]uint64{},
		decisionBuckets:  map[decisionBucketKey]*decisionBucketState{},
		executionBuckets: map[executionBucketKey]*executionBucketState{},
		decision: decisionTotalsState{
			decisionLatency:  newLatencyHistogram(latencyEdges),
			inferenceLatency: newLatencyHistogram(latencyEdges),
		},
		execution: executionTotalsState{
			duration: newLatencyHistogram(executionLatencyEdges),
		},
	}
}

// resetInteraction is the epoch-based reset: it increments the generation,
// restarts the observation period, and clears the whole interaction dataset.
// In-flight old-epoch operations contribute NOTHING to the new period —
// their observations are dropped by the epoch check, never staged.
func (c *Collector) resetInteraction() {
	c.interaction = initInteraction(c.now())
	c.interactionGeneration++
	c.interactionResets++
}

// BeginSubmission records one submission and returns the operation's epoch
// atomically: the counter increment and the epoch capture share one lock
// acquisition, so a reset can never fall between them. The epoch propagates
// through the queued task into every observation of the operation. Between
// BeginSubmission and Submit's return the submission is provisional
// (counted in Submitted, in neither acceptance ledger) — the runner may
// launch the task goroutine before Submit returns, so the ledger is DEFINED
// to include such unresolved submissions.
func (c *Collector) BeginSubmission(chatID int64) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.interaction.submission.Submitted++
	c.submissionCountLocked(chatID).Submitted++

	return c.interactionGeneration
}

// RecordSubmitRejected records a closed-runner rejection: Submit returned
// false without launching the task.
func (c *Collector) RecordSubmitRejected(epoch uint64, chatID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.liveEpochLocked(epoch) {
		return
	}
	c.interaction.submission.SubmitRejected++
	c.submissionCountLocked(chatID).SubmitRejected++
}

// RecordAcceptedCancelled records an accepted task discarded before its
// callback ran (cancelled while parked on the chat slot, or at the
// post-acquisition re-check). It is a ledger class of its own: never
// SubmitRejected (the task was accepted) and never a backend failure
// (nothing ran).
func (c *Collector) RecordAcceptedCancelled(epoch uint64, chatID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.liveEpochLocked(epoch) {
		return
	}
	c.interaction.submission.AcceptedThenCancelled++
	c.submissionCountLocked(chatID).AcceptedThenCancelled++
}

// RecordPreflight records one pre-engine skip.
func (c *Collector) RecordPreflight(obs PreflightObservation) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.liveEpochLocked(obs.Epoch) {
		return
	}
	if obs.Kind >= preflightKindCount {
		return
	}
	c.interaction.preflightTotal[obs.Kind]++
	c.preflightByChatLocked(obs.ChatID)[obs.Kind]++
}

// RecordDecision records one terminal decision-phase classification.
func (c *Collector) RecordDecision(obs DecisionObservation) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.liveEpochLocked(obs.Epoch) {
		return
	}
	if obs.Outcome >= outcomeCount {
		return
	}

	keys := obs.Keys.normalize()
	bucket := c.decisionBucketLocked(keys)
	bucket.outcomes[obs.Outcome]++
	c.interaction.decision.outcomes[obs.Outcome]++
	if obs.Scores != nil {
		for dim := ScoreDim(0); dim < scoreDimCount; dim++ {
			value := obs.Scores.dim(dim)
			bucket.scores[dim].record(value)
			c.interaction.decision.scores[dim].record(value)
		}
	}
	ms := obs.DecisionLatency.Seconds() * 1000
	bucket.decisionLatency.record(ms)
	c.interaction.decision.decisionLatency.record(ms)
}

// RecordInference records one Client.Evaluate invocation (attempt).
func (c *Collector) RecordInference(obs InferenceAttempt) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.liveEpochLocked(obs.Epoch) {
		return
	}
	if obs.ErrClass >= inferenceClassCount {
		return
	}

	keys := obs.Keys.normalize()
	bucket := c.decisionBucketLocked(keys)
	bucket.inferenceClasses[obs.ErrClass]++
	c.interaction.decision.inferenceClasses[obs.ErrClass]++
	c.interaction.decision.evaluatedAttempts++
	if obs.ErrClass == InferencePretransportRejected {
		c.interaction.decision.pretransportRejected++
	}
	ms := obs.Latency.Seconds() * 1000
	bucket.inferenceLatency.record(ms)
	c.interaction.decision.inferenceLatency.record(ms)
}

// RecordExecutionDispatch records one dispatched proactive turn (before the
// turn starts; the completion observation follows).
func (c *Collector) RecordExecutionDispatch(obs ExecutionDispatch) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.liveEpochLocked(obs.Epoch) {
		return
	}
	if obs.Action >= actionCount {
		return
	}

	bucket := c.executionBucketLocked(obs.Keys.normalize(), obs.GenerationModel)
	bucket.counters.dispatched[obs.Action]++
	c.interaction.execution.counters.dispatched[obs.Action]++
}

// RecordExecution records one completed proactive turn.
func (c *Collector) RecordExecution(obs ExecutionObservation) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.liveEpochLocked(obs.Epoch) {
		return
	}
	if obs.Action >= actionCount || obs.Failure >= execFailureCount {
		return
	}

	bucket := c.executionBucketLocked(obs.Keys.normalize(), obs.GenerationModel)
	counters := &bucket.counters
	if obs.ReactionConfirmed {
		counters.reactions++
	}
	if obs.TextConfirmed {
		counters.texts++
	}
	if obs.OtherSideEffectsConfirmed {
		counters.otherSideEffects++
	}
	hasEffect := obs.ReactionConfirmed || obs.TextConfirmed || obs.OtherSideEffectsConfirmed
	if hasEffect {
		counters.turnsWithEffect++
	}
	if obs.Failure != ExecFailureNone {
		counters.turnsFailed++
		counters.failures[obs.Failure-1]++
	} else if !hasEffect {
		counters.turnsWithoutEffect++
	}

	ms := obs.Duration.Seconds() * 1000
	bucket.duration.record(ms)

	// Mirror into the global totals.
	global := &c.interaction.execution.counters
	if obs.ReactionConfirmed {
		global.reactions++
	}
	if obs.TextConfirmed {
		global.texts++
	}
	if obs.OtherSideEffectsConfirmed {
		global.otherSideEffects++
	}
	if hasEffect {
		global.turnsWithEffect++
	}
	if obs.Failure != ExecFailureNone {
		global.turnsFailed++
		global.failures[obs.Failure-1]++
	} else if !hasEffect {
		global.turnsWithoutEffect++
	}
	c.interaction.execution.duration.record(ms)
}

// ResetInteraction resets the interaction dataset (global scope only; a
// filtered admin view resets the global dataset). General counters are
// never touched.
func (c *Collector) ResetInteraction() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.resetInteraction()
}

// liveEpochLocked reports whether an observation captured at the given
// epoch belongs to the current period. Old-epoch observations are silently
// dropped (reset consistency).
func (c *Collector) liveEpochLocked(epoch uint64) bool {
	return epoch == c.interactionGeneration
}

// submissionCountLocked resolves the per-chat submission counters.
func (c *Collector) submissionCountLocked(chatID int64) *SubmissionTotals {
	key := c.resolveChatLocked(chatID)
	counts, ok := c.interaction.submissionByChat[key]
	if !ok {
		counts = &SubmissionTotals{}
		c.interaction.submissionByChat[key] = counts
	}

	return counts
}

// preflightByChatLocked resolves the per-chat preflight counters.
func (c *Collector) preflightByChatLocked(chatID int64) *[preflightKindCount]uint64 {
	key := c.resolveChatLocked(chatID)
	counts, ok := c.interaction.preflightByChat[key]
	if !ok {
		counts = &[preflightKindCount]uint64{}
		c.interaction.preflightByChat[key] = counts
	}

	return counts
}

// decisionBucketLocked resolves (creates) the decision bucket for keys.
func (c *Collector) decisionBucketLocked(keys InteractionKeys) *decisionBucketState {
	key := decisionBucketKey{
		chat:    c.resolveChatLocked(keys.ChatID),
		profile: keys.Profile,
		model:   c.resolveModelLocked(c.interaction.decisionModels, keys.DecisionModel),
	}
	bucket, ok := c.interaction.decisionBuckets[key]
	if !ok {
		bucket = &decisionBucketState{
			decisionLatency:  newLatencyHistogram(latencyEdges),
			inferenceLatency: newLatencyHistogram(latencyEdges),
		}
		c.interaction.decisionBuckets[key] = bucket
	}

	return bucket
}

// executionBucketLocked resolves (creates) the execution bucket for keys
// plus the effective generation model.
func (c *Collector) executionBucketLocked(keys InteractionKeys, generationModel string) *executionBucketState {
	key := executionBucketKey{
		chat:            c.resolveChatLocked(keys.ChatID),
		profile:         keys.Profile,
		decisionModel:   c.resolveModelLocked(c.interaction.decisionModels, keys.DecisionModel),
		generationModel: c.resolveModelLocked(c.interaction.generationModels, generationModel),
	}
	bucket, ok := c.interaction.executionBuckets[key]
	if !ok {
		bucket = &executionBucketState{
			duration: newLatencyHistogram(executionLatencyEdges),
		}
		c.interaction.executionBuckets[key] = bucket
	}

	return bucket
}

// resolveChatLocked maps a chat ID to its tracked key, enforcing the shared
// hard cap across all per-chat maps. The cap counts distinct chats ever
// seen; further chats fold into the aggregate overflow chat key.
func (c *Collector) resolveChatLocked(chatID int64) chatKey {
	if _, ok := c.interaction.trackedChats[chatID]; ok {
		return chatKey{id: chatID}
	}
	if len(c.interaction.trackedChats) < MaxTrackedChats {
		c.interaction.trackedChats[chatID] = struct{}{}

		return chatKey{id: chatID}
	}

	return chatKey{overflow: true}
}

// resolveModelLocked maps a model ID to its tracked key within one
// dimension's cap. Oversized identifiers fold into the overflow key
// WITHOUT truncation — distinct IDs sharing a prefix would silently merge.
func (c *Collector) resolveModelLocked(set map[string]struct{}, name string) modelKey {
	if len(name) > maxModelIDBytes {
		c.interaction.oversizedIDFolds++

		return modelKey{overflow: true}
	}
	if _, ok := set[name]; ok {
		return modelKey{name: name}
	}
	if len(set) < MaxTrackedModels {
		set[name] = struct{}{}

		return modelKey{name: name}
	}

	return modelKey{overflow: true}
}
