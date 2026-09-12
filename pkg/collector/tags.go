package collector

import (
	"os"
	"strings"
)

// ParseTags accepts either a `NODEPULSE_TAGS` env value or a CLI flag value
// and normalises it into a clean, deduplicated slice. Accepted shapes:
//
//   - "env=prod,region=eu,role=db"   → ["env=prod","region=eu","role=db"]
//   - "env=prod region=eu role=db"   → ["env=prod","region=eu","role=db"]
//   - "env=prod\nregion=eu"          → ["env=prod","region=eu"]
//
// Empty segments and whitespace are dropped. Order is preserved on first
// occurrence, later duplicates removed so the JSON wire stays compact.
func ParseTags(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	raw = strings.ReplaceAll(raw, "\n", ",")
	raw = strings.ReplaceAll(raw, " ", ",")
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// TagsFromEnv is the convenience used by the agent's heartbeat path:
// prefers an explicit flag value (caller passes it), falls back to the
// environment variable. Returns nil when nothing is configured so the
// Heartbeat JSON omits the field.
func TagsFromEnv(flagValue string) []string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return ParseTags(v)
	}
	return ParseTags(os.Getenv("NODEPULSE_TAGS"))
}
