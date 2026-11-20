package tooluse

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/telegram/reactionavailability"

	t "github.com/mymmrac/telego"
)

type availableChat struct {
	calls     int
	reactions []t.ReactionType
}

func (g *availableChat) GetChat(context.Context, *t.GetChatParams) (*t.ChatFullInfo, error) {
	g.calls++

	return &t.ChatFullInfo{AvailableReactions: g.reactions}, nil
}

func TestReactionAvailabilityControlsEnumAndDispatch(tst *testing.T) {
	for _, empty := range []bool{false, true} {
		r, m := restrictedRuntime(tst, &stubActionSender{})
		getter := &availableChat{reactions: []t.ReactionType{}}
		if !empty {
			getter.reactions = append(getter.reactions, &t.ReactionTypeEmoji{Emoji: "👍"})
		}
		r.AttachReactionAvailability(reactionavailability.New(getter, nil))
		m.responses = []llm.Response{{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "try", Name: "set_reaction", Arguments: json.RawMessage(`{"emoji":"👍"}`)}}}}, {Message: llm.TextMessage(llm.RoleAssistant, "done")}}
		req := restrictedReactionRequest()
		req.CompleteAfterSideEffect = false
		result, err := r.ReplyWithTools(context.Background(), req)
		if err != nil {
			tst.Fatal(err)
		}
		if getter.calls != 1 {
			tst.Fatal("execution validation performed a lookup", getter.calls)
		}
		if empty {
			if result.ToolCalls[0].Status != "error" || len(m.lastBuildTools) != 0 {
				tst.Fatal(result, m.lastBuildTools)
			}
		} else {
			// The successful reaction is absent from the following iteration.
			if result.ToolCalls[0].Status != "ok" || len(m.lastBuildTools) != 0 {
				tst.Fatal(result, m.lastBuildTools)
			}
		}
	}
}

func TestReactionPresentationUsesOnlyAvailableCandidates(tst *testing.T) {
	r, _ := restrictedRuntime(tst, &stubActionSender{})
	g := &availableChat{reactions: []t.ReactionType{&t.ReactionTypeEmoji{Emoji: "❤"}}}
	r.AttachReactionAvailability(reactionavailability.New(g, nil))
	req := restrictedReactionRequest()
	run := &runState{}
	r.prepareReactionAvailability(context.Background(), req, run)
	defs := r.resolveDefinitions(req, run)
	var schema struct {
		Properties struct{ Emoji struct{ Enum []string } }
	}
	if len(defs) != 1 {
		tst.Fatal(defs)
	}
	if err := json.Unmarshal(defs[0].Parameters, &schema); err != nil {
		tst.Fatal(err)
	}
	if !slices.Equal(schema.Properties.Emoji.Enum, []string{"❤"}) {
		tst.Fatal(schema)
	}
	req.RequesterMediaGroupID = "album"
	r.prepareReactionAvailability(context.Background(), req, run)
	if len(r.resolveDefinitions(req, run)) != 0 || g.calls != 1 {
		tst.Fatal("album advertised or looked up")
	}
}
