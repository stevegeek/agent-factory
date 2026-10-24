package archtest

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// RegexGoldenFiles finds the regex golden files: internal/*/testdata/regex*.json
// (at any depth under a testdata directory), committed static fixtures
// written by the Python-era generator before the cutover.
func RegexGoldenFiles(dir string) ([]string, error) {
	var out []string
	root := filepath.Join(dir, "internal")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, "regex") && strings.HasSuffix(name, ".json") && underTestdata(path) {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func underTestdata(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == "testdata" {
			return true
		}
	}
	return false
}

// GoldenPatterns collects the Python patterns a regex golden file covers.
// The generator wrote internal/textx/testdata/py/regex.json as
// {"patterns": [{"name", "pattern", "py", "flags", "op", "cases"}, ...]},
// where "pattern" is the compiled pattern and "py" the pattern as the
// Python source writes it; a "// py:" comment may give either. Any object
// member named "pattern" or "py" counts, at any depth. A file with none is
// read as an object keyed by pattern.
func GoldenPatterns(files []string) (map[string]bool, error) {
	pats := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, &os.PathError{Op: "parse regex golden", Path: f, Err: err}
		}
		found := false
		var walk func(any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, val := range x {
					if s, ok := val.(string); ok && (k == "pattern" || k == "py") {
						pats[s] = true
						found = true
					}
					walk(val)
				}
			case []any:
				for _, val := range x {
					walk(val)
				}
			}
		}
		walk(v)
		if top, ok := v.(map[string]any); ok && !found {
			for k := range top {
				pats[k] = true
			}
		}
	}
	return pats, nil
}

// CheckRegexGoldens checks every regexp compile call in the module's
// production code (§8.5): each carries "// py: <python pattern>" with a
// pattern that a golden covers, or "// re: go" for a pattern that is not a
// Python port.
func CheckRegexGoldens(g *Graph, golden map[string]bool) []Violation {
	fset := token.NewFileSet()
	var vs []Violation
	for _, p := range g.Pkgs {
		for _, name := range p.GoFiles {
			path := filepath.Join(p.Dir, name)
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				vs = append(vs, Violation{"regex", path, err.Error()})
				continue
			}
			vs = append(vs, regexCalls(fset, f, p.Rel+"/"+name, golden)...)
		}
	}
	return sortViolations(vs)
}

func regexCalls(fset *token.FileSet, f *ast.File, where string, golden map[string]bool) []Violation {
	pkgName := ""
	for _, imp := range f.Imports {
		if path, _ := strconv.Unquote(imp.Path.Value); path == "regexp" {
			pkgName = "regexp"
			if imp.Name != nil {
				pkgName = imp.Name.Name
			}
		}
	}
	if pkgName == "" || pkgName == "_" {
		return nil
	}
	// Comment text by the line it ends on.
	comments := map[int][]string{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			line := fset.Position(c.End()).Line
			comments[line] = append(comments[line], strings.TrimSpace(strings.TrimPrefix(c.Text, "//")))
		}
	}
	var vs []Violation
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != pkgName {
			return true
		}
		switch sel.Sel.Name {
		case "Compile", "MustCompile", "CompilePOSIX", "MustCompilePOSIX":
		default:
			return true
		}
		callLine := fset.Position(call.Pos()).Line
		// The start of the statement, spec or element holding the call.
		start := callLine
		for i := len(stack) - 1; i >= 0; i-- {
			switch stack[i].(type) {
			case ast.Stmt, ast.Spec, *ast.KeyValueExpr, *ast.Field:
				start = fset.Position(stack[i].Pos()).Line
				i = -1
			}
		}
		var notes []string
		notes = append(notes, comments[callLine]...)
		if start != callLine {
			notes = append(notes, comments[start]...)
		}
		for l := start - 1; len(comments[l]) > 0; l-- {
			notes = append(notes, comments[l]...)
		}
		pos := where + ":" + strconv.Itoa(callLine)
		found := false
		for _, note := range notes {
			if note == "re: go" {
				found = true
				break
			}
			if pat, ok := strings.CutPrefix(note, "py: "); ok {
				found = true
				if !golden[pat] {
					vs = append(vs, Violation{"regex", pos, "pattern " + strconv.Quote(pat) + " has no entry in a regex golden (internal/*/testdata/regex*.json)"})
				}
				break
			}
		}
		if !found {
			vs = append(vs, Violation{"regex", pos, `regexp.` + sel.Sel.Name + ` needs "// py: <python pattern>" (with a golden) or "// re: go"`})
		}
		return true
	})
	return vs
}
