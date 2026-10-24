package helper

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

const installCommit = "4a1c2e9b7d30"

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// buildFile is a BUILD naming commit, with remote's sha256 on its
// factory-remote line.
func buildFile(commit, remote string) string {
	return sum("f") + "  factory\n" + sum("fm") + "  foreman\n" + sum(remote) + "  factory-remote\ncommit " + commit + " go go1.27.1\n"
}

// bundle is what the master sends: deploy.sh's helper-bundle, without
// sciontool.
func bundle(t *testing.T, commit string, extra ...member) []byte {
	t.Helper()
	remote := "new factory-remote " + commit
	exe := file("bin/factory-remote", remote)
	exe.hdr.Mode = 0o755
	ms := []member{exe, file("bin/BUILD", buildFile(commit, remote)), file("bin/pi-run", "#!/bin/sh\necho new pi-run\n"),
		file("config.toml", "# new config\n"), file("systemd/factory-rebuild.service", "new unit\n"),
		file("images/pi/Containerfile", "FROM new\n"), file("images/scion-agent/Containerfile", "FROM new scion\n")}
	return mktar(t, append(ms, extra...)...)
}

// installRig is a helper that took a bundle before (the desktop's relay)
// and has opted in to installs. Its factory-remote version answers the
// bundle's commit, unless SSH_ORIGINAL_COMMAND reaches it.
func installRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.write(t, jobproto.AcceptInstalls, "")
	r.write(t, "bin/factory-remote", "old factory-remote")
	r.write(t, "bin/BUILD", buildFile("0123456789ab", "old factory-remote"))
	r.write(t, "bin/pi-run", "old pi-run\n")
	r.write(t, "config.toml", "# old config\n")
	r.write(t, "systemd/factory-rebuild.service", "old unit\n")
	r.write(t, "images/scion-agent/sciontool", "the helper's own sciontool")
	if err := os.Symlink("factory-remote", filepath.Join(r.Root, "bin/factory")); err != nil {
		t.Fatal(err)
	}
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) {
		if slices.ContainsFunc(c.Env, func(kv string) bool { return strings.HasPrefix(kv, "SSH_ORIGINAL_COMMAND=") }) {
			return execx.Result{Code: 2, Stderr: []byte("usage")}, nil
		}
		if len(c.Argv) == 2 && filepath.Base(c.Argv[0]) == "factory-remote" && c.Argv[1] == "version" {
			return execx.Result{Stdout: []byte(installCommit + " go1.27.1\n")}, nil
		}
		return execx.Result{Code: 1}, nil
	}
	r.Stdin = bytes.NewReader(bundle(t, installCommit))
	return r
}

func (r *rig) read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// unchanged reports the rig's install as installRig left it.
func unchanged(t *testing.T, r *rig) {
	t.Helper()
	if r.read(t, "bin/factory-remote") != "old factory-remote" || r.read(t, "config.toml") != "# old config\n" ||
		onDisk(filepath.Join(r.Root, "bin/factory-remote.prev")) || onDisk(filepath.Join(r.Root, "config.toml.prev")) {
		t.Error("a refused install changed the helper")
	}
	if es, _ := os.ReadDir(filepath.Join(r.Root, ".install")); len(es) != 0 {
		t.Errorf("work left behind: %v", es)
	}
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

func TestInstallPutsTheBundleInPlace(t *testing.T) {
	r := installRig(t)
	if code := r.ssh(t, "install "+installCommit); code != 0 {
		t.Fatalf("install: %d %q", code, r.stderr)
	}
	if r.stdout.String() != "installed "+installCommit+"\n" {
		t.Errorf("answer %q", r.stdout)
	}
	for rel, want := range map[string]string{
		"bin/factory-remote":               "new factory-remote " + installCommit,
		"bin/factory-remote.prev":          "old factory-remote",
		"bin/pi-run":                       "#!/bin/sh\necho new pi-run\n",
		"config.toml":                      "# new config\n",
		"config.toml.prev":                 "# old config\n",
		"systemd/factory-rebuild.service":  "new unit\n",
		"images/pi/Containerfile":          "FROM new\n",
		"images/scion-agent/Containerfile": "FROM new scion\n",
		"images/scion-agent/sciontool":     "the helper's own sciontool",
		jobproto.AcceptInstalls:            "",
	} {
		if got := r.read(t, rel); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	if !strings.Contains(r.read(t, "bin/BUILD"), "commit "+installCommit+" ") {
		t.Error("BUILD not installed")
	}
	for rel, want := range map[string]os.FileMode{"bin/factory-remote": 0o755, "bin/pi-run": 0o755, "bin/BUILD": 0o644, "config.toml": 0o644} {
		if got := mode(t, filepath.Join(r.Root, rel)).Perm(); got != want {
			t.Errorf("%s mode %v, want %v", rel, got, want)
		}
	}
	if l, err := os.Readlink(filepath.Join(r.Root, "bin/factory")); err != nil || l != "factory-remote" {
		t.Errorf("bin/factory -> %q %v", l, err)
	}
	if es, _ := os.ReadDir(filepath.Join(r.Root, ".install")); len(es) != 0 {
		t.Errorf("work left behind: %v", es)
	}
}

// A first install, where the desktop never relayed one: no old binary, no
// old config, no .prev.
func TestInstallOnAnEmptyRoot(t *testing.T) {
	r := installRig(t)
	for _, rel := range []string{"bin", "config.toml", "systemd", "images"} {
		os.RemoveAll(filepath.Join(r.Root, rel))
	}
	if code := r.ssh(t, "install "+installCommit); code != 0 {
		t.Fatalf("install: %d %q", code, r.stderr)
	}
	if onDisk(filepath.Join(r.Root, "bin/factory-remote.prev")) || onDisk(filepath.Join(r.Root, "config.toml.prev")) {
		t.Error("a .prev of nothing")
	}
	if r.read(t, "bin/factory") != "new factory-remote "+installCommit {
		t.Error("bin/factory does not reach the new factory-remote")
	}
}

// The same bundle twice keeps the .prev of the build before it.
func TestInstallTheSameBuildKeepsItsPrev(t *testing.T) {
	r := installRig(t)
	if code := r.ssh(t, "install "+installCommit); code != 0 {
		t.Fatalf("install: %d %q", code, r.stderr)
	}
	r.Stdin = bytes.NewReader(bundle(t, installCommit))
	if code := r.ssh(t, "install "+installCommit); code != 0 {
		t.Fatalf("second install: %d %q", code, r.stderr)
	}
	if r.read(t, "bin/factory-remote.prev") != "old factory-remote" {
		t.Error("a reinstall overwrote .prev with itself")
	}
}

func TestInstallRefusesWithoutAcceptInstalls(t *testing.T) {
	r := installRig(t)
	os.Remove(filepath.Join(r.Root, jobproto.AcceptInstalls))
	if code := r.ssh(t, "install "+installCommit); code != 2 || r.stderr.String() != "this helper takes no installs (touch ~/factory/accept-installs)\n" {
		t.Errorf("install: %d %q", code, r.stderr)
	}
	unchanged(t, r)
}

func TestInstallRefusesABadCommit(t *testing.T) {
	r := installRig(t)
	if code := r.ssh(t, "install 4a1c2e9b7d"); code != 2 || !strings.Contains(r.stderr.String(), "commit") {
		t.Errorf("install: %d %q", code, r.stderr)
	}
	unchanged(t, r)
}

func TestInstallRefusesAWrongSha(t *testing.T) {
	r := installRig(t)
	exe := file("bin/factory-remote", "not the binary BUILD names")
	r.Stdin = bytes.NewReader(mktar(t, exe, file("bin/BUILD", buildFile(installCommit, "the binary")),
		file("bin/pi-run", "x"), file("config.toml", "# new config\n")))
	if code := r.ssh(t, "install "+installCommit); code != 2 || !strings.Contains(r.stderr.String(), "sha256") {
		t.Errorf("install: %d %q", code, r.stderr)
	}
	unchanged(t, r)
	if len(r.exec.argvs()) != 0 {
		t.Errorf("ran %v", r.exec.argvs())
	}
}

func TestInstallRefusesAnotherCommit(t *testing.T) {
	r := installRig(t)
	r.Stdin = bytes.NewReader(bundle(t, "0123456789ab"))
	if code := r.ssh(t, "install "+installCommit); code != 2 || !strings.Contains(r.stderr.String(), "BUILD names") {
		t.Errorf("install: %d %q", code, r.stderr)
	}
	unchanged(t, r)
}

func TestInstallRefusesALinkOrStrayFile(t *testing.T) {
	link := member{hdr: tar.Header{Typeflag: tar.TypeSymlink, Name: "images/pi/evil", Linkname: "/etc/passwd"}}
	for name, extra := range map[string]member{
		"a link":          link,
		"a stray file":    file("bin/foreman", "x"),
		"accept-installs": file(jobproto.AcceptInstalls, ""),
		"a dotfile":       file(".ssh/authorized_keys", "ssh-ed25519 AAAA"),
		"an images dir":   file("images/other/Containerfile", "x"),
	} {
		t.Run(name, func(t *testing.T) {
			r := installRig(t)
			r.Stdin = bytes.NewReader(bundle(t, installCommit, extra))
			if code := r.ssh(t, "install "+installCommit); code != 2 || !strings.Contains(r.stderr.String(), "bad install bundle") {
				t.Errorf("install: %d %q", code, r.stderr)
			}
			unchanged(t, r)
		})
	}
}

func TestInstallRefusesABundleWithoutItsFiles(t *testing.T) {
	r := installRig(t)
	remote := "new factory-remote"
	r.Stdin = bytes.NewReader(mktar(t, file("bin/factory-remote", remote), file("bin/BUILD", buildFile(installCommit, remote))))
	if code := r.ssh(t, "install "+installCommit); code != 2 || !strings.Contains(r.stderr.String(), "pi-run") {
		t.Errorf("install: %d %q", code, r.stderr)
	}
	unchanged(t, r)
}

func TestInstallRefusesABinaryThatWontRun(t *testing.T) {
	r := installRig(t)
	r.exec.answer = func(execx.Cmd) (execx.Result, error) {
		return execx.Result{Code: 126, Stderr: []byte("exec format error")}, nil
	}
	if code := r.ssh(t, "install "+installCommit); code != 2 || !strings.Contains(r.stderr.String(), "exec format error") {
		t.Errorf("install: %d %q", code, r.stderr)
	}
	unchanged(t, r)

	// a binary of another commit than its BUILD says
	r = installRig(t)
	r.exec.answer = func(execx.Cmd) (execx.Result, error) {
		return execx.Result{Stdout: []byte("0123456789ab go1.27.1\n")}, nil
	}
	if code := r.ssh(t, "install "+installCommit); code != 2 || !strings.Contains(r.stderr.String(), "0123456789ab") {
		t.Errorf("install: %d %q", code, r.stderr)
	}
	unchanged(t, r)
}

// The new binary runs from the bundle with the helper's environment, less
// SSH_ORIGINAL_COMMAND: with it, serve would refuse `version` as an
// argv over ssh.
func TestInstallRunsTheBinaryWithoutTheSSHCommand(t *testing.T) {
	r := installRig(t)
	if code := r.ssh(t, "install "+installCommit); code != 0 {
		t.Fatalf("install: %d %q", code, r.stderr)
	}
	calls := r.exec.calls
	if len(calls) != 1 {
		t.Fatalf("calls: %v", r.exec.argvs())
	}
	c := calls[0]
	if !strings.HasPrefix(c.Argv[0], filepath.Join(r.Root, ".install")+"/") || c.Argv[1] != "version" {
		t.Errorf("argv %q", c.Argv)
	}
	if !slices.Contains(c.Env, "HOME="+r.Home) || slices.ContainsFunc(c.Env, func(kv string) bool { return strings.HasPrefix(kv, "SSH_ORIGINAL_COMMAND") }) {
		t.Errorf("env %q", c.Env)
	}
}

func TestInstallOneAtATime(t *testing.T) {
	r := installRig(t)
	l, err := fsx.Lock(context.Background(), filepath.Join(r.Root, ".install.lock"), fsx.NoWait)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if code := r.ssh(t, "install "+installCommit); code != 2 || r.stderr.String() != "an install is already running here\n" {
		t.Errorf("install: %d %q", code, r.stderr)
	}
	unchanged(t, r)
}

func TestInstallInputTooLarge(t *testing.T) {
	r := installRig(t)
	r.Stdin = bytes.NewReader(make([]byte, jobproto.MaxInstallInput+1))
	if code := r.ssh(t, "install "+installCommit); code != 2 || !strings.Contains(r.stderr.String(), "too large") {
		t.Errorf("install: %d %q", code, r.stderr)
	}
	unchanged(t, r)
}
