package bot

import (
	"strconv"
	"strings"

	"telegram-ollama-reply-bot/internal/adminconfig"

	t "github.com/mymmrac/telego"
)

// Shared admin helpers kept for the interactive admin UI and its tests. The
// legacy IRC-style command handlers were removed; their capabilities live in
// the admin UI now.

func adminControlsDisabledText(user *t.User) string {
	text := "Admin controls are disabled: BOT__ADMIN_IDS is empty."
	if user == nil {
		return text
	}

	userID := strconv.FormatInt(user.ID, 10)

	return text + "\nYour Telegram user ID: " + userID + "\nSet BOT__ADMIN_IDS=" + userID
}

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
