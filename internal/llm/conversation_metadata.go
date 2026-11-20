package llm

import (
	"fmt"
	"time"
)

// renderConversationMetadata is appended after persona rendering, so custom
// templates cannot accidentally omit it and need no new placeholders.
func renderConversationMetadata(reply ChatReplyContext, now time.Time) string {
	retained, bounds := "unavailable", "unavailable"
	if metadata := reply.RetainedHistory; metadata != nil {
		retained = fmt.Sprint(metadata.RetainedMessages)
		if metadata.RetainedMessages == 0 {
			bounds = "empty"
		} else if !metadata.OldestMessageAt.IsZero() && !metadata.NewestMessageAt.IsZero() {
			bounds = metadata.OldestMessageAt.UTC().Format(time.RFC3339) + ".." + metadata.NewestMessageAt.UTC().Format(time.RFC3339)
		}
	}

	return fmt.Sprintf("Conversation metadata (retained RAM history, not complete Telegram history): now=%s; timezone=%s (UTC%s); retained_messages=%s; retained_bounds=%s; conversation_messages_in_context=%d (selected history plus trigger; excludes summaries, system and tool messages).", now.Format(time.RFC3339), now.Location().String(), now.Format("-07:00"), retained, bounds, len(reply.History)+1)
}
