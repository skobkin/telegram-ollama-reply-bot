package statistics

import "sort"

// InteractionSnapshot builds the immutable interaction view for one scope
// and filter combination, deep-copying under the collector lock.
//
// Denominator semantics:
//   - The unfiltered global view reports the maintained global totals,
//     which stay EXACT even under overflow folding (never bucket sums).
//   - Any narrowed view (chat scope or profile/model filter) derives its
//     totals from the selected buckets; exact global totals never imply
//     exact filtered totals when overflow folding is active — the snapshot
//     marks FilteredTotalsExact=false in that case.
//   - The chat-keyed Submission and Preflight sections are OMITTED under
//     profile/model filters, never misleadingly whole. Chat scope keeps
//     everything.
func (c *Collector) InteractionSnapshot(scope InteractionScope, filters InteractionFilters) InteractionSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	snapshot := InteractionSnapshot{
		GeneratedAt:   c.now(),
		Mode:          ModeLive,
		RubricVersion: c.rubricVersion,
		Period: InteractionPeriod{
			StartedAt: c.interaction.periodStartedAt,
			Resets:    c.interactionResets,
		},
		FilteredTotalsExact: true,
		OversizedIDFolds:    c.interaction.oversizedIDFolds,
	}

	filtered := filters.Narrowed() || scope.ChatSpecified
	overflowActive := c.overflowActiveLocked()

	if filters.Narrowed() {
		// Chat-keyed denominators are omitted under profile/model filters.
		snapshot.Submission = nil
		snapshot.Preflight = nil
	} else {
		snapshot.Submission = c.submissionSectionLocked(scope)
		snapshot.Preflight = c.preflightSectionLocked(scope)
	}

	snapshot.Decision = c.decisionSectionLocked(scope, filters, filtered)
	snapshot.Execution = c.executionSectionLocked(scope, filters, filtered)

	if filtered && overflowActive {
		snapshot.FilteredTotalsExact = false
		if c.selectionUntrackedLocked(scope, filters) {
			snapshot.UntrackedSelection = "no tracked observations match this selection; activity may be folded into the overflow buckets"
		}
	}

	return snapshot
}

// selectionUntrackedLocked reports when a narrowed selection targets
// nothing the collector tracked while overflow folding is active: bare
// zeros would misrepresent folded-away activity.
func (c *Collector) selectionUntrackedLocked(scope InteractionScope, filters InteractionFilters) bool {
	if scope.ChatSpecified {
		if _, tracked := c.interaction.trackedChats[scope.ChatID]; !tracked {
			return true
		}
	}
	if filters.DecisionModel != "" {
		if _, tracked := c.interaction.decisionModels[filters.DecisionModel]; !tracked {
			return true
		}
	}

	return false
}

// overflowActiveLocked reports whether any overflow bucket carries
// observations (folding actually happened).
func (c *Collector) overflowActiveLocked() bool {
	for key, bucket := range c.interaction.decisionBuckets {
		if key.chat.overflow && bucket.nonEmpty() {
			return true
		}
		if key.model.overflow && bucket.nonEmpty() {
			return true
		}
	}
	for key, bucket := range c.interaction.executionBuckets {
		if (key.chat.overflow || key.decisionModel.overflow || key.generationModel.overflow) && bucket.nonEmpty() {
			return true
		}
	}
	for key, counts := range c.interaction.preflightByChat {
		if key.overflow && preflightSum(counts) > 0 {
			return true
		}
	}
	for key, counts := range c.interaction.submissionByChat {
		if key.overflow && *counts != (SubmissionTotals{}) {
			return true
		}
	}

	return false
}

// submissionSectionLocked builds the submission ledger for the scope.
func (c *Collector) submissionSectionLocked(scope InteractionScope) *SubmissionTotals {
	if scope.ChatSpecified {
		counts := c.interaction.submissionByChat[chatKey{id: scope.ChatID}]
		if counts == nil {
			zero := SubmissionTotals{}

			return &zero
		}
		totals := *counts

		return &totals
	}

	total := c.interaction.submission

	return &total
}

// preflightSectionLocked builds the preflight section for the scope.
func (c *Collector) preflightSectionLocked(scope InteractionScope) *PreflightSection {
	section := &PreflightSection{}
	if scope.ChatSpecified {
		if counts, ok := c.interaction.preflightByChat[chatKey{id: scope.ChatID}]; ok {
			section.Totals = preflightTotalsOf(counts)
		}

		return section
	}

	for kind := PreflightKind(0); kind < preflightKindCount; kind++ {
		switch kind {
		case PreflightAccessDenied:
			section.Totals.AccessDenied = c.interaction.preflightTotal[kind]
		case PreflightConfigResolutionFailed:
			section.Totals.ConfigResolutionFailed = c.interaction.preflightTotal[kind]
		case PreflightEligibilityLost:
			section.Totals.EligibilityLost = c.interaction.preflightTotal[kind]
		case PreflightPersonaCaptureFailed:
			section.Totals.PersonaCaptureFailed = c.interaction.preflightTotal[kind]
		case PreflightEngineWiringFailed:
			section.Totals.EngineWiringFailed = c.interaction.preflightTotal[kind]
		}
	}

	chats := make([]PreflightChatBucket, 0, len(c.interaction.preflightByChat))
	for key, counts := range c.interaction.preflightByChat {
		chats = append(chats, PreflightChatBucket{
			ChatID:       chatIDPointer(key),
			ChatOverflow: key.overflow,
			Counts:       preflightTotalsOf(counts),
		})
	}
	sortPreflightChats(chats)
	section.Chats = chats

	return section
}

// decisionSectionLocked builds the decision section for the scope/filters.
// The unfiltered global view uses the maintained exact totals; any narrowed
// view sums the selected buckets.
func (c *Collector) decisionSectionLocked(scope InteractionScope, filters InteractionFilters, filtered bool) DecisionSection {
	section := DecisionSection{}

	buckets := make([]DecisionBucket, 0, len(c.interaction.decisionBuckets))
	sum := newDecisionAccumulator()
	for key, state := range c.interaction.decisionBuckets {
		if !decisionBucketSelected(key, scope, filters) {
			continue
		}
		buckets = append(buckets, decisionBucketOf(key, state))
		if filtered {
			sum.add(state)
		}
	}
	sortDecisionBuckets(buckets)
	section.Buckets = buckets

	if filtered {
		section.Totals = sum.totals()
	} else {
		section.Totals = c.decisionTotalsLocked()
	}

	return section
}

// executionSectionLocked builds the execution section for the scope/filters.
func (c *Collector) executionSectionLocked(scope InteractionScope, filters InteractionFilters, filtered bool) ExecutionSection {
	section := ExecutionSection{}

	buckets := make([]ExecutionBucket, 0, len(c.interaction.executionBuckets))
	sum := newExecutionAccumulator()
	for key, state := range c.interaction.executionBuckets {
		if !executionBucketSelected(key, scope, filters) {
			continue
		}
		buckets = append(buckets, executionBucketOf(key, state))
		if filtered {
			sum.add(state)
		}
	}
	sortExecutionBuckets(buckets)
	section.Buckets = buckets

	if filtered {
		section.Totals = sum.totals()
	} else {
		section.Totals = c.executionTotalsLocked()
	}

	return section
}

// decisionBucketSelected applies scope and filter predicates. Model filters
// never match overflow buckets (attribution unknown, disclosed).
func decisionBucketSelected(key decisionBucketKey, scope InteractionScope, filters InteractionFilters) bool {
	if scope.ChatSpecified && (key.chat.overflow || key.chat.id != scope.ChatID) {
		return false
	}
	if filters.Profile != "" && key.profile != filters.Profile {
		return false
	}
	if filters.DecisionModel != "" && (key.model.overflow || key.model.name != filters.DecisionModel) {
		return false
	}

	return true
}

// executionBucketSelected applies scope and filter predicates to an
// execution bucket.
func executionBucketSelected(key executionBucketKey, scope InteractionScope, filters InteractionFilters) bool {
	if scope.ChatSpecified && (key.chat.overflow || key.chat.id != scope.ChatID) {
		return false
	}
	if filters.Profile != "" && key.profile != filters.Profile {
		return false
	}
	if filters.DecisionModel != "" && (key.decisionModel.overflow || key.decisionModel.name != filters.DecisionModel) {
		return false
	}

	return true
}

// decisionAccumulator sums selected decision buckets into filtered totals.
type decisionAccumulator struct {
	totalsState decisionTotalsState
}

func newDecisionAccumulator() *decisionAccumulator {
	return &decisionAccumulator{totalsState: decisionTotalsState{
		decisionLatency:  newLatencyHistogram(latencyEdges),
		inferenceLatency: newLatencyHistogram(latencyEdges),
	}}
}

func (a *decisionAccumulator) add(state *decisionBucketState) {
	for i := range state.outcomes {
		a.totalsState.outcomes[i] += state.outcomes[i]
	}
	for i := range state.inferenceClasses {
		a.totalsState.inferenceClasses[i] += state.inferenceClasses[i]
	}
	a.totalsState.evaluatedAttempts += state.inferenceClasses[InferenceSuccess]
	a.totalsState.pretransportRejected += state.inferenceClasses[InferencePretransportRejected]
	for dim := ScoreDim(0); dim < scoreDimCount; dim++ {
		a.totalsState.scores[dim].add(state.scores[dim])
	}
	a.totalsState.decisionLatency.add(state.decisionLatency)
	a.totalsState.inferenceLatency.add(state.inferenceLatency)
}

func (a *decisionAccumulator) totals() DecisionTotals {
	totals := DecisionTotals{
		EvaluatedAttempts:    a.totalsState.evaluatedAttempts,
		PretransportRejected: a.totalsState.pretransportRejected,
		Outcomes:             outcomeTotals(a.totalsState.outcomes),
		InferenceErrors:      inferenceClassTotals(a.totalsState.inferenceClasses),
		ScoreHistograms:      scoreHistogramsOf(a.totalsState.scores),
		Latency:              buildLatencySection(a.totalsState.decisionLatency, a.totalsState.inferenceLatency),
	}

	return totals
}

// buildLatencySection assembles the decision/inference latency section
// from the given histograms.
func buildLatencySection(decision, inference latencyHistogram) LatencySection {
	section := LatencySection{
		Decision:  latencySnapshot(decision),
		Inference: latencySnapshot(inference),
	}
	section.Percentiles.Decision = percentilesOf(decision)
	section.Percentiles.Inference = percentilesOf(inference)

	return section
}

// executionAccumulator sums selected execution buckets into filtered totals.
type executionAccumulator struct {
	counters executionCountersState
	duration latencyHistogram
}

func newExecutionAccumulator() *executionAccumulator {
	return &executionAccumulator{duration: newLatencyHistogram(executionLatencyEdges)}
}

func (a *executionAccumulator) add(state *executionBucketState) {
	a.counters.add(state.counters)
	a.duration.add(state.duration)
}

func (a *executionAccumulator) totals() ExecutionTotals {
	totals := ExecutionTotals{
		ExecutionCounters: executionCountersOf(a.counters),
		Duration:          latencySnapshot(a.duration),
		Percentiles:       percentilesOf(a.duration),
	}

	return totals
}

// decisionTotalsLocked is the maintained exact global decision totals.
func (c *Collector) decisionTotalsLocked() DecisionTotals {
	return DecisionTotals{
		EvaluatedAttempts:    c.interaction.decision.evaluatedAttempts,
		PretransportRejected: c.interaction.decision.pretransportRejected,
		Outcomes:             outcomeTotals(c.interaction.decision.outcomes),
		InferenceErrors:      inferenceClassTotals(c.interaction.decision.inferenceClasses),
		ScoreHistograms:      scoreHistogramsOf(c.interaction.decision.scores),
		Latency:              buildLatencySection(c.interaction.decision.decisionLatency, c.interaction.decision.inferenceLatency),
	}
}

// executionTotalsLocked is the maintained exact global execution totals.
func (c *Collector) executionTotalsLocked() ExecutionTotals {
	return ExecutionTotals{
		ExecutionCounters: executionCountersOf(c.interaction.execution.counters),
		Duration:          latencySnapshot(c.interaction.execution.duration),
		Percentiles:       percentilesOf(c.interaction.execution.duration),
	}
}

// --- conversion helpers (collector state → snapshot values) ---

func preflightSum(counts *[preflightKindCount]uint64) uint64 {
	var sum uint64
	for _, v := range counts {
		sum += v
	}

	return sum
}

func preflightTotalsOf(counts *[preflightKindCount]uint64) PreflightTotals {
	return PreflightTotals{
		AccessDenied:           counts[PreflightAccessDenied],
		ConfigResolutionFailed: counts[PreflightConfigResolutionFailed],
		EligibilityLost:        counts[PreflightEligibilityLost],
		PersonaCaptureFailed:   counts[PreflightPersonaCaptureFailed],
		EngineWiringFailed:     counts[PreflightEngineWiringFailed],
	}
}

func inferenceClassTotals(classes [inferenceClassCount]uint64) InferenceErrorClassTotals {
	return InferenceErrorClassTotals{
		PretransportRejected: classes[InferencePretransportRejected],
		NotConfigured:        classes[InferenceNotConfigured],
		Timeout:              classes[InferenceTimeout],
		Cancelled:            classes[InferenceCancelled],
		Auth:                 classes[InferenceAuth],
		Unavailable:          classes[InferenceUnavailable],
		InvalidResponse:      classes[InferenceInvalidResponse],
	}
}

func scoreHistogramsOf(scores scoreHistograms) ScoreHistograms {
	scoreSnapshot := func(h scoreHistogram) HistogramSnapshot {
		counts := make([]uint64, len(h.counts))
		copy(counts, h.counts[:])

		return HistogramSnapshot{
			Edges:     append([]float64(nil), scoreEdges...),
			OpenEnded: false,
			Counts:    counts,
		}
	}

	return ScoreHistograms{
		Base:             scoreSnapshot(scores[ScoreDimBase]),
		Relevance:        scoreSnapshot(scores[ScoreDimRelevance]),
		Contribution:     scoreSnapshot(scores[ScoreDimContribution]),
		Intrusion:        scoreSnapshot(scores[ScoreDimIntrusion]),
		ReactSuitability: scoreSnapshot(scores[ScoreDimReactSuitability]),
		ReplySuitability: scoreSnapshot(scores[ScoreDimReplySuitability]),
	}
}

func latencySnapshot(h latencyHistogram) HistogramSnapshot {
	counts := make([]uint64, len(h.counts))
	copy(counts, h.counts)

	return HistogramSnapshot{
		Edges:     append([]float64(nil), h.edges...),
		OpenEnded: true,
		Counts:    counts,
	}
}

// percentilesOf computes the p50/p90/p99 approximations of one latency
// histogram (lower bounds for the open terminal bin; null when empty).
func percentilesOf(h latencyHistogram) LatencyPercentiles {
	percentile := func(p float64) *PercentileValue {
		value, lowerBound, ok := h.percentile(p)
		if !ok {
			return nil
		}
		ms := value
		if lowerBound {
			return &PercentileValue{AtLeastMs: &ms}
		}

		return &PercentileValue{ApproxMs: &ms}
	}

	return LatencyPercentiles{
		P50: percentile(0.50),
		P90: percentile(0.90),
		P99: percentile(0.99),
	}
}

func executionCountersOf(state executionCountersState) ExecutionCounters {
	return ExecutionCounters{
		DispatchedReact:    state.dispatched[ActionReact],
		DispatchedReply:    state.dispatched[ActionReply],
		ReactionsConfirmed: state.reactions,
		TextsConfirmed:     state.texts,
		OtherSideEffects:   state.otherSideEffects,
		TurnsWithEffect:    state.turnsWithEffect,
		TurnsWithoutEffect: state.turnsWithoutEffect,
		TurnsFailed:        state.turnsFailed,
		Failures: ExecutionFailureTotals{
			Timeout:         state.failures[ExecFailureTimeout-1],
			Cancelled:       state.failures[ExecFailureCancelled-1],
			Generation:      state.failures[ExecFailureGeneration-1],
			Delivery:        state.failures[ExecFailureDelivery-1],
			AbandonedTarget: state.failures[ExecFailureAbandonedTarget-1],
		},
	}
}

func (s *executionCountersState) add(other executionCountersState) {
	for i := range s.dispatched {
		s.dispatched[i] += other.dispatched[i]
	}
	s.reactions += other.reactions
	s.texts += other.texts
	s.otherSideEffects += other.otherSideEffects
	s.turnsWithEffect += other.turnsWithEffect
	s.turnsWithoutEffect += other.turnsWithoutEffect
	s.turnsFailed += other.turnsFailed
	for i := range s.failures {
		s.failures[i] += other.failures[i]
	}
}

func (s *decisionBucketState) nonEmpty() bool {
	for _, v := range s.outcomes {
		if v > 0 {
			return true
		}
	}
	for _, v := range s.inferenceClasses {
		if v > 0 {
			return true
		}
	}

	return false
}

func (s *executionBucketState) nonEmpty() bool {
	return s.counters.dispatched[ActionReact] > 0 || s.counters.dispatched[ActionReply] > 0 ||
		s.counters.reactions > 0 || s.counters.texts > 0 || s.counters.otherSideEffects > 0 ||
		s.counters.turnsWithEffect > 0 || s.counters.turnsWithoutEffect > 0 || s.counters.turnsFailed > 0
}

func chatIDPointer(key chatKey) *int64 {
	if key.overflow {
		return nil
	}
	id := key.id

	return &id
}

func modelPointer(key modelKey) *string {
	if key.overflow {
		return nil
	}
	name := key.name

	return &name
}

func decisionBucketOf(key decisionBucketKey, state *decisionBucketState) DecisionBucket {
	return DecisionBucket{
		ChatID:                chatIDPointer(key.chat),
		ChatOverflow:          key.chat.overflow,
		Profile:               key.profile,
		DecisionModel:         modelPointer(key.model),
		DecisionModelOverflow: key.model.overflow,
		Counters:              outcomeTotals(state.outcomes),
		Histograms:            scoreHistogramsOf(state.scores),
	}
}

func executionBucketOf(key executionBucketKey, state *executionBucketState) ExecutionBucket {
	return ExecutionBucket{
		ChatID:                  chatIDPointer(key.chat),
		ChatOverflow:            key.chat.overflow,
		Profile:                 key.profile,
		DecisionModel:           modelPointer(key.decisionModel),
		DecisionModelOverflow:   key.decisionModel.overflow,
		GenerationModel:         modelPointer(key.generationModel),
		GenerationModelOverflow: key.generationModel.overflow,
		Counters:                executionCountersOf(state.counters),
		Duration:                latencySnapshot(state.duration),
	}
}

// --- deterministic ordering (chat, profile, model; overflow last) ---

func overflowRank(overflow bool) int {
	if overflow {
		return 1
	}

	return 0
}

func sortPreflightChats(chats []PreflightChatBucket) {
	sort.Slice(chats, func(i, j int) bool {
		a, b := chats[i], chats[j]
		if a.ChatOverflow != b.ChatOverflow {
			return overflowRank(b.ChatOverflow) < overflowRank(a.ChatOverflow)
		}

		return *a.ChatID < *b.ChatID
	})
}

func sortDecisionBuckets(buckets []DecisionBucket) {
	sort.Slice(buckets, func(i, j int) bool {
		a, b := buckets[i], buckets[j]
		if a.ChatOverflow != b.ChatOverflow {
			return overflowRank(b.ChatOverflow) < overflowRank(a.ChatOverflow)
		}
		if !a.ChatOverflow && *a.ChatID != *b.ChatID {
			return *a.ChatID < *b.ChatID
		}
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		if a.DecisionModelOverflow != b.DecisionModelOverflow {
			return overflowRank(b.DecisionModelOverflow) < overflowRank(a.DecisionModelOverflow)
		}

		return modelSortName(a.DecisionModel) < modelSortName(b.DecisionModel)
	})
}

func sortExecutionBuckets(buckets []ExecutionBucket) {
	sort.Slice(buckets, func(i, j int) bool {
		a, b := buckets[i], buckets[j]
		if a.ChatOverflow != b.ChatOverflow {
			return overflowRank(b.ChatOverflow) < overflowRank(a.ChatOverflow)
		}
		if !a.ChatOverflow && *a.ChatID != *b.ChatID {
			return *a.ChatID < *b.ChatID
		}
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		if a.DecisionModelOverflow != b.DecisionModelOverflow {
			return overflowRank(b.DecisionModelOverflow) < overflowRank(a.DecisionModelOverflow)
		}
		if modelSortName(a.DecisionModel) != modelSortName(b.DecisionModel) {
			return modelSortName(a.DecisionModel) < modelSortName(b.DecisionModel)
		}
		if a.GenerationModelOverflow != b.GenerationModelOverflow {
			return overflowRank(b.GenerationModelOverflow) < overflowRank(a.GenerationModelOverflow)
		}

		return modelSortName(a.GenerationModel) < modelSortName(b.GenerationModel)
	})
}

func modelSortName(m *string) string {
	if m == nil {
		return ""
	}

	return *m
}
