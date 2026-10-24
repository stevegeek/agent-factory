package archtest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Package is the part of `go list -json` output the checks use.
type Package struct {
	ImportPath string
	Dir        string
	Name       string
	ForTest    string
	Standard   bool
	Module     *struct{ Path string }

	GoFiles, IgnoredGoFiles, TestGoFiles, XTestGoFiles []string
	Imports, TestImports, XTestImports, Deps           []string

	Export     string
	Error      *struct{ Err string }
	DepsErrors []*struct{ Err string }

	// Rel is the module-relative path ("." for the root, "internal/store").
	// Empty for packages outside the module.
	Rel string `json:"-"`
}

// Graph is a loaded module: its own packages and everything they depend on,
// including the test variants and their export data.
type Graph struct {
	Dir    string // the module root
	Module string // the module path
	GoMod  []byte

	// Pkgs are the module's own packages (not test variants), by Rel.
	Pkgs []*Package
	// All is every listed package by import path, including test variants
	// ("p [p.test]") and the standard library.
	All map[string]*Package
}

// Load lists the module at dir with `go list -deps -test -export`, so the
// import graph, the closures and the export data for type-checking come
// from one call. It uses the go command on PATH, with cgo off, the module
// read-only and no workspace.
func Load(ctx context.Context, dir string) (*Graph, error) {
	gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("archtest: reading go.mod: %w", err)
	}
	mod := modulePath(gomod)
	if mod == "" {
		return nil, fmt.Errorf("archtest: no module line in %s/go.mod", dir)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-json", "-deps", "-test", "-export", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOWORK=off")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("archtest: go list in %s: %w\n%s", dir, err, stderr.Bytes())
	}
	g := &Graph{Dir: dir, Module: mod, GoMod: gomod, All: map[string]*Package{}}
	dec := json.NewDecoder(&stdout)
	for {
		p := new(Package)
		if err := dec.Decode(p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("archtest: decoding go list output: %w", err)
		}
		g.All[p.ImportPath] = p
		if p.ForTest != "" || strings.HasSuffix(p.ImportPath, ".test") {
			continue
		}
		if rel, ok := g.rel(p.ImportPath); ok {
			p.Rel = rel
			g.Pkgs = append(g.Pkgs, p)
		}
	}
	sort.Slice(g.Pkgs, func(i, j int) bool { return g.Pkgs[i].Rel < g.Pkgs[j].Rel })
	return g, nil
}

// rel gives the module-relative path of an import path in the module.
func (g *Graph) rel(importPath string) (string, bool) {
	if i := strings.Index(importPath, " ["); i >= 0 {
		importPath = importPath[:i]
	}
	if importPath == g.Module {
		return rootPkg, true
	}
	if strings.HasPrefix(importPath, g.Module+"/") {
		return strings.TrimPrefix(importPath, g.Module+"/"), true
	}
	return "", false
}

// pkg finds a module package by its module-relative path.
func (g *Graph) pkg(rel string) *Package {
	for _, p := range g.Pkgs {
		if p.Rel == rel {
			return p
		}
	}
	return nil
}

// modulePath reads the module line of a go.mod file.
func modulePath(gomod []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(gomod))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && f[0] == "module" {
			return strings.Trim(f[1], `"`)
		}
	}
	return ""
}

// Violation is one broken rule.
type Violation struct {
	Rule   string // a short rule name, for example "table" or "globals"
	Where  string // a package, file or file:line
	Detail string
}

func (v Violation) String() string {
	return v.Rule + ": " + v.Where + ": " + v.Detail
}

func sortViolations(vs []Violation) []Violation {
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].Rule != vs[j].Rule {
			return vs[i].Rule < vs[j].Rule
		}
		if vs[i].Where != vs[j].Where {
			return vs[i].Where < vs[j].Where
		}
		return vs[i].Detail < vs[j].Detail
	})
	return vs
}
