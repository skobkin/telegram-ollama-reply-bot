package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"telegram-ollama-reply-bot/internal/adminconfig"

	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

func (b *Bot) adminHelpHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	text := strings.Join([]string{
		"Admin commands:",
		"/chat_list",
		"/config_fields_global",
		"/config_fields_chat",
		"/prompt_features",
		"/config_show [chat_id]",
		"/config_set_global <field> <value>",
		"/config_set_chat <chat_id> <field> <value>",
		"/config_clear_chat <chat_id> <field|all>",
		"/prompt_show <feature> [chat_id]",
		"/prompt_set_global <feature> <template>",
		"/prompt_set_chat <chat_id> <feature> <template>",
		"/prompt_clear_chat <chat_id> <feature>",
		"/whitelist_add <chat_id>",
		"/whitelist_remove <chat_id>",
		"/whitelist_list",
	}, "\n")

	return b.sendAdminText(ctx.Context(), message, text)
}

func (b *Bot) chatListHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	chats, err := b.admin.ChatList(ctx.Context())
	if err != nil {
		return b.sendAdminError(ctx.Context(), message, err)
	}
	if len(chats) == 0 {
		return b.sendAdminText(ctx.Context(), message, "No chats collected yet.")
	}

	lines := make([]string, 0, len(chats))
	for _, chat := range chats {
		label := strings.TrimSpace(chat.DisplayName)
		if label == "" {
			label = strings.TrimSpace(chat.Title)
		}
		if label == "" {
			label = chat.Username
		}
		if label == "" {
			label = "unnamed"
		}
		lines = append(lines, fmt.Sprintf("%d | %s | %s", chat.ChatID, chat.ChatType, label))
	}

	return b.sendAdminText(ctx.Context(), message, strings.Join(lines, "\n"))
}

func (b *Bot) configFieldsGlobalHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	return b.sendAdminText(ctx.Context(), message, formatConfigFields(b.admin.GlobalFields()))
}

func (b *Bot) configFieldsChatHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	return b.sendAdminText(ctx.Context(), message, formatConfigFields(b.admin.ChatFields()))
}

func (b *Bot) promptFeaturesHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	features := b.admin.PromptFeatures()
	lines := make([]string, 0, len(features))
	for _, feature := range features {
		lines = append(lines, string(feature))
	}

	return b.sendAdminText(ctx.Context(), message, strings.Join(lines, "\n"))
}

func (b *Bot) configShowHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	args := strings.Fields(message.Text)
	if len(args) == 1 {
		global, err := b.admin.Store().GetGlobalSettings(ctx.Context())
		if err != nil {
			return b.sendAdminError(ctx.Context(), message, err)
		}

		text := fmt.Sprintf(
			"global\ncharacter_name=%s\nlanguage=%s\ngender=%s\ntone_mode=%s\nallow_teasing=%t\ndefault_interactivity_mode=%s",
			global.CharacterName, global.Language, global.Gender, global.ToneMode, global.AllowTeasing, global.DefaultInteractivityMode,
		)

		return b.sendAdminText(ctx.Context(), message, text)
	}

	chatID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendAdminText(ctx.Context(), message, "Usage: /config_show [chat_id]")
	}

	chat, ok, err := b.admin.Store().GetChatSettings(ctx.Context(), chatID)
	if err != nil {
		return b.sendAdminError(ctx.Context(), message, err)
	}
	if !ok {
		return b.sendAdminText(ctx.Context(), message, "No chat overrides.")
	}

	allowTeasing := "<inherit>"
	if chat.AllowTeasing != nil {
		allowTeasing = strconv.FormatBool(*chat.AllowTeasing)
	}
	mode := "<inherit>"
	if chat.HasInteractivity {
		mode = string(chat.InteractivityMode)
	}

	text := fmt.Sprintf(
		"chat %d\ncharacter_name=%s\nlanguage=%s\ngender=%s\ntone_mode=%s\nallow_teasing=%s\ninteractivity_mode=%s",
		chatID,
		emptyFallback(chat.CharacterName),
		emptyFallback(chat.Language),
		emptyFallback(chat.Gender),
		emptyFallback(chat.ToneMode),
		allowTeasing,
		mode,
	)

	return b.sendAdminText(ctx.Context(), message, text)
}

func (b *Bot) configSetGlobalHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	field, value, ok := splitCommandArgs(message.Text, 3)
	if !ok {
		return b.sendAdminText(ctx.Context(), message, "Usage: /config_set_global <field> <value>")
	}

	if err := b.admin.Store().SetGlobalField(ctx.Context(), field, value, message.From.ID); err != nil {
		return b.sendAdminError(ctx.Context(), message, err)
	}

	return b.sendAdminText(ctx.Context(), message, "Updated global "+field+".")
}

func (b *Bot) configSetChatHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	args := strings.Fields(message.Text)
	if len(args) < 4 {
		return b.sendAdminText(ctx.Context(), message, "Usage: /config_set_chat <chat_id> <field> <value>")
	}
	chatID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendAdminText(ctx.Context(), message, "Invalid chat_id.")
	}
	field := args[2]
	value := strings.Join(args[3:], " ")
	if err := b.admin.Store().SetChatField(ctx.Context(), chatID, field, value, message.From.ID); err != nil {
		return b.sendAdminError(ctx.Context(), message, err)
	}

	return b.sendAdminText(ctx.Context(), message, fmt.Sprintf("Updated chat %d %s.", chatID, field))
}

func (b *Bot) configClearChatHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	args := strings.Fields(message.Text)
	if len(args) != 3 {
		return b.sendAdminText(ctx.Context(), message, "Usage: /config_clear_chat <chat_id> <field|all>")
	}
	chatID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendAdminText(ctx.Context(), message, "Invalid chat_id.")
	}
	if err := b.admin.Store().ClearChatField(ctx.Context(), chatID, args[2], message.From.ID); err != nil {
		return b.sendAdminError(ctx.Context(), message, err)
	}

	return b.sendAdminText(ctx.Context(), message, fmt.Sprintf("Cleared chat %d %s.", chatID, args[2]))
}

func (b *Bot) promptShowHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	args := strings.Fields(message.Text)
	if len(args) < 2 || len(args) > 3 {
		return b.sendAdminText(ctx.Context(), message, "Usage: /prompt_show <feature> [chat_id]")
	}

	feature := adminconfig.PromptFeature(args[1])
	if !adminconfig.IsPromptFeature(feature) {
		return b.sendAdminText(ctx.Context(), message, "Unknown prompt feature.")
	}
	chatID := int64(0)
	if len(args) == 3 {
		parsed, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return b.sendAdminText(ctx.Context(), message, "Invalid chat_id.")
		}
		chatID = parsed
	}

	body, ok, err := b.admin.Store().GetPromptTemplate(ctx.Context(), feature, chatID)
	if err != nil {
		return b.sendAdminError(ctx.Context(), message, err)
	}
	if !ok {
		return b.sendAdminText(ctx.Context(), message, "No stored prompt for that scope.")
	}

	return b.sendAdminText(ctx.Context(), message, body)
}

func (b *Bot) promptSetGlobalHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	args := strings.Fields(message.Text)
	if len(args) < 3 {
		return b.sendAdminText(ctx.Context(), message, "Usage: /prompt_set_global <feature> <template>")
	}
	feature := adminconfig.PromptFeature(args[1])
	body := strings.TrimSpace(strings.TrimPrefix(message.Text, args[0]+" "+args[1]))
	if err := adminconfig.ValidatePrompt(feature, body); err != nil {
		return b.sendAdminError(ctx.Context(), message, err)
	}
	if err := b.admin.Store().SetPromptTemplate(ctx.Context(), feature, 0, body, message.From.ID); err != nil {
		return b.sendAdminError(ctx.Context(), message, err)
	}

	return b.sendAdminText(ctx.Context(), message, "Updated global prompt.")
}

func (b *Bot) promptSetChatHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	args := strings.Fields(message.Text)
	if len(args) < 4 {
		return b.sendAdminText(ctx.Context(), message, "Usage: /prompt_set_chat <chat_id> <feature> <template>")
	}
	chatID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendAdminText(ctx.Context(), message, "Invalid chat_id.")
	}
	feature := adminconfig.PromptFeature(args[2])
	body := strings.TrimSpace(strings.TrimPrefix(message.Text, args[0]+" "+args[1]+" "+args[2]))
	if err := adminconfig.ValidatePrompt(feature, body); err != nil {
		return b.sendAdminError(ctx.Context(), message, err)
	}
	if err := b.admin.Store().SetPromptTemplate(ctx.Context(), feature, chatID, body, message.From.ID); err != nil {
		return b.sendAdminError(ctx.Context(), message, err)
	}

	return b.sendAdminText(ctx.Context(), message, "Updated chat prompt.")
}

func (b *Bot) promptClearChatHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	args := strings.Fields(message.Text)
	if len(args) != 3 {
		return b.sendAdminText(ctx.Context(), message, "Usage: /prompt_clear_chat <chat_id> <feature>")
	}
	chatID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendAdminText(ctx.Context(), message, "Invalid chat_id.")
	}
	feature := adminconfig.PromptFeature(args[2])
	if !adminconfig.IsPromptFeature(feature) {
		return b.sendAdminText(ctx.Context(), message, "Unknown prompt feature.")
	}
	if err := b.admin.Store().ClearPromptTemplate(ctx.Context(), feature, chatID); err != nil {
		return b.sendAdminError(ctx.Context(), message, err)
	}

	return b.sendAdminText(ctx.Context(), message, "Cleared chat prompt.")
}

func (b *Bot) whitelistAddHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	args := strings.Fields(message.Text)
	if len(args) != 2 {
		return b.sendAdminText(ctx.Context(), message, "Usage: /whitelist_add <chat_id>")
	}
	chatID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendAdminText(ctx.Context(), message, "Invalid chat_id.")
	}
	if err := b.admin.Store().AddWhitelistChat(ctx.Context(), chatID, message.From.ID); err != nil {
		return b.sendAdminError(ctx.Context(), message, err)
	}

	return b.sendAdminText(ctx.Context(), message, "Added chat to whitelist.")
}

func (b *Bot) whitelistRemoveHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	args := strings.Fields(message.Text)
	if len(args) != 2 {
		return b.sendAdminText(ctx.Context(), message, "Usage: /whitelist_remove <chat_id>")
	}
	chatID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendAdminText(ctx.Context(), message, "Invalid chat_id.")
	}
	if err := b.admin.Store().RemoveWhitelistChat(ctx.Context(), chatID); err != nil {
		return b.sendAdminError(ctx.Context(), message, err)
	}

	return b.sendAdminText(ctx.Context(), message, "Removed chat from whitelist.")
}

func (b *Bot) whitelistListHandler(ctx *th.Context, message t.Message) error {
	if !b.requireAdminDM(ctx, message) {
		return nil
	}

	chats, err := b.admin.Store().ListWhitelistChats(ctx.Context())
	if err != nil {
		return b.sendAdminError(ctx.Context(), message, err)
	}
	if len(chats) == 0 {
		return b.sendAdminText(ctx.Context(), message, "Whitelist is empty.")
	}

	lines := make([]string, 0, len(chats))
	for _, chatID := range chats {
		lines = append(lines, strconv.FormatInt(chatID, 10))
	}

	return b.sendAdminText(ctx.Context(), message, strings.Join(lines, "\n"))
}

func (b *Bot) requireAdminDM(ctx *th.Context, message t.Message) bool {
	if len(b.cfg.AdminIDs) == 0 {
		_ = b.sendAdminText(ctx.Context(), message, adminControlsDisabledText(message.From))

		return false
	}
	if !b.isAdminDM(&message) {
		_ = b.sendAdminText(ctx.Context(), message, "This command is available only in admin DMs.")

		return false
	}

	return true
}

func (b *Bot) sendAdminText(ctx context.Context, message t.Message, text string) error {
	_, err := b.api.SendMessage(ctx, b.reply(message, tu.Message(tu.ID(message.Chat.ID), text)))

	return err
}

func (b *Bot) sendAdminError(ctx context.Context, message t.Message, err error) error {
	return b.sendAdminText(ctx, message, "Admin command error: "+err.Error())
}

func adminControlsDisabledText(user *t.User) string {
	text := "Admin controls are disabled: BOT_ADMIN_IDS is empty."
	if user == nil {
		return text
	}

	userID := strconv.FormatInt(user.ID, 10)

	return text + "\nYour Telegram user ID: " + userID + "\nSet BOT_ADMIN_IDS=" + userID
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

func splitCommandArgs(text string, minArgs int) (string, string, bool) {
	args := strings.Fields(text)
	if len(args) < minArgs {
		return "", "", false
	}

	return args[1], strings.Join(args[2:], " "), true
}
