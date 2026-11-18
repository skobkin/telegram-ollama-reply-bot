package adminconfig

import (
	"fmt"
	"strconv"
	"strings"
)

// FieldKind classifies how a settings field is edited and validated.
type FieldKind int

const (
	// KindString is a free-form text field. Most instances are single-line;
	// decision_instructions may span multiple lines and is edited through a
	// multi-line pending input in the UI.
	KindString FieldKind = iota
	// KindBool is a global boolean field with On/Off states only.
	KindBool
	// KindBoolTriState is a chat boolean field that may additionally inherit
	// the global value (inherit clears the override in storage).
	KindBoolTriState
	// KindEnum is a field restricted to a fixed set of supported values.
	KindEnum
)

// EnumOption is one selectable value of a KindEnum field.
type EnumOption struct {
	Value string
	Label string
}

// FieldSpec is metadata for one editable settings field. Specs are pure
// domain data: they carry no Telegram-specific encoding (callback codes and
// short button identifiers live in the UI layer).
//
// access_policy deliberately has no FieldSpec: it is not an ordinary settings
// field and may only be written through Service.SetAccessPolicy, which the
// admin UI guards behind an explicit confirmation.
type FieldSpec struct {
	// Name is the storage field name, e.g. "language".
	Name string
	// Label is a human-readable label for screens.
	Label string
	Kind  FieldKind
	// Enum lists the supported values of a KindEnum field.
	Enum []EnumOption
	// MaxLen is the byte limit for KindString input (0 means unlimited).
	MaxLen int
	// Clearable marks global string fields where an explicit empty value is
	// meaningful (model overrides returning to their environment route,
	// decision instructions cleared). Clearable fields are emptied through
	// Service.ClearGlobalSetting instead of a normal set; non-clearable
	// fields have no global unset.
	Clearable bool
}

// GlobalFieldSpecs describes the editable global settings fields.
func GlobalFieldSpecs() []FieldSpec {
	return []FieldSpec{
		{Name: "character_name", Label: "Character name", Kind: KindString, MaxLen: 128},
		{Name: "language", Label: "Language", Kind: KindString, MaxLen: 64},
		{Name: "gender", Label: "Gender", Kind: KindString, MaxLen: 64},
		{Name: "default_interactivity_mode", Label: "Default interactivity mode", Kind: KindEnum, Enum: InteractivityEnumOptions()},
		{Name: "default_auto_summary_enabled", Label: "Default auto summary", Kind: KindBool},
		{Name: "show_reply_previews", Label: "Show reply previews", Kind: KindBool},
		{Name: "default_preview_mode", Label: "Default preview mode", Kind: KindEnum, Enum: GlobalPreviewModeEnumOptions()},
		{Name: "keep_tool_history", Label: "Keep tool history", Kind: KindBool},
		{Name: "chat_model", Label: "Generation model", Kind: KindString, MaxLen: 128, Clearable: true},
		{Name: "summarize_model", Label: "Summarize model", Kind: KindString, MaxLen: 128, Clearable: true},
		{Name: "image_recognition_model", Label: "Image recognition model", Kind: KindString, MaxLen: 128, Clearable: true},
		{Name: "systemone_model", Label: "System One decision model", Kind: KindString, MaxLen: 128, Clearable: true},
		{Name: "default_interaction_profile", Label: "Default interaction profile", Kind: KindEnum, Enum: InteractionProfileEnumOptions()},
		{Name: "decision_instructions", Label: "Decision instructions", Kind: KindString, MaxLen: 4000, Clearable: true},
	}
}

// ChatFieldSpecs describes the editable per-chat settings fields.
func ChatFieldSpecs() []FieldSpec {
	return []FieldSpec{
		{Name: "character_name", Label: "Character name", Kind: KindString, MaxLen: 128},
		{Name: "language", Label: "Language", Kind: KindString, MaxLen: 64},
		{Name: "gender", Label: "Gender", Kind: KindString, MaxLen: 64},
		{Name: "interactivity_mode", Label: "Interactivity mode", Kind: KindEnum, Enum: InteractivityEnumOptions()},
		{Name: "auto_summary_enabled", Label: "Auto summary", Kind: KindBoolTriState},
		{Name: "preview_mode", Label: "Preview mode", Kind: KindEnum, Enum: ChatPreviewModeEnumOptions()},
		{Name: "keep_tool_history", Label: "Keep tool history", Kind: KindBoolTriState},
		{Name: "chat_model", Label: "Generation model", Kind: KindString, MaxLen: 128},
		{Name: "systemone_model", Label: "System One decision model", Kind: KindString, MaxLen: 128},
		{Name: "interaction_profile", Label: "Interaction profile", Kind: KindEnum, Enum: InteractionProfileEnumOptions()},
		{Name: "decision_instructions", Label: "Decision instructions", Kind: KindString, MaxLen: 4000},
	}
}

// InteractivityEnumOptions derives enum options from the supported
// interactivity modes so a new mode is picked up everywhere at once.
func InteractivityEnumOptions() []EnumOption {
	modes := InteractivityModes()
	options := make([]EnumOption, 0, len(modes))
	for _, mode := range modes {
		options = append(options, EnumOption{Value: string(mode), Label: InteractivityModeLabel(mode)})
	}

	return options
}

// ChatPreviewModeEnumOptions derives enum options from the supported
// per-chat preview modes (off/tools/all_tools; inherit is a separate
// clear-override action, mirroring the other inheritable chat fields).
func ChatPreviewModeEnumOptions() []EnumOption {
	modes := PreviewModes()
	options := make([]EnumOption, 0, len(modes))
	for _, mode := range modes {
		options = append(options, EnumOption{Value: string(mode), Label: PreviewModeLabel(mode)})
	}

	return options
}

// GlobalPreviewModeEnumOptions derives enum options from the supported
// global default preview modes (no off — the master switch governs that).
func GlobalPreviewModeEnumOptions() []EnumOption {
	modes := GlobalPreviewModes()
	options := make([]EnumOption, 0, len(modes))
	for _, mode := range modes {
		options = append(options, EnumOption{Value: string(mode), Label: PreviewModeLabel(mode)})
	}

	return options
}

// InteractionProfileEnumOptions derives enum options from the supported
// interaction profiles so a new profile is picked up everywhere at once.
func InteractionProfileEnumOptions() []EnumOption {
	profiles := InteractionProfiles()
	options := make([]EnumOption, 0, len(profiles))
	for _, profile := range profiles {
		options = append(options, EnumOption{Value: string(profile), Label: InteractionProfileLabel(profile)})
	}

	return options
}

// ParseAndValidate validates raw UI input for the field and returns the
// canonical storage value. Strings are trimmed and must be non-empty within
// MaxLen; booleans accept strconv forms and canonicalize to "true"/"false";
// enums must match a supported value exactly. The special tri-state value
// "inherit" is intentionally not handled here: inheriting maps to clearing
// the override, which is a separate storage operation.
func (spec FieldSpec) ParseAndValidate(raw string) (string, error) {
	switch spec.Kind {
	case KindEnum:
		for _, option := range spec.Enum {
			if raw == option.Value {
				return raw, nil
			}
		}

		return "", fmt.Errorf("field %s: unsupported value %q; supported values: %s", spec.Name, raw, spec.supportedEnumValues())
	case KindBool, KindBoolTriState:
		parsed, err := strconv.ParseBool(strings.ToLower(strings.TrimSpace(raw)))
		if err != nil {
			return "", fmt.Errorf("field %s: %q is not a boolean value", spec.Name, raw)
		}

		return strconv.FormatBool(parsed), nil
	case KindString:
		value := strings.TrimSpace(raw)
		if value == "" {
			return "", fmt.Errorf("field %s: value must not be empty", spec.Name)
		}
		if spec.MaxLen > 0 && len(value) > spec.MaxLen {
			return "", fmt.Errorf("field %s: value exceeds %d bytes", spec.Name, spec.MaxLen)
		}

		return value, nil
	default:
		return "", fmt.Errorf("field %s: unsupported field kind %d", spec.Name, spec.Kind)
	}
}

func (spec FieldSpec) supportedEnumValues() string {
	values := make([]string, 0, len(spec.Enum))
	for _, option := range spec.Enum {
		values = append(values, option.Value)
	}

	return strings.Join(values, ", ")
}

// EnumLabel returns the display label for a supported enum value, falling
// back to the raw value when unknown.
func (spec FieldSpec) EnumLabel(value string) string {
	for _, option := range spec.Enum {
		if option.Value == value {
			return option.Label
		}
	}

	return value
}

// InteractivityModeLabel returns the full human-readable label of an
// interactivity mode, used in screen text.
func InteractivityModeLabel(mode InteractivityMode) string {
	switch mode {
	case InteractivityDisabled:
		return "Disabled (commands only)"
	case InteractivityMentionsOnly:
		return "Mentions only"
	case InteractivityMentionsReplies:
		return "Mentions and replies"
	default:
		return string(mode)
	}
}

// ModeShortLabel returns a compact label for inline buttons on the chat card.
func ModeShortLabel(mode InteractivityMode) string {
	switch mode {
	case InteractivityDisabled:
		return "Disabled"
	case InteractivityMentionsOnly:
		return "Mentions"
	case InteractivityMentionsReplies:
		return "Mentions/Replies"
	default:
		return string(mode)
	}
}

// PreviewModeLabel returns the full human-readable label of a preview mode,
// used in screen text.
func PreviewModeLabel(mode PreviewMode) string {
	switch mode {
	case PreviewModeOff:
		return "Off (typing indicator)"
	case PreviewModeTools:
		return "Standard tools"
	case PreviewModeAllTools:
		return "All tools and durations"
	default:
		return string(mode)
	}
}

// PreviewModeShortLabel returns a compact label for inline buttons.
func PreviewModeShortLabel(mode PreviewMode) string {
	switch mode {
	case PreviewModeOff:
		return "Off"
	case PreviewModeTools:
		return "Tools"
	case PreviewModeAllTools:
		return "All tools"
	default:
		return string(mode)
	}
}
