package mcp

import (
	"reflect"
	"strings"
	"testing"
)

func TestFilterToolsKeepsAdvertisedToolsWhenNoFilters(t *testing.T) {
	available := []string{"fetch", "summarize", "ping"}

	got, err := filterTools(available, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, available) {
		t.Fatalf("unexpected result: %v", got)
	}
}

func TestFilterToolsAppliesAllowListThenRestrict(t *testing.T) {
	available := []string{"fetch", "summarize", "delete_history", "ping"}

	got, err := filterTools(available, []string{"fetch", "summarize", "Delete_History"}, []string{"summarize"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"fetch", "delete_history"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected result: %v", got)
	}
}

func TestFilterToolsNormalisesNamesByTrimmingAndLowercasing(t *testing.T) {
	available := []string{"Fetch", "Summarize ", "PING"}

	got, err := filterTools(available, []string{" fetch ", " SUMMARIZE"}, []string{"ping"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"Fetch", "Summarize "}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected result: %v", got)
	}
}

func TestFilterToolsRejectsAllowedEntryNotOnServer(t *testing.T) {
	available := []string{"fetch", "summarize"}

	_, err := filterTools(available, []string{"fetch", "does_not_exist"}, nil)
	if err == nil {
		t.Fatalf("expected error for missing allowed tool")
	}
}

// TestFilterToolsRejectsRestrictedEntryNotOnServer mirrors the allow-list
// fail-fast behavior on the deny-list: a misspelled RESTRICTED_TOOLS entry
// must fail at startup instead of silently leaving the tool exposed.
func TestFilterToolsRejectsRestrictedEntryNotOnServer(t *testing.T) {
	available := []string{"fetch", "summarize", "delete_history"}

	_, err := filterTools(available, nil, []string{"delete_histroy"})
	if err == nil {
		t.Fatalf("expected error for misspelled restricted tool")
	}
	if !strings.Contains(err.Error(), "delete_histroy") {
		t.Fatalf("expected error to mention the unknown entry, got: %v", err)
	}
	if !strings.Contains(err.Error(), "restricted tool") {
		t.Fatalf("expected allow-list-style error wording, got: %v", err)
	}
}

// TestFilterToolsRejectsUnknownRestrictedEntryEvenInsideAllowList ensures
// the deny-list check is independent of the allow-list contents, matching
// the allow-list check's treatment (both are validated against every
// advertised tool).
func TestFilterToolsRejectsUnknownRestrictedEntryEvenInsideAllowList(t *testing.T) {
	available := []string{"fetch", "summarize", "delete_history"}

	_, err := filterTools(available, []string{"fetch", "delete_history"}, []string{"fetch", "delete_history_x"})
	if err == nil {
		t.Fatalf("expected error for unknown restricted entry inside an allow-listed setup")
	}
}

// TestFilterToolsValidRestrictedListStillFilters keeps the healthy deny-list
// path: a valid entry removes exactly the named tool and errors nothing.
func TestFilterToolsValidRestrictedListStillFilters(t *testing.T) {
	available := []string{"fetch", "delete_history", "ping"}

	got, err := filterTools(available, nil, []string{"delete_history"})
	if err != nil {
		t.Fatalf("unexpected error for valid restricted list: %v", err)
	}
	want := []string{"fetch", "ping"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected result: %v, want %v", got, want)
	}
}

func TestFilterToolsDeduplicatesRestrictedList(t *testing.T) {
	available := []string{"fetch", "summarize", "ping"}

	got, err := filterTools(available, nil, []string{"ping", "PING", "ping "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"fetch", "summarize"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected result: %v", got)
	}
}

func TestFilterToolsEmptyInputProducesEmptyResult(t *testing.T) {
	got, err := filterTools(nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %v", got)
	}
}

func TestNormalizeToolNameListDropsWhitespaceAndDedupes(t *testing.T) {
	got := normalizeToolNameList([]string{"  fetch ", "", "fetch", "PING", "ping"})
	want := []string{"fetch", "ping"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected result: %v", got)
	}
}
