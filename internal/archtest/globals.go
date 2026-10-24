package archtest

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
)

// CheckGlobals enforces "no package-level mutable state" with
// go/types: a package-level var in production code must have an allowed
// type, and no code in the module, tests included, may assign to it, take
// its address or change it through a field or index after its declaration.
func CheckGlobals(g *Graph) []Violation {
	fset := token.NewFileSet()
	var vs []Violation
	globals := map[string]bool{} // "importpath.Name" of every module package-level var

	type checked struct {
		files []*ast.File
		info  *types.Info
	}
	var prod []checked
	shared := importer.ForCompiler(fset, "gc", g.lookup(""))
	for _, p := range g.Pkgs {
		if p.Error != nil || len(p.GoFiles) == 0 {
			continue
		}
		files, err := parseFiles(fset, p.Dir, p.GoFiles)
		if err != nil {
			vs = append(vs, Violation{"globals", p.Rel, err.Error()})
			continue
		}
		info := newInfo()
		var typeErrs []string
		conf := types.Config{Importer: shared, Error: func(err error) { typeErrs = append(typeErrs, err.Error()) }}
		tpkg, _ := conf.Check(p.ImportPath, fset, files, info)
		if len(typeErrs) > 0 {
			vs = append(vs, Violation{"globals", p.Rel, "cannot type-check: " + typeErrs[0]})
			continue
		}
		for _, f := range files {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, spec := range gd.Specs {
					vspec := spec.(*ast.ValueSpec)
					for i, name := range vspec.Names {
						if name.Name == "_" {
							continue
						}
						obj, _ := info.Defs[name].(*types.Var)
						if obj == nil {
							continue
						}
						globals[tpkg.Path()+"."+name.Name] = true
						var init ast.Expr
						if len(vspec.Values) == len(vspec.Names) {
							init = vspec.Values[i]
						}
						if why := allowedGlobal(p.Rel, obj, init, info); why != "" {
							vs = append(vs, Violation{"globals", fset.Position(name.Pos()).String(), "package-level var " + name.Name + " " + why})
						}
					}
				}
			}
		}
		prod = append(prod, checked{files, info})
	}

	report := func(files []*ast.File, info *types.Info) {
		for _, f := range files {
			for _, m := range mutations(f, info, globals) {
				vs = append(vs, Violation{"globals", fset.Position(m.pos).String(), m.what + " package-level var " + m.name + " outside its declaration"})
			}
		}
	}
	for _, c := range prod {
		report(c.files, c.info)
	}
	// Tests: type-check the in-package test and the external test package
	// against the test variants' export data, and look for mutations only.
	// Type errors here are not reported (go vet and go test do that).
	for _, p := range g.Pkgs {
		if p.Error != nil {
			continue
		}
		if len(p.TestGoFiles) > 0 {
			files, err := parseFiles(fset, p.Dir, append(append([]string{}, p.GoFiles...), p.TestGoFiles...))
			if err == nil {
				info := newInfo()
				conf := types.Config{Importer: importer.ForCompiler(fset, "gc", g.lookup(p.ImportPath)), Error: func(error) {}}
				conf.Check(p.ImportPath, fset, files, info)
				report(files[len(p.GoFiles):], info)
			}
		}
		if len(p.XTestGoFiles) > 0 {
			files, err := parseFiles(fset, p.Dir, p.XTestGoFiles)
			if err == nil {
				info := newInfo()
				conf := types.Config{Importer: importer.ForCompiler(fset, "gc", g.lookup(p.ImportPath)), Error: func(error) {}}
				conf.Check(p.ImportPath+"_test", fset, files, info)
				report(files, info)
			}
		}
	}
	return sortViolations(dedupe(vs))
}

// lookup opens export data for an import path. For a test of forTest, the
// test variant of a package ("q [forTest.test]") is preferred.
func (g *Graph) lookup(forTest string) func(path string) (io.ReadCloser, error) {
	return func(path string) (io.ReadCloser, error) {
		if forTest != "" {
			if p, ok := g.All[path+" ["+forTest+".test]"]; ok && p.Export != "" {
				return os.Open(p.Export)
			}
		}
		p, ok := g.All[path]
		if !ok || p.Export == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(p.Export)
	}
}

func newInfo() *types.Info {
	return &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
}

func parseFiles(fset *token.FileSet, dir string, names []string) ([]*ast.File, error) {
	var files []*ast.File
	for _, n := range names {
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

// allowedGlobal returns "" when a package-level var is allowed, or why not.
func allowedGlobal(rel string, obj *types.Var, init ast.Expr, info *types.Info) string {
	typ := types.TypeString(obj.Type(), nil)
	switch typ {
	case "*regexp.Regexp", "embed.FS":
		return ""
	case "io/fs.FS":
		if init != nil && types.TypeString(info.TypeOf(init), nil) == "embed.FS" {
			return ""
		}
		return "has type fs.FS but is not initialised from an embed.FS"
	case "error":
		if call, ok := ast.Unparen(init).(*ast.CallExpr); ok {
			if fn := calleeFunc(call, info); fn != nil && fn.FullName() == "errors.New" {
				return ""
			}
		}
		return "has type error but is not created by errors.New"
	case "string":
		// The two values build.sh sets with -ldflags -X.
		if rel == "internal/buildinfo" && (obj.Name() == "Version" || obj.Name() == "GoVersion") {
			if lit, ok := init.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				return ""
			}
		}
	}
	return "has type " + typ + ": package-level state is not allowed (make it a field, or a function returning a fresh value)"
}

func calleeFunc(call *ast.CallExpr, info *types.Info) *types.Func {
	var id *ast.Ident
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		id = fun
	case *ast.SelectorExpr:
		id = fun.Sel
	default:
		return nil
	}
	fn, _ := info.Uses[id].(*types.Func)
	return fn
}

type mutation struct {
	pos  token.Pos
	what string
	name string
}

// mutations finds assignments, increments, range assignments and address-of
// on module package-level vars, directly or through a field, index or
// pointer.
func mutations(f *ast.File, info *types.Info, globals map[string]bool) []mutation {
	var out []mutation
	target := func(e ast.Expr, what string) {
		for e != nil {
			switch x := ast.Unparen(e).(type) {
			case *ast.Ident:
				if key, ok := globalKey(info.Uses[x]); ok && globals[key] {
					out = append(out, mutation{x.Pos(), what, x.Name})
				}
				return
			case *ast.SelectorExpr:
				if _, isPkg := info.Uses[identOf(x.X)].(*types.PkgName); isPkg {
					if key, ok := globalKey(info.Uses[x.Sel]); ok && globals[key] {
						out = append(out, mutation{x.Pos(), what, x.Sel.Name})
					}
					return
				}
				e = x.X
			case *ast.IndexExpr:
				e = x.X
			case *ast.StarExpr:
				e = x.X
			default:
				return
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if s.Tok != token.DEFINE {
				for _, l := range s.Lhs {
					target(l, "assigns to")
				}
			}
		case *ast.IncDecStmt:
			target(s.X, "changes")
		case *ast.RangeStmt:
			if s.Tok == token.ASSIGN {
				target(s.Key, "assigns to")
				target(s.Value, "assigns to")
			}
		case *ast.UnaryExpr:
			if s.Op == token.AND {
				target(s.X, "takes the address of")
			}
		}
		return true
	})
	return out
}

func identOf(e ast.Expr) *ast.Ident {
	id, _ := ast.Unparen(e).(*ast.Ident)
	return id
}

func globalKey(obj types.Object) (string, bool) {
	v, ok := obj.(*types.Var)
	if !ok || v.Pkg() == nil || v.IsField() {
		return "", false
	}
	if v.Pkg().Scope().Lookup(v.Name()) != v {
		return "", false
	}
	return v.Pkg().Path() + "." + v.Name(), true
}
