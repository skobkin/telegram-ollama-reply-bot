package execution

import (
	"testing"
	"time"
)

// TestRunContextOfCoversSealedEventFamily proves RunContextOf extracts the
// embedded context for every member of the sealed event set. A missed case
// silently returns a zero context and breaks run correlation for subscribers.
func TestRunContextOfCoversSealedEventFamily(t *testing.T) {
	at := time.Unix(1700000000, 0)
	rc := RunContext{RunID: "run-1", ChatID: -100, TopicID: 7, MessageID: 42}

	events := []Event{
		RunStarted{RunContext: rc, At: at},
		RunCompleted{RunContext: rc, At: at, Duration: time.Second, ReplyLength: 3},
		RunFailed{RunContext: rc, At: at, Duration: time.Second, Reason: ReasonBackend},
		GenerationStarted{RunContext: rc, At: at, Iteration: 1},
		GenerationCompleted{RunContext: rc, At: at, Iteration: 1, Duration: time.Second, FinishReason: "stop", ToolCallCount: 2},
		ToolStarted{RunContext: rc, At: at, CallID: "c1", ToolName: "t", Internal: false},
		ToolCompleted{RunContext: rc, At: at, CallID: "c1", ToolName: "t", Internal: false, Duration: time.Second, Status: StatusOK},
		TextDelta{RunContext: rc, At: at, Text: "x"},
		ReactionChanged{RunContext: rc, At: at, TargetMessageID: 42, UserID: 7, UpdateID: 1, Reactions: []string{"👍"}},
		MessageSent{RunContext: rc, At: at, MessageID: 42},
		BotReactionSet{RunContext: rc, At: at, TargetMessageID: 42, BotUserID: 5, Emoji: "👍"},
	}

	for _, event := range events {
		if got := RunContextOf(event); got != rc {
			t.Errorf("RunContextOf(%T) = %+v, want %+v", event, got, rc)
		}
	}
}

// TestBotReactionSetIsSealedEventMember proves the new event joined the
// sealed family: it satisfies the Event interface and its marker exists.
func TestBotReactionSetIsSealedEventMember(t *testing.T) {
	var event Event = BotReactionSet{RunContext: RunContext{RunID: "r"}, At: time.Now(), Emoji: "👍"}
	if event == nil {
		t.Fatal("BotReactionSet must satisfy execution.Event")
	}

	if _, ok := event.(BotReactionSet); !ok {
		t.Fatal("BotReactionSet must round-trip through the Event interface")
	}
}
