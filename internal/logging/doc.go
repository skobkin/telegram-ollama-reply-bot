// Package logging configures process logging and scoped child loggers.
//
// Manager owns one active slog logger. Request helpers attach request-scoped
// loggers and request IDs to context so one incoming Telegram request can be
// traced across the bot runtime.
package logging
