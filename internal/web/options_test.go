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

// writeAsking puts T0013 on the harness's board: a worker's question with
// options, the second one recommended.
func writeAsking(t *testing.T, h *harness, sel string) *ticket.Ticket {
	t.Helper()
	at := testNow.Add(-time.Hour)
	tk := ticket.New("T0013", ticket.NewIn{Project: "widgets", Title: "Rename the parser", Body: "b", Door: "human", Column: "working"}, at)
	tk.Question = ticket.Some(ticket.Question{Text: "Which name?", Stage: "work", Agent: "t0013-w1", At: at.Format(time.RFC3339), Select: sel,
		Options: []ticket.QuestionOption{
			{Key: "keep", Label: "Keep the old name", Detail: "The API stays."},
			{Key: "split", Label: "Rename it in its own change", Recommended: sel == "", Effect: ticket.EffectFile, Then: ticket.EffectContinue},
			{Key: "done", Label: "It is fine", Effect: ticket.EffectClose},
		}})
	if sel == ticket.SelectMany {
		tk.Question.V.Options = tk.Question.V.Options[:1]
		tk.Question.V.Options = append(tk.Question.V.Options, ticket.QuestionOption{Key: "docs", Label: "Fix the docs"})
	}
	ticket.Move(tk, "needs_human", model.ReasonAgentQuestion, "work asks: Which name?", "", "", at)
	tk.Updated = at.Format(time.RFC3339)
	if err := os.WriteFile(h.paths.TicketFile("T0013"), ticket.Encode(tk), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	os.Chtimes(h.paths.TicketFile("T0013"), future, future)
	return tk
}

func TestAQuestionsOptionsRender(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	writeAsking(t, h, "")
	w := h.do("GET", "/t/T0013", nil, c)
	page := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	for _, want := range []string{
		`type="radio" name="option" value="keep" id="option-keep">`,
		`type="radio" name="option" value="split" id="option-split" checked>`,
		`type="radio" name="option" value="" id="option-none">`,
		"None: my note answers", "Recommended",
		"files a change, then carries on", "closes the ticket", "a new run carries on",
		"The API stays.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, `value="answer" data-needs-note`) {
		t.Error("an answer with options needs no note")
	}
	h2 := newHarness(t, false)
	c2 := h2.login()
	writeAsking(t, h2, ticket.SelectMany)
	page = h2.do("GET", "/t/T0013", nil, c2).Body.String()
	if !strings.Contains(page, `type="checkbox" name="option" value="docs"`) || strings.Contains(page, "option-none") {
		t.Error("select many renders checkboxes and no None")
	}
}

func TestAChosenOptionReachesTheService(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	tk := writeAsking(t, h, "")
	a := present.AskFor(tk)
	form := url.Values{"csrf": {h.csrf(c)}, "answer": {"answer"}, "option": {"split"}, "note": {"soon"}, "ask": {a.Kind}, "since": {a.Since}}
	if w := h.do("POST", "/t/T0013/act", form, c); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d", w.Code)
	}
	got := h.ops.takeAnswers()
	want := AnswerRequest{ID: "T0013", Value: "answer", Note: "soon", Options: []string{"split"}, Kind: a.Kind, Since: a.Since}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("request\n got %+v\nwant %+v", got, want)
	}
	form.Set("csrf", "not-the-token")
	if w := h.do("POST", "/t/T0013/act", form, c); w.Code != http.StatusForbidden || len(h.ops.takeAnswers()) != 0 {
		t.Errorf("a bad CSRF token: status %d", w.Code)
	}
}
