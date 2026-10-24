package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jobproto"
)

const buildC = "35956371f2ff"

// buildFixture: a builder in config and the source deploy.sh left for buildC.
func buildFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.env.WriteConfig(t, "[build]\nhost = \"helpera\"\n\n[hosts.helpera]\nssh = \"factory@helpera\"\n")
	f.env.WriteFile(t, "src/"+buildC+"/go.mod", "module x\n\ngo 1.25\n\ntoolchain go1.27.1\n")
	f.env.WriteFile(t, "src/"+buildC+"/build.sh", "#!/bin/bash\n")
	return f
}

// builderAnswer is a good answer for buildC.
func builderAnswer(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	files := []jobproto.File{{Name: "factory", Data: []byte("f")}, {Name: "foreman", Data: []byte("m")}, {Name: "factory-remote", Data: []byte("r")}}
	for _, f := range files {
		sum := sha256.Sum256(f.Data)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), f.Name)
	}
	fmt.Fprintf(&b, "commit %s go go1.27.1\n", buildC)
	out, err := jobproto.TarFlat(append(files, jobproto.File{Name: "BUILD", Data: []byte(b.String())}), time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// sshes is the argv of each ssh the fake ran.
func sshes(ex *fakeExec) [][]string {
	var out [][]string
	for _, a := range ex.argv {
		if len(a) > 0 && a[0] == "ssh" {
			out = append(out, a)
		}
	}
	return out
}

func TestBuildStoresACandidate(t *testing.T) {
	f := buildFixture(t)
	ans := builderAnswer(t)
	ex := &fakeExec{answer: func([]string) (int, string) { return 0, ans }}
	code, out, errs := opsRun(t, f, opsCLI{}, ex, "build", buildC)
	if code != 0 || !strings.Contains(out, "built "+buildC+" on helpera") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	ss := sshes(ex)
	if len(ss) != 1 || !slices.Contains(ss[0], "factory@helpera") || !slices.Equal(ss[0][len(ss[0])-2:], []string{"build", buildC}) {
		t.Errorf("ssh %v", ss)
	}
	if fi, err := os.Stat(f.env.Path("builds", buildC, "factory")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("candidate: %v %v", fi, err)
	}
	if log, _ := os.ReadFile(f.env.Path("logs", "factory.log")); !strings.Contains(string(log), "build "+buildC+" on helpera: stored") {
		t.Errorf("log %q", log)
	}
}

func TestBuildNeedsTheSource(t *testing.T) {
	f := buildFixture(t)
	os.RemoveAll(f.env.Path("src", buildC))
	ex := &fakeExec{}
	if code, _, errs := opsRun(t, f, opsCLI{}, ex, "build", buildC); code == 0 || !strings.Contains(errs, "no source to build") || len(sshes(ex)) != 0 {
		t.Errorf("%d %q", code, errs)
	}
}

func TestBuildKeepsAnExistingCandidate(t *testing.T) {
	f := buildFixture(t)
	f.env.WriteFile(t, "builds/"+buildC+"/BUILD", "x\n")
	ex := &fakeExec{}
	if code, out, _ := opsRun(t, f, opsCLI{}, ex, "build", buildC); code != 0 || !strings.Contains(out, "built already") || len(sshes(ex)) != 0 {
		t.Errorf("%d %q", code, out)
	}
}

func TestBuildReportsTheHelpersRefusal(t *testing.T) {
	f := buildFixture(t)
	ex := &fakeExec{answer: func([]string) (int, string) { return 2, "" }}
	code, _, errs := opsRun(t, f, opsCLI{}, ex, "build", buildC)
	if code == 0 || !strings.Contains(errs, "build "+buildC+" on helpera") {
		t.Errorf("%d %q", code, errs)
	}
	if _, err := os.Stat(f.env.Path("builds", buildC)); err == nil {
		t.Error("a refused build stored a candidate")
	}
	if log, _ := os.ReadFile(f.env.Path("logs", "factory.log")); !strings.Contains(string(log), "build "+buildC+" on helpera: failed") {
		t.Errorf("log %q", log)
	}
}

func TestBuildRefusesABadCommitOrBuilder(t *testing.T) {
	f := buildFixture(t)
	for _, argv := range [][]string{{"build", "35956371F2FF"}, {"build", buildC, "--host", "nope"}} {
		if code, _, _ := opsRun(t, f, opsCLI{}, &fakeExec{}, argv...); code == 0 {
			t.Errorf("%v ran", argv)
		}
	}
}

// selfFixture: [build] host = "self", with SSH_ORIGINAL_COMMAND in the
// environment, as when factory build runs under a forced command.
func selfFixture(t *testing.T) *fixture {
	t.Helper()
	f := buildFixture(t)
	f.env.WriteConfig(t, "[build]\nhost = \"self\"\n")
	f.env.Vars["SSH_ORIGINAL_COMMAND"] = "start x"
	return f
}

// a master with [build] host = "self" runs factory-remote's build verb
// itself: the source on stdin, and no SSH_ORIGINAL_COMMAND (the verb is
// in argv; over SSH it would be refused).
func TestBuildSelfRunsTheLocalVerb(t *testing.T) {
	f := selfFixture(t)
	ans := builderAnswer(t)
	ex := &fakeExec{answer: func([]string) (int, string) { return 0, ans }}
	code, out, errs := opsRun(t, f, opsCLI{}, ex, "build", buildC)
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if len(ex.cmds) != 1 || len(sshes(ex)) != 0 {
		t.Fatalf("ran %v", ex.argv)
	}
	c := ex.cmds[0]
	if want := []string{filepath.Join(f.env.Root, "bin", "factory-remote"), "build", buildC}; !slices.Equal(c.Argv, want) {
		t.Errorf("argv %v, want %v", c.Argv, want)
	}
	if slices.ContainsFunc(c.Env, func(kv string) bool { return strings.HasPrefix(kv, "SSH_ORIGINAL_COMMAND=") }) {
		t.Errorf("env %v", c.Env)
	}
	if !slices.Contains(c.Env, "FACTORY_ROOT="+f.env.Root) {
		t.Errorf("env lacks the root: %v", c.Env)
	}
	if c.Stdin == nil || c.Timeout != jobproto.Timeout(jobproto.VerbBuild) || c.MaxOut != jobproto.OutputCap(jobproto.VerbBuild) {
		t.Errorf("cmd %+v", c)
	}
	if b, _ := io.ReadAll(c.Stdin); len(b) == 0 {
		t.Error("no source on stdin")
	}
}

// the self build's answer is checked and stored like a helper's,
// with "self" as the builder.
func TestBuildSelfStoresTheCandidate(t *testing.T) {
	f := selfFixture(t)
	ans := builderAnswer(t)
	ex := &fakeExec{answer: func([]string) (int, string) { return 0, ans }}
	code, out, errs := opsRun(t, f, opsCLI{}, ex, "build", buildC)
	if code != 0 || !strings.Contains(out, "built "+buildC+" on self") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if fi, err := os.Stat(f.env.Path("builds", buildC, "factory")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("candidate: %v %v", fi, err)
	}
	if log, _ := os.ReadFile(f.env.Path("logs", "factory.log")); !strings.Contains(string(log), "build "+buildC+" on self: stored") {
		t.Errorf("log %q", log)
	}
}

// a failed self build stores nothing and reports the verb's failure.
func TestBuildSelfFailureIsReported(t *testing.T) {
	f := selfFixture(t)
	ex := &fakeExec{answer: func([]string) (int, string) { return 3, "" }}
	code, _, errs := opsRun(t, f, opsCLI{}, ex, "build", buildC)
	if code == 0 || !strings.Contains(errs, "build "+buildC+" on self") || !strings.Contains(errs, "build failed (3)") {
		t.Errorf("%d %q", code, errs)
	}
	if _, err := os.Stat(f.env.Path("builds", buildC)); err == nil {
		t.Error("a failed build stored a candidate")
	}
	// --host self works without [build] too
	f.env.WriteConfig(t, "")
	ex = &fakeExec{answer: func([]string) (int, string) { return 0, builderAnswer(t) }}
	if code, out, errs := opsRun(t, f, opsCLI{}, ex, "build", buildC, "--host", "self"); code != 0 || !strings.Contains(out, "on self") {
		t.Errorf("--host self: %d %q %q", code, out, errs)
	}
}
