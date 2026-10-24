package agentfactory

// The deploy tests (slice S24, ARCHITECTURE.md §9.5, §6.2, §15.2): deploy.sh
// runs into temporary homes on this machine (a HOST of the form local:DIR),
// with binaries built here, and the tests check the installed file list and
// what a deploy must never touch. No ssh, no systemctl: deploy prints what
// it would run there.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Python-era and live files on a host before its first Go deploy.
var (
	pythonScript = []byte("#!/usr/bin/env python3\nprint('old')\n")
	oldForeman   = []byte("\x7fELF the old Go foreman")
	foremanState = []string{"foreman-secret", "foreman-secret.since", "foreman-users.json", "foreman-vapid.pem",
		"foreman-push.json", "foreman-seen.json"}
	// what a deploy must leave exactly as it found it
	keptFiles = map[string]string{
		"factory/board/tickets/T0001.json":    "{\"id\": \"T0001\"}\n",
		"factory/board/index.json":            "{}\n",
		"factory/logs/factory.log":            "2026-10-18T00:00:00+00:00 a line\n",
		"factory/images.json":                 "{\"claude\": {\"version\": \"2.1.302\"}}\n",
		"factory/state.json":                  "{}\n",
		"factory/memory/demo/worker.git/HEAD": "ref: refs/heads/main\n",
		".config/factory/backup-key":          "0123456789abcdef0123456789abcdef\n",
		".config/factory/api-tokens.json":     "{\"version\": 1, \"tokens\": []}\n",
		".config/factory/github-app.pem":      "not a real key\n",
		".config/factory/claude-oauth-token":  "not a real token\n",
	}
)

func TestDeploy(t *testing.T) {
	need(t, "bash", "git", "tar", "awk")
	if _, err := exec.LookPath("sha256sum"); err != nil {
		need(t, "shasum")
	}
	if err := exec.Command("git", "rev-parse", "--verify", "HEAD").Run(); err != nil {
		t.Skip("not a git checkout: deploy ships a commit")
	}
	bins := buildBinaries(t)
	commit := headCommit(t)

	t.Run("first Go deploy installs the explicit file list", func(t *testing.T) {
		host, helper := pythonHost(t), pythonHelper(t)
		cfg := configDir(t)
		// the dispatcher's own sciontool, which the helper does not get
		mkfile(t, filepath.Join(host, "factory/images/scion-agent/sciontool"), "binary")
		out := deployOK(t, "--config", cfg, "--host", "local:"+host, "--prebuilt", bins, "--helper", "local:"+helper)

		// the binaries, from the build; pi-run from the source tree
		for _, b := range []string{"factory", "foreman", "factory-remote"} {
			sameFile(t, filepath.Join(host, "factory/bin", b), filepath.Join(bins, b))
			executable(t, filepath.Join(host, "factory/bin", b))
		}
		sameFile(t, filepath.Join(host, "factory/bin/BUILD"), filepath.Join(bins, "BUILD"))
		sameFile(t, filepath.Join(host, "factory/bin/pi-run"), "bin/pi-run")
		executable(t, filepath.Join(host, "factory/bin/pi-run"))
		// only a previous Go binary is kept
		hasBytes(t, filepath.Join(host, "factory/bin/foreman.prev"), oldForeman)
		absent(t, host, "factory/bin/factory.prev", "factory/bin/factory-remote.prev",
			"factory/bin/render", "factory/patch_provision.py", "factory/bin/foreman.new", "factory/bin/.new", "factory/.deploy")
		link(t, filepath.Join(host, ".local/bin/factory"), filepath.Join(host, "factory/bin/factory"))
		link(t, filepath.Join(host, ".local/bin/pi-run"), filepath.Join(host, "factory/bin/pi-run"))

		// what factory render made from the config
		contains(t, filepath.Join(host, "factory/config.toml"), "[foreman]")
		hasBytes(t, filepath.Join(host, "factory/config.toml.prev"), []byte("# the old config\n"))
		hasBytes(t, filepath.Join(host, "factory/context/demo/INDEX.md"), []byte("# demo\n"))
		absent(t, host, "factory/context/gone")
		contains(t, filepath.Join(host, "factory/images/scion-agent/agent-rules.md"), "Extra rule from the config.")
		present(t, host, "factory/images/pi/Containerfile", "factory/systemd/factory@.service",
			"factory/systemd/install-units.sh", "factory/systemd/foreman.service",
			".scion/templates/default/system-prompt.md", ".scion/templates/gem-worker/agents.md",
			".scion/templates/gem-worker/scion-agent.yaml")

		// the sandboxed Foreman unit, with its state in foreman-state/
		unit := filepath.Join(host, ".config/systemd/user/foreman.service")
		sameFile(t, unit, filepath.Join(host, "factory/systemd/foreman.service"))
		contains(t, unit, "ProtectHome=read-only", "InaccessiblePaths=", "MemoryMax=128M",
			"--secret-file %h/factory/foreman-state/foreman-secret", "--api-tokens %h/.config/factory/api-tokens.json")
		for _, f := range foremanState {
			hasBytes(t, filepath.Join(host, "factory/foreman-state", f), []byte("state "+f+"\n"))
			absent(t, host, ".config/factory/"+f)
		}
		mode(t, filepath.Join(host, "factory/foreman-state"), 0o700)

		// the source tree the build came from
		src := filepath.Join(host, "factory/src", commit)
		mode(t, src, 0o700)
		hasBytes(t, filepath.Join(src, "COMMIT"), []byte(commit+"\n"))
		present(t, host, "factory/src/"+commit+"/build.sh", "factory/src/"+commit+"/go.mod")

		// never touched
		for rel, want := range keptFiles {
			hasBytes(t, filepath.Join(host, rel), []byte(want))
		}
		// the build has `factory backup` and the host a key: the timers go in and start;
		// Foreman's wake unit too
		present(t, host, ".config/systemd/user/factory-backup.timer", ".config/systemd/user/factory-backup.service",
			".config/systemd/user/factory-wake.path", ".config/systemd/user/factory-wake.service",
			".config/systemd/user/factory-screens.path", ".config/systemd/user/factory-screens.service")
		mentions(t, out, "would run: systemctl --user daemon-reload",
			"would run: systemctl --user enable --now factory-wake.path",
			"would run: systemctl --user enable --now factory-screens.path",
			"would run: systemctl --user enable --now factory-backup.timer factory-backup-verify.timer")

		// the helper: factory-remote only, and bin/factory a link to it
		sameFile(t, filepath.Join(helper, "factory/bin/factory-remote"), filepath.Join(bins, "factory-remote"))
		sameFile(t, filepath.Join(helper, "factory/bin/BUILD"), filepath.Join(bins, "BUILD"))
		if got, err := os.Readlink(filepath.Join(helper, "factory/bin/factory")); err != nil || got != "factory-remote" {
			t.Errorf("helper bin/factory links to %q (%v), want factory-remote", got, err)
		}
		absent(t, helper, "factory/bin/foreman", "factory/bin/render", "factory/patch_provision.py",
			"factory/bin/factory-remote.prev", "factory/images/scion-agent/sciontool", "factory/.deploy",
			".config/systemd/user/foreman.service")
		present(t, helper, "factory/config.toml", "factory/systemd/factory-rebuild.service",
			"factory/systemd/factory-rebuild.timer", "factory/systemd/install-units.sh", "factory/images/pi/Containerfile",
			"factory/images/scion-agent/Containerfile", "factory/bin/pi-run")
		hasBytes(t, filepath.Join(helper, "factory/config.toml.prev"), []byte("# the helper's old config\n"))
		hasBytes(t, filepath.Join(helper, "factory/jobs/j1/run.log"), []byte("a job\n"))
		// invoked as factory, factory-remote answers version
		v, err := exec.Command(filepath.Join(helper, "factory/bin/factory"), "version").CombinedOutput()
		if err != nil || !strings.Contains(string(v), "s24-test") {
			t.Errorf("helper's bin/factory version: %q, %v", v, err)
		}
	})

	t.Run("a second deploy keeps the previous Go binaries and two source trees", func(t *testing.T) {
		host := pythonHost(t)
		cfg := configDir(t)
		deployOK(t, "--config", cfg, "--host", "local:"+host, "--prebuilt", bins)
		rendered, err := os.ReadFile(filepath.Join(host, "factory/config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		// two older trees: only the newer one stays beside the current
		old := time.Now().Add(-2 * time.Hour)
		for i, name := range []string{"0000000aaaaa", "0000000bbbbb"} {
			d := filepath.Join(host, "factory/src", name)
			mkfile(t, filepath.Join(d, "COMMIT"), name+"\n")
			ts := old.Add(time.Duration(i) * time.Minute)
			if err := os.Chtimes(d, ts, ts); err != nil {
				t.Fatal(err)
			}
		}
		deployOK(t, "--config", cfg, "--host", "local:"+host, "--prebuilt", bins)
		// the same binaries again: .prev keeps the last different one, not a copy of itself
		hasBytes(t, filepath.Join(host, "factory/bin/foreman.prev"), oldForeman)
		absent(t, host, "factory/bin/factory.prev", "factory/bin/factory-remote.prev")
		hasBytes(t, filepath.Join(host, "factory/config.toml.prev"), rendered)
		trees := entries(t, filepath.Join(host, "factory/src"))
		if want := []string{"0000000bbbbb", commit}; !equal(trees, want) {
			t.Errorf("source trees %v, want %v", trees, want)
		}
	})

	t.Run("a deploy of the commit the host runs reuses its binaries", func(t *testing.T) {
		host := pythonHost(t)
		cfg := configDir(t)
		// the test binaries, with a BUILD that names the commit being deployed
		mine := t.TempDir()
		for _, b := range []string{"factory", "foreman", "factory-remote"} {
			data, err := os.ReadFile(filepath.Join(bins, b))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(mine, b), data, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		build, err := os.ReadFile(filepath.Join(bins, "BUILD"))
		if err != nil {
			t.Fatal(err)
		}
		mkfile(t, filepath.Join(mine, "BUILD"), string(build))
		deployOK(t, "--config", cfg, "--host", "local:"+host, "--prebuilt", mine)
		// no --prebuilt and no toolchain needed: the installed build is this commit's
		out := deployOK(t, "--config", cfg, "--host", "local:"+host)
		mentions(t, out, "reused, nothing built")
		for _, b := range []string{"factory", "foreman", "factory-remote"} {
			sameFile(t, filepath.Join(host, "factory/bin", b), filepath.Join(mine, b))
		}
		hasBytes(t, filepath.Join(host, "factory/bin/foreman.prev"), oldForeman)
	})

	t.Run("a binary that does not match BUILD is refused before the host is touched", func(t *testing.T) {
		bad := t.TempDir()
		for _, b := range []string{"factory", "foreman", "factory-remote", "BUILD"} {
			copyFile(t, filepath.Join(bins, b), filepath.Join(bad, b))
		}
		f, err := os.OpenFile(filepath.Join(bad, "foreman"), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString("tampered")
		f.Close()
		host := t.TempDir()
		out, code := deploy(t, "--config", configDir(t), "--host", "local:"+host, "--prebuilt", bad)
		if code == 0 {
			t.Fatalf("deploy took a tampered binary:\n%s", out)
		}
		mentions(t, out, "foreman does not match its sha256 in BUILD")
		absent(t, host, "factory")
	})

	t.Run("a config that factory check refuses installs nothing", func(t *testing.T) {
		host := pythonHost(t)
		cfg := configDir(t)
		appendFile(t, filepath.Join(cfg, "config.toml"), "\n[projects.broken]\nmode = \"neither\"\n")
		out, code := deploy(t, "--config", cfg, "--host", "local:"+host, "--prebuilt", bins)
		if code == 0 {
			t.Fatalf("deploy installed a config with problems:\n%s", out)
		}
		mentions(t, out, "nothing installed")
		hasBytes(t, filepath.Join(host, "factory/bin/factory"), pythonScript)
		hasBytes(t, filepath.Join(host, "factory/config.toml"), []byte("# the old config\n"))
		for _, f := range foremanState {
			present(t, host, ".config/factory/"+f)
		}
	})

	// §11: the new factory's check --board --asks runs on the live root
	// before anything is installed.
	t.Run("a live board the new factory cannot read cleanly installs nothing", func(t *testing.T) {
		host := pythonHost(t)
		mkfile(t, filepath.Join(host, "factory/board/tickets/T0002.json"), "{\"id\": \"T0002\", \"title\": \n")
		out, code := deploy(t, "--config", configDir(t), "--host", "local:"+host, "--prebuilt", bins)
		if code == 0 {
			t.Fatalf("deploy installed over a board it cannot read:\n%s", out)
		}
		mentions(t, out, "board: ", "T0002", "check --board --asks found problems on the live root: nothing installed")
		hasBytes(t, filepath.Join(host, "factory/bin/factory"), pythonScript)
		hasBytes(t, filepath.Join(host, "factory/config.toml"), []byte("# the old config\n"))
	})

	t.Run("a Go tarball that is not pinned is refused", func(t *testing.T) {
		tb := filepath.Join(t.TempDir(), pinnedGo(t)+".linux-amd64.tar.gz")
		mkfile(t, tb, "not go\n")
		host := t.TempDir()
		out, code := deploy(t, "--config", configDir(t), "--host", "local:"+host, "--prebuilt", bins, "--install-go",
			"FACTORY_GO_TARBALL="+tb)
		if code == 0 {
			t.Fatalf("deploy took an unpinned toolchain:\n%s", out)
		}
		mentions(t, out, "is not pinned")
		absent(t, host, "factory")
	})

	t.Run("dry run deploys into a temporary home and lists it", func(t *testing.T) {
		out := deployOK(t, "--config", configDir(t), "--dry-run", "--prebuilt", bins)
		mentions(t, out, "factory/bin/factory\n", "factory/bin/foreman\n", "factory/bin/pi-run\n",
			".config/systemd/user/foreman.service\n", "dry run: deployed into ")
		dir := ""
		for _, l := range strings.Split(out, "\n") {
			if rest, ok := strings.CutPrefix(l, "dry run: deployed into "); ok {
				dir = strings.TrimSpace(rest)
			}
		}
		if dir == "" || !strings.HasPrefix(dir, os.TempDir()) {
			t.Fatalf("dry run named no temporary directory:\n%s", out)
		}
		os.RemoveAll(dir)
	})

	t.Run("over ssh it applies the sandbox, restarts Foreman and wires the backup timers", func(t *testing.T) {
		host, helper := pythonHost(t), pythonHelper(t)
		fakes := fakeHostTools(t, map[string]string{"factory@box": host, "factory@helper": helper}, true)
		out := deployOK(t, "--config", configDir(t), "--host", "factory@box", "--prebuilt", bins,
			"--helper", "factory@helper", "PATH="+fakes.bin+":"+os.Getenv("PATH"), "FAKE_LOG="+fakes.log,
			"FAKE_SSH_HOMES="+fakes.homes)
		calls := readFile(t, fakes.log)
		mentions(t, calls,
			"systemctl --user stop foreman\n", // the state moved while it ran
			"systemctl --user daemon-reload\n",
			"systemctl --user restart foreman\n",
			"systemctl --user enable --now factory-backup.timer factory-backup-verify.timer\n",
			"-p ProtectHome=read-only",
			"-p InaccessiblePaths=-"+host+"/.config/factory/github-app.pem -"+host+"/.config/factory/claude-oauth-token"+
				" -"+host+"/.config/factory/anthropic-api-key -"+host+"/.config/factory/backup-key -"+host+"/.config/factory/r2-credentials",
			"/bus -", "/systemd -", "/podman ",
			"cat "+host+"/.config/factory/github-app.pem\n",
			"systemd-run --user --wait --pipe --quiet --collect true\n",
			"systemctl --user enable --now factory-wake.path\n",
			"systemctl --user enable --now factory-screens.path\n",
			"curl -fsS --max-time 3 http://127.0.0.1:8790/healthz\n")
		mentions(t, out, "the App key is out of Foreman's reach", "the user manager is out of Foreman's reach", "Foreman's /healthz: ok")
		if strings.Contains(calls, "%t") || strings.Contains(calls, "%h") {
			t.Errorf("a specifier reached systemd-run unexpanded:\n%s", calls)
		}
		for _, u := range []string{"factory-wake.path", "factory-wake.service", "factory-screens.path", "factory-screens.service"} {
			sameFile(t, filepath.Join(host, ".config/systemd/user", u), filepath.Join("systemd", u))
		}
		contains(t, filepath.Join(host, ".config/systemd/user/foreman.service"), "# sandbox:begin", "ProtectHome=read-only")
		for _, u := range []string{"factory-backup.service", "factory-backup.timer", "factory-backup-verify.service", "factory-backup-verify.timer"} {
			sameFile(t, filepath.Join(host, ".config/systemd/user", u), filepath.Join("systemd", u))
		}
		for _, f := range foremanState {
			present(t, host, "factory/foreman-state/"+f)
		}
		if got, err := os.Readlink(filepath.Join(helper, "factory/bin/factory")); err != nil || got != "factory-remote" {
			t.Errorf("helper bin/factory links to %q (%v)", got, err)
		}
		// the helper's deploy runs no systemd command
		if strings.Contains(calls, helper) {
			t.Errorf("the helper's deploy touched systemd:\n%s", calls)
		}
	})

	t.Run("a user manager that cannot sandbox gets the unit without it, with a warning", func(t *testing.T) {
		host := pythonHost(t)
		fakes := fakeHostTools(t, map[string]string{"factory@box": host}, false)
		out := deployOK(t, "--config", configDir(t), "--host", "factory@box", "--prebuilt", bins,
			"PATH="+fakes.bin+":"+os.Getenv("PATH"), "FAKE_LOG="+fakes.log, "FAKE_SSH_HOMES="+fakes.homes)
		mentions(t, out, "cannot apply Foreman's sandbox")
		unit := readFile(t, filepath.Join(host, ".config/systemd/user/foreman.service"))
		for _, s := range []string{"sandbox:", "ProtectHome", "PrivateUsers", "InaccessiblePaths"} {
			if strings.Contains(unit, s) {
				t.Errorf("the unsandboxed unit still has %q", s)
			}
		}
		if !strings.Contains(unit, "MemoryMax=128M") || !strings.Contains(unit, "NoNewPrivileges=yes") {
			t.Errorf("the unsandboxed unit lost the rest:\n%s", unit)
		}
	})

	// D18: a user manager that cannot apply the whole sandbox still keeps
	// the credentials and itself out of Foreman's reach with what it can
	// apply: InaccessiblePaths with PrivateUsers, then InaccessiblePaths alone.
	t.Run("a user manager that applies only part of the sandbox gets that part", func(t *testing.T) {
		for _, tc := range []struct {
			refuse string
			keep   []string
			drop   []string
		}{
			{"ProtectSystem=strict", []string{"PrivateUsers=yes", "InaccessiblePaths=-"}, []string{"ProtectSystem", "ProtectHome", "ReadWritePaths", "PrivateTmp"}},
			{"PrivateUsers=yes", []string{"InaccessiblePaths=-"}, []string{"PrivateUsers", "ProtectSystem", "ProtectHome", "ReadWritePaths", "PrivateTmp"}},
		} {
			host := pythonHost(t)
			fakes := fakeHostTools(t, map[string]string{"factory@box": host}, true)
			out := deployOK(t, "--config", configDir(t), "--host", "factory@box", "--prebuilt", bins,
				"PATH="+fakes.bin+":"+os.Getenv("PATH"), "FAKE_LOG="+fakes.log, "FAKE_SSH_HOMES="+fakes.homes,
				"FAKE_SANDBOX_REFUSE="+tc.refuse)
			mentions(t, out, "cannot apply Foreman's whole sandbox", "the App key is out of Foreman's reach",
				"the user manager is out of Foreman's reach")
			unit := readFile(t, filepath.Join(host, ".config/systemd/user/foreman.service"))
			block := strings.Join(sandboxBlock(t, unit), "\n")
			for _, s := range tc.keep {
				if !strings.Contains(block, s) {
					t.Errorf("refusing %s: the unit lacks %q:\n%s", tc.refuse, s, block)
				}
			}
			if !strings.Contains(block, "%t/bus") || !strings.Contains(block, "github-app.pem") {
				t.Errorf("refusing %s: the credentials or the bus are not inaccessible:\n%s", tc.refuse, block)
			}
			for _, s := range tc.drop {
				if strings.Contains(block, s) {
					t.Errorf("refusing %s: the unit still has %q:\n%s", tc.refuse, s, block)
				}
			}
			if !strings.Contains(unit, "MemoryMax=128M") {
				t.Errorf("refusing %s: the unit lost the rest:\n%s", tc.refuse, unit)
			}
		}
	})

	// factory deploy run on the master: deploy.sh from a bare mirror of
	// core, staged in a directory of the runner's, with real systemd.
	t.Run("--host self installs into this home, staged where the runner says", func(t *testing.T) {
		need(t, "flock")
		host := pythonHost(t)
		mirror := bareMirror(t)
		stage := filepath.Join(t.TempDir(), "work/stage")
		fakes := fakeHostTools(t, nil, true)
		out := deployOK(t, "--config", configDir(t), "--host", "self", "--core", mirror, "--commit", commit, "--prebuilt", bins,
			"HOME="+host, "FACTORY_DEPLOY_SELF=1", "FACTORY_DEPLOY_DIR="+stage,
			"PATH="+fakes.bin+":"+os.Getenv("PATH"), "FAKE_LOG="+fakes.log)
		for _, b := range []string{"factory", "foreman", "factory-remote"} {
			sameFile(t, filepath.Join(host, "factory/bin", b), filepath.Join(bins, b))
		}
		contains(t, filepath.Join(host, "factory/config.toml"), "[foreman]")
		hasBytes(t, filepath.Join(host, "factory/src", commit, "COMMIT"), []byte(commit+"\n"))
		absent(t, host, "factory/.deploy")
		if _, err := os.Stat(stage); !os.IsNotExist(err) {
			t.Errorf("finish left the stage: %v", err)
		}
		// real systemd, not local mode
		if strings.Contains(out, "would run:") {
			t.Errorf("--host self ran in local mode:\n%s", out)
		}
		mentions(t, readFile(t, fakes.log), "systemctl --user daemon-reload\n", "systemctl --user restart foreman\n", "/healthz\n")
		mentions(t, out, "deploy.sh: self: ")
	})

	// deploys/config-commit is what P3's stale-checkout guard reads
	t.Run("a deploy from a config checkout records its commit; one from a plain directory forgets it", func(t *testing.T) {
		host := pythonHost(t)
		repo := t.TempDir()
		cfg := filepath.Join(repo, "config")
		copyFile(t, "examples/config/config.toml", filepath.Join(cfg, "config.toml"))
		for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"},
			{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "config"}} {
			if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		deployOK(t, "--config", cfg, "--host", "local:"+host, "--prebuilt", bins)
		hasBytes(t, filepath.Join(host, "factory/deploys/config-commit"), head)
		// uncommitted changes ship with a warning, and HEAD is recorded
		appendFile(t, filepath.Join(cfg, "config.toml"), "\n# an edit\n")
		out := deployOK(t, "--config", cfg, "--host", "local:"+host, "--prebuilt", bins)
		mentions(t, out, "has uncommitted changes: they ship, and deploys/config-commit records HEAD")
		hasBytes(t, filepath.Join(host, "factory/deploys/config-commit"), head)
		// a plain directory cannot be checked against what the host runs:
		// refused, and with --allow-older the host forgets its config commit
		out, code := deploy(t, "--config", configDir(t), "--host", "local:"+host, "--prebuilt", bins)
		if code != 1 {
			t.Fatalf("exit %d, want 1\n%s", code, out)
		}
		mentions(t, out, "is not a git checkout: local:"+host+" runs config "+strings.TrimSpace(string(head))[:12]+
			", which it cannot check; pass --allow-older")
		hasBytes(t, filepath.Join(host, "factory/deploys/config-commit"), head)
		deployOK(t, "--config", configDir(t), "--host", "local:"+host, "--prebuilt", bins, "--allow-older")
		absent(t, host, "factory/deploys/config-commit")
		// and with no config commit recorded, a plain directory deploys
		deployOK(t, "--config", configDir(t), "--host", "local:"+host, "--prebuilt", bins)
	})

	t.Run("--host self leaves config-commit to the runner", func(t *testing.T) {
		need(t, "flock")
		host := pythonHost(t)
		mkfile(t, filepath.Join(host, "factory/deploys/config-commit"), "0123456789abcdef0123456789abcdef01234567\n")
		fakes := fakeHostTools(t, nil, true)
		deployOK(t, "--config", configDir(t), "--host", "self", "--core", bareMirror(t), "--commit", commit, "--prebuilt", bins,
			"HOME="+host, "FACTORY_DEPLOY_SELF=1", "FACTORY_DEPLOY_DIR="+filepath.Join(t.TempDir(), "stage"),
			"PATH="+fakes.bin+":"+os.Getenv("PATH"), "FAKE_LOG="+fakes.log)
		hasBytes(t, filepath.Join(host, "factory/deploys/config-commit"), []byte("0123456789abcdef0123456789abcdef01234567\n"))
	})

	t.Run("--host self is only for factory deploy run, and relays to no helper", func(t *testing.T) {
		host := pythonHost(t)
		stage := filepath.Join(t.TempDir(), "stage")
		for _, tc := range []struct {
			args []string
			want string
		}{
			{[]string{"FACTORY_DEPLOY_DIR=" + stage}, "--host self is for factory deploy run on the master"},
			{[]string{"FACTORY_DEPLOY_SELF=1", "FACTORY_DEPLOY_DIR=" + stage, "--helper", "local:" + t.TempDir()}, "--helper with --host self: the master sends its helpers the install verb itself"},
			{[]string{"FACTORY_DEPLOY_SELF=1"}, "FACTORY_DEPLOY_DIR"},
		} {
			args := append([]string{"--config", configDir(t), "--host", "self", "--commit", commit, "--prebuilt", bins, "HOME=" + host}, tc.args...)
			out, code := deploy(t, args...)
			if code == 0 {
				t.Fatalf("deploy %v ran:\n%s", tc.args, out)
			}
			mentions(t, out, tc.want)
			hasBytes(t, filepath.Join(host, "factory/bin/factory"), pythonScript)
			absent(t, host, "factory/.deploy", "factory/src")
			if _, err := os.Stat(stage); !os.IsNotExist(err) {
				t.Errorf("%v: made the stage", tc.args)
			}
		}
	})

	t.Run("--core takes the commit from another git directory, and needs --commit", func(t *testing.T) {
		mirror := bareMirror(t)
		host := t.TempDir()
		out, code := deploy(t, "--config", configDir(t), "--host", "local:"+host, "--core", mirror, "--prebuilt", bins)
		if code != 2 {
			t.Fatalf("--core without --commit: exit %d\n%s", code, out)
		}
		mentions(t, out, "--core needs --commit")
		out, code = deploy(t, "--config", configDir(t), "--host", "local:"+host, "--core", t.TempDir(), "--commit", commit, "--prebuilt", bins)
		if code == 0 {
			t.Fatalf("--core of no repository deployed:\n%s", out)
		}
		mentions(t, out, "is not a commit of")
		absent(t, host, "factory")
		deployOK(t, "--config", configDir(t), "--host", "local:"+host, "--core", mirror, "--commit", commit, "--prebuilt", bins)
		hasBytes(t, filepath.Join(host, "factory/src", commit, "COMMIT"), []byte(commit+"\n"))
	})

	t.Run("a desktop deploy stops at prepare while a self-deploy holds deploys/.lock", func(t *testing.T) {
		need(t, "flock")
		host := pythonHost(t)
		lockPath := filepath.Join(host, "factory/deploys/.lock")
		mkfile(t, lockPath, "")
		f, err := os.Open(lockPath)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		out, code := deploy(t, "--config", configDir(t), "--host", "local:"+host, "--prebuilt", bins)
		if code == 0 {
			t.Fatalf("deploy ran beside a self-deploy:\n%s", out)
		}
		mentions(t, out, "a self-deploy is running here (factory deploy status): nothing changed")
		hasBytes(t, filepath.Join(host, "factory/bin/factory"), pythonScript)
		absent(t, host, "factory/src/"+commit, "factory/.deploy")
		// once it is free, the same deploy goes through
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		deployOK(t, "--config", configDir(t), "--host", "local:"+host, "--prebuilt", bins)
	})

	// the stale-checkout guard: a deploy never rolls the host back to a core
	// or a config that lacks what it runs
	t.Run("core lacking the running commit refused", func(t *testing.T) {
		host := pythonHost(t)
		clone, ahead := coreAhead(t)
		runs(t, host, ahead)
		out, code := deploy(t, "--config", configDir(t), "--host", "local:"+host, "--core", clone, "--commit", commit, "--prebuilt", bins)
		if code != 1 {
			t.Fatalf("exit %d, want 1\n%s", code, out)
		}
		mentions(t, out, "core "+commit+" does not contain what local:"+host+" runs ("+ahead+"): update core/ "+
			"(git submodule update --remote core) or pass --allow-older")
		hasBytes(t, filepath.Join(host, "factory/bin/factory"), pythonScript)
		absent(t, host, "factory/src")
	})

	t.Run("--allow-older deploys", func(t *testing.T) {
		host := pythonHost(t)
		clone, ahead := coreAhead(t)
		runs(t, host, ahead)
		deployOK(t, "--config", configDir(t), "--host", "local:"+host, "--core", clone, "--commit", commit, "--prebuilt", bins, "--allow-older")
		sameFile(t, filepath.Join(host, "factory/bin/factory"), filepath.Join(bins, "factory"))
	})

	// factory deploy start REV takes any commit on core main, older ones too
	t.Run("--host self refuses a core lacking the running commit, unless --allow-older", func(t *testing.T) {
		need(t, "flock")
		host := pythonHost(t)
		clone, ahead := coreAhead(t)
		runs(t, host, ahead)
		fakes := fakeHostTools(t, nil, true)
		args := []string{"--config", configDir(t), "--host", "self", "--core", clone, "--commit", commit, "--prebuilt", bins,
			"HOME=" + host, "FACTORY_DEPLOY_SELF=1", "FACTORY_DEPLOY_DIR=" + filepath.Join(t.TempDir(), "stage"),
			"PATH=" + fakes.bin + ":" + os.Getenv("PATH"), "FAKE_LOG=" + fakes.log}
		out, code := deploy(t, args...)
		if code != 1 {
			t.Fatalf("exit %d, want 1\n%s", code, out)
		}
		mentions(t, out, "core "+commit+" does not contain what self runs ("+ahead+")")
		hasBytes(t, filepath.Join(host, "factory/bin/factory"), pythonScript)
		absent(t, host, "factory/src")
		deployOK(t, append(args, "--allow-older")...)
		sameFile(t, filepath.Join(host, "factory/bin/factory"), filepath.Join(bins, "factory"))
	})

	t.Run("descendant core deploys", func(t *testing.T) {
		host := pythonHost(t)
		clone, ahead := coreAhead(t)
		runs(t, host, commit)
		deployOK(t, "--config", configDir(t), "--host", "local:"+host, "--core", clone, "--commit", ahead, "--prebuilt", bins)
		sameFile(t, filepath.Join(host, "factory/bin/factory"), filepath.Join(bins, "factory"))
		// and the same commit again is no rollback
		deployOK(t, "--config", configDir(t), "--host", "local:"+host, "--core", clone, "--commit", ahead, "--prebuilt", bins)
	})

	t.Run("running commit unknown locally refused", func(t *testing.T) {
		host := pythonHost(t)
		runs(t, host, "0123456789ab")
		out, code := deploy(t, "--config", configDir(t), "--host", "local:"+host, "--prebuilt", bins)
		if code != 1 {
			t.Fatalf("exit %d, want 1\n%s", code, out)
		}
		mentions(t, out, "does not contain what local:"+host+" runs (0123456789ab)")
		hasBytes(t, filepath.Join(host, "factory/bin/factory"), pythonScript)
	})

	t.Run("config lacking the running config commit refused", func(t *testing.T) {
		host := pythonHost(t)
		cfg, repo := configRepo(t)
		appendFile(t, filepath.Join(cfg, "config.toml"), "\n# a newer config\n")
		gitIn(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-am", "newer")
		newer := gitIn(t, repo, "rev-parse", "HEAD")
		deployOK(t, "--config", cfg, "--host", "local:"+host, "--prebuilt", bins)
		hasBytes(t, filepath.Join(host, "factory/deploys/config-commit"), []byte(newer+"\n"))
		// a checkout that lags behind what the host runs
		gitIn(t, repo, "checkout", "-q", "HEAD~1")
		older := gitIn(t, repo, "rev-parse", "HEAD")
		out, code := deploy(t, "--config", cfg, "--host", "local:"+host, "--prebuilt", bins)
		if code != 1 {
			t.Fatalf("exit %d, want 1\n%s", code, out)
		}
		mentions(t, out, "config "+older[:12]+" does not contain what local:"+host+" runs ("+newer[:12]+"): pull the config checkout "+
			"or pass --allow-older")
		hasBytes(t, filepath.Join(host, "factory/deploys/config-commit"), []byte(newer+"\n"))
		deployOK(t, "--config", cfg, "--host", "local:"+host, "--prebuilt", bins, "--allow-older")
		hasBytes(t, filepath.Join(host, "factory/deploys/config-commit"), []byte(older+"\n"))
	})

	t.Run("fresh host deploys and records config", func(t *testing.T) {
		host := pythonHost(t)
		cfg, repo := configRepo(t)
		deployOK(t, "--config", cfg, "--host", "local:"+host, "--prebuilt", bins)
		hasBytes(t, filepath.Join(host, "factory/deploys/config-commit"), []byte(gitIn(t, repo, "rev-parse", "HEAD")+"\n"))
		// the same core and config again: the host runs them, so no rollback
		deployOK(t, "--config", cfg, "--host", "local:"+host, "--prebuilt", bins)
	})

	t.Run("usage errors exit 2", func(t *testing.T) {
		for _, args := range [][]string{
			{"--host", "local:/nonexistent"},
			{"--config", "x", "--host", "h", "--frobnicate"},
			{"--config", "x", "--host", "h", "--build", "elsewhere"},
			{"--config", "x"},
		} {
			if out, code := deploy(t, args...); code != 2 {
				t.Errorf("deploy %v: exit %d, want 2\n%s", args, code, out)
			}
		}
	})
}

// The Foreman unit holds the §6.2 sandbox between markers deploy.sh reads,
// with optional paths marked "-" so a missing key never stops Foreman.
func TestForemanUnitSandbox(t *testing.T) {
	b, err := os.ReadFile("systemd/foreman.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(b)
	block := sandboxBlock(t, unit)
	want := []string{
		"PrivateUsers=yes",
		"PrivateTmp=yes",
		"ProtectSystem=strict",
		"ProtectHome=read-only",
		"ReadWritePaths=%h/factory/board %h/factory/logs %h/factory/foreman-state",
		"InaccessiblePaths=-%h/.config/factory/github-app.pem -%h/.config/factory/claude-oauth-token " +
			"-%h/.config/factory/anthropic-api-key -%h/.config/factory/backup-key -%h/.config/factory/r2-credentials",
		// the user manager (and podman's API socket) would run anything outside the sandbox
		"InaccessiblePaths=-%t/bus -%t/systemd -%t/podman",
	}
	if !equal(block, want) {
		t.Errorf("sandbox block:\n%s\nwant:\n%s", strings.Join(block, "\n"), strings.Join(want, "\n"))
	}
	for _, s := range []string{"MemoryMax=128M", "UMask=0077", "NoNewPrivileges=yes",
		"ExecStart=%h/factory/bin/foreman --root %h/factory --listen {{foreman_listen}} " +
			"--secret-file %h/factory/foreman-state/foreman-secret --api-tokens %h/.config/factory/api-tokens.json {{foreman_flags}}"} {
		if !strings.Contains(unit, s+"\n") {
			t.Errorf("foreman.service lacks %q", s)
		}
	}
	// the tick unlinks .tick.again, so a bind mount of it would go stale:
	// the woken tick sets the flag itself
	for _, l := range strings.Split(unit, "\n") {
		if !strings.HasPrefix(l, "#") && strings.Contains(l, ".tick.again") {
			t.Errorf("foreman.service mounts .tick.again: %s", l)
		}
	}
}

// Foreman's wake (§5.5, §6.2): a path unit on foreman-state/wake starts a
// user-unit tick, outside Foreman's sandbox.
func TestWakeUnits(t *testing.T) {
	for name, want := range map[string][]string{
		"systemd/factory-wake.path":    {"PathModified=%h/factory/foreman-state/wake\n", "Unit=factory-wake.service\n", "WantedBy=default.target"},
		"systemd/factory-wake.service": {"Type=oneshot", "ExecStart=%h/factory/bin/factory tick\n", "TimeoutStartSec=45min"},
	} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Error(err)
			continue
		}
		for _, s := range want {
			if !strings.Contains(string(b), s) {
				t.Errorf("%s lacks %q", name, s)
			}
		}
		if strings.Contains(string(b), "User=") {
			t.Errorf("%s is a user unit: no User=", name)
		}
	}
}

// The screen relay: a path unit on foreman-state/watching starts `factory
// screens`, a user unit outside Foreman's sandbox, which exits by itself.
func TestScreensUnits(t *testing.T) {
	for name, want := range map[string][]string{
		"systemd/factory-screens.path":    {"PathModified=%h/factory/foreman-state/watching\n", "Unit=factory-screens.service\n", "WantedBy=default.target"},
		"systemd/factory-screens.service": {"ExecStart=%h/factory/bin/factory screens\n"},
	} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Error(err)
			continue
		}
		for _, s := range want {
			if !strings.Contains(string(b), s) {
				t.Errorf("%s lacks %q", name, s)
			}
		}
		for _, bad := range []string{"User=", "Restart="} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s has %q", name, bad)
			}
		}
	}
	// the service is started by the path unit only, never at boot
	if b, _ := os.ReadFile("systemd/factory-screens.service"); strings.Contains(string(b), "[Install]") {
		t.Errorf("factory-screens.service is installable on its own")
	}
}

// The backup timers (§15.2) are user units that run the factory binary.
func TestBackupUnits(t *testing.T) {
	for name, want := range map[string][]string{
		"systemd/factory-backup.service":        {"Type=oneshot", "ExecStart=%h/factory/bin/factory backup\n"},
		"systemd/factory-backup.timer":          {"OnCalendar=", "Persistent=true", "WantedBy=timers.target"},
		"systemd/factory-backup-verify.service": {"Type=oneshot", "ExecStart=%h/factory/bin/factory backup verify\n"},
		"systemd/factory-backup-verify.timer":   {"OnCalendar=Sun", "Persistent=true", "WantedBy=timers.target"},
	} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Error(err)
			continue
		}
		for _, s := range want {
			if !strings.Contains(string(b), s) {
				t.Errorf("%s lacks %q", name, s)
			}
		}
		if strings.Contains(string(b), "User=") {
			t.Errorf("%s is a user unit: no User=", name)
		}
	}
}

// FACTORY_SANDBOX_PROBE=1 applies the unit's sandbox through this
// machine's user manager (systemd-run --user), as deploy.sh does on the
// host before it installs the unit: the App key cannot be opened, the board
// can be written and the rest of the home cannot.
func TestForemanSandboxOnThisSystemd(t *testing.T) {
	if os.Getenv("FACTORY_SANDBOX_PROBE") != "1" {
		t.Skip("set FACTORY_SANDBOX_PROBE=1 to run the unit's sandbox through systemd-run --user")
	}
	need(t, "systemd-run")
	b, err := os.ReadFile("systemd/foreman.service")
	if err != nil {
		t.Fatal(err)
	}
	// not under /tmp: PrivateTmp hides it
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp(cache, "foreman-sandbox-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	for _, d := range []string{"factory/board", "factory/logs", "factory/foreman-state", "factory/bin"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mkfile(t, filepath.Join(home, ".config/factory/github-app.pem"), "key\n")
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		t.Skip("no XDG_RUNTIME_DIR: no user manager here")
	}
	argv := []string{"--user", "--wait", "--pipe", "--quiet", "--collect"}
	for _, l := range sandboxBlock(t, string(b)) {
		argv = append(argv, "-p", strings.ReplaceAll(strings.ReplaceAll(l, "%h", home), "%t", rt))
	}
	script := fmt.Sprintf(`! cat %[1]s/.config/factory/github-app.pem 2>/dev/null || exit 11
touch %[1]s/factory/board/x || exit 12
touch %[1]s/factory/foreman-state/x || exit 13
! touch %[1]s/factory/bin/x 2>/dev/null || exit 14
! touch %[1]s/factory/x 2>/dev/null || exit 15
! systemd-run --user --wait --pipe --quiet --collect true 2>/dev/null || exit 16
`, home)
	out, err := exec.Command("systemd-run", append(argv, "sh", "-c", script)...).CombinedOutput()
	if err != nil {
		t.Fatalf("the sandbox does not hold (exit 11: key readable, 12/13: state not writable, 14/15: home writable, 16: the user manager reachable): %v\n%s", err, out)
	}
}

// ------------------------------------------------------------- helpers

type hostTools struct{ bin, log, homes string }

// fakeHostTools is ssh into local homes (ssh runs its command through the
// login shell, as here), and systemctl, systemd-run and curl that log their
// arguments. systemd-run applies a sandbox only if sandboxOK, and under it
// cat cannot read anything.
func fakeHostTools(t *testing.T, homes map[string]string, sandboxOK bool) hostTools {
	t.Helper()
	d := t.TempDir()
	ht := hostTools{bin: filepath.Join(d, "bin"), log: filepath.Join(d, "calls.log"), homes: filepath.Join(d, "homes")}
	var m strings.Builder
	for h, dir := range homes {
		fmt.Fprintf(&m, "%s=%s\n", h, dir)
	}
	mkfile(t, ht.homes, m.String())
	mkfile(t, ht.log, "")
	ok := "1"
	if sandboxOK {
		ok = "0"
	}
	mkexec(t, filepath.Join(ht.bin, "ssh"), []byte(`#!/bin/sh
while [ $# -gt 0 ]; do case "$1" in -o) shift 2 ;; -*) shift ;; *) break ;; esac; done
home=$(awk -F= -v h="$1" '$1 == h { print $2 }' "$FAKE_SSH_HOMES")
[ -n "$home" ] || { echo "ssh: unknown host $1" >&2; exit 255; }
shift
cd "$home" && HOME=$home exec bash -c "$*"
`))
	mkexec(t, filepath.Join(ht.bin, "systemctl"), []byte(`#!/bin/sh
echo "systemctl $*" >>"$FAKE_LOG"
exit 0
`))
	mkexec(t, filepath.Join(ht.bin, "systemd-run"), []byte(`#!/bin/sh
echo "systemd-run $*" >>"$FAKE_LOG"
[ `+ok+` = 0 ] || exit 1
# a user manager that cannot apply some properties (FAKE_SANDBOX_REFUSE, space-separated)
for a in "$@"; do for r in ${FAKE_SANDBOX_REFUSE:-}; do [ "$a" = "$r" ] && exit 1; done; done
# under the sandbox cat reads nothing and the user manager is out of reach
first=1
for a in "$@"; do
	[ "$a" = cat ] && exit 1
	[ $first = 0 ] && [ "$a" = systemd-run ] && exit 1
	first=0
done
exit 0
`))
	mkexec(t, filepath.Join(ht.bin, "curl"), []byte(`#!/bin/sh
echo "curl $*" >>"$FAKE_LOG"
printf 'ok\n'
`))
	return ht
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func need(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
}

// sandboxBlock is the Key=Value lines between "# sandbox:begin" and
// "# sandbox:end".
func sandboxBlock(t *testing.T, unit string) []string {
	t.Helper()
	var lines []string
	in, seen := false, false
	sc := bufio.NewScanner(strings.NewReader(unit))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(l, "# sandbox:begin"):
			in, seen = true, true
		case strings.HasPrefix(l, "# sandbox:end"):
			in = false
		case in && l != "" && !strings.HasPrefix(l, "#"):
			lines = append(lines, l)
		}
	}
	if !seen || in {
		t.Fatalf("foreman.service has no closed # sandbox:begin / # sandbox:end block")
	}
	return lines
}

// buildBinaries builds the three binaries for this machine, with a BUILD
// file as build.sh writes it.
func buildBinaries(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go")
	cmd := exec.Command(gobin, "build", "-trimpath",
		"-ldflags=-X github.com/stevegeek/agent-factory/internal/buildinfo.Version=s24-test", "-o", dir+"/", "./cmd/...")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	var build strings.Builder
	for _, b := range []string{"factory", "foreman", "factory-remote"} {
		data, err := os.ReadFile(filepath.Join(dir, b))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&build, "%s  %s\n", hex.EncodeToString(sum[:]), b)
	}
	// the commit line names the commit the tests deploy, as build.sh's
	// does: the stale-checkout guard reads it back from the host
	fmt.Fprintf(&build, "commit %s go %s\n", headCommit(t), runtime.Version())
	mkfile(t, filepath.Join(dir, "BUILD"), build.String())
	return dir
}

// coreAhead is a clone of this repository with one commit on top of HEAD,
// and that commit.
func coreAhead(t *testing.T) (string, string) {
	t.Helper()
	d := filepath.Join(t.TempDir(), "core")
	if out, err := exec.Command("git", "clone", "--quiet", "--no-local", ".", d).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	gitIn(t, d, "checkout", "-q", "--detach", headCommit(t))
	gitIn(t, d, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "ahead")
	return d, gitIn(t, d, "rev-parse", "--short=12", "HEAD")
}

// runs makes the host's installed build say it is core12's.
func runs(t *testing.T, host, core12 string) {
	t.Helper()
	mkfile(t, filepath.Join(host, "factory/bin/BUILD"), "0000  factory\ncommit "+core12+" go go1.0\n")
}

// configRepo is configDir as the config subdirectory of a git checkout with
// one commit, and that checkout.
func configRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	cfg := filepath.Join(repo, "config")
	copyFile(t, "examples/config/config.toml", filepath.Join(cfg, "config.toml"))
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "config")
	return cfg, repo
}

// gitIn runs git in dir and gives its output, trimmed.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// bareMirror is a bare clone of this repository, as factory deploy run
// keeps core: no work tree, so deploy.sh must name the commit.
func bareMirror(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "core.git")
	if out, err := exec.Command("git", "clone", "--quiet", "--bare", "--no-local", ".", d).CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v\n%s", err, out)
	}
	return d
}

func headCommit(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--verify", "--short=12", "HEAD^{commit}").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func pinnedGo(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "toolchain" {
			return f[1]
		}
	}
	t.Fatal("go.mod has no toolchain line")
	return ""
}

// pythonHost is a dispatcher's home before its first Go deploy.
func pythonHost(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	for rel, data := range keptFiles {
		mkfile(t, filepath.Join(h, rel), data)
	}
	mkexec(t, filepath.Join(h, "factory/bin/factory"), pythonScript)
	mkexec(t, filepath.Join(h, "factory/bin/factory-remote"), pythonScript)
	mkexec(t, filepath.Join(h, "factory/bin/render"), pythonScript)
	mkexec(t, filepath.Join(h, "factory/bin/foreman"), oldForeman)
	mkfile(t, filepath.Join(h, "factory/patch_provision.py"), string(pythonScript))
	mkfile(t, filepath.Join(h, "factory/config.toml"), "# the old config\n")
	mkfile(t, filepath.Join(h, "factory/context/gone/INDEX.md"), "# removed from the config\n")
	for _, f := range foremanState {
		mkfile(t, filepath.Join(h, ".config/factory", f), "state "+f+"\n")
	}
	return h
}

// pythonHelper is a helper host's home before its first Go deploy.
func pythonHelper(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	mkexec(t, filepath.Join(h, "factory/bin/factory"), pythonScript)
	mkexec(t, filepath.Join(h, "factory/bin/factory-remote"), pythonScript)
	mkexec(t, filepath.Join(h, "factory/bin/render"), pythonScript)
	mkexec(t, filepath.Join(h, "factory/bin/foreman"), oldForeman)
	mkfile(t, filepath.Join(h, "factory/patch_provision.py"), string(pythonScript))
	mkfile(t, filepath.Join(h, "factory/config.toml"), "# the helper's old config\n")
	mkfile(t, filepath.Join(h, "factory/jobs/j1/run.log"), "a job\n")
	return h
}

// configDir is a config repo: the example config, rules.md and context.
func configDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	copyFile(t, "examples/config/config.toml", filepath.Join(d, "config.toml"))
	mkfile(t, filepath.Join(d, "rules.md"), "Extra rule from the config.\n")
	mkfile(t, filepath.Join(d, "context/demo/INDEX.md"), "# demo\n")
	return d
}

func deploy(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "FACTORY_") {
			env = append(env, kv)
		}
	}
	// NAME=value arguments are the environment (the last one for a name wins)
	isEnv := func(a string) bool {
		k, _, ok := strings.Cut(a, "=")
		return ok && k != "" && strings.Trim(k, "ABCDEFGHIJKLMNOPQRSTUVWXYZ_0123456789") == ""
	}
	var argv []string
	for _, a := range args {
		if isEnv(a) {
			env = append(env, a)
			continue
		}
		argv = append(argv, a)
	}
	cmd := exec.Command("bash", append([]string{"deploy.sh"}, argv...)...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		return out.String(), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return out.String(), ee.ExitCode()
	}
	t.Fatalf("deploy.sh: %v", err)
	return "", 0
}

func deployOK(t *testing.T, args ...string) string {
	t.Helper()
	out, code := deploy(t, args...)
	if code != 0 {
		t.Fatalf("deploy %v: exit %d\n%s", args, code, out)
	}
	return out
}

func mkfile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkexec(t *testing.T, path string, data []byte) {
	t.Helper()
	mkfile(t, path, string(data))
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	mkfile(t, to, string(b))
}

func sameFile(t *testing.T, got, want string) {
	t.Helper()
	w, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	hasBytes(t, got, w)
}

func hasBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("%v", err)
		return
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s: %d bytes, not the %d expected", path, len(got), len(want))
	}
}

func contains(t *testing.T, path string, subs ...string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("%v", err)
		return
	}
	for _, s := range subs {
		if !strings.Contains(string(b), s) {
			t.Errorf("%s lacks %q", path, s)
		}
	}
}

func mentions(t *testing.T, out string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(out, s) {
			t.Errorf("deploy output lacks %q:\n%s", s, out)
		}
	}
}

func present(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s missing: %v", rel, err)
		}
	}
}

func absent(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if _, err := os.Lstat(filepath.Join(root, rel)); err == nil {
			t.Errorf("%s should not exist", rel)
		}
	}
}

func executable(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Error(err)
		return
	}
	if st.Mode().Perm() != 0o755 {
		t.Errorf("%s has mode %o, want 755", path, st.Mode().Perm())
	}
}

func mode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Error(err)
		return
	}
	if st.Mode().Perm() != want {
		t.Errorf("%s has mode %o, want %o", path, st.Mode().Perm(), want)
	}
}

func link(t *testing.T, path, target string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil || got != target {
		t.Errorf("%s links to %q (%v), want %q", path, got, err, target)
	}
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range des {
		names = append(names, d.Name())
	}
	sort.Strings(names)
	return names
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sort.Strings(a)
	b = append([]string(nil), b...)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
