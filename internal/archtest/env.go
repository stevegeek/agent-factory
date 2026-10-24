package archtest

import (
	"bufio"
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// CheckHermeticTests checks that no test outside internal/testkit reads HOME
// or FACTORY_ROOT from the environment (§10.1 rule 2): tests build their
// root with testkit.Root.
func CheckHermeticTests(g *Graph) []Violation {
	fset := token.NewFileSet()
	var vs []Violation
	for _, p := range g.Pkgs {
		if inTestkit(p.Rel) {
			continue
		}
		for _, name := range slices.Concat(p.TestGoFiles, p.XTestGoFiles) {
			path := filepath.Join(p.Dir, name)
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				vs = append(vs, Violation{"hermetic", path, err.Error()})
				continue
			}
			osName := ""
			for _, imp := range f.Imports {
				if path, _ := strconv.Unquote(imp.Path.Value); path == "os" {
					osName = "os"
					if imp.Name != nil {
						osName = imp.Name.Name
					}
				}
			}
			if osName == "" {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); !ok || x.Name != osName {
					return true
				}
				pos := p.Rel + "/" + name + ":" + strconv.Itoa(fset.Position(call.Pos()).Line)
				switch sel.Sel.Name {
				case "UserHomeDir":
					vs = append(vs, Violation{"hermetic", pos, "a test reads the real home directory: use testkit.Root"})
				case "Getenv", "LookupEnv":
					if len(call.Args) == 1 {
						if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if v, _ := strconv.Unquote(lit.Value); v == "HOME" || v == "FACTORY_ROOT" {
								vs = append(vs, Violation{"hermetic", pos, "a test reads " + v + " from the environment: use testkit.Root"})
							}
						}
					}
				}
				return true
			})
		}
	}
	return sortViolations(vs)
}

// ReadPersonalStrings reads the private list of strings that must never
// appear in the public core: one per line, blank lines and # comments
// ignored.
func ReadPersonalStrings(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		s := strings.TrimSpace(sc.Text())
		if s != "" && !strings.HasPrefix(s, "#") {
			out = append(out, s)
		}
	}
	return out, sc.Err()
}

// CheckPersonalStrings scans every regular text file under dir (except .git)
// for any of the strings, case-insensitively. The violation names the file
// and the line, never the string itself, so CI output stays clean.
func CheckPersonalStrings(dir string, personal []string) ([]Violation, error) {
	var lower [][]byte
	for _, s := range personal {
		lower = append(lower, bytes.ToLower([]byte(s)))
	}
	var vs []Violation
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 8<<20 {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return nil // binary
		}
		rel, _ := filepath.Rel(dir, path)
		for i, line := range bytes.Split(bytes.ToLower(b), []byte("\n")) {
			for n, s := range lower {
				if bytes.Contains(line, s) {
					vs = append(vs, Violation{"personal", filepath.ToSlash(rel) + ":" + strconv.Itoa(i+1), "contains personal string #" + strconv.Itoa(n+1) + " of the private list"})
				}
			}
		}
		return nil
	})
	return sortViolations(vs), err
}
