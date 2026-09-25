package linters

import (
	"go/ast"
	"go/constant"
	"go/types"
)

type parsedTransition struct {
	event       string
	toState     string
	eventPos    ast.Expr
	toStatePos  ast.Expr
	description ast.Expr
	terminal    bool
}

type parsedState struct {
	from        string
	fromPos     ast.Expr
	transitions []parsedTransition
}

func (c *frameworkChecker) checkProtoFSMTables() {
	c.inspect.Preorder([]ast.Node{(*ast.CompositeLit)(nil)},
		func(node ast.Node) {
			literal := node.(*ast.CompositeLit)
			if !productionFile(c.pass, literal.Pos()) ||
				!typeIs(
					c.pass.TypesInfo.TypeOf(literal),
					protoFSMPackagePath, "TransitionTable",
				) {
				return
			}

			c.checkProtoFSMTable(literal)
		},
	)
}

func (c *frameworkChecker) checkProtoFSMTable(table *ast.CompositeLit) {
	machineName := fieldValue(table, "MachineName")
	if value, ok := constantString(c.pass.TypesInfo, machineName); !ok ||
		value == "" {

		pos := table.Pos()
		if machineName != nil {
			pos = machineName.Pos()
		}
		c.report(
			pos, "PFS004", "protofsm transition table must "+
				"have a non-empty MachineName",
		)
	}

	states := c.parseStates(fieldValue(table, "States"))
	stateRows := make(map[string]parsedState)

	for _, state := range states {
		if previous, duplicate := stateRows[state.from]; duplicate {
			_ = previous
			c.report(
				state.fromPos.Pos(),
				"PFS001", "protofsm transition table "+
					"contains duplicate rows for the "+
					"same state type",
			)
		} else if state.from != "" {
			stateRows[state.from] = state
		}

		transitions := make(map[string]bool)
		for _, transition := range state.transitions {
			key := transition.event + "->" + transition.toState
			if transition.event != "" && transition.toState != "" &&
				transitions[key] {

				c.report(
					transition.eventPos.Pos(),
					"PFS002", "protofsm state contains "+
						"duplicate event-to-state "+
						"transition entries",
				)
			}
			transitions[key] = true

			description, ok := constantString(
				c.pass.TypesInfo, transition.description,
			)
			if !ok || description == "" {
				pos := transition.eventPos.Pos()
				if transition.description != nil {
					pos = transition.description.Pos()
				}
				c.report(
					pos, "PFS003", "protofsm "+
						"transition must have a "+
						"non-empty Description",
				)
			}

		}
	}

	terminalStates := make(map[string]bool)
	for stateType, row := range stateRows {
		if len(row.transitions) == 0 {
			continue
		}

		terminal := true
		for _, transition := range row.transitions {
			if transition.toState != stateType ||
				!transition.terminal {

				terminal = false
				break
			}
		}
		terminalStates[stateType] = terminal
	}

	for _, row := range states {
		for _, transition := range row.transitions {
			if !terminalStates[transition.toState] ||
				transition.terminal {

				continue
			}

			c.report(
				transition.toStatePos.Pos(),
				"PFS005", "transition to a terminal "+
					"self-loop state must set IsTerminal",
			)
		}
	}
}

func (c *frameworkChecker) parseStates(expr ast.Expr) []parsedState {
	literal, ok := unparen(expr).(*ast.CompositeLit)
	if !ok {
		return nil
	}

	states := make([]parsedState, 0, len(literal.Elts))
	for _, element := range literal.Elts {
		row, ok := unparen(element).(*ast.CompositeLit)
		if !ok {
			continue
		}

		from := fieldValue(row, "FromState")
		if from == nil {
			continue
		}

		state := parsedState{
			from:    expressionTypeKey(c.pass.TypesInfo, from),
			fromPos: from,
		}

		transitions, ok := unparen(
			fieldValue(row, "Transitions"),
		).(*ast.CompositeLit)
		if ok {
			for _, element := range transitions.Elts {
				entry, ok :=
					unparen(element).(*ast.CompositeLit)
				if !ok {
					continue
				}

				event := fieldValue(entry, "Event")
				toState := fieldValue(entry, "ToState")
				if event == nil || toState == nil {
					continue
				}

				terminal, _ := constantBool(
					c.pass.TypesInfo,
					fieldValue(entry, "IsTerminal"),
				)
				state.transitions = append(
					state.transitions, parsedTransition{
						event: expressionTypeKey(
							c.pass.TypesInfo, event,
						),
						toState: expressionTypeKey(
							c.pass.TypesInfo,
							toState,
						),
						eventPos:   event,
						toStatePos: toState,
						description: fieldValue(
							entry, "Description",
						),
						terminal: terminal,
					},
				)
			}
		}

		states = append(states, state)
	}

	return states
}

func unparen(expr ast.Expr) ast.Expr {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			return expr
		}
		expr = paren.X
	}
}

func expressionTypeKey(info *types.Info, expr ast.Expr) string {
	t := info.TypeOf(expr)
	if t == nil {
		return ""
	}

	return types.TypeString(
		types.Unalias(t),
		func(pkg *types.Package) string {
			return pkg.Path()
		},
	)
}

func constantString(info *types.Info, expr ast.Expr) (string, bool) {
	if expr == nil {
		return "", false
	}

	value := info.Types[expr].Value
	if value == nil || value.Kind() != constant.String {
		return "", false
	}

	return constant.StringVal(value), true
}

func constantBool(info *types.Info, expr ast.Expr) (bool, bool) {
	if expr == nil {
		return false, false
	}

	value := info.Types[expr].Value
	if value == nil || value.Kind() != constant.Bool {
		return false, false
	}

	return constant.BoolVal(value), true
}
