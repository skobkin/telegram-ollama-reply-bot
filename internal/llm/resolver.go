package llm

import "context"

// ModelResolver resolves the effective model for a feature from runtime
// settings (database overrides taking precedence over environment routes).
// Implementations return "" for unset — the caller then falls back to its
// configured environment route — and must propagate resolution failures
// instead of silently falling back: unset and failed are different states.
// chatID scopes per-chat overrides; global-only features (summarize,
// image recognition) ignore it, and callers pass 0 for them.
type ModelResolver interface {
	ResolveModel(ctx context.Context, feature Feature, chatID int64) (string, error)
}

// TurnConfig is one operation's resolved model selection, taken from a
// single settings snapshot when the operation starts and kept fixed for its
// whole duration — a settings change mid-operation affects only operations
// that start later.
type TurnConfig struct {
	// GenerationModel routes the chat wire requests and the {{.Model}}
	// prompt placeholder across every tool-loop iteration. Empty means
	// unset: the environment route stays in effect.
	GenerationModel string
	// SystemOneModel names the decision backend model used by the
	// interaction engine and the decision tools. Empty means unset: the
	// decision backend is unconfigured for this operation.
	SystemOneModel string
}
