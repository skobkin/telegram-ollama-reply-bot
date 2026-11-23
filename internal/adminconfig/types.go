package adminconfig

import (
	"context"
	"time"
)

type InteractivityMode string

const (
	// InteractivityDisabled means the bot ignores ordinary chat messages in that chat.
	// It still answers explicit admin DM commands and other dedicated command handlers.
	InteractivityDisabled InteractivityMode = "disabled"
	// InteractivityMentionsOnly means the bot answers only when directly mentioned
	// or when a configured soft trigger such as the character name is used.
	InteractivityMentionsOnly InteractivityMode = "mentions_only"
	// InteractivityMentionsReplies means the bot answers on mentions, replies to its
	// own messages, private chats, and configured soft triggers.
	InteractivityMentionsReplies InteractivityMode = "mentions_or_replies"
)

func InteractivityModes() []InteractivityMode {
	return []InteractivityMode{
		InteractivityDisabled,
		InteractivityMentionsOnly,
		InteractivityMentionsReplies,
	}
}

// InteractionProfile selects how proactively the bot participates in a chat
// when a message did not produce an explicit trigger. The values are stable
// storage identifiers for the five built-in decision profiles; their policy
// parameters live in the interactivity engine, not here.
type InteractionProfile string

const (
	// ProfileNonInteractive disables proactive participation entirely: the
	// decision engine is a NoOp that makes no backend call. This is the
	// global default.
	ProfileNonInteractive InteractionProfile = "non_interactive"
	// ProfileReactionsLow reacts rarely, never replies.
	ProfileReactionsLow InteractionProfile = "reactions_low"
	// ProfileReactionsHigh reacts more eagerly, never replies.
	ProfileReactionsHigh InteractionProfile = "reactions_high"
	// ProfileTalkLow reacts rarely and replies very conservatively.
	ProfileTalkLow InteractionProfile = "talk_low"
	// ProfileTalkHigh reacts and replies most eagerly of the built-ins.
	ProfileTalkHigh InteractionProfile = "talk_high"
)

// InteractionProfiles lists the supported interaction profile identifiers.
func InteractionProfiles() []InteractionProfile {
	return []InteractionProfile{
		ProfileNonInteractive,
		ProfileReactionsLow,
		ProfileReactionsHigh,
		ProfileTalkLow,
		ProfileTalkHigh,
	}
}

// IsValidInteractionProfile reports whether the value is a supported profile.
func IsValidInteractionProfile(profile InteractionProfile) bool {
	for _, supported := range InteractionProfiles() {
		if profile == supported {
			return true
		}
	}

	return false
}

// InteractionProfileLabel returns the display label of a profile, used in
// screen text.
func InteractionProfileLabel(profile InteractionProfile) string {
	switch profile {
	case ProfileNonInteractive:
		return "Non-interactive"
	case ProfileReactionsLow:
		return "Reactions (low)"
	case ProfileReactionsHigh:
		return "Reactions (high)"
	case ProfileTalkLow:
		return "Talk (low)"
	case ProfileTalkHigh:
		return "Talk (high)"
	default:
		return string(profile)
	}
}

type PreviewMode string

const (
	// PreviewModeOff disables previews; the bot keeps the plain typing
	// indicator. This is the effective mode whenever the global master
	// switch is off, regardless of overrides.
	PreviewModeOff PreviewMode = "off"
	// PreviewModeTools shows the neutral progress indicator and standard
	// tools only.
	PreviewModeTools PreviewMode = "tools"
	// PreviewModeAllTools shows all tools including internal ones, plus
	// execution durations.
	PreviewModeAllTools PreviewMode = "all_tools"
)

// PreviewModes lists the supported per-chat preview modes (includes off).
func PreviewModes() []PreviewMode {
	return []PreviewMode{PreviewModeOff, PreviewModeTools, PreviewModeAllTools}
}

// GlobalPreviewModes lists the supported global default modes. There is no
// global "off": the ShowReplyPreviews master switch governs that, so stored
// per-chat overrides survive a master toggle.
func GlobalPreviewModes() []PreviewMode {
	return []PreviewMode{PreviewModeTools, PreviewModeAllTools}
}

// IsValidPreviewMode reports whether the value is a supported preview mode.
func IsValidPreviewMode(mode PreviewMode) bool {
	for _, supported := range PreviewModes() {
		if mode == supported {
			return true
		}
	}

	return false
}

// IsValidGlobalPreviewMode reports whether the value is a supported global
// default preview mode (no off — see GlobalPreviewModes).
func IsValidGlobalPreviewMode(mode PreviewMode) bool {
	for _, supported := range GlobalPreviewModes() {
		if mode == supported {
			return true
		}
	}

	return false
}

// AccessPolicy decides which chats the bot serves beyond admin DMs.
type AccessPolicy string

const (
	// AccessPolicyAllowlist serves only chats on the whitelist. An empty
	// allowlist denies all ordinary chats (admin DMs stay exempt).
	AccessPolicyAllowlist AccessPolicy = "allowlist"
	// AccessPolicyAllowAll serves every chat regardless of whitelist
	// membership. Membership data is preserved and still shown.
	AccessPolicyAllowAll AccessPolicy = "allow_all"
)

// AccessPolicies lists the supported access policies.
func AccessPolicies() []AccessPolicy {
	return []AccessPolicy{AccessPolicyAllowlist, AccessPolicyAllowAll}
}

// IsValidAccessPolicy reports whether the value is a supported policy.
func IsValidAccessPolicy(policy AccessPolicy) bool {
	for _, supported := range AccessPolicies() {
		if policy == supported {
			return true
		}
	}

	return false
}

// WhitelistEntry is one allowlist membership row with audit fields.
type WhitelistEntry struct {
	ChatID  int64
	AddedAt time.Time
	AddedBy int64
}

type PromptFeature string

const (
	PromptFeatureChat             PromptFeature = "chat"
	PromptFeatureSummarize        PromptFeature = "summarize"
	PromptFeatureImageRecognition PromptFeature = "image_recognition"
)

type GlobalSettings struct {
	CharacterName            string
	Language                 string
	Gender                   string
	DefaultInteractivityMode InteractivityMode
	// DefaultAutoSummaryEnabled is the global default of the per-chat
	// auto-summarizer toggle. Chats without an explicit override inherit it.
	DefaultAutoSummaryEnabled bool
	// ShowReplyPreviews is the master switch for live reply previews. When
	// off, no previews are shown and per-chat preview overrides have no
	// effect (they stay stored for re-enabling).
	ShowReplyPreviews bool
	// DefaultPreviewMode is the preview mode for chats without an override.
	DefaultPreviewMode PreviewMode
	// KeepToolHistory controls whether completed tool calls are appended as
	// a collapsible section to final replies when previews are enabled.
	KeepToolHistory bool
	// WebSearchEnabled is the global default for whether the model may use
	// external web search. It only matters when a search backend is configured
	// in the environment; it can never create a search capability.
	WebSearchEnabled bool
	// ImageGenerationEnabled is the global default for whether the model may
	// generate images. It only matters when a generation backend is
	// configured in the environment (IMAGE_GENERATION__BACKEND); it can never
	// create a generation capability.
	ImageGenerationEnabled bool
	// ChatModel is the global default generation model override. "" means
	// unset: the model routes configured in the environment stay in effect.
	ChatModel string
	// SummarizeModel is the global default summarize model override. ""
	// means unset (environment route).
	SummarizeModel string
	// ImageRecognitionModel is the global default image-recognition model
	// override. "" means unset (environment route). It has no effect while
	// the active recognizer is MiniMax, which takes no model selection.
	ImageRecognitionModel string
	// ImageGenerationModel is the global image-generation model name passed to
	// the generation backend. There is no environment fallback (#139): ""
	// means no model is selected, and the generate_image tool stays
	// unavailable until an operator sets one. Global-only, like
	// ImageRecognitionModel.
	ImageGenerationModel string
	// SystemOneModel is the global default decision model used by the
	// interaction engine and the decision tools. Unlike the generation
	// models there is no environment fallback: "" means the decision backend
	// is unconfigured for chats without an explicit override.
	SystemOneModel string
	// DefaultInteractionProfile is the global default proactive-participation
	// profile. "" resolves to Non-Interactive.
	DefaultInteractionProfile InteractionProfile
	// DecisionInstructions carries operator guidance for the decision engine
	// (topics to engage with, expected social behavior). "" means unset.
	DecisionInstructions string
	UpdatedAt            time.Time
	UpdatedBy            int64
}

type ChatSettings struct {
	ChatID            int64
	CharacterName     string
	Language          string
	Gender            string
	InteractivityMode InteractivityMode
	HasInteractivity  bool
	// AutoSummaryEnabled opts the chat into automatic summarization of
	// linked long-form content. nil means inherit the global default.
	AutoSummaryEnabled *bool
	// PreviewMode overrides the global default preview mode. "" (has=false)
	// means inherit.
	PreviewMode    PreviewMode
	HasPreviewMode bool
	// KeepToolHistory overrides the global tool-history retention. nil means
	// inherit the global default.
	KeepToolHistory *bool
	// WebSearchEnabled overrides the global web search permission. nil means
	// inherit the global default.
	WebSearchEnabled *bool
	// ImageGenerationEnabled overrides the global image generation
	// permission. nil means inherit the global default.
	ImageGenerationEnabled *bool
	// ChatModel overrides the generation model. "" means inherit the global
	// default (which may itself be unset — the environment route applies).
	ChatModel string
	// SystemOneModel overrides the decision model. "" means inherit the
	// global default.
	SystemOneModel string
	// InteractionProfile overrides the proactive-participation profile.
	// "" means inherit the global default.
	InteractionProfile InteractionProfile
	// DecisionInstructions overrides the decision-engine guidance. "" means
	// inherit; an explicitly-empty override is not representable (cleared
	// overrides inherit), mirroring every other inheritable string field.
	DecisionInstructions string
	UpdatedAt            time.Time
	UpdatedBy            int64
}

type PromptTemplate struct {
	Feature   PromptFeature
	ScopeType string
	ScopeID   int64
	Template  string
	UpdatedAt time.Time
	UpdatedBy int64
}

type ChatCatalogEntry struct {
	ChatID      int64
	ChatType    string
	Title       string
	Username    string
	DisplayName string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

type ResolvedChatConfig struct {
	CharacterName     string
	Language          string
	Gender            string
	InteractivityMode InteractivityMode
	// AutoSummaryEnabled is the effective auto-summarizer switch for the
	// chat: the global default unless the chat overrides it.
	AutoSummaryEnabled bool
	// PreviewMode is the effective preview mode: off whenever the master
	// switch is off (overrides preserved but inert), otherwise the chat
	// override or the global default.
	PreviewMode PreviewMode
	// KeepToolHistory is the effective tool-history retention switch.
	KeepToolHistory bool
	// WebSearchEnabled is the effective web search permission: the chat
	// override, else the global default.
	WebSearchEnabled bool
	// ImageGenerationEnabled is the effective image generation permission:
	// the chat override, else the global default.
	ImageGenerationEnabled bool
	// ChatModel is the effective generation model override chain result:
	// the chat override, else the global default. "" means the environment
	// model routes stay in effect.
	ChatModel string
	// SummarizeModel is the effective summarize model. Summarize is a
	// global-only feature (no per-chat override exists), so this is the
	// global default for every chat. "" means the environment route applies.
	SummarizeModel string
	// ImageRecognitionModel is the effective image-recognition model, a
	// global-only setting like SummarizeModel. It has no effect while the
	// active recognizer is MiniMax, which takes no model selection.
	ImageRecognitionModel string
	// ImageGenerationModel is the effective image-generation model, a
	// global-only setting. "" means none is selected.
	ImageGenerationModel string
	// SystemOneModel is the effective decision model: the chat override,
	// else the global default. "" means the decision backend is
	// unconfigured for this chat.
	SystemOneModel string
	// InteractionProfile is the effective proactive-participation profile:
	// the chat override, else the global default, else Non-Interactive.
	InteractionProfile InteractionProfile
	// DecisionInstructions is the effective decision-engine guidance: the
	// chat override, else the global default.
	DecisionInstructions string
}

type Store interface {
	GetGlobalSettings(ctx context.Context) (GlobalSettings, error)
	SetGlobalField(ctx context.Context, field, value string, actorID int64) error
	GetChatSettings(ctx context.Context, chatID int64) (ChatSettings, bool, error)
	SetChatField(ctx context.Context, chatID int64, field, value string, actorID int64) error
	ClearChatField(ctx context.Context, chatID int64, field string, actorID int64) error
	GetPromptTemplate(ctx context.Context, feature PromptFeature, chatID int64) (string, bool, error)
	SetPromptTemplate(ctx context.Context, feature PromptFeature, chatID int64, body string, actorID int64) error
	ClearPromptTemplate(ctx context.Context, feature PromptFeature, chatID int64) error
	UpsertChatCatalog(ctx context.Context, entry ChatCatalogEntry) error
	ListChats(ctx context.Context) ([]ChatCatalogEntry, error)
	GetAccessPolicy(ctx context.Context) (AccessPolicy, error)
	SetAccessPolicy(ctx context.Context, policy AccessPolicy, actorID int64) error
	AddWhitelistChat(ctx context.Context, chatID int64, actorID int64) error
	RemoveWhitelistChat(ctx context.Context, chatID int64) error
	ListWhitelistChats(ctx context.Context) ([]int64, error)
	ListWhitelistEntries(ctx context.Context) ([]WhitelistEntry, error)
	IsChatWhitelisted(ctx context.Context, chatID int64) (bool, error)
}
