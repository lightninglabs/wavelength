package protofsm

type TransitionEntry[S, E, M any] struct {
	Event       E
	ToState     S
	Description string
	EmitsOutbox []M
	IsTerminal  bool
}

type StateTransitions[S, E, M any] struct {
	FromState   S
	Transitions []TransitionEntry[S, E, M]
}

type TransitionTable[S, E, M any] struct {
	MachineName string
	States      []StateTransitions[S, E, M]
}
