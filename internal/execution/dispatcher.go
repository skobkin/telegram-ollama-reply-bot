package execution

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Dispatcher accepts execution events and delivers them to subscribers
// without ever blocking or failing the producer. The zero delivery
// guarantees are deliberately modest and documented here:
//
//   - Publish never blocks and never returns an error. Producers (the tool
//     runtime) stay the authoritative source of execution state regardless
//     of subscriber behavior.
//   - While a subscriber is healthy, transition events (ToolCompleted,
//     RunCompleted, RunFailed, and every other non-expendable event) are
//     delivered in publish order. Only expendable events (currently
//     TextDelta) may be dropped under backpressure.
//   - Degradation mode: if a subscriber lags so far that its bounded queue
//     holds only non-expendable events and is still full, it is unsubscribed
//     with an error log and its pending events are lost. This is an explicit
//     failure condition, not a delivery guarantee; consumers must be
//     correct without any events (e.g. the preview renderer derives its
//     final-state cleanup from the execution flow, not from events).
//
// There is no global ordering between events of unrelated concurrent runs;
// ordering is preserved per publishing goroutine (per run). Events are
// delivered on per-subscriber goroutines under the dispatcher's base
// context, so deliveries can outlive the context of the run that produced
// them. Nothing is persisted.
type Dispatcher interface {
	// Publish hands one event to every current subscriber. It is
	// non-blocking; under backpressure expendable events are dropped and
	// extreme non-expendable overflow unsubscribes the affected subscriber
	// (see the interface documentation). Publish after Stop is a no-op.
	Publish(event Event)
}

// Handler consumes execution events. Implementations must be fast and
// non-blocking: HandleExecutionEvent runs on a dispatcher delivery
// goroutine, and a slow handler causes bounded event drops. Handlers must
// never panic (a panicking handler is unsubscribed); Telegram or LLM I/O
// belongs in the consumer's own worker, not here.
type Handler interface {
	HandleExecutionEvent(ctx context.Context, event Event)
}

// HandlerFunc adapts a function to the Handler interface.
type HandlerFunc func(ctx context.Context, event Event)

// HandleExecutionEvent implements Handler.
func (f HandlerFunc) HandleExecutionEvent(ctx context.Context, event Event) {
	f(ctx, event)
}

// DispatcherConfig holds the dispatcher sizing knobs.
type DispatcherConfig struct {
	// QueueSize is the per-subscriber event queue capacity. The default
	// covers the worst-case event volume of several concurrent runs.
	QueueSize int
}

const (
	defaultQueueSize  = 256
	stopDrainTimeout  = 3 * time.Second
	stopJoinTimeout   = 2 * time.Second
	drainPollInterval = 2 * time.Millisecond
)

// NewDispatcher creates a dispatcher. Call Start before Subscribe, and Stop
// at shutdown.
func NewDispatcher(cfg DispatcherConfig, logger *slog.Logger) *dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}

	return &dispatcher{
		queueSize: cfg.QueueSize,
		subs:      make(map[*subscriber]struct{}),
		logger:    logger,
	}
}

type dispatcher struct {
	mu        sync.Mutex
	subs      map[*subscriber]struct{}
	baseCtx   context.Context
	started   bool
	stopped   bool
	queueSize int
	logger    *slog.Logger
	dropped   atomic.Int64
}

// Start arms the dispatcher with its base context. It must be called once
// before Subscribe; subscriptions before Start are ignored.
func (d *dispatcher) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		return
	}
	d.started = true
	d.baseCtx = ctx
}

// Subscribe registers a handler and starts its delivery goroutine. The
// returned func unsubscribes it (idempotent, safe to call from the handler
// itself). Subscribing before Start or after Stop returns a no-op.
func (d *dispatcher) Subscribe(handler Handler) func() {
	if handler == nil {
		return func() {}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.started || d.stopped {
		d.logger.Warn("execution event subscription ignored", "reason", "dispatcher not running")

		return func() {}
	}

	sub := newSubscriber(d.baseCtx, handler, d.queueSize)
	d.subs[sub] = struct{}{}
	go sub.run(d)

	return func() { d.unsubscribe(sub, "unsubscribed") }
}

// Stop drains pending events for up to stopDrainTimeout, then terminates all
// delivery goroutines. It is idempotent.
func (d *dispatcher) Stop() {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()

		return
	}
	d.stopped = true
	subs := make([]*subscriber, 0, len(d.subs))
	for sub := range d.subs {
		subs = append(subs, sub)
	}
	d.mu.Unlock()

	deadline := time.Now().Add(stopDrainTimeout)
	for _, sub := range subs {
		for time.Now().Before(deadline) && sub.pending() > 0 && !sub.isClosed() {
			time.Sleep(drainPollInterval)
		}
	}

	for _, sub := range subs {
		sub.beginClose()
		sub.signal()
	}
	for _, sub := range subs {
		select {
		case <-sub.done:
		case <-time.After(stopJoinTimeout):
		}
	}

	d.mu.Lock()
	clear(d.subs)
	d.mu.Unlock()

	if dropped := d.dropped.Load(); dropped > 0 {
		d.logger.Info("execution event dispatcher stopped", "dropped_events", dropped)
	}
}

// subscriberCount reports the number of live subscribers (test visibility).
func (d *dispatcher) subscriberCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return len(d.subs)
}

// Publish implements Dispatcher.
func (d *dispatcher) Publish(event Event) {
	if event == nil {
		return
	}

	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()

		return
	}
	targets := make([]*subscriber, 0, len(d.subs))
	for sub := range d.subs {
		targets = append(targets, sub)
	}
	d.mu.Unlock()

	for _, sub := range targets {
		accepted, dropped := sub.push(event)
		switch {
		case accepted:
			sub.signal()
		case dropped:
			d.dropped.Add(1)
		default:
			// The queue is full of non-expendable events: the subscriber
			// lagged beyond the documented degradation point.
			d.unsubscribe(sub, "subscriber queue overflow of non-expendable events")
		}
	}
}

// unsubscribe removes a subscriber and tears it down. It is safe to call
// from a delivery goroutine (handler panic path) because teardown never
// waits for the goroutine to exit.
func (d *dispatcher) unsubscribe(sub *subscriber, reason string) {
	d.mu.Lock()
	_, known := d.subs[sub]
	delete(d.subs, sub)
	d.mu.Unlock()
	if !known {
		return
	}

	sub.beginClose()
	sub.signal()
	d.logger.Error("execution event subscriber removed", "reason", reason)
}

// expendable reports whether an event may be dropped or coalesced under
// backpressure. Expendable events carry replaceable incremental state; every
// other event is a state transition that must not silently disappear while
// the subscriber is healthy.
func expendable(event Event) bool {
	_, isDelta := event.(TextDelta)

	return isDelta
}

type subscriber struct {
	handler Handler
	ctx     context.Context
	cancel  context.CancelFunc
	stopCh  chan struct{}
	wake    chan struct{}
	done    chan struct{}
	close   sync.Once

	mu     sync.Mutex
	ring   []Event
	head   int
	count  int
	closed bool
}

func newSubscriber(baseCtx context.Context, handler Handler, queueSize int) *subscriber {
	ctx, cancel := context.WithCancel(baseCtx)

	return &subscriber{
		handler: handler,
		ctx:     ctx,
		cancel:  cancel,
		stopCh:  make(chan struct{}),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		ring:    make([]Event, queueSize),
	}
}

// push enqueues one event. It returns (accepted, dropped): accepted events
// are queued; dropped events were expendable and discarded; a false/false
// result signals non-expendable overflow (the caller unsubscribes).
func (s *subscriber) push(event Event) (accepted, dropped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, true
	}

	if s.count == len(s.ring) {
		if expendable(event) {
			return false, true
		}
		if !s.makeRoomLocked() {
			return false, false
		}
	}

	s.ring[(s.head+s.count)%len(s.ring)] = event
	s.count++

	return true, false
}

// makeRoomLocked evicts the oldest expendable event to free one slot,
// preserving the order of the remaining events. It reports false when the
// queue holds no expendable events.
func (s *subscriber) makeRoomLocked() bool {
	kept := make([]Event, 0, len(s.ring))
	evicted := false
	for i := 0; i < s.count; i++ {
		event := s.ring[(s.head+i)%len(s.ring)]
		if !evicted && expendable(event) {
			evicted = true

			continue
		}
		kept = append(kept, event)
	}
	if !evicted {
		return false
	}

	clear(s.ring)
	copy(s.ring, kept)
	s.head = 0
	s.count = len(kept)

	return true
}

func (s *subscriber) pop() (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count == 0 {
		return nil, false
	}

	event := s.ring[s.head]
	s.ring[s.head] = nil
	s.head = (s.head + 1) % len(s.ring)
	s.count--

	return event, true
}

func (s *subscriber) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.count
}

func (s *subscriber) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.closed
}

// beginClose marks the subscriber closed and releases its context. Teardown
// is non-blocking and idempotent; the delivery goroutine exits on its next
// boundary, discarding any events still queued.
func (s *subscriber) beginClose() {
	s.close.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		s.cancel()
		close(s.stopCh)
	})
}

func (s *subscriber) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run is the delivery goroutine: it drains the queue FIFO and invokes the
// handler, exiting when the subscriber is closed or its context is done.
func (s *subscriber) run(d *dispatcher) {
	defer close(s.done)
	for {
		select {
		case <-s.wake:
		case <-s.stopCh:
			return
		case <-s.ctx.Done():
			return
		}

		for {
			event, ok := s.pop()
			if !ok {
				break
			}
			if s.isClosed() {
				return
			}
			d.deliver(s, event)
			if s.isClosed() {
				return
			}
		}
	}
}

// deliver invokes the handler once, converting a panic into an unsubscribe.
func (d *dispatcher) deliver(sub *subscriber, event Event) {
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error("execution event subscriber panicked", "panic", r)
			d.unsubscribe(sub, "handler panic")
		}
	}()

	sub.handler.HandleExecutionEvent(sub.ctx, event)
}
