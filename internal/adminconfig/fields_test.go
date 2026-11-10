package adminconfig

import (
	"strings"
	"testing"
)

func TestGlobalFieldSpecsMatchServiceFieldList(t *testing.T) {
	specs := GlobalFieldSpecs()
	if len(specs) != len(NewService(nil, nil, nil).GlobalFields()) {
		t.Fatalf("global specs %v do not match service fields %v", specNames(specs), NewService(nil, nil, nil).GlobalFields())
	}
	for i, spec := range specs {
		if spec.Name != NewService(nil, nil, nil).GlobalFields()[i] {
			t.Fatalf("global spec %d is %q, service lists %q", i, spec.Name, NewService(nil, nil, nil).GlobalFields()[i])
		}
		if spec.Label == "" {
			t.Fatalf("global spec %q has empty label", spec.Name)
		}
	}
}

func TestChatFieldSpecsMatchServiceFieldList(t *testing.T) {
	specs := ChatFieldSpecs()
	if len(specs) != len(NewService(nil, nil, nil).ChatFields()) {
		t.Fatalf("chat specs %v do not match service fields %v", specNames(specs), NewService(nil, nil, nil).ChatFields())
	}
	for i, spec := range specs {
		if spec.Name != NewService(nil, nil, nil).ChatFields()[i] {
			t.Fatalf("chat spec %d is %q, service lists %q", i, spec.Name, NewService(nil, nil, nil).ChatFields()[i])
		}
		if spec.Label == "" {
			t.Fatalf("chat spec %q has empty label", spec.Name)
		}
	}
}

func TestFieldSpecsExcludeAccessPolicy(t *testing.T) {
	// access_policy must never be an ordinary editable field: it is only
	// writable through the confirmation-gated Service.SetAccessPolicy.
	for _, spec := range append(GlobalFieldSpecs(), ChatFieldSpecs()...) {
		if spec.Name == "access_policy" {
			t.Fatalf("access_policy must not appear as a field spec")
		}
	}
}

func TestInteractivityEnumDerivedFromModes(t *testing.T) {
	specs := GlobalFieldSpecs()
	var spec FieldSpec
	for _, candidate := range specs {
		if candidate.Name == "default_interactivity_mode" {
			spec = candidate
		}
	}
	if spec.Kind != KindEnum {
		t.Fatalf("default_interactivity_mode must be an enum field")
	}

	modes := InteractivityModes()
	if len(spec.Enum) != len(modes) {
		t.Fatalf("enum options %v do not match modes %v", spec.Enum, modes)
	}
	for i, option := range spec.Enum {
		if option.Value != string(modes[i]) {
			t.Fatalf("enum option %d is %q, mode is %q", i, option.Value, modes[i])
		}
		if option.Label == "" {
			t.Fatalf("enum option %q has empty label", option.Value)
		}
		if ModeShortLabel(modes[i]) == "" {
			t.Fatalf("mode %q has empty short label", modes[i])
		}
	}
}

func TestParseAndValidateEnum(t *testing.T) {
	spec := FieldSpec{Name: "interactivity_mode", Kind: KindEnum, Enum: InteractivityEnumOptions()}

	value, err := spec.ParseAndValidate("mentions_only")
	if err != nil || value != "mentions_only" {
		t.Fatalf("expected mentions_only, got %q err %v", value, err)
	}

	_, err = spec.ParseAndValidate("proactive")
	if err == nil {
		t.Fatalf("expected unsupported enum value to be rejected")
	}
	if !strings.Contains(err.Error(), "mentions_only") {
		t.Fatalf("error should list supported values, got: %v", err)
	}
	if !strings.Contains(err.Error(), "proactive") {
		t.Fatalf("error should echo the rejected value, got: %v", err)
	}
}

func TestParseAndValidateBool(t *testing.T) {
	spec := FieldSpec{Name: "allow_teasing", Kind: KindBool}

	for _, raw := range []string{"true", "TRUE", " 1", "True"} {
		value, err := spec.ParseAndValidate(raw)
		if err != nil || value != "true" {
			t.Fatalf("raw %q: expected true, got %q err %v", raw, value, err)
		}
	}

	for _, raw := range []string{"false", "0", "F"} {
		value, err := spec.ParseAndValidate(raw)
		if err != nil || value != "false" {
			t.Fatalf("raw %q: expected false, got %q err %v", raw, value, err)
		}
	}

	if _, err := spec.ParseAndValidate("maybe"); err == nil {
		t.Fatalf("expected non-boolean to be rejected")
	}
}

func TestParseAndValidateString(t *testing.T) {
	spec := FieldSpec{Name: "character_name", Kind: KindString, MaxLen: 8}

	value, err := spec.ParseAndValidate("  kitsune ")
	if err != nil || value != "kitsune" {
		t.Fatalf("expected trimmed value, got %q err %v", value, err)
	}

	if _, err := spec.ParseAndValidate("   "); err == nil {
		t.Fatalf("expected empty string to be rejected")
	}

	if _, err := spec.ParseAndValidate("012345678"); err == nil {
		t.Fatalf("expected over-length value to be rejected")
	}
}

func TestEnumLabel(t *testing.T) {
	spec := FieldSpec{Kind: KindEnum, Enum: []EnumOption{{Value: "a", Label: "Alpha"}}}

	if got := spec.EnumLabel("a"); got != "Alpha" {
		t.Fatalf("expected label, got %q", got)
	}
	if got := spec.EnumLabel("zzz"); got != "zzz" {
		t.Fatalf("expected raw fallback, got %q", got)
	}
}

func specNames(specs []FieldSpec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}

	return names
}
