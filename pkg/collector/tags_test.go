package collector

import "testing"

func TestParseTags(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"spaces-only", "   \t  ", nil},
		{"simple-csv", "env=prod,region=eu,role=db", []string{"env=prod", "region=eu", "role=db"}},
		{"whitespace-separated", "env=prod region=eu role=db", []string{"env=prod", "region=eu", "role=db"}},
		{"mixed-separators", "env=prod\nregion=eu, role=db", []string{"env=prod", "region=eu", "role=db"}},
		{"dedup-preserve-order", "env=prod,env=staging,env=prod", []string{"env=prod", "env=staging"}},
		{"trailing-commas", "env=prod,,,", []string{"env=prod"}},
		{"plain-labels", "edge,canary", []string{"edge", "canary"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseTags(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("ParseTags(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i, v := range tc.want {
				if got[i] != v {
					t.Fatalf("ParseTags(%q)[%d] = %q, want %q", tc.in, i, got[i], v)
				}
			}
		})
	}
}

func TestTagsFromEnvPrefersFlag(t *testing.T) {
	t.Setenv("NODEPULSE_TAGS", "env=env,role=fromenv")
	if got := TagsFromEnv("env=flag"); len(got) != 1 || got[0] != "env=flag" {
		t.Fatalf("flag wins, got %v", got)
	}
}

func TestTagsFromEnvFallsBack(t *testing.T) {
	t.Setenv("NODEPULSE_TAGS", "env=env,role=fromenv")
	got := TagsFromEnv("")
	if len(got) != 2 || got[0] != "env=env" || got[1] != "role=fromenv" {
		t.Fatalf("env fallback, got %v", got)
	}
}

func TestTagsFromEnvEmpty(t *testing.T) {
	t.Setenv("NODEPULSE_TAGS", "")
	if got := TagsFromEnv(""); got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}
