package execution

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testDispatcher(t *testing.T, queueSize int) *dispatcher {
	t.Helper()

	d := NewDispatcher(DispatcherConfig{QueueSize: queueSize}, slog.New(slog.DiscardHandler))
	d.Start(context.Background())
	t.Cleanup(d.Stop)

	return d
}

// recorder collects delivered events under a lock.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) HandleExecutionEvent(_ context.Context, event Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]Event(nil), r.events...)
}

func runCtx(runID string) RunContext {
	return RunContext{RunID: runID, ChatID: 1, TopicID: 2, MessageID: 3}
}

func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}

	return cond()
}

// runSequence publishes the canonical event sequence of one run.
func runSequence(runID string) []Event {
	rc := runCtx(runID)
	now := time.Now()

	return []Event{
		RunStarted{RunContext: rc, At: now},
		ToolStarted{RunContext: rc, At: now, CallID: runID + "-call", ToolName: "search_web"},
		ToolCompleted{RunContext: rc, At: now, CallID: runID + "-call", ToolName: "search_web", Duration: time.Second, Status: StatusOK},
		RunCompleted{RunContext: rc, At: now, Duration: 2 * time.Second, ReplyLength: 10},
	}
}

func TestSubscribeDeliversEventsInOrder(t *testing.T) {
	d := testDispatcher(t, 0)

	rec := &recorder{}
	d.Subscribe(rec)

	for _, event := range runSequence("a") {
		d.Publish(event)
	}
	for _, event := range runSequence("b") {
		d.Publish(event)
	}
	d.Stop()

	events := rec.snapshot()
	if len(events) != 8 {
		t.Fatalf("delivered %d events, want 8", len(events))
	}

	for _, runID := range []string{"a", "b"} {
		var order []string
		for _, event := range events {
			switch e := event.(type) {
			case RunStarted:
				if e.RunID == runID {
					order = append(order, "started")
				}
			case ToolStarted:
				if e.RunID == runID {
					order = append(order, "tool_started")
				}
			case ToolCompleted:
				if e.RunID == runID {
					order = append(order, "tool_completed")
				}
			case RunCompleted:
				if e.RunID == runID {
					order = append(order, "run_completed")
				}
			}
		}
		want := []string{"started", "tool_started", "tool_completed", "run_completed"}
		if fmt.Sprint(order) != fmt.Sprint(want) {
			t.Fatalf("run %s order = %v, want %v", runID, order, want)
		}
	}
}

func TestConcurrentRunsStayInternallyOrdered(t *testing.T) {
	d := testDispatcher(t, 0)

	rec := &recorder{}
	d.Subscribe(rec)

	const runs = 8
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, event := range runSequence(fmt.Sprintf("run-%d", i)) {
				d.Publish(event)
			}
		}()
	}
	wg.Wait()
	d.Stop()

	events := rec.snapshot()
	if len(events) != runs*4 {
		t.Fatalf("delivered %d events, want %d", len(events), runs*4)
	}

	for i := range runs {
		runID := fmt.Sprintf("run-%d", i)
		pos := map[string]int{"RunStarted": -1, "ToolStarted": -1, "ToolCompleted": -1, "RunCompleted": -1}
		for idx, event := range events {
			var name, id string
			switch e := event.(type) {
			case RunStarted:
				name, id = "RunStarted", e.RunID
			case ToolStarted:
				name, id = "ToolStarted", e.RunID
			case ToolCompleted:
				name, id = "ToolCompleted", e.RunID
			case RunCompleted:
				name, id = "RunCompleted", e.RunID
			}
			if id == runID && pos[name] == -1 {
				pos[name] = idx
			}
		}
		if pos["RunStarted"] >= pos["ToolStarted"] ||
			pos["ToolStarted"] >= pos["ToolCompleted"] ||
			pos["ToolCompleted"] >= pos["RunCompleted"] {
			t.Fatalf("run %s out of order: %v", runID, pos)
		}
	}
}

func TestSlowSubscriberDoesNotBlockPublish(t *testing.T) {
	d := testDispatcher(t, 0)

	rec := &recorder{}
	slow := HandlerFunc(func(_ context.Context, event Event) {
		time.Sleep(20 * time.Millisecond)
		rec.HandleExecutionEvent(context.Background(), event)
	})
	d.Subscribe(slow)

	start := time.Now()
	const total = 6
	for i := range total {
		d.Publish(RunStarted{RunContext: runCtx(fmt.Sprintf("r%d", i)), At: time.Now()})
	}
	elapsed := time.Since(start)
	if elapsed >= time.Duration(total)*20*time.Millisecond {
		t.Fatalf("publish took %v; slow subscriber blocked the producer", elapsed)
	}

	d.Stop()
	if got := len(rec.snapshot()); got != total {
		t.Fatalf("delivered %d events, want %d", got, total)
	}
}

func TestExpendableEventsDroppedBeforeTransitions(t *testing.T) {
	d := testDispatcher(t, 4)

	release := make(chan struct{})
	var entered atomic.Bool
	rec := &recorder{}
	blocking := HandlerFunc(func(ctx context.Context, event Event) {
		entered.Store(true)
		<-release
		rec.HandleExecutionEvent(ctx, event)
	})
	d.Subscribe(blocking)

	rc := runCtx("stuck")
	// Fill the queue with transition events while the handler is blocked.
	d.Publish(RunStarted{RunContext: rc, At: time.Now()})
	d.Publish(ToolStarted{RunContext: rc, At: time.Now(), CallID: "c1", ToolName: "search_web"})
	d.Publish(ToolCompleted{RunContext: rc, At: time.Now(), CallID: "c1", ToolName: "search_web", Status: StatusOK})
	d.Publish(RunCompleted{RunContext: rc, At: time.Now()})

	if !waitFor(t, entered.Load) {
		t.Fatal("handler never started")
	}

	for range 10 {
		d.Publish(TextDelta{RunContext: rc, At: time.Now(), Text: "x"})
	}

	close(release)
	d.Stop()

	// Every transition event survived, in order; deltas were dropped under
	// backpressure (accounted: dropped + delivered == published).
	events := rec.snapshot()
	deliveredDeltas := 0
	var kinds []string
	for _, event := range events {
		switch event.(type) {
		case RunStarted:
			kinds = append(kinds, "started")
		case ToolStarted:
			kinds = append(kinds, "tool_started")
		case ToolCompleted:
			kinds = append(kinds, "tool_completed")
		case RunCompleted:
			kinds = append(kinds, "run_completed")
		case TextDelta:
			deliveredDeltas++
		}
	}
	want := []string{"started", "tool_started", "tool_completed", "run_completed"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("delivered %v, want %v", kinds, want)
	}
	dropped := d.dropped.Load()
	if dropped == 0 || dropped+int64(deliveredDeltas) != 10 {
		t.Fatalf("deltas dropped=%d delivered=%d, want drop accounting to cover all 10", dropped, deliveredDeltas)
	}
}

func TestOverflowUnsubscribesLaggingSubscriber(t *testing.T) {
	d := testDispatcher(t, 2)

	release := make(chan struct{})
	seen := make(chan Event, 4)
	blocking := HandlerFunc(func(_ context.Context, event Event) {
		seen <- event
		<-release
	})
	unsubscribe := d.Subscribe(blocking)
	defer unsubscribe()

	rc := runCtx("lag")
	d.Publish(RunStarted{RunContext: rc, At: time.Now()})
	if !waitFor(t, func() bool {
		select {
		case <-seen:
			return true
		default:
			return false
		}
	}) {
		t.Fatal("handler never received the first event")
	}

	// The handler is blocked mid-delivery: the next two transitions fill the
	// queue and the third overflows it with only non-expendable events
	// pending, so the subscriber is unsubscribed (its pending events are
	// lost by design).
	d.Publish(ToolStarted{RunContext: rc, At: time.Now(), CallID: "c1", ToolName: "t"})
	d.Publish(ToolCompleted{RunContext: rc, At: time.Now(), CallID: "c1", ToolName: "t", Status: StatusOK})
	d.Publish(RunCompleted{RunContext: rc, At: time.Now()})
	if !waitFor(t, func() bool { return d.subscriberCount() == 0 }) {
		t.Fatal("lagging subscriber was not unsubscribed on overflow")
	}

	close(release)
	d.Stop()

	// A fresh dispatcher with a healthy subscriber is unaffected.
	healthy := testDispatcher(t, 2)
	rec := &recorder{}
	healthy.Subscribe(rec)
	healthy.Publish(RunCompleted{RunContext: rc, At: time.Now()})
	healthy.Stop()

	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("healthy subscriber got %d events after the overflow, want 1", len(events))
	}
}

func TestStopDrainsAndPublishAfterStopIsNoop(t *testing.T) {
	d := NewDispatcher(DispatcherConfig{QueueSize: 16}, slog.New(slog.DiscardHandler))
	d.Start(context.Background())

	rec := &recorder{}
	d.Subscribe(rec)
	for i := range 5 {
		d.Publish(RunStarted{RunContext: runCtx(fmt.Sprintf("r%d", i)), At: time.Now()})
	}

	d.Stop()
	if got := len(rec.snapshot()); got != 5 {
		t.Fatalf("drained %d events, want 5", got)
	}

	d.Publish(RunStarted{RunContext: runCtx("late"), At: time.Now()})
	if got := len(rec.snapshot()); got != 5 {
		t.Fatalf("publish after Stop delivered %d extra events", got-5)
	}

	d.Stop() // idempotent
	if got := d.subscriberCount(); got != 0 {
		t.Fatalf("%d subscribers still registered after Stop", got)
	}
}

func TestStopReleasesDeliveryGoroutines(t *testing.T) {
	d := testDispatcher(t, 0)

	before := runtime.NumGoroutine()
	var subs []func()
	for range 4 {
		subs = append(subs, d.Subscribe(&recorder{}))
	}
	if !waitFor(t, func() bool { return runtime.NumGoroutine() >= before+4 }) {
		t.Fatal("delivery goroutines did not start")
	}
	for _, unsubscribe := range subs {
		unsubscribe()
	}
	if !waitFor(t, func() bool { return runtime.NumGoroutine() <= before+2 }) {
		t.Fatalf("goroutines did not unwind after unsubscribe: %d (baseline %d)", runtime.NumGoroutine(), before)
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	d := testDispatcher(t, 0)

	rec := &recorder{}
	unsubscribe := d.Subscribe(rec)
	unsubscribe()
	unsubscribe() // idempotent

	d.Publish(RunStarted{RunContext: runCtx("x"), At: time.Now()})
	d.Stop()

	if got := len(rec.snapshot()); got != 0 {
		t.Fatalf("unsubscribed subscriber received %d events", got)
	}
}

func TestHandlerPanicUnsubscribesOnlyTheOffender(t *testing.T) {
	d := testDispatcher(t, 0)

	panicky := HandlerFunc(func(_ context.Context, _ Event) {
		panic("boom")
	})
	d.Subscribe(panicky)

	rec := &recorder{}
	d.Publish(RunStarted{RunContext: runCtx("x"), At: time.Now()})
	if !waitFor(t, func() bool { return d.subscriberCount() == 0 }) {
		t.Fatal("panicking subscriber was not removed")
	}

	d.Subscribe(rec)
	d.Publish(RunStarted{RunContext: runCtx("y"), At: time.Now()})
	d.Stop()

	if got := len(rec.snapshot()); got != 1 {
		t.Fatalf("healthy subscriber received %d events, want 1", got)
	}
}

func TestNewRunIDUnique(t *testing.T) {
	seen := make(map[string]struct{})
	for range 1000 {
		id := NewRunID()
		if id == "" {
			t.Fatal("empty run id")
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate run id %q", id)
		}
		seen[id] = struct{}{}
	}
}
