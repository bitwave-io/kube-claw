package main

import "testing"

func TestStripUsageFooter(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"no footer":      {"All good, deployed.", "All good, deployed."},
		"plain usage":    {"All good.\n\n_tokens: 15.9k in · 8 out_", "All good."},
		"model tag":      {"Done.\n\n_agent: general · model: claude-opus-4-8 · tokens: 8.4k in · 49 out_", "Done."},
		"model only":     {"Done.\n\n_model: claude-opus-4-8 · tokens: 124.2k in · 1.6k out_", "Done."},
		"no markup":      {"NO_REPLY\n\ntokens: 15.9k in · 8 out", "NO_REPLY"},
		"stacked":        {"Hi.\n\n_tokens: 1k in · 2 out_\n_agent: general · model: m · tokens: 3k in · 4 out_", "Hi."},
		"footer only":    {"_tokens: 1k in · 2 out_", ""},
		"mid-text usage": {"tokens: 5k in · 2 out is what the last run cost.", "tokens: 5k in · 2 out is what the last run cost."},
	} {
		if got := stripUsageFooter(tc.in); got != tc.want {
			t.Errorf("%s: stripUsageFooter(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}
