package ticket

import (
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// reworkWhy is the public reason a PR's change went back to work, by the
// reason of the move that sent it back, or "" for a reason that is not a
// rework. The texts are published in the PR's Updates, so they name no
// person and no internal stage.
func reworkWhy(r model.Reason) string {
	switch r {
	case model.ReasonConflicts:
		return "to resolve conflicts with the base branch"
	case model.ReasonMainMoved:
		return "because the checks failed on the latest base branch"
	case model.ReasonChangesRequested:
		return "after changes were requested in review"
	case model.ReasonReviewChanges, model.ReasonPublicChanges:
		return "after review asked for changes"
	case model.ReasonByHuman, model.ReasonRedirected, model.ReasonReopened:
		return "after feedback"
	}
	return ""
}

// ReworkWhy is why the work now finishing was sent back: the last move back
// to ready since the work before it was done, as reworkWhy names it; ""
// when there is none (a first run, or only retries).
func ReworkWhy(t *Ticket) string {
	for i := len(t.History) - 1; i >= 0; i-- {
		m, ok := t.History[i].Data.(Moved)
		if !ok {
			continue
		}
		if m.Reason == model.ReasonWorkDone {
			return ""
		}
		if why := reworkWhy(m.Reason); why != "" && m.To == string(model.ColReady) {
			return why
		}
	}
	return ""
}

// AddUpdate records what a rework of the ticket's open PR did.
func AddUpdate(t *Ticket, why, text string, now time.Time) {
	t.Updates = append(t.Updates, Update{At: clock.Stamp(now), Why: why, Text: textx.Strip(text)})
}

// KeepSummary is a work report to store: report itself, or, when it has no
// summary (or a blank one) and the stored work report has one, report with
// that summary added, so that a rework that leaves the change as it was
// keeps its description.
func KeepSummary(t *Ticket, report jsonx.Value) jsonx.Value {
	o, ok := report.Object()
	if !ok {
		return report
	}
	if s, has := o.Lookup("summary"); has {
		if str, isStr := s.Str(); !isStr || textx.Strip(str) != "" {
			return report
		}
	}
	prev, ok := t.Reports.Get("work").Object()
	if !ok {
		return report
	}
	s, ok := prev.Lookup("summary")
	if str, isStr := s.Str(); !ok || !isStr || textx.Strip(str) == "" {
		return report
	}
	o = o.Clone()
	o.Set("summary", s.Clone())
	return o.Value()
}

// UpdateLines are the PR's Updates, one Markdown list item each, oldest
// first: when the rework finished (to the minute, UTC), why the change was
// sent back, and what the round changed, on one line.
func UpdateLines(us []Update) []string {
	out := make([]string, len(us))
	for i, u := range us {
		when := u.At
		if at, err := time.Parse(clock.Layout, u.At); err == nil {
			when = at.UTC().Format("2006-01-02 15:04") + " UTC"
		}
		if u.Why != "" {
			when += ", " + u.Why
		}
		out[i] = "- " + when + ": " + textx.Fold(u.Text)
	}
	return out
}

// UpdatesText is the Updates as the PR body prints them, "" when there are
// none.
func UpdatesText(t *Ticket) string { return strings.Join(UpdateLines(t.Updates), "\n") }
