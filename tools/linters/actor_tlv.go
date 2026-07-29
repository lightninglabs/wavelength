package linters

import (
	"go/ast"
	"go/constant"
	"go/types"
)

func (c *frameworkChecker) checkActorTLV() {
	tlvMessage := c.lookupType(actorPackagePath, "TLVMessage")
	if tlvMessage == nil {
		return
	}

	tlvInterface, ok :=
		types.Unalias(tlvMessage).Underlying().(*types.Interface)
	if !ok {
		return
	}
	tlvInterface.Complete()

	typeIDs := make(map[*types.TypeName]uint64)
	constructors := make(map[*types.Func]types.Type)

	for _, file := range c.pass.Files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}

			if function.Recv == nil {
				if result := returnedConcreteType(
					c.pass.TypesInfo, function.Body,
				); result != nil {

					obj, _ :=
						c.pass.TypesInfo.Defs[function.Name].(*types.Func)
					if obj != nil {
						constructors[obj.Origin()] = result
					}
				}
				continue
			}

			receiver := c.pass.TypesInfo.TypeOf(
				function.Recv.List[0].Type,
			)
			if !implementsInterface(receiver, tlvInterface) {
				continue
			}

			if function.Name.Name == "TLVType" {
				if id, ok := returnedConstantUint(
					c.pass.TypesInfo, function.Body,
				); ok {

					typeIDs[namedTypeObject(receiver)] = id
				}
			}

			if function.Name.Name == "Encode" ||
				function.Name.Name == "Decode" {

				c.checkTLVMethodEncoding(function)
			}
		}
	}

	for _, file := range c.pass.Files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil ||
				!productionFile(c.pass, function.Pos()) {

				continue
			}

			c.checkCodecRegistrations(
				function.Body, typeIDs, constructors,
			)
		}
	}
}

func implementsInterface(t types.Type, iface *types.Interface) bool {
	if t == nil {
		return false
	}

	if types.Implements(t, iface) {
		return true
	}

	if _, pointer := t.(*types.Pointer); !pointer {
		return types.Implements(types.NewPointer(t), iface)
	}

	return false
}

func returnedConstantUint(info *types.Info,
	body *ast.BlockStmt) (uint64, bool) {

	var (
		result uint64
		found  bool
		valid  = true
	)
	walkFunctionBody(body, func(node ast.Node) {
		statement, ok := node.(*ast.ReturnStmt)
		if !ok || len(statement.Results) != 1 {
			return
		}

		value := info.Types[statement.Results[0]].Value
		if value == nil {
			valid = false

			return
		}
		id, ok := constant.Uint64Val(value)
		if !ok || found && id != result {
			valid = false

			return
		}

		result = id
		found = true
	})

	return result, found && valid
}

func returnedConcreteType(info *types.Info, body *ast.BlockStmt) types.Type {
	var result types.Type
	walkFunctionBody(body, func(node ast.Node) {
		statement, ok := node.(*ast.ReturnStmt)
		if !ok || len(statement.Results) != 1 {
			return
		}

		candidate := info.TypeOf(statement.Results[0])
		if namedTypeObject(candidate) == nil {
			return
		}

		if result == nil {
			result = candidate

			return
		}

		if namedTypeObject(result) != namedTypeObject(candidate) {
			result = nil
		}
	})

	return result
}

func (c *frameworkChecker) checkTLVMethodEncoding(function *ast.FuncDecl) {
	if !productionFile(c.pass, function.Pos()) {
		return
	}

	walkFunctionBody(function.Body, func(node ast.Node) {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 3 {
			return
		}

		obj := c.calledObject(call)
		if !objectIs(obj, "encoding/binary", "Read") &&
			!objectIs(obj, "encoding/binary", "Write") {
			return
		}

		c.report(
			call.Pos(),
			"ATL001", "durable actor message uses fixed-layout "+
				"binary encoding; encode top-level fields "+
				"with tlv.NewStream",
		)
	})
}

func (c *frameworkChecker) checkCodecRegistrations(body *ast.BlockStmt,
	typeIDs map[*types.TypeName]uint64,
	constructors map[*types.Func]types.Type) {

	seen := make(map[types.Object]map[uint64]ast.Expr)
	walkFunctionBody(body, func(node ast.Node) {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return
		}

		obj := c.calledObject(call)
		if !objectIs(obj, actorPackagePath, "Register") &&
			!objectIs(obj, actorPackagePath, "MustRegister") {
			return
		}

		id, ok := constantUint(c.pass.TypesInfo, call.Args[0])
		if !ok {
			return
		}

		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok {
			receiver := rootObject(c.pass.TypesInfo, selector.X)
			if receiver != nil {
				if seen[receiver] == nil {
					seen[receiver] = make(
						map[uint64]ast.Expr,
					)
				}
				if _, duplicate := seen[receiver][id]; duplicate {
					c.report(
						call.Args[0].Pos(),
						"ATL003", "message codec "+
							"registers the "+
							"same TLV type "+
							"more than once",
					)
				} else {
					seen[receiver][id] = call.Args[0]
				}
			}
		}

		messageType := c.constructorType(call.Args[1], constructors)
		messageObj := namedTypeObject(messageType)
		expected, known := typeIDs[messageObj]
		if !known || expected == id {
			return
		}

		c.report(
			call.Args[0].Pos(),
			"ATL002", "message codec registration ID does not "+
				"match the constructor message's TLVType",
		)
	})
}

func constantUint(info *types.Info, expr ast.Expr) (uint64, bool) {
	value := info.Types[expr].Value
	if value == nil {
		return 0, false
	}

	return constant.Uint64Val(value)
}

func (c *frameworkChecker) constructorType(expr ast.Expr,
	constructors map[*types.Func]types.Type) types.Type {

	switch expr := expr.(type) {
	case *ast.FuncLit:
		return returnedConcreteType(c.pass.TypesInfo, expr.Body)

	case *ast.Ident, *ast.SelectorExpr, *ast.IndexExpr,
		*ast.IndexListExpr:

		obj := rootObject(c.pass.TypesInfo, expr)
		fn, ok := obj.(*types.Func)
		if ok {
			return constructors[fn.Origin()]
		}
	}

	return nil
}
