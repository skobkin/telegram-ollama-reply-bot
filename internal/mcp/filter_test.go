package mcp

import (
	"reflect"
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
