package archtest

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// MaxPackageLines is the size limit of one internal package's non-test Go
// (§2.2 rule 8): a package is one compile unit on a small box.
const MaxPackageLines = 4000

// CheckLoad reports module packages that go list could not load. Every other
// check needs a loadable module.
func CheckLoad(g *Graph) []Violation {
	var vs []Violation
	for _, p := range g.Pkgs {
		if p.Error != nil {
			vs = append(vs, Violation{"load", p.Rel, p.Error.Err})
		}
	}
	return sortViolations(vs)
}

// CheckTable checks every internal import edge, in production and test
// code, against Table (§2.2 rule 1), refuses packages that are not in the
// table, and keeps testkit out of production code.
func CheckTable(g *Graph) []Violation {
	t := Table()
	var vs []Violation
	for _, p := range g.Pkgs {
		if _, ok := t[tableKey(p.Rel)]; !ok {
			vs = append(vs, Violation{"table", p.Rel, "package is not in the allowed-imports table (ARCHITECTURE.md §2.2)"})
			continue
		}
		edges := func(imports []string, test bool) {
			for _, imp := range imports {
				to, ok := g.rel(imp)
				if !ok {
					continue
				}
				if allowedEdge(t, p.Rel, to, test) {
					continue
				}
				what := "imports"
				if test {
					what = "tests import"
				}
				detail := what + " " + to + ", which the table does not allow"
				if inTestkit(to) && !test {
					detail = "production code imports " + to + ", which is test-only"
				}
				vs = append(vs, Violation{"table", p.Rel, detail})
			}
		}
		edges(p.Imports, false)
		edges(p.TestImports, true)
		edges(p.XTestImports, true)
	}
	return sortViolations(vs)
}

// CheckForemanClosure checks that cmd/foreman's whole dependency closure
// holds none of ForemanForbidden (§2.2 rule 3). A missing cmd/foreman is
// not a violation.
func CheckForemanClosure(g *Graph) []Violation {
	p := g.pkg(foremanCmd)
	if p == nil {
		return nil
	}
	forbidden := map[string]bool{}
	for _, name := range ForemanForbidden() {
		forbidden[relOf(name)] = true
	}
	var vs []Violation
	for _, dep := range p.Deps {
		rel, ok := g.rel(dep)
		if ok && (forbidden[rel] || inTestkit(rel)) {
			vs = append(vs, Violation{"foreman-closure", foremanCmd, "links " + rel + " (Foreman never pushes, merges, runs agents or gates, or holds credentials)"})
		}
	}
	return sortViolations(vs)
}

// CheckHelperClosure checks that cmd/factory-remote's dependency closure
// stays inside HelperAllowed (§2.2 rule 5).
func CheckHelperClosure(g *Graph) []Violation {
	p := g.pkg(helperCmd)
	if p == nil {
		return nil
	}
	allowed := map[string]bool{}
	for _, name := range HelperAllowed() {
		allowed[relOf(name)] = true
	}
	var vs []Violation
	for _, dep := range p.Deps {
		rel, ok := g.rel(dep)
		if ok && !allowed[rel] {
			vs = append(vs, Violation{"helper-closure", helperCmd, "links " + rel + " (the helper runs no dispatcher, board or GitHub code)"})
		}
	}
	return sortViolations(vs)
}

// CheckPullClosure checks that cmd/factory-pull's dependency closure stays
// inside PullAllowed: on the desktop it moves bytes between ssh, Lever and
// a worker's directory, and links no dispatcher, mailbox, git or agent
// code.
func CheckPullClosure(g *Graph) []Violation {
	p := g.pkg(pullCmd)
	if p == nil {
		return nil
	}
	allowed := map[string]bool{}
	for _, name := range PullAllowed() {
		allowed[relOf(name)] = true
	}
	var vs []Violation
	for _, dep := range p.Deps {
		rel, ok := g.rel(dep)
		if ok && !allowed[rel] {
			vs = append(vs, Violation{"pull-closure", pullCmd, "links " + rel + " (factory-pull runs no dispatcher, git or agent code)"})
		}
	}
	return sortViolations(vs)
}

// CheckStdlibOnly checks that nothing outside the standard library and the
// module is required or imported, by production or test code (§2.2 rule 8).
func CheckStdlibOnly(g *Graph) []Violation {
	var vs []Violation
	sc := bufio.NewScanner(bytes.NewReader(g.GoMod))
	inBlock := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		f := strings.Fields(line)
		switch {
		case inBlock && line == ")":
			inBlock = false
		case inBlock && len(f) > 0:
			vs = append(vs, Violation{"stdlib", "go.mod", "requires " + f[0]})
		case len(f) > 0 && f[0] == "require":
			if len(f) >= 2 && f[1] == "(" {
				inBlock = true
			} else if len(f) >= 2 {
				vs = append(vs, Violation{"stdlib", "go.mod", "requires " + f[1]})
			}
		}
	}
	for path, p := range g.All {
		if p.Standard || strings.HasSuffix(path, ".test") {
			continue
		}
		if _, ok := g.rel(path); ok {
			continue
		}
		vs = append(vs, Violation{"stdlib", path, "is neither standard library nor part of the module"})
	}
	for _, p := range g.Pkgs {
		for _, imp := range slices.Concat(p.Imports, p.TestImports, p.XTestImports) {
			if _, ok := g.rel(imp); ok || isStdPath(imp) {
				continue
			}
			vs = append(vs, Violation{"stdlib", p.Rel, "imports " + imp})
		}
	}
	return sortViolations(dedupe(vs))
}

// isStdPath is the go command's rule: a standard-library path has no dot in
// its first element.
func isStdPath(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// CheckSize checks that no internal package has more than limit lines of
// non-test Go, counting files excluded by build tags too.
func CheckSize(g *Graph, limit int) []Violation {
	var vs []Violation
	for _, p := range g.Pkgs {
		if !strings.HasPrefix(p.Rel, "internal/") {
			continue
		}
		n := 0
		for _, f := range slices.Concat(p.GoFiles, p.IgnoredGoFiles) {
			if strings.HasSuffix(f, "_test.go") || !strings.HasSuffix(f, ".go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(p.Dir, f))
			if err != nil {
				vs = append(vs, Violation{"size", p.Rel, err.Error()})
				continue
			}
			n += bytes.Count(b, []byte("\n"))
		}
		if n > limit {
			vs = append(vs, Violation{"size", p.Rel, strconv.Itoa(n) + " lines of non-test Go, over the limit of " + strconv.Itoa(limit) + ": split the package"})
		}
	}
	return sortViolations(vs)
}

func dedupe(vs []Violation) []Violation {
	seen := map[Violation]bool{}
	var out []Violation
	for _, v := range vs {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
