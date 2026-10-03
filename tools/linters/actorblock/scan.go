package actorblock

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"
)

// scanDirectives records the allow directives and reports the ones that lack
// a reason.
func (st *pkgState) scanDirectives() {
	for _, f := range st.files {
		for _, group := range f.Comments {
			for _, c := range group.List {
				kind, rest, ok := parseDirective(c.Text)
				if !ok {
					continue
				}

				if strings.TrimSpace(rest) == "" {
					st.pass.Reportf(
						c.Pos(),
						"%s requires a non-empty "+
							"reason",
						strings.Fields(c.Text)[0],
					)

					continue
				}

				pos := st.pass.Fset.Position(c.Pos())
				byFile := st.allowed[kind]
				if byFile == nil {
					byFile = make(map[string]map[int]bool)
					st.allowed[kind] = byFile
				}
				if byFile[pos.Filename] == nil {
					byFile[pos.Filename] = make(
						map[int]bool,
					)
				}
				byFile[pos.Filename][pos.Line] = true
			}
		}
	}
}

// parseDirective splits an allow directive comment into its kind and the
// reason text.
func parseDirective(text string) (string, string, bool) {
	for prefix, kind := range map[string]string{
		allowAwaitDirective: KindAwait,
		allowSendDirective:  KindSend,
	} {
		rest, ok := strings.CutPrefix(text, prefix)
		if !ok {
			continue
		}

		// Reject a longer directive name such as allow-awaiting.
		if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
			continue
		}

		return kind, rest, true
	}

	return "", "", false
}

// isAllowed reports whether a directive of the given kind covers the call,
// that is, sits on the line of the call or the line above it.
func (st *pkgState) isAllowed(kind string, call *ast.CallExpr) bool {
	lines := []token.Pos{call.Pos()}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		lines = append(lines, sel.Sel.Pos())
	}

	for _, p := range lines {
		pos := st.pass.Fset.Position(p)
		covered := st.allowed[kind][pos.Filename]
		if covered[pos.Line] || covered[pos.Line-1] {
			return true
		}
	}

	return false
}

// scanVars collects parameters and variable definitions for the context
// classification.
func (st *pkgState) scanVars() {
	info := st.pass.TypesInfo

	define := func(lhs ast.Expr, rhs ast.Expr) {
		id, ok := lhs.(*ast.Ident)
		if !ok {
			return
		}

		if v, ok := info.ObjectOf(id).(*types.Var); ok {
			st.defs[v] = append(st.defs[v], rhs)
		}
	}

	// addParams marks every named field of the lists as a parameter.
	addParams := func(lists ...*ast.FieldList) {
		for _, fl := range lists {
			if fl == nil {
				continue
			}

			for _, field := range fl.List {
				for _, name := range field.Names {
					v, ok := info.Defs[name].(*types.Var)
					if ok {
						st.params[v] = true
					}
				}
			}
		}
	}

	// defineAll records the definitions made by pairing names with values.
	// A multi-value call defines only its first result as the call itself,
	// which matters for context.With* returning (ctx, cancel). The rest are
	// unknown.
	defineAll := func(lhs []ast.Expr, rhs []ast.Expr) {
		for i, l := range lhs {
			switch {
			case len(lhs) == len(rhs):
				define(l, rhs[i])

			case i == 0 && len(rhs) == 1:
				define(l, rhs[0])

			case len(rhs) > 0:
				define(l, nil)
			}
		}
	}

	for _, f := range st.files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				addParams(n.Recv, n.Type.Params, n.Type.Results)

			case *ast.FuncLit:
				addParams(n.Type.Params, n.Type.Results)

			case *ast.AssignStmt:
				defineAll(n.Lhs, n.Rhs)

			case *ast.ValueSpec:
				names := make([]ast.Expr, len(n.Names))
				for i, name := range n.Names {
					names[i] = name
				}
				defineAll(names, n.Values)

			case *ast.RangeStmt:
				for _, e := range []ast.Expr{n.Key, n.Value} {
					if e != nil {
						define(e, nil)
					}
				}
			}

			return true
		})
	}
}
