// Package present is Foreman's pure presentation (ARCHITECTURE.md §2.3,
// §6.2): what the desk asks and in which order, the facts and callouts beside
// an ask, the story of how a ticket got where it is, the week's floor and the
// conversation view. It formats; it decides nothing, writes nothing and does
// no I/O. Every function reads tickets as package ticket decodes them, and
// reads history only through ticket.Interpret.
//
// It is Foreman's ask.go, facts.go, story.go, floor.go and talk.go on the
// shared types, with the tables Foreman hard-coded taken from package model:
// ask kinds and desk order, column and stage labels, tier names, the panel's
// keep rule and the flagged move reasons.
//
// Times are written in time.Local, as Foreman always did.
package present

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// KindLabel is an ask kind's desk label; an unknown kind reads as itself,
// capitalised.
func KindLabel(k string) string {
	if info, ok := model.AskKindOf(model.AskKind(k)); ok {
		return info.Label
	}
	return CapFirst(k)
}

// KindGlyph is an ask kind's desk glyph; "?" for an unknown kind.
func KindGlyph(k string) string {
	if info, ok := model.AskKindOf(model.AskKind(k)); ok {
		return info.Glyph
	}
	return "?"
}

// askRank is an ask kind's place in desk order; unknown kinds go last.
func askRank(k string) int {
	if info, ok := model.AskKindOf(model.AskKind(k)); ok {
		return info.DeskOrder
	}
	return len(model.AskKinds())
}

// ColumnLabel is the label of any column, factory or board, or the name
// itself when it is not a column.
func ColumnLabel(c string) string { return model.ColumnLabel(c) }

// TicketColumnLabel is where a ticket is, in words: a proposal the panel or
// the maintainer turned down reads as dropped.
func TicketColumnLabel(t *ticket.Ticket) string {
	if t.Column == string(model.ColRejected) && t.KindName() == string(model.KindProposal) {
		return "Dropped"
	}
	return ColumnLabel(t.Column)
}

// RoleLabel is who runs a stage ("Worker" for work); an unknown stage is
// named by itself, capitalised.
func RoleLabel(stage string) string {
	if info, ok := model.StageInfoOf(model.Stage(stage)); ok {
		return info.Label
	}
	return CapFirst(stage)
}

// roleOf is the model's label for a stage, or "" (Foreman's roles[stage]).
func roleOf(stage string) string {
	if info, ok := model.StageInfoOf(model.Stage(stage)); ok {
		return info.Label
	}
	return ""
}

// flaggedReason reports a move reason the story marks as a run that stopped
// or a check that failed.
func flaggedReason(r string) bool {
	for _, info := range model.Reasons() {
		if string(info.Reason) == r {
			return info.Flagged
		}
	}
	return false
}

// tierName is a security tier's name, or "" for a tier the model lacks.
func tierName(n int) string {
	names := model.TierNames()
	if n < 0 || n >= len(names) {
		return ""
	}
	return names[n]
}

// SafeURL keeps only absolute http(s) links with a host: ticket text comes
// from public issue trackers. Anything else is "".
func SafeURL(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}

// ByID indexes tickets by id.
func ByID(ts []*ticket.Ticket) map[ticket.ID]*ticket.Ticket {
	m := make(map[ticket.ID]*ticket.Ticket, len(ts))
	for _, t := range ts {
		m[t.ID] = t
	}
	return m
}

// IDLess orders ticket ids as the desk lists them: a shorter id first, then
// by text.
func IDLess(a, b ticket.ID) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// ReportField is field key of t's report for stage, read loosely as the
// pages always read agents' reports: a string trimmed, a number or a bool in
// its JSON form, anything else (or no report) "".
func ReportField(t *ticket.Ticket, stage, key string) string { return reportOf(t, stage).str(key) }

// ParseTime reads an RFC 3339 stamp, or gives the zero time.
func ParseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Age is a duration as the desk says it: "just now", "N min", "N h" up to
// two days, then "N days".
func Age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// AgeSince is the age of a stamp at now, or "" for a stamp that does not
// parse.
func AgeSince(at string, now time.Time) string {
	t := ParseTime(at)
	if t.IsZero() {
		return ""
	}
	return Age(now.Sub(t))
}

// sentence trims s, capitalises it and ends it with a full stop.
func sentence(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), " ,;:")
	if s == "" {
		return s
	}
	s = CapFirst(s)
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "?") && !strings.HasSuffix(s, "!") {
		s += "."
	}
	return s
}

// CapFirst upper-cases the first character.
func CapFirst(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// Clip cuts s to n characters (runes), marking the cut with an ellipsis.
// This is Foreman's clip, not Python's (textx.Clip): the pages keep their
// own wording.
func Clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// Plural is "1 word" or "N words".
func Plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return strings.TrimSpace(x)
		}
	}
	return ""
}

// views of the optional fields, as Foreman's plain struct read them

func parentOf(t *ticket.Ticket) ticket.ID { return ticket.ID(t.Parent.Or("")) }

func agentOf(t *ticket.Ticket) (ticket.Agent, bool) { return t.Agent.Get() }

func issueOf(t *ticket.Ticket) int64 {
	if s, ok := t.Source.Get(); ok {
		return s.Issue
	}
	return 0
}

func prNumber(t *ticket.Ticket) int {
	if pr, ok := t.PR.Get(); ok {
		return pr.Number
	}
	return 0
}

func prDraft(pr ticket.PR) bool { return pr.Draft.Or(false) }

func closed(t *ticket.Ticket) bool { return model.Closed(t.Column) }
