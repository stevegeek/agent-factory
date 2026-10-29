package ops_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func TestAddRecordsTheTicketsItWaitsFor(t *testing.T) {
	w := newWorld(t, twoProjects)
	ctx := context.Background()
	first, err := w.svc.Add(ctx, ops.AddIn{Project: "gem", Title: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.svc.Add(ctx, ops.AddIn{Project: "gem", Title: "second", After: []string{strings.ToLower(string(first))}})
	if err != nil {
		t.Fatal(err)
	}
	if got := w.load(second).After; !slices.Equal(got, []ticket.ID{first}) {
		t.Fatalf("after %v", got)
	}
	if got := w.load(first).After; got != nil {
		t.Fatalf("a ticket filed without after waits for %v", got)
	}
}

func TestAddRefusesToWaitForAnUnknownTicket(t *testing.T) {
	w := newWorld(t, twoProjects)
	var r *ops.Refused
	_, err := w.svc.Add(context.Background(), ops.AddIn{Project: "gem", Title: "x", After: []string{"T0099"}})
	if !errors.As(err, &r) || !strings.Contains(r.Message, "no ticket T0099") {
		t.Fatalf("%v", err)
	}
	if len(w.files()) != 0 {
		t.Fatalf("filed %v", w.files())
	}
}

func TestTheAfterAnswerSetsAndClearsTheList(t *testing.T) {
	w := newWorld(t, twoProjects)
	ctx := context.Background()
	first, _ := w.svc.Add(ctx, ops.AddIn{Project: "gem", Title: "first"})
	second, _ := w.svc.Add(ctx, ops.AddIn{Project: "gem", Title: "second"})
	if _, err := w.svc.Answer(ctx, ops.AnswerIn{ID: string(second), Value: ops.AnswerAfter, Note: string(first)}); err != nil {
		t.Fatal(err)
	}
	if got := w.load(second).After; !slices.Equal(got, []ticket.ID{first}) {
		t.Fatalf("after %v", got)
	}
	if _, err := w.svc.Answer(ctx, ops.AnswerIn{ID: string(first), Value: ops.AnswerAfter, Note: string(second)}); err == nil {
		t.Fatal("a cycle was accepted")
	}
	if _, err := w.svc.Answer(ctx, ops.AnswerIn{ID: string(second), Value: ops.AnswerAfter}); err != nil {
		t.Fatal(err)
	}
	if got := w.load(second).After; len(got) != 0 {
		t.Fatalf("after %v once cleared", got)
	}
}

func TestTheAfterAnswerReadsIdsOnSeparateLines(t *testing.T) {
	w := newWorld(t, twoProjects)
	ctx := context.Background()
	first, _ := w.svc.Add(ctx, ops.AddIn{Project: "gem", Title: "first"})
	second, _ := w.svc.Add(ctx, ops.AddIn{Project: "gem", Title: "second"})
	third, _ := w.svc.Add(ctx, ops.AddIn{Project: "gem", Title: "third"})
	note := string(first) + "\r\n" + string(second) + "\r\n"
	if _, err := w.svc.Answer(ctx, ops.AnswerIn{ID: string(third), Value: ops.AnswerAfter, Note: note}); err != nil {
		t.Fatal(err)
	}
	if got := w.load(third).After; !slices.Equal(got, []ticket.ID{first, second}) {
		t.Fatalf("after %v", got)
	}
}

func TestFileRecordsTheTicketsItWaitsFor(t *testing.T) {
	w := newWorld(t, twoProjects)
	ctx := context.Background()
	first, err := w.svc.File(ctx, "gem", "change", "first", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.svc.File(ctx, "gem", "change", "second", "", "", string(first))
	if err != nil {
		t.Fatal(err)
	}
	if got := w.load(second).After; !slices.Equal(got, []ticket.ID{first}) {
		t.Fatalf("after %v", got)
	}
	if got := w.load(first).After; got != nil {
		t.Fatalf("a ticket filed without after waits for %v", got)
	}
}
