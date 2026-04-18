# Repository Guidelines

## Project Structure

- Entrypoint: `cmd/bot`
- Runtime wiring: `internal/app`
- Config loading and defaults: `internal/config`
- Telegram handlers, history, and request flow: `internal/telegram/bot`
- LLM requests, prompt rendering, and model checks: `internal/llm`
- Article extraction: `internal/content/extractor`
- Shared support code: `internal/support/...`

## Working Rules

- This is a containerized Go server app.
- Do not write runtime data into the project directory.
- Prefer simple changes over speculative abstractions.
- Use `context.Context` on IO boundaries.
- When a feature grows large enough to hurt readability or context usage, split it into focused files instead of growing a single large file.

## Privacy And State

- Treat chat history, summaries, image metadata, caches, and similar runtime state as ephemeral unless the task explicitly introduces durable storage.
- Preserve the boundary between ephemeral conversational state and persistent operator-managed data.
- Do not introduce new durable storage for chat/user data without making the persistence decision explicit in code, tests, and docs.

## Logging And Safety

- Use structured logging with actionable fields.
- Keep normal log levels free of full prompts, raw chat history dumps, admin data, secrets, tokens, and large article or image payloads.
- DEBUG logging may include extra investigation detail when needed, but keep it narrowly scoped and do not leave broad sensitive dumps enabled by default.

## Testing And Completion

- New functionality should be designed for thorough testing and should come with proper coverage.
- When modifying existing functionality, add as much useful coverage as practical without large refactors.
- Small refactors are acceptable when they materially improve testability, readability, or regression protection.
- Avoid large refactors whose main outcome is a higher coverage number without meaningful engineering benefit.
- Before finishing normal tasks, format changed Go files, run `golangci-lint run ./cmd/bot/... ./internal/...`, and run `go test ./...`.
- For larger tasks, also build the binary into `/tmp`, not the project directory.
- Validate Docker only when the task changes Docker or build configuration.
- If `PLAN.md` exists and is relevant to the task, update it before finishing.

## Commits

- Use Conventional Commits consistent with the existing history.
