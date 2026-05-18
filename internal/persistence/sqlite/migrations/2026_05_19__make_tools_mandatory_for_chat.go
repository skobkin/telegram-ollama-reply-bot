package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"telegram-ollama-reply-bot/internal/llm"
)

func migrateV4MakeToolsMandatoryForChat(ctx context.Context, tx *sql.Tx, _ *slog.Logger) error {
	if _, err := tx.ExecContext(ctx, `
UPDATE prompt_templates
SET template_text = ?, updated_at = CURRENT_TIMESTAMP, updated_by = 0
WHERE feature = 'chat' AND template_text = ?
`, llm.DefaultChatPromptTemplate, llm.OldDefaultChatPromptTemplate); err != nil {
		return fmt.Errorf("upgrade default chat prompt template: %w", err)
	}

	return nil
}
