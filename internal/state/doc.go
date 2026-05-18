// Package state defines privacy-first runtime state interfaces.
//
// The default implementation is in-memory only. Conversational history, rolling
// summaries, image descriptions, and similar request-serving data stay
// ephemeral unless a future task explicitly introduces durable storage.
package state
