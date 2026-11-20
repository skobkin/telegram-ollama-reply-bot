package bot

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/llmcontext"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/statistics"
	"telegram-ollama-reply-bot/internal/tooluse"

	tg "github.com/mymmrac/telego"
)

// A controllable deadline context makes the post-work deadline race
// deterministic, without depending on scheduler timing or sleeps.
type silenceDeadlineContext struct {
	context.Context
	expired bool
}

func (c *silenceDeadlineContext) Err() error {
	if c.expired {
		return context.DeadlineExceeded
	}

	return nil
}

func TestProcessingDeadlineHonorsAcceptedSilence(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		for _, workErr := range []error{nil, context.Canceled, context.DeadlineExceeded} {
			t.Run(fmt.Sprintf("accepted=%t/error=%v", accepted, workErr), func(t *testing.T) {
				b := &Bot{stats: statistics.NewCollector(statistics.Config{})}
				ctx := &silenceDeadlineContext{Context: context.Background()}
				err := b.withinProcessingDeadline(ctx, func(context.Context) error {
					ctx.expired = true // expires after the tool's acceptance boundary

					return workErr
				}, func() bool { return accepted })
				if accepted && workErr == nil {
					if err != nil || b.stats.GeneralSnapshot().LlmTimeouts != 0 {
						t.Fatalf("accepted silence reclassified: %v / %+v", err, b.stats.GeneralSnapshot())
					}
				} else if !errors.Is(err, ErrRequestTimeout) || b.stats.GeneralSnapshot().LlmTimeouts != 1 {
					t.Fatalf("existing timeout handling lost: %v / %+v", err, b.stats.GeneralSnapshot())
				}
			})
		}
	}
}

type silenceEventHook func(execution.Event)

func (f silenceEventHook) Publish(event execution.Event) { f(event) }

func TestConversationTurnIntentionalSilence(t *testing.T) {
	for _, mode := range []string{"off", "group", "private"} {
		for _, reaction := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reaction=%t", mode, reaction), func(t *testing.T) {
				p := newProactiveTestBot(t, nil)
				wire := newPreviewServerState()
				p.bot.api = newPreviewTestBot(t, wire).api
				manager := newPreviewTestManager(t, p.bot)
				p.bot.previews = manager
				// Also prove that a failed preview deletion cannot turn
				// accepted silence into an error notice or generation retry.
				if reaction && mode == "group" {
					wire.armFailure("deleteMessage", 1)
				}
				msg := *pipelineGroupUpdate(21, "hello").Message
				msg.MessageThreadID = 77
				if mode == "private" {
					msg.Chat.Type, msg.Chat.ID = tg.ChatTypePrivate, 21
				}
				user := state.Message{MessageID: msg.MessageID, ChatID: msg.Chat.ID, FromID: 21, Text: msg.Text}
				p.history.AppendMessage(scopeFromMessage(msg), user)
				calls := []llm.ToolCall{}
				if reaction {
					calls = append(calls, llm.ToolCall{ID: "reaction", Name: "set_reaction", Arguments: []byte(`{"emoji":"🎉"}`)})
				}
				calls = append(calls, llm.ToolCall{ID: "silence", Name: "say_nothing", Arguments: []byte(`{}`)}, llm.ToolCall{ID: "skipped", Name: "send_dice", Arguments: []byte(`{}`)})
				response := llm.Response{Message: llm.TextMessage(llm.RoleAssistant, "must never publish"), Usage: llm.TokenUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5, Cost: 0.25}}
				response.Message.ToolCalls = calls
				gate := make(chan struct{})
				var once sync.Once
				release := func() { once.Do(func() { close(gate) }) }
				defer release()
				p.llm.gate, p.llm.responses = gate, []llm.Response{response}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				p.bot.tools = tooluse.New(p.llm, p.history, nil, nil, apiActionSender{api: p.bot.api}, nil, p.bot.logger, tooluse.Config{
					BotIdentity: tooluse.BotIdentity{UserID: testBotID.ID, Name: testBotID.FirstName},
					Dispatcher: silenceEventHook(func(event execution.Event) {
						manager.HandleExecutionEvent(context.Background(), event)
						if e, ok := event.(execution.ToolCompleted); ok && e.ToolName == "say_nothing" {
							cancel() // cancellation AFTER acceptance must stay silent
						}
					}),
				})
				resolved := adminconfig.ResolvedChatConfig{PreviewMode: adminconfig.PreviewModeAllTools, KeepToolHistory: true}
				if mode == "off" {
					resolved.PreviewMode = adminconfig.PreviewModeOff
				}
				done := make(chan turnEffects, 1)
				go func() {
					done <- p.bot.runConversationTurn(ctx, msg, user, resolved, turnOptions{trigger: llmcontext.TriggerMention, withPreview: true, typingFallback: true})
				}()
				if mode != "off" {
					method := "sendMessage"
					if mode == "private" {
						method = "sendRichMessageDraft"
					}
					waitForCondition(t, 2*time.Second, "preview before completion", func() bool { return wire.callCount(method) >= 1 })
				}
				release()
				var effects turnEffects
				select {
				case effects = <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("turn did not finish")
				}
				if effects != (turnEffects{ReactionConfirmed: reaction}) {
					t.Fatalf("unexpected effects: %+v", effects)
				}
				if p.llm.buildCount() != 1 || wire.callCount("sendRichMessage") != 0 || wire.callCount("sendDice") != 0 {
					t.Fatalf("unexpected followup or text delivery: builds=%d requests=%v", p.llm.buildCount(), wire.requests)
				}
				wantMessages, wantDeletes := 0, 0
				if mode == "group" {
					wantMessages, wantDeletes = 1, 1 // preview only; no answer or notice
				}
				if wire.callCount("sendMessage") != wantMessages || wire.callCount("deleteMessage") != wantDeletes {
					t.Fatalf("unexpected preview cleanup: messages=%d deletes=%d", wire.callCount("sendMessage"), wire.callCount("deleteMessage"))
				}
				beforeDrafts := wire.callCount("sendRichMessageDraft")
				time.Sleep(2 * manager.timings.draftRefresh)
				if wire.callCount("sendRichMessageDraft") != beforeDrafts {
					t.Fatal("private draft refreshed after intentional completion")
				}
				snapshot := p.history.Snapshot(scopeFromMessage(msg))
				if len(snapshot.Messages) != 1 || snapshot.Messages[0].IsMe || (len(snapshot.Messages[0].Reactions) == 1) != reaction {
					t.Fatalf("history changed incorrectly: %+v", snapshot.Messages)
				}
				usage := p.bot.stats.GeneralSnapshot()
				if usage.PromptTokens != 3 || usage.CompletionTokens != 2 || usage.TotalTokens != 5 || usage.TotalCost != 0.25 || usage.LlmTimeouts != 0 {
					t.Fatalf("accounting changed: %+v", usage)
				}
			})
		}
	}
}

func TestConversationTurnErrorDiscardsPreview(t *testing.T) {
	for _, failure := range []error{errors.New("backend failure"), context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			p := newProactiveTestBot(t, nil)
			wire := newPreviewServerState()
			p.bot.api = newPreviewTestBot(t, wire).api
			p.bot.previews = newPreviewTestManager(t, p.bot)
			gate := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(gate) }) }
			defer release()
			p.llm.gate, p.llm.err = gate, failure
			msg := *pipelineGroupUpdate(21, "question").Message
			done := make(chan turnEffects, 1)
			go func() {
				done <- p.bot.runConversationTurn(context.Background(), msg, state.Message{Text: msg.Text}, adminconfig.ResolvedChatConfig{PreviewMode: adminconfig.PreviewModeTools}, turnOptions{withPreview: true, typingFallback: true})
			}()
			waitForCondition(t, 2*time.Second, "preview", func() bool { return wire.callCount("sendMessage") == 1 })
			release()
			select {
			case effects := <-done:
				wantFailure := turnFailureGeneration
				if errors.Is(failure, context.DeadlineExceeded) {
					wantFailure = turnFailureTimeout
				}
				if effects.Failure != wantFailure {
					t.Fatalf("failure classification changed: %+v", effects)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("failed turn did not finish")
			}
			if wire.callCount("deleteMessage") != 1 || wire.callCount("sendMessage") != 2 {
				t.Fatalf("failure must delete preview and keep error notice: deletes=%d messages=%d", wire.callCount("deleteMessage"), wire.callCount("sendMessage"))
			}
		})
	}
}
