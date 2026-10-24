package config

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// selfHostProblems are the problems and warnings about the self-hosting
// keys (walls, hygiene, self_deploy, [hygiene]).
func selfHostProblems(t *testing.T, text string) (problems, warnings []string) {
	t.Helper()
	ps, ws := mustParse(t, text).Problems(nil)
	about := func(s string) bool {
		return strings.Contains(s, "self_deploy") || strings.Contains(s, "walls") || strings.Contains(s, "hygiene")
	}
	for _, p := range ps {
		if about(p) {
			problems = append(problems, p)
		}
	}
	for _, w := range ws {
		if about(w) {
			warnings = append(warnings, w)
		}
	}
	return problems, warnings
}

const selfHostProject = "[projects.core]\npath = \"/p/core\"\ngithub = \"example/core\"\ninstall = \"i\"\ntest = \"t\"\nlint = \"l\"\n"

func TestSelfHostKeysDecode(t *testing.T) {
	c := mustParse(t, "[hygiene]\ndeny = [\"hostx\", \"192.0.2.7\"]\n\n"+selfHostProject+
		"walls = [\"internal/gate/*\", \"deploy.sh\"]\nhygiene = true\nself_deploy = \"core\"\n\n[projects.gem]\npath = \"/p/gem\"\n")
	if len(c.Unknown) != 0 || len(c.Mistyped) != 0 {
		t.Fatalf("unknown %q, mistyped %q", c.Unknown, c.Mistyped)
	}
	if !reflect.DeepEqual(c.Hygiene.Deny, []string{"hostx", "192.0.2.7"}) {
		t.Errorf("[hygiene] deny = %q", c.Hygiene.Deny)
	}
	core, gem := c.Projects[0], c.Projects[1]
	if !reflect.DeepEqual(core.Walls, []string{"internal/gate/*", "deploy.sh"}) || !core.Hygiene || core.SelfDeploy != SelfDeployCore {
		t.Errorf("core: walls %q hygiene %v self_deploy %q", core.Walls, core.Hygiene, core.SelfDeploy)
	}
	if gem.Walls != nil || gem.Hygiene || gem.SelfDeploy != "" {
		t.Errorf("an ordinary project: walls %q hygiene %v self_deploy %q", gem.Walls, gem.Hygiene, gem.SelfDeploy)
	}
	if problems, warnings := selfHostProblems(t, "[hygiene]\ndeny = [\"hostx\"]\n\n"+selfHostProject+"walls = [\"x/*\"]\nhygiene = true\nself_deploy = \"config\"\n"); problems != nil || warnings != nil {
		t.Errorf("a good self-hosted project: %q %q", problems, warnings)
	}
}

func TestSelfDeployProblems(t *testing.T) {
	text := selfHostProject + "self_deploy = \"both\"\n\n" +
		"[projects.conf]\npath = \"/p/conf\"\ngithub = \"example/conf\"\nself_deploy = \"config\"\n\n" +
		"[projects.conf2]\npath = \"/p/conf2\"\ngithub = \"example/conf2\"\nself_deploy = \"config\"\n\n" +
		"[projects.local]\npath = \"/p/local\"\nself_deploy = \"core\"\n\n" +
		"[projects.notes]\nmode = \"board\"\nself_deploy = \"core\"\n\n" +
		"[projects.wiki]\nmode = \"board\"\ngithub = \"example/wiki\"\nself_deploy = \"core\"\n\n" +
		"[projects.real]\npath = \"/p/real\"\ngithub = \"example/real\"\nself_deploy = \"core\"\n"
	problems, _ := selfHostProblems(t, text)
	// core, local, notes and wiki each fail a check, so none of them holds
	// "core": real, the first good one, is no duplicate.
	want := []string{
		`[projects.core] self_deploy = "both": it is "core" or "config"`,
		`[projects.conf2] self_deploy = "config": [projects.conf] already deploys config`,
		`[projects.local] self_deploy = "core" needs github: a deploy follows a merged pull request`,
		`[projects.notes] self_deploy = "core" is on a board project: the pipeline never merges one`,
		`[projects.wiki] self_deploy = "core" is on a board project: the pipeline never merges one`,
	}
	if !reflect.DeepEqual(problems, want) {
		t.Errorf("problems:\n got %q\nwant %q", problems, want)
	}
}

func TestHygieneDenyProblems(t *testing.T) {
	problems, warnings := selfHostProblems(t, "[hygiene]\ndeny = [\"hostx\", \"ab\", \"\", \"   \", \" a \", \" hostx \"]\n\n"+selfHostProject+"walls = [\"x/*\", \"\"]\n")
	want := []string{
		`[hygiene] deny: "ab" is shorter than 3 characters`,
		`[hygiene] deny: "" is shorter than 3 characters`,
		`[hygiene] deny: "   " is shorter than 3 characters`,
		`[hygiene] deny: " a " is shorter than 3 characters`,
		`[hygiene] deny: a term starts or ends with whitespace, so it misses a hit at a line's start or end`,
		`[projects.core] walls: an entry is empty`,
	}
	if !reflect.DeepEqual(problems, want) || warnings != nil {
		t.Errorf("problems %q, warnings %q", problems, warnings)
	}
	problems, warnings = selfHostProblems(t, selfHostProject+"hygiene = true\n")
	if problems != nil || !slices.Equal(warnings, []string{"[projects.core] hygiene = true with an empty [hygiene] deny: the scan finds nothing"}) {
		t.Errorf("an empty deny list: problems %q, warnings %q", problems, warnings)
	}
}

// A safety key that is not the type it must be fails the check (it would
// otherwise decode to nothing and turn the protection off), and never quotes
// the value.
func TestSelfHostKeysMistyped(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"walls a string", selfHostProject + "walls = \"deploy.sh\"\n", "[projects.core] walls is not a list of strings: no wall applies until it is"},
		{"walls with a number", selfHostProject + "walls = [\"images/*\", 7]\n", "[projects.core] walls is not a list of strings: no wall applies until it is"},
		{"deny a string", "[hygiene]\ndeny = \"hostx\"\n", "[hygiene] deny is not a list of strings: the scan cannot run until it is"},
		{"deny with a number", "[hygiene]\ndeny = [\"hostx\", 7]\n", "[hygiene] deny is not a list of strings: the scan cannot run until it is"},
		{"self_deploy a number", selfHostProject + "self_deploy = 1\n", "[projects.core] self_deploy is not a string: it is \"core\" or \"config\""},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			conf := mustParse(t, c.text)
			if len(conf.Mistyped) != 0 {
				t.Errorf("mistyped %q: a problem, not a warning", conf.Mistyped)
			}
			problems, _ := selfHostProblems(t, c.text)
			if !slices.Equal(problems, []string{c.want}) {
				t.Errorf("problems %q, want %q", problems, c.want)
			}
			for _, p := range problems {
				if strings.Contains(p, "hostx") || strings.Contains(p, "deploy.sh") {
					t.Errorf("%q quotes the value", p)
				}
			}
		})
	}
}

func TestSelfDeployProject(t *testing.T) {
	c := mustParse(t, selfHostProject+"self_deploy = \"core\"\n\n[projects.conf]\npath = \"/p/conf\"\ngithub = \"example/conf\"\nself_deploy = \"config\"\n\n[projects.gem]\npath = \"/p/gem\"\n")
	if p, ok := c.SelfDeployProject(SelfDeployConfig); !ok || p.Key != "conf" {
		t.Errorf("config: %v %v", p, ok)
	}
	if p, ok := c.SelfDeployProject(SelfDeployCore); !ok || p.Key != "core" {
		t.Errorf("core: %v %v", p, ok)
	}
	if _, ok := c.SelfDeployProject(""); ok {
		t.Error("an ordinary project is no self-deploy project")
	}
	if _, ok := mustParse(t, "[projects.gem]\npath = \"/p/gem\"\n").SelfDeployProject(SelfDeployConfig); ok {
		t.Error("no config project")
	}
	// a board project never merges, so it is not the one
	if _, ok := mustParse(t, "[projects.conf]\nmode = \"board\"\ngithub = \"example/conf\"\nself_deploy = \"config\"\n").SelfDeployProject(SelfDeployConfig); ok {
		t.Error("a board project")
	}
}

// Public review's private values: the deny terms and every other name
// config holds, once each, never the project's own name, a built-in kind
// or what [hygiene] allow names.
func TestPrivateNames(t *testing.T) {
	c := mustParse(t, "[hygiene]\ndeny = [\"hosty\", \"HOSTY\"]\nallow = [\" Gadgets \", \"bob\"]\n\n"+
		"[board]\npeople = [\"alice\", \"bob\"]\n\n[github]\nreviewers = [\"alice-gh\"]\n\n[foreman]\ntailscale_users = [\"carol@example.com\"]\n\n"+
		"[projects.gem]\npath = \"/p/gem\"\ngithub = \"acme/gem\"\nhygiene = true\n\n"+
		"[projects.widgets]\npath = \"/p/w\"\ngithub = \"acme/widgets-app\"\n\n[projects.gadgets]\npath = \"/p/g\"\n\n"+
		"[hosts.hostz]\nssh = \"factory@hostz\"\n\n[hosts.hostw]\nssh = \"factory@hostw.example:2222\"\n\n[remote.hostq]\nagent = \"pi\"\n\n"+
		"[kinds.ruby-gem]\nlint = \"rubocop\"\n\n[kinds.web-shard]\nfrom = \"crystal-shard\"\n")
	// the project's own repository, owner and name are not private; the
	// other project's are
	want := []string{"hosty", "widgets", "acme/widgets-app", "widgets-app", "hostz", "hostw", "hostw.example", "hostq", "web-shard",
		"alice", "alice-gh", "carol@example.com"}
	if got := c.PrivateNames("gem"); !slices.Equal(got, want) {
		t.Errorf("PrivateNames(gem) = %q, want %q", got, want)
	}
	if got := c.PrivateNames("widgets"); !slices.Contains(got, "gem") || !slices.Contains(got, "acme/gem") || slices.Contains(got, "widgets") ||
		slices.Contains(got, "widgets-app") || slices.Contains(got, "acme") {
		t.Errorf("PrivateNames(widgets) = %q", got)
	}
	// with no [board] people, the default name is a plain word, not a person
	if got := mustParse(t, "[projects.gem]\npath = \"/p/gem\"\n").PrivateNames("gem"); slices.Contains(got, DefaultPerson) {
		t.Errorf("the default person is private: %q", got)
	}
}
