package fizzy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/ticketapi"
)

// Importer files a dump's cards as board tickets, one per card, through
// Board: new tickets under the create lock, refreshes under each ticket's
// lock. What it writes is cut to Limits (zero: the ticket API's), and
// the moves and edits are the maintainer's (Config's first person).
type Importer struct {
	Board  *store.Board
	Config *config.Config
	Clock  clock.Clock
	Limits ticketapi.Limits
	Wait   fsx.Wait // for a lock; 0 is store.CLIWait
}

// Mapping is which project each board goes to, by board id.
type Mapping map[string]string

// ParseMap reads --map BOARD_ID=PROJECT flags.
func ParseMap(specs []string) (Mapping, error) {
	m := Mapping{}
	for _, s := range specs {
		b, p, ok := strings.Cut(s, "=")
		b, p = strings.TrimSpace(b), strings.TrimSpace(p)
		if !ok || !ValidID(b) || p == "" {
			return nil, fmt.Errorf("--map %q: give BOARD_ID=PROJECT", s)
		}
		if old, dup := m[b]; dup && old != p {
			return nil, fmt.Errorf("--map: board %s is mapped to %s and %s", b, old, p)
		}
		m[b] = p
	}
	if len(m) == 0 {
		return nil, errors.New("give at least one --map BOARD_ID=PROJECT")
	}
	return m, nil
}

// BoardReport is what an import did, or would do, with one board.
type BoardReport struct {
	Board, Name, Project                   string
	Cards                                  int
	Created, Refreshed, Unchanged, Skipped int
	Comments                               int            // comments copied
	Columns                                map[string]int // cards by the column they go to
	Labels                                 []string       // the labels the cards get
	Notes                                  []string       // why cards were skipped, comments not copied
}

// Report is an import's report: the boards it mapped, and the boards of
// the dump it was not asked to map.
type Report struct {
	Boards   []BoardReport
	Unmapped []Board
}

// Text is the report as `factory fizzy import` prints it.
func (r Report) Text(dry bool) string {
	var b strings.Builder
	for _, br := range r.Boards {
		name := br.Board
		if br.Name != "" {
			name += " (" + line(br.Name) + ")"
		}
		fmt.Fprintf(&b, "board %s -> %s: %d cards\n", name, br.Project, br.Cards)
		if dry {
			fmt.Fprintf(&b, "  would create %d, refresh %d, leave %d unchanged, skip %d\n", br.Created, br.Refreshed, br.Unchanged, br.Skipped)
		} else {
			fmt.Fprintf(&b, "  created %d, refreshed %d, unchanged %d, skipped %d; %d comments copied\n",
				br.Created, br.Refreshed, br.Unchanged, br.Skipped, br.Comments)
		}
		var cols []string
		for _, c := range model.BoardColumns() {
			if n := br.Columns[string(c.Name)]; n > 0 {
				cols = append(cols, fmt.Sprintf("%s %d", c.Name, n))
			}
		}
		if len(cols) > 0 {
			fmt.Fprintf(&b, "  columns: %s\n", strings.Join(cols, ", "))
		}
		if len(br.Labels) > 0 {
			fmt.Fprintf(&b, "  labels: %s\n", strings.Join(br.Labels, ", "))
		}
		for _, n := range br.Notes {
			fmt.Fprintf(&b, "  note: %s\n", n)
		}
	}
	if len(r.Unmapped) > 0 {
		var names []string
		for _, u := range r.Unmapped {
			s := u.ID
			if u.Name != "" {
				s += " (" + line(u.Name) + ")"
			}
			names = append(names, s)
		}
		fmt.Fprintf(&b, "skipped (not mapped): %s\n", strings.Join(names, ", "))
	}
	return b.String()
}

// want is what one card makes.
type want struct {
	card     Card
	title    string
	body     string
	labels   []string
	place    Place
	url      string
	author   string
	created  time.Time
	comments []Comment
	digest   string
}

func (im Importer) lim() ticketapi.Limits {
	if im.Limits.Title == 0 {
		return ticketapi.DefaultLimits()
	}
	return im.Limits
}

func (im Importer) now() time.Time {
	if im.Clock == nil {
		return time.Now().UTC()
	}
	return im.Clock.Now().UTC()
}

func (im Importer) wait() fsx.Wait {
	if im.Wait == 0 {
		return store.CLIWait
	}
	return im.Wait
}

// Check refuses a mapping to a project that is not a configured board
// project, naming what to add to config.toml.
func (im Importer) Check(m Mapping) error {
	var boards []string
	for b := range m {
		boards = append(boards, b)
	}
	sort.Strings(boards)
	for _, b := range boards {
		p := m[b]
		mode, ok := im.Config.ProjectMode(p)
		switch {
		case !ok:
			return fmt.Errorf("no project %s: add [projects.%s] with mode = \"board\" to config.toml first", p, p)
		case mode != string(model.ModeBoard):
			return fmt.Errorf("%s is a %s project: a Fizzy board goes to a board project (mode = \"board\" in [projects.%s] of config.toml)", p, mode, p)
		}
	}
	return nil
}

// Import files or refreshes a ticket for every card of the mapped boards
// of d. Dry, it writes nothing and reports what it would do. A ticket is
// found again by its imported record (the card's id), so a rerun never
// files a card twice: it refreshes a ticket whose card changed in Fizzy
// (title, body, labels it wrote, column) and copies only comments it has
// not copied before.
func (im Importer) Import(ctx context.Context, d Dump, m Mapping, dry bool) (Report, error) {
	var r Report
	if err := im.Check(m); err != nil {
		return r, err
	}
	boards, err := d.Boards()
	if err != nil {
		return r, err
	}
	named := map[string]Board{}
	for _, b := range boards {
		named[b.ID] = b
		if _, ok := m[b.ID]; !ok {
			r.Unmapped = append(r.Unmapped, b)
		}
	}
	var ids []string
	for b := range m {
		if _, ok := named[b]; !ok {
			return r, fmt.Errorf("the dump has no board %s", b)
		}
		ids = append(ids, b)
	}
	sort.Strings(ids)
	type planned struct {
		rep   BoardReport
		wants []want
	}
	var plans []planned
	for _, id := range ids {
		wants, notes, err := im.plan(d, id)
		if err != nil {
			return r, err
		}
		rep := BoardReport{Board: id, Name: named[id].Name, Project: m[id], Cards: len(wants), Columns: map[string]int{}, Notes: notes}
		labels := map[string]bool{}
		for _, w := range wants {
			rep.Columns[string(w.place.Column)]++
			for _, l := range w.labels {
				labels[l] = true
			}
		}
		for l := range labels {
			rep.Labels = append(rep.Labels, l)
		}
		sort.Strings(rep.Labels)
		plans = append(plans, planned{rep, wants})
	}
	if !dry {
		l, err := im.Board.CreateLock(ctx, im.wait())
		if err != nil {
			return r, err
		}
		defer l.Unlock()
	}
	have, err := im.imported()
	if err != nil {
		return r, err
	}
	for _, p := range plans {
		rep := p.rep
		if err := im.board(ctx, &rep, p.wants, have, dry); err != nil {
			return r, err
		}
		r.Boards = append(r.Boards, rep)
	}
	return r, nil
}

// imported are the tickets an import filed, by card id.
func (im Importer) imported() (map[string]*ticket.Ticket, error) {
	ts, problems := im.Board.Reader.List()
	if ts == nil && len(problems) > 0 && problems[0].Err != nil {
		return nil, problems[0].Err
	}
	out := map[string]*ticket.Ticket{}
	for _, t := range ts {
		if rec, ok := t.Imported.Get(); ok && rec.From == From && rec.Card != "" {
			out[rec.Card] = t
		}
	}
	return out, nil
}

// errUnchanged ends a refresh that found nothing to write.
var errUnchanged = errors.New("unchanged")

// board carries out (or, dry, counts) one board's wants.
func (im Importer) board(ctx context.Context, rep *BoardReport, wants []want, have map[string]*ticket.Ticket, dry bool) error {
	lim := im.lim()
	fresh := 0
	for _, w := range wants {
		if have[w.card.ID] == nil {
			fresh++
		}
	}
	if fresh > 0 {
		count := 0
		ts, _ := im.Board.Reader.List()
		for _, t := range ts {
			if t.Project == rep.Project {
				count++
			}
		}
		if count+fresh > lim.ProjectTickets {
			return fmt.Errorf("%s has %d tickets; %d more would pass the %d a board project holds", rep.Project, count, fresh, lim.ProjectTickets)
		}
	}
	for _, w := range wants {
		old := have[w.card.ID]
		switch {
		case old != nil && old.Project != rep.Project:
			rep.Skipped++
			rep.Notes = append(rep.Notes, fmt.Sprintf("card #%d is %s on %s already; not moved to %s", w.card.Number, old.ID, old.Project, rep.Project))
		case old != nil:
			if !changes(old, w) {
				rep.Unchanged++
				continue
			}
			if dry {
				rep.Refreshed++
				continue
			}
			err := im.Board.Changing(ctx, string(old.ID), im.wait(), func(t *ticket.Ticket) error {
				n, err := im.refresh(t, w)
				rep.Comments += n
				return err
			})
			switch {
			case errors.Is(err, errUnchanged):
				rep.Unchanged++
			case err != nil:
				return err
			default:
				rep.Refreshed++
			}
		case dry:
			rep.Created++
		default:
			t, n, err := im.create(ctx, rep.Project, w)
			if err != nil {
				return err
			}
			have[w.card.ID] = t
			rep.Created++
			rep.Comments += n
		}
	}
	return nil
}

// changes reports whether a ticket's card changed since its last import,
// or has comments the import has not copied.
func changes(t *ticket.Ticket, w want) bool {
	rec, _ := t.Imported.Get()
	return rec.Digest != w.digest || len(newComments(rec, w.comments)) > 0
}

func newComments(rec ticket.Imported, cms []Comment) []Comment {
	done := map[string]bool{}
	for _, id := range rec.Comments {
		done[id] = true
	}
	var out []Comment
	for _, c := range cms {
		if !done[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

// create files a card's ticket: dated and authored as the card was, into
// Inbox through the import door, its comments copied, then moved to its
// place.
func (im Importer) create(ctx context.Context, project string, w want) (*ticket.Ticket, int, error) {
	id, err := im.Board.NextID(ctx)
	if err != nil {
		return nil, 0, err
	}
	t := ticket.New(id, ticket.NewIn{Project: project, Title: w.title, Body: w.body, Door: string(model.DoorImport),
		Column: string(model.BoardInbox), By: w.author, Mode: model.ModeBoard}, w.created)
	t.Labels = append([]string{}, w.labels...)
	if w.url != "" {
		t.Links = []ticket.Link{{Rel: "origin", URL: w.url, Title: "Fizzy card #" + strconv.Itoa(w.card.Number)}}
	}
	rec := ticket.Imported{From: From, Card: w.card.ID, Number: w.card.Number, Board: boardOf(w.card), URL: w.url,
		Digest: w.digest, Labels: append([]string{}, w.labels...), Comments: []string{}}
	n := im.comments(t, &rec, w.comments)
	t.Imported = ticket.Some(rec)
	im.put(t, w.place)
	if err := im.Board.Save(t); err != nil {
		return nil, 0, err
	}
	return t, n, nil
}

// refresh brings a ticket up to its card: when the card changed, its
// title, body, the labels the import wrote and its place; and the comments
// not copied yet. Nothing to write is errUnchanged.
func (im Importer) refresh(t *ticket.Ticket, w want) (int, error) {
	rec, ok := t.Imported.Get()
	if !ok {
		return 0, errUnchanged
	}
	rec.Comments = append([]string{}, rec.Comments...)
	before := len(rec.Comments)
	wrote := false
	if rec.Digest != w.digest {
		var fields []string
		if t.Title != w.title {
			t.Title = w.title
			fields = append(fields, "title")
		}
		if t.Body != w.body {
			t.Body = w.body
			fields = append(fields, "body")
		}
		if labels := mergeLabels(t.Labels, rec.Labels, w.labels, im.lim().Labels); !slices.Equal(labels, t.Labels) {
			t.Labels = labels
			fields = append(fields, "labels")
		}
		if len(fields) > 0 {
			ticket.Event(t, "edit", "the Fizzy import changed "+strings.Join(fields, ", "), "",
				ticket.Edited{By: im.Config.Maintainer(), Fields: fields}, im.now())
		}
		im.put(t, w.place)
		rec.Digest, rec.Labels, rec.Number, rec.URL = w.digest, append([]string{}, w.labels...), w.card.Number, w.url
		wrote = true
	}
	n := im.comments(t, &rec, newComments(rec, w.comments))
	if !wrote && len(rec.Comments) == before {
		return 0, errUnchanged
	}
	t.Imported = ticket.Some(rec)
	return n, nil
}

// mergeLabels are a ticket's labels after a refresh: those people added
// on the board (not the import's last ones), and the import's new ones,
// sorted, unique, at most most.
func mergeLabels(have, lastImport, now []string, most int) []string {
	last := map[string]bool{}
	for _, l := range lastImport {
		last[l] = true
	}
	set := map[string]bool{}
	for _, l := range now {
		set[l] = true
	}
	for _, l := range have {
		if !last[l] {
			set[l] = true
		}
	}
	out := []string{}
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	if len(out) > most {
		out = out[:most]
	}
	return out
}

// comments copies cms onto t as conversation comments, each by its Fizzy
// author at its Fizzy time, within the ticket's caps, and records every
// one it has dealt with (an empty one included) in rec. It returns how
// many it copied.
func (im Importer) comments(t *ticket.Ticket, rec *ticket.Imported, cms []Comment) int {
	lim := im.lim()
	total, n := 0, 0
	for _, c := range t.Comments {
		total += len(c.Text)
		n = max(n, c.N)
	}
	copied := 0
	for _, c := range cms {
		text := cutBytes(clean(c.Text()), lim.Comment)
		if text == "" {
			rec.Comments = append(rec.Comments, c.ID)
			continue
		}
		if len(t.Comments) >= lim.Comments || int64(total+len(text)) > lim.CommentBytes {
			break // not recorded: a later run copies it if room is made
		}
		at := when(c.CreatedAt, im.now())
		by := person(c.Creator)
		n++
		total += len(text)
		if t.Comments == nil {
			t.Comments = []ticket.Comment{}
		}
		t.Comments = append(t.Comments, ticket.Comment{N: n, At: clock.Stamp(at), By: by, Type: "comment", Text: text})
		ticket.Event(t, "comment", by+": "+textx.Clip(text, 200), "", ticket.Commented{By: by, N: n}, at)
		rec.Comments = append(rec.Comments, c.ID)
		copied++
	}
	return copied
}

// put moves a ticket to its place, as the maintainer, unless it is there
// already; a ticket waiting on an open question is left waiting.
func (im Importer) put(t *ticket.Ticket, p Place) {
	to := string(p.Column)
	w, waiting := t.Waiting.Get()
	if t.Column == to {
		if to == string(model.BoardWaiting) && waiting && w.Reason == "other" && w.Text != p.Wait {
			w.Text = p.Wait
			t.Waiting = ticket.Some(w)
		}
		return
	}
	if waiting && w.Reason == "question" {
		for _, q := range t.Questions {
			if q.Open() {
				return
			}
		}
	}
	now := im.now()
	old := t.Column
	if old == string(model.BoardWaiting) {
		t.Waiting = ticket.Nil[ticket.Waiting]()
	}
	if to == string(model.BoardWaiting) {
		if t.Tree().Get("waiting").Kind() == jsonx.KindObject {
			t.Waiting = ticket.Nil[ticket.Waiting]()
			ticket.Encode(t)
		}
		t.Waiting = ticket.Some(ticket.Waiting{Reason: "other", Text: p.Wait, From: old, Since: clock.Stamp(now)})
	}
	if model.Closed(to) {
		for i := range t.Questions {
			if t.Questions[i].Open() {
				t.Questions[i].Withdrawn = ticket.Some(clock.Stamp(now))
			}
		}
	}
	detail := "as in Fizzy"
	if p.Wait != "" {
		detail = p.Wait
	}
	ticket.Move(t, to, model.ReasonImported, detail, "", im.Config.Maintainer(), now)
}

// plan is what each card of a board makes, by number, and notes on what
// the dump held that the import leaves out.
func (im Importer) plan(d Dump, board string) ([]want, []string, error) {
	cols, err := d.Columns(board)
	if err != nil {
		return nil, nil, err
	}
	cards, comments, err := d.Cards(board)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].Number < cards[j].Number })
	lim := im.lim()
	var out []want
	var notes []string
	for _, c := range cards {
		w := im.wantOf(c, cols, lim)
		for _, cm := range comments[c.Number] {
			if ValidID(cm.ID) {
				w.comments = append(w.comments, cm)
			}
		}
		sort.SliceStable(w.comments, func(i, j int) bool {
			a, b := when(w.comments[i].CreatedAt, time.Time{}), when(w.comments[j].CreatedAt, time.Time{})
			if !a.Equal(b) {
				return a.Before(b)
			}
			return w.comments[i].ID < w.comments[j].ID
		})
		if n := len(w.comments); n > lim.Comments {
			notes = append(notes, fmt.Sprintf("card #%d has %d comments; a ticket holds %d", c.Number, n, lim.Comments))
		}
		if c.Status != "" && c.Status != "published" {
			notes = append(notes, fmt.Sprintf("card #%d is %s in Fizzy", c.Number, line(c.Status)))
		}
		out = append(out, w)
	}
	return out, notes, nil
}

// wantOf is what one card makes, but its comments.
func (im Importer) wantOf(c Card, cols map[string]string, lim ticketapi.Limits) want {
	w := want{card: c, author: person(c.Creator), created: when(c.CreatedAt, im.now())}
	w.title = cutRunes(line(c.Title), lim.Title)
	if w.title == "" {
		w.title = "Fizzy card #" + strconv.Itoa(c.Number)
	}
	if u := strings.TrimSpace(c.URL); validURL(u) && textx.RuneLen(u) <= lim.URL {
		w.url = u
	}
	column, hasColumn := "", c.Column != nil
	if hasColumn {
		column = c.Column.Name
		if column == "" {
			column = cols[c.Column.ID]
		}
	}
	w.place = PlaceOf(column, c.Closed, c.Postponed)
	w.labels = labelsOf(c, w.place, lim.Labels)
	w.body = bodyOf(c, w.url, lim.Body)
	sum := sha256.Sum256(mustJSON([]any{w.title, w.body, w.labels, w.place.Column, w.place.Wait, w.url}))
	w.digest = hex.EncodeToString(sum[:])
	return w
}

// labelsOf are a card's labels: its tags as labels, golden when it is,
// and the label its column adds; sorted, unique, at most most (golden and
// the column's first).
func labelsOf(c Card, p Place, most int) []string {
	var first []string
	if c.Golden {
		first = append(first, "golden")
	}
	if p.Label != "" {
		first = append(first, p.Label)
	}
	var tags []string
	for _, t := range c.TagNames() {
		if l := Label(t); l != "" {
			tags = append(tags, l)
		}
	}
	sort.Strings(tags)
	seen := map[string]bool{}
	out := []string{}
	for _, l := range append(first, tags...) {
		if !seen[l] && len(out) < most {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// bodyOf is a card's body: its description as Markdown, its steps as a
// checklist, then where it came from. The description, then the steps, are
// cut so the whole fits in most bytes.
func bodyOf(c Card, url string, most int) string {
	desc := clean(c.Description)
	if desc == "" {
		desc = clean(PlainText(c.DescriptionHTML))
	}
	var steps []string
	for _, s := range c.Steps {
		if text := cutRunes(line(s.Content), 1000); text != "" {
			box := "[ ]"
			if s.Completed {
				box = "[x]"
			}
			steps = append(steps, "- "+box+" "+text)
		}
	}
	foot := "Imported from Fizzy card #" + strconv.Itoa(c.Number)
	if url != "" {
		foot += ": " + url
	}
	var names []string
	for i := range c.Assignees {
		if n := cutRunes(line(c.Assignees[i].Name), maxPersonLen); n != "" {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		foot += "\nAssigned in Fizzy: " + strings.Join(names, ", ")
	}
	foot = cutBytes(foot, most/4)
	checklist := strings.Join(steps, "\n")
	join := func(desc, checklist string) string {
		var parts []string
		for _, p := range []string{desc, checklist, foot} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	body := join(desc, checklist)
	if over := len(body) - most; over > 0 && desc != "" {
		desc = cutBytes(desc, max(len(desc)-over, 0))
		if len(desc) == len(cutMark) {
			desc = ""
		}
		body = join(desc, checklist)
	}
	if over := len(body) - most; over > 0 {
		body = join(desc, cutBytes(checklist, max(len(checklist)-over, 0)))
	}
	return body
}

// when is a Fizzy timestamp, or def when it is not one.
func when(s string, def time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	if t, _, err := clock.ParseISO(s); err == nil {
		return t.UTC()
	}
	return def
}

func boardOf(c Card) string {
	if c.Board != nil {
		return c.Board.ID
	}
	return ""
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // strings and slices of them always marshal
	}
	return b
}
