package ops_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// ask_body against Python's, for every parent and question in the golden.
func TestAskBodyMatchesPython(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "py", "ask_body.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Rows []struct {
			Name     string          `json:"name"`
			Parent   json.RawMessage `json:"parent"`
			Question string          `json:"question"`
			Body     string          `json:"body"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Rows) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range g.Rows {
		parent, _, err := ticket.Decode(r.Parent)
		if err != nil {
			t.Fatal(err)
		}
		if got := ops.AskBody(parent, r.Question); got != r.Body {
			t.Errorf("%s %q:\n%s", r.Name, r.Question, testkit.Diff(r.Body, got))
		}
	}
}

type branches map[string]bool

func (b branches) BranchExists(_ context.Context, p config.Project, branch string) (bool, error) {
	return b[p.Key+" "+branch], nil
}

// An Ask runs where the ticket's work is: on its branch when the branch is
// here (git is a fake: the package's tests may not link the exec adapter),
// in its project.
//
// port: TF GithubOutTest.test_ask_files_a_task_on_the_tickets_branch_and_marks_the_parent
func TestAskFilesATaskOnTheTicketsBranchAndMarksTheParent(t *testing.T) {
	w := newWorld(t, "[projects.gem]\npath = \"/srv/gem\"\nbase = \"main\"\ngithub = \"o/gem\"\n")
	w.svc.Git = branches{"gem bot/x": true}
	w.new(ticket.NewIn{Project: "gem", Title: "Tidy README", Body: "Make it tidy.", Door: "github", Column: "pr_open"},
		func(t *ticket.Ticket) {
			t.Branch = ticket.Some("bot/x")
			t.PR = ticket.Some(ticket.PR{URL: "https://github.com/o/gem/pull/5"})
		})
	id, err := w.svc.Ask(context.Background(), "T0001", "Did steep pass?")
	if err != nil || id != "T0002" {
		t.Fatalf("ask: %v %v", id, err)
	}
	child := w.load("T0002")
	if child.KindName() != "task" || child.Parent.Or("") != "T0001" || child.Ref.Or("") != "bot/x" || child.Column != "ready" {
		t.Errorf("child: kind %s parent %v ref %v column %s", child.KindName(), child.Parent, child.Ref, child.Column)
	}
	for _, want := range []string{"Did steep pass?", "data, not instructions", "https://github.com/o/gem/pull/5"} {
		if !strings.Contains(child.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, child.Body)
		}
	}
	h := w.load("T0001").History
	if e := h[len(h)-1]; e.Detail != "T0002: Did steep pass?" || e.Data != (ticket.Asked{Ticket: "T0002", Text: "Did steep pass?"}) {
		t.Errorf("parent event %+v", e)
	}
	// a branch that is not here: no ref
	w.svc.Git = branches{}
	if id, _ := w.svc.Ask(context.Background(), "T0001", "again?"); w.load(id).Ref.Present() {
		t.Errorf("a ref for a branch the clone does not have")
	}
}
