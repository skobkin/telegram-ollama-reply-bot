package adminconfig

import (
	"context"
	"time"
)

type InteractivityMode string

const (
	InteractivityDisabled        InteractivityMode = "disabled"
	InteractivityMentionsOnly    InteractivityMode = "mentions_only"
	InteractivityMentionsReplies InteractivityMode = "mentions_or_replies"
)

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
	Alias             string
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
	Alias             string
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
	AddWhitelistChat(ctx context.Context, chatID int64, actorID int64) error
	RemoveWhitelistChat(ctx context.Context, chatID int64) error
	ListWhitelistChats(ctx context.Context) ([]int64, error)
	IsChatWhitelisted(ctx context.Context, chatID int64) (bool, error)
}
