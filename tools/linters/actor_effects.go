package linters

import (
	"go/types"

	"golang.org/x/tools/go/ssa"
)

type effect uint32

const (
	effectAwaitActor effect = 1 << iota
	effectStrippedActorSend
	effectOpenIndependentWrite
	effectStage
	effectCommit
)

type effectFact struct {
	Flags uint32
}

// AFact marks effectFact as an analysis fact.
func (*effectFact) AFact() {}

func (c *frameworkChecker) buildEffects() {
	c.effects = make(map[*ssa.Function]effect)
	c.functions = make(map[*types.Func]*ssa.Function)
	edges := make(map[*ssa.Function][]*ssa.Function)

	for _, function := range c.ssa.SrcFuncs {
		if obj, ok := function.Object().(*types.Func); ok {
			c.functions[obj.Origin()] = function
		}

		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				call, ok := instruction.(ssa.CallInstruction)
				if !ok {
					continue
				}

				common := call.Common()
				obj := callObject(common)
				c.effects[function] |= directCallEffect(obj)
				if (objectIs(obj, actorPackagePath, "Ask") ||
					objectIs(obj, actorPackagePath, "Tell")) &&
					valueIsWithoutTx(
						callArgument(common, obj, 0),
						make(map[ssa.Value]bool),
					) {

					c.effects[function] |= effectStrippedActorSend
				}

				callee := common.StaticCallee()
				if callee != nil && callee.Pkg == c.ssa.Pkg {
					edges[function] = append(
						edges[function], callee,
					)
					continue
				}

				fn, ok := obj.(*types.Func)
				if !ok || fn.Pkg() == nil ||
					fn.Pkg() == c.pass.Pkg {

					continue
				}

				var fact effectFact
				if c.pass.ImportObjectFact(fn.Origin(), &fact) {
					c.effects[function] |= effect(
						fact.Flags,
					)
				}
			}
		}
	}

	changed := true
	for changed {
		changed = false
		for caller, callees := range edges {
			before := c.effects[caller]
			for _, callee := range callees {
				c.effects[caller] |= c.effects[callee]
			}
			if c.effects[caller] != before {
				changed = true
			}
		}
	}

	for function, flags := range c.effects {
		obj, ok := function.Object().(*types.Func)
		if !ok || obj.Pkg() != c.pass.Pkg || flags == 0 {
			continue
		}

		c.pass.ExportObjectFact(obj.Origin(), &effectFact{
			Flags: uint32(flags),
		})
	}
}

func directCallEffect(obj types.Object) effect {
	switch {
	case objectIs(obj, actorPackagePath, "Await"):
		return effectAwaitActor

	case objectIs(obj, actorPackagePath, "Stage"):
		return effectStage

	case objectIs(obj, actorPackagePath, "Commit"):
		return effectCommit

	case objectIs(obj, "database/sql", "Begin") ||
		objectIs(obj, "database/sql", "BeginTx"):
		return effectOpenIndependentWrite
	}

	return 0
}

func callObject(common *ssa.CallCommon) types.Object {
	if common.IsInvoke() {
		return common.Method.Origin()
	}

	callee := common.StaticCallee()
	if callee == nil {
		return nil
	}

	obj, ok := callee.Object().(*types.Func)
	if !ok {
		return nil
	}

	return obj.Origin()
}

func functionFromValue(value ssa.Value) *ssa.Function {
	switch value := value.(type) {
	case *ssa.Function:
		return value

	case *ssa.MakeClosure:
		function, _ := value.Fn.(*ssa.Function)

		return function

	case *ssa.MakeInterface:
		return functionFromValue(value.X)

	case *ssa.ChangeInterface:
		return functionFromValue(value.X)
	}

	return nil
}

func callArgument(common *ssa.CallCommon, obj types.Object,
	parameter int) ssa.Value {

	index := parameter
	if !common.IsInvoke() {
		fn, ok := obj.(*types.Func)
		if ok {
			signature, _ := fn.Type().(*types.Signature)
			if signature != nil && signature.Recv() != nil {
				index++
			}
		}
	}

	if index < 0 || index >= len(common.Args) {
		return nil
	}

	return common.Args[index]
}

func valueIsWithoutTx(value ssa.Value, seen map[ssa.Value]bool) bool {
	if value == nil || seen[value] {
		return false
	}
	seen[value] = true

	switch value := value.(type) {
	case *ssa.Call:
		return objectIs(
			callObject(
				value.Common(),
			),
			actorPackagePath, "WithoutTx",
		)

	case *ssa.ChangeInterface:
		return valueIsWithoutTx(value.X, seen)

	case *ssa.ChangeType:
		return valueIsWithoutTx(value.X, seen)

	case *ssa.Convert:
		return valueIsWithoutTx(value.X, seen)

	case *ssa.Extract:
		return valueIsWithoutTx(value.Tuple, seen)

	case *ssa.Phi:
		for _, edge := range value.Edges {
			if valueIsWithoutTx(edge, seen) {
				return true
			}
		}
	}

	return false
}
