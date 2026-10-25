package helper

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/gocache"
)

// podmanImageEnv names an image with go, git and sh in which the real
// podman test runs (for example docker.io/library/golang:1.25); without it
// the test is skipped, since it needs rootless podman and that image.
const podmanImageEnv = "FACTORY_TEST_GO_IMAGE"

// hostileScript plays an agent and its tests in the job's own container:
// it checks go uses the lent cache and finds the base's packages in it,
// then tries everything to change the cache and builds new code.
const hostileScript = `set -e
test "$(go env GOCACHE)" = ` + gocache.Target + `
cd /job && git clone -q -b main repo.bundle repo && cd repo && test -f lib/lib.go
echo "compiles=$(go test -count=1 -x ./... 2>&1 | grep -cE '^[^ ]*/compile ' || true)"
C=` + gocache.Target + `
f=$(find $C -type f -name '*-d' | head -1); echo poison > "$f"
rm -rf $C/0* $C/1*
echo planted > $C/planted
chmod -R 777 $C
echo 'package lib; func G() int { return 2 }' > lib/g.go
go build ./... && echo hostile-done
`

// The real thing: a warm in rootless podman fills a generation, a job's
// container (the argv run builds, keep-id and all) is lent it as an
// overlay, go uses it and compiles nothing the base has, and nothing the
// job does reaches the generation, which a second job finds as it was.
func TestTrustedCacheWithPodman(t *testing.T) {
	image := os.Getenv(podmanImageEnv)
	if image == "" {
		t.Skip(podmanImageEnv + " not set")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("no podman")
	}
	base := t.TempDir()
	root := filepath.Join(base, "factory")
	src := filepath.Join(base, "src")
	for _, d := range []string{root, filepath.Join(src, "lib")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"go.mod":          "module example.com/lib\n\ngo 1.25\n",
		"lib/lib.go":      "package lib\n\nimport \"net/http\"\n\nfunc F() int { _ = http.StatusOK; return 1 }\n",
		"lib/lib_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {\n\tif F() != 1 {\n\t\tt.Fatal()\n\t}\n}\n",
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", src, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	head := git("rev-parse", "HEAD")

	s := &Server{Root: root, Home: filepath.Join(base, "home"), Self: "/bin/false", Exec: execx.OS{},
		Clock: clock.Fixed{T: time.Now()}, Environ: os.Environ()}
	os.MkdirAll(filepath.Join(s.Home, ".config", "factory"), 0o700)
	os.WriteFile(filepath.Join(s.Home, ".config", "factory", "anthropic-api-key"), []byte("test-key"), 0o600)
	os.WriteFile(filepath.Join(root, "config.toml"), []byte("[remote]\nimage = \""+image+"\"\nmemory = \"1g\"\n"), 0o644)

	job := func(name string) (string, string) {
		t.Helper()
		d := filepath.Join(root, "jobs", name)
		for _, sub := range []string{"work", "home"} {
			if err := os.MkdirAll(filepath.Join(d, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		git("bundle", "create", "-q", filepath.Join(d, "work", "repo.bundle"), "main")
		os.WriteFile(filepath.Join(d, "meta.json"), []byte(goMeta("example-lib", "go", head)), 0o644)
		lend, err := s.goCache(t.Context(), d, name)
		if err != nil || lend.Gen == "" {
			t.Fatalf("lend %+v %v (warm.log: %s)", lend, err, readMaybe(filepath.Join(d, "warm.log")))
		}
		cmd, err := s.jobCmd(d, name, lend.Gen, 0)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Argv[len(cmd.Argv)-1] = hostileScript
		res, err := s.Exec.Run(t.Context(), cmd)
		lend.Release()
		if err != nil || res.Code != 0 {
			t.Fatalf("job: %v exit %d\n%s%s", err, res.Code, res.Stdout, res.Stderr)
		}
		return lend.Gen, string(res.Stdout) + string(res.Stderr)
	}

	gen, out := job("t0001-w1")
	before := hashTree(t, gen)
	if len(before) < 100 {
		t.Fatalf("the warm cached only %d entries", len(before))
	}
	if !strings.Contains(out, "compiles=0") || !strings.Contains(out, "hostile-done") {
		t.Errorf("job output:\n%s", out)
	}
	if after := hashTree(t, gen); !slices.Equal(after, before) {
		t.Errorf("the job changed the trusted cache: %d entries before, %d after", len(before), len(after))
	}
	gen2, out := job("t0002-r1")
	if gen2 != gen || !strings.Contains(out, "compiles=0") {
		t.Errorf("second job: %s\n%s", gen2, out)
	}
	if after := hashTree(t, gen); !slices.Equal(after, before) {
		t.Error("the second job changed the trusted cache")
	}
}

func hashTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s %v %d", strings.TrimPrefix(p, dir), fi.Mode(), fi.ModTime().UnixNano())
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			line += fmt.Sprintf(" %x", sha256.Sum256(b))
		}
		out = append(out, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func readMaybe(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}
