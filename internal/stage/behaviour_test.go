package stage

import (
	"testing"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Every stage has a behaviour (and, in prompts_test, a golden prompt).
func TestEveryStageHasABehaviour(t *testing.T) {
	for _, info := range model.Stages() {
		b := For(info.Name)
		if b.Prompt == nil || b.Finish == nil {
			t.Errorf("stage %s has no behaviour", info.Name)
		}
		if KeepBranch(info.Name) != info.Branch {
			t.Errorf("stage %s keeps its branch: %v", info.Name, KeepBranch(info.Name))
		}
	}
	if b := For("nonsense"); b.Prompt != nil || b.Finish != nil {
		t.Error("an unknown stage has no behaviour")
	}
}

func TestStageForTicket(t *testing.T) {
	// stage_for:
	// port: TF IntentTest.test_design_waits_for_review_then_approve_files_the_chosen_tickets
	cases := []struct {
		kind, column, mode string
		want               model.Stage
	}{
		{"", "triage", "", model.StageTriage}, {"", "ready", "", model.StageWork}, {"", "review", "", model.StageReview},
		{"", "security", "", model.StageSecurity}, {"", "working", "", ""}, {"", "gate", "", ""}, {"", "needs_human", "", ""},
		{"task", "ready", "", model.StageTask}, {"task", "triage", "", ""}, {"intent", "ready", "", model.StageDesign},
		{"manage", "ready", "", model.StageManage}, {"prospect", "ready", "", model.StageProspect},
		{"prospect", "voting", "", model.StageVote}, {"prospect", "triage", "", ""}, {"proposal", "ready", "", ""},
		{"change", "ready", "", model.StageWork}, {"", "ready", "board", ""}, {"task", "ready", "board", ""},
	}
	for _, c := range cases {
		tk := decode(t, `{"id": "T0001", "column": "`+c.column+`", "history": []}`)
		if c.kind != "" {
			tk.Kind = ticket.Some(c.kind)
		} else {
			tk.Kind = ticket.Nil[string]()
		}
		if c.mode != "" {
			tk.Mode = ticket.Some(c.mode)
		}
		got, ok := ForTicket(tk)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s in %s (%s): %q %v", c.kind, c.column, c.mode, got, ok)
		}
	}
}

func TestAgentNamesCountEarlierStarts(t *testing.T) {
	tk := decode(t, `{"id": "T0042", "history": [{"at": "x", "event": "start", "detail": "work t0042-w1 on b"},
		{"at": "x", "event": "start", "detail": "review t0042-r1", "type": "started", "stage": "review", "agent": "t0042-r1", "host": "local"},
		{"at": "x", "event": "start", "detail": "work t0042-w2 on b", "stage": "work"}]}`)
	for s, want := range map[model.Stage]string{model.StageWork: "t0042-w3", model.StageReview: "t0042-r2", model.StageVote: "t0042-v1"} {
		if got := AgentName(tk, s); got != want {
			t.Errorf("%s: %s, want %s", s, got, want)
		}
	}
}

func TestParseReportFailsClosed(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"status": "done", "x": [1, 2.50, -0, 1e2], "x": 3}`, `{"status": "done", "x": 3}`},
		{"\xef\xbb\xbf" + `{"a": [2.50, -0, 1e2, 1E400, 12345678901234567890]}`, `{"a": [2.5, 0, 100.0, Infinity, 12345678901234567890]}`},
		{`not json`, `{"status": "blocked", "verdict": "fail", "summary": "report.json is not valid JSON"}`},
		{``, `{"status": "blocked", "verdict": "fail", "summary": "report.json is not valid JSON"}`},
		{`{"a": 1} trailing`, `{"status": "blocked", "verdict": "fail", "summary": "report.json is not valid JSON"}`},
		{`[1, 2]`, `{"status": "blocked", "verdict": "fail", "summary": "report.json is not a JSON object"}`},
		{`"done"`, `{"status": "blocked", "verdict": "fail", "summary": "report.json is not a JSON object"}`},
		{`null`, `{"status": "blocked", "verdict": "fail", "summary": "report.json is not a JSON object"}`},
		{"\xff\xfe{}", `{"status": "blocked", "verdict": "fail", "summary": "report.json is not valid JSON"}`},
	}
	for _, c := range cases {
		if got := string(jsonx.EncodePython(ParseReport([]byte(c.in)), -1)); got != c.want {
			t.Errorf("%q: %s", c.in, got)
		}
	}
}

func TestPyInt(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{`3`, 3, true}, {`-2`, -2, true}, {`3.9`, 3, true}, {`-3.9`, -3, true}, {`1e2`, 100, true}, {`true`, 1, true},
		{`false`, 0, true}, {`" 4 "`, 4, true}, {`"+5"`, 5, true}, {`"1_0"`, 10, true}, {`"١٢"`, 12, true},
		{`"3.0"`, 0, false}, {`"x"`, 0, false}, {`""`, 0, false}, {`"1__0"`, 0, false}, {`"_1"`, 0, false},
		{`null`, 0, false}, {`[1]`, 0, false}, {`{}`, 0, false}, {`"0x10"`, 0, false},
	}
	for _, c := range cases {
		v, err := jsonx.Parse([]byte(c.in))
		if err != nil {
			t.Fatal(err)
		}
		got, ok := pyInt(v)
		if got != c.want || ok != c.ok {
			t.Errorf("int(%s): %d %v", c.in, got, ok)
		}
	}
}
