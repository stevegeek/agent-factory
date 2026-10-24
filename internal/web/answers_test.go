package web

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// fakeVerbs stands in for *ops.Service behind OpsVerbs: it records what
// reached it and answers as the test says.
type fakeVerbs struct {
	answered []ops.AnswerIn
	filed    [][4]string
	looked   []string
	out      string
	err      error
	screen   ops.Screen
}

func (f *fakeVerbs) Answer(_ context.Context, in ops.AnswerIn) (string, error) {
	f.answered = append(f.answered, in)
	return f.out, f.err
}

func (f *fakeVerbs) File(_ context.Context, project, kind, title, body, _ string) (ticket.ID, error) {
	f.filed = append(f.filed, [4]string{project, kind, title, body})
	if f.err != nil {
		return "", f.err
	}
	return "T0042", nil
}

func (f *fakeVerbs) Look(_ context.Context, id string, lines int) (ops.Screen, error) {
	f.looked = append(f.looked, fmt.Sprintf("%s/%d", id, lines))
	return f.screen, f.err
}

// The desk offers exactly what package ops offers, for every ask kind and
// for a ticket waiting to go out as a PR: web keeps no answer table of its
// own (the decisions stay in ops).
func TestAnswersAreOpsAnswers(t *testing.T) {
	o := Ops{Verbs: &fakeVerbs{}}
	pr := ticket.Some(ticket.PR{Number: 7, URL: "https://github.com/example/widgets/pull/7"})
	tickets := []*ticket.Ticket{
		{ID: "T0001", Column: string(model.ColNeedsHuman), PR: pr},
		{ID: "T0002", Column: string(model.ColPROpen), PR: pr, Gate: ticket.Some(ticket.Gate{AutoEligible: true, ChecksPass: true})},
		{ID: "T0003", Column: string(model.ColReadyForPR)},
		{ID: "T0004", Column: string(model.ColReadyForPR), PR: pr},
		{ID: "T0005", Column: string(model.ColNeedsHuman), Source: ticket.Some(ticket.Source{Issue: 3})},
	}
	asks := []*ticket.Ask{nil}
	for _, k := range model.AskKinds() {
		asks = append(asks, &ticket.Ask{Kind: string(k.Kind), Since: "2026-10-18T09:00:00+00:00", RetryColumn: ticket.Some("ready")})
	}
	seen := 0
	for _, tk := range tickets {
		for _, a := range asks {
			want := ops.Answers(tk, a)
			got := o.Answers(tk, a)
			if len(got) != len(want) {
				t.Errorf("%s %v: %d answers, ops offers %d", tk.ID, a, len(got), len(want))
				continue
			}
			for i := range want {
				if !reflect.DeepEqual(Answer(want[i]), got[i]) {
					t.Errorf("%s %v answer %d: %+v, ops offers %+v", tk.ID, a, i, got[i], want[i])
				}
			}
			seen += len(got)
		}
	}
	if seen == 0 {
		t.Fatal("no answers compared")
	}
}

// web's Answer and ops' Answer are the same shape field for field, so a
// conversion never drops one (the conversion itself fails to compile if
// they part; this names the fields when they do).
func TestAnswerShapeMatchesOps(t *testing.T) {
	a, b := reflect.TypeOf(Answer{}), reflect.TypeOf(ops.Answer{})
	if a.NumField() != b.NumField() {
		t.Fatalf("web.Answer has %d fields, ops.Answer %d", a.NumField(), b.NumField())
	}
	for i := 0; i < a.NumField(); i++ {
		if a.Field(i).Name != b.Field(i).Name || a.Field(i).Type != b.Field(i).Type {
			t.Errorf("field %d: %s %s vs %s %s", i, a.Field(i).Name, a.Field(i).Type, b.Field(i).Name, b.Field(i).Type)
		}
	}
}

// What a person posted reaches ops as given: the id, the answer, the note
// as typed (ops trims and checks it), the picks, and the ask the page
// showed, which ops checks under the ticket's lock.
func TestAnswerReachesOpsAsPosted(t *testing.T) {
	f := &fakeVerbs{out: "moved T0002 to ready"}
	o := Ops{Verbs: f}
	out, err := o.Answer(context.Background(), AnswerRequest{ID: "T0002", Value: "approve", Note: "  -v smaller ",
		Picks: []int{1, 3}, Kind: "design", Since: "2026-10-18T09:00:00+00:00"})
	if err != nil || out != "moved T0002 to ready" {
		t.Fatalf("answer: %q %v", out, err)
	}
	want := ops.AnswerIn{ID: "T0002", Value: "approve", Note: "  -v smaller ", Picks: []int{1, 3}, Kind: "design",
		Since: "2026-10-18T09:00:00+00:00"}
	if len(f.answered) != 1 || !reflect.DeepEqual(f.answered[0], want) {
		t.Errorf("reached ops: %+v", f.answered)
	}
}

// An answer to an ask that moved on is web's ErrStale, so the page says
// "changed since you opened it"; other refusals keep their text for the
// person.
func TestAnswerErrors(t *testing.T) {
	f := &fakeVerbs{err: fmt.Errorf("T0002: %w", ops.ErrStale)}
	o := Ops{Verbs: f}
	_, err := o.Answer(context.Background(), AnswerRequest{ID: "T0002", Value: "start"})
	if !errors.Is(err, ErrStale) || !errors.Is(err, ops.ErrStale) {
		t.Errorf("stale: %v", err)
	}
	f.err = &ops.Refused{Code: ops.BadRequest, Message: "the note is longer than 4000 characters"}
	_, err = o.Answer(context.Background(), AnswerRequest{ID: "T0002", Value: "start"})
	var r *ops.Refused
	if errors.Is(err, ErrStale) || !errors.As(err, &r) || err.Error() != "the note is longer than 4000 characters" {
		t.Errorf("refusal: %v", err)
	}
}

// Filing from ＋ is ops' File with the form's fields; the new id comes back.
func TestFileIsOpsFile(t *testing.T) {
	f := &fakeVerbs{}
	o := Ops{Verbs: f}
	id, err := o.File(context.Background(), NewTicket{Project: "widgets", Kind: "intent", Title: "-weird", Body: "b"})
	if err != nil || id != "T0042" {
		t.Fatalf("file: %q %v", id, err)
	}
	if len(f.filed) != 1 || f.filed[0] != [4]string{"widgets", "intent", "-weird", "b"} {
		t.Errorf("filed: %v", f.filed)
	}
	f.err = &ops.Refused{Code: ops.BadRequest, Message: "unknown project gadgets"}
	if _, err := o.File(context.Background(), NewTicket{Project: "gadgets"}); err == nil || err.Error() != "unknown project gadgets" {
		t.Errorf("refusal: %v", err)
	}
}

// A look is `factory look T --lines N` in-process: the header line, then
// the screen.
func TestLookIsOpsLook(t *testing.T) {
	f := &fakeVerbs{screen: ops.Screen{Agent: ticket.Agent{Name: "t0011-w1", Stage: "work", Started: "2026-10-18T09:00:00+00:00",
		Host: "local"}, Text: "$ make test"}}
	o := Ops{Verbs: f}
	out, err := o.Look(context.Background(), "T0011", 150)
	if err != nil || out != "t0011-w1 · work · started 2026-10-18T09:00:00+00:00\n$ make test" {
		t.Fatalf("look: %q %v", out, err)
	}
	if len(f.looked) != 1 || f.looked[0] != "T0011/150" {
		t.Errorf("looked: %v", f.looked)
	}
	f.err = &ops.ErrNoAgent{ID: "T0011"}
	if _, err := o.Look(context.Background(), "T0011", 150); err == nil || err.Error() != "T0011 has no agent running" {
		t.Errorf("no agent: %v", err)
	}
}

// OpsServices fills the three answer services and the config, and leaves
// the token APIs to the caller.
func TestOpsServices(t *testing.T) {
	f := &fakeVerbs{}
	cfg := fakeConfig{}
	s := OpsServices(f, cfg)
	if s.Answers == nil || s.Filer == nil || s.Looker == nil || s.Config == nil || s.API != nil || s.MCP != nil {
		t.Errorf("services: %+v", s)
	}
}
