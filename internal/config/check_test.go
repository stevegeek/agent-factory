package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fakeTokens stands in for the token store (S5 owns it): the records, or
// the error message load_tokens raised in the golden's case.
type fakeTokens struct {
	records []TokenRecord
	err     error
}

func (f fakeTokens) Records() ([]TokenRecord, error) { return f.records, f.err }

// tokensFrom reads a golden's api-tokens.json as load_tokens would for the
// cases the goldens cover: version 1 records, a store of an unknown version,
// or text that is not JSON.
func tokensFrom(t *testing.T, path string) TokenRecords {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fakeTokens{}
	}
	if err != nil {
		t.Fatal(err)
	}
	var store struct {
		Version int `json:"version"`
		Tokens  []struct {
			Name     string   `json:"name"`
			Projects []string `json:"projects"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(b, &store); err != nil {
		return fakeTokens{err: errors.New("Expecting value: line 1 column 1 (char 0)")}
	}
	if store.Version != 1 {
		return fakeTokens{err: errors.New("<root>/api-tokens.json is not a token store this factory knows")}
	}
	var rs []TokenRecord
	for _, r := range store.Tokens {
		rs = append(rs, TokenRecord{Name: r.Name, Projects: r.Projects})
	}
	return fakeTokens{records: rs}
}

// newWarningPrefixes are the warnings the port adds to config_problems
// (§3.4, quirk 6, D15): they come after Python's, so Python's list is a
// prefix of the port's.
var newWarning = []string{
	": not a key the factory knows",
	"has no install",
	"is not a whole number", "is not a string", "is not a list", "is not a number", "is not a table",
	"is skipped",
	"app_slug",
}

func checkNewWarnings(t *testing.T, name string, py, got []string) {
	t.Helper()
	if len(got) < len(py) || (len(py) > 0 && !reflect.DeepEqual(got[:len(py)], py)) {
		t.Errorf("%s: warnings\n got %q\nwant %q first", name, got, py)
		return
	}
	for _, w := range got[len(py):] {
		known := false
		for _, s := range newWarning {
			known = known || strings.Contains(w, s)
		}
		if !known {
			t.Errorf("%s: a warning Python does not give and the port does not mean to add: %q", name, w)
		}
	}
}

// The goldens: config_problems over the check cases (tools/goldens.py
// CHECK_CASES), problems exactly, warnings with the port's additions after
// Python's.
func TestCheckGoldens(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "py", "check", "*.toml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no check goldens: %v", err)
	}
	for _, f := range files {
		base := strings.TrimSuffix(f, ".toml")
		var want struct {
			Problems []string `json:"problems"`
			Warnings []string `json:"warnings"`
		}
		b, err := os.ReadFile(base + ".json")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &want); err != nil {
			t.Fatal(err)
		}
		text, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		c := mustParse(t, string(text))
		problems, warnings := c.Problems(tokensFrom(t, base+".tokens.json"))
		if !reflect.DeepEqual(problems, want.Problems) && !(len(problems) == 0 && len(want.Problems) == 0) {
			t.Errorf("%s: problems\n got %q\nwant %q", base, problems, want.Problems)
		}
		checkNewWarnings(t, base, want.Warnings, warnings)
	}
}

// port: TT ModeTest.test_check_finds_bad_modes_people_and_tokens_for_gone_projects
func TestCheckFindsBadModesPeopleAndTokensForGoneProjects(t *testing.T) {
	c := mustParse(t, `
[board]
people = ["alice", "factory"]

[projects.x]
mode = "kanban"

[projects.y]
mode = "board"
github = "o/y"
`)
	problems, warnings := c.Problems(fakeTokens{records: []TokenRecord{{Name: "example-agent", Projects: []string{"other"}}}})
	all := strings.Join(problems, "\n")
	for _, want := range []string{"kanban", "'factory'", "example-agent names other"} {
		if !strings.Contains(all, want) {
			t.Errorf("problems lack %q: %q", want, problems)
		}
	}
	if !reflect.DeepEqual(warnings, []string{"[projects.y] is a board project; github are not used"}) {
		t.Errorf("warnings = %q", warnings)
	}
	// and a clean TT config passes the check
	ok := mustParse(t, ticketsConfig)
	if problems, _ := ok.Problems(fakeTokens{}); len(problems) != 0 {
		t.Errorf("the TT config has problems: %q", problems)
	}
}

// port: TL BoardApartTest.test_a_recipe_never_files_on_a_board_project
// (the check part; the add refusals are cli/ops tests)
func TestCheckWarnsOfAWatchOnABoardProject(t *testing.T) {
	c := mustParse(t, ticketsConfig+`
[[recipes]]
name = "scout"
kind = "prospect"
title = "Scout"
body = "Look around."

[[watches]]
name = "w"
project = "sandbox"
command = ["true"]
title = "t"
body = "b"
`)
	_, warnings := c.Problems(nil)
	found := false
	for _, w := range warnings {
		found = found || strings.Contains(w, "[[watches]] w names sandbox")
	}
	if !found {
		t.Errorf("warnings = %q", warnings)
	}
}

// §13.2 row F: a schedule, recipe or watch naming no project still runs as
// in Python, and check says so; the factory's own project is no warning.
func TestCheckWarnsOfAnUnknownProject(t *testing.T) {
	c := mustParse(t, `
[projects.gem]
path = "/x"

[[schedules]]
name = "s"
projects = ["gem", "gone", "factory"]
every_days = 1
title = "t"
body = "b"

[[watches]]
name = "w"
project = "nowhere"
command = ["true"]
title = "t"
body = "b"
`)
	_, warnings := c.Problems(nil)
	var got []string
	for _, w := range warnings {
		if strings.Contains(w, "which is not a project") {
			got = append(got, w)
		}
	}
	want := []string{"[[watches]] w names nowhere, which is not a project", "[[schedules]] s names gone, which is not a project"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("warnings %q", warnings)
	}
}

// The people rules one by one, with Python's wording.
func TestCheckPeople(t *testing.T) {
	cases := map[string][]string{
		"[board]\npeople = []\n":                                    {"[board] people: a list of names, the maintainer first"},
		"[board]\npeople = 3\n":                                     {"[board] people: a list of names, the maintainer first"},
		"[board]\npeople = [\"maintainer\", \"bob\"]\n":             nil,
		"[board]\npeople = [\"me\"]\n":                              {"[board] people: 'me' is not a name a person can have here"},
		"[board]\npeople = [\"a\", \"a\\n\"]\n":                     {"[board] people: 'a\\n' is not a name a person can have here"},
		"[board]\npeople = [\"" + strings.Repeat("a", 33) + "\"]\n": {"[board] people: '" + strings.Repeat("a", 33) + "' is not a name a person can have here"},
		"[board]\npeople = [\"x\", 1, \"1\"]\n": {
			"[board] people: 1 is not a name a person can have here",
			"[board] people: '1' is not a name a person can have here",
			"[board] people: a name is there twice",
		},
		"[board]\npeople = [\"é\"]\n": {"[board] people: 'é' is not a name a person can have here"},
	}
	for text, want := range cases {
		problems, _ := mustParse(t, text).Problems(nil)
		if !reflect.DeepEqual(problems, want) && !(len(problems) == 0 && len(want) == 0) {
			t.Errorf("%q: problems = %q, want %q", text, problems, want)
		}
	}
}

func TestCheckTokens(t *testing.T) {
	c := mustParse(t, "[board]\npeople = [\"alice\"]\n[projects.a]\n")
	problems, _ := c.Problems(fakeTokens{records: []TokenRecord{
		{Name: "alice", Projects: []string{"a"}},
		{Name: "bot", Projects: []string{"a", "gone", "also-gone"}},
		{Name: "ok"},
	}})
	want := []string{"token alice has a person's name", "token bot names gone, also-gone, not in config"}
	if !reflect.DeepEqual(problems, want) {
		t.Errorf("problems = %q", problems)
	}
	problems, _ = c.Problems(fakeTokens{err: errors.New("the store is broken")})
	if !reflect.DeepEqual(problems, []string{"the store is broken"}) {
		t.Errorf("an unreadable store: %q", problems)
	}
}

// [hosts.<name>] takes an optional kind (§2.4.6): "jobs" when absent, the
// only kind there is; any other is a problem, and kind is a known key.
func TestHostKind(t *testing.T) {
	c := mustParse(t, "[hosts.a]\nssh = \"a\"\n\n[hosts.b]\nssh = \"b\"\nkind = \"jobs\"\n\n[hosts.c]\nssh = \"c\"\nkind = \"lever\"\n")
	var kinds []string
	for _, h := range c.Hosts {
		kinds = append(kinds, h.Name+"="+h.Kind)
	}
	if !reflect.DeepEqual(kinds, []string{"a=jobs", "b=jobs", "c=lever"}) {
		t.Errorf("kinds = %q", kinds)
	}
	problems, warnings := c.Problems(nil)
	if !reflect.DeepEqual(problems, []string{`[hosts.c] kind = "lever": the only host kind is "jobs"`}) {
		t.Errorf("problems = %q", problems)
	}
	for _, w := range warnings {
		if strings.Contains(w, "kind") {
			t.Errorf("kind is reported as unknown: %q", w)
		}
	}
}

// [hosts.<name>] transport: "ssh" (factory-remote, the default) or "pull"
// (a mailbox the host claims jobs from). A pull host has no ssh, and its
// claim and beat windows default to 30 and 10 minutes.
func TestHostTransport(t *testing.T) {
	c := mustParse(t, "[hosts.a]\nssh = \"a\"\n\n[hosts.desktop]\ntransport = \"pull\"\nclaim_minutes = 20\n\n"+
		"[hosts.c]\nssh = \"c\"\ntransport = \"carrier-pigeon\"\n\n[hosts.d]\ntransport = \"pull\"\nbeat_minutes = 0\n\n[hosts.e]\n")
	var got []string
	for _, h := range c.Hosts {
		got = append(got, fmt.Sprintf("%s=%s/%d/%d", h.Name, h.Transport, h.ClaimMinutes, h.BeatMinutes))
	}
	if want := []string{"a=ssh/30/10", "desktop=pull/20/10", "c=carrier-pigeon/30/10", "d=pull/30/0", "e=ssh/30/10"}; !reflect.DeepEqual(got, want) {
		t.Errorf("hosts = %q", got)
	}
	if !c.Hosts[1].Pull() || c.Hosts[0].Pull() {
		t.Error("Pull")
	}
	problems, warnings := c.Problems(nil)
	want := []string{
		`[hosts.c] transport = "carrier-pigeon": a host is reached by "ssh" or "pull"`,
		`[hosts.d] beat_minutes must be at least 1`,
		`[hosts.e] has no ssh destination (or transport = "pull")`,
	}
	if !reflect.DeepEqual(problems, want) {
		t.Errorf("problems = %q", problems)
	}
	for _, w := range warnings {
		if strings.Contains(w, "transport") || strings.Contains(w, "minutes") {
			t.Errorf("a host key is reported as unknown: %q", w)
		}
	}
}

func TestCheckBoardProjectKeys(t *testing.T) {
	c := mustParse(t, "[projects.b]\nmode = \"board\"\nlanguage = \"x\"\npath = \"/p\"\nbase = \"main\"\ninstall = \"i\"\nlint = \"l\"\nnotes = \"fine\"\n")
	_, warnings := c.Problems(nil)
	if len(warnings) == 0 || warnings[0] != "[projects.b] is a board project; path, base, install, lint, language are not used" {
		t.Errorf("warnings = %q", warnings)
	}
}

// Jobs naming board projects: a schedule's and a recipe's projects list,
// a watch's project; "list(job.get('projects') or [])" reads a string as
// its characters.
func TestCheckJobsOnBoardProjects(t *testing.T) {
	c := mustParse(t, `
[projects.b]
mode = "board"

[[schedules]]
name = "s"
every_days = 1
title = "t"
body = "b"
projects = "b"

[[recipes]]
title = "t"
body = "b"
projects = ["b", "b"]
`)
	_, warnings := c.Problems(nil)
	want := []string{
		"[[schedules]] s names b, a board project: the pipeline never works on one",
		"[[recipes]] None names b, a board project: the pipeline never works on one",
		"[[recipes]] None names b, a board project: the pipeline never works on one",
	}
	checkNewWarnings(t, "jobs", want, warnings)
}

// The port's own warnings: a factory project missing a gate command
// (quirk 6), and GitHub with no app_slug (D15).
func TestCheckNewWarnings(t *testing.T) {
	c := mustParse(t, "[github]\ninstallation_id = 1\n[projects.gem]\npath = \"/g\"\ninstall = \"i\"\n[projects.b]\nmode = \"board\"\n")
	_, warnings := c.Problems(nil)
	want := []string{
		"[projects.gem] has no test, lint: the gate refuses to run its checks without them",
		"[github] app_slug is not set: gate-workflow needs the App's slug",
	}
	if !reflect.DeepEqual(warnings, want) {
		t.Errorf("warnings = %q", warnings)
	}
}

// [remote] agent, auth and cpus, and each helper's [remote.<name>] over
// it, are the helper's keys: known here, and checked as the helper does.
func TestRemoteHosts(t *testing.T) {
	c := mustParse(t, "[remote]\ncpus = 2\ndeps = false\nenv = { A = \"1\" }\n\n[remote.helperb]\ndeps = true\nbuild = true\nbuild_memory = \"2g\"\nagent = \"claude\"\nimage = \"localhost/factory-agent:scion\"\ncpus = 4\n"+
		"memory = \"3g\"\nmax_jobs = 2\nmounts = []\nenv = { B = \"2\" }\n\n[remote.helperc]\nagent = \"claude\"\nauth = \"api-key\"\nimage = \"localhost/factory-agent:scion\"\n\n"+
		"[remote.bad]\nagent = \"codex\"\n\n[remote.pi]\nauth = \"subscription\"\n\n[remote.odd]\nauth = \"oauth\"\ncolour = \"red\"\n\n"+
		"[remote.nopi]\nagent = \"claude\"\nimage = \"localhost/factory-pi:dev\"\n\n[remote.HelperB]\nimage = \"x\"\n\n[remote.memory]\nagent = \"pi\"\n")
	var got []string
	for _, h := range c.Remote.Hosts {
		got = append(got, h.Name+"="+h.Agent+"/"+h.Auth)
	}
	if want := []string{"helperb=claude/", "helperc=claude/api-key", "bad=codex/", "pi=/subscription", "odd=/oauth", "nopi=claude/", "HelperB=/", "memory=pi/"}; !reflect.DeepEqual(got, want) {
		t.Errorf("hosts = %q", got)
	}
	problems, warnings := c.Problems(nil)
	want := []string{
		`[remote.bad] agent = "codex": it is "pi" or "claude"`,
		`[remote.pi] auth = "subscription" is for Claude Code only: Pi needs an API key`,
		`[remote.odd] auth = "oauth": it is "subscription" or "api-key"`,
		`[remote.nopi] agent = "claude" needs image = the Claude Code image, not the Pi image localhost/factory-pi:dev`,
		`[remote.HelperB] never applies: a helper reads the table named by its lowercased short hostname`,
		`[remote.memory] is not a helper's table: memory is a [remote] key`,
	}
	if !reflect.DeepEqual(problems, want) {
		t.Errorf("problems = %q", problems)
	}
	if !reflect.DeepEqual(c.Unknown, []string{"remote.odd.colour"}) {
		t.Errorf("unknown = %q (warnings %q)", c.Unknown, warnings)
	}
}

// [build] host names an ssh helper in [hosts].
func TestBuildHost(t *testing.T) {
	hosts := "[hosts.helpera]\nssh = \"factory@helpera\"\n\n[hosts.desk]\ntransport = \"pull\"\n"
	for build, want := range map[string]string{
		"helpera": "",
		"nope":    `[build] host = "nope" is not in [hosts]`,
		"desk":    `[build] host = "desk" is a pull host: only an ssh helper builds`,
	} {
		c := mustParse(t, "[build]\nhost = \""+build+"\"\n\n"+hosts)
		if c.Build.Host != build {
			t.Errorf("%s: host %q", build, c.Build.Host)
		}
		problems, warnings := c.Problems(nil)
		got := ""
		for _, p := range problems {
			if strings.HasPrefix(p, "[build]") {
				got = p
			}
		}
		if got != want || slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "build") }) {
			t.Errorf("%s: %q %q", build, problems, warnings)
		}
	}
}

// [deploy] names core and config as owner/repo and needs the App and a
// builder; an absent [deploy] is off.
func TestDeploy(t *testing.T) {
	if c := mustParse(t, ""); c.Deploy.On() || c.Deploy.ConfigDir != "config" {
		t.Errorf("absent [deploy] = %+v, on %v", c.Deploy, c.Deploy.On())
	}
	ready := "[github]\ninstallation_id = 7\n\n[build]\nhost = \"helpera\"\n\n[hosts.helpera]\nssh = \"factory@helpera\"\n\n"
	c := mustParse(t, ready+"[deploy]\ncore = \"example/agent-factory\"\nconfig = \"example/factory-config\"\nconfig_dir = \"conf/live\"\n")
	if !c.Deploy.On() || c.Deploy != (Deploy{Core: "example/agent-factory", Config: "example/factory-config", ConfigDir: "conf/live"}) {
		t.Errorf("[deploy] = %+v", c.Deploy)
	}
	problems, warnings := c.Problems(nil)
	if len(problems) > 0 || slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "deploy") }) {
		t.Errorf("a good [deploy]: %q %q", problems, warnings)
	}

	deployProblems := func(text string) []string {
		var out []string
		ps, _ := mustParse(t, text).Problems(nil)
		for _, p := range ps {
			if strings.HasPrefix(p, "[deploy]") {
				out = append(out, p)
			}
		}
		return out
	}
	got := deployProblems(ready + "[deploy]\ncore = \"agent-factory\"\nconfig = \"a/b/c\"\nconfig_dir = \"../up\"\n")
	want := []string{
		`[deploy] core = "agent-factory" is not owner/repo`,
		`[deploy] config = "a/b/c" is not owner/repo`,
		`[deploy] config_dir = "../up" must be a relative path inside the config repo`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bad names: %q", got)
	}
	// what gitx.Mirror refuses: a leading dash or dot in either part
	got = deployProblems(ready + "[deploy]\ncore = \"-u/x\"\nconfig = \"a/-b\"\n")
	want = []string{`[deploy] core = "-u/x" is not owner/repo`, `[deploy] config = "a/-b" is not owner/repo`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("leading dashes: %q", got)
	}
	got = deployProblems(ready + "[deploy]\ncore = \".u/x\"\nconfig = \"a/.b\"\n")
	want = []string{`[deploy] core = ".u/x" is not owner/repo`, `[deploy] config = "a/.b" is not owner/repo`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("leading dots: %q", got)
	}
	if got := deployProblems(ready + "[deploy]\ncore = \"a/b\"\nconfig = \"a/c\"\nconfig_dir = \"/etc\"\n"); !reflect.DeepEqual(got, []string{`[deploy] config_dir = "/etc" must be a relative path inside the config repo`}) {
		t.Errorf("absolute config_dir: %q", got)
	}
	got = deployProblems("[deploy]\ncore = \"a/b\"\nconfig = \"a/c\"\n")
	want = []string{
		"[deploy] needs [github] installation_id: the config repo is fetched with an App token",
		"[deploy] needs [build] host: a self-deploy builds on a helper",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("no App, no builder: %q", got)
	}
	if got := deployProblems("[deploy]\nconfig_dir = \"x\"\n"); !reflect.DeepEqual(got, []string{`[deploy] core = "" is not owner/repo`, `[deploy] config = "" is not owner/repo`, "[deploy] needs [github] installation_id: the config repo is fetched with an App token", "[deploy] needs [build] host: a self-deploy builds on a helper"}) {
		t.Errorf("a [deploy] without core: %q", got)
	}
}

// P6: [backup] name is used as it is in the archive's name, so it must
// already be a slug, and not one of the host part's fallbacks.
func TestBackupNameMustBeASlug(t *testing.T) {
	for name, bad := range map[string]bool{
		"master": false, "hub": false, "box-2": false,
		"Master": true, "the master": true, "a_b": true, "host": true, "change": true, "-x": true,
	} {
		c := mustParse(t, "[backup]\nname = \""+name+"\"\n")
		problems, _ := c.Problems(nil)
		got := slices.ContainsFunc(problems, func(p string) bool { return strings.HasPrefix(p, "[backup] name") })
		if got != bad {
			t.Errorf("%q: problems %q", name, problems)
		}
	}
	if problems, _ := mustParse(t, "").Problems(nil); slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "[backup]") }) {
		t.Errorf("no name: %q", problems)
	}
}

// P6: [build] host = "self" needs no [hosts] entry; this host's own
// factory-remote builds, so [remote.<this host>] (or [remote]) has
// build = true. That is HostProblems' check, for the host it names.
func TestBuildHostSelfPassesCheck(t *testing.T) {
	for _, remote := range []string{
		"[remote.box]\nbuild = true\n",
		"[remote]\nbuild = true\n",
	} {
		c := mustParse(t, "[build]\nhost = \"self\"\n\n"+remote)
		if c.Build.Host != BuildSelf {
			t.Errorf("host %q", c.Build.Host)
		}
		problems, _ := c.Problems(nil)
		problems = append(problems, c.HostProblems("box")...)
		if slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "[build]") }) {
			t.Errorf("%s: %q", remote, problems)
		}
	}
}

func TestBuildHostSelfNeedsRemoteBuild(t *testing.T) {
	want := `[build] host = "self" needs build = true in [remote.box] (this host's factory-remote builds)`
	for _, remote := range []string{
		"",
		"[remote.some-other-host]\nbuild = true\n",
		"[remote]\nbuild = true\n\n[remote.box]\nbuild = false\n",
	} {
		c := mustParse(t, "[build]\nhost = \"self\"\n\n"+remote)
		if problems := c.HostProblems("box"); !slices.Contains(problems, want) {
			t.Errorf("%q: %q", remote, problems)
		}
	}
	// Problems is the config alone: a restored archive's "self" checks
	// on any machine (backup verify, adopt)
	c := mustParse(t, "[build]\nhost = \"self\"\n\n[remote.some-other-host]\nbuild = true\n")
	if problems, _ := c.Problems(nil); slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "[build]") }) {
		t.Errorf("Problems checks this host: %q", problems)
	}
	if problems := mustParse(t, "[build]\nhost = \"helper\"\n").HostProblems("box"); len(problems) != 0 {
		t.Errorf("not self: %q", problems)
	}
	// a [hosts] entry cannot take the name
	c = mustParse(t, "[hosts.self]\nssh = \"factory@x\"\n")
	if problems, _ := c.Problems(nil); !slices.Contains(problems, `[hosts.self]: "self" is [build] host's name for this host`) {
		t.Errorf("[hosts.self]: %q", problems)
	}
}
