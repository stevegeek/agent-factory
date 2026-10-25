package gate

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/gocache"
	"github.com/stevegeek/agent-factory/internal/podman"
)

// podmanImageEnv names an image with go and sh in which the real podman
// test runs (for example docker.io/library/golang:1.25); without it the
// test is skipped, since it needs rootless podman and that image.
const podmanImageEnv = "FACTORY_TEST_GO_IMAGE"

// hostileChecks play a branch's tests in the gate's container: they check
// go uses the lent cache and finds the base's packages in it, then try
// everything to change the cache and build new code.
const hostileChecks = `test "$(go env GOCACHE)" = ` + gocache.Target + `; ` +
	`echo "compiles=$(go test -count=1 -x ./... 2>&1 | grep -cE '^[^ ]*/compile ' || true)"; ` +
	`C=` + gocache.Target + `; f=$(find $C -type f -name '*-d' | head -1); echo poison > "$f"; ` +
	`rm -rf $C/0* $C/1*; echo planted > $C/planted; chmod -R 777 $C; ` +
	`echo 'package lib; func G() int { return 2 }' > lib/g.go; go build ./... && echo hostile-done`

// The real thing for the gate: Gate 1's checks warm this host's cache in
// rootless podman from the base head, are lent it as an overlay, compile
// nothing the base has, and nothing they do reaches the generation, which
// the next gate, and the early checks, find as it was.
func TestGateTrustedCacheWithPodman(t *testing.T) {
	image := os.Getenv(podmanImageEnv)
	if image == "" {
		t.Skip(podmanImageEnv + " not set")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("no podman")
	}
	r := newRig(t)
	r.repo.Write("go.mod", "module example.com/lib\n\ngo 1.21\n")
	r.repo.Write("lib/lib.go", "package lib\n\nimport \"net/http\"\n\nfunc F() int { _ = http.StatusOK; return 1 }\n")
	r.repo.Write("lib/lib_test.go", "package lib\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {\n\tif F() != 1 {\n\t\tt.Fatal()\n\t}\n}\n")
	r.repo.Commit("go")
	r.repo.BranchWith("b", map[string]string{"README.md": "branch\n"})
	base := r.git(r.repo.Dir, "rev-parse", "main")
	r.runner.Podman = podman.Client{Exec: execx.OS{}, Env: os.Environ()}
	cfg := mustConfig(t, "image = "+strconv.Quote(image)+"\n\n[projects.gem]\npath = "+strconv.Quote(r.repo.Dir)+
		"\nbase = \"main\"\nkind = \"go-module\"\ninstall = \"true\"\ntest = "+strconv.Quote(hostileChecks)+"\nlint = \"true\"\n")
	p, _ := cfg.Project("gem")

	check := func(id string) string {
		t.Helper()
		ok, tail, err := r.runner.Checks(t.Context(), CheckIn{Config: cfg, Project: *p, ID: id, Branch: "b", Base: base})
		if err != nil || !ok {
			t.Fatalf("%s: %v %v\n%s\nlog %q", id, ok, err, tail, r.log.lines)
		}
		return tail
	}
	out := check("T0001")
	gen := filepath.Join(r.paths.Root, "cache", "go", "gem", base[:12])
	before := hashTree(t, gen)
	if len(before) < 100 {
		t.Fatalf("the warm cached only %d entries", len(before))
	}
	if !strings.Contains(out, "compiles=0") || !strings.Contains(out, "hostile-done") {
		t.Errorf("checks output:\n%s", out)
	}
	if after := hashTree(t, gen); !slices.Equal(after, before) {
		t.Errorf("the checks changed the trusted cache: %d entries before, %d after", len(before), len(after))
	}
	if out := check("T0002"); !strings.Contains(out, "compiles=0") {
		t.Errorf("second gate:\n%s", out)
	}
	if after := hashTree(t, gen); !slices.Equal(after, before) {
		t.Error("the second gate changed the trusted cache")
	}
	// the early checks are the same checks: kept under the image's real
	// id, used by the gate, and the generation is still as it was
	tk := gateTicket(t, `{"work": {"status": "done", "summary": "s", "category": "docs"}}`)
	if err := r.runner.RunEarly(t.Context(), cfg, *p, tk, r.git(r.repo.Dir, "rev-parse", "b")); err != nil {
		t.Fatal(err)
	}
	if after := hashTree(t, gen); !slices.Equal(after, before) {
		t.Error("the early checks changed the trusted cache")
	}
	kept, ok, err := r.runner.Kept(t.Context(), cfg, *p, tk)
	if err != nil || !ok || !kept.ChecksPass || !strings.Contains(kept.Output, "hostile-done") {
		t.Errorf("Kept = %+v %v %v\nlog %q", kept, ok, err, r.log.lines)
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
