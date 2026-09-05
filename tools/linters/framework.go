package linters

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/types/typeutil"
)

const (
	frameworkLinterName = "framework"

	actorPackagePath    = "github.com/lightninglabs/wavelength/baselib/actor"
	protoFSMPackagePath = "github.com/lightninglabs/wavelength/baselib/" +
		"protofsm"
)

// FrameworkPlugin provides semantic checks for the actor and protofsm
// frameworks.
type FrameworkPlugin struct{}

// NewFramework creates the framework plugin.
func NewFramework(any) (register.LinterPlugin, error) {
	return &FrameworkPlugin{}, nil
}

// BuildAnalyzers creates the framework analyzer.
func (f *FrameworkPlugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{frameworkAnalyzer}, nil
}

// GetLoadMode requests the type information needed by the framework checks.
func (f *FrameworkPlugin) GetLoadMode() string {
	return register.LoadModeTypesInfo
}

var frameworkAnalyzer = &analysis.Analyzer{
	Name: frameworkLinterName,
	Doc:  "checks actor and protofsm semantic invariants",
	Requires: []*analysis.Analyzer{
		inspect.Analyzer,
		buildssa.Analyzer,
	},
	FactTypes: []analysis.Fact{
		new(effectFact),
	},
	Run: runFramework,
}

type frameworkChecker struct {
	pass      *analysis.Pass
	inspect   *inspector.Inspector
	ssa       *buildssa.SSA
	effects   map[*ssa.Function]effect
	functions map[*types.Func]*ssa.Function
}

func runFramework(pass *analysis.Pass) (any, error) {
	checker := &frameworkChecker{
		pass:    pass,
		inspect: pass.ResultOf[inspect.Analyzer].(*inspector.Inspector),
		ssa:     pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA),
	}

	checker.buildEffects()
	checker.checkActorTransactions()
	checker.checkActorLifecycle()
	checker.checkActorTLV()
	checker.checkProtoFSMTables()

	return nil, nil
}

func (c *frameworkChecker) report(pos token.Pos, code, message string) {
	c.pass.Reportf(pos, "%s: %s", code, message)
}

func (c *frameworkChecker) calledObject(call *ast.CallExpr) types.Object {
	return typeutil.Callee(c.pass.TypesInfo, call)
}

func objectIs(obj types.Object, packagePath, name string) bool {
	return obj != nil && obj.Pkg() != nil &&
		obj.Pkg().Path() == packagePath && obj.Name() == name
}

func namedTypeObject(t types.Type) *types.TypeName {
	if t == nil {
		return nil
	}

	t = types.Unalias(t)
	if pointer, ok := t.(*types.Pointer); ok {
		t = types.Unalias(pointer.Elem())
	}

	named, ok := t.(*types.Named)
	if !ok {
		return nil
	}

	return named.Origin().Obj()
}

func typeIs(t types.Type, packagePath, name string) bool {
	obj := namedTypeObject(t)
	if obj == nil {
		return false
	}

	return objectIs(obj, packagePath, name)
}

func (c *frameworkChecker) lookupType(packagePath, name string) types.Type {
	var pkg *types.Package
	if c.pass.Pkg.Path() == packagePath {
		pkg = c.pass.Pkg
	} else {
		for _, imported := range c.pass.Pkg.Imports() {
			if imported.Path() == packagePath {
				pkg = imported
				break
			}
		}
	}
	if pkg == nil {
		return nil
	}

	obj := pkg.Scope().Lookup(name)
	if obj == nil {
		return nil
	}

	return obj.Type()
}

func productionFile(pass *analysis.Pass, pos token.Pos) bool {
	name := pass.Fset.PositionFor(pos, false).Filename

	return !strings.HasSuffix(name, "_test.go")
}

func fieldValue(lit *ast.CompositeLit, name string) ast.Expr {
	for _, elt := range lit.Elts {
		field, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}

		ident, ok := field.Key.(*ast.Ident)
		if ok && ident.Name == name {
			return field.Value
		}
	}

	return nil
}

func rootObject(info *types.Info, expr ast.Expr) types.Object {
	switch expr := expr.(type) {
	case *ast.Ident:
		if obj := info.Uses[expr]; obj != nil {
			return obj
		}

		return info.Defs[expr]

	case *ast.ParenExpr:
		return rootObject(info, expr.X)

	case *ast.SelectorExpr:
		return rootObject(info, expr.X)

	case *ast.IndexExpr:
		return rootObject(info, expr.X)

	case *ast.IndexListExpr:
		return rootObject(info, expr.X)
	}

	return nil
}

func init() {
	register.Plugin(frameworkLinterName, NewFramework)
}
