package interactivity

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/systemone"
)

// TestDeriveActivity pins the transient participation metadata: unknown
// never becomes a fabricated zero, and the recent window is inclusive.
func TestDeriveActivity(t *testing.T) {
	now := testTime()

	t.Run("no bot message reads as unknown", func(t *testing.T) {
		activity := deriveActivity([]state.Message{
			{MessageID: 1, Name: "A", CreatedAt: now.Add(-time.Minute)},
			{MessageID: 2, Name: "B", CreatedAt: now.Add(-30 * time.Second)},
		}, now)
		if activity.TimeSinceBotParticipation != "unknown" ||
			activity.MessagesSinceBotParticipation != "unknown" ||
			activity.BotSpokeRecently != "unknown" {
			t.Fatalf("expected all-unknown activity, got %+v", activity)
		}
	})

	t.Run("bot message yields durations and counts", func(t *testing.T) {
		activity := deriveActivity([]state.Message{
			{MessageID: 1, Name: "A", CreatedAt: now.Add(-2 * time.Hour)},
			{MessageID: 2, IsMe: true, Name: "Ada", CreatedAt: now.Add(-90 * time.Minute)},
			{MessageID: 3, Name: "B", CreatedAt: now.Add(-time.Minute)},
			{MessageID: 4, Name: "C", CreatedAt: now.Add(-30 * time.Second)},
		}, now)
		if activity.TimeSinceBotParticipation != "1h30m0s" {
			t.Fatalf("unexpected since duration %q", activity.TimeSinceBotParticipation)
		}
		if activity.MessagesSinceBotParticipation != "2" {
			t.Fatalf("unexpected message count %q", activity.MessagesSinceBotParticipation)
		}
		if activity.BotSpokeRecently != "no" {
			t.Fatalf("90m ago must not count as recent, got %q", activity.BotSpokeRecently)
		}
	})

	t.Run("recent participation boundary is inclusive", func(t *testing.T) {
		exactly := deriveActivity([]state.Message{
			{MessageID: 1, IsMe: true, CreatedAt: now.Add(-recentParticipationWindow)},
		}, now)
		if exactly.BotSpokeRecently != "yes" {
			t.Fatalf("participation exactly at the window must count as recent, got %q", exactly.BotSpokeRecently)
		}
	})
}

// TestHistoryEntryFor pins sender attribution and data rendering.
func TestHistoryEntryFor(t *testing.T) {
	bot := state.Message{IsMe: true, Name: "Ada", Text: "hello", Image: "a cat on a sofa"}
	entry := historyEntryFor(bot)
	if entry.Sender != "Ada (the bot)" || !entry.FromBot || entry.Image != "a cat on a sofa" {
		t.Fatalf("bot entry rendering wrong: %+v", entry)
	}

	unnamed := state.Message{Text: "hi", HasImage: true}
	entry = historyEntryFor(unnamed)
	if entry.Sender != "unknown" {
		t.Fatalf("nameless author must fall back to unknown, got %q", entry.Sender)
	}
	if entry.Image != "[image attached]" {
		t.Fatalf("unhydrated image must render the placeholder, got %q", entry.Image)
	}

	replier := state.Message{
		Name:    "Dana",
		Text:    "agreed",
		ReplyTo: &state.Message{Name: "Ivan", Text: "original thought"},
	}
	entry = historyEntryFor(replier)
	if entry.ReplyingTo != "Ivan: original thought" {
		t.Fatalf("reply attribution wrong: %q", entry.ReplyingTo)
	}
}

// TestBuildDecisionStateTargetExclusion pins that the target renders exactly
// once even though the history snapshot already contains it.
func TestBuildDecisionStateTargetExclusion(t *testing.T) {
	input := testInput()
	stateBody, err := json.Marshal(buildDecisionState(input, "persona", testHistory()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if got := strings.Count(string(stateBody), markerText); got != 1 {
		t.Fatalf("target must appear exactly once, got %d: %s", got, stateBody)
	}
	// The other history messages stay, the target is not duplicated into
	// the history array.
	if !strings.Contains(string(stateBody), "earlier message one") {
		t.Fatalf("history must keep the non-target messages: %s", stateBody)
	}
}

// composerFixture builds the fixed decision state used by the budget tests:
// a 200-byte summary, six 120-byte history messages, and mandatory sections.
func composerFixture() decisionState {
	persona := strings.Repeat("p", 300)
	instructions := strings.Repeat("i", 100)

	history := make([]historyEntry, 0, 6)
	for range 6 {
		history = append(history, historyEntry{Sender: "U", Text: strings.Repeat("m", 120)})
	}

	return decisionState{
		Persona:        persona,
		Instructions:   instructions,
		UntrustedNote:  historyNote,
		Activity:       activity{TimeSinceBotParticipation: "unknown", MessagesSinceBotParticipation: "unknown", BotSpokeRecently: "unknown"},
		EarlierSummary: strings.Repeat("s", 200),
		History:        history,
		Target:         historyEntry{Sender: "Dana", Text: strings.Repeat("t", 150)},
	}
}

// TestRequestComposerBudget pins the drop-oldest composition order with
// runtime-measured budgets: the earlier summary goes first, then history at
// message boundaries; persona, instructions, and the target are never
// truncated.
func TestRequestComposerBudget(t *testing.T) {
	questions := rubricQuestions()
	model := "laya"

	serialized := func(state decisionState) int {
		t.Helper()
		body, err := json.Marshal(systemone.Request{Model: model, State: state, Questions: questions})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		return len(body)
	}

	t.Run("one byte over the full request drops only the summary", func(t *testing.T) {
		stateIn := composerFixture()
		fullLen := serialized(stateIn)

		composer := requestComposer{budget: int64(fullLen - 1), persona: stateIn.Persona, state: stateIn, questions: questions}
		req, err := composer.request(model)
		if err != nil {
			t.Fatalf("compose: %v", err)
		}
		composed := req.State.(decisionState)
		if composed.EarlierSummary != "" {
			t.Fatalf("summary must drop before any history message")
		}
		if len(composed.History) != 6 {
			t.Fatalf("history must survive the summary-only drop, got %d messages", len(composed.History))
		}
	})

	t.Run("tight budget drops oldest messages at boundaries", func(t *testing.T) {
		stateIn := composerFixture()
		stateNoSummary := stateIn
		stateNoSummary.EarlierSummary = ""
		noSummaryLen := serialized(stateNoSummary)

		composer := requestComposer{budget: int64(noSummaryLen - 1), persona: stateIn.Persona, state: stateIn, questions: questions}
		req, err := composer.request(model)
		if err != nil {
			t.Fatalf("compose: %v", err)
		}
		composed := req.State.(decisionState)
		if composed.EarlierSummary != "" {
			t.Fatalf("summary must drop first")
		}
		if len(composed.History) >= 6 {
			t.Fatalf("history must drop oldest first until the request fits, got %d", len(composed.History))
		}
		// Message boundaries stay intact: the kept entries are the newest
		// ones, undivided.
		for _, entry := range composed.History {
			if entry.Text != strings.Repeat("m", 120) {
				t.Fatalf("messages must drop whole, got %+v", entry)
			}
		}
		if composed.Target.Text != stateIn.Target.Text || composed.Persona != stateIn.Persona || composed.Instructions != stateIn.Instructions {
			t.Fatalf("mandatory sections must never be truncated")
		}
	})

	t.Run("budget fits only the mandatory sections", func(t *testing.T) {
		stateIn := composerFixture()
		mandatory := stateIn
		mandatory.EarlierSummary = ""
		mandatory.History = nil
		mandatoryLen := serialized(mandatory)

		composer := requestComposer{budget: int64(mandatoryLen), persona: stateIn.Persona, state: stateIn, questions: questions}
		req, err := composer.request(model)
		if err != nil {
			t.Fatalf("compose: %v", err)
		}
		composed := req.State.(decisionState)
		if len(composed.History) != 0 || composed.EarlierSummary != "" {
			t.Fatalf("history and summary must drop entirely, got %d messages and summary %q", len(composed.History), composed.EarlierSummary)
		}
		if composed.Target.Text == "" || composed.Persona == "" {
			t.Fatalf("mandatory sections must survive")
		}
	})

	t.Run("one byte under the mandatory length fails closed", func(t *testing.T) {
		stateIn := composerFixture()
		mandatory := stateIn
		mandatory.EarlierSummary = ""
		mandatory.History = nil
		mandatoryLen := serialized(mandatory)

		composer := requestComposer{budget: int64(mandatoryLen - 1), persona: stateIn.Persona, state: stateIn, questions: questions}
		req, err := composer.request(model)
		if !errors.Is(err, ErrContextTooLarge) {
			t.Fatalf("expected ErrContextTooLarge, got %v", err)
		}
		if req.Model != "" || req.State != nil {
			t.Fatalf("no request may be returned on fail-closed")
		}
	})
}

// TestRequestComposerFailClosed pins the fail-closed outcome for an
// oversized mandatory input (a 22 KB synthetic persona against a small
// budget): a typed, content-free error, no request.
func TestRequestComposerFailClosed(t *testing.T) {
	persona := strings.Repeat("X", 22*1024) // synthetic large persona
	input := DecisionInput{Instructions: "stay terse"}
	stateIn := buildDecisionState(input, persona, HistorySnapshot{})

	composer := requestComposer{
		budget:    4 * 1024,
		persona:   persona,
		state:     stateIn,
		questions: rubricQuestions(),
	}

	req, err := composer.request("laya")
	if err == nil {
		t.Fatalf("oversized mandatory input must fail closed")
	}
	if !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("expected ErrContextTooLarge, got %v", err)
	}
	if req.Model != "" || req.State != nil {
		t.Fatalf("no request may be returned on fail-closed")
	}
	if strings.Contains(err.Error(), "XXXX") {
		t.Fatalf("fail-closed error must stay content-free, got %q", err.Error())
	}
}
