// Package stickers implements the operator-curated sticker catalog: the
// domain model, validation, the YAML export/import template, the pure
// import-planning (merge) logic and the immutable in-memory search index.
//
// The package has no Telegram or SQLite dependency. The catalog is
// administrator-managed durable data (separate from the volatile conversation
// history); the model can only search it and send by semantic ID.
package stickers
