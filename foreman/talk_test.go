package main

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestConversationShowsTheThreadAndAReplyStaysInIt(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	for _, tk := range fixtures(t) {
		if w := h.do("GET", "/t/"+tk.ID+"/talk", nil, c); w.Code != 200 {
			t.Errorf("%s talk: %d", tk.ID, w.Code)
		}
	}
	body := h.do("GET", "/t/T0009/talk", nil, c).Body.String()
	for _, want := range []string{`class="thread"`, `value="talk"`, `name="view" value="talk"`, "Your answer", `aria-current="page">Conversation`} {
		if !strings.Contains(body, want) {
			t.Errorf("T0009 conversation lacks %q", want)
		}
	}
	if !strings.Contains(h.do("GET", "/t/T0009", nil, c).Body.String(), `href="/t/T0009/talk"`) {
		t.Errorf("the ticket page should link its conversation")
	}
	w := h.do("POST", "/t/T0009/act", url.Values{"csrf": {h.srv.auth.CSRF(h.srv.auth.session(c.Value))}, "answer": {"talk"}, "view": {"talk"},
		"note": {"Yes, keep the alias"}}, c)
	if w.Code != 303 || w.Header().Get("Location") != "/t/T0009/talk" {
		t.Fatalf("reply: %d %s", w.Code, w.Header().Get("Location"))
	}
	if got := h.argv(); len(got) != 1 || strings.Join(got[0], " ") != "reply T0009 -- Yes, keep the alias" {
		t.Errorf("reply argv: %v", got)
	}
}

func TestConversationOrder(t *testing.T) {
	tk := &Ticket{ID: "T0042", Project: "p", Title: "Tune it", Body: "Make it faster.", Door: "human", Column: "needs_human",
		Notes:   []Note{{Text: "Your question: “8 or 16?” The maintainer's answer: 16"}},
		Reports: map[string]Report{"work": {"status": "question", "question": "8 or 16?", "summary": "stopped"}},
		History: []Hist{
			{At: "2026-10-16T10:00:00Z", Event: "created"},
			{At: "2026-10-16T10:01:00Z", Event: "start", Detail: "work t0042-w1 on bot/x"},
			{At: "2026-10-16T10:05:00Z", Event: "finished", Detail: "work t0042-w1"},
			{At: "2026-10-16T10:05:00Z", Event: "move", Detail: "working -> needs_human: work asks: 8 or 16?"},
			{At: "2026-10-16T10:07:00Z", Event: "note", Detail: "Your question: …"},
		}}
	v := Conversation(tk, map[string]*Ticket{"T0042": tk}, "", time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	var got []string
	for _, m := range v.Msgs {
		got = append(got, m.Who+"|"+m.Side+"|"+m.Text+"|"+m.Tone)
	}
	want := []string{"You|me|**Tune it**\n\nMake it faster.|", "Worker|agent|**Question:** 8 or 16?|question", "You|me|16|"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("thread:\n%s", strings.Join(got, "\n"))
	}
}

func TestAnAgentsCommentOnAFactoryTicketShows(t *testing.T) {
	tk := &Ticket{ID: "T0051", Project: "p", Title: "Tune it", Door: "human", Column: "ready",
		Comments: []Comment{{N: 1, At: "2026-10-16T10:02:00Z", By: "example-agent", Type: "comment", Text: "Seen on staging too."}},
		History: []Hist{
			{At: "2026-10-16T10:00:00Z", Event: "created"},
			{At: "2026-10-16T10:02:00Z", Event: "comment", Type: "commented", By: "example-agent", N: 1, Detail: "example-agent: Seen on staging"},
		}}
	all := map[string]*Ticket{"T0051": tk}
	v := Conversation(tk, all, "", time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	if m := v.Msgs[len(v.Msgs)-1]; m.Who != "example-agent" || m.Side != "agent" || m.Text != "Seen on staging too." {
		t.Errorf("conversation: %+v", m)
	}
	steps := BuildStory(tk, all).Steps
	found := false
	for _, st := range steps {
		found = found || (st.Who == "example-agent" && st.What == "Comment: Seen on staging too.")
	}
	if !found {
		t.Errorf("the story leaves the comment out: %+v", steps)
	}
}
