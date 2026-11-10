package adminconfig

import (
	"context"
	"fmt"
)

// AllowlistEntry is a whitelist membership enriched with catalog display
// information for rendering.
type AllowlistEntry struct {
	WhitelistEntry
	ChatType    string
	Title       string
	Username    string
	DisplayName string
}

// AccessSummary is the dashboard-level access overview.
type AccessSummary struct {
	Policy         AccessPolicy
	AllowlistCount int
}

// ShouldAllowChat is the single access decision point. Admin DMs are always
// allowed. Under allow_all every chat is allowed. Under allowlist only
// whitelisted chats are allowed — an empty allowlist denies all ordinary
// chats (there is deliberately no implicit "empty means allow everyone").
func (s *Service) ShouldAllowChat(ctx context.Context, chatID int64, isAdminDM bool) (bool, error) {
	if isAdminDM {
		return true, nil
	}

	policy, err := s.AccessPolicy(ctx)
	if err != nil {
		return false, err
	}
	if policy == AccessPolicyAllowAll {
		return true, nil
	}

	return s.store.IsChatWhitelisted(ctx, chatID)
}

// AccessPolicy loads the effective policy. Unknown stored values (possible
// only through manual DB edits) normalize to allowlist so the failure mode
// stays closed rather than wide open.
func (s *Service) AccessPolicy(ctx context.Context) (AccessPolicy, error) {
	policy, err := s.store.GetAccessPolicy(ctx)
	if err != nil {
		return AccessPolicyAllowlist, err
	}
	if !IsValidAccessPolicy(policy) {
		return AccessPolicyAllowlist, nil
	}

	return policy, nil
}

// SetAccessPolicy switches the access policy. Switching never modifies
// allowlist membership: entries survive in both directions so the operator
// can switch back without data loss. Validation happens before any SQL so an
// invalid policy never reaches storage.
func (s *Service) SetAccessPolicy(ctx context.Context, policy AccessPolicy, actorID int64) error {
	if !IsValidAccessPolicy(policy) {
		return fmt.Errorf("unsupported access policy %q; supported values: allowlist, allow_all", policy)
	}

	return s.store.SetAccessPolicy(ctx, policy, actorID)
}

// Allowlist lists whitelist memberships joined with catalog display data.
// Chats missing from the catalog keep zero display fields.
func (s *Service) Allowlist(ctx context.Context) ([]AllowlistEntry, error) {
	entries, err := s.store.ListWhitelistEntries(ctx)
	if err != nil {
		return nil, err
	}

	catalog, err := s.store.ListChats(ctx)
	if err != nil {
		return nil, err
	}
	display := make(map[int64]ChatCatalogEntry, len(catalog))
	for _, entry := range catalog {
		display[entry.ChatID] = entry
	}

	result := make([]AllowlistEntry, 0, len(entries))
	for _, entry := range entries {
		joined := AllowlistEntry{WhitelistEntry: entry}
		if info, ok := display[entry.ChatID]; ok {
			joined.ChatType = info.ChatType
			joined.Title = info.Title
			joined.Username = info.Username
			joined.DisplayName = info.DisplayName
		}
		result = append(result, joined)
	}

	return result, nil
}

// SetAllowlist adds or removes one chat from the whitelist. Adding an
// already-listed chat refreshes its audit fields (the store upserts).
func (s *Service) SetAllowlist(ctx context.Context, chatID int64, member bool, actorID int64) error {
	if member {
		return s.store.AddWhitelistChat(ctx, chatID, actorID)
	}

	return s.store.RemoveWhitelistChat(ctx, chatID)
}

// AccessSummary reports the policy and allowlist size for dashboards.
func (s *Service) AccessSummary(ctx context.Context) (AccessSummary, error) {
	policy, err := s.AccessPolicy(ctx)
	if err != nil {
		return AccessSummary{}, err
	}

	entries, err := s.store.ListWhitelistEntries(ctx)
	if err != nil {
		return AccessSummary{}, err
	}

	return AccessSummary{Policy: policy, AllowlistCount: len(entries)}, nil
}
