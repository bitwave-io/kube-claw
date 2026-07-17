package main

import "testing"

// stopCommand must catch the ways people actually say "stop" in Slack (bot
// mention included) without hijacking task requests that merely contain the
// word ("can you stop the dev cluster?").
func TestStopCommand(t *testing.T) {
	cases := []struct {
		text   string
		isStop bool
		rest   string
	}{
		{"stop", true, ""},
		{"STOP!", true, ""},
		{"<@U0BOT> stop", true, ""},
		{"<@U0PAT>: <@U0BOT> please stop", true, ""},
		{"ok stop it", true, ""},
		{"cancel that", true, ""},
		{"abort", true, ""},
		{"stop everything now", true, ""},
		{"stop, check prod-a instead", true, "check prod-a instead"},
		{"cancel the access request", true, "the access request"},
		{"can you stop the dev cluster?", false, ""},
		{"we should stop paying for that", false, ""},
		{"don't stop", false, ""},
		{"looks good, thanks", false, ""},
		{"", false, ""},
	}
	for _, c := range cases {
		isStop, rest := stopCommand(c.text)
		if isStop != c.isStop || rest != c.rest {
			t.Errorf("stopCommand(%q) = (%v, %q), want (%v, %q)", c.text, isStop, rest, c.isStop, c.rest)
		}
	}
}
