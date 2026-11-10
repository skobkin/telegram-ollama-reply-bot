package bot

import (
	"strings"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

// Shared admin helpers kept for the interactive admin UI and its tests. The
// legacy IRC-style command handlers were removed; their capabilities live in
// the admin UI now. The bootstrap diagnostic (empty BOT__ADMIN_IDS) moved to
// the admin UI adapter package.

func formatConfigFields(fields []string) string {
	lines := make([]string, 0, len(fields))
	for _, field := range fields {
		line := field
		if field == "default_interactivity_mode" || field == "interactivity_mode" {
			line += " (" + formatInteractivityModeList() + ")"
		}
		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}

func formatInteractivityModeList() string {
	modes := adminconfig.InteractivityModes()
	values := make([]string, 0, len(modes))
	for _, mode := range modes {
		values = append(values, string(mode))
	}

	return strings.Join(values, ", ")
}
