package main

import (
	"fmt"
	"go/ast"
	"path"
	"strconv"
)

type mapping struct {
	GnoImportPath string // time
	GnoFunc       string // now
	GoImportPath  string // github.com/gnolang/gno/gnovm/stdlibs/time
	GoFunc        string // X_now
	Params        []mappingType
	Results       []mappingType
	MachineParam  bool

	gnoImports []*ast.ImportSpec
	goImports  []*ast.ImportSpec
}

type mappingType struct {
	// type of ast.Expr is from the normal ast.Expr types
	// + *linkedIdent.
	Type ast.Expr
}

func (mt mappingType) GoQualifiedName() string {
	return (&exprPrinter{
		mode: printerModeGoQualified,
	}).ExprString(mt.Type)
}

func (mt mappingType) GnoType() string {
	return (&exprPrinter{
		mode: printerModeGnoType,
	}).ExprString(mt.Type)
}

type linkedIdent struct {
	ast.BadExpr // Unused, but it makes *linkedIdent implement ast.Expr

	lt linkedType
}

func linkFunctions(pkgs []*pkgData) []mapping {
	var mappings []mapping
	for _, pkg := range pkgs {
		for _, gb := range pkg.gnoBodyless {
			nameWant := gb.Name.Name
			if !gb.Name.IsExported() {
				nameWant = "X_" + nameWant
			}
			fn := findFuncByName(pkg.goExported, nameWant)
			if fn.FuncDecl == nil {
				logWarning("package %q: no matching go function declaration (%q) exists for function %q",
					pkg.importPath, nameWant, gb.Name.Name)
				continue
			}
			mp := mapping{
				GnoImportPath: pkg.importPath,
				GnoFunc:       gb.Name.Name,
				GoImportPath:  "github.com/gnolang/gno/" + relPath() + "/" + pkg.importPath,
				GoFunc:        fn.Name.Name,

				gnoImports: gb.imports,
				goImports:  fn.imports,
			}
			if !mp.signaturesMatch(gb, fn) {
				logWarning("package %q: signature of gno function %s doesn't match signature of go function %s",
					pkg.importPath, gb.Name.Name, fn.Name.Name)
				continue
			}
			mp.loadParamsResults(gb, fn)
			mappings = append(mappings, mp)
		}
	}
	return mappings
}

func findFuncByName(fns []funcDecl, name string) funcDecl {
	for _, fn := range fns {
		if fn.Name.Name == name {
			return fn
		}
	}
	return funcDecl{}
}

func (m *mapping) loadParamsResults(gnof, gof funcDecl) {
	// initialise with lengths
	m.Params = make([]mappingType, 0, gnof.Type.Params.NumFields())
	m.Results = make([]mappingType, 0, gnof.Type.Results.NumFields())

	gofpl := gof.Type.Params.List
	if m.MachineParam {
		// skip machine parameter
		gofpl = gofpl[1:]
	}
	if gnof.Type.Params != nil {
		m._loadParamsResults(&m.Params, gnof.Type.Params.List, gofpl)
	}
	if gnof.Type.Results != nil {
		m._loadParamsResults(&m.Results, gnof.Type.Results.List, gof.Type.Results.List)
	}
}

func (m *mapping) _loadParamsResults(dst *[]mappingType, gnol, gol []*ast.Field) {
	// we know at this point in code that the signatures match;
	// however, we still need to guard against cases like this one:
	// func(n, n1 int) and func(n int, n1 int).
	// these generate two different param lists, so we need to keep track of
	// where we are in each of the two lists separately.
	var goIdx, goNameIdx int
	goAdvance := func() {
		goNameIdx++
		if goNameIdx >= len(gol[goIdx].Names) {
			goIdx++
			goNameIdx = 0
		}
	}

	for _, l := range gnol {
		n := len(l.Names)
		if n == 0 {
			n = 1
		}
		gnoe := l.Type
		for i := 0; i < n; i++ {
			goe := gol[goIdx].Type
			merged := m.mergeTypes(gnoe, goe)
			*dst = append(*dst, mappingType{Type: merged})

			goAdvance()
		}
	}
}

// mergeTypes merges gnoe and goe into a single ast.Expr.
//
// gnoe and goe are expected to have the same underlying structure, but they
// may differ in their type identifiers (possibly qualified, ie pkg.T).
// if they differ, mergeTypes returns nil.
//
// When two type identifiers are found, they are checked against the list of
// linkedTypes to determine if they refer to a linkedType. If they are not,
// mergeTypes returns nil. If they are, the *ast.Ident/*ast.SelectorExpr is
// replaced with a *linkedIdent.
//
// mergeTypes does not modify the given gnoe or goe; the returned ast.Expr is
// (recursively) newly allocated.
func (m *mapping) mergeTypes(gnoe, goe ast.Expr) ast.Expr {
	resolveGoNamed := func(lt *linkedType) bool {
		switch goe := goe.(type) {
		case *ast.SelectorExpr:
			// selector - resolve pkg ident to path
			lt.goPackage = resolveSelectorImport(m.goImports, goe)
			lt.goName = goe.Sel.Name
		case *ast.Ident:
			// local name -- use import path of go pkg
			lt.goPackage = m.GoImportPath
			lt.goName = goe.Name
		default:
			return false
		}
		return true
	}

	switch gnoe := gnoe.(type) {
	// We're working with a subset of all expressions:
	// https://go.dev/ref/spec#Type

	case *ast.SelectorExpr:
		lt := linkedType{
			gnoPackage: resolveSelectorImport(m.gnoImports, gnoe),
			gnoName:    gnoe.Sel.Name,
		}
		if !resolveGoNamed(&lt) || !linkedTypeExists(lt) {
			return nil
		}
		return &linkedIdent{lt: lt}
	case *ast.Ident:
		// easy case - built-in identifiers
		goi, ok := goe.(*ast.Ident)
		if ok && isBuiltin(gnoe.Name) && gnoe.Name == goi.Name {
			return &ast.Ident{Name: gnoe.Name}
		}

		lt := linkedType{
			gnoPackage: m.GnoImportPath,
			gnoName:    gnoe.Name,
		}
		if !resolveGoNamed(&lt) || !linkedTypeExists(lt) {
			return nil
		}
		return &linkedIdent{lt: lt}

	// easier cases -- check for equality of structure and underlying types
	case *ast.Ellipsis:
		goe, ok := goe.(*ast.Ellipsis)
		if !ok {
			return nil
		}
		elt := m.mergeTypes(gnoe.Elt, goe.Elt)
		if elt == nil {
			return nil
		}
		return &ast.Ellipsis{Elt: elt}
	case *ast.StarExpr:
		goe, ok := goe.(*ast.StarExpr)
		if !ok {
			return nil
		}
		x := m.mergeTypes(gnoe.X, goe.X)
		if x == nil {
			return nil
		}
		return &ast.StarExpr{X: x}
	case *ast.ArrayType:
		goe, ok := goe.(*ast.ArrayType)
		if !ok || !basicLitsEqual(gnoe.Len, goe.Len) {
			return nil
		}
		elt := m.mergeTypes(gnoe.Elt, goe.Elt)
		if elt == nil {
			return nil
		}
		return &ast.ArrayType{Len: gnoe.Len, Elt: elt}
	case *ast.StructType,
		*ast.FuncType,
		*ast.InterfaceType,
		*ast.MapType:
		// TODO
		panic("not implemented")
	default:
		panic(fmt.Errorf("invalid expression as func param/return type: %T (%v)", gnoe, gnoe))
	}
}

// returns full import path from package ident
func resolveImport(imports []*ast.ImportSpec, ident string) string {
	for _, i := range imports {
		s, err := strconv.Unquote(i.Path.Value)
		if err != nil {
			panic(fmt.Errorf("could not unquote import path literal: %s", i.Path.Value))
		}

		// TODO: for simplicity, if i.Name is nil we assume the name to be ==
		// to the last part of the import path.
		// ideally, use importer to resolve package directory on user's FS and
		// resolve by parsing and reading package clause
		var name string
		if i.Name != nil {
			name = i.Name.Name
		} else {
			name = path.Base(s)
		}

		if name == ident {
			return s
		}
	}
	return ""
}

func resolveSelectorImport(imports []*ast.ImportSpec, sx *ast.SelectorExpr) string {
	pkgIdent, ok := sx.X.(*ast.Ident)
	if !ok {
		panic(fmt.Errorf("encountered unhandled SelectorExpr.X type: %T (%v)", sx.X, sx))
	}
	impPath := resolveImport(imports, pkgIdent.Name)
	if impPath == "" {
		panic(fmt.Errorf(
			"unknown identifier %q (for resolving type %q)",
			pkgIdent.Name, pkgIdent.Name+"."+sx.Sel.Name,
		))
	}
	return impPath
}

// simple equivalence between two BasicLits.
// Note that this returns true only if the expressions are exactly the same;
// ie. 16 != 0x10, only 16 == 16.
func basicLitsEqual(x1, x2 ast.Expr) bool {
	if x1 == nil || x2 == nil {
		return x1 == nil && x2 == nil
	}
	l1, ok1 := x1.(*ast.BasicLit)
	l2, ok2 := x2.(*ast.BasicLit)
	if !ok1 || !ok2 {
		return false
	}
	return l1.Value == l2.Value
}

// Signatures match when they accept the same elementary types, or a linked
// type mapping (see [linkedTypes]).
//
// Additionally, if the first parameter to the Go function is
// *[gnolang.Machine], it is ignored when matching to the Gno function.
func (m *mapping) signaturesMatch(gnof, gof funcDecl) bool {
	if gnof.Type.TypeParams != nil || gof.Type.TypeParams != nil {
		panic("type parameters not supported")
	}

	// if first param of go function is *gno.Machine, remove it
	gofp := gof.Type.Params
	if len(gofp.List) > 0 && m.isGnoMachine(gofp.List[0]) {
		// avoid touching original struct
		n := *gofp
		n.List = n.List[1:]
		gofp = &n

		m.MachineParam = true
	}

	return m.fieldListsMatch(gnof.Type.Params, gofp) &&
		m.fieldListsMatch(gnof.Type.Results, gof.Type.Results)
}

func (m *mapping) isGnoMachine(field *ast.Field) bool {
	if len(field.Names) > 1 {
		return false
	}

	px, ok := field.Type.(*ast.StarExpr)
	if !ok {
		return false
	}

	sx, ok := px.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	imp := resolveSelectorImport(m.goImports, sx)
	return imp == "github.com/gnolang/gno/gnovm/pkg/gnolang" && sx.Sel.Name == "Machine"
}

func (m *mapping) fieldListsMatch(gnofl, gofl *ast.FieldList) bool {
	if gnofl == nil || gofl == nil {
		return gnofl == nil && gofl == nil
	}
	gnots, gots := fieldListToTypes(gnofl), fieldListToTypes(gofl)
	if len(gnots) != len(gots) {
		return false
	}
	for idx, gnot := range gnots {
		if m.mergeTypes(gnot, gots[idx]) == nil {
			return false
		}
	}
	return true
}

func fieldListToTypes(fl *ast.FieldList) []ast.Expr {
	e := make([]ast.Expr, 0, len(fl.List))
	for _, f := range fl.List {
		nnames := len(f.Names)
		if nnames < 1 {
			// case of unnamed param (ie. `func X(int) {}`)
			nnames = 1
		}
		for i := 0; i < nnames; i++ {
			e = append(e, f.Type)
		}
	}
	return e
}

// TODO: this is created based on the uverse definitions. This should be
// centralized, or at least have a CI/make check to make sure this stays the
// same
var builtinTypes = [...]string{
	"bool",
	"string",
	"int",
	"int8",
	"int16",
	"rune",
	"int32",
	"int64",
	"uint",
	"byte",
	"uint8",
	"uint16",
	"uint32",
	"uint64",
	"bigint",
	"float32",
	"float64",
	"error",
}

func isBuiltin(name string) bool {
	for _, x := range builtinTypes {
		if x == name {
			return true
		}
	}
	return false
}

type linkedType struct {
	gnoPackage string
	gnoName    string
	goPackage  string
	goName     string
}

var linkedTypes = [...]linkedType{
	{
		"std", "Address",
		"github.com/gnolang/gno/tm2/pkg/crypto", "Bech32Address",
	},
	{
		"std", "Coin",
		"github.com/gnolang/gno/tm2/pkg/std", "Coin",
	},
	{
		"std", "Coins",
		"github.com/gnolang/gno/tm2/pkg/std", "Coins",
	},
	{
		"std", "Realm",
		"github.com/gnolang/gno/gnovm/stdlibs/std", "Realm",
	},
	{
		"std", "BankerType",
		"github.com/gnolang/gno/gnovm/stdlibs/std", "BankerType",
	},
	{
		"std", "Banker",
		"github.com/gnolang/gno/gnovm/stdlibs/std", "Banker",
	},
}

func linkedTypeExists(lt linkedType) bool {
	for _, ltx := range linkedTypes {
		if lt == ltx {
			return true
		}
	}
	return false
}