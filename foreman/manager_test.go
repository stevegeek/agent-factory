package main

import (
	"strings"
	"testing"
)

func TestManagerDraftsAreShownAndAnswered(t *testing.T) {
	tk := &Ticket{ID: "T0050", Project: "factory", Kind: "manage", Title: "A morning check", Column: "needs_human",
		Ask:    &Ask{Kind: "draft", Question: "Apply the manager's 1 config change?", Since: "2026-10-16T10:00:00Z"},
		Drafts: []Draft{{Title: "Daily check", Why: "Files a check every morning.", Patch: "--- a/config/config.toml\n+++ b/config/config.toml\n@@ -1 +1,2 @@\n+[[schedules]]\n"}}}
	if got := Action(tk, tk.Ask); got != "Apply the draft? · 1 change" {
		t.Errorf("action: %q", got)
	}
	cases := map[string][][]string{
		"applied": {{"move", "T0050", "done"}},
		"drop":    {{"move", "T0050", "rejected"}},
	}
	for answer, want := range cases {
		got, err := Plan(tk, tk.Ask, answer, "", nil)
		if err != nil || fmtArgv(got) != fmtArgv(want) {
			t.Errorf("%s: %v %v", answer, got, err)
		}
	}
	if _, err := Plan(tk, tk.Ask, "send_back", "", nil); err == nil {
		t.Errorf("sending a draft back needs a note")
	}
	got, _ := Plan(tk, tk.Ask, "send_back", "weekdays only", nil)
	if fmtArgv(got) != fmtArgv([][]string{{"note", "T0050", "weekdays only"}, {"move", "T0050", "ready"}}) {
		t.Errorf("send back: %v", got)
	}
	h := newHarness(t, false)
	c := h.login()
	body := h.do("GET", "/new", nil, c).Body.String()
	if !strings.Contains(body, `value="manage"`) {
		t.Errorf("the new-ticket page should offer the manager")
	}
}

func fmtArgv(cmds [][]string) string {
	var out []string
	for _, c := range cmds {
		out = append(out, strings.Join(c, " "))
	}
	return strings.Join(out, " | ")
}
