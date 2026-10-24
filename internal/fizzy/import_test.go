package fizzy

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

const testConfig = `
[board]
people = ["alice"]

[projects.notes]
mode = "board"

[projects.fac]
path = "/nonexistent"
github = "o/fac"
`

type importFixture struct {
	t     *testing.T
	dump  string
	board *store.Board
	clk   *clock.Fixed
	cfg   *config.Config
}

func newImport(t *testing.T) *importFixture {
	t.Helper()
	env := testkit.Root(t)
	p := paths.FromEnv(env.Vars)
	clk := &clock.Fixed{T: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	f := &importFixture{t: t, dump: filepath.Join(t.TempDir(), "dump"), clk: clk, cfg: cfg,
		board: &store.Board{Reader: &boardread.Reader{Paths: p}, Paths: p, Clock: clk}}
	f.writeDump(fixtureCards(), fixtureComments())
	return f
}

func (f *importFixture) importer() Importer {
	return Importer{Board: f.board, Config: f.cfg, Clock: f.clk}
}

func (f *importFixture) run(dry bool) Report {
	f.t.Helper()
	r, err := f.importer().Import(context.Background(), Dump{Dir: f.dump}, Mapping{"b1": "notes"}, dry)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *importFixture) writeDump(cards []map[string]any, comments map[int][]map[string]any) {
	f.t.Helper()
	writeJSON(f.t, filepath.Join(f.dump, "boards.json"), []map[string]any{{"id": "b1", "name": "Product"}, {"id": "b2", "name": "Archive"}})
	writeJSON(f.t, filepath.Join(f.dump, "b1", "columns.json"), []map[string]any{{"id": "c-review", "name": "Review"}})
	var listed []map[string]any
	for _, c := range cards {
		short := map[string]any{}
		for k, v := range c {
			if k != "steps" {
				short[k] = v
			}
		}
		listed = append(listed, short)
		if _, ok := c["steps"]; ok {
			writeJSON(f.t, filepath.Join(f.dump, "b1", "cards", strconv.Itoa(c["number"].(int))+".json"), c)
		}
	}
	writeJSON(f.t, filepath.Join(f.dump, "b1", "cards.json"), listed)
	for n, cms := range comments {
		writeJSON(f.t, filepath.Join(f.dump, "b1", "comments", strconv.Itoa(n)+".json"), cms)
	}
	writeJSON(f.t, filepath.Join(f.dump, "b2", "cards.json"), []map[string]any{{"id": "z1", "number": 90, "title": "Old"}})
}

func card(id string, n int, title, column string, extra map[string]any) map[string]any {
	c := map[string]any{"id": id, "number": n, "title": title, "status": "published", "created_at": "2025-03-01T09:00:00.000Z",
		"url": "https://fizzy.example/1000/cards/" + strconv.Itoa(n), "board": map[string]any{"id": "b1"},
		"creator": map[string]any{"id": "u1", "name": "Ana Example"}, "tags": []string{}, "assignees": []any{}}
	if column != "" {
		c["column"] = map[string]any{"id": "c-" + strings.ToLower(column), "name": column}
	}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

func fixtureCards() []map[string]any {
	return []map[string]any{
		card("k1", 1, "Fix\tthe login\x07 page", "Maybe?", map[string]any{
			"description": "The **login** page\r\nbreaks on Safari.", "tags": []string{"Bug", "UI Polish"},
			"assignees": []any{map[string]any{"name": "Bo Example"}, map[string]any{"name": "Cy Example"}},
			"steps":     []any{map[string]any{"content": "reproduce", "completed": true}, map[string]any{"content": "fix", "completed": false}},
		}),
		card("k2", 2, "Golden rule", "Next", map[string]any{"golden": true}),
		card("k3", 3, "Build it", "In progress", nil),
		card("k4", 4, "Check it", "", map[string]any{"column": map[string]any{"id": "c-review"}}), // the name from columns.json
		card("k5", 5, "Vendor", "Waiting on vendor", nil),
		card("k6", 6, "Later", "Next", map[string]any{"postponed": true}),
		card("k7", 7, "Shipped", "Next", map[string]any{"closed": true}),
		card("k8", 8, "", "Someday", map[string]any{"description_html": "<p>Hello <strong>there</strong></p>"}),
	}
}

func comment(id, at, who, text string) map[string]any {
	return map[string]any{"id": id, "created_at": at, "creator": map[string]any{"name": who},
		"body": map[string]any{"plain_text": text, "html": "<p>" + text + "</p>"}}
}

func fixtureComments() map[int][]map[string]any {
	return map[int][]map[string]any{
		1: {comment("m2", "2025-03-03T10:00:00Z", "Bo Example", "second"), comment("m1", "2025-03-02T10:00:00Z", "Ana Example", "first")},
		3: {{"id": "m3", "created_at": "2025-04-01T00:00:00Z", "creator": map[string]any{"name": "Cy"}, "body": map[string]any{"html": "<p>only <em>html</em></p>"}}},
	}
}

func (f *importFixture) tickets() map[int]*ticket.Ticket {
	f.t.Helper()
	ts, problems := f.board.Reader.List()
	if len(problems) > 0 {
		f.t.Fatalf("board problems: %v", problems)
	}
	out := map[int]*ticket.Ticket{}
	for _, t := range ts {
		rec, ok := t.Imported.Get()
		if !ok {
			f.t.Fatalf("%s has no imported record", t.ID)
		}
		if out[rec.Number] != nil {
			f.t.Fatalf("card #%d imported twice", rec.Number)
		}
		out[rec.Number] = t
	}
	return out
}

func TestImportFilesOneBoardTicketPerCard(t *testing.T) {
	f := newImport(t)
	r := f.run(false)
	if len(r.Boards) != 1 || r.Boards[0].Created != 8 || r.Boards[0].Comments != 3 || len(r.Unmapped) != 1 || r.Unmapped[0].ID != "b2" {
		t.Fatalf("report %+v", r)
	}
	text := r.Text(false)
	for _, want := range []string{"board b1 (Product) -> notes: 8 cards", "created 8, refreshed 0, unchanged 0, skipped 0; 3 comments copied",
		"columns: inbox 1, next 2, doing 2, waiting 2, done 1", "labels: bug, golden, review, ui-polish", "skipped (not mapped): b2 (Archive)"} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
	ts := f.tickets()
	if len(ts) != 8 {
		t.Fatalf("%d tickets", len(ts))
	}
	for n, want := range map[int]struct {
		column, wait string
		labels       string
	}{
		1: {"inbox", "", "bug,ui-polish"}, 2: {"next", "", "golden"}, 3: {"doing", "", ""}, 4: {"doing", "", "review"},
		5: {"waiting", "Waiting on vendor", ""}, 6: {"waiting", NotNow, ""}, 7: {"done", "", ""}, 8: {"next", "", ""},
	} {
		tk := ts[n]
		w, _ := tk.Waiting.Get()
		if tk.Column != want.column || w.Text != want.wait || strings.Join(tk.Labels, ",") != want.labels {
			t.Errorf("card #%d: %s wait %q labels %v; want %+v", n, tk.Column, w.Text, tk.Labels, want)
		}
		if tk.Project != "notes" || !tk.Board() || tk.Door != string(model.DoorImport) || tk.By != "Ana Example (Fizzy)" ||
			tk.Created != "2025-03-01T09:00:00+00:00" || tk.KindName() != "item" || tk.Assignee.Present() {
			t.Errorf("card #%d: %s %v %s %q %s %q", n, tk.Project, tk.Board(), tk.Door, tk.By, tk.Created, tk.KindName())
		}
		if len(tk.Links) != 1 || tk.Links[0].Rel != "origin" || tk.Links[0].URL != "https://fizzy.example/1000/cards/"+strconv.Itoa(n) {
			t.Errorf("card #%d links %+v", n, tk.Links)
		}
		// typed history: created at the card's time, the move (if any) now, by the maintainer
		h := tk.History
		if h[0].Type != model.EventCreated || h[0].At != "2025-03-01T09:00:00+00:00" || h[0].Data.(ticket.Created).By != "Ana Example (Fizzy)" {
			t.Errorf("card #%d created %+v", n, h[0])
		}
		if want.column != "inbox" {
			last := h[len(h)-1]
			m, ok := last.Data.(ticket.Moved)
			if !ok || m.Reason != model.ReasonImported || m.By != "alice" || m.To != want.column || last.At != "2026-10-19T12:00:00+00:00" {
				t.Errorf("card #%d last event %+v", n, last)
			}
		}
	}
	one := ts[1]
	if one.Title != "Fix the login page" {
		t.Errorf("title %q", one.Title)
	}
	wantBody := "The **login** page\nbreaks on Safari.\n\n- [x] reproduce\n- [ ] fix\n\n" +
		"Imported from Fizzy card #1: https://fizzy.example/1000/cards/1\nAssigned in Fizzy: Bo Example, Cy Example"
	if one.Body != wantBody {
		t.Errorf("body:\n%q\nwant\n%q", one.Body, wantBody)
	}
	if len(one.Comments) != 2 || one.Comments[0].Text != "first" || one.Comments[0].By != "Ana Example (Fizzy)" ||
		one.Comments[0].At != "2025-03-02T10:00:00+00:00" || one.Comments[1].Text != "second" || one.Comments[1].N != 2 {
		t.Errorf("comments %+v", one.Comments)
	}
	if c := ts[3].Comments; len(c) != 1 || c[0].Text != "only _html_" {
		t.Errorf("an HTML-only comment %+v", c)
	}
	if ts[8].Title != "Fizzy card #8" || !strings.HasPrefix(ts[8].Body, "Hello **there**\n\nImported from Fizzy card #8") {
		t.Errorf("card #8: %q %q", ts[8].Title, ts[8].Body)
	}
	// Foreman, the index and `factory check --board` read every ticket cleanly.
	if problems := f.board.Reader.CheckBoard(); len(problems) > 0 {
		t.Errorf("check --board: %v", problems)
	}
	ix, err := f.board.RefreshIndex(context.Background())
	if err != nil || ix.Len() != 8 {
		t.Errorf("index %d %v", ix.Len(), err)
	}
}

func TestImportAgainChangesNothing(t *testing.T) {
	f := newImport(t)
	f.run(false)
	before := map[string]string{}
	for _, tk := range f.tickets() {
		b, _ := os.ReadFile(f.board.Paths.TicketFile(string(tk.ID)))
		before[string(tk.ID)] = string(b)
	}
	f.clk.T = f.clk.T.Add(time.Hour)
	r := f.run(false)
	if b := r.Boards[0]; b.Created != 0 || b.Refreshed != 0 || b.Unchanged != 8 || b.Comments != 0 {
		t.Errorf("rerun %+v", b)
	}
	for id, was := range before {
		if b, _ := os.ReadFile(f.board.Paths.TicketFile(id)); string(b) != was {
			t.Errorf("%s was rewritten", id)
		}
	}
}

func TestImportRefreshesAChangedCardAndCopiesNewCommentsOnly(t *testing.T) {
	f := newImport(t)
	f.run(false)
	// On the board meanwhile: a label of our own, and card 3 moved on.
	ts := f.tickets()
	if err := f.board.Changing(context.Background(), string(ts[1].ID), store.NoWait, func(t *ticket.Ticket) error {
		t.Labels = append(t.Labels, "mine")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.board.Changing(context.Background(), string(ts[3].ID), store.NoWait, func(t *ticket.Ticket) error {
		ticket.Move(t, "next", model.ReasonParked, "", "", "alice", f.clk.T)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cards := fixtureCards()
	cards[0]["title"] = "Fix the login page on Safari"
	cards[0]["tags"] = []string{"bug"}
	cards[0]["column"] = map[string]any{"id": "c-next", "name": "Next"}
	cms := fixtureComments()
	cms[1] = append(cms[1], comment("m9", "2025-05-01T00:00:00Z", "Bo Example", "third"))
	f.writeDump(cards, cms)
	f.clk.T = f.clk.T.Add(time.Hour)
	r := f.run(false)
	if b := r.Boards[0]; b.Created != 0 || b.Refreshed != 1 || b.Unchanged != 7 || b.Comments != 1 {
		t.Errorf("refresh %+v", b)
	}
	ts = f.tickets()
	one := ts[1]
	if one.Title != "Fix the login page on Safari" || one.Column != "next" || strings.Join(one.Labels, ",") != "bug,mine" {
		t.Errorf("card #1: %q %s %v", one.Title, one.Column, one.Labels)
	}
	if len(one.Comments) != 3 || one.Comments[2].Text != "third" || one.Comments[2].N != 3 {
		t.Errorf("comments %+v", one.Comments)
	}
	var edited bool
	for _, e := range one.History {
		if ed, ok := e.Data.(ticket.Edited); ok && ed.By == "alice" && strings.Join(ed.Fields, ",") == "title,labels" {
			edited = true
		}
	}
	if !edited {
		t.Errorf("no edited event for title and labels: %+v", one.History)
	}
	if ts[3].Column != "next" {
		t.Errorf("card #3 did not change in Fizzy, but its local move was undone: %s", ts[3].Column)
	}
	if problems := f.board.Reader.CheckBoard(); len(problems) > 0 {
		t.Errorf("check --board: %v", problems)
	}
	// And once more: nothing new.
	if b := f.run(false).Boards[0]; b.Unchanged != 8 {
		t.Errorf("third run %+v", b)
	}
}

func TestImportDryRunWritesNothing(t *testing.T) {
	f := newImport(t)
	r := f.run(true)
	if b := r.Boards[0]; b.Created != 8 || b.Columns["waiting"] != 2 {
		t.Errorf("dry run %+v", b)
	}
	if text := r.Text(true); !strings.Contains(text, "would create 8, refresh 0, leave 0 unchanged, skip 0") {
		t.Errorf("dry report:\n%s", text)
	}
	entries, _ := os.ReadDir(f.board.Paths.Tickets)
	if len(entries) != 0 {
		t.Errorf("a dry run wrote %v", entries)
	}
	f.run(false)
	cards := fixtureCards()
	cards[1]["title"] = "Golden rules"
	f.writeDump(cards, fixtureComments())
	if b := f.run(true).Boards[0]; b.Refreshed != 1 || b.Unchanged != 7 {
		t.Errorf("dry rerun %+v", b)
	}
	if f.tickets()[2].Title != "Golden rule" {
		t.Error("a dry run refreshed a ticket")
	}
}

func TestImportRefusesAProjectThatIsNotABoard(t *testing.T) {
	f := newImport(t)
	ctx := context.Background()
	for m, want := range map[string]string{
		"fac":     `fac is a factory project: a Fizzy board goes to a board project (mode = "board" in [projects.fac] of config.toml)`,
		"missing": `no project missing: add [projects.missing] with mode = "board" to config.toml first`,
	} {
		_, err := f.importer().Import(ctx, Dump{Dir: f.dump}, Mapping{"b1": m}, false)
		if err == nil || err.Error() != want {
			t.Errorf("map to %s: %v", m, err)
		}
	}
	if _, err := f.importer().Import(ctx, Dump{Dir: f.dump}, Mapping{"b7": "notes"}, false); err == nil || !strings.Contains(err.Error(), "no board b7") {
		t.Errorf("a board not in the dump: %v", err)
	}
	if entries, _ := os.ReadDir(f.board.Paths.Tickets); len(entries) != 0 {
		t.Errorf("a refused import wrote %v", entries)
	}
}

func TestImportCutsToTheTicketLimits(t *testing.T) {
	f := newImport(t)
	long := strings.Repeat("word ", 100_000)
	tags := []string{}
	for i := 0; i < 30; i++ {
		tags = append(tags, "tag"+strconv.Itoa(i))
	}
	cards := []map[string]any{card("k1", 1, strings.Repeat("T", 400), "Next", map[string]any{"description": long, "tags": tags, "golden": true,
		"steps": []any{map[string]any{"content": "step one", "completed": false}}})}
	cms := map[int][]map[string]any{1: {comment("m1", "2025-03-02T10:00:00Z", strings.Repeat("N", 200), strings.Repeat("c", 40_000)),
		comment("m2", "2025-03-02T11:00:00Z", "Bo", " \x00 ")}}
	f.writeDump(cards, cms)
	f.run(false)
	tk := f.tickets()[1]
	if n := len([]rune(tk.Title)); n != 300 {
		t.Errorf("title is %d characters", n)
	}
	if len(tk.Body) > 64<<10 || !strings.Contains(tk.Body, cutMark) || !strings.HasSuffix(tk.Body, "- [ ] step one\n\nImported from Fizzy card #1: https://fizzy.example/1000/cards/1") {
		t.Errorf("body: %d bytes, ends %q", len(tk.Body), tk.Body[len(tk.Body)-120:])
	}
	if len(tk.Labels) != 20 || tk.Labels[0] != "golden" {
		t.Errorf("labels %v", tk.Labels)
	}
	if len(tk.Comments) != 1 || len(tk.Comments[0].Text) > 16<<10 || len([]rune(tk.Comments[0].By)) != maxPersonLen+len(" (Fizzy)") {
		t.Errorf("comments %d, first %d bytes by %q", len(tk.Comments), len(tk.Comments[0].Text), tk.Comments[0].By)
	}
	if rec, _ := tk.Imported.Get(); len(rec.Comments) != 2 {
		t.Errorf("the empty comment is not recorded as dealt with: %v", rec.Comments)
	}
	if problems := f.board.Reader.CheckBoard(); len(problems) > 0 {
		t.Errorf("check --board: %v", problems)
	}
}
