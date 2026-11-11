package adminconfig

import (
	"context"
	"strconv"
)

// ValueSource reports where an effective chat value comes from. It is
// existence-based, not equality-based: a chat override is an override even
// when its value happens to equal the global value.
type ValueSource string

const (
	// SourceGlobal means the value is inherited from global settings.
	SourceGlobal ValueSource = "global"
	// SourceChatOverride means a chat-level row stores this field.
	SourceChatOverride ValueSource = "chat_override"
)

// EffectiveField pairs a field spec with its effective value and origin.
type EffectiveField struct {
	Spec        FieldSpec
	Effective   string
	Source      ValueSource
	GlobalValue string
	Overridden  bool
}

// EffectiveGlobalConfig is the global settings screen model: raw settings
// plus the specs describing each editable field.
type EffectiveGlobalConfig struct {
	Settings GlobalSettings
	Fields   []FieldSpec
}

// EffectiveChatConfig is the per-chat settings model with per-field sources.
type EffectiveChatConfig struct {
	ChatID      int64
	Fields      []EffectiveField
	Allowlisted bool
}

// EffectiveGlobalConfig loads global settings together with the field specs.
func (s *Service) EffectiveGlobalConfig(ctx context.Context) (EffectiveGlobalConfig, error) {
	settings, err := s.store.GetGlobalSettings(ctx)
	if err != nil {
		return EffectiveGlobalConfig{}, err
	}

	return EffectiveGlobalConfig{Settings: settings, Fields: GlobalFieldSpecs()}, nil
}

// EffectiveChatConfig loads the chat card model: every chat field with its
// effective value, its source (inherited vs override), and the global value
// it would inherit. Chats without a settings row report every field as
// inherited.
func (s *Service) EffectiveChatConfig(ctx context.Context, chatID int64) (EffectiveChatConfig, error) {
	global, err := s.store.GetGlobalSettings(ctx)
	if err != nil {
		return EffectiveChatConfig{}, err
	}

	chat, found, err := s.store.GetChatSettings(ctx, chatID)
	if err != nil {
		return EffectiveChatConfig{}, err
	}

	specs := ChatFieldSpecs()
	fields := make([]EffectiveField, 0, len(specs))
	for _, spec := range specs {
		fields = append(fields, effectiveChatField(spec, global, chat, found))
	}

	allowlisted, err := s.store.IsChatWhitelisted(ctx, chatID)
	if err != nil {
		return EffectiveChatConfig{}, err
	}

	return EffectiveChatConfig{ChatID: chatID, Fields: fields, Allowlisted: allowlisted}, nil
}

func effectiveChatField(spec FieldSpec, global GlobalSettings, chat ChatSettings, found bool) EffectiveField {
	field := EffectiveField{Spec: spec, Source: SourceGlobal}

	switch spec.Name {
	case "character_name":
		field.GlobalValue = global.CharacterName
		if found && chat.CharacterName != "" {
			field.Effective, field.Source, field.Overridden = chat.CharacterName, SourceChatOverride, true
		}
	case "language":
		field.GlobalValue = global.Language
		if found && chat.Language != "" {
			field.Effective, field.Source, field.Overridden = chat.Language, SourceChatOverride, true
		}
	case "gender":
		field.GlobalValue = global.Gender
		if found && chat.Gender != "" {
			field.Effective, field.Source, field.Overridden = chat.Gender, SourceChatOverride, true
		}
	case "tone_mode":
		field.GlobalValue = global.ToneMode
		if found && chat.ToneMode != "" {
			field.Effective, field.Source, field.Overridden = chat.ToneMode, SourceChatOverride, true
		}
	case "allow_teasing":
		field.GlobalValue = strconv.FormatBool(global.AllowTeasing)
		if found && chat.AllowTeasing != nil {
			field.Effective, field.Source, field.Overridden = strconv.FormatBool(*chat.AllowTeasing), SourceChatOverride, true
		}
	case "interactivity_mode":
		field.GlobalValue = string(global.DefaultInteractivityMode)
		if found && chat.HasInteractivity && chat.InteractivityMode != "" {
			field.Effective, field.Source, field.Overridden = string(chat.InteractivityMode), SourceChatOverride, true
		}
	case "auto_summary_enabled":
		field.GlobalValue = strconv.FormatBool(global.DefaultAutoSummaryEnabled)
		if found && chat.AutoSummaryEnabled != nil {
			field.Effective, field.Source, field.Overridden = strconv.FormatBool(*chat.AutoSummaryEnabled), SourceChatOverride, true
		}
	}

	if field.Effective == "" {
		field.Effective = field.GlobalValue
	}

	return field
}
