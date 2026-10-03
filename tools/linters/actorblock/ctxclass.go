package actorblock

import (
	"go/ast"
	"go/types"
)

// ctxClass is the verdict on whether a context is derived from the turn.
type ctxClass int

const (
	// maybeDerived is the verdict when the analyzer cannot tell, which is
	// never reported.
	maybeDerived ctxClass = iota

	// notDerived marks a context that clearly is not the turn's.
	notDerived
)

// classify decides whether the context expression e is clearly not derived
// from the turn context. It is conservative: only context.Background and
// context.TODO, struct fields, and variables assigned solely from those
// (optionally wrapped by context.With*) are notDerived.
func (st *pkgState) classify(e ast.Expr, seen map[*types.Var]bool) ctxClass {
	info := st.pass.TypesInfo

	switch e := e.(type) {
	case *ast.ParenExpr:
		return st.classify(e.X, seen)

	case *ast.Ident:
		v, ok := info.Uses[e].(*types.Var)
		if !ok || st.params[v] || v.IsField() {
			return maybeDerived
		}

		defs, ok := st.defs[v]
		if !ok || len(defs) == 0 || seen[v] {
			return maybeDerived
		}

		if seen == nil {
			seen = make(map[*types.Var]bool)
		}
		seen[v] = true
		defer delete(seen, v)

		// Every definition must be clearly not derived.
		for _, rhs := range defs {
			if rhs == nil || st.classify(rhs, seen) != notDerived {
				return maybeDerived
			}
		}

		return notDerived

	case *ast.SelectorExpr:
		sel, ok := info.Selections[e]
		if ok && sel.Kind() == types.FieldVal &&
			isContext(info.TypeOf(e)) {
			return notDerived
		}

		return maybeDerived

	case *ast.CallExpr:
		callee := calleeFunc(info, e)
		if callee == nil || callee.Pkg() == nil ||
			callee.Pkg().Path() != "context" {
			return maybeDerived
		}

		switch callee.Name() {
		case "Background", "TODO":
			return notDerived

		case "WithCancel", "WithTimeout", "WithDeadline", "WithValue",
			"WithCancelCause", "WithTimeoutCause",
			"WithDeadlineCause", "WithoutCancel":

			if len(e.Args) > 0 {
				return st.classify(e.Args[0], seen)
			}
		}
	}

	return maybeDerived
}
