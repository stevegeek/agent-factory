package onboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/paths"
)

const greenConfig = `[projects.gem]
kind = "ruby-gem"
path = "/p/gem"
github = "o/gem"
base = "main"

[projects.old]
language = "Crystal shard: x"
path = "/p/old"
github = "o/old"
base = "main"
install = "shards install"
test = "crystal spec"
lint = "true"

[projects.bare]
path = "/p/bare"
base = "main"
install = "bundle check"

[projects.odd]
kind = "no-such"
path = "/p/odd"
base = "main"

[projects.desk]
mode = "board"
`

// fakeReach is the App: reach and rules per repository.
type fakeReach struct {
	reach    map[string]bool
	reachErr error
	rules    []string
	rulesErr error
	asked    []string
}

func (f *fakeReach) CanReach(_ context.Context, repo string) (bool, error) {
	f.asked = append(f.asked, "reach "+repo)
	return f.reach[repo], f.reachErr
}

func (f *fakeReach) BranchRules(_ context.Context, repo, branch string) ([]string, error) {
	f.asked = append(f.asked, "rules "+repo+" "+branch)
	return f.rules, f.rulesErr
}

// green is a Deps under which project gem passes every check.
func green(t *testing.T) (Deps, *fakeReach) {
	t.Helper()
	cfg, err := config.Parse([]byte(greenConfig))
	if err != nil {
		t.Fatal(err)
	}
	ctxDir := t.TempDir()
	for _, key := range []string{"gem", "old", "bare", "odd"} {
		if err := os.MkdirAll(filepath.Join(ctxDir, key), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ctxDir, key, "INDEX.md"), []byte("# x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gh := &fakeReach{reach: map[string]bool{"o/gem": true, "o/old": true}, rules: []string{"deletion", "pull_request"}}
	return Deps{
		Config: cfg,
		Paths:  paths.Paths{Context: ctxDir},
		Head: func(_ context.Context, p config.Project) (string, error) {
			return "0123456789abcdef0123", nil
		},
		GitHub: gh,
		Checks: func(_ context.Context, p config.Project, ref string) (bool, string, error) {
			return true, "ok\n", nil
		},
	}, gh
}

func check(t *testing.T, d Deps, key string) Report {
	t.Helper()
	r, err := Check(context.Background(), d, key)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// item is the report's item for check name.
func item(t *testing.T, r Report, name string) Item {
	t.Helper()
	for _, it := range r.Items {
		if it.Check == name {
			return it
		}
	}
	t.Fatalf("no %s item in %+v", name, r.Items)
	return Item{}
}

func statuses(r Report) string {
	var b []string
	for _, it := range r.Items {
		b = append(b, it.Check+"="+it.Status)
	}
	return strings.Join(b, " ")
}

func TestOnboardAllGreen(t *testing.T) {
	d, gh := green(t)
	r := check(t, d, "gem")
	if got := statuses(r); got != "kind=ok commands=ok checkout=ok context=ok github=ok rules=ok main=ok" {
		t.Errorf("statuses %s", got)
	}
	if !r.OK() || r.Project != "gem" || r.Kind != "ruby-gem" || r.Inferred {
		t.Errorf("report %+v", r)
	}
	if got := strings.Join(gh.asked, "; "); got != "reach o/gem; rules o/gem main" {
		t.Errorf("asked GitHub %s", got)
	}
}

func TestOnboardInferredKindWarns(t *testing.T) {
	d, _ := green(t)
	r := check(t, d, "old")
	it := item(t, r, "kind")
	if it.Status != StatusWarn || it.Detail != `inferred from language; set kind = "crystal-shard"` {
		t.Errorf("kind %+v", it)
	}
	if !r.Inferred || r.Kind != "crystal-shard" || !r.OK() {
		t.Errorf("report %+v", r)
	}
}

// An inferred kind is the built-in, so with a [kinds.<name>] of that name
// in config, setting the kind as advised would change what the project
// takes: the warning says so.
func TestOnboardInferredKindNotesAnOverride(t *testing.T) {
	d, _ := green(t)
	cfg, err := config.Parse([]byte("[kinds.crystal-shard]\nlint = \"true\"\n\n" + greenConfig))
	if err != nil {
		t.Fatal(err)
	}
	d.Config = cfg
	it := item(t, check(t, d, "old"), "kind")
	want := `inferred from language; set kind = "crystal-shard" (note: [kinds.crystal-shard] in config overrides the built-in the project uses now)`
	if it.Status != StatusWarn || it.Detail != want {
		t.Errorf("kind %+v", it)
	}
}

func TestOnboardUnknownKindFails(t *testing.T) {
	d, _ := green(t)
	r := check(t, d, "odd")
	if it := item(t, r, "kind"); it.Status != StatusFail || !strings.Contains(it.Detail, `"no-such"`) {
		t.Errorf("kind %+v", it)
	}
	if r.OK() || r.Kind != "no-such" {
		t.Errorf("report %+v", r)
	}
}

func TestOnboardMissingCommandFails(t *testing.T) {
	d, _ := green(t)
	r := check(t, d, "bare")
	if it := item(t, r, "commands"); it.Status != StatusFail || it.Detail != "missing: test, lint" {
		t.Errorf("commands %+v", it)
	}
	if r.OK() {
		t.Error("OK with commands missing")
	}
}

func TestOnboardNoCheckoutFails(t *testing.T) {
	d, _ := green(t)
	ran := false
	d.Head = func(context.Context, config.Project) (string, error) { return "", errors.New("no such branch") }
	d.Checks = func(context.Context, config.Project, string) (bool, string, error) { ran = true; return true, "", nil }
	r := check(t, d, "gem")
	if it := item(t, r, "checkout"); it.Status != StatusFail || !strings.Contains(it.Detail, "no such branch") {
		t.Errorf("checkout %+v", it)
	}
	if it := item(t, r, "main"); it.Status != StatusSkip || ran {
		t.Errorf("main %+v (ran %v)", it, ran)
	}
	if r.OK() {
		t.Error("OK with no checkout")
	}
}

// A checkout that reads an empty head is a failed checkout, not "main
// at ".
func TestOnboardEmptyHeadFails(t *testing.T) {
	d, _ := green(t)
	d.Head = func(context.Context, config.Project) (string, error) { return "", nil }
	r := check(t, d, "gem")
	if it := item(t, r, "checkout"); it.Status != StatusFail || it.Detail != "main in /p/gem: no head" {
		t.Errorf("checkout %+v", it)
	}
	if it := item(t, r, "main"); it.Status != StatusSkip {
		t.Errorf("main %+v", it)
	}
}

func TestOnboardNoContextWarns(t *testing.T) {
	d, _ := green(t)
	if err := os.Remove(filepath.Join(d.Paths.ProjectContext("gem"), "INDEX.md")); err != nil {
		t.Fatal(err)
	}
	r := check(t, d, "gem")
	if it := item(t, r, "context"); it.Status != StatusWarn || !strings.Contains(it.Detail, "INDEX.md") {
		t.Errorf("context %+v", it)
	}
	if !r.OK() {
		t.Error("a warning failed the report")
	}
}

func TestOnboardNoGitHubKeyWarns(t *testing.T) {
	d, gh := green(t)
	d.Config.Projects[0].GitHub = ""
	r := check(t, d, "gem")
	if it := item(t, r, "github"); it.Status != StatusWarn || it.Detail != "no github key: no PRs or sweep" {
		t.Errorf("github %+v", it)
	}
	if it := item(t, r, "rules"); it.Status != StatusSkip {
		t.Errorf("rules %+v", it)
	}
	if len(gh.asked) != 0 || !r.OK() {
		t.Errorf("asked %q, ok %v", gh.asked, r.OK())
	}
}

func TestOnboardAppCannotReachFails(t *testing.T) {
	d, gh := green(t)
	gh.reach["o/gem"] = false
	r := check(t, d, "gem")
	if it := item(t, r, "github"); it.Status != StatusFail || it.Detail != "grant the App access to o/gem" {
		t.Errorf("github %+v", it)
	}
	if it := item(t, r, "rules"); it.Status != StatusSkip {
		t.Errorf("rules %+v", it)
	}
	if r.OK() {
		t.Error("OK when the App cannot reach the repo")
	}
	d, gh = green(t)
	gh.reachErr = errors.New("HTTP Error 500: Internal Server Error")
	if it := item(t, check(t, d, "gem"), "github"); it.Status != StatusFail || !strings.Contains(it.Detail, "500") {
		t.Errorf("github on an error %+v", it)
	}
}

func TestOnboardRulesUnreadableWarn(t *testing.T) {
	for _, code := range []int{403, 404} {
		d, gh := green(t)
		gh.rules, gh.rulesErr = nil, &github.HTTPError{Code: code, Reason: "x"}
		r := check(t, d, "gem")
		it := item(t, r, "rules")
		if it.Status != StatusWarn || it.Detail != "cannot read the branch's rules: a private repo without GitHub Pro has none" {
			t.Errorf("%d: rules %+v", code, it)
		}
		if !r.OK() {
			t.Errorf("%d failed the report", code)
		}
	}
}

func TestOnboardNoPullRequestRuleWarns(t *testing.T) {
	d, gh := green(t)
	gh.rules = []string{"deletion"}
	r := check(t, d, "gem")
	if it := item(t, r, "rules"); it.Status != StatusWarn || !strings.Contains(it.Detail, "main") {
		t.Errorf("rules %+v", it)
	}
	if !r.OK() {
		t.Error("a warning failed the report")
	}
}

func TestOnboardRedMainFails(t *testing.T) {
	d, _ := green(t)
	var gotRef string
	tail := strings.Repeat("é", 2000) + "FAILED"
	d.Checks = func(_ context.Context, _ config.Project, ref string) (bool, string, error) {
		gotRef = ref
		return false, tail, nil
	}
	r := check(t, d, "gem")
	it := item(t, r, "main")
	if it.Status != StatusFail || len([]rune(it.Detail)) != 1500 || !strings.HasSuffix(it.Detail, "FAILED") {
		t.Errorf("main %s, %d code points", it.Status, len([]rune(it.Detail)))
	}
	if gotRef != "0123456789abcdef0123" || r.OK() {
		t.Errorf("ref %q, ok %v", gotRef, r.OK())
	}
}

func TestOnboardSkipsWithoutDeps(t *testing.T) {
	d, _ := green(t)
	d.GitHub, d.Checks = nil, nil
	r := check(t, d, "gem")
	if got := statuses(r); got != "kind=ok commands=ok checkout=ok context=ok github=skip rules=skip main=skip" {
		t.Errorf("statuses %s", got)
	}
	for _, name := range []string{"github", "rules", "main"} {
		if item(t, r, name).Detail == "" {
			t.Errorf("%s skipped with no reason", name)
		}
	}
	if !r.OK() {
		t.Error("skips failed the report")
	}
}

func TestOnboardJSON(t *testing.T) {
	r := Report{Project: "gem", Kind: "ruby-gem", Items: []Item{
		{Check: "kind", Status: StatusOK, Detail: "ruby-gem"},
		{Check: "main", Status: StatusFail, Detail: "a \"quoted\" tail\n"},
	}}
	want := `{"project": "gem", "kind": "ruby-gem", "inferred": false, "ok": false, "items": [` +
		`{"check": "kind", "status": "ok", "detail": "ruby-gem"}, ` +
		`{"check": "main", "status": "fail", "detail": "a \"quoted\" tail\n"}]}`
	if got := string(r.JSON()); got != want {
		t.Errorf("JSON\n got %s\nwant %s", got, want)
	}
	if got := string(Report{Project: "x"}.JSON()); got != `{"project": "x", "kind": "", "inferred": false, "ok": true, "items": []}` {
		t.Errorf("empty JSON %s", got)
	}
}

func TestOnboardBoardProjectRefused(t *testing.T) {
	d, _ := green(t)
	if _, err := Check(context.Background(), d, "desk"); err == nil || !strings.Contains(err.Error(), "board") {
		t.Errorf("board project: %v", err)
	}
	if _, err := Check(context.Background(), d, "nope"); err == nil {
		t.Error("an unknown project was checked")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Check(ctx, d, "gem"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}
