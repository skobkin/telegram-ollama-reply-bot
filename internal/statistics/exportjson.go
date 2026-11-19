package statistics

import (
	"encoding/json"
)

// GeneralJSON renders the legacy pretty-JSON representation of a general
// snapshot — the /stats command and admin-screen format. This is a
// presenter, not a collector method: callers snapshot first, then render.
// The output is byte-identical to the pre-consolidation StatsSnapshot
// string form (same struct tags, same field order, same marshal settings).
func GeneralJSON(s GeneralSnapshot) string {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "{\"error\": \"cannot serialize stats\"}"
	}

	return string(data)
}
