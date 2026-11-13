package bot

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/support/safeerr"
	"telegram-ollama-reply-bot/internal/tooluse"

	t "github.com/mymmrac/telego"
	telegoapi "github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"
)

// Preview wire policy. All values are overridable per manager so tests can
// shorten them; the shape stays identical between tests and production.
type previewTimings struct {
	// debounce is the initial silence window: a run that finishes inside it
	// never creates a preview at all.
	debounce time.Duration
	// minInterval is the minimum spacing between a run's wire updates.
	minInterval time.Duration
	// flushFloor is the earliest delay a state-transition flush may use.
	flushFloor time.Duration
	// draftRefresh re-sends the private draft before its ~30s lifetime ends.
	draftRefresh time.Duration
	// ratePad is added on top of a 429 retry_after when backing off.
	ratePad time.Duration
}

func defaultPreviewTimings() previewTimings {
	return previewTimings{
		debounce:     1500 * time.Millisecond,
		minInterval:  2500 * time.Millisecond,
		flushFloor:   1000 * time.Millisecond,
		draftRefresh: 25 * time.Second,
		ratePad:      time.Second,
	}
}

// ErrDeliveryRateLimited reports that the chat's rate-limit window was
// still open when the bounded final-delivery wait expired, so the
// completed answer was not sent. It wraps context.DeadlineExceeded: the
// wait always ends through the delivery budget or the caller's deadline.
var ErrDeliveryRateLimited = fmt.Errorf("chat rate-limit window still open: %w", context.DeadlineExceeded)

const (
	// previewMaxToolLines caps the rendered tool lines; older entries are
	// compacted into an "and N earlier" line.
	previewMaxToolLines = 20
	// previewCallTimeout bounds one preview wire call so run finalization
	// always rejoins the handler goroutine in bounded time.
	previewCallTimeout = 5 * time.Second
	// previewDeleteTimeout bounds the best-effort delete after delivery.
	previewDeleteTimeout = 3 * time.Second
	// previewStopJoinTimeout bounds shutdown's wait for worker goroutine
	// exit.
	previewStopJoinTimeout = 3 * time.Second
	// previewFinalizeJoinTimeout bounds lifecycle finalization's wait for
	// worker exit. It exceeds previewCallTimeout so a wire call already in
	// flight when termination was requested is guaranteed resolved: after
	// finalization returns, no preview write of this run can still be in
	// flight and land after the final answer.
	previewFinalizeJoinTimeout = previewCallTimeout + 500*time.Millisecond
	// previewFinalDeliveryMaxWait bounds the whole rate-limited redelivery
	// of one final answer (gate waits across retries included) beyond the
	// caller's context. It comfortably exceeds the largest retry_after the
	// Bot API hands out for ordinary chat flood control, so a known backoff
	// is waited out instead of abandoned, while a pathological gate can
	// never hold delivery forever.
	previewFinalDeliveryMaxWait = 2 * time.Minute
	// previewFinalDeliveryRetries caps how many times authoritative
	// delivery may absorb a 429 into the shared chat gate and redeliver the
	// same completed answer after the gate reopens.
	previewFinalDeliveryRetries = 2
	// previewMsgsPerChatCap bounds the per-chat registry of preview message
	// IDs used to keep their presentation text out of conversational
	// context (aborted previews leave their messages, and their IDs, alive).
	previewMsgsPerChatCap = 64
	// previewMsgsChatCap bounds how many chats retain preview identities.
	// Chats beyond it lose their tombstones FIFO (live previews are never
	// evicted): per-chat IDs are bounded by previewMsgsPerChatCap, so the
	// registry as a whole stays bounded on long-lived bots.
	previewMsgsChatCap = 4096
	// previewMaxFailures disables updates for a run after this many
	// consecutive non-rate-limit wire failures (group previews).
	previewMaxFailures = 3
)

// RunInfo describes one conversation run's preview parameters. Mode comes
// from the resolved chat configuration; RunID correlates the preview with
// the execution events of the same run.
type RunInfo struct {
	RunID            string
	ChatID           int64
	TopicID          int
	TriggerMessageID int
	ChatType         string
	Mode             adminconfig.PreviewMode
	KeepToolHistory  bool
}

// previewManager owns the live preview runs. It implements
// execution.Handler: event handling is pure state update plus a non-blocking
// worker signal — never Telegram I/O, because it runs on a dispatcher
// delivery goroutine. All Telegram rendering happens on per-run worker
// goroutines whose contexts derive from the bot's base context, so a worker
// outlives the LLM run context and dies only at explicit lifecycle close or
// shutdown.
type previewManager struct {
	baseCtx context.Context
	api     *t.Bot
	logger  *slog.Logger
	typing  func(ctx context.Context, chatID t.ChatID)
	timings previewTimings

	mu        sync.Mutex
	runs      map[string]*previewRun
	chatGates map[int64]*chatGate
	// previewMsgs holds chat -> preview message IDs, live and deleted
	// (tombstone) alike, so their presentation text stays out of history.
	previewMsgs map[int64]map[int]struct{}
	// previewMsgsOrder records first-registration order of the chats in
	// previewMsgs; each chat appears at most once. It drives FIFO eviction
	// once previewMsgsChatCap chats are held.
	previewMsgsOrder []int64
	closed           bool
	// unsubscribe detaches the manager from the dispatcher at Stop; nil when
	// the manager was constructed without one (tests).
	unsubscribe func()
}

// chatGate paces wire calls per chat. Every preview wire call of every run
// in the chat waits for it, so a 429 on one run backs off all concurrent
// runs in that chat without serializing their LLM or tool execution. An
// unexpired backoff survives the chat's last run exiting, so a new request
// in the same chat cannot send straight into the outstanding retry window.
type chatGate struct {
	earliest time.Time
	runs     int // active runs in this chat (gate lifetime refcount)
}

// newPreviewManager constructs the manager. typing reuses the bot's typing
// indicator loop for the private-chat draft-failure fallback.
func newPreviewManager(baseCtx context.Context, api *t.Bot, logger *slog.Logger, typing func(context.Context, t.ChatID)) *previewManager {
	if logger == nil {
		logger = slog.Default()
	}

	return &previewManager{
		baseCtx:     baseCtx,
		api:         api,
		logger:      logger,
		typing:      typing,
		timings:     defaultPreviewTimings(),
		runs:        make(map[string]*previewRun),
		chatGates:   make(map[int64]*chatGate),
		previewMsgs: make(map[int64]map[int]struct{}),
	}
}

// RunFor starts a preview run for the given run parameters, or returns nil
// when previews are inactive (manager absent, mode off, or manager
// stopped). All *previewRun methods are nil-receiver-safe, so callers never
// need to branch on the result.
func (m *previewManager) RunFor(info RunInfo) *previewRun {
	if m == nil || info.Mode == adminconfig.PreviewModeOff || info.ChatID == 0 {
		return nil
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()

		return nil
	}
	m.sweepExpiredGatesLocked(time.Now())
	if info.RunID == "" {
		info.RunID = execution.NewRunID()
	}
	run := newPreviewRun(m, info)
	m.runs[info.RunID] = run
	gate := m.chatGates[info.ChatID]
	if gate == nil {
		gate = &chatGate{}
		m.chatGates[info.ChatID] = gate
	}
	gate.runs++
	m.mu.Unlock()

	go run.worker(m.baseCtx)

	return run
}

// HandleExecutionEvent implements execution.Handler. Unknown or already
// finalized runs are dropped silently — events never resurrect state.
func (m *previewManager) HandleExecutionEvent(_ context.Context, event execution.Event) {
	if m == nil {
		return
	}

	runID := execution.RunContextOf(event).RunID
	m.mu.Lock()
	run := m.runs[runID]
	m.mu.Unlock()
	if run == nil {
		return
	}

	run.applyEvent(event)
}

// Stop signals every active worker, waits bounded for their exit, stops any
// typing fallbacks, and clears all state. Stop is idempotent.
func (m *previewManager) Stop() {
	if m == nil {
		return
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()

		return
	}
	m.closed = true
	runs := make([]*previewRun, 0, len(m.runs))
	for _, run := range m.runs {
		runs = append(runs, run)
	}
	unsubscribe := m.unsubscribe
	m.mu.Unlock()

	if unsubscribe != nil {
		unsubscribe()
	}
	for _, run := range runs {
		run.requestTerminate()
	}
	deadline := time.After(previewStopJoinTimeout)
	for _, run := range runs {
		select {
		case <-run.done:
		case <-deadline:
			m.logger.Warn("preview worker did not stop in time", "run_id", run.info.RunID)
			deadline = make(chan time.Time) // stop waiting on further workers
		}
	}
	for _, run := range runs {
		run.stopTypingFallback()
	}

	m.mu.Lock()
	clear(m.runs)
	clear(m.chatGates)
	clear(m.previewMsgs)
	m.previewMsgsOrder = nil
	m.mu.Unlock()
}

// notePreviewMessage registers a group preview message so ingress can
// recognize replies to it and keep its presentation text out of the
// conversational context (history, prompts, summaries). The registry holds
// live previews and tombstones of deleted ones alike: quoted snapshots of a
// deleted preview still carry its text.
func (m *previewManager) notePreviewMessage(chatID int64, msgID int) {
	if m == nil || msgID == 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.previewMsgs[chatID]
	if ids == nil {
		ids = make(map[int]struct{})
		m.previewMsgs[chatID] = ids
		m.previewMsgsOrder = append(m.previewMsgsOrder, chatID)
		m.evictPreviewIdentitiesLocked()
	}
	if len(ids) >= previewMsgsPerChatCap {
		for k := range ids {
			delete(ids, k)

			break
		}
	}
	ids[msgID] = struct{}{}
}

// evictPreviewIdentitiesLocked keeps the identity registry bounded across
// chats: once more chats hold identities than previewMsgsChatCap, the
// oldest chats lose their entries FIFO. Chats with an active run are
// skipped, never evicted — their previews are still live on the wire — so
// the bound can be exceeded by at most the number of concurrently active
// runs; finishing runs reclaim the overflow from unregister. Eviction
// reopens the quoted-snapshot exclusion only for the evicted chat's long
// past previews; per-chat Telegram message IDs are never reused, so it can
// never suppress an unrelated message. The caller holds m.mu.
func (m *previewManager) evictPreviewIdentitiesLocked() {
	overflow := len(m.previewMsgsOrder) - previewMsgsChatCap
	if overflow <= 0 {
		return
	}

	kept := make([]int64, 0, len(m.previewMsgsOrder))
	for _, chatID := range m.previewMsgsOrder {
		if overflow > 0 && !m.chatHasActiveRunLocked(chatID) {
			delete(m.previewMsgs, chatID)
			overflow--

			continue
		}
		kept = append(kept, chatID)
	}
	m.previewMsgsOrder = kept
}

// chatHasActiveRunLocked reports whether any registered run belongs to the
// chat. Registered runs are few (they are the concurrently executing
// requests), so the scan is cheap. The caller holds m.mu.
func (m *previewManager) chatHasActiveRunLocked(chatID int64) bool {
	for _, run := range m.runs {
		if run.info.ChatID == chatID {
			return true
		}
	}

	return false
}

// isPreviewMessage reports whether the message is a live preview or a
// deleted one (tombstone): both kinds must stay excluded from context.
// Nil managers and unknown IDs return false.
func (m *previewManager) isPreviewMessage(chatID int64, msgID int) bool {
	if m == nil || msgID == 0 {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.previewMsgs[chatID]
	_, ok := ids[msgID]

	return ok
}

// sweepExpiredGatesLocked drops gates left behind by chats whose last run
// exited during a backoff and whose backoff has since expired. It runs
// wherever the manager already mutates gate state, so a chat that never
// runs again cannot leave its entry behind forever: every new run anywhere
// reclaims expired gates. The caller must hold m.mu.
func (m *previewManager) sweepExpiredGatesLocked(now time.Time) {
	for chatID, gate := range m.chatGates {
		if gate.runs <= 0 && !gate.earliest.After(now) {
			delete(m.chatGates, chatID)
		}
	}
}

// gateEarliest returns the chat's earliest allowed wire-call time. An
// emptied gate whose backoff has expired is dropped here, so gates retained
// past their chat's last run (see unregister) do not linger forever.
func (m *previewManager) gateEarliest(chatID int64) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	gate := m.chatGates[chatID]
	if gate == nil {
		return time.Time{}
	}
	if gate.runs <= 0 && !gate.earliest.After(time.Now()) {
		delete(m.chatGates, chatID)

		return time.Time{}
	}

	return gate.earliest
}

// backoffChat advances the chat gate after a 429 so every concurrent run in
// the chat backs off together.
func (m *previewManager) backoffChat(chatID int64, wait time.Duration) {
	until := time.Now().Add(wait)
	m.mu.Lock()
	defer m.mu.Unlock()
	gate := m.chatGates[chatID]
	if gate == nil {
		gate = &chatGate{}
		m.chatGates[chatID] = gate
	}
	if until.After(gate.earliest) {
		gate.earliest = until
	}
}

// unregister removes a finalized run after its worker exited and drops the
// chat gate when the chat's last run is gone — unless a rate-limit backoff
// is still outstanding, which survives so a new run in that chat respects
// it. Missing entries are no-ops (manager Stop may already have cleared
// them).
func (m *previewManager) unregister(run *previewRun) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.runs, run.info.RunID)
	if gate := m.chatGates[run.info.ChatID]; gate != nil {
		gate.runs--
		if gate.runs <= 0 && !gate.earliest.After(time.Now()) {
			delete(m.chatGates, run.info.ChatID)
		}
	}
	// A finishing run frees its chat for identity eviction: overflow held
	// back only by active runs is reclaimed here, not just on new chats.
	m.evictPreviewIdentitiesLocked()
}

// previewRun is one run's live preview: accumulated tool state, a worker
// goroutine owning all wire calls, and lifecycle finalization owned by the
// request flow — never by events, so execution completion is separate from
// preview termination.
//
// Locking: r.mu guards the event-updated state (completed/active/finished/
// dirty) and the fields read by finalization goroutines (finalized,
// groupMsgID, deleted, disabled, draftID). The wire schedule (sendAt,
// lastSent, refreshDue) is owned by the worker goroutine alone.
type previewRun struct {
	manager   *previewManager
	info      RunInfo
	isPrivate bool
	draftID   int

	wake      chan struct{} // buffered 1: coalesces flush requests
	terminate chan struct{} // closed once at lifecycle close or shutdown
	termOnce  sync.Once
	done      chan struct{} // closed when the worker exited

	mu         sync.Mutex
	completed  []previewToolLine
	active     []previewActiveTool
	finished   bool
	finishedOK bool
	finalized  bool
	dirty      bool
	version    uint64 // bumped on every state change; guards dirty clearing
	failures   int
	disabled   bool
	groupMsgID int
	deleted    bool
	typingStop context.CancelFunc

	// wire schedule and last delivered text, worker-goroutine-owned.
	// firstSendAt is fixed at construction and only read by the worker.
	sendAt      time.Time
	lastSent    time.Time
	lastText    string
	refreshDue  time.Time
	firstSendAt time.Time
}

type previewToolLine struct {
	name     string
	internal bool
	ok       bool
	duration time.Duration
}

type previewActiveTool struct {
	callID   string
	name     string
	internal bool
}

func newPreviewRun(m *previewManager, info RunInfo) *previewRun {
	now := time.Now()

	return &previewRun{
		manager:   m,
		info:      info,
		isPrivate: info.ChatType == t.ChatTypePrivate,
		draftID:   newDraftID(),
		wake:      make(chan struct{}, 1),
		terminate: make(chan struct{}),
		done:      make(chan struct{}),
		// dirty starts true: the neutral indicator must go out at the
		// debounce even before the first execution event arrives, so the
		// preview covers silent preprocessing (history summarization) too.
		dirty:       true,
		sendAt:      now.Add(m.timings.debounce),
		refreshDue:  now.Add(m.timings.draftRefresh),
		firstSendAt: now.Add(m.timings.debounce),
	}
}

// ID returns the run's execution correlation ID ("" without a run).
func (r *previewRun) ID() string {
	if r == nil {
		return ""
	}

	return r.info.RunID
}

// applyEvent updates run state from one execution event and requests a
// flush. It never performs I/O and never blocks.
func (r *previewRun) applyEvent(event execution.Event) {
	r.mu.Lock()
	if r.finalized {
		r.mu.Unlock()

		return
	}
	switch e := event.(type) {
	case execution.RunStarted, execution.GenerationCompleted:
		// Rendering needs no start timestamps or generation metadata yet.
	case execution.GenerationStarted:
	case execution.ToolStarted:
		r.active = append(r.active, previewActiveTool{callID: e.CallID, name: e.ToolName, internal: e.Internal})
	case execution.ToolCompleted:
		for i, a := range r.active {
			if a.callID == e.CallID {
				r.active = append(r.active[:i], r.active[i+1:]...)

				break
			}
		}
		r.completed = append(r.completed, previewToolLine{
			name:     e.ToolName,
			internal: e.Internal,
			ok:       e.Status == execution.StatusOK,
			duration: e.Duration,
		})
	case execution.RunCompleted:
		r.finished, r.finishedOK = true, true
	case execution.RunFailed:
		r.finished = true
	case execution.TextDelta:
		// No producer in this phase; nothing to render yet.
	}
	r.dirty = true
	r.version++
	r.mu.Unlock()

	r.wakeUp()
}

func (r *previewRun) wakeUp() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Quiesce stops all preview writes and joins the worker before the final
// reply's gate check and send. Two reasons make the join mandatory for both
// chat types: a private draft still in flight could land after the answer
// and resurrect the working preview below it, and a 429 landing on any
// in-flight wire call closes the chat gate only afterwards — it must be
// harvested before the gate check, not after it. Group previews freeze at
// their last rendered state until FinalDelivered deletes them. Idempotent
// with the other lifecycle methods.
func (r *previewRun) Quiesce() {
	if r == nil {
		return
	}
	r.finalize()
}

// AwaitChatGate blocks until the chat's pacing gate is open. Nil runs
// return true immediately. See previewManager.awaitChatGate.
func (r *previewRun) AwaitChatGate(ctx context.Context) bool {
	if r == nil {
		return true
	}

	return r.manager.awaitChatGate(ctx, r.info.ChatID)
}

// awaitChatGate blocks until the chat's pacing gate is open — a preview 429
// closes it for every sender in the chat — bounded only by ctx, so callers
// that must cap the wait pass a context with a deadline. It reports whether
// the gate is open on return: false means the wait was exhausted (deadline
// reached or context cancelled) while a backoff is still outstanding, so a
// caller can react to a still-closed window instead of assuming the gate
// opened.
func (m *previewManager) awaitChatGate(ctx context.Context, chatID int64) bool {
	for {
		now := time.Now()
		earliest := m.gateEarliest(chatID)
		if !earliest.After(now) {
			return true
		}
		wait := earliest
		if dl, ok := ctx.Deadline(); ok && dl.Before(wait) {
			wait = dl
		}
		if !now.Before(wait) {
			return false
		}
		timer := time.NewTimer(wait.Sub(now))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()

			return false
		}
	}
}

// deliverFinalReply sends the authoritative answer and survives a 429:
// a rate limit recorded by a concurrent run's preview call can close the
// chat's gate between the pre-delivery gate wait and the send, and the send
// can draw a fresh 429 of its own. Either way the completed answer is not
// dropped on a predictable, recoverable rejection — the limit is absorbed
// into the shared gate, the wait honors the recorded backoff (bounded by
// previewFinalDeliveryMaxWait), and the same answer is redelivered without
// regenerating it. The chat gate is consulted through the manager even when
// this request's previews are off: an outstanding window rate-limits every
// sender in the chat, so the bounded wait runs before the first attempt
// too — a retained backoff (possibly from a request whose own run is nil)
// must not draw a predictably rejected send. An exhausted wait stops
// delivery instead of sending into the known-closed window, surfacing
// ErrDeliveryRateLimited; parent cancellation keeps its own identity. Only
// with no preview manager wired at all is delivery exactly-once with no
// gate interaction.
func (b *Bot) deliverFinalReply(ctx context.Context, message t.Message, llmReply string, calls []tooluse.ToolCallRecord, resolved adminconfig.ResolvedChatConfig) (*t.Message, error) {
	deliver := func(sendCtx context.Context) (*t.Message, error) {
		if resolved.PreviewMode != adminconfig.PreviewModeOff && resolved.KeepToolHistory {
			return b.sendReplyWithToolHistory(sendCtx, message, llmReply, calls, resolved.PreviewMode)
		}

		return b.sendRichMarkdownReply(sendCtx, message, llmReply)
	}

	mgr := b.previews
	if mgr == nil {
		return deliver(ctx)
	}

	waitCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, previewFinalDeliveryMaxWait)
		defer cancel()
	}

	// The initial send rides the bounded delivery context like every
	// retry, and never runs into a known-closed window: an exhausted
	// pre-send wait ends delivery here, the same rule the retry loop
	// lives by.
	if !mgr.awaitChatGate(waitCtx, message.Chat.ID) {
		cause := context.Cause(waitCtx)
		if cause != nil && !errors.Is(cause, context.DeadlineExceeded) {
			return nil, cause
		}

		return nil, fmt.Errorf("chat %d: %w", message.Chat.ID, ErrDeliveryRateLimited)
	}

	sent, err := deliver(waitCtx)
	for retries := 0; err != nil; retries++ {
		// Every rate-limit response is recorded into the shared gate — the
		// last rejected attempt's backoff included, so later runs in the
		// chat respect the window even once redelivery gives up.
		wait, limited := mgr.absorbDeliveryRateLimit(message.Chat.ID, err)
		if !limited {
			break
		}
		b.loggerFromContext(ctx).Warn("final delivery hit the chat rate-limit window",
			"chat_id", message.Chat.ID, "retry_in", wait.String(), "attempt", retries+1)
		if retries >= previewFinalDeliveryRetries {
			break
		}
		// An exhausted wait leaves the window known-closed: redelivering
		// then would burn the answer's attempts on a predictable 429, so
		// the recorded rate limit is returned to the caller's error path.
		if !mgr.awaitChatGate(waitCtx, message.Chat.ID) {
			break
		}
		// Redelivery rides the bounded delivery context: a stalled retry
		// request cannot outlive previewFinalDeliveryMaxWait.
		sent, err = deliver(waitCtx)
	}

	return sent, err
}

// FinalDelivered terminates the preview after the final reply was delivered
// successfully: the group preview is deleted best-effort, the private draft
// is dismissed by the final send itself.
func (r *previewRun) FinalDelivered() {
	if r == nil {
		return
	}
	r.finalize()
	if !r.isPrivate {
		r.deleteGroupPreview()
	}
}

// CompleteNoText terminates the preview after a successful run produced no
// reply text: the group preview is deleted; the private chat gets one final
// draft without the active marker. The final draft is rendered from the
// run's authoritative tool records — the event stream may still hold
// un-applied completions when finalization lands, so event state cannot be
// trusted to show every tool as finished.
func (r *previewRun) CompleteNoText(calls []tooluse.ToolCallRecord) {
	if r == nil {
		return
	}
	r.finalize()
	if !r.isPrivate {
		r.deleteGroupPreview()

		return
	}

	r.mu.Lock()
	gone := r.disabled
	draftID := r.draftID
	text := r.renderFinalText(previewDoneHeader, calls)
	r.mu.Unlock()
	if gone {
		return
	}

	// The terminal draft is a wire call like any other: sending it into an
	// outstanding chat backoff would burn it on a predictable 429 and leave
	// the stale working preview on the wire. Wait for the gate (bounded)
	// first; the worker is already joined, so the wait races nothing. A
	// wait that expires with the backoff outstanding means the window is
	// known-closed: the draft is skipped rather than sent to certain 429.
	waitCtx := r.manager.baseCtx
	if _, hasDeadline := r.manager.baseCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(r.manager.baseCtx, previewFinalDeliveryMaxWait)
		defer cancel()
	}
	if !r.AwaitChatGate(waitCtx) {
		r.manager.logger.Warn("terminal draft skipped: chat rate-limit window still open",
			"chat_id", r.info.ChatID)

		return
	}

	ctx, cancel := context.WithTimeout(r.manager.baseCtx, previewCallTimeout)
	defer cancel()
	if err := r.manager.api.SendRichMessageDraft(ctx, draftParams(r.info, draftID, text)); err != nil {
		// A fresh 429 must be retained for the chat, not merely logged —
		// this is the chat's last observed rate-limit signal. Non-rate-limit
		// failures log identically either way.
		r.handleSendError("sendRichMessageDraft", err)
	}
}

// Abort terminates the preview after a failure. The last rendered preview
// stays visible (accepted degradation); the worker stops and the run state
// is cleaned up.
func (r *previewRun) Abort() {
	if r == nil {
		return
	}
	r.finalize()
}

// Close is the deferred safety net; idempotent with the lifecycle methods.
func (r *previewRun) Close() {
	r.Abort()
}

// finalize marks the run terminated, stops the worker, waits bounded for
// its exit, and then stops a possibly running typing fallback (which must
// outlive the worker — see disableWithTypingFallback). Manager map cleanup
// happens on worker exit.
func (r *previewRun) finalize() {
	r.mu.Lock()
	already := r.finalized
	r.finalized = true
	r.mu.Unlock()
	if already {
		return
	}

	r.requestTerminate()
	select {
	case <-r.done:
	case <-time.After(previewFinalizeJoinTimeout):
		r.manager.logger.Warn("preview worker did not stop after finalization", "run_id", r.info.RunID)
	}
	r.stopTypingFallback()
}

func (r *previewRun) requestTerminate() {
	r.termOnce.Do(func() { close(r.terminate) })
}

// worker drives the run's wire calls. It exits on lifecycle termination,
// manager shutdown, or base-context cancellation, and unregisters the run
// from the manager on the way out.
func (r *previewRun) worker(ctx context.Context) {
	defer r.exit()

	delay := time.Until(r.nextSendAt())
	timer := time.NewTimer(delay)
	defer timer.Stop()
	firesAt := time.Now().Add(delay)

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.terminate:
			return
		case <-r.wake:
			// Pull the schedule earlier, never later: compare against the
			// armed timer's remaining time, not its original duration.
			if next := time.Until(r.pullSendAt()); next < time.Until(firesAt) {
				delay = next
				firesAt = time.Now().Add(delay)
				timer.Reset(delay)
			}
		case <-timer.C:
			if !r.tick(ctx) {
				return
			}
			delay = time.Until(r.nextSendAt())
			firesAt = time.Now().Add(delay)
			timer.Reset(delay)
		}
	}
}

// exit performs worker-goroutine cleanup. The typing fallback intentionally
// survives: it must keep running after the worker disabled itself (draft
// failures) until lifecycle finalization or shutdown stops it.
func (r *previewRun) exit() {
	r.manager.unregister(r)
	close(r.done)
}

// nextSendAt returns the scheduled next wire-call time (worker goroutine only).
func (r *previewRun) nextSendAt() time.Time {
	return r.sendAt
}

// pullSendAt advances the schedule to the earliest useful flush time after
// new state: at least flushFloor from now and no earlier than the previous
// send plus minInterval (worker goroutine only).
func (r *previewRun) pullSendAt() time.Time {
	now := time.Now()

	earliest := r.lastSent.Add(r.manager.timings.minInterval)
	if r.lastSent.IsZero() {
		// Before the first delivery the initial debounce is a lower bound:
		// early events (RunStarted fires as soon as execution begins) must
		// not pull the first preview earlier than it, or runs finishing
		// inside the debounce window would still flash a preview.
		earliest = r.firstSendAt
	}
	if floor := now.Add(r.manager.timings.flushFloor); floor.After(earliest) {
		earliest = floor
	}
	if earliest.Before(now) {
		earliest = now
	}
	if earliest.Before(r.sendAt) {
		r.sendAt = earliest
	}

	return r.sendAt
}

// tick renders and sends one update when due. It returns false when the
// worker should exit (disabled or finalized).
func (r *previewRun) tick(ctx context.Context) bool {
	now := time.Now()

	r.mu.Lock()
	finalized := r.finalized
	dirty := r.dirty
	r.mu.Unlock()
	if finalized {
		return false
	}

	// Chat pacing gate: a 429 on any concurrent run in this chat delays
	// this run's wire calls too.
	if gate := r.manager.gateEarliest(r.info.ChatID); gate.After(now) {
		r.sendAt = gate

		return true
	}

	refresh := r.isPrivate && !dirty && now.After(r.refreshDue)
	if !dirty && !refresh {
		r.sendAt = maxTime(now.Add(r.manager.timings.minInterval), r.refreshDue)

		return true
	}

	r.mu.Lock()
	text := r.renderText(previewWorkingHeader)
	version := r.version
	r.dirty = false // tentatively; re-set below unless the send succeeds cleanly
	r.mu.Unlock()

	if !r.isPrivate && text == r.lastText {
		// The rendered state is already on the wire (events for tools the
		// preview mode hides, no-op generation events). Skip the edit — a
		// redundant one is rejected by Telegram as "message is not modified"
		// and would burn the run's failure budget on a healthy preview.
		r.mu.Lock()
		// An event that arrived during the render must not lose its flush.
		if r.version != version {
			r.dirty = true
		}
		r.mu.Unlock()
		r.scheduleNext()

		return true
	}

	if r.sendUpdate(ctx, text) {
		r.lastText = text

		r.mu.Lock()
		// An event that arrived during the send must not lose its flush.
		if r.version != version {
			r.dirty = true
		}
		r.mu.Unlock()

		r.lastSent = time.Now()
		r.refreshDue = r.lastSent.Add(r.manager.timings.draftRefresh)
	} else {
		// Failed send: the rendered state never reached the wire — retry it
		// after the backoff. A 429 already pushed sendAt past the chat gate.
		r.mu.Lock()
		r.dirty = true
		r.mu.Unlock()

		if r.isDisabled() {
			return false
		}
	}

	r.scheduleNext()

	return true
}

// scheduleNext spaces the run's next wire call at least minInterval out
// (worker goroutine only).
func (r *previewRun) scheduleNext() {
	if floor := time.Now().Add(r.manager.timings.minInterval); r.sendAt.Before(floor) {
		r.sendAt = floor
	}
}

func (r *previewRun) isDisabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.disabled
}

// noteSendSuccess resets the consecutive-failure budget: only failures
// without a successful update in between accumulate toward disabling the
// run's previews.
func (r *previewRun) noteSendSuccess() {
	r.mu.Lock()
	r.failures = 0
	r.mu.Unlock()
}

// sendUpdate performs the wire call: create/edit for groups, a draft send
// for private chats. It returns true when the update reached the wire.
func (r *previewRun) sendUpdate(_ context.Context, text string) bool {
	callCtx, cancel := context.WithTimeout(r.manager.baseCtx, previewCallTimeout)
	defer cancel()

	if r.isPrivate {
		err := r.manager.api.SendRichMessageDraft(callCtx, draftParams(r.info, r.draftID, text))
		if err == nil {
			r.noteSendSuccess()

			return true
		}
		if r.handleSendError("sendRichMessageDraft", err) {
			return false
		}
		// First non-rate-limit draft failure disables drafts for the run;
		// the typing indicator takes over as the activity signal.
		r.disableWithTypingFallback()

		return false
	}

	msgID := r.groupMessageID()

	if msgID == 0 {
		params := &t.SendMessageParams{
			ChatID:              tu.ID(r.info.ChatID),
			Text:                text,
			DisableNotification: true,
		}
		if r.info.TopicID != 0 {
			params.MessageThreadID = r.info.TopicID
		}
		if r.info.TriggerMessageID != 0 {
			params.ReplyParameters = &t.ReplyParameters{MessageID: r.info.TriggerMessageID}
		}

		sent, err := r.manager.api.SendMessage(callCtx, params)
		if err == nil {
			if sent != nil {
				msgID := sent.MessageID
				r.mu.Lock()
				r.groupMsgID = msgID
				// Finalization may have raced this creation: the lifecycle
				// delete ran while the message ID did not exist yet and
				// latched deleted with nothing to remove. Re-arm the latch
				// so the late preview is removed now.
				late := r.deleted
				r.deleted = false
				r.mu.Unlock()
				r.manager.notePreviewMessage(r.info.ChatID, msgID)
				if late {
					r.deleteGroupPreview()
				}
			}
			r.noteSendSuccess()

			return true
		}
		if r.handleSendError("sendMessage", err) {
			return false
		}
	} else {
		_, err := r.manager.api.EditMessageText(callCtx, &t.EditMessageTextParams{
			ChatID:    tu.ID(r.info.ChatID),
			MessageID: msgID,
			Text:      text,
		})
		if err == nil || isMessageNotModified(err) {
			r.noteSendSuccess()

			return true
		}
		if r.handleSendError("editMessageText", err) {
			return false
		}
	}

	r.mu.Lock()
	r.failures++
	if r.failures >= previewMaxFailures {
		r.disabled = true
	}
	disabled := r.disabled
	r.mu.Unlock()
	if disabled {
		r.manager.logger.Warn("preview updates disabled for run after repeated failures",
			"chat_id", r.info.ChatID, "run_id", r.info.RunID)
	}

	return false
}

func (r *previewRun) groupMessageID() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.groupMsgID
}

// isRateLimit reports whether the error is a Telegram rate limit (429).
func isRateLimit(err error) bool {
	var apiErr *telegoapi.Error

	return errors.As(err, &apiErr) && apiErr.ErrorCode == 429
}

// absorbDeliveryRateLimit records a 429 — from a preview wire call or from
// an authoritative delivery send — into the shared chat gate, so every
// concurrent run in the chat backs off together. It reports whether the
// error was a rate limit; wait is the recorded backoff.
func (m *previewManager) absorbDeliveryRateLimit(chatID int64, err error) (time.Duration, bool) {
	var apiErr *telegoapi.Error
	if !errors.As(err, &apiErr) || apiErr.ErrorCode != 429 {
		return 0, false
	}

	wait := m.timings.ratePad
	if apiErr.Parameters != nil && apiErr.Parameters.RetryAfter > 0 {
		wait = time.Duration(apiErr.Parameters.RetryAfter)*time.Second + m.timings.ratePad
	}
	m.backoffChat(chatID, wait)

	return wait, true
}

// handleSendError reports one wire failure. It returns true when the error
// was a rate limit: the chat gate absorbed it (not counted against the
// run's failure budget) and the schedule moved past the gate.
func (r *previewRun) handleSendError(method string, err error) bool {
	if wait, limited := r.manager.absorbDeliveryRateLimit(r.info.ChatID, err); limited {
		r.sendAt = time.Now().Add(wait)

		return true
	}

	r.logSendFailure(method, err)

	return false
}

// isMessageNotModified reports whether the Bot API rejected the edit
// because the message already carries exactly this content: the target
// state is already on the wire, so the update counts as a successful
// no-op instead of burning the run's failure budget.
func isMessageNotModified(err error) bool {
	var apiErr *telegoapi.Error

	return errors.As(err, &apiErr) && apiErr.ErrorCode == 400 &&
		strings.Contains(apiErr.Description, "message is not modified")
}

// errPreviewSend names preview wire-call failures for logging. Raw transport
// errors are never surfaced: the request URL embeds the bot token, so causes
// are discarded via safeerr and only the typed kind and ctx sentinels remain.
var errPreviewSend = errors.New("preview wire call failed")

// logSendFailure logs a sanitized warning: method, chat, and the Bot API's
// own error classification. Never tool payloads or raw run state; the raw
// fallback branch discards the transport cause (its request URL embeds the
// bot token).
func (r *previewRun) logSendFailure(method string, err error) {
	var apiErr *telegoapi.Error
	if errors.As(err, &apiErr) {
		r.manager.logger.Warn("preview update failed",
			"method", method, "chat_id", r.info.ChatID,
			"error_code", apiErr.ErrorCode, "description", apiErr.Description)

		return
	}
	r.manager.logger.Warn("preview update failed",
		"method", method, "chat_id", r.info.ChatID,
		"error", safeerr.WrapTransport(errPreviewSend, err))
}

// disableWithTypingFallback stops updates for the run and, in private
// chats, starts the typing indicator so the chat still sees activity.
func (r *previewRun) disableWithTypingFallback() {
	r.mu.Lock()
	if r.disabled {
		r.mu.Unlock()

		return
	}
	r.disabled = true
	private := r.isPrivate
	r.mu.Unlock()

	if private {
		r.startTypingFallback()
	}
	r.manager.logger.Warn("preview disabled for run",
		"chat_id", r.info.ChatID, "run_id", r.info.RunID, "private", private)
}

func (r *previewRun) startTypingFallback() {
	r.mu.Lock()
	if r.typingStop != nil || r.manager.typing == nil {
		r.mu.Unlock()

		return
	}
	ctx, stop := context.WithCancel(r.manager.baseCtx)
	r.typingStop = stop
	typing := r.manager.typing
	chatID := tu.ID(r.info.ChatID)
	r.mu.Unlock()

	go typing(ctx, chatID)
}

func (r *previewRun) stopTypingFallback() {
	r.mu.Lock()
	stop := r.typingStop
	r.typingStop = nil
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// deleteGroupPreview removes the group preview message best-effort. Idempotent.
func (r *previewRun) deleteGroupPreview() {
	r.mu.Lock()
	msgID := r.groupMsgID
	deleted := r.deleted || msgID == 0
	r.deleted = true
	r.mu.Unlock()
	if deleted {
		return
	}

	ctx, cancel := context.WithTimeout(r.manager.baseCtx, previewDeleteTimeout)
	defer cancel()
	if err := r.manager.api.DeleteMessage(ctx, &t.DeleteMessageParams{
		ChatID:    tu.ID(r.info.ChatID),
		MessageID: msgID,
	}); err != nil {
		// Deletion failed, so the preview message stays visible: keep its
		// ID registered so replies to it stay excluded from context.
		r.logSendFailure("deleteMessage", err)

		return
	}
	// Deletion succeeded: the identity is retained as a tombstone anyway.
	// The message is gone from the wire, but Telegram snapshots that quoted
	// it — updates queued in the long-poll backlog, replies processed by
	// later history saves — still carry its preview text, and only a
	// registered ID keeps that text out of conversational context. Per-chat
	// message IDs are never reused, so a tombstone can never suppress an
	// unrelated message; the per-chat cap bounds memory.
}

// renderText builds the preview text from event state. The caller must
// hold r.mu.
func (r *previewRun) renderText(header string) string {
	var lines []string
	for _, tool := range r.completed {
		if r.info.Mode != adminconfig.PreviewModeAllTools && tool.internal {
			continue
		}
		lines = append(lines, previewToolLineText(tool, r.info.Mode, r.isPrivate))
	}
	for _, a := range r.active {
		if r.info.Mode != adminconfig.PreviewModeAllTools && a.internal {
			continue
		}
		lines = append(lines, previewRunningLine(a.name, r.isPrivate))
	}

	return assemblePreviewText(header, lines, r.isPrivate)
}

// renderFinalText builds a terminal draft from the run's authoritative tool
// records instead of event state: a completion still queued in the
// dispatcher when finalization lands would otherwise leave its tool
// rendering as forever-running. The caller must hold r.mu.
func (r *previewRun) renderFinalText(header string, calls []tooluse.ToolCallRecord) string {
	var lines []string
	for _, tool := range recordsToPreviewLines(calls) {
		if r.info.Mode != adminconfig.PreviewModeAllTools && tool.internal {
			continue
		}
		lines = append(lines, previewToolLineText(tool, r.info.Mode, r.isPrivate))
	}

	return assemblePreviewText(header, lines, r.isPrivate)
}

// recordsToPreviewLines converts authoritative tool records into preview
// tool lines.
func recordsToPreviewLines(calls []tooluse.ToolCallRecord) []previewToolLine {
	lines := make([]previewToolLine, 0, len(calls))
	for _, call := range calls {
		lines = append(lines, previewToolLine{
			name:     call.Name,
			internal: call.Visibility == tooluse.VisibilityInternal,
			ok:       call.Status == execution.StatusOK,
			duration: call.Duration,
		})
	}

	return lines
}

// assemblePreviewText joins the header and rendered tool lines into the
// final preview text, compacting the oldest lines beyond the cap.
func assemblePreviewText(header string, lines []string, private bool) string {
	var b strings.Builder
	b.WriteString(header)
	if len(lines) == 0 {
		if private {
			b.WriteString("\n\n…")
		}

		return b.String()
	}

	b.WriteString("\n\n")
	if dropped := len(lines) - previewMaxToolLines; dropped > 0 {
		lines = lines[len(lines)-previewMaxToolLines:]
		fmt.Fprintf(&b, "… and %d earlier\n", dropped)
	}
	for _, line := range lines {
		b.WriteString(line + "\n")
	}

	return strings.TrimSuffix(b.String(), "\n")
}

// previewToolLineText renders one completed tool line. Group previews are
// plain text; private drafts are rich markdown, so the dynamically supplied
// tool name goes through the markdown-safe rendering.
func previewToolLineText(tool previewToolLine, mode adminconfig.PreviewMode, rich bool) string {
	mark := "✅"
	if !tool.ok {
		mark = "❌"
	}

	return mark + " " + previewToolName(tool.name, rich) + previewDurationSuffix(tool.duration, mode)
}

func previewRunningLine(name string, rich bool) string {
	return "🔄 " + previewToolName(name, rich)
}

// previewWorkingHeader and previewDoneHeader are the exact first lines of
// rendered preview text: the neutral working indicator and the terminal
// no-text marker.
const (
	previewWorkingHeader = "🔄 Working…"
	previewDoneHeader    = "✔️ Done"
)

// isPreviewPresentation reports whether rendered text matches the live
// preview shape: the exact working/done header followed by nothing but
// tool-progress lines, or nothing but tool-progress lines alone (a
// snapshot truncated past its header). The match is structural — exact
// header strings and the name() tool-call syntax — because the check
// gates dropping quoted bot answers: an answer that merely opens with
// ✔️, carries ✅ checklist lines, or continues the "✔️ Done" line with
// real content is an ordinary answer and must be kept. Quoted snapshots
// of previews whose registry identities were evicted reach the bot-quote
// fallback in message_history.go; without this check their working
// indicator and tool lines would enter conversational context as if the
// bot had said them.
func isPreviewPresentation(text string) bool {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) == 0 {
		return false
	}

	headerSeen := false
	body := lines
	if first := strings.TrimSpace(lines[0]); first == previewWorkingHeader || first == previewDoneHeader {
		headerSeen = true
		body = lines[1:]
	}

	toolLines := 0
	for _, line := range body {
		t := strings.TrimSpace(line)
		if t == "" || t == "…" || strings.HasPrefix(t, "… and ") {
			continue
		}
		if !isPreviewToolLine(t) {
			return false
		}
		toolLines++
	}

	// A header with no tool lines is still the preview shape: an empty
	// private snapshot renders the bare header (with the "…" placeholder),
	// so the header alone identifies presentation only when nothing else
	// follows — real content after it is an ordinary answer.
	return toolLines > 0 || headerSeen
}

// isPreviewToolLine reports whether one rendered line is a preview
// tool-progress line: a status mark followed by the tool-call syntax —
// plain "name()" or rich "`name()`" — with the optional duration suffix of
// all_tools mode. Any mark-prefixed text without the call syntax (a
// checklist, an emoji-led sentence) is not a tool line.
func isPreviewToolLine(line string) bool {
	for _, mark := range []string{"✅ ", "❌ ", "🔄 "} {
		if !strings.HasPrefix(line, mark) {
			continue
		}

		rest := strings.TrimPrefix(line, mark)
		if at := strings.Index(rest, " ⌛ "); at >= 0 {
			rest = rest[:at]
		}
		if rest == "" {
			return false
		}
		if strings.HasPrefix(rest, "`") && strings.HasSuffix(rest, "()`") {
			return true
		}
		if strings.HasSuffix(rest, "()") && !strings.ContainsAny(strings.TrimSuffix(rest, "()"), " `") {
			return true
		}

		return false
	}

	return false
}

// previewToolName renders a tool name for the target format. Rich markdown
// puts registry identifiers into a code span; names that could break the
// span fall back to escaped plain text (escaping, never stripping).
func previewToolName(name string, rich bool) string {
	if !rich {
		return name + "()"
	}
	if strings.ContainsRune(name, '`') {
		return escapeRichMarkdown(name) + "()"
	}

	return "`" + name + "()`"
}

func previewDurationSuffix(d time.Duration, mode adminconfig.PreviewMode) string {
	if mode != adminconfig.PreviewModeAllTools || d <= 0 {
		return ""
	}

	return " ⌛ " + formatShortDuration(d)
}

// formatShortDuration renders durations compactly: 0.2s, 5s, 1m20s.
func formatShortDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Minute {
		if d%time.Second == 0 {
			return fmt.Sprintf("%ds", int(d.Seconds()))
		}

		return fmt.Sprintf("%.1fs", d.Seconds())
	}

	return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
}

// draftParams assembles a private-chat draft send.
func draftParams(info RunInfo, draftID int, markdownText string) *t.SendRichMessageDraftParams {
	params := &t.SendRichMessageDraftParams{
		ChatID:      info.ChatID,
		DraftID:     draftID,
		RichMessage: tu.RichMessageMarkdown(markdownText),
	}
	if info.TopicID != 0 {
		params.MessageThreadID = info.TopicID
	}

	return params
}

// newDraftID returns a positive non-zero draft identifier. Same-ID drafts
// animate their updates on the wire.
func newDraftID() int {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return int(time.Now().UnixNano()&0x7fffffff) | 1
	}

	return int(binary.LittleEndian.Uint32(buf[:])&0x7fffffff) | 1
}

// escapeRichMarkdown backslash-escapes every ASCII punctuation character
// that carries markdown structure, so dynamically supplied text renders
// literally instead of changing the document. This is real escaping (the
// characters survive), not stripping.
func escapeRichMarkdown(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if r < 128 && isRichMarkdownPunct(r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}

	return b.String()
}

func isRichMarkdownPunct(r rune) bool {
	switch {
	case r >= '!' && r <= '/': // !"#$%&'()*+,-./
		return true
	case r >= ':' && r <= '@': // :;<=>?@
		return true
	case r >= '[' && r <= '`': // [\]^_`
		return true
	case r >= '{' && r <= '~': // {|}~
		return true
	default:
		return false
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}

	return b
}
