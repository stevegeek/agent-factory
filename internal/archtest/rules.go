package archtest

import (
	"go/ast"
	"go/constant"
	"go/importer"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// typed is one production package, parsed and type-checked.
type typed struct {
	pkg   *Package
	files []*ast.File
	tpkg  *types.Package
	info  *types.Info
}

// typeCheck parses and type-checks every module package's production files
// against the export data of the loaded graph. A package that does not
// type-check is reported under rule.
func typeCheck(g *Graph, fset *token.FileSet, rule string) ([]typed, []Violation) {
	var out []typed
	var vs []Violation
	shared := importer.ForCompiler(fset, "gc", g.lookup(""))
	for _, p := range g.Pkgs {
		if p.Error != nil || len(p.GoFiles) == 0 {
			continue
		}
		files, err := parseFiles(fset, p.Dir, p.GoFiles)
		if err != nil {
			vs = append(vs, Violation{rule, p.Rel, err.Error()})
			continue
		}
		info := newInfo()
		var typeErrs []string
		conf := types.Config{Importer: shared, Error: func(err error) { typeErrs = append(typeErrs, err.Error()) }}
		tpkg, _ := conf.Check(p.ImportPath, fset, files, info)
		if len(typeErrs) > 0 {
			vs = append(vs, Violation{rule, p.Rel, "cannot type-check: " + typeErrs[0]})
			continue
		}
		out = append(out, typed{p, files, tpkg, info})
	}
	return out, vs
}

// domainPackages are the packages §2.2 rule 2 keeps free of I/O and of the
// wall clock.
func domainPackages() []string { return []string{"internal/model", "internal/ticket"} }

// impureImport reports whether a standard-library import does file I/O,
// exec or network.
func impureImport(path string) bool {
	switch path {
	case "os", "io/ioutil", "syscall", "net":
		return true
	}
	return strings.HasPrefix(path, "net/") || strings.HasPrefix(path, "os/")
}

// CheckDomainPurity enforces §2.2 rule 2: model and ticket do no file I/O,
// no exec and no network (no import of os, os/exec, net or net/...), and
// never ask for the time (no reference to time.Now: the clock is passed in).
func CheckDomainPurity(g *Graph) []Violation {
	fset := token.NewFileSet()
	pkgs, vs := typeCheck(g, fset, "purity")
	for _, tp := range pkgs {
		if !slices.Contains(domainPackages(), tp.pkg.Rel) {
			continue
		}
		for _, f := range tp.files {
			for _, imp := range f.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if impureImport(path) {
					vs = append(vs, Violation{"purity", fset.Position(imp.Pos()).String(), "the domain package imports " + path + " (no file I/O, exec or network: §2.2 rule 2)"})
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if fn, ok := tp.info.Uses[id].(*types.Func); ok && fn.FullName() == "time.Now" {
					vs = append(vs, Violation{"purity", fset.Position(id.Pos()).String(), "the domain package calls time.Now (the clock is passed in: §2.2 rule 2)"})
				}
				return true
			})
		}
	}
	return sortViolations(dedupe(vs))
}

// MaxInterfaceMethods is §2.2 rule 6: no package exports an interface of
// more than four methods for others to implement.
const MaxInterfaceMethods = 4

// CheckInterfaceSize enforces §2.2 rule 6 on every exported interface type
// in production code, counting the methods of embedded interfaces.
func CheckInterfaceSize(g *Graph) []Violation {
	fset := token.NewFileSet()
	pkgs, vs := typeCheck(g, fset, "interfaces")
	for _, tp := range pkgs {
		for _, f := range tp.files {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, spec := range gd.Specs {
					ts := spec.(*ast.TypeSpec)
					if !ts.Name.IsExported() {
						continue
					}
					obj, _ := tp.info.Defs[ts.Name].(*types.TypeName)
					if obj == nil {
						continue
					}
					iface, ok := obj.Type().Underlying().(*types.Interface)
					if !ok {
						continue
					}
					if n := iface.NumMethods(); n > MaxInterfaceMethods {
						vs = append(vs, Violation{"interfaces", fset.Position(ts.Pos()).String(),
							"exported interface " + ts.Name.Name + " has " + strconv.Itoa(n) + " methods (at most 4: §2.2 rule 6)"})
					}
				}
			}
		}
	}
	return sortViolations(dedupe(vs))
}

// ReasonUse is what CheckReasons found: every model.Reason constant, and
// where production code outside package model writes it.
type ReasonUse struct {
	Reasons []string            // every constant of type model.Reason, by value
	Written map[string][]string // value -> positions that write it
}

// CheckReasons enforces §3.2 and §3.5 for moves and their reasons:
//
//   - a model.Reason is never spelled as a string literal outside package
//     model (Go converts an untyped constant to model.Reason silently, so a
//     typo would compile): every reason goes through its named constant.
//     The empty literal, "no reason", is the one exception;
//   - every model.Reason constant is written by production code outside
//     model: passed to ticket.Move, or to anything else that takes a
//     model.Reason (the stage finishers hand theirs to ticket.Move through
//     their outcome). A comparison (==, !=, a switch case) reads a reason
//     and does not count.
func CheckReasons(g *Graph) (ReasonUse, []Violation) {
	fset := token.NewFileSet()
	pkgs, vs := typeCheck(g, fset, "reasons")
	use := ReasonUse{Written: map[string][]string{}}
	// Each package is checked against export data, so the model.Reason a
	// caller sees is a different types object from the one the model check
	// made: compare by qualified name.
	reasonType := ""
	isReason := func(t types.Type) bool { return t != nil && types.TypeString(t, nil) == reasonType }
	for _, tp := range pkgs {
		if tp.pkg.Rel != "internal/model" {
			continue
		}
		obj, _ := tp.tpkg.Scope().Lookup("Reason").(*types.TypeName)
		if obj == nil {
			return use, vs // a model with no reasons has nothing to check
		}
		reasonType = types.TypeString(obj.Type(), nil)
		for _, name := range tp.tpkg.Scope().Names() {
			c, ok := tp.tpkg.Scope().Lookup(name).(*types.Const)
			if !ok || !isReason(c.Type()) {
				continue
			}
			use.Reasons = append(use.Reasons, constant.StringVal(c.Val()))
		}
	}
	if reasonType == "" {
		return use, vs
	}
	sort.Strings(use.Reasons)
	for _, tp := range pkgs {
		if tp.pkg.Rel == "internal/model" {
			continue
		}
		for _, f := range tp.files {
			reads := map[*ast.Ident]bool{}
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BinaryExpr:
					if x.Op == token.EQL || x.Op == token.NEQ {
						markIdents(reads, x.X)
						markIdents(reads, x.Y)
					}
				case *ast.CaseClause:
					for _, e := range x.List {
						markIdents(reads, e)
					}
				case *ast.BasicLit:
					if tv, ok := tp.info.Types[x]; ok && tv.Value != nil && isReason(tv.Type) && x.Value != `""` {
						vs = append(vs, Violation{"reasons", fset.Position(x.Pos()).String(),
							"the string literal " + x.Value + " is used as a model.Reason: name its model.Reason constant (§3.2)"})
					}
				case *ast.Ident:
					c, ok := tp.info.Uses[x].(*types.Const)
					if !ok || !isReason(c.Type()) || c.Val().Kind() != constant.String || reads[x] {
						return true
					}
					v := constant.StringVal(c.Val())
					use.Written[v] = append(use.Written[v], fset.Position(x.Pos()).String())
				}
				return true
			})
		}
	}
	for _, r := range use.Reasons {
		if len(use.Written[r]) == 0 {
			vs = append(vs, Violation{"reasons", "internal/model", "model.Reason " + r + " is never written by production code (no ticket.Move or other writer names it: §3.5)"})
		}
	}
	return use, sortViolations(dedupe(vs))
}

// markIdents marks the identifiers of a comparison operand (a bare name or
// a package-qualified one) as reads.
func markIdents(m map[*ast.Ident]bool, e ast.Expr) {
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		m[x] = true
	case *ast.SelectorExpr:
		m[x.Sel] = true
	}
}
