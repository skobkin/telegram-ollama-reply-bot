package sqlite

import (
	"context"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

func TestAccessPolicyRoundTrip(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	policy, err := store.GetAccessPolicy(ctx)
	if err != nil {
		t.Fatalf("get access policy: %v", err)
	}
	if policy != adminconfig.AccessPolicyAllowlist {
		t.Fatalf("expected default allowlist policy, got %q", policy)
	}

	if err := store.SetAccessPolicy(ctx, adminconfig.AccessPolicyAllowAll, 7); err != nil {
		t.Fatalf("set access policy: %v", err)
	}

	policy, err = store.GetAccessPolicy(ctx)
	if err != nil {
		t.Fatalf("get access policy: %v", err)
	}
	if policy != adminconfig.AccessPolicyAllowAll {
		t.Fatalf("expected allow_all policy, got %q", policy)
	}
}

func TestSetAccessPolicyRejectsInvalidValues(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	err := store.SetAccessPolicy(ctx, adminconfig.AccessPolicy("bogus"), 7)
	if err == nil {
		t.Fatalf("expected invalid policy to be rejected")
	}

	policy, err := store.GetAccessPolicy(ctx)
	if err != nil {
		t.Fatalf("get access policy: %v", err)
	}
	if policy != adminconfig.AccessPolicyAllowlist {
		t.Fatalf("rejected write must not change policy, got %q", policy)
	}
}

// TestAccessPolicyNotWritableAsGlobalField proves the policy is not reachable
// through the stringly global-field path; only the typed setter may write it.
func TestAccessPolicyNotWritableAsGlobalField(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.SetGlobalField(ctx, "access_policy", "allow_all", 7); err == nil {
		t.Fatalf("expected access_policy to be rejected as a global field")
	}

	policy, err := store.GetAccessPolicy(ctx)
	if err != nil {
		t.Fatalf("get access policy: %v", err)
	}
	if policy != adminconfig.AccessPolicyAllowlist {
		t.Fatalf("policy must stay untouched, got %q", policy)
	}
}

func TestListWhitelistEntriesReturnsAuditFields(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.AddWhitelistChat(ctx, -1001234567890, 42); err != nil {
		t.Fatalf("add whitelist chat: %v", err)
	}

	entries, err := store.ListWhitelistEntries(ctx)
	if err != nil {
		t.Fatalf("list whitelist entries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one entry, got %d", len(entries))
	}
	if entries[0].ChatID != -1001234567890 {
		t.Fatalf("unexpected chat id: %d", entries[0].ChatID)
	}
	if entries[0].AddedBy != 42 {
		t.Fatalf("unexpected added_by: %d", entries[0].AddedBy)
	}
	if entries[0].AddedAt.IsZero() {
		t.Fatalf("expected added_at to be populated")
	}
}

// TestAccessPolicySwitchKeepsWhitelist pins the invariant that switching the
// policy never touches membership data.
func TestAccessPolicySwitchKeepsWhitelist(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.AddWhitelistChat(ctx, 55, 1); err != nil {
		t.Fatalf("add whitelist chat: %v", err)
	}
	if err := store.SetAccessPolicy(ctx, adminconfig.AccessPolicyAllowAll, 1); err != nil {
		t.Fatalf("switch to allow_all: %v", err)
	}

	member, err := store.IsChatWhitelisted(ctx, 55)
	if err != nil {
		t.Fatalf("check whitelist: %v", err)
	}
	if !member {
		t.Fatalf("membership must survive a policy switch")
	}

	if err := store.SetAccessPolicy(ctx, adminconfig.AccessPolicyAllowlist, 1); err != nil {
		t.Fatalf("switch back to allowlist: %v", err)
	}

	member, err = store.IsChatWhitelisted(ctx, 55)
	if err != nil {
		t.Fatalf("check whitelist: %v", err)
	}
	if !member {
		t.Fatalf("membership must survive switching back")
	}
}
