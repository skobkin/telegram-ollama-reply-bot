package statistics

import (
	"math"
	"time"
)

// Cardinality bounds of the tool-usage dataset. Chats and models share the
// interaction caps (MaxTrackedChats, MaxTrackedModels) but are tracked in
// their own sets so the tool reset stays independent of the interaction one.
const (
	// MaxTrackedTools bounds the distinct tool names holding rows. Registered
	// tools beyond it fold into the tagged other_registered key.
	MaxTrackedTools = 128
	// MaxTrackedErrorCodes bounds the dataset-wide error-code dictionary.
	MaxTrackedErrorCodes = 64
	// MaxErrorCodesPerTool bounds the distinct error codes admitted per tool
	// key. The first codes seen are admitted; later ones fold into "other"
	// for that tool, identically in every projection.
	MaxErrorCodesPerTool = 16
	// maxErrorCodeBytes bounds one error code; invalid or longer codes fold
	// into "other" without truncation.
	maxErrorCodeBytes = 48

	// otherCodeID is the tagged id of the folded error-code bucket.
	otherCodeID uint16 = math.MaxUint16
)

// ToolMode distinguishes explicit from proactive generation turns.
type ToolMode uint8

const (
	ToolModeExplicit ToolMode = iota
	ToolModeProactive
	toolModeCount
)

func (m ToolMode) String() string {
	if m == ToolModeProactive {
		return "proactive"
	}

	return "explicit"
}

// ToolRunFailure is the bounded generation/runtime outcome of a turn as seen
// at the observation point. It deliberately does NOT cover final Telegram
// text delivery: the observation precedes it, and interaction execution
// statistics own delivery failures.
type ToolRunFailure uint8

const (
	ToolRunOK ToolRunFailure = iota
	ToolRunTimeout
	ToolRunCancelled
	ToolRunGeneration
	toolRunFailureCount
)

// ToolRejection is the bounded reason a call never reached its handler.
type ToolRejection uint8

const (
	ToolNotRejected ToolRejection = iota
	ToolRejectedNotRegistered
	ToolRejectedNotAdvertised
	ToolRejectedUnavailable
	toolRejectionCount
)

// ToolPreparation classifies how a call's model-facing result was prepared.
type ToolPreparation uint8

const (
	ToolPreparationComplete ToolPreparation = iota
	ToolPreparationReduced
	ToolPreparationFailed
)

// ToolCallObservation is the statistics-facing view of one authoritative
// tool-call record. Name is used ONLY for in-turn retry/recovery matching and
// is never stored: raw model-chosen names must not become aggregation keys.
type ToolCallObservation struct {
	Name              string
	Rejection         ToolRejection
	Executed          bool
	Status            string // "ok", "empty" or "error"
	ErrorCode         string
	ErrorClass        string
	SideEffecting     bool
	SideEffectOutcome string // "none", "not_applied", "confirmed", "unknown"
	Preparation       ToolPreparation
	Duration          time.Duration
}

// ToolTurnObservation records one conversation turn's tool activity. It is
// recorded once per turn after the generation/tool loop ended, on every exit
// path (success, silence, failure, cancellation, timeout). It carries no
// arguments, results, message content, IDs or timestamps.
type ToolTurnObservation struct {
	// Epoch is captured with ToolEpoch before the turn starts; a reset in
	// between drops the whole turn.
	Epoch           uint64
	ChatID          int64
	Mode            ToolMode
	GenerationModel string
	// Advertised is the set of tools advertised in submitted generation
	// requests (once per tool per turn).
	Advertised []string
	// Calls are the authoritative records in execution order.
	Calls      []ToolCallObservation
	RunFailure ToolRunFailure
}

// toolCounter indexes the per-row scalar counters.
type toolCounter uint8

const (
	tcAdvertisedTurns toolCounter = iota
	tcAdvertisedInvokedTurns
	tcInvokedTurns
	tcAttempts
	tcExecuted
	tcSucceeded
	tcSucceededEmpty
	tcFailed
	tcRejectedNotRegistered
	tcRejectedNotAdvertised
	tcRejectedUnavailable
	tcSideConfirmed
	tcSideNotApplied
	tcSideUnknown
	tcPrepReduced
	tcPrepFailed
	tcRetries
	tcRecoveredCalls
	tcFailureIdentityTurns
	tcRecoveredIdentityTurns
	tcRepeatsAfterPrepFailure
	tcStatusUnrecognized
	toolCounterCount
)

// toolErrorClassNames are the closed error-class set plus "other".
var toolErrorClassNames = [...]string{
	"invalid_arguments", "not_found", "unavailable", "rate_limited",
	"permanent", "transient", "cancelled", "ambiguous_outcome", "other",
}

const toolClassCount = len(toolErrorClassNames)

func toolClassIndex(class string) int {
	for i, name := range toolErrorClassNames[:toolClassCount-1] {
		if name == class {
			return i
		}
	}

	return toolClassCount - 1
}

// toolLatencyEdges are the execution-latency bin boundaries in milliseconds
// (14 bins, the last open-ended).
var toolLatencyEdges = []float64{10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000}

// toolRow is one aggregation row: scalar counters, error classes, admitted
// error codes and the execution-latency histogram.
type toolRow struct {
	c       [toolCounterCount]uint64
	classes [toolClassCount]uint64
	codes   map[uint16]uint64
	latency latencyHistogram
}

func newToolRow() *toolRow {
	return &toolRow{latency: newLatencyHistogram(toolLatencyEdges)}
}

func (r *toolRow) add(o *toolRow) {
	for i := range r.c {
		r.c[i] += o.c[i]
	}
	for i := range r.classes {
		r.classes[i] += o.classes[i]
	}
	for id, n := range o.codes {
		if r.codes == nil {
			r.codes = map[uint16]uint64{}
		}
		r.codes[id] += n
	}
	r.latency.add(o.latency)
}

func (r *toolRow) addCode(id uint16) {
	if r.codes == nil {
		r.codes = map[uint16]uint64{}
	}
	r.codes[id]++
}

// toolKind tags an aggregation identity. Overflow kinds are tags, never
// names, so they cannot collide with a real tool called "other_registered".
type toolKind uint8

const (
	toolTracked toolKind = iota
	toolOtherRegistered
	toolUnregistered
)

type toolKey struct {
	kind toolKind
	name string // tracked only
}

type toolModelKey struct {
	model modelKey
	mode  ToolMode
	tool  toolKey
}

type toolChatKey struct {
	chat chatKey
	mode ToolMode
	tool toolKey
}

type toolTurnModelKey struct {
	model modelKey
	mode  ToolMode
}

type toolTurnChatKey struct {
	chat chatKey
	mode ToolMode
}

type toolTurnCounters struct {
	turns     uint64
	withCalls uint64
	failures  [toolRunFailureCount - 1]uint64
}

func (t *toolTurnCounters) add(o *toolTurnCounters) {
	t.turns += o.turns
	t.withCalls += o.withCalls
	for i := range t.failures {
		t.failures[i] += o.failures[i]
	}
}

// toolState is the tool-usage dataset, guarded by the collector mutex.
type toolState struct {
	periodStartedAt time.Time

	trackedChats  map[int64]struct{}
	trackedModels map[string]struct{}
	trackedTools  map[string]struct{}

	// codeNames is the dataset-wide error-code dictionary (id = index);
	// admission restricts which ids each tool key may use.
	codeNames []string
	codeIDs   map[string]uint16
	admission map[toolKey]map[uint16]struct{}

	oversizedIDFolds uint64

	byModel      map[toolModelKey]*toolRow
	byChat       map[toolChatKey]*toolRow
	turnsByModel map[toolTurnModelKey]*toolTurnCounters
	turnsByChat  map[toolTurnChatKey]*toolTurnCounters
}

func initTool(started time.Time) toolState {
	return toolState{
		periodStartedAt: started,
		trackedChats:    map[int64]struct{}{},
		trackedModels:   map[string]struct{}{},
		trackedTools:    map[string]struct{}{},
		codeIDs:         map[string]uint16{},
		admission:       map[toolKey]map[uint16]struct{}{},
		byModel:         map[toolModelKey]*toolRow{},
		byChat:          map[toolChatKey]*toolRow{},
		turnsByModel:    map[toolTurnModelKey]*toolTurnCounters{},
		turnsByChat:     map[toolTurnChatKey]*toolTurnCounters{},
	}
}

// ToolEpoch returns the tool dataset's current reset generation. Capture it
// before a turn starts and pass it in the observation: a reset in between
// drops the whole turn.
func (c *Collector) ToolEpoch() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.toolPersist.current.Generation
}

// SetToolCatalog installs the registered tool catalog (registration order),
// used to report registered tools with zero usage and to disclose what the
// bounds omit. Catalog names occupy tracked slots after already-tracked
// (restored) names, up to MaxTrackedTools.
func (c *Collector) SetToolCatalog(names []string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.toolCatalog = append([]string(nil), names...)
	c.applyToolCatalogLocked()
}

func (c *Collector) applyToolCatalogLocked() {
	changed := false
	for _, name := range c.toolCatalog {
		if _, ok := c.tool.trackedTools[name]; ok {
			continue
		}
		if len(c.tool.trackedTools) >= MaxTrackedTools {
			break
		}
		c.tool.trackedTools[name] = struct{}{}
		changed = true
	}
	if changed {
		c.toolPersist.touch()
	}
}

// ResetToolUsage resets the tool-usage dataset (global scope only), starting
// a new generation. General and interaction statistics are never touched. It
// returns the new generation for durable acknowledgement.
func (c *Collector) ResetToolUsage() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.tool = initTool(c.now())
	c.toolResets++
	generation := c.toolPersist.reset()
	c.applyToolCatalogLocked()

	return generation
}

func (c *Collector) resolveToolChatLocked(chatID int64) chatKey {
	if _, ok := c.tool.trackedChats[chatID]; ok {
		return chatKey{id: chatID}
	}
	if len(c.tool.trackedChats) < MaxTrackedChats {
		c.tool.trackedChats[chatID] = struct{}{}

		return chatKey{id: chatID}
	}

	return chatKey{overflow: true}
}

func (c *Collector) resolveToolModelLocked(name string) modelKey {
	if len(name) > maxModelIDBytes {
		c.tool.oversizedIDFolds++

		return modelKey{overflow: true}
	}
	if _, ok := c.tool.trackedModels[name]; ok {
		return modelKey{name: name}
	}
	if len(c.tool.trackedModels) < MaxTrackedModels {
		c.tool.trackedModels[name] = struct{}{}

		return modelKey{name: name}
	}

	return modelKey{overflow: true}
}

// resolveToolKeyLocked maps a REGISTERED tool name (advertised names and the
// names of executed or not-advertised calls are registered by construction)
// to its tracked key, or folds it into other_registered at the cap.
func (c *Collector) resolveToolKeyLocked(name string) toolKey {
	if _, ok := c.tool.trackedTools[name]; ok {
		return toolKey{kind: toolTracked, name: name}
	}
	if len(c.tool.trackedTools) < MaxTrackedTools {
		c.tool.trackedTools[name] = struct{}{}

		return toolKey{kind: toolTracked, name: name}
	}

	return toolKey{kind: toolOtherRegistered}
}

func validErrorCode(code string) bool {
	if code == "" || len(code) > maxErrorCodeBytes {
		return false
	}
	for i := 0; i < len(code); i++ {
		ch := code[i]
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '_' {
			return false
		}
	}

	return true
}

// resolveErrorCodeLocked admits an error code for a tool key. The decision is
// made once per (tool key, code) and reused for every projection, so
// byModel and byChat always fold identically, whatever order codes arrive in.
// The dictionary interns a code only when it will be admitted.
func (c *Collector) resolveErrorCodeLocked(tool toolKey, code string) uint16 {
	if !validErrorCode(code) {
		return otherCodeID
	}
	t := &c.tool
	id, known := t.codeIDs[code]
	set := t.admission[tool]
	if known {
		if _, ok := set[id]; ok {
			return id
		}
	}
	if len(set) >= MaxErrorCodesPerTool {
		return otherCodeID
	}
	if !known {
		if len(t.codeNames) >= MaxTrackedErrorCodes {
			return otherCodeID
		}
		id = uint16(len(t.codeNames)) //nolint:gosec // bounded by MaxTrackedErrorCodes
		t.codeNames = append(t.codeNames, code)
		t.codeIDs[code] = id
	}
	if set == nil {
		set = map[uint16]struct{}{}
		t.admission[tool] = set
	}
	set[id] = struct{}{}

	return id
}

// toolIdentity is the original (pre-folding) identity of a call within one
// turn. Retry and recovery matching uses it, never the folded key.
type toolIdentity struct {
	name         string
	unregistered bool
}

type toolIdentityState struct {
	attempted      bool
	advertised     bool
	failedSeen     bool
	recovered      bool
	prepFailedSeen bool
}

// RecordToolTurn records one turn's tool activity. See the tool statistics
// specification for the exact counting rules.
func (c *Collector) RecordToolTurn(obs ToolTurnObservation) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if obs.Epoch != c.toolPersist.current.Generation {
		return
	}
	if obs.Mode >= toolModeCount || obs.RunFailure >= toolRunFailureCount {
		return
	}

	model := c.resolveToolModelLocked(obs.GenerationModel)
	chat := c.resolveToolChatLocked(obs.ChatID)

	delta := map[toolKey]*toolRow{}
	rowOf := func(k toolKey) *toolRow {
		row, ok := delta[k]
		if !ok {
			row = newToolRow()
			delta[k] = row
		}

		return row
	}
	keyOf := func(id toolIdentity) toolKey {
		if id.unregistered {
			return toolKey{kind: toolUnregistered}
		}

		return c.resolveToolKeyLocked(id.name)
	}

	local := map[toolIdentity]*toolIdentityState{}
	var order []toolIdentity // first-seen order keeps slot assignment deterministic
	stateOf := func(id toolIdentity) *toolIdentityState {
		st, ok := local[id]
		if !ok {
			st = &toolIdentityState{}
			local[id] = st
			order = append(order, id)
		}

		return st
	}
	for _, name := range obs.Advertised {
		stateOf(toolIdentity{name: name}).advertised = true
	}

	for _, call := range obs.Calls {
		id := toolIdentity{name: call.Name, unregistered: call.Rejection == ToolRejectedNotRegistered}
		key := keyOf(id)
		row := rowOf(key)
		st := stateOf(id)
		c.applyToolCall(row, key, st, call)
		st.attempted = true
	}

	// Per-row turn counters are deduplicated AFTER folding: several
	// identities of one overflow row count once per turn.
	advertisedRows, invokedRows, advertisedInvokedRows := map[toolKey]bool{}, map[toolKey]bool{}, map[toolKey]bool{}
	for _, id := range order {
		st := local[id]
		key := keyOf(id)
		if st.advertised {
			advertisedRows[key] = true
		}
		if st.attempted {
			invokedRows[key] = true
		}
		if st.advertised && st.attempted {
			advertisedInvokedRows[key] = true
		}
	}
	for key := range advertisedRows {
		rowOf(key).c[tcAdvertisedTurns]++
	}
	for key := range invokedRows {
		rowOf(key).c[tcInvokedTurns]++
	}
	for key := range advertisedInvokedRows {
		rowOf(key).c[tcAdvertisedInvokedTurns]++
	}

	t := &c.tool
	for key, row := range delta {
		mk := toolModelKey{model: model, mode: obs.Mode, tool: key}
		dst, ok := t.byModel[mk]
		if !ok {
			dst = newToolRow()
			t.byModel[mk] = dst
		}
		dst.add(row)

		ck := toolChatKey{chat: chat, mode: obs.Mode, tool: key}
		dst, ok = t.byChat[ck]
		if !ok {
			dst = newToolRow()
			t.byChat[ck] = dst
		}
		dst.add(row)
	}

	turn := toolTurnCounters{turns: 1}
	if len(obs.Calls) > 0 {
		turn.withCalls = 1
	}
	if obs.RunFailure != ToolRunOK {
		turn.failures[obs.RunFailure-1] = 1
	}
	tm := toolTurnModelKey{model: model, mode: obs.Mode}
	if t.turnsByModel[tm] == nil {
		t.turnsByModel[tm] = &toolTurnCounters{}
	}
	t.turnsByModel[tm].add(&turn)
	tc := toolTurnChatKey{chat: chat, mode: obs.Mode}
	if t.turnsByChat[tc] == nil {
		t.turnsByChat[tc] = &toolTurnCounters{}
	}
	t.turnsByChat[tc].add(&turn)

	c.toolPersist.touch()
}

// applyToolCall folds one call into its row and the identity's turn state.
func (c *Collector) applyToolCall(row *toolRow, key toolKey, st *toolIdentityState, call ToolCallObservation) {
	row.c[tcAttempts]++

	rejection := call.Rejection
	if !call.Executed && rejection == ToolNotRejected {
		rejection = ToolRejectedUnavailable
	}

	success := false
	failure := false
	errorClass := call.ErrorClass
	switch {
	case rejection != ToolNotRejected && rejection < toolRejectionCount:
		row.c[tcRejectedNotRegistered+toolCounter(rejection-1)]++
		failure = true
	case rejection >= toolRejectionCount:
		row.c[tcRejectedUnavailable]++
		failure = true
	default:
		row.c[tcExecuted]++
		row.latency.record(call.Duration.Seconds() * 1000)
		switch call.Status {
		case "ok":
			success = true
			row.c[tcSucceeded]++
		case "empty":
			success = true
			row.c[tcSucceeded]++
			row.c[tcSucceededEmpty]++
		default:
			failure = true
			row.c[tcFailed]++
			if call.Status != "error" {
				row.c[tcStatusUnrecognized]++
				errorClass = "other"
			}
		}
		if call.SideEffecting {
			switch call.SideEffectOutcome {
			case "confirmed":
				row.c[tcSideConfirmed]++
			case "not_applied":
				row.c[tcSideNotApplied]++
			case "unknown":
				row.c[tcSideUnknown]++
			}
		}
		switch call.Preparation {
		case ToolPreparationReduced:
			row.c[tcPrepReduced]++
		case ToolPreparationFailed:
			row.c[tcPrepFailed]++
		}
	}

	if failure {
		row.classes[toolClassIndex(errorClass)]++
		row.addCode(c.resolveErrorCodeLocked(key, call.ErrorCode))
	}

	// Retry and recovery describe OBSERVABLE later calls of the same original
	// identity within the turn, never the model's reasoning. A model-facing
	// result preparation failure is not an execution failure and takes no
	// part in them.
	if st.failedSeen {
		row.c[tcRetries]++
		if success {
			row.c[tcRecoveredCalls]++
			if !st.recovered {
				st.recovered = true
				row.c[tcRecoveredIdentityTurns]++
			}
		}
	}
	if failure && !st.failedSeen {
		st.failedSeen = true
		row.c[tcFailureIdentityTurns]++
	}
	if st.prepFailedSeen {
		row.c[tcRepeatsAfterPrepFailure]++
	}
	if call.Executed && call.Preparation == ToolPreparationFailed {
		st.prepFailedSeen = true
	}
}
