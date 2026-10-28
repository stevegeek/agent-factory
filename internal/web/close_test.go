package web

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// writeQuestion puts an agent's open question on the harness's board as
// T0013, on widgets.
func writeQuestion(t *testing.T, h *harness) *ticket.Ticket {
	t.Helper()
	at := testNow.Add(-time.Hour)
	tk := ticket.New("T0013", ticket.NewIn{Project: "widgets", Title: "Add a flag", Body: "b", Door: "human", Column: "working"}, at)
	tk.Question = ticket.Some(ticket.Question{Text: "It exists already; what should change?", Stage: "work", Agent: "t0013-w1", At: at.Format(time.RFC3339)})
	ticket.Move(tk, "needs_human", model.ReasonAgentQuestion, "work asks: It exists already; what should change?", "", "", at)
	if err := os.WriteFile(h.paths.TicketFile("T0013"), ticket.Encode(tk), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	os.Chtimes(h.paths.TicketFile("T0013"), future, future)
	return tk
}

func TestCloseItAndDropAreTheMaintainersOnly(t *testing.T) {
	h := newHarness(t, false)
	verbs := &fakeVerbs{}
	h.srv.svc.Answers = Ops{Verbs: verbs}
	m := h.login()
	secret, _ := h.users.Add("robin", []string{"widgets"})
	robin := h.loginWith(secret, "/")
	a := present.AskFor(writeQuestion(t, h))

	page := h.do("GET", "/t/T0013", nil, m).Body.String()
	for _, want := range []string{`value="answer"`, `value="close"`, `value="drop"`, "Close it", "Drop"} {
		if !strings.Contains(page, want) {
			t.Errorf("the maintainer's page lacks %q", want)
		}
	}
	page = h.do("GET", "/t/T0013", nil, robin).Body.String()
	if !strings.Contains(page, `value="answer"`) || strings.Contains(page, `value="close"`) || strings.Contains(page, `value="drop"`) {
		t.Errorf("another user's answers:\n%s", page)
	}

	form := url.Values{"answer": {"close"}, "note": {"it exists"}, "ask": {a.Kind}, "since": {a.Since}}
	if w := h.do("POST", "/t/T0013/act", form, m); w.Code != http.StatusForbidden {
		t.Errorf("no CSRF token: %d", w.Code)
	}
	form.Set("csrf", h.csrf(m))
	if w := h.doWith("POST", "/t/T0013/act", form, map[string]string{"Sec-Fetch-Site": "cross-site"}, m); w.Code != http.StatusForbidden {
		t.Errorf("cross-site: %d", w.Code)
	}
	if len(verbs.answered) != 0 {
		t.Fatalf("a refused post reached ops: %+v", verbs.answered)
	}
	if w := h.do("POST", "/t/T0013/act", form, m); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d", w.Code)
	}
	if len(verbs.answered) != 1 || verbs.answered[0].Value != "close" || verbs.answered[0].Note != "it exists" || verbs.answered[0].By != "" {
		t.Errorf("reached ops: %+v", verbs.answered)
	}
}
