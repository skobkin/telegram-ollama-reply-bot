package bot

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/interactivity"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/llmcontext"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/support/markdown"
	"telegram-ollama-reply-bot/internal/systemone"
	"telegram-ollama-reply-bot/internal/tooluse"

	tg "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
)

// testBotID is the identity the harness bot reports through GetMe-free
// wiring: mention and reply predicates match against it.
var testBotID = botInfo{ID: 999, Username: "testbot", FirstName: "TestBot"}

// proactiveDebugLogger routes harness bot logs to stderr for diagnosis.
func proactiveDebugLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// turnStubLLM feeds the tool loop and records what the pipeline sent.
type turnStubLLM struct {
	mu        sync.Mutex
	responses []llm.Response
	err       error
	builds    int
	lastHint  string
}

func (s *turnStubLLM) BuildChatToolRequest(_ context.Context, _ llm.PromptScope, requestContext llm.ChatReplyContext, generationModel string, tools []llm.ToolDefinition, _ string, extraMessages []llm.Message) (llm.Request, error) {
	s.mu.Lock()
	s.builds++
	s.lastHint = requestContext.SystemHint
	s.mu.Unlock()

	return llm.Request{Feature: llm.FeatureChat, Model: generationModel, Tools: tools, Messages: extraMessages}, nil
}

func (s *turnStubLLM) Generate(_ context.Context, _ llm.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return llm.Response{}, s.err
	}
	if len(s.responses) == 0 {
		return llm.Response{Message: llm.TextMessage(llm.RoleAssistant, "stub answer")}, nil
	}

	resp := s.responses[0]
	s.responses = s.responses[1:]

	return resp, nil
}

func (s *turnStubLLM) buildCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.builds
}

// fakeDecisionEngine records Decide calls and scripts outcomes.
type fakeDecisionEngine struct {
	mu     sync.Mutex
	inputs []interactivity.DecisionInput
	// script receives the decision context so blocking scenarios can also
	// unblock on task cancellation — a script that waits forever would hang
	// the runner drain in test cleanup.
	script func(ctx context.Context, input interactivity.DecisionInput) (interactivity.ActionDecision, error)
}

func (f *fakeDecisionEngine) Decide(ctx context.Context, input interactivity.DecisionInput) (interactivity.ActionDecision, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, input)
	f.mu.Unlock()

	if f.script != nil {
		return f.script(ctx, input)
	}

	return interactivity.ActionDecision{Action: interactivity.ActionIgnore, Reason: interactivity.ReasonBelowThresholds}, nil
}

func (f *fakeDecisionEngine) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.inputs)
}

// fakePersonaSource counts persona renderings: the engine renders the
// persona only after its NoOp check and model resolution succeed.
type fakePersonaSource struct {
	calls atomic.Int32
}

func (f *fakePersonaSource) DecisionPersona(_ context.Context, _ int64, _ string, _ interactivity.PersonaSettings) (string, error) {
	f.calls.Add(1)

	return "You are the character under evaluation.", nil
}

// fakeDecisionClient records outgoing System One requests and scripts
// answers or failures.
type fakeDecisionClient struct {
	mu       sync.Mutex
	requests []systemone.Request
	answers  map[string]systemone.Answer
	err      error
}

func (f *fakeDecisionClient) Evaluate(_ context.Context, req systemone.Request) (map[string]systemone.Answer, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	answer, err := f.answers, f.err
	f.mu.Unlock()

	return answer, err
}

func (f *fakeDecisionClient) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.requests)
}

// countingSettingsStore counts settings reads, so the NoOp test can prove
// the proactive task reads the settings exactly once.
type countingSettingsStore struct {
	adminStoreStub
	globalReads atomic.Int32
	chatReads   atomic.Int32
}

// GetAccessPolicy isolates the proactive tests from the access layer: the
// gate must pass every chat so the eligibility matrix is what is observed.
func (s *countingSettingsStore) GetAccessPolicy(context.Context) (adminconfig.AccessPolicy, error) {
	return adminconfig.AccessPolicyAllowAll, nil
}

func (s *countingSettingsStore) GetGlobalSettings(ctx context.Context) (adminconfig.GlobalSettings, error) {
	s.globalReads.Add(1)

	return s.adminStoreStub.GetGlobalSettings(ctx)
}

func (s *countingSettingsStore) GetChatSettings(ctx context.Context, chatID int64) (adminconfig.ChatSettings, bool, error) {
	s.chatReads.Add(1)

	return s.adminStoreStub.GetChatSettings(ctx, chatID)
}

// countingHistoryStore counts history snapshots: the decision context read
// is the only Snapshot call an Ignore/NoOp evaluation may make.
type countingHistoryStore struct {
	state.ConversationStore
	snapshots atomic.Int32
}

func (s *countingHistoryStore) Snapshot(scope state.ConversationScope) state.ConversationSnapshot {
	s.snapshots.Add(1)

	return s.ConversationStore.Snapshot(scope)
}

// apiActionSender forwards tool actions to the fake-API-backed telego bot,
// so reactions land in the same captured request log as every other call.
type apiActionSender struct {
	api *tg.Bot
}

func (s apiActionSender) SendPoll(ctx context.Context, params *tg.SendPollParams) (*tg.Message, error) {
	return s.api.SendPoll(ctx, params)
}

func (s apiActionSender) SendDice(ctx context.Context, params *tg.SendDiceParams) (*tg.Message, error) {
	return s.api.SendDice(ctx, params)
}

func (s apiActionSender) SetMessageReaction(ctx context.Context, params *tg.SetMessageReactionParams) error {
	return s.api.SetMessageReaction(ctx, params)
}

// proactiveTestBot drives the REAL pipeline with the full turn wiring:
// reply-context builder, tool runtime over a stub LLM, and the fake Bot API.
type proactiveTestBot struct {
	bot     *Bot
	bh      *th.BotHandler
	api     *fakeAPIServer
	store   *countingSettingsStore
	history *countingHistoryStore
	llm     *turnStubLLM
	engine  *fakeDecisionEngine
}

func newProactiveTestBot(t *testing.T, configure func(*adminStoreStub)) *proactiveTestBot {
	t.Helper()

	api := newFakeAPIServer(t)
	botAPI, err := tg.NewBot("1:"+strings.Repeat("A", 35), tg.WithAPIServer(api.server.URL), tg.WithDiscardLogger())
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bh, err := th.NewBotHandler(botAPI, make(chan tg.Update))
	if err != nil {
		t.Fatalf("NewBotHandler: %v", err)
	}

	store := &countingSettingsStore{}
	if configure != nil {
		configure(&store.adminStoreStub)
	}
	memoryHistory := newMemoryConversationStore()
	history := &countingHistoryStore{ConversationStore: memoryHistory}

	testBot := &Bot{
		ctx:       context.Background(),
		logger:    proactiveDebugLogger(),
		cfg:       config.BotConfig{AdminIDs: []int64{7}, ProcessingTimeout: 5 * time.Second},
		history:   history,
		stats:     &pipelineStatsSpy{},
		admin:     adminconfig.NewService(store, nil, nil),
		api:       botAPI,
		me:        testBotID,
		sanitizer: markdown.NewTgMarkdownV2Sanitizer(),
	}
	testBot.replyCtx = llmcontext.NewReplyBuilder(history, testBot.hydrateMessagesWithImageDescriptions, 20, 40)
	stub := &turnStubLLM{}
	testBot.tools = tooluse.New(
		stub,
		memoryHistory,
		nil,
		nil,
		apiActionSender{api: botAPI},
		nil,
		slog.New(slog.NewTextHandler(os.Stderr, nil)),
		tooluse.Config{MaxIterations: 6, BotIdentity: tooluse.BotIdentity{UserID: testBotID.ID, Name: testBotID.FirstName, Username: testBotID.Username}},
	)
	testBot.registerRoutes(bh)

	return &proactiveTestBot{bot: testBot, bh: bh, api: api, store: store, history: history, llm: stub}
}

// wireFakeEngine injects the scripted engine, bypassing SetProactiveDecisions.
func (p *proactiveTestBot) wireFakeEngine(t *testing.T) *fakeDecisionEngine {
	t.Helper()

	engine := &fakeDecisionEngine{}
	p.bot.decisions = &proactiveDecisions{
		engineFor: func(adminconfig.ResolvedChatConfig) interactionDecider { return engine },
		budget:    96 * 1024,
	}
	p.bot.participation = newParticipationRunner(p.bot.ctx, 5*time.Second, p.bot.logger)
	t.Cleanup(p.bot.participation.Close)
	p.engine = engine

	return engine
}

// deliver pushes one update through the real pipeline synchronously.
func (p *proactiveTestBot) deliver(t *testing.T, update tg.Update) {
	t.Helper()

	if err := p.bh.BaseGroup().HandleUpdate(context.Background(), p.bot.api, update); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
}

// apiRequests lists every captured Bot API method, in order.
func (p *proactiveTestBot) apiRequests() []string {
	p.api.mu.Lock()
	defer p.api.mu.Unlock()

	methods := make([]string, 0, len(p.api.requests))
	for _, request := range p.api.requests {
		methods = append(methods, request.Method)
	}

	return methods
}

// deliveredCount counts delivered text messages across both send paths
// (plain sendMessage and the rich-message route).
func (p *proactiveTestBot) deliveredCount() int {
	return p.api.callCount("sendMessage") + p.api.callCount("sendRichMessage")
}

// lastDeliveredText extracts the text of the last delivered message on
// either send path.
func (p *proactiveTestBot) lastDeliveredText() string {
	for i := len(p.api.requests) - 1; i >= 0; i-- {
		request := p.api.requests[i]
		if request.Method != "sendMessage" && request.Method != "sendRichMessage" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(request.Body), &payload); err != nil {
			return ""
		}
		text, _ := payload["text"].(string)
		if text == "" {
			// Rich sends carry the content inside the rich_message object.
			if rich, ok := payload["rich_message"].(map[string]any); ok {
				text, _ = rich["markdown"].(string)
			}
		}

		return text
	}

	return ""
}

// assertNoEngineCall gives a wrongly-queued task a grace window to surface:
// the pipeline runs handlers synchronously, so a gate that wrongly passes
// spawns the decision goroutine during deliver and Decide lands within the
// window. Fails early on the first observed call.
func assertNoEngineCall(t *testing.T, engine *fakeDecisionEngine) {
	t.Helper()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if engine.calls() > 0 {
			t.Fatalf("engine must not be reached, saw %d call(s)", engine.calls())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// decisionAnswers builds a valid rubric answer set from semantic values:
// relevance as a raw score, everything else as noul probabilities.
func decisionAnswers(intrusion, reactSuitable, replySuitable float64) map[string]systemone.Answer {
	return map[string]systemone.Answer{
		// Relevance is always fully relevant: the raw maximum score.
		interactivity.QuestionRelevance: {Type: systemone.TypeScore, Score: 3.0},
		interactivity.QuestionIntrusion: {Type: systemone.TypeNoul, Noul: intrusion},
		// Contribution is always a clearly valuable message.
		interactivity.QuestionContribution:  {Type: systemone.TypeNoul, Noul: 1.0},
		interactivity.QuestionReactSuitable: {Type: systemone.TypeNoul, Noul: reactSuitable},
		interactivity.QuestionReplySuitable: {Type: systemone.TypeNoul, Noul: replySuitable},
	}
}

// --- update builders (mention/reply predicates match testBotID) ---

func proactiveGroupUpdate(text string) tg.Update {
	update := pipelineGroupUpdate(21, text)
	update.UpdateID = 44

	return update
}

func proactiveMentionUpdate() tg.Update {
	return tg.Update{UpdateID: 50, Message: &tg.Message{
		MessageID: 60,
		Chat:      tg.Chat{ID: -100999, Type: tg.ChatTypeSupergroup, Title: "Test Group"},
		Text:      "@testbot what do you think?",
		From:      &tg.User{ID: 21},
		Entities:  []tg.MessageEntity{{Type: tg.EntityTypeMention, Offset: 0, Length: 8}},
	}}
}

func proactiveReplyUpdate(text string) tg.Update {
	return tg.Update{UpdateID: 51, Message: &tg.Message{
		MessageID: 61,
		Chat:      tg.Chat{ID: -100999, Type: tg.ChatTypeSupergroup, Title: "Test Group"},
		Text:      text,
		From:      &tg.User{ID: 22},
		ReplyToMessage: &tg.Message{
			MessageID: 59,
			Chat:      tg.Chat{ID: -100999, Type: tg.ChatTypeSupergroup},
			From:      &tg.User{ID: testBotID.ID, IsBot: true, Username: testBotID.Username},
		},
	}}
}

func proactiveBotAuthorUpdate() tg.Update {
	return tg.Update{UpdateID: 52, Message: &tg.Message{
		MessageID: 62,
		Chat:      tg.Chat{ID: -100999, Type: tg.ChatTypeSupergroup, Title: "Test Group"},
		Text:      "another bot speaking",
		From:      &tg.User{ID: 500, IsBot: true, Username: "otherbot"},
	}}
}

// TestShouldEvaluateParticipationGate pins the eligibility gate cell-by-cell:
// private chats are never engine-eligible, disabled and unknown modes block
// the engine, bot senders are excluded, and the feature must be wired.
func TestShouldEvaluateParticipationGate(t *testing.T) {
	group := tg.Message{Chat: tg.Chat{ID: -100999, Type: tg.ChatTypeSupergroup}, From: &tg.User{ID: 21}}
	private := tg.Message{Chat: tg.Chat{ID: 100500, Type: tg.ChatTypePrivate}, From: &tg.User{ID: 21}}
	botAuthor := tg.Message{Chat: tg.Chat{ID: -100999, Type: tg.ChatTypeSupergroup}, From: &tg.User{ID: 500, IsBot: true}}

	wiredBot := &Bot{decisions: &proactiveDecisions{}}
	unwiredBot := &Bot{}

	cases := []struct {
		name     string
		bot      *Bot
		mode     adminconfig.InteractivityMode
		message  tg.Message
		eligible bool
	}{
		{"group mentions_only", wiredBot, adminconfig.InteractivityMentionsOnly, group, true},
		{"group mentions_or_replies", wiredBot, adminconfig.InteractivityMentionsReplies, group, true},
		{"group disabled", wiredBot, adminconfig.InteractivityDisabled, group, false},
		{"group unknown mode", wiredBot, adminconfig.InteractivityMode("everyone"), group, false},
		{"group unwired", unwiredBot, adminconfig.InteractivityMentionsReplies, group, false},
		{"private never eligible", wiredBot, adminconfig.InteractivityMentionsReplies, private, false},
		{"private disabled", wiredBot, adminconfig.InteractivityDisabled, private, false},
		{"bot sender excluded", wiredBot, adminconfig.InteractivityMentionsReplies, botAuthor, false},
		{"nil sender treated as human", wiredBot, adminconfig.InteractivityMentionsOnly, tg.Message{Chat: tg.Chat{ID: -100999, Type: tg.ChatTypeSupergroup}}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved := adminconfig.ResolvedChatConfig{InteractivityMode: tc.mode}
			if got := tc.bot.shouldEvaluateParticipation(resolved, tc.message); got != tc.eligible {
				t.Fatalf("eligibility = %v, want %v", got, tc.eligible)
			}
		})
	}
}

// TestProactiveEligibilityMatrix drives the §3 matrix through the real
// pipeline: engine-eligible non-explicit messages reach the engine, accepted
// explicit triggers produce turns and never reach the engine, and the
// preserved-drop cells stay silent.
func TestProactiveEligibilityMatrix(t *testing.T) {
	type cell struct {
		name      string
		mode      adminconfig.InteractivityMode
		character string
		update    func() tg.Update
		engine    bool // engine must evaluate the message
		explicit  bool // an explicit turn must run
	}

	cells := []cell{
		{
			name:   "ordinary message mentions_only is engine eligible",
			mode:   adminconfig.InteractivityMentionsOnly,
			update: func() tg.Update { return proactiveGroupUpdate("anyone up?") },
			engine: true,
		},
		{
			name:   "ordinary message mentions_or_replies is engine eligible",
			mode:   adminconfig.InteractivityMentionsReplies,
			update: func() tg.Update { return proactiveGroupUpdate("anyone up?") },
			engine: true,
		},
		{
			name:   "ordinary message disabled stays silent",
			mode:   adminconfig.InteractivityDisabled,
			update: func() tg.Update { return proactiveGroupUpdate("anyone up?") },
		},
		{
			name:      "soft trigger mentions_only is engine eligible",
			mode:      adminconfig.InteractivityMentionsOnly,
			character: "Aurora",
			update:    func() tg.Update { return proactiveGroupUpdate("hey aurora, nice weather") },
			engine:    true,
		},
		{
			name:     "mention mentions_only is explicit",
			mode:     adminconfig.InteractivityMentionsOnly,
			update:   proactiveMentionUpdate,
			explicit: true,
		},
		{
			name:     "mention mentions_or_replies is explicit",
			mode:     adminconfig.InteractivityMentionsReplies,
			update:   proactiveMentionUpdate,
			explicit: true,
		},
		{
			name:   "mention disabled stays silent",
			mode:   adminconfig.InteractivityDisabled,
			update: proactiveMentionUpdate,
		},
		{
			name:   "reply to bot mentions_only is engine eligible (formerly dropped)",
			mode:   adminconfig.InteractivityMentionsOnly,
			update: func() tg.Update { return proactiveReplyUpdate("thanks") },
			engine: true,
		},
		{
			name:     "reply to bot mentions_or_replies is explicit",
			mode:     adminconfig.InteractivityMentionsReplies,
			update:   func() tg.Update { return proactiveReplyUpdate("thanks") },
			explicit: true,
		},
		{
			name:      "reply to bot with character name mentions_only is explicit",
			mode:      adminconfig.InteractivityMentionsOnly,
			character: "Aurora",
			update:    func() tg.Update { return proactiveReplyUpdate("aurora, thanks") },
			explicit:  true,
		},
		{
			name:   "private non-admin mentions_only stays dropped",
			mode:   adminconfig.InteractivityMentionsOnly,
			update: func() tg.Update { return pipelinePrivateUpdate(21, "hello") },
		},
		{
			name:     "private non-admin mentions_or_replies is explicit",
			mode:     adminconfig.InteractivityMentionsReplies,
			update:   func() tg.Update { return pipelinePrivateUpdate(21, "hello") },
			explicit: true,
		},
		{
			name:     "private admin disabled is explicit",
			mode:     adminconfig.InteractivityDisabled,
			update:   func() tg.Update { return pipelinePrivateUpdate(7, "hello") },
			explicit: true,
		},
		{
			name:   "private admin mentions_only stays dropped",
			mode:   adminconfig.InteractivityMentionsOnly,
			update: func() tg.Update { return pipelinePrivateUpdate(7, "hello") },
		},
		{
			name:   "bot-authored message is never evaluated",
			mode:   adminconfig.InteractivityMentionsReplies,
			update: proactiveBotAuthorUpdate,
		},
	}

	for _, tc := range cells {
		t.Run(tc.name, func(t *testing.T) {
			p := newProactiveTestBot(t, func(s *adminStoreStub) {
				s.DefaultInteractivityMode = tc.mode
				s.CharacterName = tc.character
				s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
				s.SystemOneModel = "decision-model"
			})
			engine := p.wireFakeEngine(t)

			p.deliver(t, tc.update())

			switch {
			case tc.engine:
				waitFor(t, func() bool { return engine.calls() >= 1 })
				if got := p.llm.buildCount(); got != 0 {
					t.Fatalf("engine-eligible message must not run a turn, got %d builds", got)
				}
			case tc.explicit:
				waitFor(t, func() bool { return p.deliveredCount() >= 1 })
				// The explicit branch is exclusive: once the turn ran, the
				// engine branch was not taken.
				if got := engine.calls(); got != 0 {
					t.Fatalf("explicit trigger must never reach the engine, got %d calls", got)
				}
				if got := p.api.callCount("setMessageReaction"); got != 0 {
					t.Fatalf("explicit turn must not react, got %d reaction calls", got)
				}
			default:
				assertNoEngineCall(t, engine)
				if got := p.deliveredCount(); got != 0 {
					t.Fatalf("preserved drop must stay silent, got %d messages", got)
				}
				if got := p.llm.buildCount(); got != 0 {
					t.Fatalf("preserved drop must not run a turn, got %d builds", got)
				}
			}
		})
	}
}

// TestProactiveNoOpIsFree pins the Non-Interactive economics end-to-end:
// the engine is a NoOp inside the task — exactly one settings read per
// resolve point, no persona rendering, no System One HTTP, no history
// snapshot, and zero Telegram calls.
func TestProactiveNoOpIsFree(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		// Profile left unset: resolves to non_interactive. The decision
		// model is set, so a wrongly-ordered engine would proceed to render
		// the persona and call the client.
		s.SystemOneModel = "decision-model"
	})
	persona := &fakePersonaSource{}
	client := &fakeDecisionClient{answers: decisionAnswers(0.0, 1.0, 1.0)}
	stop := p.bot.SetProactiveDecisions(client, persona, 96*1024)
	t.Cleanup(stop)

	p.deliver(t, proactiveGroupUpdate("idle chatter"))

	// Two resolves total: the handler's gate resolve and the task's
	// operation snapshot after slot acquisition.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if p.store.globalReads.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // settle: nothing further may happen

	if got := p.store.globalReads.Load(); got != 2 {
		t.Fatalf("global settings reads = %d, want exactly 2 (handler gate + task snapshot)", got)
	}
	if got := p.store.chatReads.Load(); got != 2 {
		t.Fatalf("chat settings reads = %d, want exactly 2 (handler gate + task snapshot)", got)
	}
	if got := persona.calls.Load(); got != 0 {
		t.Fatalf("NoOp must not render the persona, got %d renders", got)
	}
	if got := client.calls(); got != 0 {
		t.Fatalf("NoOp must not call the backend, got %d calls", got)
	}
	if got := p.history.snapshots.Load(); got != 0 {
		t.Fatalf("NoOp must not read history at all, got %d snapshots", got)
	}
	if requests := p.apiRequests(); len(requests) != 0 {
		t.Fatalf("NoOp must make no Telegram calls, got %v", requests)
	}
	if got := p.llm.buildCount(); got != 0 {
		t.Fatalf("NoOp must not run a turn, got %d builds", got)
	}
}

// TestProactiveNoOpSkipsImageHydration pins the NoOp contract on photo
// messages: target hydration runs vision inference and Telegram file
// downloads, which an opted-out (Non-Interactive) chat must never pay for
// even with recognition enabled and the decision backend configured.
func TestProactiveNoOpSkipsImageHydration(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.SystemOneModel = "decision-model"
	})
	// Recognition is enabled, so a wrongly-ordered hydration would fetch
	// the file from the Bot API.
	p.bot.imageRecognition.Enabled = true
	persona := &fakePersonaSource{}
	client := &fakeDecisionClient{answers: decisionAnswers(0.0, 1.0, 1.0)}
	stop := p.bot.SetProactiveDecisions(client, persona, 96*1024)
	t.Cleanup(stop)

	p.deliver(t, tg.Update{UpdateID: 45, Message: &tg.Message{
		MessageID: 62,
		Chat:      tg.Chat{ID: -100999, Type: tg.ChatTypeSupergroup, Title: "Test Group"},
		From:      &tg.User{ID: 21},
		Photo:     []tg.PhotoSize{{FileID: "photo-file-id", Width: 1280, Height: 720}},
	}})

	// Settle: the queued task resolves its snapshot and must stop before
	// any hydration.
	time.Sleep(300 * time.Millisecond)

	if got := client.calls(); got != 0 {
		t.Fatalf("NoOp must not call the backend, got %d calls", got)
	}
	for _, request := range p.apiRequests() {
		if strings.Contains(request, "getFile") {
			t.Fatalf("NoOp must not download the photo, got %s", request)
		}
	}
	if got := p.llm.buildCount(); got != 0 {
		t.Fatalf("NoOp must not run a turn, got %d builds", got)
	}
}

// TestProactiveQueuedTaskHonorsModeChange pins the execution-time gate: a
// task that passed the pre-Submit eligibility window must not run once the
// operation snapshot says the mode flipped to disabled while it sat queued
// behind the chat slot.
func TestProactiveQueuedTaskHonorsModeChange(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
		s.SystemOneModel = "decision-model"
	})
	persona := &fakePersonaSource{}
	client := &fakeDecisionClient{answers: decisionAnswers(0.0, 1.0, 1.0)}
	stop := p.bot.SetProactiveDecisions(client, persona, 96*1024)
	t.Cleanup(stop)

	// The flip: by the time the task acquires the slot, the chat is disabled.
	// Eligibility at Submit time (mentions_or_replies, talk-low, human
	// sender) would have accepted this message.
	p.store.DefaultInteractivityMode = adminconfig.InteractivityDisabled

	p.bot.evaluateParticipation(context.Background(), *proactiveGroupUpdate("late chatter").Message)

	if got := client.calls(); got != 0 {
		t.Fatalf("disabled mode must block the queued task, got %d backend calls", got)
	}
	if got := p.llm.buildCount(); got != 0 {
		t.Fatalf("disabled mode must not run a turn, got %d builds", got)
	}
	if requests := p.apiRequests(); len(requests) != 0 {
		t.Fatalf("disabled mode must make no Telegram calls, got %v", requests)
	}
}

// TestProactiveReactTurnIsReactionOnly drives a real React decision through
// the restricted turn: the only Telegram call is setMessageReaction, the
// produced text is discarded and never recorded, and no preview or typing
// traffic exists.
func TestProactiveReactTurnIsReactionOnly(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.DefaultInteractionProfile = adminconfig.ProfileReactionsHigh
		s.SystemOneModel = "decision-model"
		s.ChatModel = "generation-model"
	})
	persona := &fakePersonaSource{}
	client := &fakeDecisionClient{
		// reactions_high: base = .45*1 + .55*1 - .35*.1 = .965 >= .45 react
		// threshold; react suitable 1.0 >= .60. Reply is disallowed by flags.
		answers: decisionAnswers(0.1, 1.0, 0.0),
	}
	stop := p.bot.SetProactiveDecisions(client, persona, 96*1024)
	t.Cleanup(stop)
	p.llm.mu.Lock()
	p.llm.responses = []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "set_reaction", Arguments: []byte(`{"emoji":"🎉"}`)},
		}}},
		{Message: llm.TextMessage(llm.RoleAssistant, "fallback text that must never be generated")},
	}
	p.llm.mu.Unlock()

	p.deliver(t, proactiveGroupUpdate("great news everyone"))

	waitFor(t, func() bool { return p.api.callCount("setMessageReaction") >= 1 })
	time.Sleep(150 * time.Millisecond) // settle: the completion must end the run

	requests := p.apiRequests()
	if len(requests) != 1 || requests[0] != "setMessageReaction" {
		t.Fatalf("react turn must issue exactly one setMessageReaction, got %v", requests)
	}
	if got := p.llm.buildCount(); got != 1 {
		t.Fatalf("completion must skip the next generation, got %d builds", got)
	}
	// The model must know the run is reaction-only before it writes text:
	// the system hint carries the restriction, unlike Reply and explicit
	// turns.
	p.llm.mu.Lock()
	hint := p.llm.lastHint
	p.llm.mu.Unlock()
	if !strings.Contains(hint, "Reaction-only run") {
		t.Fatalf("react turn hint must state the reaction-only restriction, got %q", hint)
	}
	// The suppressed text is never recorded: only the middleware-saved user
	// message is in the conversation.
	snapshot := p.history.Snapshot(state.ConversationScope{ChatID: -100999})
	if len(snapshot.Messages) != 1 || snapshot.Messages[0].IsMe {
		t.Fatalf("suppressed text must not be recorded, got %d messages", len(snapshot.Messages))
	}
}

// TestProactiveReplyRunsOrdinaryPath drives a real Reply decision: the text
// is delivered and recorded, and the reply context carries the proactive
// trigger.
func TestProactiveReplyRunsOrdinaryPath(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
		s.SystemOneModel = "decision-model"
	})
	persona := &fakePersonaSource{}
	client := &fakeDecisionClient{
		// talk_low: base = .45*1 + .55*1 - .50*.05 = .975 >= .90 reply
		// threshold; reply suitable 1.0 >= .70.
		answers: decisionAnswers(0.05, 0.0, 1.0),
	}
	stop := p.bot.SetProactiveDecisions(client, persona, 96*1024)
	t.Cleanup(stop)

	p.deliver(t, proactiveGroupUpdate("does anyone know a good pho place"))

	waitFor(t, func() bool { return p.deliveredCount() >= 1 })
	time.Sleep(150 * time.Millisecond) // settle: delivery and recording complete

	if text := p.lastDeliveredText(); text != "stub answer" {
		t.Fatalf("proactive reply must be delivered, got %q", text)
	}
	p.llm.mu.Lock()
	hint := p.llm.lastHint
	p.llm.mu.Unlock()
	if !strings.Contains(hint, string(llmcontext.TriggerProactive)) {
		t.Fatalf("reply context must carry the proactive trigger, got hint %q", hint)
	}
	if strings.Contains(hint, "Reaction-only run") {
		t.Fatalf("reply turns must not carry the reaction-only restriction, got hint %q", hint)
	}
	snapshot := p.history.Snapshot(state.ConversationScope{ChatID: -100999})
	if len(snapshot.Messages) != 2 {
		t.Fatalf("delivered proactive reply must be recorded, got %d messages", len(snapshot.Messages))
	}
	if !snapshot.Messages[1].IsMe || snapshot.Messages[1].Text != "stub answer" {
		t.Fatalf("recorded reply mismatch: %+v", snapshot.Messages[1])
	}
}

// TestProactiveDecisionContextCarriesReactions pins that bot-authored
// reactions recorded in history reach the decision request: they render with
// actor attribution and count as confirmed participation, while staying
// distinct from speaking (no bot message exists in this conversation).
func TestProactiveDecisionContextCarriesReactions(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
		s.SystemOneModel = "decision-model"
	})
	scope := state.ConversationScope{ChatID: -100999}
	// An earlier user message the bot has already reacted to: the store is
	// the same one the ordinary turn path records into.
	p.history.AppendMessage(scope, state.Message{
		MessageID: 90,
		Name:      "Dana",
		Text:      "earlier note worth a reaction",
		ChatID:    -100999,
		CreatedAt: time.Now().Add(-2 * time.Minute),
	})
	if !p.history.SetBotReaction(scope, 90, state.Reaction{
		UserID:    testBotID.ID,
		Name:      testBotID.FirstName,
		Username:  testBotID.Username,
		Emoji:     "🔥",
		UpdatedAt: time.Now().Add(-time.Minute),
	}) {
		t.Fatalf("bot reaction must be recorded into history")
	}
	persona := &fakePersonaSource{}
	client := &fakeDecisionClient{answers: decisionAnswers(0.05, 0.0, 1.0)}
	stop := p.bot.SetProactiveDecisions(client, persona, 96*1024)
	t.Cleanup(stop)

	p.deliver(t, proactiveGroupUpdate("fresh chatter for evaluation"))

	waitFor(t, func() bool { return client.calls() >= 1 })
	time.Sleep(100 * time.Millisecond) // settle: nothing further may be sent

	client.mu.Lock()
	requests := len(client.requests)
	body := ""
	if requests > 0 {
		encoded, err := json.Marshal(client.requests[0])
		if err != nil {
			client.mu.Unlock()
			t.Fatalf("marshal decision request: %v", err)
		}
		body = string(encoded)
	}
	client.mu.Unlock()

	if requests != 1 {
		t.Fatalf("decision must run exactly once, got %d backend calls", requests)
	}
	if !strings.Contains(body, `"by":"TestBot (the bot)"`) || !strings.Contains(body, `"by_bot":true`) {
		t.Fatalf("bot-authored reaction must render with actor attribution: %s", body)
	}
	if !strings.Contains(body, `"emoji":"🔥"`) {
		t.Fatalf("reaction emoji must reach the decision state: %s", body)
	}
	if !strings.Contains(body, `"bot_reacted_recently":"yes"`) {
		t.Fatalf("bot reaction must count as recent participation: %s", body)
	}
	if !strings.Contains(body, `"bot_spoke_recently":"unknown"`) {
		t.Fatalf("reacting must stay distinct from speaking: %s", body)
	}
	// The store stamps its own UpdatedAt at insertion, so only the shape can
	// be pinned here: a concrete duration — not "unknown" — must drive
	// participation time.
	if strings.Contains(body, `"time_since_bot_participation":"unknown"`) {
		t.Fatalf("the bot reaction must drive participation time: %s", body)
	}
}

// TestProactiveReplyFailureIsSilent pins the fail-silent posture: a failed
// proactive reply sends nothing user-visible — no notices, no typing — and
// leaves no user-facing trace.
func TestProactiveReplyFailureIsSilent(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
		s.SystemOneModel = "decision-model"
	})
	persona := &fakePersonaSource{}
	client := &fakeDecisionClient{answers: decisionAnswers(0.05, 0.0, 1.0)}
	stop := p.bot.SetProactiveDecisions(client, persona, 96*1024)
	t.Cleanup(stop)
	p.llm.mu.Lock()
	p.llm.err = llm.ErrLlmBackendRequestFailed
	p.llm.mu.Unlock()

	p.deliver(t, proactiveGroupUpdate("anything interesting today"))

	time.Sleep(200 * time.Millisecond) // settle: the failed turn must clean up quietly

	if got := p.deliveredCount(); got != 0 {
		t.Fatalf("failed proactive reply must send nothing, got %d messages", got)
	}
	if got := p.api.callCount("sendChatAction"); got != 0 {
		t.Fatalf("failed proactive reply must leave no typing indicator, got %d calls", got)
	}
	if got := client.calls(); got != 1 {
		t.Fatalf("decision must run exactly once, got %d backend calls", got)
	}
}

// TestProactiveDecisionFailureIsSilent pins that engine failures never
// surface: no Telegram traffic, no turn, and the evaluation stays
// content-free.
func TestProactiveDecisionFailureIsSilent(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
		s.SystemOneModel = "decision-model"
	})
	persona := &fakePersonaSource{}
	client := &fakeDecisionClient{err: systemone.ErrMalformed}
	stop := p.bot.SetProactiveDecisions(client, persona, 96*1024)
	t.Cleanup(stop)

	p.deliver(t, proactiveGroupUpdate("hello from the outage"))

	waitFor(t, func() bool { return client.calls() >= 1 })
	time.Sleep(150 * time.Millisecond)

	if requests := p.apiRequests(); len(requests) != 0 {
		t.Fatalf("failed decision must make no Telegram calls, got %v", requests)
	}
	if got := p.llm.buildCount(); got != 0 {
		t.Fatalf("failed decision must not run a turn, got %d builds", got)
	}
}

// TestExplicitRunsAlongsideProactive pins that an explicit turn is never
// delayed or blocked by proactive per-chat serialization: both complete.
func TestExplicitRunsAlongsideProactive(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
		s.SystemOneModel = "decision-model"
	})
	engine := p.wireFakeEngine(t)

	// The proactive task is queued first and blocks the chat slot until the
	// explicit turn below has fully completed.
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	engine.script = func(ctx context.Context, _ interactivity.DecisionInput) (interactivity.ActionDecision, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
		}

		return interactivity.ActionDecision{Action: interactivity.ActionIgnore, Reason: interactivity.ReasonBelowThresholds}, nil
	}

	p.deliver(t, proactiveGroupUpdate("queued first"))
	<-started

	// The explicit mention runs to completion while the slot is held.
	p.deliver(t, proactiveMentionUpdate())
	if got := p.deliveredCount(); got == 0 {
		t.Fatalf("explicit turn must run while a proactive task holds the chat slot")
	}

	close(release)
	waitFor(t, func() bool { return engine.calls() >= 1 })
}

// TestParticipationRunnerSerializesPerChat pins the runner's concurrency
// contract: one task in flight per chat, queued tasks all run, different
// chats proceed concurrently, and shutdown drains cleanly.
func TestParticipationRunnerSerializesPerChat(t *testing.T) {
	runner := newParticipationRunner(context.Background(), 5*time.Second, reactionTestLogger())

	var chat1InFlight atomic.Int32
	var chat1Max atomic.Int32
	var completed atomic.Int32

	// task tracks only chat 1's concurrency: chat 2 proves cross-chat
	// concurrency is allowed, so its task must not pollute the per-chat
	// in-flight bound.
	task := func(track bool) func(context.Context) {
		return func(_ context.Context) {
			if track {
				current := chat1InFlight.Add(1)
				for {
					old := chat1Max.Load()
					if current <= old || chat1Max.CompareAndSwap(old, current) {
						break
					}
				}
			}
			time.Sleep(20 * time.Millisecond)
			if track {
				chat1InFlight.Add(-1)
			}
			completed.Add(1)
		}
	}

	for i := 0; i < 3; i++ {
		if !runner.Submit(1, task(true)) {
			t.Fatalf("submit %d rejected before shutdown", i)
		}
	}
	// A different chat must not wait on chat 1's queue.
	if !runner.Submit(2, task(false)) {
		t.Fatalf("chat 2 submit rejected")
	}

	waitFor(t, func() bool { return completed.Load() >= 4 })

	if got := chat1Max.Load(); got != 1 {
		t.Fatalf("per-chat serialization violated, %d tasks of one chat ran concurrently", got)
	}

	runner.Close()
	if runner.Submit(1, func(context.Context) {}) {
		t.Fatalf("submit after close must be rejected")
	}
	if got := completed.Load(); got != 4 {
		t.Fatalf("close must not drop completed accounting, got %d", got)
	}
}

// TestParticipationRunnerShutdownDrains pins the lifecycle: cancellation
// ends waiting tasks before they start, executing tasks observe the cancel
// through their context, Close blocks until every task exits, and new
// submissions are rejected afterwards.
func TestParticipationRunnerShutdownDrains(t *testing.T) {
	runner := newParticipationRunner(context.Background(), 5*time.Second, reactionTestLogger())

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	observedCtx := make(chan context.Context, 1)
	runner.Submit(1, func(ctx context.Context) {
		started <- struct{}{}
		observedCtx <- ctx
		select {
		case <-release:
		case <-ctx.Done():
		}
	})

	<-started
	// A queued task behind the holder must be cancellable while waiting.
	queuedRan := make(chan struct{})
	runner.Submit(1, func(context.Context) { close(queuedRan) })

	runner.Close() // cancels and joins: the holder aborts via its context

	select {
	case <-queuedRan:
		t.Fatalf("queued task must not start after cancellation")
	case <-time.After(100 * time.Millisecond):
	}

	ctx := <-observedCtx
	if err := ctx.Err(); err == nil {
		t.Fatalf("executing task must observe the shutdown cancellation")
	}

	if runner.Submit(1, func(context.Context) {}) {
		t.Fatalf("submit after close must be rejected")
	}
}

// TestParticipationRunnerZeroTimeoutHasNoDeadline pins the timeout-0
// semantics: a non-positive processing timeout must give tasks a live,
// deadline-free context (mirroring withProcessingDeadline for explicit
// turns), not an instantly-expired one that would abort every decision.
func TestParticipationRunnerZeroTimeoutHasNoDeadline(t *testing.T) {
	runner := newParticipationRunner(context.Background(), 0, reactionTestLogger())

	started := make(chan struct{}, 1)
	observed := make(chan context.Context, 1)
	runner.Submit(1, func(ctx context.Context) {
		started <- struct{}{}
		observed <- ctx
		<-ctx.Done() // ends when Close cancels the lifecycle
	})

	<-started
	ctx := <-observed
	if err := ctx.Err(); err != nil {
		t.Fatalf("task context must be live with timeout 0, got %v", err)
	}
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		t.Fatalf("task context must carry no deadline with timeout 0")
	}

	runner.Close()
}

// TestSnapshotModelsSharesOperationSnapshot pins the resolver adapter: the
// engine's model resolution reads the operation snapshot, not the settings
// store — unset stays unset and is never turned into an error.
func TestSnapshotModelsSharesOperationSnapshot(t *testing.T) {
	resolved := adminconfig.ResolvedChatConfig{
		ChatModel:      "generation-model",
		SystemOneModel: "decision-model",
	}
	models := snapshotModels{resolved: resolved}

	generation, err := models.GenerationModel(context.Background(), 1)
	if err != nil || generation != "generation-model" {
		t.Fatalf("generation model = %q, %v", generation, err)
	}
	decision, err := models.DecisionModel(context.Background(), 1)
	if err != nil || decision != "decision-model" {
		t.Fatalf("decision model = %q, %v", decision, err)
	}

	unset := snapshotModels{resolved: adminconfig.ResolvedChatConfig{}}
	if generation, _ := unset.GenerationModel(context.Background(), 1); generation != "" {
		t.Fatalf("unset generation model must stay empty, got %q", generation)
	}
	if decision, _ := unset.DecisionModel(context.Background(), 1); decision != "" {
		t.Fatalf("unset decision model must stay empty, got %q", decision)
	}
}
