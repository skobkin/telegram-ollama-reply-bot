package adminui

import (
	"context"

	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"telegram-ollama-reply-bot/internal/support/safeerr"
)

// Command menus: what each surface advertises. The default (all private
// chats) and group scopes list user commands only; each admin's private chat
// additionally gets the admin commands. /admin never appears in groups.
func defaultCommands() []t.BotCommand {
	return []t.BotCommand{
		{Command: "start", Description: "Restart the bot"},
		{Command: "help", Description: "Show help"},
		{Command: "summarize", Description: "Summarize the chat history"},
	}
}

func groupCommands() []t.BotCommand {
	return []t.BotCommand{
		{Command: "help", Description: "Show help"},
		{Command: "summarize", Description: "Summarize the chat history"},
	}
}

func adminCommands() []t.BotCommand {
	return []t.BotCommand{
		{Command: "admin", Description: "Open the admin panel"},
		{Command: "cancel", Description: "Cancel pending admin input"},
		{Command: "start", Description: "Restart the bot"},
		{Command: "help", Description: "Show help"},
		{Command: "summarize", Description: "Summarize the chat history"},
		{Command: "stats", Description: "Usage statistics (admins)"},
		{Command: "reset", Description: "Reset chat history (admins)"},
	}
}

// SyncCommandMenus pushes the command menus once at startup. A per-admin
// failure (the admin never started the bot — ScopeChat setMyCommands fails
// with CHAT_NOT_FOUND) is logged and retried lazily on that admin's next
// successful /admin.
func (a *Adapter) SyncCommandMenus(ctx context.Context) {
	if err := a.setCommands(ctx, tu.ScopeDefault(), defaultCommands()); err != nil {
		a.logger.Warn("admin ui cannot sync default command menu", "error", err)
	}
	if err := a.setCommands(ctx, tu.ScopeAllGroupChats(), groupCommands()); err != nil {
		a.logger.Warn("admin ui cannot sync group command menu", "error", err)
	}
	for _, adminID := range a.adminIDs {
		if err := a.setCommands(ctx, tu.ScopeChat(tu.ID(adminID)), adminCommands()); err != nil {
			a.logger.Warn("admin ui cannot sync admin command menu, will retry on next /admin",
				"admin_id", adminID, "error", err)
			a.menuMu.Lock()
			a.menuFailed[adminID] = true
			a.menuMu.Unlock()
		}
	}
}

// retryAdminMenu re-attempts a failed per-admin menu sync on a successful
// /admin (the chat is proven started by then).
func (a *Adapter) retryAdminMenu(ctx *th.Context, adminID int64) {
	a.menuMu.Lock()
	retry := a.menuFailed[adminID]
	a.menuMu.Unlock()
	if !retry {
		return
	}

	if err := a.setCommands(ctx, tu.ScopeChat(tu.ID(adminID)), adminCommands()); err != nil {
		a.logger.Debug("admin ui admin menu retry failed", "admin_id", adminID, "error", err)

		return
	}

	a.menuMu.Lock()
	delete(a.menuFailed, adminID)
	a.menuMu.Unlock()
}

func (a *Adapter) setCommands(ctx context.Context, scope t.BotCommandScope, commands []t.BotCommand) error {
	if err := a.api.SetMyCommands(ctx, &t.SetMyCommandsParams{
		Commands: commands,
		Scope:    scope,
	}); err != nil {
		// Sanitized here once: every caller logs the returned error.
		return safeerr.WrapTransport(errAdminMenuSync, err)
	}

	return nil
}
