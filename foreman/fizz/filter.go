package fizz

import (
	"net/url"
	"sort"
	"strings"
	"time"
)

// filter: the card list's query (the CLI's card list flags), as the real API reads it.
type filter struct {
	index                                        string
	boards, columns, tags, assignees             map[string]bool
	creators, closers                            map[string]bool
	terms                                        []string
	unassigned                                   bool
	createdFrom, createdTo, closedFrom, closedTo string
	sortBy                                       string
	acct                                         *Account
	now                                          time.Time
}

func set(q url.Values, k string) map[string]bool {
	if len(q[k]) == 0 {
		return nil
	}
	m := map[string]bool{}
	for _, v := range q[k] {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				m[s] = true
			}
		}
	}
	return m
}

// newFilter reads the query; the string is a reason to refuse it.
func newFilter(q url.Values, a *Account, now time.Time) (*filter, string) {
	f := &filter{index: q.Get("indexed_by"), boards: set(q, "board_ids[]"), columns: set(q, "column_ids[]"),
		tags: set(q, "tag_ids[]"), assignees: set(q, "assignee_ids[]"), creators: set(q, "creator_ids[]"),
		closers: set(q, "closer_ids[]"), unassigned: q.Get("assignment_status") == "unassigned",
		sortBy: q.Get("sorted_by"), acct: a, now: now.UTC()}
	for _, t := range q["terms[]"] {
		f.terms = append(f.terms, strings.Fields(strings.ToLower(t))...)
	}
	if f.index == "" && (q.Get("closure") != "" || f.closers != nil) {
		f.index = "closed" // asking who closed what, or when, is asking about closed cards
	}
	switch f.index {
	case "", "all", "maybe", "closed", "not_now", "golden", "stalled", "everything":
	default:
		return nil, "indexed_by " + f.index + " is not supported here"
	}
	switch f.sortBy {
	case "", "latest", "newest", "oldest":
	default:
		return nil, "sorted_by must be newest, oldest or latest"
	}
	var ok bool
	if f.createdFrom, f.createdTo, ok = window(q.Get("creation"), f.now); !ok {
		return nil, "creation must be today, yesterday, thisweek, lastweek, thismonth or lastmonth"
	}
	if f.closedFrom, f.closedTo, ok = window(q.Get("closure"), f.now); !ok {
		return nil, "closure must be today, yesterday, thisweek, lastweek, thismonth or lastmonth"
	}
	return f, ""
}

// window: a named period as [from, to) stamps; "" for none.
func window(name string, now time.Time) (string, string, bool) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	week := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7)) // Monday
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var from, to time.Time
	switch name {
	case "":
		return "", "", true
	case "today":
		from, to = day, day.AddDate(0, 0, 1)
	case "yesterday":
		from, to = day.AddDate(0, 0, -1), day
	case "thisweek":
		from, to = week, week.AddDate(0, 0, 7)
	case "lastweek":
		from, to = week.AddDate(0, 0, -7), week
	case "thismonth":
		from, to = month, month.AddDate(0, 1, 0)
	case "lastmonth":
		from, to = month.AddDate(0, -1, 0), month
	default:
		return "", "", false
	}
	return from.Format(TimeFormat), to.Format(TimeFormat), true
}

func anyIn(ids []string, m map[string]bool) bool {
	for _, id := range ids {
		if m[id] {
			return true
		}
	}
	return false
}

func (f *filter) match(c *Card) bool {
	open := !c.Closed && !c.Postponed
	switch f.index {
	case "", "all":
		if !open {
			return false
		}
	case "maybe":
		if !open || c.ColumnID != "" {
			return false
		}
	case "closed":
		if !c.Closed {
			return false
		}
	case "not_now":
		if !c.Postponed || c.Closed {
			return false
		}
	case "golden":
		if !open || !c.Golden {
			return false
		}
	case "stalled":
		if !open || c.LastActiveAt >= f.now.AddDate(0, 0, -30).Format(TimeFormat) {
			return false
		}
	}
	if f.boards != nil && !f.boards[c.BoardID] {
		return false
	}
	if f.columns != nil && (!open || !f.columns[c.ColumnID]) {
		return false
	}
	if f.tags != nil && !anyIn(c.Tags, f.tags) {
		return false
	}
	if f.assignees != nil && !anyIn(c.Assignees, f.assignees) {
		return false
	}
	if f.unassigned && len(c.Assignees) > 0 {
		return false
	}
	if f.creators != nil && !f.creators[c.CreatorID] {
		return false
	}
	if f.closers != nil && !f.closers[c.CloserID] {
		return false
	}
	if f.createdFrom != "" && (c.CreatedAt < f.createdFrom || c.CreatedAt >= f.createdTo) {
		return false
	}
	if f.closedFrom != "" && (!c.Closed || c.ClosedAt < f.closedFrom || c.ClosedAt >= f.closedTo) {
		return false
	}
	if len(f.terms) > 0 {
		var text strings.Builder
		text.WriteString(strings.ToLower(c.Title) + "\n" + strings.ToLower(PlainText(c.Description, false)))
		for _, cm := range c.Comments {
			text.WriteString("\n" + strings.ToLower(PlainText(cm.Body, false)))
		}
		for _, id := range c.Tags {
			if t := f.acct.Tag(id); t != nil {
				text.WriteString("\n" + strings.ToLower(t.Title))
			}
		}
		s := text.String()
		for _, t := range f.terms {
			if !strings.Contains(s, t) {
				return false
			}
		}
	}
	return true
}

func (f *filter) sort(cs []*Card) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		switch f.sortBy {
		case "newest":
			return a.CreatedAt > b.CreatedAt || a.CreatedAt == b.CreatedAt && a.Number > b.Number
		case "oldest":
			return a.CreatedAt < b.CreatedAt || a.CreatedAt == b.CreatedAt && a.Number < b.Number
		}
		return a.LastActiveAt > b.LastActiveAt || a.LastActiveAt == b.LastActiveAt && a.Number > b.Number
	})
}
