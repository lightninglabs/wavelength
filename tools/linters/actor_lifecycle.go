package linters

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/ssa"
)

type contextState uint8

const (
	contextUnbounded contextState = 1 << iota
	contextBounded
)

func (c *frameworkChecker) checkActorLifecycle() {
	c.inspect.Preorder(
		[]ast.Node{(*ast.CallExpr)(nil)},
		func(node ast.Node) {
			call := node.(*ast.CallExpr)
			if !productionFile(c.pass, call.Pos()) {
				return
			}

			if objectIs(
				c.calledObject(call), actorPackagePath,
				"AllowConcurrentClassicBehavior",
			) {

				c.report(
					call.Pos(),
					"ALC002", "AllowConcurrentClassicBeh"+
						"avior is a test-only "+
						"escape hatch and cannot "+
						"be used in production code",
				)
			}
		},
	)

	for _, function := range c.ssa.SrcFuncs {
		if !isTxBehaviorReceive(function) ||
			!productionFile(c.pass, function.Pos()) {

			continue
		}

		flags := c.effects[function]
		if flags&effectStage != 0 && flags&effectCommit == 0 {
			c.report(
				function.Pos(),
				"ALC003", "TxBehavior.Receive stages "+
					"durable state but never commits "+
					"message consumption",
			)
		}
	}

	c.inspect.Preorder(
		[]ast.Node{(*ast.FuncDecl)(nil), (*ast.FuncLit)(nil)},
		func(node ast.Node) {
			if !productionFile(c.pass, node.Pos()) {
				return
			}

			switch function := node.(type) {
			case *ast.FuncDecl:
				if function.Body != nil {
					c.checkDetachedAskContexts(
						function.Body,
					)
				}

			case *ast.FuncLit:
				c.checkDetachedAskContexts(function.Body)
			}
		},
	)
}

func isTxBehaviorReceive(function *ssa.Function) bool {
	obj, ok := function.Object().(*types.Func)
	if !ok || obj.Name() != "Receive" {
		return false
	}

	signature, ok := obj.Type().(*types.Signature)
	if !ok {
		return false
	}

	params := signature.Params()
	for i := 0; i < params.Len(); i++ {
		if typeIs(params.At(i).Type(), actorPackagePath, "Exec") {
			return true
		}
	}

	return false
}

func (c *frameworkChecker) checkDetachedAskContexts(body *ast.BlockStmt) {
	states := make(map[types.Object]contextState)
	assignments := collectContextAssignments(body)

	for range 8 {
		changed := false
		for _, assignment := range assignments {
			if assignment.apply(c, states) {
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	walkFunctionBody(body, func(node ast.Node) {
		call, ok := node.(*ast.CallExpr)
		if !ok || !objectIs(
			c.calledObject(call), actorPackagePath, "OnComplete",
		) || len(call.Args) == 0 {
			return
		}

		state := c.contextState(call.Args[0], states)
		if state&contextUnbounded == 0 {
			return
		}

		c.report(
			call.Args[0].Pos(),
			"ALC001", "DetachedAsk.CallerCtx must be wrapped "+
				"with context.WithTimeout or "+
				"context.WithDeadline before OnComplete",
		)
	})
}

type contextAssignment struct {
	lhs ast.Expr
	rhs ast.Expr
}

func (a contextAssignment) apply(c *frameworkChecker,
	states map[types.Object]contextState) bool {

	obj := rootObject(c.pass.TypesInfo, a.lhs)
	if obj == nil {
		return false
	}

	state := c.contextState(a.rhs, states)
	if state == 0 || states[obj]|state == states[obj] {
		return false
	}

	states[obj] |= state

	return true
}

func collectContextAssignments(body *ast.BlockStmt) []contextAssignment {
	var assignments []contextAssignment
	walkFunctionBody(body, func(node ast.Node) {
		switch node := node.(type) {
		case *ast.AssignStmt:
			if len(node.Rhs) == 1 && len(node.Lhs) > 1 {
				assignments = append(
					assignments, contextAssignment{
						lhs: node.Lhs[0],
						rhs: node.Rhs[0],
					},
				)

				return
			}

			for i := 0; i < len(node.Lhs) && i < len(node.Rhs); i++ {
				assignments = append(
					assignments, contextAssignment{
						lhs: node.Lhs[i],
						rhs: node.Rhs[i],
					},
				)
			}

		case *ast.ValueSpec:
			for i := 0; i < len(node.Names) && i < len(node.Values); i++ {
				assignments = append(
					assignments, contextAssignment{
						lhs: node.Names[i],
						rhs: node.Values[i],
					},
				)
			}
		}
	})

	return assignments
}

func (c *frameworkChecker) contextState(expr ast.Expr,
	states map[types.Object]contextState) contextState {

	switch expr := expr.(type) {
	case *ast.Ident:
		return states[rootObject(c.pass.TypesInfo, expr)]

	case *ast.ParenExpr:
		return c.contextState(expr.X, states)

	case *ast.SelectorExpr:
		selection := c.pass.TypesInfo.Selections[expr]
		if selection != nil && objectIs(
			selection.Obj(), actorPackagePath, "CallerCtx",
		) {
			return contextUnbounded
		}

		return states[rootObject(c.pass.TypesInfo, expr)]

	case *ast.CallExpr:
		if len(expr.Args) == 0 {
			return 0
		}

		obj := c.calledObject(expr)
		parent := c.contextState(expr.Args[0], states)
		if objectIs(obj, "context", "WithTimeout") ||
			objectIs(obj, "context", "WithDeadline") {

			if parent != 0 {
				return contextBounded
			}
		}

		if obj != nil && obj.Pkg() != nil &&
			(obj.Pkg().Path() == "context" ||
				obj.Pkg().Path() == actorPackagePath) {
			return parent
		}
	}

	return 0
}

func walkFunctionBody(body *ast.BlockStmt, visit func(ast.Node)) {
	ast.Inspect(body, func(node ast.Node) bool {
		if node == nil {
			return true
		}
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}

		visit(node)

		return true
	})
}
