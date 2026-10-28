package hosts

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

const local = model.LocalHost

func cfgOf(t *testing.T, toml string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(toml))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// HOSTS in test_factory.py, with max_agents = 1 (PickHostTest.cfg).
const pickCfg = `max_agents = 1
[hosts.pi]
ssh = "factory@pi"
slots = 1
stages = ["triage", "review"]
`

func pick(c *config.Config, stage string, busy map[string]int, down []string, pinned string, localOK bool) string {
	d := map[string]bool{}
	for _, h := range down {
		d[h] = true
	}
	h, ok := Pick(c, stage, busy, d, pinned, localOK, "")
	if ok != (h != "") {
		panic(fmt.Sprintf("Pick(%s) = %q, %v", stage, h, ok))
	}
	return h
}

// port: TF PickHostTest.test_local_first_then_helper_as_overflow_then_none
func TestLocalFirstThenHelperAsOverflowThenNone(t *testing.T) {
	c := cfgOf(t, pickCfg)
	for _, tc := range []struct {
		busy map[string]int
		want string
	}{
		{map[string]int{local: 0}, local},
		{map[string]int{local: 1}, "pi"},
		{map[string]int{local: 1, "pi": 1}, ""},
	} {
		if got := pick(c, "review", tc.busy, nil, "", true); got != tc.want {
			t.Errorf("busy %v: %q, want %q", tc.busy, got, tc.want)
		}
	}
}

// port: TF PickHostTest.test_helper_takes_only_listed_stages_and_not_when_down
func TestHelperTakesOnlyListedStagesAndNotWhenDown(t *testing.T) {
	c := cfgOf(t, pickCfg)
	if got := pick(c, "work", map[string]int{local: 1}, nil, "", true); got != "" {
		t.Errorf("work went to %q", got)
	}
	if got := pick(c, "triage", map[string]int{local: 1}, []string{"pi"}, "", true); got != "" {
		t.Errorf("down helper got %q", got)
	}
}

// port: TF PickHostTest.test_pinned_ticket_runs_only_on_its_host_whatever_the_stage
func TestPinnedTicketRunsOnlyOnItsHost(t *testing.T) {
	c := cfgOf(t, pickCfg)
	for _, tc := range []struct {
		stage  string
		busy   map[string]int
		down   []string
		pinned string
		want   string
	}{
		{"work", map[string]int{local: 0}, nil, "pi", "pi"},
		{"work", map[string]int{local: 0, "pi": 1}, nil, "pi", ""},
		{"work", map[string]int{local: 0}, []string{"pi"}, "pi", ""},
		{"review", map[string]int{local: 1}, nil, local, ""},
		{"review", map[string]int{local: 0}, nil, "gone", ""},
	} {
		if got := pick(c, tc.stage, tc.busy, tc.down, tc.pinned, true); got != tc.want {
			t.Errorf("%+v: %q", tc, got)
		}
	}
}

const preferCfg = `max_agents = 1
[hosts.pi]
ssh = "factory@pi"
slots = 1
stages = ["triage", "review"]
prefer = ["task"]
`

// port: TF TaskTicketTest.test_helper_preferred_for_tasks_then_local_and_never_local_without_a_project
func TestHelperPreferredForTasksThenLocalNeverLocalWithoutAProject(t *testing.T) {
	c := cfgOf(t, preferCfg)
	for _, tc := range []struct {
		stage   string
		busy    map[string]int
		pinned  string
		localOK bool
		want    string
	}{
		{"task", map[string]int{local: 0}, "", true, "pi"},
		{"task", map[string]int{local: 0, "pi": 1}, "", true, local},
		{"review", map[string]int{local: 0}, "", true, local},
		{"task", map[string]int{local: 0, "pi": 1}, "", false, ""},
		{"task", map[string]int{local: 0}, local, false, ""},
	} {
		if got := pick(c, tc.stage, tc.busy, nil, tc.pinned, tc.localOK); got != tc.want {
			t.Errorf("%+v: %q", tc, got)
		}
	}
}

// The routing half of LimitTest: a paused host gets no new work, this host
// still does (the requeue itself is the pipeline's).
// port: TF LimitTest.test_a_helper_job_out_of_credit_goes_back_and_pauses_its_host
func TestPausedHostsGetNoWork(t *testing.T) {
	c := cfgOf(t, `agent_timeout_minutes = 30
[hosts.pi]
ssh = "factory@pi"
slots = 1
stages = ["triage", "review"]
`)
	busy := map[string]int{local: 0, "pi": 0}
	if got := pick(c, "task", busy, []string{"pi"}, "", true); got != local {
		t.Errorf("got %q", got)
	}
	if got := pick(c, "task", busy, []string{"pi", local}, "", true); got != "" {
		t.Errorf("got %q", got)
	}
	if got := pick(c, "task", busy, []string{"pi"}, "pi", true); got != "" {
		t.Errorf("pinned to a paused host: %q", got)
	}
}

// More of pick_host's rules: capacity is max_agents here (default 2) and
// slots on a helper (default 1); prefer beats this host; hosts are tried in
// the order config declares them; a busy count for an unknown host is
// ignored.
func TestPickOrderAndCapacity(t *testing.T) {
	c := cfgOf(t, `[hosts.a]
ssh = "a"
stages = ["work"]
[hosts.b]
ssh = "b"
slots = 2
prefer = ["review"]
stages = ["work"]
`)
	for _, tc := range []struct {
		stage string
		busy  map[string]int
		down  []string
		want  string
	}{
		{"work", map[string]int{}, nil, local},
		{"work", map[string]int{local: 1}, nil, local},
		{"work", map[string]int{local: 2}, nil, "a"},
		{"work", map[string]int{local: 2, "a": 1}, nil, "b"},
		{"work", map[string]int{local: 2, "a": 1, "b": 1}, nil, "b"},
		{"work", map[string]int{local: 2, "a": 1, "b": 2}, nil, ""},
		{"review", map[string]int{}, nil, "b"},
		{"review", map[string]int{"b": 2}, nil, local},
		{"review", map[string]int{"b": 2, local: 2}, nil, ""},
		{"review", map[string]int{}, []string{"b"}, local},
		{"triage", map[string]int{local: 2}, nil, ""},
		{"work", map[string]int{local: 2}, []string{"a"}, "b"},
	} {
		if got := pick(c, tc.stage, tc.busy, tc.down, "", true); got != tc.want {
			t.Errorf("%+v: %q", tc, got)
		}
	}
}

func ticketOf(t *testing.T, js string) *ticket.Ticket {
	t.Helper()
	tk, _, err := ticket.Decode([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// port: TF GithubOutTest.test_models_per_stage_on_any_host_and_per_voter
func TestModelsPerStageOnAnyHostAndPerVoter(t *testing.T) {
	c := cfgOf(t, `[models]
triage = "claude-sonnet-5-5"
review = "claude-opus-5-5"
[hosts.helper]
model = "claude-sonnet-5-5"
[panel]
models = ["claude-opus-5-5", "claude-sonnet-5-5"]
`)
	tk := ticketOf(t, `{"id": "T0001", "history": []}`)
	for _, tc := range []struct{ stage, host, want string }{
		{"review", local, "claude-opus-5-5"},
		{"review", "helper", "claude-opus-5-5"},
		{"task", "helper", "claude-sonnet-5-5"},
		{"work", local, ""},
		{"work", "gone", ""},
	} {
		if got := Model(c, tk, tc.stage, tc.host); got != tc.want {
			t.Errorf("%s@%s: %q, want %q", tc.stage, tc.host, got, tc.want)
		}
	}
	var voters []string
	var hist []string
	for i := range 3 {
		tk := ticketOf(t, `{"id": "T0001", "history": [`+strings.Join(hist, ",")+`]}`)
		voters = append(voters, Model(c, tk, "vote", "helper"))
		hist = append(hist, fmt.Sprintf(`{"at": "2026-10-18T09:00:00+00:00", "event": "start", "detail": "vote t0001-v%d"}`, i+1))
	}
	if strings.Join(voters, ",") != "claude-opus-5-5,claude-sonnet-5-5,claude-opus-5-5" {
		t.Errorf("voters %q", voters)
	}
	// with no [panel] models a voter takes [models].vote like any stage
	c = cfgOf(t, "[models]\nvote = \"v\"\n")
	if got := Model(c, tk, "vote", local); got != "v" {
		t.Errorf("vote: %q", got)
	}
}

// Public review takes [models] public, else the security stage's model,
// else a helper's own.
func TestPublicReviewDefaultsToTheSecurityModel(t *testing.T) {
	tk := ticketOf(t, `{"id": "T0001", "history": []}`)
	for _, tc := range []struct{ config, host, want string }{
		{"[models]\npublic = \"p\"\nsecurity = \"s\"\n", local, "p"},
		{"[models]\nsecurity = \"s\"\n[hosts.helper]\nmodel = \"h\"\n", "helper", "s"},
		{"[models]\npublic = \"\"\nsecurity = \"s\"\n", local, "s"},
		{"[hosts.helper]\nmodel = \"h\"\n", "helper", "h"},
		{"[hosts.helper]\nmodel = \"h\"\n", local, ""},
	} {
		if got := Model(cfgOf(t, tc.config), tk, string(model.StagePublic), tc.host); got != tc.want {
			t.Errorf("%q@%s: %q, want %q", tc.config, tc.host, got, tc.want)
		}
	}
	// the fallback is public review's alone
	if got := Model(cfgOf(t, "[models]\nsecurity = \"s\"\n"), tk, "review", local); got != "" {
		t.Errorf("review took security's model: %q", got)
	}
}

// port: TF LimitTest.test_only_real_limit_messages_count
func TestOnlyRealLimitMessagesCount(t *testing.T) {
	for _, tc := range []struct {
		text string
		hit  bool
	}{
		{"working…\n⎿  Claude usage limit reached. Your limit will reset at 5pm", true},
		{"Error: Your credit balance is too low to access the Anthropic API.", true},
		{"5-hour limit reached ∙ resets 3am", true},
		{"● Edit(lib/rate_limit.rb)\n  def rate_limit_exceeded?; end", false},
		{"usage limit reached\n" + strings.Repeat("line\n", 30), false},
	} {
		if _, hit := HitLimit(tc.text, DefaultLimitLines); hit != tc.hit {
			t.Errorf("%q: %v", tc.text, hit)
		}
	}
}

// hit_limit over the golden corpus, with 15 and 40 lines (the Python goldens tool).
func TestHitLimitGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/py/hit_limit.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Rows []map[string]*string
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Rows) < 10 {
		t.Fatalf("%d rows", len(doc.Rows))
	}
	for _, row := range doc.Rows {
		in := *row["in"]
		for _, n := range []int{15, 40} {
			want := row[fmt.Sprint(n)]
			got, hit := HitLimit(in, n)
			switch {
			case want == nil && hit:
				t.Errorf("%q/%d: %q, want none", in, n, got)
			case want != nil && (!hit || got != *want):
				t.Errorf("%q/%d: %q %v, want %q", in, n, got, hit, *want)
			}
		}
	}
}

// The limit pattern over the shared regex corpus.
func TestLimitTextMatchesTheRegexGolden(t *testing.T) {
	b, err := os.ReadFile("../textx/testdata/py/regex.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Patterns []struct {
			Name  string
			Cases []struct {
				In  string
				Out json.RawMessage
			}
		}
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, p := range doc.Patterns {
		if p.Name != "limit_text" {
			continue
		}
		for _, c := range p.Cases {
			var out []*string
			if err := json.Unmarshal(c.Out, &out); err != nil {
				t.Fatal(err)
			}
			got, hit := limitSearch(c.In)
			if hit != (out != nil) || hit && got != *out[0] {
				t.Errorf("%q: %q %v, want %s", c.In, got, hit, c.Out)
			}
		}
		return
	}
	t.Fatal("limit_text not in the golden")
}

// RETRY_FOR_STAGE, and "ready" for anything else.
func TestRetryColumn(t *testing.T) {
	want := map[string]string{"triage": "triage", "work": "ready", "task": "ready", "design": "ready", "prospect": "ready",
		"manage": "ready", "review": "review", "security": "security", "vote": "voting", "odd": "ready", "": "ready"}
	for stage, col := range want {
		if got := RetryColumn(stage); got != col {
			t.Errorf("%q: %q, want %q", stage, got, col)
		}
	}
	if PauseFor != 30*time.Minute || QuietFor != 4*time.Minute {
		t.Error("pause or quiet")
	}
}

// ticket_timeout: the ticket's own timeout_minutes when set and not 0,
// else agent_timeout_minutes (45 by default).
func TestTimeout(t *testing.T) {
	c := cfgOf(t, "agent_timeout_minutes = 30\n")
	for js, want := range map[string]time.Duration{
		`{"id": "T0001"}`:                          30 * time.Minute,
		`{"id": "T0001", "timeout_minutes": 60}`:   60 * time.Minute,
		`{"id": "T0001", "timeout_minutes": 0}`:    30 * time.Minute,
		`{"id": "T0001", "timeout_minutes": null}`: 30 * time.Minute,
	} {
		if got := Timeout(c, ticketOf(t, js)); got != want {
			t.Errorf("%s: %v", js, got)
		}
	}
	if got := Timeout(cfgOf(t, ""), ticketOf(t, `{"id": "T0001"}`)); got != 45*time.Minute {
		t.Errorf("default: %v", got)
	}
}

// A helper whose kinds rule leaves out the project's kind is never
// picked, pinned or not; "" (no project) is taken anywhere.
func TestPickFollowsTheHostsKinds(t *testing.T) {
	c := cfgOf(t, `max_agents = 1
[hosts.hostx]
ssh = "factory@10.9.8.7"
prefer = ["review"]
not_kinds = ["go-module"]

[hosts.hosty]
ssh = "factory@10.9.8.8"
stages = ["review"]
kinds = ["crystal-shard"]
`)
	full := map[string]int{local: 1}
	pick := func(kind, pinned string) string {
		h, _ := Pick(c, "review", full, map[string]bool{}, pinned, true, kind)
		return h
	}
	for _, tc := range []struct{ kind, pinned, want string }{
		{"ruby-gem", "", "hostx"},
		{"go-module", "", ""},
		{"crystal-shard", "", "hostx"},
		{"", "", "hostx"},
		{"go-module", "hostx", ""},
		{"ruby-gem", "hosty", ""},
		{"crystal-shard", "hosty", "hosty"},
	} {
		if got := pick(tc.kind, tc.pinned); got != tc.want {
			t.Errorf("kind %q pinned %q: %q, want %q", tc.kind, tc.pinned, got, tc.want)
		}
	}
}
