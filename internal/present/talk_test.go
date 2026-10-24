package present

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

var noon = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func msgs(v *TalkView) string {
	var got []string
	for _, m := range v.Msgs {
		got = append(got, m.Who+"|"+m.Side+"|"+m.Text+"|"+m.Tone)
	}
	return strings.Join(got, "\n")
}

func TestConversationOfAnAgentsQuestion(t *testing.T) {
	ts := fixtures(t)
	v := Conversation(find(ts, "T0009"), ByID(ts), nil, noon)
	if !v.Question || v.Label != "Your answer" || v.Button != "Answer" || v.Waiting == "" {
		t.Errorf("a question asks for an answer: %+v", v)
	}
	for _, x := range ts {
		Conversation(x, ByID(ts), nil, noon) // every fixture renders
	}
}

func TestConversationOrder(t *testing.T) {
	x := tk(t, `{"id": "T0042", "project": "p", "title": "Tune it", "body": "Make it faster.", "door": "human", "column": "needs_human",
		"notes": [{"text": "Your question: “8 or 16?” The maintainer's answer: 16"}],
		"reports": {"work": {"status": "question", "question": "8 or 16?", "summary": "stopped"}},
		"history": [
			{"at": "2026-10-16T10:00:00Z", "event": "created"},
			{"at": "2026-10-16T10:01:00Z", "event": "start", "detail": "work t0042-w1 on bot/x"},
			{"at": "2026-10-16T10:05:00Z", "event": "finished", "detail": "work t0042-w1"},
			{"at": "2026-10-16T10:05:00Z", "event": "move", "detail": "working -> needs_human: work asks: 8 or 16?"},
			{"at": "2026-10-16T10:07:00Z", "event": "note", "detail": "Your question: …"}]}`)
	v := Conversation(x, map[ticket.ID]*ticket.Ticket{"T0042": x}, nil, noon)
	want := []string{"You|me|**Tune it**\n\nMake it faster.|", "Worker|agent|**Question:** 8 or 16?|question", "You|me|16|"}
	if got := msgs(v); got != strings.Join(want, "\n") {
		t.Errorf("thread:\n%s", got)
	}
}

func TestAnAgentsCommentOnAFactoryTicketShows(t *testing.T) {
	x := tk(t, `{"id": "T0051", "project": "p", "title": "Tune it", "door": "human", "column": "ready",
		"comments": [{"n": 1, "at": "2026-10-16T10:02:00Z", "by": "example-agent", "type": "comment", "text": "Seen on staging too."}],
		"history": [
			{"at": "2026-10-16T10:00:00Z", "event": "created"},
			{"at": "2026-10-16T10:02:00Z", "event": "comment", "detail": "example-agent: Seen on staging", "type": "commented",
			 "by": "example-agent", "n": 1}]}`)
	all := map[ticket.ID]*ticket.Ticket{"T0051": x}
	v := Conversation(x, all, nil, noon)
	if m := v.Msgs[len(v.Msgs)-1]; m.Who != "example-agent" || m.Side != "agent" || m.Text != "Seen on staging too." {
		t.Errorf("conversation: %+v", m)
	}
	found := false
	for _, st := range BuildStory(x, all).Steps {
		found = found || (st.Who == "example-agent" && st.What == "Comment: Seen on staging too.")
	}
	if !found {
		t.Errorf("the story leaves the comment out: %+v", BuildStory(x, all).Steps)
	}
}

// An archived run's own report is read through the caller's archive, only
// for an agent name of the factory's form and at most 1 MiB; anything else
// falls back to the ticket's latest report.
func TestConversationReadsArchivedReportsSafely(t *testing.T) {
	x := tk(t, `{"id": "T0045", "project": "p", "title": "T", "door": "human", "column": "review",
		"reports": {"work": {"status": "done", "summary": "second try"}},
		"history": [
			{"at": "2026-10-16T10:00:00Z", "event": "finished", "detail": "work t0045-w1", "type": "finished", "stage": "work", "agent": "t0045-w1"},
			{"at": "2026-10-16T10:10:00Z", "event": "finished", "detail": "work ../../etc", "type": "finished", "stage": "work", "agent": "../../etc"},
			{"at": "2026-10-16T10:20:00Z", "event": "finished", "detail": "work t0045-w3", "type": "finished", "stage": "work", "agent": "t0045-w3"}]}`)
	var asked []string
	archive := func(id ticket.ID, agent string) []byte {
		asked = append(asked, string(id)+"/"+agent)
		switch agent {
		case "t0045-w1":
			return []byte(`{"status": "done", "summary": "first try"}`)
		case "t0045-w3":
			return []byte(`{"summary": "` + strings.Repeat("x", 1<<20) + `"}`)
		}
		return []byte(`{"summary": "leaked"}`)
	}
	got := msgs(Conversation(x, nil, archive, noon))
	want := "You|me|**T**|\nWorker|agent|first try|\nWorker|agent|Finished.|\nWorker|agent|second try|"
	if got != want {
		t.Errorf("thread:\n%s", got)
	}
	if fmt.Sprint(asked) != "[T0045/t0045-w1 T0045/t0045-w3]" {
		t.Errorf("archive asked for %v", asked)
	}
}

func TestConversationOfARunningAgent(t *testing.T) {
	x := tk(t, `{"id": "T0046", "project": "p", "title": "T", "door": "clock", "column": "working",
		"agent": {"name": "t0046-w1", "stage": "work", "started": "2026-10-16T10:00:00Z", "host": "local"}, "history": []}`)
	v := Conversation(x, nil, nil, noon)
	if v.Waiting != "Worker is working on it now." || v.Msgs[0].Who != "Clock" || v.Msgs[0].Side != "system" {
		t.Errorf("running: %+v", v)
	}
	if v.Msgs[0].When != "" {
		t.Errorf("no history, no time: %q", v.Msgs[0].When)
	}
}

func TestWhen(t *testing.T) {
	if got := When(ParseTime("2026-10-16T09:30:00Z"), noon); got != "09:30" {
		t.Errorf("today: %q", got)
	}
	if got := When(ParseTime("2026-10-15T09:30:00Z"), noon); got != "15 Oct 09:30" {
		t.Errorf("yesterday: %q", got)
	}
	if When(time.Time{}, noon) != "" {
		t.Errorf("no time")
	}
}

func TestColumnWordsInTheThread(t *testing.T) {
	for col, want := range map[string]string{"pr_open": "PR opened", "done": "Closed as done", "rejected": "Dropped",
		"ready": model.ColumnLabel("ready")} {
		if got := threadColumnLabel(col); got != want {
			t.Errorf("%s: %q", col, got)
		}
	}
}
