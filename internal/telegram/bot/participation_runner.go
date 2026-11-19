package bot

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// participationRunner executes proactive decision tasks with per-chat
// serialization and a tracked lifecycle. At most one decision/action is in
// flight per chat — waiting tasks park on a buffered token channel, which
// keeps the wait genuinely cancellable (a plain mutex lock is not) — while
// tasks for different chats run concurrently and nothing is ever dropped.
// No ordering guarantee is claimed between queued tasks of one chat.
type participationRunner struct {
	ctx     context.Context
	cancel  context.CancelFunc
	timeout time.Duration
	logger  *slog.Logger

	wg     sync.WaitGroup
	mu     sync.Mutex
	slots  map[int64]chan struct{}
	closed bool
}

func newParticipationRunner(parent context.Context, timeout time.Duration, logger *slog.Logger) *participationRunner {
	ctx, cancel := context.WithCancel(parent)

	return &participationRunner{
		ctx:     ctx,
		cancel:  cancel,
		timeout: timeout,
		logger:  logger,
		slots:   make(map[int64]chan struct{}),
	}
}

// Submit schedules one proactive task for the chat. It returns false when
// the runner is already shutting down — no new tasks exist after Close.
func (r *participationRunner) Submit(chatID int64, task func(ctx context.Context)) bool {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()

		return false
	}
	slot := r.slots[chatID]
	if slot == nil {
		slot = make(chan struct{}, 1)
		r.slots[chatID] = slot
	}
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()

		select {
		case slot <- struct{}{}:
		case <-r.ctx.Done():
			// Shutdown while waiting: the task ends without evaluating
			// anything, and the update was already recorded in history by
			// the middleware — nothing is dropped mid-flight.
			return
		}
		defer func() { <-slot }()

		// Cancellation re-check: with both select branches ready Go may
		// pick either, so shutdown can win the race — bail before any
		// settings read or inference starts, so a cancellation racing slot
		// acquisition can never initiate a proactive action.
		if r.ctx.Err() != nil {
			return
		}

		// The deadline is created only now: slot waiting time never
		// consumes the inference budget. A non-positive timeout means no
		// deadline, mirroring withProcessingDeadline for explicit turns.
		var taskCtx context.Context
		var cancel context.CancelFunc
		if r.timeout > 0 {
			taskCtx, cancel = context.WithTimeout(r.ctx, r.timeout)
		} else {
			taskCtx, cancel = context.WithCancel(r.ctx)
		}
		defer cancel()

		task(taskCtx)
	}()

	return true
}

// Close cancels the lifecycle: waiting tasks exit through their select,
// executing tasks observe cancellation through their task contexts
// (in-flight HTTP aborts) and finish, and Close blocks until every task has
// exited. Submissions are rejected afterwards.
func (r *participationRunner) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()

		return
	}
	r.closed = true
	r.mu.Unlock()

	r.cancel()
	r.wg.Wait()
}
