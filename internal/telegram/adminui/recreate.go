package adminui

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	t "github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	core "telegram-ollama-reply-bot/internal/adminui"
	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/support/safeerr"
)

// menuRecreateQuietWindow is the debounce: the menu is re-created once chat
// activity stays quiet for this long, so a burst of messages costs one
// re-creation instead of one per message.
const menuRecreateQuietWindow = 1500 * time.Millisecond

// menuRecreateSendTimeout bounds one re-creation's Telegram IO (send, delete,
// adopt) on the worker's own context.
const menuRecreateSendTimeout = 15 * time.Second

// menuRecreator is the adapter's debounce worker for menu re-creation. It
// owns all timer state on its single goroutine; Telegram IO happens on the
// AfterFunc goroutines only through the bounded `due` hand-off, so no locks
// are needed. The core (internal/adminui) stays Telegram-free and
// timer-free: this is the only place the two meet.
type menuRecreator struct {
	adapter *Adapter
	quiet   time.Duration

	events chan activity // MessageSent chat activity (buffered; overflow drops)
	due    chan dueMenu  // fired debounce timers, serialized back to the worker
	stop   chan struct{}
	done   chan struct{}

	// workerCtx bounds all menu IO of the worker and is canceled when the
	// watcher stops. The worker runs re-creations synchronously on its loop
	// goroutine, so an in-flight stalled menu send would otherwise hold the
	// stop function — and with it dispatcher shutdown and the deferred
	// tool/store cleanup — for the whole send timeout, which can exceed the
	// deployment's stop grace (Docker's default 10s vs the 15s send timeout).
	workerCtx context.Context

	// lastMsg records each admin chat's latest MessageSent timestamp, updated
	// synchronously in the subscriber before the event enters the queue. A
	// fired expiry consults it at fire time: activity newer than the
	// schedule's anchor re-anchors the window instead of re-creating — even
	// when the event itself is still in flight behind the dispatcher
	// goroutine and the queue is empty.
	lastMsg map[int64]time.Time
	lastMu  sync.Mutex

	// gen is the next schedule generation; touched only on the worker
	// goroutine. Every re-arm supersedes the previous schedule, so a fired
	// notification of an older generation is recognized and dropped instead
	// of triggering a re-creation outside the new quiet window.
	gen uint64
}

// activity is one chat-activity notification: the admin chat and the message
// time the event carries.
type activity struct {
	chatID int64
	at     time.Time
}

// armedMenu is one armed debounce schedule: the timer, the generation it was
// armed with, and the activity anchor it covers — the window starts at the
// anchor, so any message newer than it belongs to a later window.
type armedMenu struct {
	timer  *time.Timer
	gen    uint64
	anchor time.Time
}

// dueMenu is a fired debounce notification handed back to the worker loop.
type dueMenu struct {
	chatID int64
	gen    uint64
}

// noteActivity records one message's time as the chat's latest activity.
// Runs in the subscriber before the event is queued, so the timestamp is
// visible to a fire-time check even while the event itself is still in
// flight.
func (r *menuRecreator) noteActivity(chatID int64, at time.Time) {
	r.lastMu.Lock()
	r.lastMsg[chatID] = at
	r.lastMu.Unlock()
}

// latestActivity reports the chat's latest recorded message time, or the
// zero time when none was recorded.
func (r *menuRecreator) latestActivity(chatID int64) time.Time {
	r.lastMu.Lock()
	defer r.lastMu.Unlock()

	return r.lastMsg[chatID]
}

// StartWatching subscribes the adapter to execution events and starts the
// debounce worker. The worker's menu IO is bound to ctx: its cancellation
// must reach the worker even while the bot's handlers are still being
// joined — a stalled re-creation send holds the render gate, and a handler
// waiting on that gate (and with it the bot's deferred handler join) cannot
// be interrupted by canceling the handler's own context. Binding the worker
// to the runtime context aborts the stalled send and frees the gate the
// moment shutdown begins, instead of after the send timeout. The returned
// stop function unsubscribes and joins the worker; call it before the
// dispatcher stops at shutdown.
func (a *Adapter) StartWatching(ctx context.Context, dispatcher execution.Subscriber) func() {
	return a.startWatching(ctx, dispatcher, menuRecreateQuietWindow)
}

func (a *Adapter) startWatching(parent context.Context, dispatcher execution.Subscriber, quiet time.Duration) func() {
	workerCtx, cancelWorker := context.WithCancel(parent)
	r := &menuRecreator{
		adapter:   a,
		quiet:     quiet,
		events:    make(chan activity, 64),
		due:       make(chan dueMenu, 64),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		lastMsg:   make(map[int64]time.Time),
		workerCtx: workerCtx,
	}

	// The reopen wake: the /admin handler hands chat IDs straight to the
	// worker when a reopen resurfaced a menu its own ingress event can no
	// longer cover (see Adapter.notifyRecreate). Same buffered queue, same
	// best-effort drop contract as the dispatcher path.
	a.wakeMu.Lock()
	a.wake = func(chatID int64) {
		r.noteActivity(chatID, time.Now())
		select {
		case r.events <- activity{chatID: chatID, at: time.Now()}:
		default:
		}
	}
	a.wakeMu.Unlock()

	unsubscribe := dispatcher.Subscribe(execution.HandlerFunc(func(_ context.Context, event execution.Event) {
		sent, ok := event.(execution.MessageSent)
		if !ok {
			return
		}
		chatID := execution.RunContextOf(sent).ChatID
		if !slices.Contains(a.adminIDs, chatID) {
			// Admin DMs carry the admin's user ID as the chat ID; everything
			// else (groups, non-admin private chats) cannot hold a menu.
			return
		}

		// Record the message time synchronously, before the event is queued:
		// a fire-time expiry check reads this map, so the timestamp of an
		// event still in flight behind this goroutine is already accounted
		// for.
		r.noteActivity(chatID, sent.At)

		select {
		case r.events <- activity{chatID: chatID, at: sent.At}:
		default:
			// Best-effort by contract: a flooded worker drops the event, but
			// its timestamp is already recorded — a pending schedule re-arms
			// for it at fire time instead of re-creating under it.
		}
	}))

	go r.run()

	var once sync.Once

	return func() {
		once.Do(func() {
			// Cancel in-flight menu IO before joining: the worker executes
			// re-creations on its own loop goroutine, so a stalled send or
			// delete would otherwise keep it from reaching the stop select
			// for the whole send timeout.
			cancelWorker()
			unsubscribe()
			close(r.stop)
		})
		<-r.done
	}
}

// run is the worker loop: events (re)arm the per-admin debounce schedule,
// fired schedules trigger the re-creation. All timer and map access stays
// here.
func (r *menuRecreator) run() {
	defer close(r.done)

	timers := make(map[int64]*armedMenu)

	// arm schedules (or restarts) the quiet window of one chat, anchored at
	// the activity time the window covers. Every arm is a fresh generation:
	// the previous timer is stopped and its replacement carries the next gen,
	// so an already-fired (or in-flight) notification of the old deadline is
	// invalidated instead of landing when the new window elapses. A bare
	// timer.Reset cannot do this — a timer whose AfterFunc already ran cannot
	// be revived, and its queued notification would still fire the
	// re-creation for the superseded expiry.
	arm := func(chatID int64, anchor time.Time) {
		if !r.adapter.canRecreate(chatID) {
			return
		}

		r.gen++
		gen := r.gen
		if previous, ok := timers[chatID]; ok {
			previous.timer.Stop()
		}
		timers[chatID] = &armedMenu{
			gen:    gen,
			anchor: anchor,
			timer: time.AfterFunc(r.quiet, func() {
				select {
				case r.due <- dueMenu{chatID: chatID, gen: gen}:
				case <-r.stop:
				}
			}),
		}
	}

	for {
		select {
		case <-r.stop:
			for _, armed := range timers {
				armed.timer.Stop()
			}

			return
		case e := <-r.events:
			if armed, ok := timers[e.chatID]; ok && !e.at.After(armed.anchor) {
				// The message predates the current window's anchor — an old
				// or duplicated delivery. The armed schedule already covers
				// it; re-anchoring would drag the deadline backwards.
				continue
			}
			arm(e.chatID, e.at)
		case fired := <-r.due:
			armed, ok := timers[fired.chatID]
			if !ok || armed.gen != fired.gen {
				// Superseded: newer activity restarted the window while this
				// notification of the old deadline was already in flight.
				continue
			}

			// Activity recorded after this schedule was armed belongs to a
			// later window: its event may still sit in the queues (or be
			// dropped under backpressure), so re-anchor from the recorded
			// timestamp instead of re-creating under live activity. The
			// recording happens in the subscriber before the event is queued,
			// so any event already handed to a subscriber is accounted for
			// here.
			if last := r.latestActivity(fired.chatID); last.After(armed.anchor) {
				arm(fired.chatID, last)

				continue
			}
			delete(timers, fired.chatID)
			if r.adapter.inputPending(fired.chatID) {
				// The input still owns the bottom of the chat — and arm-time
				// pending state is stale by construction, so the check
				// belongs here. Keep the activity alive: the menu must
				// surface once the input resolves even if no further message
				// arrives (e.g. /admin consumed its event before its handler
				// cleared the pending).
				arm(fired.chatID, time.Now())

				continue
			}
			if r.adapter.recreate(r.workerCtx, fired.chatID) {
				// The render gate saw a pending input committed after the
				// inputPending check above — the gate view is authoritative.
				// Resolving the input through inline navigation publishes no
				// chat activity, so a dropped schedule would bury the menu
				// forever; keep it alive exactly like the check above.
				arm(fired.chatID, time.Now())
			}
		}
	}
}

// canRecreate is the arm-time gate: only the API client must exist. Session
// state is never validated here — at arm time it is stale by construction:
// the event may precede the very handler that creates or replaces the
// session. A pending input resolving before the window elapses loses its
// event forever (the fire-time re-arm covers that), and /admin after a
// session expiry edits the buried menu in place — since admin-UI sends
// publish no events, the reopen activity must survive until the replacement
// exists. Everything is validated at fire time instead: pending inputs by
// inputPending, session liveness by the core (RecreateRender — gone,
// detached, and expired sessions are silent skips).
func (a *Adapter) canRecreate(int64) bool {
	return a.api != nil
}

// inputPending is the fire-time gate for a re-creation: while the admin's
// live session awaits an input (ForceReply prompt or chat picker), the
// bottom of the chat belongs to that input, not to the menu.
func (a *Adapter) inputPending(chatID int64) bool {
	session, live := a.manager.ActiveSession(chatID)

	return live && session.HasPendingInput()
}

// recreate performs one menu re-creation and reports whether the core
// suppressed it because a pending input owns the bottom of the chat. All
// other suppression and liveness outcomes are quiet: gone, detached, and
// expired sessions are silent skips, and shutdown cancellation must not
// masquerade as a transport failure.
func (a *Adapter) recreate(ctx context.Context, chatID int64) bool {
	ctx, cancel := context.WithTimeout(ctx, menuRecreateSendTimeout)
	defer cancel()

	err := a.controller.RecreateMenu(ctx, chatID, func(render *core.Render, session *core.Session) error {
		return a.resendMenu(ctx, chatID, render, session)
	})
	switch {
	case err == nil:
		return false
	case errors.Is(err, core.ErrRecreateSuppressed):
		return true
	case errors.Is(err, core.ErrSessionGone), errors.Is(err, core.ErrDetached), errors.Is(err, core.ErrSessionExpired):
		a.logger.Debug("admin ui menu recreate skipped", "admin_id", chatID, "error", err)
	case errors.Is(err, context.Canceled):
		// The watcher was stopped mid-send (shutdown): quiet by design.
		a.logger.Debug("admin ui menu recreate aborted by shutdown", "admin_id", chatID)
	default:
		// Transport failures can embed the token-bearing API URL — log the
		// sanitized kind only.
		a.logger.Warn("admin ui menu recreate failed", "admin_id", chatID,
			"error", safeerr.WrapTransport(errAdminScreenUpdate, err))
	}

	return false
}

// resendMenu sends the fresh menu message, adopts its ID onto the committing
// session, and deletes the buried one (best-effort — a Telegram-side delete
// failure leaves a harmless stale copy whose callbacks are refused as
// stale). Adoption precedes the delete: the fresh menu is already visible,
// and callbacks on it are validated against the session's message ID —
// deferring adoption behind the best-effort delete would bounce every press
// on the new menu as "no longer active" until the delete finished or the
// shared re-creation timeout expired. Runs inside the manager's render gate.
func (a *Adapter) resendMenu(ctx context.Context, adminID int64, render *core.Render, session *core.Session) error {
	params := tu.Message(tu.ID(adminID), render.Built.Text)
	params.ReplyMarkup = inlineKeyboard(render.Built)
	message, err := a.api.SendMessage(ctx, params)
	if err != nil {
		return err
	}

	a.manager.AdoptMsgID(session, message.MessageID)

	if render.MsgID != 0 {
		if delErr := a.api.DeleteMessage(ctx, &t.DeleteMessageParams{ChatID: tu.ID(adminID), MessageID: render.MsgID}); delErr != nil {
			a.logger.Debug("admin ui old menu delete failed", "admin_id", adminID, "message_id", render.MsgID,
				"error", safeerr.WrapTransport(errAdminMessageCleanup, delErr))
		}
	}

	return nil
}
