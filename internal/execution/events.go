// Package execution provides a small, typed, in-process event mechanism for
// observing the bot's conversation runs, LLM generation steps, and tool
// calls. Producers publish events through a Dispatcher; subscribers (such as
// the Telegram progress-preview renderer) consume them without coupling
// execution logic to presentation.
//
// Events describe execution state and completed actions. They are transient:
// nothing in this package persists anything, and event payloads deliberately
// exclude tool arguments, tool results, prompts, secrets, and raw error
// strings. Failure events carry short stable reason classifiers only.
package execution

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

const runIDLengthBytes = 8

// Event is the sealed set of execution events. The unexported marker keeps
// the set closed; new event families (e.g. interaction events) join this
// package without changes to the delivery machinery.
type Event interface {
	isExecutionEvent()
}

// RunContext correlates every event of one processing run. It is embedded by
// value in every event so subscribers can group events per run and keep
// concurrent runs in the same chat isolated from each other.
type RunContext struct {
	// RunID uniquely identifies one processing run.
	RunID string
	// ChatID is the Telegram chat the run serves.
	ChatID int64
	// TopicID is the forum topic of the run (0 when the chat has no topics).
	TopicID int
	// MessageID is the originating user message (0 when not message-bound).
	MessageID int
}

// Tool completion statuses. A tool is successful only when its actual result
// indicates success — a nil Go error with a structured error result is still
// a failure.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// Run failure reason classifiers. RunFailed never carries raw error strings.
const (
	// ReasonPrepare: building the LLM request failed before any generation.
	ReasonPrepare = "prepare"
	// ReasonBackend: the LLM backend returned an error.
	ReasonBackend = "backend"
	// ReasonTimeout: the processing deadline was exceeded.
	ReasonTimeout = "timeout"
	// ReasonCancelled: the run was cancelled via its context.
	ReasonCancelled = "cancelled"
	// ReasonLoopLimit: the tool loop iteration limit was reached.
	ReasonLoopLimit = "loop_limit"
	// ReasonNoChoices: the backend returned no usable choices.
	ReasonNoChoices = "no_choices"
)

// NewRunID returns a random run identifier (hex, mirroring the request ID
// format). It never fails: if the cryptographic source is unavailable it
// degrades to a time-based value, because a run ID only needs uniqueness
// within the process lifetime.
func NewRunID() string {
	buf := make([]byte, runIDLengthBytes)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("run-%x", time.Now().UnixNano())
	}

	return hex.EncodeToString(buf)
}

// RunStarted marks the beginning of a processing run.
type RunStarted struct {
	RunContext
	At time.Time
}

// RunCompleted marks a successful run completion. ReplyLength is the length
// of the final answer text in runes; it is 0 for side-effect-only completions
// (e.g. a tool performed an action and no text was produced).
type RunCompleted struct {
	RunContext
	At          time.Time
	Duration    time.Duration
	ReplyLength int
}

// RunFailed marks a failed run. Reason is a short stable classifier (see the
// Reason* constants) — never a raw error string, which could expose backend
// or tool internals.
type RunFailed struct {
	RunContext
	At       time.Time
	Duration time.Duration
	Reason   string
}

// GenerationStarted marks the beginning of one LLM generation step within a
// run. Iteration is the zero-based tool-loop iteration index.
type GenerationStarted struct {
	RunContext
	At        time.Time
	Iteration int
}

// GenerationCompleted marks one successful LLM generation step. FinishReason
// is the backend finish reason (e.g. "stop", "tool_calls"); ToolCallCount is
// the number of tool calls requested by the step.
type GenerationCompleted struct {
	RunContext
	At            time.Time
	Iteration     int
	Duration      time.Duration
	FinishReason  string
	ToolCallCount int
}

// ToolStarted marks the beginning of one tool invocation within a run.
// Internal reports whether the tool is classified as internal (context
// management); it is presentation metadata only.
type ToolStarted struct {
	RunContext
	At       time.Time
	CallID   string
	ToolName string
	Internal bool
}

// ToolCompleted marks the end of one tool invocation. Status is StatusOK or
// StatusError; Duration is the monotonic elapsed execution time. Events
// carry no arguments, results, or error details.
type ToolCompleted struct {
	RunContext
	At       time.Time
	CallID   string
	ToolName string
	Internal bool
	Duration time.Duration
	Status   string
}

// TextDelta carries a fragment of streamed user-facing answer text. It is
// transient by nature: subscribers must never persist it. In the current
// phase there is no producer — it is the extension point for the deferred
// LLM streaming work.
type TextDelta struct {
	RunContext
	At   time.Time
	Text string
}

func (RunStarted) isExecutionEvent()          {}
func (RunCompleted) isExecutionEvent()        {}
func (RunFailed) isExecutionEvent()           {}
func (GenerationStarted) isExecutionEvent()   {}
func (GenerationCompleted) isExecutionEvent() {}
func (ToolStarted) isExecutionEvent()         {}
func (ToolCompleted) isExecutionEvent()       {}
func (TextDelta) isExecutionEvent()           {}

// RunContextOf extracts the run context carried by any event. Every event
// embeds one, so the switch is exhaustive over the sealed set.
func RunContextOf(event Event) RunContext {
	switch e := event.(type) {
	case RunStarted:
		return e.RunContext
	case RunCompleted:
		return e.RunContext
	case RunFailed:
		return e.RunContext
	case GenerationStarted:
		return e.RunContext
	case GenerationCompleted:
		return e.RunContext
	case ToolStarted:
		return e.RunContext
	case ToolCompleted:
		return e.RunContext
	case TextDelta:
		return e.RunContext
	default:
		return RunContext{}
	}
}
