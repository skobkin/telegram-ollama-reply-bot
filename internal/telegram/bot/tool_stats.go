package bot

import (
	"telegram-ollama-reply-bot/internal/llmcontext"
	"telegram-ollama-reply-bot/internal/statistics"
	"telegram-ollama-reply-bot/internal/tooluse"
)

// SetChatRouteModel wires the environment chat route model: the effective
// generation model recorded in statistics when a chat's model override is
// empty. It is independent of proactive participation.
func (b *Bot) SetChatRouteModel(model string) {
	b.chatRouteModel = model
}

// toolRunFailureOf maps the pipeline's turn failure to the tool statistics
// run-failure class. It covers generation/runtime outcomes only: the
// observation point precedes final Telegram text delivery, so a delivery
// failure never appears here (interaction execution statistics own it).
func toolRunFailureOf(err error) statistics.ToolRunFailure {
	switch turnFailureFromError(err) {
	case turnFailureTimeout:
		return statistics.ToolRunTimeout
	case turnFailureCancelled:
		return statistics.ToolRunCancelled
	case turnFailureGeneration, turnFailureDelivery:
		return statistics.ToolRunGeneration
	case turnFailureNone:
		return statistics.ToolRunOK
	}

	return statistics.ToolRunOK
}

// toolTurnObservationOf converts the run's authoritative result into the
// statistics observation. Records are used as returned by the runtime, so
// failed, cancelled and timed-out turns keep their tool statistics.
func toolTurnObservationOf(epoch uint64, chatID int64, trigger llmcontext.TriggerKind, generationModel string, advertised []string, records []tooluse.ToolCallRecord, runErr error) statistics.ToolTurnObservation {
	mode := statistics.ToolModeExplicit
	if trigger == llmcontext.TriggerProactive {
		mode = statistics.ToolModeProactive
	}

	obs := statistics.ToolTurnObservation{
		Epoch:           epoch,
		ChatID:          chatID,
		Mode:            mode,
		GenerationModel: generationModel,
		Advertised:      advertised,
		RunFailure:      toolRunFailureOf(runErr),
		Calls:           make([]statistics.ToolCallObservation, 0, len(records)),
	}
	for _, record := range records {
		obs.Calls = append(obs.Calls, toolCallObservationOf(record))
	}

	return obs
}

func toolCallObservationOf(record tooluse.ToolCallRecord) statistics.ToolCallObservation {
	call := statistics.ToolCallObservation{
		Name:              record.Name,
		Executed:          record.Executed,
		Status:            record.Status,
		ErrorCode:         record.ErrorCode,
		ErrorClass:        record.ErrorClass,
		SideEffecting:     record.SideEffecting,
		SideEffectOutcome: record.SideEffectOutcome,
		Duration:          record.Duration,
	}
	switch record.Rejection {
	case tooluse.RejectionNotRegistered:
		call.Rejection = statistics.ToolRejectedNotRegistered
	case tooluse.RejectionNotAdvertised:
		call.Rejection = statistics.ToolRejectedNotAdvertised
	case tooluse.RejectionUnavailable:
		call.Rejection = statistics.ToolRejectedUnavailable
	case tooluse.RejectionNone:
	}
	switch record.ResultPreparation {
	case tooluse.ResultPreparationReduced:
		call.Preparation = statistics.ToolPreparationReduced
	case tooluse.ResultPreparationFailed:
		call.Preparation = statistics.ToolPreparationFailed
	case tooluse.ResultPreparationComplete:
	}

	return call
}

// recordToolTurn records one turn's tool statistics.
func (b *Bot) recordToolTurn(epoch uint64, chatID int64, trigger llmcontext.TriggerKind, resolvedModel string, advertised []string, records []tooluse.ToolCallRecord, runErr error) {
	b.stats.RecordToolTurn(toolTurnObservationOf(epoch, chatID, trigger, resolvedModel, advertised, records, runErr))
}
