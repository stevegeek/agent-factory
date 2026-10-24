package render

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	agentfactory "github.com/stevegeek/agent-factory"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func parse(t *testing.T, text string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

func exampleConfig(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "config", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// values(cfg) over the Python golden: the values in order, or the message
// render exits with.
func TestValuesMatchPython(t *testing.T) {
	var golden struct {
		Rows []struct {
			Name   string             `json:"name"`
			TOML   *string            `json:"toml"`
			Values *orderedStringJSON `json:"values"`
			Exit   *string            `json:"exit"`
		} `json:"rows"`
	}
	readJSON(t, filepath.Join("testdata", "py", "values.json"), &golden)
	if len(golden.Rows) == 0 {
		t.Fatal("no rows")
	}
	for _, row := range golden.Rows {
		t.Run(row.Name, func(t *testing.T) {
			text := string(exampleConfig(t))
			if row.TOML != nil {
				text = *row.TOML
			}
			got, err := Values(parse(t, text))
			if row.Exit != nil {
				var p Problem
				if !errors.As(err, &p) {
					t.Fatalf("err = %v (%T), want the problem %q", err, err, *row.Exit)
				}
				if err.Error() != *row.Exit {
					t.Errorf("message\n got %q\nwant %q", err.Error(), *row.Exit)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(row.Values.list) {
				t.Errorf("values\n got %v\nwant %v", got, row.Values.list)
			}
		})
	}
}

// orderedStringJSON decodes a JSON object of strings keeping key order.
type orderedStringJSON struct{ list ValueList }

func (o *orderedStringJSON) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	if _, err := dec.Token(); err != nil {
		return err
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return err
		}
		var v string
		if err := dec.Decode(&v); err != nil {
			return err
		}
		o.list = append(o.list, Value{Name: k.(string), Text: v})
	}
	_, err := dec.Token()
	return err
}

// port: TT ModeTest.test_render_gives_foreman_only_factory_projects
func TestForemanGetsOnlyFactoryProjects(t *testing.T) {
	cfg := parse(t, `
[identity]
git_name = "n"
git_email = "e"
pr_footer = "f"
[host]
factory_uid = 1000
[board]
people = ["alice", "bob"]
[projects.sandbox]
mode = "board"
[projects.other]
mode = "board"
[projects.fac]
path = "/nonexistent"
github = "o/fac"
`)
	vs, err := Values(cfg)
	if err != nil {
		t.Fatal(err)
	}
	flags, _ := vs.Get("foreman_flags")
	if !strings.Contains(flags, "--project fac") || strings.Contains(flags, "sandbox") || strings.Contains(flags, "other") {
		t.Errorf("foreman_flags = %q", flags)
	}
}

func TestValuesEdges(t *testing.T) {
	base := "[identity]\ngit_name = \"B\"\ngit_email = \"b@x\"\npr_footer = \"f\"\n[host]\nfactory_uid = 7\n"
	cases := []struct {
		name, extra string
		want        map[string]string
		problem     string
	}{
		{"uid false is str()", "", nil, ""},
		{"empty home is missing", "factory_home = \"\"\n", nil, "config.toml is missing: factory_home (see [identity] and [host])"},
		{"home slashes stripped", "factory_home = \"/srv/f///\"\n", map[string]string{"factory_root": "/srv/f/factory", "factory_home": "/srv/f///"}, ""},
		{"empty listen is missing", "[foreman]\nlisten = \"\"\n", nil, "config.toml is missing: foreman_listen (see [identity] and [host])"},
		{"users without trust", "[foreman]\ntailscale_users = [\"a@b\"]\n", map[string]string{"foreman_flags": "--tailscale-user a@b"}, ""},
		{"login with a dollar", "[foreman]\ntailscale_users = [\"a$b\"]\n", nil, "[foreman] tailscale_users: not a login: 'a$b'"},
		{"login with a quote", "[foreman]\ntailscale_users = [\"a'b\"]\n", nil, `[foreman] tailscale_users: not a login: "a'b"`},
		{"login with NBSP", "[foreman]\ntailscale_users = [\"a b\"]\n", nil, `[foreman] tailscale_users: not a login: 'a\xa0b'`},
		{"host with a newline", "[foreman]\nhosts = [\"a\\n\"]\n", nil, `[foreman] hosts: not a host name: 'a\n'`},
		{"unicode project", "[projects.\"gém\"]\npath = \"/x\"\n", nil, "[projects] not a plain name: 'gém'"},
		{"disabled factory project still listed", "[projects.p]\nenabled = false\n", map[string]string{"foreman_flags": "--project p"}, ""},
		{"projects before hosts", "[foreman]\nhosts = [\"h\"]\n[projects.p]\npath = \"/x\"\n", map[string]string{"foreman_flags": "--project p --host h"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// base ends in [host], so a bare key lands there.
			vs, err := Values(parse(t, base+c.extra))
			if c.problem != "" {
				if err == nil || err.Error() != c.problem {
					t.Fatalf("err = %v, want %q", err, c.problem)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for k, want := range c.want {
				if got, _ := vs.Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			if uid, _ := vs.Get("factory_uid"); uid != "7" {
				t.Errorf("factory_uid = %q", uid)
			}
		})
	}
	// str() of a TOML boolean, as Python prints it.
	vs, err := Values(parse(t, "[identity]\ngit_name = \"B\"\ngit_email = \"b@x\"\npr_footer = \"f\"\n[host]\nfactory_uid = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if uid, _ := vs.Get("factory_uid"); uid != "False" {
		t.Errorf("factory_uid = %q, want False", uid)
	}
}

// The ported patterns agree with Python's over the regex corpus.
func TestPatternsMatchPython(t *testing.T) {
	var golden struct {
		Patterns []struct {
			Name  string `json:"name"`
			Py    string `json:"py"`
			Op    string `json:"op"`
			Cases []struct {
				In  string          `json:"in"`
				Out json.RawMessage `json:"out"`
			} `json:"cases"`
		} `json:"patterns"`
	}
	readJSON(t, filepath.Join("..", "textx", "testdata", "py", "regex.json"), &golden)
	p := newPatterns()
	full := map[string]func(string) bool{
		"render_project": p.project.MatchString,
		"render_host":    p.host.MatchString,
		"render_login":   p.login.MatchString,
	}
	seen := 0
	for _, g := range golden.Patterns {
		if !strings.HasPrefix(g.Name, "render_") {
			continue
		}
		seen++
		for _, c := range g.Cases {
			var got any
			switch g.Name {
			case "render_placeholder":
				names := []string{}
				for _, m := range p.placeholder.FindAllStringSubmatch(c.In, -1) {
					names = append(names, m[1])
				}
				got = names
			case "render_placeholder_sub":
				got = substitute(p.placeholder, c.In, func(name string) string { return "<" + name + ">" })
			default:
				if full[g.Name](c.In) {
					got = []string{c.In}
				}
			}
			gotJSON, _ := json.Marshal(got)
			var want, have any
			_ = json.Unmarshal(c.Out, &want)
			_ = json.Unmarshal(gotJSON, &have)
			if fmt.Sprint(want) != fmt.Sprint(have) {
				t.Errorf("%s(%q) = %s, want %s", g.Name, c.In, gotJSON, c.Out)
			}
		}
	}
	if seen != 5 {
		t.Errorf("found %d render patterns in the golden, want 5", seen)
	}
}

// listTree is every regular file under dir with its mode and bytes.
func listTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			if info.Mode().Perm() != 0o755 {
				t.Errorf("directory %s has mode %o", rel, info.Mode().Perm())
			}
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = fmt.Sprintf("%#o", info.Mode().Perm())
		out[filepath.ToSlash(rel)+"#"] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The example config renders from the embedded assets exactly as bin/render
// does: every file, its bytes and its mode (bin/ and patch_provision.py are
// deploy's job now, §8.4).
func TestRenderExampleMatchesPython(t *testing.T) {
	var golden struct {
		Files map[string]struct {
			Mode string `json:"mode"`
		} `json:"files"`
	}
	readJSON(t, filepath.Join("testdata", "py", "example.json"), &golden)
	cfgDir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("..", "..", "examples", "config", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out")
	if err := Render(agentfactory.Assets, cfgDir, out); err != nil {
		t.Fatal(err)
	}
	got := listTree(t, out)
	var gotNames []string
	for k := range got {
		if !strings.HasSuffix(k, "#") {
			gotNames = append(gotNames, k)
		}
	}
	sort.Strings(gotNames)
	var wantNames []string
	for k := range golden.Files {
		wantNames = append(wantNames, k)
	}
	sort.Strings(wantNames)
	if strings.Join(gotNames, "\n") != strings.Join(wantNames, "\n") {
		t.Fatalf("files differ:\n%s", testkit.Diff(strings.Join(wantNames, "\n"), strings.Join(gotNames, "\n")))
	}
	for _, name := range wantNames {
		if want := strings.Replace(golden.Files[name].Mode, "0o", "0", 1); got[name] != want {
			t.Errorf("%s: mode %s, want %s", name, got[name], want)
		}
		want, err := os.ReadFile(filepath.Join("testdata", "py", "example", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if got[name+"#"] != string(want) {
			t.Errorf("%s differs:\n%s", name, testkit.Diff(string(want), got[name+"#"]))
		}
	}
}

// Every stage has a role template in the default flavour (§2.4.1: render's
// test fails if a role lacks a template).
func TestEveryStageHasATemplate(t *testing.T) {
	for _, s := range stageTemplates() {
		dir := "templates/ruby-gem/" + s
		entries, err := fs.ReadDir(agentfactory.Assets, dir)
		if err != nil || len(entries) == 0 {
			t.Errorf("stage template %s: %v (%d files)", dir, err, len(entries))
		}
	}
	if n := len(stageTemplates()); n != 10 {
		t.Errorf("%d stages, want the 9 of ROLES and the verifier", n)
	}
}

const smallConfig = `template_flavour = "flav"
[identity]
git_name = "Bot"
git_email = "bot@example.com"
pr_footer = "By a bot."
[host]
factory_uid = 1001
[templates]
work = "my-worker"
`

func smallAssets() fstest.MapFS {
	fsys := fstest.MapFS{
		"templates/default/system-prompt.md":  {Data: []byte(`{"pr": "{{pr_footer}}"}`)},
		"systemd/x.service":                   {Data: []byte("User={{factory_user}}\r\nHome={{factory_home}}\rRoot={{factory_root}}\n")},
		"images/claude/agent-rules.md":        {Data: []byte("# rules for {{git_name}}\n")},
		"images/claude/managed-settings.json": {Data: []byte(`{"email": "{{git_email}}"}`)},
		"images/claude/Containerfile":         {Data: []byte("FROM x\r\n# {{not_rendered}}\n")},
		"images/claude/.dockerignore":         {Data: []byte("*\n")},
		"images/pi/Containerfile":             {Data: []byte("FROM pi\n")},
	}
	for _, s := range stageTemplates() {
		fsys["templates/flav/"+s+"/agents.md"] = &fstest.MapFile{Data: []byte(s + " as {{git_name}}\n")}
	}
	return fsys
}

func writeConfigDir(t *testing.T, cfg string, rules *string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if rules != nil {
		if err := os.WriteFile(filepath.Join(dir, "rules.md"), []byte(*rules), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRenderLayoutAndPlaceholders(t *testing.T) {
	rules := "Local rule: sign as {{git_name}}.\r\n"
	cfgDir := writeConfigDir(t, smallConfig, &rules)
	out := filepath.Join(t.TempDir(), "out")
	// A stale file from an earlier render goes.
	if err := os.MkdirAll(filepath.Join(out, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "old", "stale"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Render(smallAssets(), cfgDir, out); err != nil {
		t.Fatal(err)
	}
	got := listTree(t, out)
	want := map[string]string{
		"scion-templates/triage/agents.md#":         "triage as Bot\n",
		"scion-templates/my-worker/agents.md#":      "worker as Bot\n",
		"scion-templates/manager/agents.md#":        "manager as Bot\n",
		"scion-templates/voter/agents.md#":          "voter as Bot\n",
		"scion-templates/default/system-prompt.md#": `{"pr": "By a bot."}`,
		"systemd/x.service#":                        "User=factory\nHome=/home/factory\nRoot=/home/factory/factory\n",
		"images/scion-agent/agent-rules.md#":        "# rules for Bot\n\nLocal rule: sign as Bot.\n",
		"images/pi/agent-rules.md#":                 "# rules for Bot\n\nLocal rule: sign as Bot.\n",
		"images/scion-agent/managed-settings.json#": `{"email": "bot@example.com"}`,
		"images/scion-agent/Containerfile#":         "FROM x\r\n# {{not_rendered}}\n",
		"images/scion-agent/.dockerignore#":         "*\n",
		"images/pi/Containerfile#":                  "FROM pi\n",
		"config.toml#":                              smallConfig,
		"config.toml":                               "0600",
		"images/pi/agent-rules.md":                  "0644",
		"systemd/x.service":                         "0644",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for k := range got {
		if strings.HasPrefix(k, "old/") || strings.HasPrefix(k, "bin/") || strings.HasPrefix(k, "scion-templates/worker/") || k == "patch_provision.py" {
			t.Errorf("unexpected file %s", k)
		}
	}
	if n := len(got) / 2; n != 10+1+1+4+2+1 {
		t.Errorf("%d files: %v", n, got)
	}
}

func TestRenderRefusals(t *testing.T) {
	assets := smallAssets()
	unknownRules := "{{nope}} and {{also_no}} and {{nope}}\n"
	badUTF8 := fstest.MapFS{}
	for k, v := range assets {
		badUTF8[k] = v
	}
	badUTF8["systemd/bad.service"] = &fstest.MapFile{Data: []byte("\xff\xfe")}
	cases := []struct {
		name   string
		cfg    string
		rules  *string
		assets fs.FS
		want   string // the message; "<out>" stands for the out dir
	}{
		{"unknown placeholder in a template", smallConfig, nil, func() fs.FS {
			m := fstest.MapFS{}
			for k, v := range assets {
				m[k] = v
			}
			m["templates/flav/task/agents.md"] = &fstest.MapFile{Data: []byte("{{zeta}} {{alpha}}")}
			return m
		}(), "<out>/scion-templates/task/agents.md: unknown placeholders ['alpha', 'zeta']"},
		{"unknown placeholder in rules.md", smallConfig, &unknownRules, assets,
			"<out>/images/scion-agent/agent-rules.md: unknown placeholders ['also_no', 'nope']"},
		{"missing values", "[host]\nfactory_uid = 1\n", nil, assets,
			"config.toml is missing: git_name, git_email, pr_footer (see [identity] and [host])"},
		{"missing flavour", strings.Replace(smallConfig, `"flav"`, `"nope"`, 1), nil, assets, "templates/nope/triage"},
		{"flavour outside the templates", strings.Replace(smallConfig, `"flav"`, `"../images"`, 1), nil, assets, "template_flavour"},
		{"template name with a slash", smallConfig + "triage = \"../../x\"\n", nil, assets, "[templates] triage: not a plain name: '../../x'"},
		{"template name dot-dot", smallConfig + "triage = \"..\"\n", nil, assets, "[templates] triage: not a plain name: '..'"},
		{"not UTF-8", smallConfig, nil, badUTF8, "<out>/systemd/bad.service: not UTF-8 text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfgDir := writeConfigDir(t, c.cfg, c.rules)
			out := filepath.Join(t.TempDir(), "out")
			err := Render(c.assets, cfgDir, out)
			if err == nil {
				t.Fatal("no error")
			}
			want := strings.ReplaceAll(c.want, "<out>", out)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want %q", err, want)
			}
		})
	}
}

// Refusals found before any file is written leave the out dir as it was;
// so do placeholder errors, which Python found only after wiping it.
func TestRenderKeepsTheOutDirOnRefusal(t *testing.T) {
	cfgDir := writeConfigDir(t, "[host]\nfactory_uid = 1\n", nil)
	out := t.TempDir()
	keep := filepath.Join(out, "keep")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Render(smallAssets(), cfgDir, out); err == nil {
		t.Fatal("no error")
	}
	m := fstest.MapFS{}
	for k, v := range smallAssets() {
		m[k] = v
	}
	m["systemd/y.service"] = &fstest.MapFile{Data: []byte("{{bad}}")}
	if err := Render(m, writeConfigDir(t, smallConfig, nil), out); err == nil {
		t.Fatal("no error")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("out dir changed on a refusal: %v", err)
	}
}

// Render wipes its out dir, so it refuses one that would take the config,
// or the whole filesystem, with it.
func TestRenderRefusesAnOutDirHoldingTheConfig(t *testing.T) {
	cfgDir := writeConfigDir(t, smallConfig, nil)
	for _, out := range []string{cfgDir, filepath.Dir(cfgDir), cfgDir + "/", "/"} {
		err := Render(smallAssets(), cfgDir, out)
		var p Problem
		if !errors.As(err, &p) {
			t.Errorf("Render(%q) = %v, want a refusal", out, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "config.toml")); err != nil {
		t.Fatalf("config gone: %v", err)
	}
}

func TestRenderMissingConfig(t *testing.T) {
	err := Render(smallAssets(), t.TempDir(), filepath.Join(t.TempDir(), "out"))
	if err == nil || !strings.Contains(err.Error(), "config.toml") {
		t.Errorf("err = %v", err)
	}
}

// The live config renders (skipped unless FACTORY_LIVE_SNAPSHOT is set);
// it is the strongest check that nothing in a real config is refused.
func TestRenderLiveConfig(t *testing.T) {
	snap := testkit.LiveSnapshot(t)
	var cfgDir string
	for _, c := range []string{filepath.Join(snap, "config"), snap} {
		if _, err := os.Stat(filepath.Join(c, "config.toml")); err == nil {
			cfgDir = c
			break
		}
	}
	if cfgDir == "" {
		t.Skip("no config.toml in the live snapshot")
	}
	out := filepath.Join(t.TempDir(), "out")
	if err := Render(agentfactory.Assets, cfgDir, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "systemd", "foreman.service")); err != nil {
		t.Error(err)
	}
}
