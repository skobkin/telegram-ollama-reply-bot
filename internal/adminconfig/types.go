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
	ToneMode                 string
	AllowTeasing             bool
	DefaultInteractivityMode InteractivityMode
	UpdatedAt                time.Time
	UpdatedBy                int64
}

type ChatSettings struct {
	ChatID            int64
	CharacterName     string
	Language          string
	Gender            string
	ToneMode          string
	AllowTeasing      *bool
	InteractivityMode InteractivityMode
	HasInteractivity  bool
	UpdatedAt         time.Time
	UpdatedBy         int64
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
	ToneMode          string
	AllowTeasing      bool
	InteractivityMode InteractivityMode
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
