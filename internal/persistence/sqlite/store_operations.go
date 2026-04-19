package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

func (s *Store) GetGlobalSettings(ctx context.Context) (adminconfig.GlobalSettings, error) {
	var settings adminconfig.GlobalSettings
	var allowTeasing int
	var updatedAt string

	err := s.db.QueryRowContext(ctx, `
SELECT character_name, language, gender, tone_mode, allow_teasing, default_interactivity_mode, updated_at, updated_by
FROM global_settings
WHERE id = 1
`).Scan(
		&settings.CharacterName,
		&settings.Language,
		&settings.Gender,
		&settings.ToneMode,
		&allowTeasing,
		&settings.DefaultInteractivityMode,
		&updatedAt,
		&settings.UpdatedBy,
	)
	if err != nil {
		return adminconfig.GlobalSettings{}, fmt.Errorf("get global settings: %w", err)
	}

	settings.AllowTeasing = allowTeasing == 1
	settings.UpdatedAt = parseSQLiteTime(updatedAt)

	return settings, nil
}

func (s *Store) SetGlobalField(ctx context.Context, field, value string, actorID int64) error {
	column, transformed, err := globalFieldAssignment(field, value)
	if err != nil {
		return err
	}

	query, err := globalFieldUpdateQuery(column)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, query, transformed, actorID)
	if err != nil {
		return fmt.Errorf("set global field %s: %w", field, err)
	}

	return nil
}

func (s *Store) GetChatSettings(ctx context.Context, chatID int64) (adminconfig.ChatSettings, bool, error) {
	var settings adminconfig.ChatSettings
	var alias string
	var language string
	var gender string
	var toneMode string
	var allow sql.NullInt64
	var interactivity string
	var updatedAt string

	err := s.db.QueryRowContext(ctx, `
SELECT chat_id, alias, language, gender, tone_mode, allow_teasing, interactivity_mode, updated_at, updated_by
FROM chat_settings
WHERE chat_id = ?
`, chatID).Scan(
		&settings.ChatID,
		&alias,
		&language,
		&gender,
		&toneMode,
		&allow,
		&interactivity,
		&updatedAt,
		&settings.UpdatedBy,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return adminconfig.ChatSettings{}, false, nil
		}

		return adminconfig.ChatSettings{}, false, fmt.Errorf("get chat settings: %w", err)
	}

	settings.Alias = alias
	settings.Language = language
	settings.Gender = gender
	settings.ToneMode = toneMode
	settings.InteractivityMode = adminconfig.InteractivityMode(interactivity)
	settings.HasInteractivity = interactivity != ""
	if allow.Valid {
		v := allow.Int64 == 1
		settings.AllowTeasing = &v
	}
	settings.UpdatedAt = parseSQLiteTime(updatedAt)

	return settings, true, nil
}

func (s *Store) SetChatField(ctx context.Context, chatID int64, field, value string, actorID int64) error {
	column, transformed, err := chatFieldAssignment(field, value)
	if err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx, `
INSERT INTO chat_settings(chat_id, alias, language, gender, tone_mode, allow_teasing, interactivity_mode, updated_at, updated_by)
VALUES(?, '', '', '', '', NULL, '', CURRENT_TIMESTAMP, ?)
ON CONFLICT(chat_id) DO NOTHING
`, chatID, actorID); err != nil {
		return fmt.Errorf("bootstrap chat settings: %w", err)
	}

	query, err := chatFieldUpdateQuery(column)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, query, transformed, actorID, chatID)
	if err != nil {
		return fmt.Errorf("set chat field %s: %w", field, err)
	}

	return nil
}

func (s *Store) ClearChatField(ctx context.Context, chatID int64, field string, actorID int64) error {
	if field == "all" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM chat_settings WHERE chat_id = ?`, chatID)
		if err != nil {
			return fmt.Errorf("clear chat settings: %w", err)
		}

		return nil
	}

	column, value, err := chatFieldClearValue(field)
	if err != nil {
		return err
	}

	query, err := chatFieldUpdateQuery(column)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, query, value, actorID, chatID)
	if err != nil {
		return fmt.Errorf("clear chat field %s: %w", field, err)
	}

	return nil
}

func (s *Store) GetPromptTemplate(ctx context.Context, feature adminconfig.PromptFeature, chatID int64) (string, bool, error) {
	scopeType := "global"
	scopeID := int64(0)
	if chatID != 0 {
		scopeType = "chat"
		scopeID = chatID
	}

	var body string
	err := s.db.QueryRowContext(ctx, `
SELECT template_text
FROM prompt_templates
WHERE scope_type = ? AND scope_id = ? AND feature = ?
`, scopeType, scopeID, feature).Scan(&body)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("get prompt template: %w", err)
	}

	return body, true, nil
}

func (s *Store) SetPromptTemplate(ctx context.Context, feature adminconfig.PromptFeature, chatID int64, body string, actorID int64) error {
	scopeType := "global"
	scopeID := int64(0)
	if chatID != 0 {
		scopeType = "chat"
		scopeID = chatID
	}

	_, err := s.db.ExecContext(ctx, `
INSERT INTO prompt_templates(scope_type, scope_id, feature, template_text, updated_at, updated_by)
VALUES(?, ?, ?, ?, CURRENT_TIMESTAMP, ?)
ON CONFLICT(scope_type, scope_id, feature)
DO UPDATE SET template_text = excluded.template_text, updated_at = CURRENT_TIMESTAMP, updated_by = excluded.updated_by
`, scopeType, scopeID, feature, body, actorID)
	if err != nil {
		return fmt.Errorf("set prompt template: %w", err)
	}

	return nil
}

func (s *Store) ClearPromptTemplate(ctx context.Context, feature adminconfig.PromptFeature, chatID int64) error {
	scopeType := "global"
	scopeID := int64(0)
	if chatID != 0 {
		scopeType = "chat"
		scopeID = chatID
	}

	_, err := s.db.ExecContext(ctx, `
DELETE FROM prompt_templates
WHERE scope_type = ? AND scope_id = ? AND feature = ?
`, scopeType, scopeID, feature)
	if err != nil {
		return fmt.Errorf("clear prompt template: %w", err)
	}

	return nil
}

func (s *Store) UpsertChatCatalog(ctx context.Context, entry adminconfig.ChatCatalogEntry) error {
	if entry.FirstSeenAt.IsZero() {
		entry.FirstSeenAt = time.Now().UTC()
	}
	if entry.LastSeenAt.IsZero() {
		entry.LastSeenAt = entry.FirstSeenAt
	}

	_, err := s.db.ExecContext(ctx, `
INSERT INTO chat_catalog(chat_id, chat_type, title, username, display_name, first_seen_at, last_seen_at)
VALUES(?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(chat_id)
DO UPDATE SET
  chat_type = excluded.chat_type,
  title = excluded.title,
  username = excluded.username,
  display_name = excluded.display_name,
  last_seen_at = excluded.last_seen_at
`, entry.ChatID, entry.ChatType, entry.Title, entry.Username, entry.DisplayName, formatSQLiteTime(entry.FirstSeenAt), formatSQLiteTime(entry.LastSeenAt))
	if err != nil {
		return fmt.Errorf("upsert chat catalog: %w", err)
	}

	return nil
}

func (s *Store) ListChats(ctx context.Context) ([]adminconfig.ChatCatalogEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT chat_id, chat_type, title, username, display_name, first_seen_at, last_seen_at
FROM chat_catalog
ORDER BY last_seen_at DESC, chat_id ASC
`)
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []adminconfig.ChatCatalogEntry
	for rows.Next() {
		var entry adminconfig.ChatCatalogEntry
		var firstSeen string
		var lastSeen string
		if err := rows.Scan(&entry.ChatID, &entry.ChatType, &entry.Title, &entry.Username, &entry.DisplayName, &firstSeen, &lastSeen); err != nil {
			return nil, fmt.Errorf("scan chat catalog: %w", err)
		}
		entry.FirstSeenAt = parseSQLiteTime(firstSeen)
		entry.LastSeenAt = parseSQLiteTime(lastSeen)
		result = append(result, entry)
	}

	return result, rows.Err()
}

func (s *Store) AddWhitelistChat(ctx context.Context, chatID int64, actorID int64) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO chat_whitelist(chat_id, added_at, added_by)
VALUES(?, CURRENT_TIMESTAMP, ?)
ON CONFLICT(chat_id) DO UPDATE SET added_at = CURRENT_TIMESTAMP, added_by = excluded.added_by
`, chatID, actorID)
	if err != nil {
		return fmt.Errorf("add whitelist chat: %w", err)
	}

	return nil
}

func (s *Store) RemoveWhitelistChat(ctx context.Context, chatID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM chat_whitelist WHERE chat_id = ?`, chatID)
	if err != nil {
		return fmt.Errorf("remove whitelist chat: %w", err)
	}

	return nil
}

func (s *Store) ListWhitelistChats(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT chat_id FROM chat_whitelist ORDER BY chat_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list whitelist chats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []int64
	for rows.Next() {
		var chatID int64
		if err := rows.Scan(&chatID); err != nil {
			return nil, fmt.Errorf("scan whitelist chat: %w", err)
		}
		result = append(result, chatID)
	}

	return result, rows.Err()
}

func (s *Store) IsChatWhitelisted(ctx context.Context, chatID int64) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_whitelist WHERE chat_id = ?`, chatID).Scan(&count); err != nil {
		return false, fmt.Errorf("check whitelist chat: %w", err)
	}

	return count > 0, nil
}
