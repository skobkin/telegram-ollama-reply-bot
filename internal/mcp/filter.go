// Package mcp wires the bot to one or more external Model Context Protocol
// (MCP) servers and registers their tools on the existing tooluse.Registry.
//
// Per-server allow-list and deny-list filtering lives here as a pure function
// so it can be unit-tested without spinning up an MCP server.
package mcp

import (
	"fmt"
	"strings"
)

// normalizeToolNameList lower-cases, trims, and de-duplicates a list of MCP
// tool names. Whitespace-only entries are dropped. The result preserves the
// first occurrence order.
func normalizeToolNameList(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.ToLower(strings.TrimSpace(value))
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}

	return out
}

// filterTools applies the per-MCP allow-then-restrict pipeline to a set of
// tool names discovered on the server.
//
//   - If `allowed` is non-empty, only tools present in it survive.
//   - `restricted` is then subtracted from the survivors.
//
// The returned slice preserves the order of `available`. When the operator
// names a tool in `allowed` that the server does not advertise, filterTools
// returns an error so the misconfiguration is caught at startup.
func filterTools(available, allowed, restricted []string) ([]string, error) {
	allowedNorm := normalizeToolNameList(allowed)
	restrictedNorm := normalizeToolNameList(restricted)

	availableSet := make(map[string]struct{}, len(available))
	for _, name := range available {
		availableSet[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
	}

	// Fail-fast on typos in the allow-list: every entry must correspond to a
	// real tool. This catches "I meant 'fetch' but typed 'fetche'" early.
	if len(allowedNorm) > 0 {
		for _, name := range allowedNorm {
			if _, ok := availableSet[name]; !ok {
				return nil, fmt.Errorf("allowed tool %q is not advertised by the server", name)
			}
		}
	}

	restrictedSet := make(map[string]struct{}, len(restrictedNorm))
	for _, name := range restrictedNorm {
		restrictedSet[name] = struct{}{}
	}

	out := make([]string, 0, len(available))
	for _, name := range available {
		lower := strings.ToLower(strings.TrimSpace(name))
		if len(allowedNorm) > 0 {
			allowed := false
			for _, allowedName := range allowedNorm {
				if allowedName == lower {
					allowed = true

					break
				}
			}
			if !allowed {
				continue
			}
		}
		if _, blocked := restrictedSet[lower]; blocked {
			continue
		}
		out = append(out, name)
	}

	return out, nil
}
