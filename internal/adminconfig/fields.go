package adminconfig

import (
	"fmt"
	"strconv"
	"strings"
)

// FieldKind classifies how a settings field is edited and validated.
type FieldKind int

const (
	// KindString is a free-form single-line text field.
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
}

// GlobalFieldSpecs describes the seven editable global settings fields.
func GlobalFieldSpecs() []FieldSpec {
	return []FieldSpec{
		{Name: "character_name", Label: "Character name", Kind: KindString, MaxLen: 128},
		{Name: "language", Label: "Language", Kind: KindString, MaxLen: 64},
		{Name: "gender", Label: "Gender", Kind: KindString, MaxLen: 64},
		{Name: "tone_mode", Label: "Tone mode", Kind: KindString, MaxLen: 128},
		{Name: "allow_teasing", Label: "Allow teasing", Kind: KindBool},
		{Name: "default_interactivity_mode", Label: "Default interactivity mode", Kind: KindEnum, Enum: InteractivityEnumOptions()},
		{Name: "default_auto_summary_enabled", Label: "Default auto summary", Kind: KindBool},
	}
}

// ChatFieldSpecs describes the seven editable per-chat settings fields.
func ChatFieldSpecs() []FieldSpec {
	return []FieldSpec{
		{Name: "character_name", Label: "Character name", Kind: KindString, MaxLen: 128},
		{Name: "language", Label: "Language", Kind: KindString, MaxLen: 64},
		{Name: "gender", Label: "Gender", Kind: KindString, MaxLen: 64},
		{Name: "tone_mode", Label: "Tone mode", Kind: KindString, MaxLen: 128},
		{Name: "allow_teasing", Label: "Allow teasing", Kind: KindBoolTriState},
		{Name: "interactivity_mode", Label: "Interactivity mode", Kind: KindEnum, Enum: InteractivityEnumOptions()},
		{Name: "auto_summary_enabled", Label: "Auto summary", Kind: KindBoolTriState},
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
