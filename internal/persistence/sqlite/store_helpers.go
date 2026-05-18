package sqlite

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

func globalFieldAssignment(field, value string) (string, any, error) {
	switch field {
	case "character_name":
		return "character_name", value, nil
	case "language":
		return "language", value, nil
	case "gender":
		return "gender", value, nil
	case "tone_mode":
		return "tone_mode", value, nil
	case "allow_teasing":
		parsed, err := parseBoolValue(value)

		return "allow_teasing", boolToSQLite(parsed), err
	case "default_interactivity_mode":
		mode := adminconfig.InteractivityMode(value)
		if !isValidInteractivityMode(mode) {
			return "", nil, fmt.Errorf("unsupported interactivity mode %q; supported values: %s", value, formatInteractivityModes())
		}

		return "default_interactivity_mode", string(mode), nil
	default:
		return "", nil, fmt.Errorf("unsupported global field %q", field)
	}
}

func chatFieldAssignment(field, value string) (string, any, error) {
	switch field {
	case "character_name":
		return "character_name", value, nil
	case "language":
		return "language", value, nil
	case "gender":
		return "gender", value, nil
	case "tone_mode":
		return "tone_mode", value, nil
	case "allow_teasing":
		parsed, err := parseBoolValue(value)

		return "allow_teasing", boolToSQLite(parsed), err
	case "interactivity_mode":
		mode := adminconfig.InteractivityMode(value)
		if !isValidInteractivityMode(mode) {
			return "", nil, fmt.Errorf("unsupported interactivity mode %q; supported values: %s", value, formatInteractivityModes())
		}

		return "interactivity_mode", string(mode), nil
	default:
		return "", nil, fmt.Errorf("unsupported chat field %q", field)
	}
}

func chatFieldClearValue(field string) (string, any, error) {
	switch field {
	case "character_name":
		return "character_name", "", nil
	case "language":
		return "language", "", nil
	case "gender":
		return "gender", "", nil
	case "tone_mode":
		return "tone_mode", "", nil
	case "allow_teasing":
		return "allow_teasing", nil, nil
	case "interactivity_mode":
		return "interactivity_mode", "", nil
	default:
		return "", nil, fmt.Errorf("unsupported chat field %q", field)
	}
}

func parseBoolValue(value string) (bool, error) {
	parsed, err := strconv.ParseBool(strings.ToLower(strings.TrimSpace(value)))
	if err != nil {
		return false, fmt.Errorf("parse bool value %q: %w", value, err)
	}

	return parsed, nil
}

func boolToSQLite(v bool) int {
	if v {
		return 1
	}

	return 0
}

func isValidInteractivityMode(mode adminconfig.InteractivityMode) bool {
	for _, supported := range adminconfig.InteractivityModes() {
		if mode == supported {
			return true
		}
	}

	return false
}

func formatInteractivityModes() string {
	modes := make([]string, 0, len(adminconfig.InteractivityModes()))
	for _, mode := range adminconfig.InteractivityModes() {
		modes = append(modes, string(mode))
	}

	return strings.Join(modes, ", ")
}

func globalFieldUpdateQuery(column string) (string, error) {
	switch column {
	case "character_name":
		return `UPDATE global_settings SET character_name = ?, updated_at = CURRENT_TIMESTAMP, updated_by = ? WHERE id = 1`, nil
	case "language":
		return `UPDATE global_settings SET language = ?, updated_at = CURRENT_TIMESTAMP, updated_by = ? WHERE id = 1`, nil
	case "gender":
		return `UPDATE global_settings SET gender = ?, updated_at = CURRENT_TIMESTAMP, updated_by = ? WHERE id = 1`, nil
	case "tone_mode":
		return `UPDATE global_settings SET tone_mode = ?, updated_at = CURRENT_TIMESTAMP, updated_by = ? WHERE id = 1`, nil
	case "allow_teasing":
		return `UPDATE global_settings SET allow_teasing = ?, updated_at = CURRENT_TIMESTAMP, updated_by = ? WHERE id = 1`, nil
	case "default_interactivity_mode":
		return `UPDATE global_settings SET default_interactivity_mode = ?, updated_at = CURRENT_TIMESTAMP, updated_by = ? WHERE id = 1`, nil
	default:
		return "", fmt.Errorf("unsupported global column %q", column)
	}
}

func chatFieldUpdateQuery(column string) (string, error) {
	switch column {
	case "character_name":
		return `UPDATE chat_settings SET character_name = ?, updated_at = CURRENT_TIMESTAMP, updated_by = ? WHERE chat_id = ?`, nil
	case "language":
		return `UPDATE chat_settings SET language = ?, updated_at = CURRENT_TIMESTAMP, updated_by = ? WHERE chat_id = ?`, nil
	case "gender":
		return `UPDATE chat_settings SET gender = ?, updated_at = CURRENT_TIMESTAMP, updated_by = ? WHERE chat_id = ?`, nil
	case "tone_mode":
		return `UPDATE chat_settings SET tone_mode = ?, updated_at = CURRENT_TIMESTAMP, updated_by = ? WHERE chat_id = ?`, nil
	case "allow_teasing":
		return `UPDATE chat_settings SET allow_teasing = ?, updated_at = CURRENT_TIMESTAMP, updated_by = ? WHERE chat_id = ?`, nil
	case "interactivity_mode":
		return `UPDATE chat_settings SET interactivity_mode = ?, updated_at = CURRENT_TIMESTAMP, updated_by = ? WHERE chat_id = ?`, nil
	default:
		return "", fmt.Errorf("unsupported chat column %q", column)
	}
}

func formatSQLiteTime(ts time.Time) string {
	return ts.UTC().Format(time.RFC3339)
}

func parseSQLiteTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts
	}
	if ts, err := time.Parse("2006-01-02 15:04:05", raw); err == nil {
		return ts.UTC()
	}

	return time.Time{}
}
