package web

import (
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// writeHunt puts a verified bug hunt on the harness's board as T0013: two
// confirmed findings (the second a behaviour change, its evidence a wide
// code block) and a duplicate.
func writeHunt(t *testing.T, h *harness) *ticket.Ticket {
	t.Helper()
	at := testNow.Add(-time.Hour)
	tk := ticket.New("T0013", ticket.NewIn{Project: "widgets", Title: "Bug hunt", Body: "Look.", Door: "clock", Column: "ready"}, at)
	tk.Kind = ticket.Some("task")
	tk.Findings = []ticket.Finding{
		{Title: "Raise a format error for `nil`", Severity: "low", Kind: "bug", Verdict: ticket.VerdictConfirmed,
			Evidence: "decode(nil) raised NoMethodError", Body: "`decode(nil)` raises NoMethodError."},
		{Title: "Keep hex leading zeros", Severity: "medium", Kind: "bug", BehaviourChange: true, Verdict: ticket.VerdictConfirmed,
			Evidence: "```ruby\n" + strings.Repeat("r.decode_hex(r.encode_hex(\"00ff\")) ", 8) + "# => [\"ff\"]\n```", Body: "Leading zeros are lost."},
		{Title: "Make decode_hex errors consistent", Severity: "low", Kind: "bug", Verdict: ticket.VerdictDuplicate, Duplicate: "PR #27",
			Evidence: "PR #27 adds the guard", Body: "No guard."},
	}
	ticket.Move(tk, "verifying", model.ReasonFindingsReported, "task done with 3 findings: x; a verifier checks them next", "", "", at)
	ticket.Move(tk, "needs_human", model.ReasonFindingsVerified, "findings verified: 2 of 3 confirmed", "", "", at)
	tk.Updated = at.Format(time.RFC3339)
	if err := os.WriteFile(h.paths.TicketFile("T0013"), ticket.Encode(tk), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	os.Chtimes(h.paths.TicketFile("T0013"), future, future)
	return tk
}

func TestTheFindingsAskRenders(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	writeHunt(t, h)
	w := h.do("GET", "/t/T0013", nil, c)
	page := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	for _, want := range []string{
		"File 2 confirmed findings as tickets?",
		`name="pick" value="1" id="finding-1" checked`, `name="pick" value="2" id="finding-2" checked`,
		`<select name="as-1"`, `<option value="change" selected>`, // a plain bug is a change
		`<select name="as-2"`, `<option value="intent" selected>`, // a behaviour change is designed first
		"confirmed · low bug", "confirmed · medium bug · changes behaviour",
		`class="pick finding off"`, "duplicate of PR #27",
		"<code>nil</code>", // titles are Markdown, escaped
		"<pre>",            // the evidence's code block, inside .md so it scrolls
		"File selected", "File none",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, `value="3" id="finding-3"`) {
		t.Error("a duplicate cannot be ticked")
	}
	// the desk row says what it wants
	if w := h.do("GET", "/", nil, c); !strings.Contains(w.Body.String(), "File findings · 2 of 3 confirmed") {
		t.Error("desk row")
	}
}

// What the person ticked and chose reaches ops: the picks and each one's kind.
func TestFileSelectedPostsPicksAndKinds(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	csrf := h.csrf(c)
	tk := writeHunt(t, h)
	a := present.AskFor(tk)
	form := url.Values{"csrf": {csrf}, "answer": {"file"}, "pick": {"1", "2"}, "as-1": {"intent"}, "as-2": {"change"}, "as-3": {"intent"},
		"ask": {a.Kind}, "since": {a.Since}}
	w := h.do("POST", "/t/T0013/act", form, c)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status %d", w.Code)
	}
	got := h.ops.takeAnswers()
	want := AnswerRequest{ID: "T0013", Value: "file", Picks: []int{1, 2}, As: map[int]string{1: "intent", 2: "change"}, Kind: a.Kind, Since: a.Since}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("request\n got %+v\nwant %+v", got, want)
	}
	form.Set("as-1", "task")
	w = h.do("POST", "/t/T0013/act", form, c)
	if f := flashOf(t, h, w); f == nil || f.Kind != "error" || len(h.ops.takeAnswers()) != 0 {
		t.Errorf("a bad kind reaches nothing: %+v", f)
	}
}

// A closed hunt still lists its findings and what each became.
func TestAClosedHuntListsItsFindings(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	writeHunt(t, h)
	h.change("T0013", func(tk *ticket.Ticket) {
		if _, err := ticket.FileFinding("T0014", tk, 1, "change", "maintainer", testNow); err != nil {
			t.Fatal(err)
		}
		ticket.CloseFindings(tk, "maintainer", "", testNow)
	})
	page := h.do("GET", "/t/T0013", nil, c).Body.String()
	for _, want := range []string{"filed as T0014 (change)", "Filed the findings as T0014 (change)."} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, `name="pick"`) {
		t.Error("nothing to tick on a closed hunt")
	}
}
