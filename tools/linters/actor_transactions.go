package linters

import (
	"go/ast"
	"go/constant"
	"go/types"

	"golang.org/x/tools/go/ssa"
)

func (c *frameworkChecker) checkActorTransactions() {
	for _, function := range c.ssa.SrcFuncs {
		if !productionFile(c.pass, function.Pos()) {
			continue
		}

		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				call, ok := instruction.(ssa.CallInstruction)
				if !ok {
					continue
				}

				common := call.Common()
				obj := callObject(common)
				if !isWriterTransaction(common, obj) {
					continue
				}

				callback := functionFromValue(
					common.Args[len(common.Args)-1],
				)
				if callback == nil {
					continue
				}

				flags := c.effects[callback]
				if flags&effectAwaitActor != 0 {
					c.report(
						common.Pos(),
						"ATX001", "writer "+
							"transaction "+
							"callback waits on "+
							"an actor future; "+
							"perform the "+
							"Ask/Await between "+
							"Stage or Read and "+
							"Commit",
					)
				}
				if flags&effectStrippedActorSend != 0 {
					c.report(
						common.Pos(),
						"ATX003", "writer "+
							"transaction "+
							"callback sends "+
							"with "+
							"actor.WithoutTx; "+
							"the independent "+
							"durable enqueue "+
							"can wait on the "+
							"held writer",
					)
				}
				if flags&effectOpenIndependentWrite != 0 {
					c.report(
						common.Pos(),
						"ATX004", "writer "+
							"transaction "+
							"callback opens an "+
							"independent "+
							"sql.DB transaction",
					)
				}
				if callbackEscapesTxContext(callback) {
					c.report(
						common.Pos(),
						"ATX005", "transaction "+
							"context escapes "+
							"its callback lifetime",
					)
				}
			}
		}
	}

	c.checkClassicDurableBehaviors()
}

func isWriterTransaction(common *ssa.CallCommon, obj types.Object) bool {
	if objectIs(obj, actorPackagePath, "Commit") ||
		objectIs(obj, actorPackagePath, "Stage") {
		return len(common.Args) > 0
	}

	if !objectIs(obj, actorPackagePath, "ExecTx") ||
		len(common.Args) == 0 {
		return false
	}

	readOnly := callArgument(common, obj, 1)
	constantValue, ok := readOnly.(*ssa.Const)
	if !ok || constantValue.Value == nil ||
		constantValue.Value.Kind() != constant.Bool {
		return false
	}

	return !constant.BoolVal(constantValue.Value)
}

func callbackEscapesTxContext(function *ssa.Function) bool {
	if len(function.Params) == 0 {
		return false
	}

	txCtx := function.Params[0]
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			switch instruction := instruction.(type) {
			case *ssa.Go:
				if callUsesValue(instruction.Common(), txCtx) {
					return true
				}

			case *ssa.Return:
				for _, result := range instruction.Results {
					if valueDependsOnContext(
						result, txCtx,
						make(map[ssa.Value]bool),
					) {
						return true
					}
				}

			case ssa.CallInstruction:
				common := instruction.Common()
				obj := callObject(common)
				if !objectIs(
					obj, actorPackagePath, "OnComplete",
				) &&
					!objectIs(
						obj, actorPackagePath,
						"ThenApply",
					) {

					continue
				}

				ctx := callArgument(common, obj, 0)
				if valueDependsOnContext(
					ctx, txCtx, make(map[ssa.Value]bool),
				) {
					return true
				}
			}
		}
	}

	return false
}

func callUsesValue(common *ssa.CallCommon, source ssa.Value) bool {
	for _, arg := range common.Args {
		if valueDependsOnContext(
			arg, source, make(map[ssa.Value]bool),
		) {
			return true
		}
	}

	if closure, ok := common.Value.(*ssa.MakeClosure); ok {
		for _, binding := range closure.Bindings {
			if valueDependsOnContext(
				binding, source, make(map[ssa.Value]bool),
			) {
				return true
			}
		}
	}

	return false
}

func valueDependsOnContext(value, source ssa.Value,
	seen map[ssa.Value]bool) bool {

	if value == nil || seen[value] {
		return false
	}
	if value == source {
		return true
	}
	seen[value] = true

	switch value := value.(type) {
	case *ssa.Call:
		obj := callObject(value.Common())
		if objectIs(obj, actorPackagePath, "WithoutTx") {
			return false
		}
		if obj == nil || obj.Pkg() == nil ||
			obj.Pkg().Path() != "context" {
			return false
		}

		ctx := callArgument(value.Common(), obj, 0)

		return valueDependsOnContext(ctx, source, seen)

	case *ssa.ChangeInterface:
		return valueDependsOnContext(value.X, source, seen)

	case *ssa.ChangeType:
		return valueDependsOnContext(value.X, source, seen)

	case *ssa.Convert:
		return valueDependsOnContext(value.X, source, seen)

	case *ssa.Extract:
		return valueDependsOnContext(value.Tuple, source, seen)

	case *ssa.MakeInterface:
		return valueDependsOnContext(value.X, source, seen)

	case *ssa.Phi:
		for _, edge := range value.Edges {
			if valueDependsOnContext(edge, source, seen) {
				return true
			}
		}

	case *ssa.MakeClosure:
		for _, binding := range value.Bindings {
			if valueDependsOnContext(binding, source, seen) {
				return true
			}
		}
	}

	return false
}

func (c *frameworkChecker) checkClassicDurableBehaviors() {
	c.inspect.Preorder(
		[]ast.Node{(*ast.CallExpr)(nil)},
		func(node ast.Node) {
			call := node.(*ast.CallExpr)
			if !productionFile(c.pass, call.Pos()) {
				return
			}

			obj := c.calledObject(call)
			var behavior ast.Expr
			switch {
			case objectIs(
				obj, actorPackagePath,
				"DefaultDurableActorConfig",
			) && len(call.Args) > 1:

				behavior = call.Args[1]

			case objectIs(
				obj, actorPackagePath, "NewClassicBehavior",
			) && len(call.Args) > 0:

				behavior = call.Args[0]

			default:
				return
			}

			method, _, _ := types.LookupFieldOrMethod(
				c.pass.TypesInfo.TypeOf(behavior), true,
				c.pass.Pkg, "Receive",
			)
			fn, ok := method.(*types.Func)
			if !ok || c.effectForObject(fn)&effectAwaitActor == 0 {
				return
			}

			c.report(
				behavior.Pos(),
				"ATX002", "classic durable behavior may "+
					"await an actor while its implicit "+
					"writer transaction is held; "+
					"migrate it to the "+
					"Read/Stage/Commit behavior",
			)
		},
	)
}

func (c *frameworkChecker) effectForObject(function *types.Func) effect {
	function = function.Origin()
	if local := c.functions[function]; local != nil {
		return c.effects[local]
	}

	var fact effectFact
	if c.pass.ImportObjectFact(function, &fact) {
		return effect(fact.Flags)
	}

	return 0
}
