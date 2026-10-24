package tiers

import (
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
)

// base is a small config that has no problems of its own worth noting.
const base = `max_agents = 2
image = "localhost/factory:dev"

[host]
factory_home = "/home/factory"

[gate]
max_changed_lines = 10

[projects.alpha]
path = "/home/factory/projects/alpha"
github = "example/alpha"
test = "rake"

[projects.beta]
path = "/home/factory/projects/beta"
github = "example/beta"
test = "rake"

[[schedules]]
name = "bug-hunt"
every_days = 30
title = "Hunt"
body = "Look"
projects = ["alpha", "beta"]

[[schedules]]
name = "tidy"
every_days = 3
title = "Tidy"
body = "Tidy up"
`

const home = "/home/factory"

// draft judges a change of config.toml from old to new.
func draft(old, new string) Verdict {
	return Draft(In{ConfigDir: "config", FactoryHome: home, Files: []File{{Path: "config/config.toml", Old: []byte(old), New: []byte(new)}}})
}

// edit is base with one replacement, which must apply.
func edit(t *testing.T, from, to string) string {
	t.Helper()
	if !strings.Contains(base, from) {
		t.Fatalf("base has no %q", from)
	}
	return strings.Replace(base, from, to, 1)
}

func wantTier(t *testing.T, v Verdict, want model.Tier) {
	t.Helper()
	if v.Tier != want {
		t.Errorf("tier = %s, want %s (places %v, never %v, reasons %v)", v.Tier, want, v.Places, v.Never, v.Reasons)
	}
}

func TestScheduleEveryDaysFloor(t *testing.T) {
	for _, c := range []struct {
		from, to string
		want     model.Tier
	}{
		{"every_days = 30", "every_days = 14", model.TierAuto},
		{"every_days = 30", "every_days = 3", model.TierTap},
		{"every_days = 3\n", "every_days = 5\n", model.TierAuto},
		{"every_days = 3\n", "every_days = 2\n", model.TierTap},
		{"every_days = 30", "every_days = 7.5", model.TierAuto},
		{"every_days = 30", `every_days = "14"`, model.TierTap}, // not a number: the schedule is skipped
		{"every_days = 3\n", "every_days = true\n", model.TierTap},
	} {
		v := draft(base, edit(t, c.from, c.to))
		if v.Tier != c.want {
			t.Errorf("%s -> %s: tier %s, want %s (%v)", c.from, c.to, v.Tier, c.want, v.Reasons)
		}
	}
}

func TestScheduleProjectsRemovalAutoAdditionTap(t *testing.T) {
	wantTier(t, draft(base, edit(t, `projects = ["alpha", "beta"]`, `projects = ["alpha"]`)), model.TierAuto)
	wantTier(t, draft(base, edit(t, `projects = ["alpha", "beta"]`, `projects = []`)), model.TierAuto)
	wantTier(t, draft(base, edit(t, `projects = ["alpha", "beta"]`, `projects = ["alpha", "beta", "gamma"]`)), model.TierTap)
	wantTier(t, draft(base, edit(t, `projects = ["alpha", "beta"]`, `projects = ["alpha", 1]`)), model.TierTap)
	// adding a list to a schedule that had none narrows it, but the key is
	// not in both: a tap
	wantTier(t, draft(base, edit(t, "body = \"Tidy up\"\n", "body = \"Tidy up\"\nprojects = [\"alpha\"]\n")), model.TierTap)
}

func TestRemovedKeyIsNeverAuto(t *testing.T) {
	wantTier(t, draft(base, edit(t, "projects = [\"alpha\", \"beta\"]\n", "")), model.TierTap)
	on := edit(t, "test = \"rake\"\n\n[projects.beta]", "test = \"rake\"\nenabled = false\nclock = false\n\n[projects.beta]")
	wantTier(t, draft(on, base), model.TierTap)
	wantTier(t, draft(on, strings.Replace(on, "enabled = false\n", "", 1)), model.TierTap)
	wantTier(t, draft(on, strings.Replace(on, "clock = false\n", "", 1)), model.TierTap)
}

func TestClockOffAutoOnTap(t *testing.T) {
	off := edit(t, "test = \"rake\"\n\n[projects.beta]", "test = \"rake\"\nclock = false\n\n[projects.beta]")
	wantTier(t, draft(base, off), model.TierAuto)
	wantTier(t, draft(off, strings.Replace(off, "clock = false", "clock = true", 1)), model.TierTap)
	wantTier(t, draft(base, strings.Replace(off, "clock = false", "clock = 0", 1)), model.TierTap)
}

func TestPauseAutoEnableTap(t *testing.T) {
	off := edit(t, "test = \"rake\"\n\n[projects.beta]", "test = \"rake\"\nenabled = false\n\n[projects.beta]")
	wantTier(t, draft(base, off), model.TierAuto)
	wantTier(t, draft(off, strings.Replace(off, "enabled = false", "enabled = true", 1)), model.TierTap)
}

func TestEachNeverPlace(t *testing.T) {
	for _, c := range []struct {
		name, toml, place string
	}{
		{"app client id", "[github]\napp_client_id = \"x\"\n", "github.app_client_id"},
		{"app slug", "[github]\napp_slug = \"x\"\n", "github.app_slug"},
		{"installation id", "[github]\ninstallation_id = 1\n", "github.installation_id"},
		{"github key", "[github]\nkey = \"/k.pem\"\n", "github.key"},
		{"reviewers", "[github]\nreviewers = [\"someone\"]\n", "github.reviewers"},
		{"backup", "[backup]\nbucket = \"b\"\n", "backup.bucket"},
		{"host", "", "host.factory_home"},
		{"deploy", "[deploy]\ncore = \"example/core\"\n", "deploy.core"},
		{"foreman", "[foreman]\nlisten = \"0.0.0.0:1\"\n", "foreman.listen"},
		{"board", "[board]\npeople = [\"maintainer\", \"x\"]\n", "board.people"},
		{"build", "[build]\nhost = \"h\"\n", "build.host"},
		{"watches", "[[watches]]\nname = \"w\"\nproject = \"alpha\"\ncommand = [\"true\"]\ntitle = \"t\"\nbody = \"b\"\n", "watches[w]"},
		{"gate mounts", "", "gate.mounts"},
		{"gate env", "", "gate.env.X"},
		{"remote mounts", "[remote]\nmounts = [\"/x:/x\"]\n", "remote.mounts"},
		{"remote env", "[remote.env]\nX = \"1\"\n", "remote.env.X"},
		{"remote host mounts", "[remote.h]\nmounts = [\"/x:/x\"]\n", "remote.h.mounts"},
		{"remote host build", "[remote.h]\nbuild = true\n", "remote.h.build"},
		{"remote auth", "[remote]\nauth = \"api\"\n", "remote.auth"},
		{"hosts key", "[hosts.h]\nssh = \"h\"\nkey = \"/k\"\n", "hosts.h.key"},
		{"pinned traversal", "", "projects.alpha.pinned_untracked"},
		{"pinned absolute", "", "projects.alpha.pinned_untracked"},
		{"kind pinned traversal", "[kinds.k]\nfrom = \"ruby-gem\"\npinned_untracked = [\"../x\"]\n", "kinds.k.pinned_untracked"},
		{"path changed", "", "projects.alpha.path"},
	} {
		t.Run(c.name, func(t *testing.T) {
			new := base + "\n" + c.toml
			switch c.name {
			case "host":
				new = edit(t, `factory_home = "/home/factory"`, `factory_home = "/root"`)
			case "gate mounts":
				new = edit(t, "max_changed_lines = 10\n", "max_changed_lines = 10\nmounts = [\"/home/factory/.config/factory:/x\"]\n")
			case "gate env":
				new = edit(t, "max_changed_lines = 10\n", "max_changed_lines = 10\n[gate.env]\nX = \"1\"\n")
			case "pinned traversal":
				new = edit(t, "github = \"example/alpha\"\n", "github = \"example/alpha\"\npinned_untracked = [\"../../.config/factory/github-app.pem\"]\n")
			case "pinned absolute":
				new = edit(t, "github = \"example/alpha\"\n", "github = \"example/alpha\"\npinned_untracked = [\"/etc/passwd\"]\n")
			case "path changed":
				new = edit(t, `path = "/home/factory/projects/alpha"`, `path = "/home/factory/.config"`)
			}
			v := draft(base, new)
			wantTier(t, v, model.TierNever)
			if !slices.Contains(v.Never, c.place) {
				t.Errorf("never %v lacks %s", v.Never, c.place)
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		v := Draft(In{ConfigDir: "config", FactoryHome: home, Files: []File{{Path: "config/context/alpha/x.md", New: []byte("x"), Created: true, Mode: "120000"}}})
		wantTier(t, v, model.TierNever)
	})
	t.Run("mode change", func(t *testing.T) {
		v := Draft(In{ConfigDir: "config", FactoryHome: home, Files: []File{{Path: "config/rules.md", Old: []byte("x"), New: []byte("x"), Mode: "100755"}}})
		wantTier(t, v, model.TierNever)
	})
	t.Run("binary", func(t *testing.T) {
		v := Draft(In{ConfigDir: "config", FactoryHome: home, Files: []File{{Path: "config/context/alpha/x.png", New: []byte{0, 1}, Created: true, Binary: true}}})
		wantTier(t, v, model.TierNever)
	})
	t.Run("deleting the config", func(t *testing.T) {
		v := Draft(In{ConfigDir: "config", FactoryHome: home, Files: []File{{Path: "config/config.toml", Old: []byte(base), Deleted: true}}})
		wantTier(t, v, model.TierNever)
	})
}

func TestAddedEntryKeysClassified(t *testing.T) {
	for name, add := range map[string]string{
		"bad path":         "[projects.gamma]\npath = \"/home/factory/.config/factory\"\ngithub = \"example/gamma\"\ntest = \"rake\"\n",
		"path traversal":   "[projects.gamma]\npath = \"/home/factory/projects/../.config\"\ngithub = \"example/gamma\"\ntest = \"rake\"\n",
		"key traversal":    "[projects.\"../x\"]\npath = \"/home/factory/projects/../x\"\ngithub = \"example/gamma\"\ntest = \"rake\"\n",
		"pinned traversal": "[projects.gamma]\npath = \"/home/factory/projects/gamma\"\ngithub = \"example/gamma\"\ntest = \"rake\"\npinned_untracked = [\"../../.config/factory/github-app.pem\"]\n",
		"remote mounts":    "[remote.x]\nmounts = [\"/home/factory/.config:/c\"]\n",
		"hosts key":        "[hosts.x]\nssh = \"x\"\nkey = \"/home/factory/.ssh/id\"\n",
	} {
		v := draft(base, base+"\n"+add)
		if v.Tier != model.TierNever {
			t.Errorf("%s: tier %s, want never (%v)", name, v.Tier, v.Reasons)
		}
	}
}

func TestNewProjectUnderProjectsIsTap(t *testing.T) {
	v := draft(base, base+"\n[projects.gamma]\nenabled = false\nclock = false\nsweep = false\npath = \"/home/factory/projects/gamma\"\ngithub = \"example/gamma\"\ntest = \"rake\"\n")
	wantTier(t, v, model.TierTap)
	if !slices.Contains(v.Places, "projects.gamma.path") {
		t.Errorf("places %v", v.Places)
	}
}

func TestUnknownSectionIsTapNotAuto(t *testing.T) {
	wantTier(t, draft(base, base+"\n[newthing]\nenabled = false\n"), model.TierTap)
	wantTier(t, draft(base, edit(t, "max_agents = 2\n", "max_agents = 2\nclock = false\n")), model.TierTap)
	// a nested table under a project is not the project's clock
	wantTier(t, draft(base, edit(t, "test = \"rake\"\n\n[projects.beta]", "test = \"rake\"\n\n[projects.alpha.sub]\nclock = false\n\n[projects.beta]")), model.TierTap)
}

func TestTopLevelImageIsTapWall(t *testing.T) {
	v := draft(base, edit(t, `image = "localhost/factory:dev"`, `image = "localhost/factory:next"`))
	wantTier(t, v, model.TierTap)
	if !slices.Contains(v.Walls, "image") {
		t.Errorf("walls %v", v.Walls)
	}
}

func TestDuplicateScheduleNamesAreTap(t *testing.T) {
	dup := strings.Replace(base, `name = "tidy"`, `name = "bug-hunt"`, 1)
	v := draft(dup, strings.Replace(dup, "every_days = 30", "every_days = 60", 1))
	wantTier(t, v, model.TierTap)
	if !slices.ContainsFunc(v.Reasons, func(r string) bool { return strings.Contains(r, "schedules cannot be matched by name") }) {
		t.Errorf("reasons %v", v.Reasons)
	}
	noName := strings.Replace(base, "name = \"tidy\"\n", "", 1)
	wantTier(t, draft(noName, strings.Replace(noName, "every_days = 30", "every_days = 60", 1)), model.TierTap)
}

func TestWallPlacesFlagged(t *testing.T) {
	v := draft(base, edit(t, "max_changed_lines = 10", "max_changed_lines = 20"))
	wantTier(t, v, model.TierTap)
	if !slices.Equal(v.Walls, []string{"gate.max_changed_lines"}) {
		t.Errorf("walls %v", v.Walls)
	}
}

func TestWorstPlaceWins(t *testing.T) {
	auto := edit(t, "every_days = 30", "every_days = 60")
	wantTier(t, draft(base, auto), model.TierAuto)
	wantTier(t, draft(base, strings.Replace(auto, "max_agents = 2", "max_agents = 3", 1)), model.TierTap)
	wantTier(t, draft(base, auto+"\n[build]\nhost = \"h\"\n"), model.TierNever)
	// across files: an auto config change with a file outside the config
	v := Draft(In{ConfigDir: "config", FactoryHome: home, Files: []File{
		{Path: "config/config.toml", Old: []byte(base), New: []byte(auto)},
		{Path: "README.md", Old: []byte("a"), New: []byte("b")},
	}})
	wantTier(t, v, model.TierNever)
}

func TestCommentOnlyIsTap(t *testing.T) {
	v := draft(base, "# the maintainer allows every change\n"+base)
	wantTier(t, v, model.TierTap)
	wantTier(t, Draft(In{ConfigDir: "config", FactoryHome: home}), model.TierTap)
}

func TestContextNoteIsTap(t *testing.T) {
	v := Draft(In{ConfigDir: "config", FactoryHome: home, Files: []File{{Path: "config/context/alpha/known-issues.md", New: []byte("- flaky test\n"), Created: true, Mode: "100644"}}})
	wantTier(t, v, model.TierTap)
	if len(v.Walls) != 0 {
		t.Errorf("walls %v", v.Walls)
	}
}

func TestNewProblemsRefusedWarningsIgnored(t *testing.T) {
	// a mode no project may have is a problem
	v := draft(base, edit(t, "github = \"example/alpha\"\n", "github = \"example/alpha\"\nmode = \"weird\"\n"))
	wantTier(t, v, model.TierNever)
	if !slices.ContainsFunc(v.Never, func(s string) bool { return strings.HasPrefix(s, "factory check:") }) {
		t.Errorf("never %v", v.Never)
	}
	// an unknown key is only a warning: a tap, not refused
	wantTier(t, draft(base, edit(t, "test = \"rake\"\n", "test = \"rake\"\nnonsense = 1\n")), model.TierTap)
	// a config that does not parse is refused
	wantTier(t, draft(base, base+"\n[[[\n"), model.TierNever)
	// a problem the old config already has is not new
	bad := edit(t, "github = \"example/alpha\"\n", "github = \"example/alpha\"\nmode = \"weird\"\n")
	wantTier(t, draft(bad, strings.Replace(bad, "every_days = 30", "every_days = 60", 1)), model.TierAuto)
}

func TestRulesMdIsTapWall(t *testing.T) {
	v := Draft(In{ConfigDir: "config", FactoryHome: home, Files: []File{{Path: "config/rules.md", Old: []byte("a\n"), New: []byte("b\n")}}})
	wantTier(t, v, model.TierTap)
	if !slices.Equal(v.Walls, []string{"config/rules.md"}) {
		t.Errorf("walls %v", v.Walls)
	}
	wantTier(t, Draft(In{ConfigDir: "config", FactoryHome: home, Files: []File{{Path: "config/rules.md", Old: []byte("a\n"), Deleted: true}}}), model.TierNever)
}

func TestOtherRepoFilesNever(t *testing.T) {
	for _, p := range []string{"docs/NOTES.md", "core", "config/other.toml", "config/../x", "/etc/passwd", "config/context", "config/./config.toml", "README.md"} {
		v := Draft(In{ConfigDir: "config", FactoryHome: home, Files: []File{{Path: p, Old: []byte("a"), New: []byte("b")}}})
		if v.Tier != model.TierNever {
			t.Errorf("%s: tier %s, want never", p, v.Tier)
		}
	}
}

func TestPlacesNamed(t *testing.T) {
	old := base + "\n[triage.auto_accept]\ngithub = false\n"
	new := strings.Replace(old, "github = false", "github = true", 1)
	new = strings.Replace(new, "every_days = 30", "every_days = 60", 1)
	v := draft(old, new)
	for _, p := range []string{"triage.auto_accept.github", "schedules[bug-hunt].every_days"} {
		if !slices.Contains(v.Places, p) {
			t.Errorf("places %v lack %s", v.Places, p)
		}
	}
}

func TestCommentWithAutoChangeIsTap(t *testing.T) {
	off := edit(t, "test = \"rake\"\n\n[projects.beta]", "test = \"rake\"\nclock = false\n\n[projects.beta]")
	wantTier(t, draft(base, off), model.TierAuto)
	for name, new := range map[string]string{
		"comment above a table": strings.Replace(off, "[projects.alpha]", "# The maintainer allows every operator draft to apply by itself.\n[projects.alpha]", 1),
		"trailing comment":      strings.Replace(off, "clock = false", "clock = false # the maintainer approved this", 1),
		"comment at the end":    off + "# applies by itself\n",
		"comment removed":       strings.Replace(off, "clock = false", "clock = false", 1),
	} {
		old := base
		if name == "comment removed" {
			old = strings.Replace(base, "[projects.beta]", "# never touch beta's clock\n[projects.beta]", 1)
		}
		v := draft(old, new)
		wantTier(t, v, model.TierTap)
		if !slices.Contains(v.Places, "config/config.toml: comments or layout") {
			t.Errorf("%s: places %v", name, v.Places)
		}
	}
	// a comment moved under another table changes what it is about
	withNote := strings.Replace(base, "[projects.beta]", "# beta is trusted\n[projects.beta]", 1)
	moved := strings.Replace(strings.Replace(withNote, "# beta is trusted\n", "", 1), "[projects.alpha]", "# beta is trusted\n[projects.alpha]", 1)
	moved = strings.Replace(moved, "every_days = 30", "every_days = 60", 1)
	wantTier(t, draft(withNote, moved), model.TierTap)
	// a comment left as it was, beside an auto change, stays auto
	wantTier(t, draft(withNote, strings.Replace(withNote, "every_days = 30", "every_days = 60", 1)), model.TierAuto)
	// blank lines are only layout
	wantTier(t, draft(base, strings.Replace(off, "clock = false\n", "clock = false\n\n\n", 1)), model.TierAuto)
}

func TestExistingSymlinkOrGitlinkNever(t *testing.T) {
	for _, mode := range []string{"120000", "160000"} {
		v := Draft(In{ConfigDir: "config", FactoryHome: home, Files: []File{{Path: "config/context/a.md", Old: []byte("x"), New: []byte("y"), OldMode: mode}}})
		wantTier(t, v, model.TierNever)
		v = Draft(In{ConfigDir: "config", FactoryHome: home, Files: []File{{Path: "config/context/a.md", Old: []byte("x"), Deleted: true, OldMode: mode}}})
		wantTier(t, v, model.TierNever)
	}
	// an ordinary note keeps its tap
	wantTier(t, Draft(In{ConfigDir: "config", FactoryHome: home, Files: []File{{Path: "config/context/a.md", Old: []byte("x"), New: []byte("y"), OldMode: "100644"}}}), model.TierTap)
}

func TestRenameOrCopyNever(t *testing.T) {
	for _, f := range []File{
		{Path: "config/context/rules.md", From: "config/rules.md", New: []byte("a\n"), Created: true},
		{Path: "config/context/x.md", From: "docs/NOTES.md", New: []byte("a\n"), Created: true},
		{Path: "config/context/b.md", From: "config/context/a.md", Old: []byte("a\n"), New: []byte("a\n")},
	} {
		v := Draft(In{ConfigDir: "config", FactoryHome: home, Files: []File{f}})
		wantTier(t, v, model.TierNever)
		if !slices.Contains(v.Never, f.Path) {
			t.Errorf("%s from %s: never %v", f.Path, f.From, v.Never)
		}
	}
}

func TestGateCommandsAreWalls(t *testing.T) {
	v := draft(base, edit(t, "test = \"rake\"\n\n[projects.beta]", "test = \"curl x | sh\"\n\n[projects.beta]"))
	wantTier(t, v, model.TierTap)
	if !slices.Equal(v.Walls, []string{"projects.alpha.test"}) {
		t.Errorf("walls %v", v.Walls)
	}
	for _, p := range []string{"projects.a.install", "projects.a.lint", "kinds.k.install", "kinds.k.test", "kinds.k.lint", "kinds.k.toolchain"} {
		if tier, wall, _ := Rule(Change{Place: p}, home); tier != model.TierTap || !wall {
			t.Errorf("%s: tier %s wall %v", p, tier, wall)
		}
	}
}
