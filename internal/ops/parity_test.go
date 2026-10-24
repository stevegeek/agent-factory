package ops_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/testkit/pyticket"
)

// scenario is one file written by testdata/scenarios/gen.py: a maintainer
// verb run by Python on a small board.
type scenario struct {
	Name   string            `json:"name"`
	Config string            `json:"config"`
	Op     string            `json:"op"`
	Args   map[string]any    `json:"args"`
	Before map[string]string `json:"before"`
	Stdout string            `json:"stdout"`
	Error  *string           `json:"error"`
	Wakes  int               `json:"wakes"`
	Logs   []string          `json:"logs"`
	After  map[string]string `json:"after"`
}

func (s scenario) str(k string) string {
	v, _ := s.Args[k].(string)
	return v
}

func (s scenario) flag(k string) bool {
	v, _ := s.Args[k].(bool)
	return v
}

func (s scenario) words(k string) string {
	var out []string
	for _, w := range s.Args[k].([]any) {
		out = append(out, w.(string))
	}
	return strings.Join(out, " ")
}

// run is the CLI command the scenario names, through ops: what it prints
// on stdout and the error it exits with.
func (s scenario) run(t *testing.T, svc *ops.Service) (string, error) {
	ctx := context.Background()
	line := func(text string, err error) (string, error) {
		if err != nil || text == "" {
			return "", err
		}
		return text + "\n", nil
	}
	switch s.Op {
	case "add":
		timeout, _ := s.Args["timeout"].(float64)
		id, err := svc.Add(ctx, ops.AddIn{Project: s.str("project"), Title: s.str("title"), Recipe: s.str("recipe"),
			Body: s.str("body"), BodyFile: s.str("body_file"), Door: s.str("door"), Triage: s.flag("triage"),
			Host: s.str("host"), Task: s.flag("task"), Intent: s.flag("intent"), Manage: s.flag("manage"),
			Timeout: int(timeout)})
		return line(string(id), err)
	case "move":
		return line(svc.Move(ctx, s.str("ticket"), s.str("column"), s.str("note")))
	case "note":
		return "", svc.Note(ctx, s.str("ticket"), s.words("text"))
	case "reply":
		return line(svc.Reply(ctx, s.str("ticket"), s.words("text")))
	case "ask":
		id, err := svc.Ask(ctx, s.str("ticket"), s.words("question"))
		return line(string(id), err)
	case "approve":
		var only []int
		if o := s.str("only"); o != "" {
			for _, x := range strings.Split(o, ",") {
				n, err := strconv.Atoi(x)
				if err != nil {
					t.Fatal(err)
				}
				only = append(only, n)
			}
		}
		ids, err := svc.Approve(ctx, s.str("ticket"), only)
		var names []string
		for _, id := range ids {
			names = append(names, string(id))
		}
		return line(strings.Join(names, " "), err)
	case "redirect":
		return "", svc.Redirect(ctx, s.str("ticket"), s.str("note"))
	case "merge":
		return line(svc.RequestMerge(ctx, s.str("ticket")))
	case "host":
		return "", svc.Host(ctx, s.str("ticket"), s.str("host"))
	case "result":
		return line(svc.Result(ctx, s.str("ticket")))
	case "migrate":
		lines, err := svc.Migrate(ctx)
		out := ""
		for _, l := range lines {
			out += l + "\n"
		}
		return out, err
	}
	t.Fatalf("unknown op %q", s.Op)
	return "", nil
}

// The verbs against Python's own runs: the same board, call and frozen
// clock give the same files byte for byte, the same output or refusal, the
// same wakes and the same log lines.
//
// port: TF IntentTest.test_design_waits_for_review_then_approve_files_the_chosen_tickets
// port: TF IntentTest.test_redirect_reruns_the_designer_with_the_feedback
// port: TF IntentTest.test_design_without_tickets_is_blocked_and_cannot_be_approved
// port: TF ManagerTest.test_a_recipe_files_with_its_title_body_and_kind
// port: TF ManagerTest.test_filing_runs_here_only_on_the_factory_project
// port: TF ManagerTest.test_drafts_wait_for_you_and_a_reply_carries_on
// port: TF AgentQuestionTest.test_an_agent_asks_the_reply_carries_the_stage_on
// port: TF MigrateTest.test_migrate_backfills_asks_votes_and_dropped_proposals_once
// port: TF TaskTicketTest.test_migrate_sends_answers_already_on_the_desk_back_to_their_ticket
// port: TF GithubOutTest.test_merge_it_only_asks_and_the_tick_merges
// port: TT ModeTest.test_factory_add_on_a_board_project_says_use_ticket_create
func TestParityWithPython(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("testdata", "scenarios", "*.json"))
	if err != nil || len(names) < 60 {
		t.Fatalf("scenarios: %d, %v", len(names), err)
	}
	for _, name := range names {
		var sc scenario
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &sc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Run(sc.Name, func(t *testing.T) {
			w := newWorld(t, sc.Config)
			for f, body := range sc.Before {
				w.file(f, body)
			}
			out, err := sc.run(t, w.svc)
			if sc.Error != nil {
				if err == nil || err.Error() != *sc.Error {
					t.Errorf("error %v, want %q", err, *sc.Error)
				}
			} else if err != nil {
				t.Errorf("error %v", err)
			}
			if out != sc.Stdout {
				t.Errorf("stdout %q, want %q", out, sc.Stdout)
			}
			if got := w.wakes.count(); got != sc.Wakes {
				t.Errorf("wakes %d, want %d", got, sc.Wakes)
			}
			if got := w.log.take(); !reflect.DeepEqual(got, sc.Logs) && !(len(got) == 0 && len(sc.Logs) == 0) {
				t.Errorf("log lines\n got %q\nwant %q", got, sc.Logs)
			}
			got := w.files()
			if !reflect.DeepEqual(sortedKeys(got), sortedKeys(sc.After)) {
				t.Errorf("files %v, want %v", sortedKeys(got), sortedKeys(sc.After))
			}
			for f, want := range sc.After {
				if g := pyticket.History(got[f]); g != want {
					t.Errorf("%s differs (-python +go):\n%s", f, testkit.Diff(want, g))
				}
			}
		})
	}
}
