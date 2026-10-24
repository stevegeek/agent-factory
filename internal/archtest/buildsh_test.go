package archtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests of build.sh (§9.2-9.4): it refuses any toolchain but the pinned
// official one, never falls back to a system go, ignores the caller's go
// settings, and installs only a sha-pinned tarball.

// fakeGo is a stand-in go command: it reports the version and the baked-in
// GOEXPERIMENT kept beside it, and, like the real one, lets the environment
// override GOEXPERIMENT. GOFLAGS in the environment makes it fail, as a bad
// flag would.
const fakeGo = `#!/bin/sh
here=$(dirname "$0")
if [ -n "${GOFLAGS:-}" ]; then echo "go: bad GOFLAGS $GOFLAGS" >&2; exit 2; fi
case "$1 ${2:-}" in
"env GOVERSION") cat "$here/../VERSION.fake" ;;
"env GOEXPERIMENT") if [ -n "${GOEXPERIMENT:-}" ]; then echo "$GOEXPERIMENT"; else cat "$here/../EXPERIMENT.fake"; fi ;;
"telemetry off") ;;
*) echo "fake go: $*" >&2; exit 3 ;;
esac
`

func needBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
}

func fakeGoroot(t *testing.T, version, experiment string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "go1.27.1")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string, perm os.FileMode) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), perm); err != nil {
			t.Fatal(err)
		}
	}
	write("bin/go", fakeGo, 0o755)
	write("VERSION.fake", version+"\n", 0o644)
	write("EXPERIMENT.fake", experiment+"\n", 0o644)
	return root
}

// runBuildSh runs core/build.sh with a minimal environment: a temp HOME, a
// PATH whose first entry holds a decoy go that must never run, and extra.
func runBuildSh(t *testing.T, extra []string, args ...string) (string, int) {
	t.Helper()
	needBash(t)
	script := filepath.Join(moduleRoot(t), "build.sh")
	home := t.TempDir()
	decoy := filepath.Join(home, "decoy")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "go"), []byte("#!/bin/sh\necho decoy go ran >&2\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{script}, args...)...)
	cmd.Env = append([]string{
		"PATH=" + decoy + ":/usr/local/bin:/usr/bin:/bin",
		"HOME=" + home,
		"FACTORY_ROOT=" + filepath.Join(home, "no-factory"),
		"FACTORY_GO_CACHE=" + filepath.Join(home, "cache"),
	}, extra...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "decoy go ran") {
		t.Fatalf("build.sh ran the go on PATH:\n%s", out.String())
	}
	return out.String(), code
}

func TestBuildShToolchainChecks(t *testing.T) {
	cases := []struct {
		name       string
		version    string
		experiment string
		extra      []string
		wantOK     bool
		want       string
	}{
		{name: "the pinned official toolchain", version: "go1.27.1", wantOK: true, want: "toolchain ok"},
		{name: "a GOEXPERIMENT build (X: version)", version: "go1.27.1-X:nodwarf5", experiment: "nodwarf5", want: "GOEXPERIMENT build (X:)"},
		{name: "a baked-in GOEXPERIMENT", version: "go1.27.1", experiment: "nodwarf5", want: "GOEXPERIMENT=nodwarf5"},
		{name: "another version", version: "go1.26.3", want: "is go1.26.3, not the pinned go1.27.1"},
		{
			name: "the caller's GOEXPERIMENT and GOFLAGS are ignored", version: "go1.27.1", wantOK: true, want: "toolchain ok",
			extra: []string{"GOEXPERIMENT=nodwarf5", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=auto"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := fakeGoroot(t, tc.version, tc.experiment)
			out, code := runBuildSh(t, append([]string{"FACTORY_GOROOT=" + root}, tc.extra...), "--check-toolchain")
			if (code == 0) != tc.wantOK || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d, output:\n%s\nwant ok=%v and %q", code, out, tc.wantOK, tc.want)
			}
		})
	}
}

func TestBuildShNeverUsesASystemGo(t *testing.T) {
	out, code := runBuildSh(t, []string{"FACTORY_GOROOT=" + filepath.Join(t.TempDir(), "none")}, "--no-box")
	if code == 0 || !strings.Contains(out, "no pinned toolchain") || !strings.Contains(out, "a system go is never used") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	// The default GOROOT is under the (temp) home, not the system's.
	out, code = runBuildSh(t, nil, "--check-toolchain")
	if code == 0 || !strings.Contains(out, "/.local/share/factory/go/go1.27.1") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// On the box, a source tree that is not ours at mode 0700 is refused (it
// would be agent-writable or someone else's).
func TestBuildShRefusesAnOpenSourceTreeOnTheBox(t *testing.T) {
	st, err := os.Stat(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() == 0o700 {
		t.Skip("the checkout itself is mode 0700")
	}
	root := fakeGoroot(t, "go1.27.1", "")
	out, code := runBuildSh(t, []string{"FACTORY_GOROOT=" + root}, "--box", "--no-scope")
	if code == 0 || !strings.Contains(out, "not 700: refusing to build it") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// The distribution's toolchain on this machine, when it is a GOEXPERIMENT
// build, is refused.
func TestBuildShRefusesTheDistroToolchain(t *testing.T) {
	const distro = "/usr/lib/go"
	b, err := exec.Command(distro+"/bin/go", "env", "GOVERSION").Output()
	if err != nil || !strings.Contains(string(b), "X:") {
		t.Skip("no GOEXPERIMENT distro toolchain at " + distro)
	}
	out, code := runBuildSh(t, []string{"FACTORY_GOROOT=" + distro}, "--check-toolchain")
	if code == 0 || !strings.Contains(out, "X:") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func tarball(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := []struct {
		name, body string
		mode       int64
	}{
		{"go/bin/go", fakeGo, 0o755},
		{"go/VERSION.fake", "go1.27.1\n", 0o644},
		{"go/EXPERIMENT.fake", "\n", 0o644},
	}
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return path, hex.EncodeToString(sum[:])
}

func TestBuildShInstallsOnlyAPinnedTarball(t *testing.T) {
	dir := t.TempDir()
	tb, sum := tarball(t, dir, "go1.27.1.linux-amd64.tar.gz")
	goroot := filepath.Join(t.TempDir(), "go1.27.1")

	// The real pin file does not know this tarball.
	out, code := runBuildSh(t, []string{"FACTORY_GOROOT=" + goroot}, "--install-toolchain", tb)
	if code == 0 || !strings.Contains(out, "not pinned") {
		t.Fatalf("unpinned tarball: exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(goroot); !os.IsNotExist(err) {
		t.Fatal("an unpinned tarball was unpacked")
	}

	// A tarball of another version is refused by name.
	other, _ := tarball(t, dir, "go1.26.0.linux-amd64.tar.gz")
	out, code = runBuildSh(t, []string{"FACTORY_GOROOT=" + goroot}, "--install-toolchain", other)
	if code == 0 || !strings.Contains(out, "is not a go1.27.1 release tarball") {
		t.Fatalf("other version: exit %d:\n%s", code, out)
	}

	// Pinned: installed, then checked.
	pins := filepath.Join(dir, "go.sha256")
	if err := os.WriteFile(pins, []byte(sum+"  go1.27.1.linux-amd64.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"FACTORY_GOROOT=" + goroot, "FACTORY_GO_PINS=" + pins}
	out, code = runBuildSh(t, env, "--install-toolchain", tb)
	if code != 0 || !strings.Contains(out, "installed go1.27.1") {
		t.Fatalf("pinned tarball: exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(goroot, "bin", "go")); err != nil {
		t.Fatal(err)
	}
	// A second install does not overwrite the first.
	out, code = runBuildSh(t, env, "--install-toolchain", tb)
	if code == 0 || !strings.Contains(out, "already exists") {
		t.Fatalf("reinstall: exit %d:\n%s", code, out)
	}
}

// The committed pin file names the official go1.27.1 tarballs.
func TestPinFile(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), "build", "go.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	gomod, err := os.ReadFile(filepath.Join(moduleRoot(t), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(gomod, []byte("\ntoolchain go1.27.1\n")) {
		t.Fatalf("go.mod does not pin go1.27.1:\n%s", gomod)
	}
	for _, name := range []string{"go1.27.1.linux-amd64.tar.gz", "go1.27.1.darwin-arm64.tar.gz"} {
		found := false
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[1] == name && len(f[0]) == 64 {
				found = true
			}
		}
		if !found {
			t.Errorf("no sha256 for %s", name)
		}
	}
}

// With the real pinned toolchain (FACTORY_TEST_GOROOT), build.sh builds the
// three binaries with the injected version and a BUILD manifest.
func TestBuildShBuildsWithThePinnedToolchain(t *testing.T) {
	goroot := os.Getenv("FACTORY_TEST_GOROOT")
	if goroot == "" {
		t.Skip("FACTORY_TEST_GOROOT (an official go1.27.1) not set")
	}
	out := filepath.Join(t.TempDir(), "out")
	log, code := runBuildSh(t, []string{"FACTORY_GOROOT=" + goroot}, "--no-box", "--out", out, "--commit", "c0ffee1")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, log)
	}
	manifest, err := os.ReadFile(filepath.Join(out, "BUILD"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(manifest)), "\n")
	if len(lines) != 4 || lines[3] != "commit c0ffee1 go go1.27.1" {
		t.Fatalf("BUILD:\n%s", manifest)
	}
	for i, bin := range []string{"factory", "foreman", "factory-remote"} {
		if !strings.HasSuffix(lines[i], "  "+bin) {
			t.Errorf("BUILD line %d = %q", i, lines[i])
		}
		v, err := exec.Command(filepath.Join(out, bin), "version").Output()
		if err != nil || strings.TrimSpace(string(v)) != "c0ffee1 go1.27.1" {
			t.Errorf("%s version = %q, %v", bin, v, err)
		}
	}
}
