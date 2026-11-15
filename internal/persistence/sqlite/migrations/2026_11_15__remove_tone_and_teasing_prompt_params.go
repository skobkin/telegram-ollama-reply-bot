package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"telegram-ollama-reply-bot/internal/llm"
)

func migrateV11RemoveToneAndTeasingPromptParams(ctx context.Context, tx *sql.Tx, _ *slog.Logger) error {
	if _, err := tx.ExecContext(ctx, `
UPDATE prompt_templates
SET template_text = ?, updated_at = CURRENT_TIMESTAMP, updated_by = 0
WHERE feature = 'chat' AND template_text = ?
`, llm.DefaultChatPromptTemplate, llm.OldTeasingChatPromptTemplate); err != nil {
		return fmt.Errorf("refresh default chat prompt template: %w", err)
	}

	if err := applyStatements(ctx, tx, "remove_tone_and_teasing_prompt_params", []string{
		`ALTER TABLE global_settings DROP COLUMN tone_mode;`,
		`ALTER TABLE global_settings DROP COLUMN allow_teasing;`,
		`ALTER TABLE chat_settings DROP COLUMN tone_mode;`,
		`ALTER TABLE chat_settings DROP COLUMN allow_teasing;`,
	}, nil); err != nil {
		return err
	}

	return nil
}
