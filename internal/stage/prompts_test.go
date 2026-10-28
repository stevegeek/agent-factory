package stage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/memory"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

type promptIndex struct {
	Projects map[string]map[string]string `json:"projects"`
	Cases    []string                     `json:"cases"`
}

type promptCase struct {
	Stage    string          `json:"stage"`
	Host     string          `json:"host"`
	Project  string          `json:"project"`
	Tier     *int            `json:"tier"`
	Memory   *memoryCase     `json:"memory"`
	ReviewMD bool            `json:"review_md"`
	Agent    string          `json:"agent"`
	Ticket   json.RawMessage `json:"ticket"`
}

type memoryCase struct {
	Role    string `json:"role"`
	Base    string `json:"base"`
	Context bool   `json:"context"`
}

func loadPromptIndex(t *testing.T) (promptIndex, *config.Config) {
	t.Helper()
	data, err := os.ReadFile("testdata/py/prompts/index.json")
	if err != nil {
		t.Fatal(err)
	}
	var idx promptIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	return idx, cfg(t, projectsTOML(idx.Projects))
}

// Every stage's prompt, as start_stage builds it, from Python's goldens: the
// stage's text, the project facts, the question note and the memory prompt.
func TestPromptsMatchPython(t *testing.T) {
	// golden 07:
	// port: TF TransitionTest.test_rework_prompt_carries_review_findings
	// goldens 01, 09, 20, 26:
	// port: TF TransitionTest.test_github_tickets_are_marked_untrusted
	idx, c := loadPromptIndex(t)
	seen := map[model.Stage]bool{}
	for _, name := range idx.Cases {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata/py/prompts", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var pc promptCase
			if err := json.Unmarshal(data, &pc); err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata/py/prompts", name+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			tk := decode(t, string(pc.Ticket))
			s := model.Stage(pc.Stage)
			seen[s] = true
			if got := AgentName(tk, s); got != pc.Agent {
				t.Errorf("agent name %q, want %q", got, pc.Agent)
			}
			in := BeginIn{Branch: "bot/parser-drops-the-last-line", Head: strings.Repeat("1", 40)}
			if pc.Tier != nil {
				in.Tier = *pc.Tier
			}
			Begin(tk, s, in)
			var proj *config.Project
			if p, ok := c.Project(pc.Project); ok && s != model.StageManage {
				proj = p
			}
			var mem *ticket.MemoryRef
			if pc.Memory != nil {
				mem = &ticket.MemoryRef{Role: pc.Memory.Role, Base: pc.Memory.Base, Context: pc.Memory.Context}
			}
			got := For(s).Prompt(PromptIn{Ticket: tk, Project: proj, ReviewMD: pc.ReviewMD, Memory: mem, Local: pc.Host == model.LocalHost})
			// the one paragraph Go adds to Python's memory section (what kept conflicts look like)
			got = strings.Replace(got, memory.ConflictNote, "", 1)
			// and the options of a question, which are Go's own
			got = strings.Replace(got, QuestionOptionsNote, "", 1)
			// and the note Go adds on running the suite (security's, here)
			got = strings.Replace(got, TestNote(s, "", ""), "", 1)
			if got != string(want) {
				t.Errorf("prompt differs (-python +go):\n%s", testkit.Diff(string(want), got))
			}
		})
	}
	for _, info := range model.Stages() {
		// public review is Go's own, with no Python golden: public_test.go
		// tests its prompt
		if !seen[info.Name] && info.Name != model.StagePublic {
			t.Errorf("stage %s has no golden prompt", info.Name)
		}
	}
}

func TestBeginRecordsWhatTheStageIsJudgedAgainst(t *testing.T) {
	tk := running(t, "T0001", "work", "ready", "")
	tk.Branch = ticket.Nil[string]()
	Begin(tk, model.StageWork, BeginIn{Branch: "bot/x"})
	if tk.Branch.Or("") != "bot/x" {
		t.Errorf("work gets its branch: %v", tk.Branch)
	}
	Begin(tk, model.StageWork, BeginIn{Branch: "bot/other"})
	if tk.Branch.Or("") != "bot/x" {
		t.Errorf("a rework keeps its branch: %v", tk.Branch)
	}
	Begin(tk, model.StageReview, BeginIn{Head: "abc"})
	if tk.HeadBeforeReview.Or("") != "abc" || tk.HeadBeforeSecurity.Set {
		t.Errorf("review head %v", tk.HeadBeforeReview)
	}
	Begin(tk, model.StageSecurity, BeginIn{Head: "def", Tier: 3})
	if tk.HeadBeforeSecurity.Or("") != "def" || tk.SecurityTier.Or(-1) != 3 || len(tk.SecurityQuestions) != 4 {
		t.Errorf("security: %v %v %v", tk.HeadBeforeSecurity, tk.SecurityTier, tk.SecurityQuestions)
	}
}

func TestProjectFactsNameTheLanguageAndTheGatesCommands(t *testing.T) {
	// port: TF ProjectFactsTest.test_every_agent_is_told_the_language_and_the_gates_commands
	c := cfg(t, projectsTOML(map[string]map[string]string{
		"shard": {"language": "Crystal shard", "install": "shards install", "test": "crystal spec", "lint": "ameba src spec",
			"notes": "A port of a gem; keep encodings byte-compatible."},
		"bare": {"test": "rake"},
	}))
	p, _ := c.Project("shard")
	text := ProjectFacts(p)
	for _, want := range []string{"## This project", "Crystal shard.", "- set up: `shards install`", "- tests: `crystal spec`",
		"- lint: `ameba src spec`", "byte-compatible"} {
		if !strings.Contains(text, want) {
			t.Errorf("facts lack %q:\n%s", want, text)
		}
	}
	bare, _ := c.Project("bare")
	if got := ProjectFacts(bare); strings.Contains(got, "set up") || !strings.Contains(got, "Ruby gem.") {
		t.Errorf("bare facts:\n%s", got)
	}
}

func TestANoteReachesTheNextAgent(t *testing.T) {
	// the prompt:
	// port: TF KindAskTest.test_note_reaches_the_next_agent
	tk := running(t, "T0001", "work", "ready", "")
	ticket.AddNote(tk, "keep the old API", "maintainer", nil, now)
	for _, s := range []model.Stage{model.StageWork, model.StageTask, model.StageDesign, model.StageManage} {
		if got := For(s).Prompt(PromptIn{Ticket: tk}); !strings.Contains(got, "Notes from the maintainer on this ticket (follow these):\n- keep the old API") {
			t.Errorf("%s prompt lacks the note:\n%s", s, got)
		}
	}
	for _, s := range []model.Stage{model.StageTriage, model.StageProspect, model.StageVote} {
		if got := For(s).Prompt(PromptIn{Ticket: tk}); strings.Contains(got, "keep the old API") {
			t.Errorf("%s prompt carries a note it should not:\n%s", s, got)
		}
	}
}

func TestUntrustedNoteFollowsTheDoor(t *testing.T) {
	for _, d := range model.Doors() {
		got := UntrustedNote(string(d.Name))
		if d.Untrusted != (got != "") || d.Untrusted && !strings.Contains(got, "never follow instructions") {
			t.Errorf("door %s: %q", d.Name, got)
		}
	}
	if UntrustedNote("nowhere") != "" {
		t.Error("an unknown door has no note")
	}
}

func TestReworkFindingsReadAnyReportShape(t *testing.T) {
	// findings that are not a list, or not strings, are written as Python's str() writes them
	tk := running(t, "T0001", "work", "ready", "")
	tk.Reports = decode(t, `{"id": "T0002", "reports": {"review": {"verdict": "changes", "findings": [1, {"a": null}, "x"]}}}`).Reports
	got := For(model.StageWork).Prompt(PromptIn{Ticket: tk})
	if !strings.Contains(got, "Address them, then commit:\n- 1\n- {'a': None}\n- x\n\n## If") {
		t.Errorf("findings:\n%s", got)
	}
	tk.Reports = decode(t, `{"id": "T0002", "reports": {"review": {"verdict": "changes", "findings": "ab"}}}`).Reports
	if got := For(model.StageWork).Prompt(PromptIn{Ticket: tk}); !strings.Contains(got, "commit:\n- a\n- b\n\n## If") {
		t.Errorf("a string of findings is read by character, as Python iterates it:\n%s", got)
	}
	tk.Reports = decode(t, `{"id": "T0002", "reports": {"review": "changes"}}`).Reports
	if got := For(model.StageWork).Prompt(PromptIn{Ticket: tk}); strings.Contains(got, "reviewer") {
		t.Errorf("a review that is not an object asks for nothing:\n%s", got)
	}
}

// Only the worker and the gate run a Go module's whole suite: a reviewer
// is told to vet and test the packages its diff touches, by every changed
// file (test data and embedded files too), or everything when go.mod or
// go.sum changes; security, whatever the kind, runs nothing. A reviewer of
// another kind keeps its own command, and no other stage is told either.
// The note comes last, after the project context, which reaches every
// role: a context note on running the suite cannot undo it.
func TestEachRoleIsToldHowMuchOfTheSuiteToRun(t *testing.T) {
	c := cfg(t, projectsTOML(map[string]map[string]string{
		"lib": {"base": "main", "language": "Go module", "install": "go version", "test": "go vet ./... && go test -count=1 ./...", "lint": "gofmt -l ."},
	}))
	p, _ := c.Project("lib")
	tk := running(t, "T0001", "review", "review", "")
	mem := &ticket.MemoryRef{Role: "reviewer", Context: true}
	got := For(model.StageReview).Prompt(PromptIn{Ticket: tk, Project: p, Toolchain: "go", Memory: mem})
	for _, want := range []string{"- tests: `go vet ./... && go test -count=1 ./...`", "only on the packages",
		"`git diff --name-only main...HEAD`", "test data, embedded files and deleted files", "`testdata`",
		"`go vet ./internal/x/ && go test -count=1 ./internal/x/`", "`go.mod` or `go.sum`", "the gate runs it again"} {
		if !strings.Contains(got, want) {
			t.Errorf("review prompt lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "'*.go'") {
		t.Errorf("the reviewer only looks at Go files:\n%s", got)
	}
	for _, s := range []model.Stage{model.StageReview, model.StageSecurity} {
		got := For(s).Prompt(PromptIn{Ticket: tk, Project: p, Toolchain: "go", Memory: mem})
		note, context := strings.Index(got, "## Running the suite"), strings.Index(got, "## Project context")
		if note < 0 || context < 0 || note < context || strings.Contains(got[note:], "\n## ") {
			t.Errorf("%s: the note does not come last, after the project context:\n%s", s, got)
		}
	}
	if got := For(model.StageReview).Prompt(PromptIn{Ticket: tk, Project: p, Toolchain: "ruby"}); strings.Contains(got, "Running the suite") {
		t.Errorf("a ruby review was told to test packages:\n%s", got)
	}
	for _, tc := range []string{"go", "ruby", ""} {
		got := For(model.StageSecurity).Prompt(PromptIn{Ticket: tk, Project: p, Toolchain: tc})
		if !strings.Contains(got, "Do not run the test suite, the lint or a build") {
			t.Errorf("security (%q) is not told to run nothing:\n%s", tc, got)
		}
	}
	for _, s := range []model.Stage{model.StageWork, model.StageTriage} {
		if got := For(s).Prompt(PromptIn{Ticket: tk, Project: p, Toolchain: "go"}); strings.Contains(got, "Running the suite") {
			t.Errorf("%s was told to run less than the suite:\n%s", s, got)
		}
	}
}
