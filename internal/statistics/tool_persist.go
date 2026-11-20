package statistics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

const (
	// ToolDatasetName is the storage key of the tool-usage dataset.
	ToolDatasetName = "tool_usage"

	toolFormatVersion = 1

	// Tagged tool references in the payload (>= 0 indexes the tool dictionary).
	toolRefOtherRegistered = -1
	toolRefUnregistered    = -2
)

// toolDataset adapts the collector's tool group to the persister.
type toolDataset struct{ c *Collector }

var _ Dataset = toolDataset{}

// ToolDataset returns the persistable view of the tool-usage statistics.
func (c *Collector) ToolDataset() Dataset { return toolDataset{c: c} }

func (d toolDataset) Name() string       { return ToolDatasetName }
func (d toolDataset) FormatVersion() int { return toolFormatVersion }

func (d toolDataset) Capture() (PersistPair, func() ([]byte, error), bool) {
	d.c.mu.Lock()
	pair := d.c.toolPersist.current
	if !d.c.toolPersist.dirty() {
		d.c.mu.Unlock()

		return pair, nil, false
	}
	payload := d.c.toolPayloadLocked()
	d.c.mu.Unlock()

	return pair, func() ([]byte, error) { return json.Marshal(payload) }, true
}

func (d toolDataset) Acknowledge(pair PersistPair) {
	d.c.mu.Lock()
	defer d.c.mu.Unlock()
	d.c.toolPersist.ack(pair)
}

func (d toolDataset) SetPersistenceInfo(info PersistenceInfo) {
	d.c.mu.Lock()
	defer d.c.mu.Unlock()
	d.c.toolPersist.info = info
}

func (d toolDataset) Restore(version int, pair PersistPair, payload []byte) error {
	if version != toolFormatVersion {
		return fmt.Errorf("%w: unsupported tool usage format version %d", ErrMalformedSnapshot, version)
	}
	state, resets, err := decodeToolPayload(payload)
	if err != nil {
		return err
	}

	d.c.mu.Lock()
	defer d.c.mu.Unlock()
	d.c.tool = state
	d.c.toolResets = resets
	d.c.toolPersist.install(pair)
	d.c.applyToolCatalogLocked()

	return nil
}

// --- payload (format version 1) ---

type toolPayload struct {
	PeriodStartedAt  time.Time `json:"period_started_at"`
	Resets           uint64    `json:"resets"`
	Chats            []int64   `json:"chats"`
	Models           []string  `json:"models"`
	Tools            []string  `json:"tools"`
	ErrorCodes       []string  `json:"error_codes"`
	OversizedIDFolds uint64    `json:"oversized_id_folds"`

	Admission  []toolAdmissionPayload `json:"admission"`
	ByModel    []toolModelRowPayload  `json:"by_model"`
	ByChat     []toolChatRowPayload   `json:"by_chat"`
	TurnsModel []toolTurnModelPayload `json:"turns_by_model"`
	TurnsChat  []toolTurnChatPayload  `json:"turns_by_chat"`
}

type toolAdmissionPayload struct {
	Tool  int   `json:"tool"`
	Codes []int `json:"codes"`
}

type toolRowData struct {
	C       [toolCounterCount]uint64 `json:"c"`
	Classes [toolClassCount]uint64   `json:"classes"`
	Codes   [][2]uint64              `json:"codes"` // [codeID, count]
	Latency []uint64                 `json:"latency"`
}

type toolModelRowPayload struct {
	Model int `json:"model"`
	Mode  int `json:"mode"`
	Tool  int `json:"tool"`
	toolRowData
}

type toolChatRowPayload struct {
	Chat int `json:"chat"`
	Mode int `json:"mode"`
	Tool int `json:"tool"`
	toolRowData
}

type toolTurnData struct {
	Turns     uint64                          `json:"turns"`
	WithCalls uint64                          `json:"with_calls"`
	Failures  [toolRunFailureCount - 1]uint64 `json:"failures"`
}

type toolTurnModelPayload struct {
	Model int `json:"model"`
	Mode  int `json:"mode"`
	toolTurnData
}

type toolTurnChatPayload struct {
	Chat int `json:"chat"`
	Mode int `json:"mode"`
	toolTurnData
}

func toolRowDataOf(r *toolRow) toolRowData {
	d := toolRowData{C: r.c, Classes: r.classes, Latency: append([]uint64(nil), r.latency.counts...), Codes: [][2]uint64{}}
	for id, n := range r.codes {
		d.Codes = append(d.Codes, [2]uint64{uint64(id), n})
	}
	sort.Slice(d.Codes, func(i, j int) bool { return d.Codes[i][0] < d.Codes[j][0] })

	return d
}

// toolPayloadLocked deep-copies the tool dataset with deterministic order.
// The caller holds c.mu.
func (c *Collector) toolPayloadLocked() toolPayload {
	t := &c.tool

	chats := make([]int64, 0, len(t.trackedChats))
	for id := range t.trackedChats {
		chats = append(chats, id)
	}
	sort.Slice(chats, func(i, j int) bool { return chats[i] < chats[j] })
	chatIdx := make(map[int64]int, len(chats))
	for i, id := range chats {
		chatIdx[id] = i
	}
	models, modelIdx := sortedDictionary(t.trackedModels)
	tools, toolIdx := sortedDictionary(t.trackedTools)

	chatRef := func(k chatKey) int {
		if k.overflow {
			return overflowRef
		}

		return chatIdx[k.id]
	}
	modelRef := func(k modelKey) int {
		if k.overflow {
			return overflowRef
		}

		return modelIdx[k.name]
	}
	toolRef := func(k toolKey) int {
		switch k.kind {
		case toolOtherRegistered:
			return toolRefOtherRegistered
		case toolUnregistered:
			return toolRefUnregistered
		}

		return toolIdx[k.name]
	}

	p := toolPayload{
		PeriodStartedAt:  t.periodStartedAt,
		Resets:           c.toolResets,
		Chats:            chats,
		Models:           models,
		Tools:            tools,
		ErrorCodes:       append([]string{}, t.codeNames...),
		OversizedIDFolds: t.oversizedIDFolds,
	}
	for key, set := range t.admission {
		entry := toolAdmissionPayload{Tool: toolRef(key), Codes: []int{}}
		for id := range set {
			entry.Codes = append(entry.Codes, int(id))
		}
		sort.Ints(entry.Codes)
		p.Admission = append(p.Admission, entry)
	}
	sort.Slice(p.Admission, func(i, j int) bool { return p.Admission[i].Tool < p.Admission[j].Tool })

	for key, row := range t.byModel {
		p.ByModel = append(p.ByModel, toolModelRowPayload{Model: modelRef(key.model), Mode: int(key.mode), Tool: toolRef(key.tool), toolRowData: toolRowDataOf(row)})
	}
	sort.Slice(p.ByModel, func(i, j int) bool {
		a, b := p.ByModel[i], p.ByModel[j]
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.Mode != b.Mode {
			return a.Mode < b.Mode
		}

		return a.Tool < b.Tool
	})
	for key, row := range t.byChat {
		p.ByChat = append(p.ByChat, toolChatRowPayload{Chat: chatRef(key.chat), Mode: int(key.mode), Tool: toolRef(key.tool), toolRowData: toolRowDataOf(row)})
	}
	sort.Slice(p.ByChat, func(i, j int) bool {
		a, b := p.ByChat[i], p.ByChat[j]
		if a.Chat != b.Chat {
			return a.Chat < b.Chat
		}
		if a.Mode != b.Mode {
			return a.Mode < b.Mode
		}

		return a.Tool < b.Tool
	})
	for key, turns := range t.turnsByModel {
		p.TurnsModel = append(p.TurnsModel, toolTurnModelPayload{Model: modelRef(key.model), Mode: int(key.mode), toolTurnData: toolTurnData{Turns: turns.turns, WithCalls: turns.withCalls, Failures: turns.failures}})
	}
	sort.Slice(p.TurnsModel, func(i, j int) bool {
		a, b := p.TurnsModel[i], p.TurnsModel[j]
		if a.Model != b.Model {
			return a.Model < b.Model
		}

		return a.Mode < b.Mode
	})
	for key, turns := range t.turnsByChat {
		p.TurnsChat = append(p.TurnsChat, toolTurnChatPayload{Chat: chatRef(key.chat), Mode: int(key.mode), toolTurnData: toolTurnData{Turns: turns.turns, WithCalls: turns.withCalls, Failures: turns.failures}})
	}
	sort.Slice(p.TurnsChat, func(i, j int) bool {
		a, b := p.TurnsChat[i], p.TurnsChat[j]
		if a.Chat != b.Chat {
			return a.Chat < b.Chat
		}

		return a.Mode < b.Mode
	})

	return p
}

// --- decode and validate ---

func decodeToolPayload(raw []byte) (toolState, uint64, error) {
	var p toolPayload
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return toolState{}, 0, malformed("decode: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return toolState{}, 0, malformed("trailing data after the payload")
	}

	state, err := buildToolState(&p)
	if err != nil {
		return toolState{}, 0, err
	}

	return state, p.Resets, nil
}

func toolKeyOf(p *toolPayload, ref int) (toolKey, error) {
	switch {
	case ref == toolRefOtherRegistered:
		return toolKey{kind: toolOtherRegistered}, nil
	case ref == toolRefUnregistered:
		return toolKey{kind: toolUnregistered}, nil
	case ref >= 0 && ref < len(p.Tools):
		return toolKey{kind: toolTracked, name: p.Tools[ref]}, nil
	}

	return toolKey{}, malformed("tool reference %d is out of range", ref)
}

func toolModeOf(v int) (ToolMode, error) {
	if v < 0 || v >= int(toolModeCount) {
		return 0, malformed("tool mode %d is out of range", v)
	}

	return ToolMode(v), nil
}

// validateToolRow checks one row's internal invariants.
func validateToolRow(name string, r *toolRow, admitted map[uint16]struct{}, codeCount int) error {
	c := &r.c
	var rejected checkedSum
	rejected.add(c[tcRejectedNotRegistered])
	rejected.add(c[tcRejectedNotAdvertised])
	rejected.add(c[tcRejectedUnavailable])
	var executed, attempts checkedSum
	executed.add(c[tcSucceeded])
	executed.add(c[tcFailed])
	attempts.add(c[tcExecuted])
	attempts.add(rejected.v)
	if rejected.overflow || executed.overflow || attempts.overflow {
		return malformed("%s counters overflow", name)
	}
	if executed.v != c[tcExecuted] {
		return malformed("%s: succeeded + failed differs from executed", name)
	}
	if attempts.v != c[tcAttempts] {
		return malformed("%s: executed + rejected differs from attempts", name)
	}
	if c[tcSucceededEmpty] > c[tcSucceeded] {
		return malformed("%s: more empty successes than successes", name)
	}
	if c[tcAdvertisedInvokedTurns] > c[tcAdvertisedTurns] || c[tcAdvertisedInvokedTurns] > c[tcInvokedTurns] {
		return malformed("%s: advertised-and-invoked turns exceed their bounds", name)
	}
	if c[tcRecoveredIdentityTurns] > c[tcFailureIdentityTurns] || c[tcRecoveredCalls] > c[tcRetries] {
		return malformed("%s: recovery counters exceed their bounds", name)
	}
	if c[tcInvokedTurns] > c[tcAttempts] {
		return malformed("%s: invoked turns exceed attempts", name)
	}
	if total, ok := sliceTotal(r.latency.counts); !ok || total != c[tcExecuted] {
		return malformed("%s: latency histogram total differs from executed", name)
	}
	classes, ok := sliceTotal(r.classes[:])
	var failures checkedSum
	failures.add(c[tcFailed])
	failures.add(rejected.v)
	if !ok || failures.overflow || classes != failures.v {
		return malformed("%s: error classes differ from failed + rejected", name)
	}
	var codeTotal checkedSum
	for id, n := range r.codes {
		if id != otherCodeID {
			if int(id) >= codeCount {
				return malformed("%s: error code %d is outside the dictionary", name, id)
			}
			if _, ok := admitted[id]; !ok {
				return malformed("%s: error code %d is not admitted for the tool", name, id)
			}
		}
		codeTotal.add(n)
	}
	if codeTotal.overflow || codeTotal.v != failures.v {
		return malformed("%s: error codes differ from failed + rejected", name)
	}

	return nil
}

func rowFromData(d *toolRowData) (*toolRow, error) {
	r := newToolRow()
	r.c = d.C
	r.classes = d.Classes
	if len(d.Latency) != len(toolLatencyEdges)+1 {
		return nil, malformed("tool latency histogram has %d bins, expected %d", len(d.Latency), len(toolLatencyEdges)+1)
	}
	copy(r.latency.counts, d.Latency)
	for _, pair := range d.Codes {
		if pair[0] > uint64(otherCodeID) {
			return nil, malformed("error code id %d is out of range", pair[0])
		}
		if r.codes == nil {
			r.codes = map[uint16]uint64{}
		}
		id := uint16(pair[0]) //nolint:gosec // range-checked above
		if _, dup := r.codes[id]; dup {
			return nil, malformed("duplicate error code entry")
		}
		r.codes[id] = pair[1]
	}

	return r, nil
}

func buildToolState(p *toolPayload) (toolState, error) {
	if err := validateDictionary("chat", p.Chats, MaxTrackedChats); err != nil {
		return toolState{}, err
	}
	if err := validateDictionary("model", p.Models, MaxTrackedModels); err != nil {
		return toolState{}, err
	}
	if err := validateDictionary("tool", p.Tools, MaxTrackedTools); err != nil {
		return toolState{}, err
	}
	if err := validateDictionary("error code", p.ErrorCodes, MaxTrackedErrorCodes); err != nil {
		return toolState{}, err
	}
	if err := validateIDs("model", p.Models, maxModelIDBytes); err != nil {
		return toolState{}, err
	}
	for _, code := range p.ErrorCodes {
		if !validErrorCode(code) {
			return toolState{}, malformed("invalid error code in the dictionary")
		}
	}

	st := initTool(p.PeriodStartedAt)
	for _, id := range p.Chats {
		st.trackedChats[id] = struct{}{}
	}
	for _, name := range p.Models {
		st.trackedModels[name] = struct{}{}
	}
	for _, name := range p.Tools {
		st.trackedTools[name] = struct{}{}
	}
	for i, code := range p.ErrorCodes {
		st.codeNames = append(st.codeNames, code)
		st.codeIDs[code] = uint16(i) //nolint:gosec // bounded by MaxTrackedErrorCodes
	}
	st.oversizedIDFolds = p.OversizedIDFolds

	for _, entry := range p.Admission {
		key, err := toolKeyOf(p, entry.Tool)
		if err != nil {
			return toolState{}, err
		}
		if _, dup := st.admission[key]; dup {
			return toolState{}, malformed("duplicate admission entry")
		}
		if len(entry.Codes) > MaxErrorCodesPerTool {
			return toolState{}, malformed("admission set exceeds %d codes", MaxErrorCodesPerTool)
		}
		set := map[uint16]struct{}{}
		for _, id := range entry.Codes {
			if id < 0 || id >= len(p.ErrorCodes) {
				return toolState{}, malformed("admitted error code %d is outside the dictionary", id)
			}
			set[uint16(id)] = struct{}{} //nolint:gosec // range-checked above
		}
		if len(set) != len(entry.Codes) {
			return toolState{}, malformed("duplicate admitted error code")
		}
		st.admission[key] = set
	}

	chatOf := func(ref int) (chatKey, error) {
		switch {
		case ref == overflowRef:
			return chatKey{overflow: true}, nil
		case ref >= 0 && ref < len(p.Chats):
			return chatKey{id: p.Chats[ref]}, nil
		}

		return chatKey{}, malformed("chat reference %d is out of range", ref)
	}
	modelOf := func(ref int) (modelKey, error) { return modelKeyOf(p.Models, ref) }

	// Per-(tool, mode) sums of the two projections must agree.
	type toolModeKey struct {
		tool toolKey
		mode ToolMode
	}
	modelSums := map[toolModeKey]*toolRow{}
	chatSums := map[toolModeKey]*toolRow{}

	for i := range p.ByModel {
		e := &p.ByModel[i]
		model, err := modelOf(e.Model)
		if err != nil {
			return toolState{}, err
		}
		mode, err := toolModeOf(e.Mode)
		if err != nil {
			return toolState{}, err
		}
		tool, err := toolKeyOf(p, e.Tool)
		if err != nil {
			return toolState{}, err
		}
		row, err := rowFromData(&e.toolRowData)
		if err != nil {
			return toolState{}, err
		}
		if err := validateToolRow("by-model row", row, st.admission[tool], len(p.ErrorCodes)); err != nil {
			return toolState{}, err
		}
		key := toolModelKey{model: model, mode: mode, tool: tool}
		if _, dup := st.byModel[key]; dup {
			return toolState{}, malformed("duplicate by-model row")
		}
		st.byModel[key] = row
		sum := modelSums[toolModeKey{tool, mode}]
		if sum == nil {
			sum = newToolRow()
			modelSums[toolModeKey{tool, mode}] = sum
		}
		sum.add(row)
	}
	for i := range p.ByChat {
		e := &p.ByChat[i]
		chat, err := chatOf(e.Chat)
		if err != nil {
			return toolState{}, err
		}
		mode, err := toolModeOf(e.Mode)
		if err != nil {
			return toolState{}, err
		}
		tool, err := toolKeyOf(p, e.Tool)
		if err != nil {
			return toolState{}, err
		}
		row, err := rowFromData(&e.toolRowData)
		if err != nil {
			return toolState{}, err
		}
		if err := validateToolRow("by-chat row", row, st.admission[tool], len(p.ErrorCodes)); err != nil {
			return toolState{}, err
		}
		key := toolChatKey{chat: chat, mode: mode, tool: tool}
		if _, dup := st.byChat[key]; dup {
			return toolState{}, malformed("duplicate by-chat row")
		}
		st.byChat[key] = row
		sum := chatSums[toolModeKey{tool, mode}]
		if sum == nil {
			sum = newToolRow()
			chatSums[toolModeKey{tool, mode}] = sum
		}
		sum.add(row)
	}
	if len(modelSums) != len(chatSums) {
		return toolState{}, malformed("the model and chat projections cover different tools")
	}
	for key, a := range modelSums {
		b, ok := chatSums[key]
		if !ok || !toolRowsEqual(a, b) {
			return toolState{}, malformed("the model and chat projections disagree for a tool")
		}
	}

	var modelTurns, chatTurns [toolModeCount]toolTurnCounters
	for i := range p.TurnsModel {
		e := &p.TurnsModel[i]
		model, err := modelOf(e.Model)
		if err != nil {
			return toolState{}, err
		}
		mode, err := toolModeOf(e.Mode)
		if err != nil {
			return toolState{}, err
		}
		if err := validateToolTurns(&e.toolTurnData); err != nil {
			return toolState{}, err
		}
		key := toolTurnModelKey{model: model, mode: mode}
		if _, dup := st.turnsByModel[key]; dup {
			return toolState{}, malformed("duplicate model turn counters")
		}
		counters := toolTurnCounters{turns: e.Turns, withCalls: e.WithCalls, failures: e.Failures}
		st.turnsByModel[key] = &counters
		modelTurns[mode].add(&counters)
	}
	for i := range p.TurnsChat {
		e := &p.TurnsChat[i]
		chat, err := chatOf(e.Chat)
		if err != nil {
			return toolState{}, err
		}
		mode, err := toolModeOf(e.Mode)
		if err != nil {
			return toolState{}, err
		}
		if err := validateToolTurns(&e.toolTurnData); err != nil {
			return toolState{}, err
		}
		key := toolTurnChatKey{chat: chat, mode: mode}
		if _, dup := st.turnsByChat[key]; dup {
			return toolState{}, malformed("duplicate chat turn counters")
		}
		counters := toolTurnCounters{turns: e.Turns, withCalls: e.WithCalls, failures: e.Failures}
		st.turnsByChat[key] = &counters
		chatTurns[mode].add(&counters)
	}
	if modelTurns != chatTurns {
		return toolState{}, malformed("the model and chat turn counters disagree")
	}

	return st, nil
}

func validateToolTurns(d *toolTurnData) error {
	failures, ok := sliceTotal(d.Failures[:])
	if !ok || d.WithCalls > d.Turns || failures > d.Turns {
		return malformed("turn counters are inconsistent")
	}

	return nil
}

func toolRowsEqual(a, b *toolRow) bool {
	if a.c != b.c || a.classes != b.classes || len(a.codes) != len(b.codes) {
		return false
	}
	for id, n := range a.codes {
		if b.codes[id] != n {
			return false
		}
	}
	for i := range a.latency.counts {
		if a.latency.counts[i] != b.latency.counts[i] {
			return false
		}
	}

	return true
}
