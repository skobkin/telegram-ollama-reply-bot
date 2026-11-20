package statistics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"sort"
	"time"
)

const (
	// InteractionDatasetName is the storage key of the interaction dataset.
	InteractionDatasetName = "interaction"

	// interactionFormatVersion is the payload format of the interaction
	// dataset. Any incompatible payload change bumps it; an older build then
	// treats the row as a newer format and leaves it untouched.
	interactionFormatVersion = 1

	// overflowRef is the dictionary reference of an overflow key. Overflow
	// is never encoded as a label.
	overflowRef = -1
)

// interactionDataset adapts the collector's interaction group to the
// persister's Dataset interface.
type interactionDataset struct{ c *Collector }

var _ Dataset = interactionDataset{}

// InteractionDataset returns the persistable view of the interaction
// statistics. General statistics stay ephemeral.
func (c *Collector) InteractionDataset() Dataset { return interactionDataset{c: c} }

func (d interactionDataset) Name() string       { return InteractionDatasetName }
func (d interactionDataset) FormatVersion() int { return interactionFormatVersion }

// Capture copies the interaction state under the collector lock. The JSON
// encoding runs after the lock is released.
func (d interactionDataset) Capture() (PersistPair, func() ([]byte, error), bool) {
	d.c.mu.Lock()
	pair := d.c.interactionPersist.current
	if !d.c.interactionPersist.dirty() {
		d.c.mu.Unlock()

		return pair, nil, false
	}
	payload := d.c.interactionPayloadLocked()
	d.c.mu.Unlock()

	return pair, func() ([]byte, error) { return json.Marshal(payload) }, true
}

func (d interactionDataset) Acknowledge(pair PersistPair) {
	d.c.mu.Lock()
	defer d.c.mu.Unlock()
	d.c.interactionPersist.ack(pair)
}

func (d interactionDataset) SetPersistenceInfo(info PersistenceInfo) {
	d.c.mu.Lock()
	defer d.c.mu.Unlock()
	d.c.interactionPersist.info = info
}

// Restore decodes, validates, reconciles and installs a persisted payload
// atomically. Failures wrap ErrMalformedSnapshot and leave the live dataset
// untouched.
func (d interactionDataset) Restore(version int, pair PersistPair, payload []byte) error {
	if version != interactionFormatVersion {
		return fmt.Errorf("%w: unsupported interaction format version %d", ErrMalformedSnapshot, version)
	}
	state, resets, reconciled, err := decodeInteractionPayload(payload)
	if err != nil {
		return err
	}

	d.c.mu.Lock()
	defer d.c.mu.Unlock()
	d.c.interaction = state
	d.c.interactionResets = resets
	d.c.interactionPersist.install(pair)
	if reconciled {
		// Persist the reconciled interrupted counters at the next flush.
		d.c.interactionPersist.touch()
	}

	return nil
}

// --- payload types (format version 1) ---

type interactionPayload struct {
	PeriodStartedAt  time.Time `json:"period_started_at"`
	Resets           uint64    `json:"resets"`
	Chats            []int64   `json:"chats"`
	Profiles         []string  `json:"profiles"`
	DecisionModels   []string  `json:"decision_models"`
	GenerationModels []string  `json:"generation_models"`
	OversizedIDFolds uint64    `json:"oversized_id_folds"`

	Submission       SubmissionTotals           `json:"submission"`
	SubmissionByChat []submissionChatPayload    `json:"submission_by_chat"`
	Preflight        [preflightKindCount]uint64 `json:"preflight"`
	PreflightByChat  []preflightChatPayload     `json:"preflight_by_chat"`

	Decision         decisionPayload          `json:"decision"`
	DecisionBuckets  []decisionBucketPayload  `json:"decision_buckets"`
	Execution        executionData            `json:"execution"`
	ExecutionBuckets []executionBucketPayload `json:"execution_buckets"`
}

type submissionChatPayload struct {
	Chat   int              `json:"chat"`
	Totals SubmissionTotals `json:"totals"`
}

type preflightChatPayload struct {
	Chat   int                        `json:"chat"`
	Counts [preflightKindCount]uint64 `json:"counts"`
}

type decisionData struct {
	Outcomes         [outcomeCount]uint64                 `json:"outcomes"`
	InferenceClasses [inferenceClassCount]uint64          `json:"inference_classes"`
	Scores           [scoreDimCount][scoreBinCount]uint64 `json:"scores"`
	DecisionLatency  []uint64                             `json:"decision_latency"`
	InferenceLatency []uint64                             `json:"inference_latency"`
}

type decisionPayload struct {
	EvaluatedAttempts    uint64 `json:"evaluated_attempts"`
	PretransportRejected uint64 `json:"pretransport_rejected"`
	decisionData
}

type decisionBucketPayload struct {
	Chat    int `json:"chat"`
	Profile int `json:"profile"`
	Model   int `json:"model"`
	decisionData
}

type executionData struct {
	Dispatched         [actionCount]uint64          `json:"dispatched"`
	Reactions          uint64                       `json:"reactions"`
	Texts              uint64                       `json:"texts"`
	OtherSideEffects   uint64                       `json:"other_side_effects"`
	TurnsWithEffect    uint64                       `json:"turns_with_effect"`
	TurnsWithoutEffect uint64                       `json:"turns_without_effect"`
	TurnsFailed        uint64                       `json:"turns_failed"`
	TurnsInterrupted   uint64                       `json:"turns_interrupted"`
	Failures           [execFailureCount - 1]uint64 `json:"failures"`
	Duration           []uint64                     `json:"duration"`
}

type executionBucketPayload struct {
	Chat            int `json:"chat"`
	Profile         int `json:"profile"`
	DecisionModel   int `json:"decision_model"`
	GenerationModel int `json:"generation_model"`
	executionData
}

// --- encode (state → payload, under the collector lock) ---

// interactionPayloadLocked deep-copies the interaction state into the
// payload with deterministic ordering. The caller holds c.mu.
func (c *Collector) interactionPayloadLocked() interactionPayload {
	in := &c.interaction

	chats := make([]int64, 0, len(in.trackedChats))
	for id := range in.trackedChats {
		chats = append(chats, id)
	}
	sort.Slice(chats, func(i, j int) bool { return chats[i] < chats[j] })
	chatIdx := make(map[int64]int, len(chats))
	for i, id := range chats {
		chatIdx[id] = i
	}
	profiles, profileIdx := sortedDictionary(in.profiles)
	decisionModels, decisionIdx := sortedDictionary(in.decisionModels)
	generationModels, generationIdx := sortedDictionary(in.generationModels)

	chatRef := func(k chatKey) int {
		if k.overflow {
			return overflowRef
		}

		return chatIdx[k.id]
	}
	profileRef := func(k profileKey) int {
		if k.overflow {
			return overflowRef
		}

		return profileIdx[k.name]
	}
	modelRef := func(idx map[string]int, k modelKey) int {
		if k.overflow {
			return overflowRef
		}

		return idx[k.name]
	}

	payload := interactionPayload{
		PeriodStartedAt:  in.periodStartedAt,
		Resets:           c.interactionResets,
		Chats:            chats,
		Profiles:         profiles,
		DecisionModels:   decisionModels,
		GenerationModels: generationModels,
		OversizedIDFolds: in.oversizedIDFolds,
		Submission:       in.submission,
		Preflight:        in.preflightTotal,
		Decision: decisionPayload{
			EvaluatedAttempts:    in.decision.evaluatedAttempts,
			PretransportRejected: in.decision.pretransportRejected,
			decisionData: decisionDataOf(in.decision.outcomes, in.decision.inferenceClasses, in.decision.scores,
				in.decision.decisionLatency, in.decision.inferenceLatency),
		},
		Execution: executionDataOf(in.execution.counters, in.execution.duration),
	}

	for key, counts := range in.submissionByChat {
		payload.SubmissionByChat = append(payload.SubmissionByChat, submissionChatPayload{Chat: chatRef(key), Totals: *counts})
	}
	sort.Slice(payload.SubmissionByChat, func(i, j int) bool { return payload.SubmissionByChat[i].Chat < payload.SubmissionByChat[j].Chat })
	for key, counts := range in.preflightByChat {
		payload.PreflightByChat = append(payload.PreflightByChat, preflightChatPayload{Chat: chatRef(key), Counts: *counts})
	}
	sort.Slice(payload.PreflightByChat, func(i, j int) bool { return payload.PreflightByChat[i].Chat < payload.PreflightByChat[j].Chat })

	for key, b := range in.decisionBuckets {
		payload.DecisionBuckets = append(payload.DecisionBuckets, decisionBucketPayload{
			Chat: chatRef(key.chat), Profile: profileRef(key.profile), Model: modelRef(decisionIdx, key.model),
			decisionData: decisionDataOf(b.outcomes, b.inferenceClasses, b.scores, b.decisionLatency, b.inferenceLatency),
		})
	}
	sort.Slice(payload.DecisionBuckets, func(i, j int) bool {
		a, b := payload.DecisionBuckets[i], payload.DecisionBuckets[j]
		if a.Chat != b.Chat {
			return a.Chat < b.Chat
		}
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}

		return a.Model < b.Model
	})
	for key, b := range in.executionBuckets {
		payload.ExecutionBuckets = append(payload.ExecutionBuckets, executionBucketPayload{
			Chat: chatRef(key.chat), Profile: profileRef(key.profile),
			DecisionModel:   modelRef(decisionIdx, key.decisionModel),
			GenerationModel: modelRef(generationIdx, key.generationModel),
			executionData:   executionDataOf(b.counters, b.duration),
		})
	}
	sort.Slice(payload.ExecutionBuckets, func(i, j int) bool {
		a, b := payload.ExecutionBuckets[i], payload.ExecutionBuckets[j]
		if a.Chat != b.Chat {
			return a.Chat < b.Chat
		}
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		if a.DecisionModel != b.DecisionModel {
			return a.DecisionModel < b.DecisionModel
		}

		return a.GenerationModel < b.GenerationModel
	})

	return payload
}

func sortedDictionary(set map[string]struct{}) ([]string, map[string]int) {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	idx := make(map[string]int, len(names))
	for i, name := range names {
		idx[name] = i
	}

	return names, idx
}

func decisionDataOf(outcomes [outcomeCount]uint64, classes [inferenceClassCount]uint64, scores scoreHistograms, decision, inference latencyHistogram) decisionData {
	d := decisionData{
		Outcomes:         outcomes,
		InferenceClasses: classes,
		DecisionLatency:  append([]uint64(nil), decision.counts...),
		InferenceLatency: append([]uint64(nil), inference.counts...),
	}
	for dim := range scores {
		d.Scores[dim] = scores[dim].counts
	}

	return d
}

func executionDataOf(c executionCountersState, duration latencyHistogram) executionData {
	return executionData{
		Dispatched:         c.dispatched,
		Reactions:          c.reactions,
		Texts:              c.texts,
		OtherSideEffects:   c.otherSideEffects,
		TurnsWithEffect:    c.turnsWithEffect,
		TurnsWithoutEffect: c.turnsWithoutEffect,
		TurnsFailed:        c.turnsFailed,
		TurnsInterrupted:   c.turnsInterrupted,
		Failures:           c.failures,
		Duration:           append([]uint64(nil), duration.counts...),
	}
}

// --- decode, validate, reconcile (payload → state) ---

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformedSnapshot, fmt.Sprintf(format, args...))
}

// checkedSum accumulates uint64 values and remembers an overflow.
type checkedSum struct {
	v        uint64
	overflow bool
}

func (s *checkedSum) add(x uint64) {
	sum, carry := bits.Add64(s.v, x, 0)
	if carry != 0 {
		s.overflow = true
	}
	s.v = sum
}

// subtract returns a-b, ok=false when it would underflow.
func subtract(a uint64, parts ...uint64) (uint64, bool) {
	for _, p := range parts {
		if p > a {
			return 0, false
		}
		a -= p
	}

	return a, true
}

func sliceTotal(values []uint64) (uint64, bool) {
	var s checkedSum
	for _, v := range values {
		s.add(v)
	}

	return s.v, !s.overflow
}

func decodeInteractionPayload(raw []byte) (state interactionState, resets uint64, reconciled bool, err error) {
	var p interactionPayload
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return interactionState{}, 0, false, malformed("decode: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return interactionState{}, 0, false, malformed("trailing data after the payload")
	}

	v := interactionValidator{p: &p}
	state, err = v.build()
	if err != nil {
		return interactionState{}, 0, false, err
	}
	reconciled, err = v.reconcile(&state)
	if err != nil {
		return interactionState{}, 0, false, err
	}

	return state, p.Resets, reconciled, nil
}

type interactionValidator struct {
	p *interactionPayload
}

func (v *interactionValidator) chatKeyOf(ref int) (chatKey, error) {
	switch {
	case ref == overflowRef:
		return chatKey{overflow: true}, nil
	case ref >= 0 && ref < len(v.p.Chats):
		return chatKey{id: v.p.Chats[ref]}, nil
	}

	return chatKey{}, malformed("chat reference %d is out of range", ref)
}

func (v *interactionValidator) profileKeyOf(ref int) (profileKey, error) {
	switch {
	case ref == overflowRef:
		return profileKey{overflow: true}, nil
	case ref >= 0 && ref < len(v.p.Profiles):
		return profileKey{name: v.p.Profiles[ref]}, nil
	}

	return profileKey{}, malformed("profile reference %d is out of range", ref)
}

func modelKeyOf(dict []string, ref int) (modelKey, error) {
	switch {
	case ref == overflowRef:
		return modelKey{overflow: true}, nil
	case ref >= 0 && ref < len(dict):
		return modelKey{name: dict[ref]}, nil
	}

	return modelKey{}, malformed("model reference %d is out of range", ref)
}

func validateDictionary[T comparable](name string, values []T, limit int) error {
	if len(values) > limit {
		return malformed("%s dictionary has %d entries, the cap is %d", name, len(values), limit)
	}
	seen := make(map[T]struct{}, len(values))
	for _, value := range values {
		if _, dup := seen[value]; dup {
			return malformed("%s dictionary has a duplicate entry", name)
		}
		seen[value] = struct{}{}
	}

	return nil
}

func validateIDs(name string, ids []string, limit int) error {
	for _, id := range ids {
		if len(id) > limit {
			return malformed("%s identifier exceeds %d bytes", name, limit)
		}
	}

	return nil
}

func histogramFrom(name string, edges []float64, counts []uint64) (latencyHistogram, error) {
	if len(counts) != len(edges)+1 {
		return latencyHistogram{}, malformed("%s histogram has %d bins, expected %d", name, len(counts), len(edges)+1)
	}
	h := newLatencyHistogram(edges)
	copy(h.counts, counts)

	return h, nil
}

// build validates the payload structure and builds the interaction state,
// without reconciliation.
func (v *interactionValidator) build() (interactionState, error) {
	p := v.p
	if err := validateDictionary("chat", p.Chats, MaxTrackedChats); err != nil {
		return interactionState{}, err
	}
	if err := validateDictionary("profile", p.Profiles, MaxTrackedProfiles); err != nil {
		return interactionState{}, err
	}
	if err := validateDictionary("decision model", p.DecisionModels, MaxTrackedModels); err != nil {
		return interactionState{}, err
	}
	if err := validateDictionary("generation model", p.GenerationModels, MaxTrackedModels); err != nil {
		return interactionState{}, err
	}
	for _, check := range []error{
		validateIDs("profile", p.Profiles, maxProfileIDBytes),
		validateIDs("decision model", p.DecisionModels, maxModelIDBytes),
		validateIDs("generation model", p.GenerationModels, maxModelIDBytes),
	} {
		if check != nil {
			return interactionState{}, check
		}
	}

	st := initInteraction(p.PeriodStartedAt)
	for _, id := range p.Chats {
		st.trackedChats[id] = struct{}{}
	}
	for _, name := range p.Profiles {
		st.profiles[name] = struct{}{}
	}
	for _, name := range p.DecisionModels {
		st.decisionModels[name] = struct{}{}
	}
	for _, name := range p.GenerationModels {
		st.generationModels[name] = struct{}{}
	}
	st.oversizedIDFolds = p.OversizedIDFolds
	st.submission = p.Submission
	st.preflightTotal = p.Preflight

	// Per-chat ledgers.
	subSum := make([]checkedSum, 3)
	for _, entry := range p.SubmissionByChat {
		key, err := v.chatKeyOf(entry.Chat)
		if err != nil {
			return interactionState{}, err
		}
		if _, dup := st.submissionByChat[key]; dup {
			return interactionState{}, malformed("duplicate submission chat entry")
		}
		totals := entry.Totals
		st.submissionByChat[key] = &totals
		subSum[0].add(totals.Submitted)
		subSum[1].add(totals.SubmitRejected)
		subSum[2].add(totals.AcceptedThenCancelled)
	}
	var interruptedSum checkedSum
	for _, entry := range p.SubmissionByChat {
		interruptedSum.add(entry.Totals.InterruptedByRestart)
	}
	if subSum[0].overflow || subSum[1].overflow || subSum[2].overflow || interruptedSum.overflow ||
		subSum[0].v != p.Submission.Submitted || subSum[1].v != p.Submission.SubmitRejected ||
		subSum[2].v != p.Submission.AcceptedThenCancelled || interruptedSum.v != p.Submission.InterruptedByRestart {
		return interactionState{}, malformed("global submission totals do not equal the per-chat sums")
	}

	var preflightSums [preflightKindCount]checkedSum
	for _, entry := range p.PreflightByChat {
		key, err := v.chatKeyOf(entry.Chat)
		if err != nil {
			return interactionState{}, err
		}
		if _, dup := st.preflightByChat[key]; dup {
			return interactionState{}, malformed("duplicate preflight chat entry")
		}
		counts := entry.Counts
		st.preflightByChat[key] = &counts
		for i, c := range counts {
			preflightSums[i].add(c)
		}
	}
	for i := range preflightSums {
		if preflightSums[i].overflow || preflightSums[i].v != p.Preflight[i] {
			return interactionState{}, malformed("global preflight totals do not equal the per-chat sums")
		}
	}

	if err := v.buildDecision(&st); err != nil {
		return interactionState{}, err
	}
	if err := v.buildExecution(&st); err != nil {
		return interactionState{}, err
	}

	return st, nil
}

func decisionDataSums(buckets []decisionData) (outcomes [outcomeCount]checkedSum, classes [inferenceClassCount]checkedSum,
	scores [scoreDimCount][scoreBinCount]checkedSum, decisionLat, inferenceLat []checkedSum) {
	decisionLat = make([]checkedSum, len(latencyEdges)+1)
	inferenceLat = make([]checkedSum, len(latencyEdges)+1)
	for _, b := range buckets {
		for i, x := range b.Outcomes {
			outcomes[i].add(x)
		}
		for i, x := range b.InferenceClasses {
			classes[i].add(x)
		}
		for d := range b.Scores {
			for i, x := range b.Scores[d] {
				scores[d][i].add(x)
			}
		}
		for i, x := range b.DecisionLatency {
			decisionLat[i].add(x)
		}
		for i, x := range b.InferenceLatency {
			inferenceLat[i].add(x)
		}
	}

	return outcomes, classes, scores, decisionLat, inferenceLat
}

// validateDecisionData checks the internal consistency of one decision data
// block (a bucket or the global totals).
func validateDecisionData(name string, d *decisionData) error {
	outcomes, ok1 := sliceTotal(d.Outcomes[:])
	classes, ok2 := sliceTotal(d.InferenceClasses[:])
	decisionLat, ok3 := sliceTotal(d.DecisionLatency)
	inferenceLat, ok4 := sliceTotal(d.InferenceLatency)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return malformed("%s counters overflow", name)
	}
	if len(d.DecisionLatency) != len(latencyEdges)+1 || len(d.InferenceLatency) != len(latencyEdges)+1 {
		return malformed("%s latency histograms have the wrong bin count", name)
	}
	if decisionLat != outcomes {
		return malformed("%s decision latency total %d differs from the outcome total %d", name, decisionLat, outcomes)
	}
	if inferenceLat != classes {
		return malformed("%s inference latency total %d differs from the inference class total %d", name, inferenceLat, classes)
	}
	first, _ := sliceTotal(d.Scores[0][:])
	for dim := range d.Scores {
		total, ok := sliceTotal(d.Scores[dim][:])
		if !ok || total != first {
			return malformed("%s score histograms disagree on the number of evaluated decisions", name)
		}
	}
	if first > outcomes {
		return malformed("%s has more scored decisions than outcomes", name)
	}

	return nil
}

func (v *interactionValidator) buildDecision(st *interactionState) error {
	p := v.p
	if err := validateDecisionData("global decision", &p.Decision.decisionData); err != nil {
		return err
	}
	if _, ok := sliceTotal(p.Decision.Outcomes[:]); !ok {
		return malformed("decision outcomes overflow")
	}
	classTotal, _ := sliceTotal(p.Decision.InferenceClasses[:])
	if p.Decision.EvaluatedAttempts != classTotal || p.Decision.PretransportRejected != p.Decision.InferenceClasses[InferencePretransportRejected] {
		return malformed("evaluated/pretransport totals do not match the inference classes")
	}

	blocks := make([]decisionData, 0, len(p.DecisionBuckets))
	for i := range p.DecisionBuckets {
		b := &p.DecisionBuckets[i]
		chat, err := v.chatKeyOf(b.Chat)
		if err != nil {
			return err
		}
		profile, err := v.profileKeyOf(b.Profile)
		if err != nil {
			return err
		}
		model, err := modelKeyOf(p.DecisionModels, b.Model)
		if err != nil {
			return err
		}
		if err := validateDecisionData("decision bucket", &b.decisionData); err != nil {
			return err
		}
		key := decisionBucketKey{chat: chat, profile: profile, model: model}
		if _, dup := st.decisionBuckets[key]; dup {
			return malformed("duplicate decision bucket")
		}
		state := &decisionBucketState{outcomes: b.Outcomes, inferenceClasses: b.InferenceClasses}
		for dim := range b.Scores {
			state.scores[dim].counts = b.Scores[dim]
		}
		var err2 error
		if state.decisionLatency, err2 = histogramFrom("decision latency", latencyEdges, b.DecisionLatency); err2 != nil {
			return err2
		}
		if state.inferenceLatency, err2 = histogramFrom("inference latency", latencyEdges, b.InferenceLatency); err2 != nil {
			return err2
		}
		st.decisionBuckets[key] = state
		blocks = append(blocks, b.decisionData)
	}

	outcomes, classes, scores, decisionLat, inferenceLat := decisionDataSums(blocks)
	for i := range outcomes {
		if outcomes[i].overflow || outcomes[i].v != p.Decision.Outcomes[i] {
			return malformed("global decision outcomes differ from the bucket sums")
		}
	}
	for i := range classes {
		if classes[i].overflow || classes[i].v != p.Decision.InferenceClasses[i] {
			return malformed("global inference classes differ from the bucket sums")
		}
	}
	for d := range scores {
		for i := range scores[d] {
			if scores[d][i].overflow || scores[d][i].v != p.Decision.Scores[d][i] {
				return malformed("global score histograms differ from the bucket sums")
			}
		}
	}
	for i := range decisionLat {
		if decisionLat[i].overflow || decisionLat[i].v != p.Decision.DecisionLatency[i] ||
			inferenceLat[i].overflow || inferenceLat[i].v != p.Decision.InferenceLatency[i] {
			return malformed("global latency histograms differ from the bucket sums")
		}
	}

	st.decision.evaluatedAttempts = p.Decision.EvaluatedAttempts
	st.decision.pretransportRejected = p.Decision.PretransportRejected
	st.decision.outcomes = p.Decision.Outcomes
	st.decision.inferenceClasses = p.Decision.InferenceClasses
	for dim := range p.Decision.Scores {
		st.decision.scores[dim].counts = p.Decision.Scores[dim]
	}
	var err error
	if st.decision.decisionLatency, err = histogramFrom("decision latency", latencyEdges, p.Decision.DecisionLatency); err != nil {
		return err
	}
	st.decision.inferenceLatency, err = histogramFrom("inference latency", latencyEdges, p.Decision.InferenceLatency)

	return err
}

// validateExecutionData checks one execution block's internal consistency.
func validateExecutionData(name string, e *executionData) error {
	if len(e.Duration) != len(executionLatencyEdges)+1 {
		return malformed("%s duration histogram has the wrong bin count", name)
	}
	completed, ok1 := sliceTotal(e.Duration)
	failures, ok2 := sliceTotal(e.Failures[:])
	dispatched, ok3 := sliceTotal(e.Dispatched[:])
	if !ok1 || !ok2 || !ok3 {
		return malformed("%s counters overflow", name)
	}
	if failures != e.TurnsFailed {
		return malformed("%s failure classes total %d differs from turns_failed %d", name, failures, e.TurnsFailed)
	}
	var nonFailed checkedSum
	nonFailed.add(e.TurnsFailed)
	nonFailed.add(e.TurnsWithoutEffect)
	if nonFailed.overflow || nonFailed.v > completed || e.TurnsWithEffect > completed {
		return malformed("%s has more classified turns than completed executions", name)
	}
	var settled checkedSum
	settled.add(completed)
	settled.add(e.TurnsInterrupted)
	if settled.overflow || settled.v > dispatched {
		return malformed("%s has more completed or interrupted executions than dispatches", name)
	}

	return nil
}

func (v *interactionValidator) buildExecution(st *interactionState) error {
	p := v.p
	if err := validateExecutionData("global execution", &p.Execution); err != nil {
		return err
	}

	var (
		dispatched [actionCount]checkedSum
		failures   [execFailureCount - 1]checkedSum
		scalars    [7]checkedSum
		duration   = make([]checkedSum, len(executionLatencyEdges)+1)
	)
	for i := range p.ExecutionBuckets {
		b := &p.ExecutionBuckets[i]
		chat, err := v.chatKeyOf(b.Chat)
		if err != nil {
			return err
		}
		profile, err := v.profileKeyOf(b.Profile)
		if err != nil {
			return err
		}
		decisionModel, err := modelKeyOf(p.DecisionModels, b.DecisionModel)
		if err != nil {
			return err
		}
		generationModel, err := modelKeyOf(p.GenerationModels, b.GenerationModel)
		if err != nil {
			return err
		}
		if err := validateExecutionData("execution bucket", &b.executionData); err != nil {
			return err
		}
		key := executionBucketKey{chat: chat, profile: profile, decisionModel: decisionModel, generationModel: generationModel}
		if _, dup := st.executionBuckets[key]; dup {
			return malformed("duplicate execution bucket")
		}
		state := &executionBucketState{counters: executionCountersFrom(&b.executionData)}
		if state.duration, err = histogramFrom("execution duration", executionLatencyEdges, b.Duration); err != nil {
			return err
		}
		st.executionBuckets[key] = state

		for a := range b.Dispatched {
			dispatched[a].add(b.Dispatched[a])
		}
		for f := range b.Failures {
			failures[f].add(b.Failures[f])
		}
		for s, x := range []uint64{b.Reactions, b.Texts, b.OtherSideEffects, b.TurnsWithEffect, b.TurnsWithoutEffect, b.TurnsFailed, b.TurnsInterrupted} {
			scalars[s].add(x)
		}
		for d, x := range b.Duration {
			duration[d].add(x)
		}
	}

	for a := range dispatched {
		if dispatched[a].overflow || dispatched[a].v != p.Execution.Dispatched[a] {
			return malformed("global dispatched counters differ from the bucket sums")
		}
	}
	for f := range failures {
		if failures[f].overflow || failures[f].v != p.Execution.Failures[f] {
			return malformed("global failure counters differ from the bucket sums")
		}
	}
	for s, want := range []uint64{p.Execution.Reactions, p.Execution.Texts, p.Execution.OtherSideEffects,
		p.Execution.TurnsWithEffect, p.Execution.TurnsWithoutEffect, p.Execution.TurnsFailed, p.Execution.TurnsInterrupted} {
		if scalars[s].overflow || scalars[s].v != want {
			return malformed("global execution counters differ from the bucket sums")
		}
	}
	for d := range duration {
		if duration[d].overflow || duration[d].v != p.Execution.Duration[d] {
			return malformed("global execution durations differ from the bucket sums")
		}
	}

	st.execution.counters = executionCountersFrom(&p.Execution)
	var err error
	st.execution.duration, err = histogramFrom("execution duration", executionLatencyEdges, p.Execution.Duration)

	return err
}

func executionCountersFrom(e *executionData) executionCountersState {
	return executionCountersState{
		dispatched:         e.Dispatched,
		reactions:          e.Reactions,
		texts:              e.Texts,
		otherSideEffects:   e.OtherSideEffects,
		turnsWithEffect:    e.TurnsWithEffect,
		turnsWithoutEffect: e.TurnsWithoutEffect,
		turnsFailed:        e.TurnsFailed,
		turnsInterrupted:   e.TurnsInterrupted,
		failures:           e.Failures,
	}
}

// chatLedger aggregates one chat key's ledger terms for reconciliation.
type chatLedger struct {
	sub       SubmissionTotals
	preflight checkedSum
	decisions checkedSum
}

// reconcile converts operations that were pending or in flight at the last
// successful flush into explicit interruption counters. It never invents a
// successful outcome. The formulas subtract the counters reconciled
// earlier, so repeated restore/flush/restart cycles add nothing:
//
//	pending  = Submitted - SubmitRejected - AcceptedThenCancelled - Preflight - Decisions - InterruptedByRestart
//	inflight = Dispatched - Completed - TurnsInterrupted
//
// Completed is the execution-duration histogram total (exactly one record
// per completed turn). An impossible ledger (a negative intermediate) is
// reported as malformed.
func (v *interactionValidator) reconcile(st *interactionState) (bool, error) {
	changed := false

	ledgers := map[chatKey]*chatLedger{}
	ledger := func(k chatKey) *chatLedger {
		l, ok := ledgers[k]
		if !ok {
			l = &chatLedger{}
			ledgers[k] = l
		}

		return l
	}
	for k, c := range st.submissionByChat {
		ledger(k).sub = *c
	}
	for k, c := range st.preflightByChat {
		for _, x := range c {
			ledger(k).preflight.add(x)
		}
	}
	for k, b := range st.decisionBuckets {
		for _, x := range b.outcomes {
			ledger(chatKey{id: k.chat.id, overflow: k.chat.overflow}).decisions.add(x)
		}
	}

	var globalPending checkedSum
	for k, l := range ledgers {
		if l.preflight.overflow || l.decisions.overflow {
			return false, malformed("ledger counters overflow")
		}
		pending, ok := subtract(l.sub.Submitted, l.sub.SubmitRejected, l.sub.AcceptedThenCancelled, l.preflight.v, l.decisions.v, l.sub.InterruptedByRestart)
		if !ok {
			return false, malformed("impossible submission ledger: terminated decision phases exceed accepted submissions")
		}
		if pending == 0 {
			continue
		}
		counts, ok := st.submissionByChat[k]
		if !ok {
			counts = &SubmissionTotals{}
			st.submissionByChat[k] = counts
		}
		counts.InterruptedByRestart += pending
		globalPending.add(pending)
		changed = true
	}
	if globalPending.overflow {
		return false, malformed("ledger counters overflow")
	}
	st.submission.InterruptedByRestart += globalPending.v

	var globalInflight checkedSum
	for _, b := range st.executionBuckets {
		inflight, err := executionInflight(&b.counters, &b.duration)
		if err != nil {
			return false, err
		}
		if inflight == 0 {
			continue
		}
		b.counters.turnsInterrupted += inflight
		globalInflight.add(inflight)
		changed = true
	}
	if globalInflight.overflow {
		return false, malformed("execution counters overflow")
	}
	st.execution.counters.turnsInterrupted += globalInflight.v

	// The global ledgers must agree with the per-key ones after the update.
	globalSub := st.submission
	if pending, ok := subtract(globalSub.Submitted, globalSub.SubmitRejected, globalSub.AcceptedThenCancelled, sumArray(st.preflightTotal[:]), sumArray(st.decision.outcomes[:]), globalSub.InterruptedByRestart); !ok || pending != 0 {
		return false, malformed("global submission ledger disagrees with the per-chat ledgers")
	}
	if inflight, err := executionInflight(&st.execution.counters, &st.execution.duration); err != nil || inflight != 0 {
		return false, malformed("global execution ledger disagrees with the bucket ledgers")
	}

	return changed, nil
}

func sumArray(values []uint64) uint64 {
	total, _ := sliceTotal(values)

	return total
}

func executionInflight(c *executionCountersState, duration *latencyHistogram) (uint64, error) {
	completed := duration.total()
	dispatched := c.dispatched[ActionReact] + c.dispatched[ActionReply]
	inflight, ok := subtract(dispatched, completed, c.turnsInterrupted)
	if !ok {
		return 0, malformed("impossible execution ledger: completed and interrupted turns exceed dispatches")
	}

	return inflight, nil
}
