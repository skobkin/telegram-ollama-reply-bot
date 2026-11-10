package adminconfig

import (
	"context"
	"fmt"
)

// SetGlobalSetting validates raw UI input for a global settings field and
// stores it. Validation runs before any store write, so an invalid value
// never reaches storage. access_policy is not a settings field and is
// rejected here — it is only writable through SetAccessPolicy.
func (s *Service) SetGlobalSetting(ctx context.Context, field, rawValue string, actorID int64) error {
	spec, err := globalFieldSpecByName(field)
	if err != nil {
		return err
	}

	value, err := spec.ParseAndValidate(rawValue)
	if err != nil {
		return err
	}

	return s.store.SetGlobalField(ctx, field, value, actorID)
}

// SetChatSetting validates raw UI input for a chat settings field and stores
// the override.
func (s *Service) SetChatSetting(ctx context.Context, chatID int64, field, rawValue string, actorID int64) error {
	spec, err := chatFieldSpecByName(field)
	if err != nil {
		return err
	}

	value, err := spec.ParseAndValidate(rawValue)
	if err != nil {
		return err
	}

	return s.store.SetChatField(ctx, chatID, field, value, actorID)
}

// ClearChatOverride removes one chat override so the field inherits the
// global value again.
func (s *Service) ClearChatOverride(ctx context.Context, chatID int64, field string, actorID int64) error {
	if _, err := chatFieldSpecByName(field); err != nil {
		return err
	}

	return s.store.ClearChatField(ctx, chatID, field, actorID)
}

// ClearAllChatOverrides removes every chat settings override for a chat
// (fields only — prompt templates have their own reset operations).
func (s *Service) ClearAllChatOverrides(ctx context.Context, chatID int64, actorID int64) error {
	for _, spec := range ChatFieldSpecs() {
		if err := s.store.ClearChatField(ctx, chatID, spec.Name, actorID); err != nil {
			return err
		}
	}

	return nil
}

func globalFieldSpecByName(field string) (FieldSpec, error) {
	for _, spec := range GlobalFieldSpecs() {
		if spec.Name == field {
			return spec, nil
		}
	}

	return FieldSpec{}, fmt.Errorf("unknown global settings field %q", field)
}

func chatFieldSpecByName(field string) (FieldSpec, error) {
	for _, spec := range ChatFieldSpecs() {
		if spec.Name == field {
			return spec, nil
		}
	}

	return FieldSpec{}, fmt.Errorf("unknown chat settings field %q", field)
}
