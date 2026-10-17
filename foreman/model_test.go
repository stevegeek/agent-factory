package main

import "testing"

// Everything the dispatcher can produce has a place in Foreman: a new column, stage or ask added in Python
// without its Go side fails here, not on the screen.
func TestForemanCoversTheDispatchersModel(t *testing.T) {
	for _, c := range model.Columns {
		if columnLabels[c] == "" {
			t.Errorf("column %q has no label", c)
		}
	}
	for _, s := range model.Stages {
		if roles[s] == "" {
			t.Errorf("stage %q has no label", s)
		}
	}
	order := map[string]bool{}
	for _, k := range askOrder {
		order[k] = true
	}
	for _, k := range model.AskKinds {
		if _, ok := askKinds[k]; !ok || !order[k] {
			t.Errorf("ask kind %q has no label or no place in the desk order", k)
		}
		if len(Answers(&Ticket{ID: "T0001", Column: "needs_human"}, &Ask{Kind: k})) == 0 && k != "review" {
			t.Errorf("ask kind %q has no answers", k)
		}
	}
}
