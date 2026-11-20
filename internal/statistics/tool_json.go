package statistics

import (
	"encoding/json"
	"fmt"
	"time"
)

// Schema and version of the tool-statistics export. Any shape change bumps
// the version.
const (
	ToolSchemaName    = "tg-llm-bot/tool-statistics"
	ToolSchemaVersion = 1
)

// ToolJSON renders the versioned tool-statistics export for one snapshot and
// the scope it was taken under. It is a presenter: callers snapshot first,
// then render the same immutable value as JSON and PNG.
func ToolJSON(snapshot ToolSnapshot, scope InteractionScope) ([]byte, error) {
	envelope := toolExport{
		Schema:       ToolSchemaName,
		Version:      ToolSchemaVersion,
		GeneratedAt:  snapshot.GeneratedAt,
		Scope:        exportScopeOf(scope),
		ToolSnapshot: snapshot,
		Notes:        toolNotes(),
	}

	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("serialize tool export: %w", err)
	}

	return data, nil
}

type toolExport struct {
	Schema      string      `json:"schema"`
	Version     int         `json:"version"`
	GeneratedAt time.Time   `json:"generated_at"`
	Scope       exportScope `json:"scope"`
	ToolSnapshot
	Notes toolExportNotes `json:"notes"`
}

type toolExportNotes struct {
	Percentiles       string `json:"percentiles"`
	Counting          string `json:"counting"`
	Utilization       string `json:"utilization"`
	Recovery          string `json:"recovery"`
	ResultPreparation string `json:"result_preparation"`
	RunFailures       string `json:"run_failures"`
	Persistence       string `json:"persistence"`
	Privacy           string `json:"privacy"`
}

func toolNotes() toolExportNotes {
	return toolExportNotes{
		Percentiles:       "percentiles are histogram approximations; at_least_ms is the terminal open-bin lower bound; null = no data",
		Counting:          "attempts = executed + rejected; succeeded counts ok and empty results; failed counts executed error results; rejected calls never reached their handler",
		Utilization:       "utilization_rate = advertised_invoked_turns / advertised_turns; calls to tools that were not advertised count as attempts but not in the numerator",
		Recovery:          "recovery counts observable later calls of the same tool in the same turn after an error; it makes no claim about model reasoning; on overflow rows identity-turn counts can exceed unique turns",
		ResultPreparation: "classifies preparing the model-facing result within its budget, not delivery; handler outcome and side effects are independent of it",
		RunFailures:       "generation/runtime outcome observed before final Telegram text delivery",
		Persistence:       "statistics are persistent aggregates; recently collected observations can be lost after an abrupt termination",
		Privacy:           "aggregates only: no arguments, results, messages, user attribution, call IDs or timestamps",
	}
}
