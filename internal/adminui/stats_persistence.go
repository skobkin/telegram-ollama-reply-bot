package adminui

import (
	"context"

	"telegram-ollama-reply-bot/internal/statistics"
)

// StatsPersister makes a statistics reset durable. It is the admin-side view
// of statistics.Persister: the acknowledgement depends on the persisted
// generation, not on this call performing the write.
type StatsPersister interface {
	EnsureDurable(ctx context.Context, dataset string, generation uint64) statistics.DurableOutcome
}

// WithStatsPersistence wires the persister used to acknowledge resets of the
// persistent statistics datasets. Without it resets stay in-memory notices.
func WithStatsPersistence(p StatsPersister) ControllerOption {
	return func(o *ControllerOptions) {
		o.StatsPersistence = p
	}
}

// resetNotice completes a reset notice with the durability outcome of the
// reset generation. A temporary write failure promises a retry; disabled
// persistence promises none.
func (c *Controller) resetNotice(ctx context.Context, dataset string, generation uint64, base string) string {
	if c.opts.StatsPersistence == nil {
		return base
	}

	out := c.opts.StatsPersistence.EnsureDurable(ctx, dataset, generation)
	switch {
	case out.Durable:
		return base + " Reset saved."
	case out.State.Disabled():
		return base + " Applied in memory only: persistence for this dataset is disabled (" +
			persistenceReason(out.State) + "), so the reset is not saved and the stored statistics stay unchanged — " +
			"after a restart they are loaded again."
	default:
		return base + " ⚠️ Applied, but saving it failed; it will be retried. If the bot stops abruptly " +
			"before it is saved, the previous statistics come back."
	}
}

func persistenceReason(state statistics.PersistState) string {
	switch state {
	case statistics.PersistDisabledNewerFormat:
		return "the stored data uses a newer format"
	case statistics.PersistDisabledQuarantineFailed:
		return "the stored data was unreadable and could not be set aside"
	default:
		return string(state)
	}
}
