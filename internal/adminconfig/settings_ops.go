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

// ClearGlobalSetting empties a clearable global field, restoring its unset
// semantics. For the generation model fields the unset state means the
// environment-configured model routes take effect again; for systemone_model
// it means the decision backend is unconfigured for chats without a per-chat
// override — clearing the global default does not disable the backend where
// overrides exist. Only fields marked Clearable in their spec may be cleared;
// booleans and enums have no unset state.
func (s *Service) ClearGlobalSetting(ctx context.Context, field string, actorID int64) error {
	spec, err := globalFieldSpecByName(field)
	if err != nil {
		return err
	}
	if !spec.Clearable {
		return fmt.Errorf("field %s has no unset state and cannot be cleared", field)
	}

	return s.store.SetGlobalField(ctx, field, "", actorID)
}

// ClearChatOverride removes one chat override so the field inherits the
// global value again.
func (s *Service) ClearChatOverride(ctx context.Context, chatID int64, field string, actorID int64) error {
	if _, err := chatFieldSpecByName(field); err != nil {
		return err
	}

	return s.store.ClearChatField(ctx, chatID, field, actorID)
}

// ClearAllChatOverrides removes every chat settings override for a chat in a
// single atomic store operation (fields only — prompt templates have their
// own reset operations). The store implementation is one SQL DELETE, so a
// failed reset cannot leave the chat partially reset: either every override
// is gone or none is.
func (s *Service) ClearAllChatOverrides(ctx context.Context, chatID int64, actorID int64) error {
	return s.store.ClearChatField(ctx, chatID, "all", actorID)
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
