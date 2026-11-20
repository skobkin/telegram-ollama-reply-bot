package tooluse

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestDatetimeMathSchemaRequiresOperationArguments(t *testing.T) {
	r := newRegistryForTest(t)
	def, _ := r.registry.Lookup("datetime_math")
	var schema jsonschema.Schema
	if err := json.Unmarshal(def.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args  string
		valid bool
	}{
		{`{"operation":"diff"}`, false},
		{`{"operation":"diff","left":"2026-01-01T00:00:00Z","right":"2026-01-02T00:00:00Z"}`, true},
		{`{"operation":"shift","timestamp":"2026-01-01T00:00:00Z"}`, false},
		{`{"operation":"shift","timestamp":"2026-01-01T00:00:00Z","days":0}`, true},
		{`{"operation":"weekday","timestamp":"2026-01-01T00:00:00Z"}`, true},
		{`{"operation":"convert_timezone","timestamp":"2026-01-01T00:00:00Z"}`, false},
		{`{"operation":"convert_timezone","timestamp":"2026-01-01T00:00:00Z","target_timezone":"UTC"}`, true},
	} {
		var args any
		if err := json.Unmarshal([]byte(tc.args), &args); err != nil {
			t.Fatal(err)
		}
		if got := resolved.Validate(args) == nil; got != tc.valid {
			t.Fatalf("%s valid=%v expected=%v", tc.args, got, tc.valid)
		}
	}
}
