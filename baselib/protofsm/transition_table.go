package protofsm

import (
	"fmt"
	"reflect"
	"strings"
)

// TransitionEntry represents a single state transition with type safety.
// This provides at-a-glance visibility into how states transition in response
// to events, including any outbox messages emitted.
type TransitionEntry[S, E, M any] struct {
	// Event is the event that triggers this transition.
	Event E

	// ToState is the destination state after this transition.
	ToState S

	// Description explains why this transition occurs and any important
	// context about validation or security checks.
	Description string

	// EmitsOutbox contains the strongly-typed outbox messages emitted
	// during this transition. These messages flow between FSMs via gRPC.
	EmitsOutbox []M

	// IsTerminal indicates the ToState is a terminal state that only
	// self-loops.
	IsTerminal bool
}

// StateTransitions holds all transitions from a single state.
type StateTransitions[S, E, M any] struct {
	// FromState is the source state for these transitions.
	FromState S

	// Transitions is the list of possible transitions from this state.
	Transitions []TransitionEntry[S, E, M]
}

// TransitionTable defines all valid state transitions for a state machine.
// This provides complete, at-a-glance visibility into the state machine
// behavior and serves as both documentation and a validation target for tests.
//
// This is intended to be used as a static compile-time structure.
type TransitionTable[S, E, M any] struct {
	// MachineName identifies which state machine this table describes.
	MachineName string

	// States contains the transitions for each state in the machine.
	States []StateTransitions[S, E, M]
}

// GetTransitionsFrom returns all possible transitions from the given state.
// This allows callers to quickly see what events a state can handle and
// where they lead.
func (tt *TransitionTable[S, E, M]) GetTransitionsFrom(
	fromState S) []TransitionEntry[S, E, M] {

	fromType := reflect.TypeOf(fromState)
	for _, st := range tt.States {
		if reflect.TypeOf(st.FromState) == fromType {
			return st.Transitions
		}
	}

	return nil
}

// GetTransitionsFor returns all transitions triggered by a specific event type
// across all states. The result includes the source state for context.
func (tt *TransitionTable[S, E, M]) GetTransitionsFor(
	event E) []TransitionWithSource[S, E, M] {

	var result []TransitionWithSource[S, E, M]
	eventType := reflect.TypeOf(event)

	for _, st := range tt.States {
		for _, t := range st.Transitions {
			if reflect.TypeOf(t.Event) == eventType {
				result = append(result, TransitionWithSource[S, E, M]{
					FromState:       st.FromState,
					TransitionEntry: t,
				})
			}
		}
	}

	return result
}

// TransitionWithSource pairs a transition with its source state.
type TransitionWithSource[S, E, M any] struct {
	// FromState is the state this transition originates from.
	FromState S

	// TransitionEntry contains the transition details.
	TransitionEntry[S, E, M]
}

// GetTerminalTransitions returns all transitions that lead to terminal states.
func (tt *TransitionTable[S, E, M]) GetTerminalTransitions() []TransitionWithSource[
	S, E, M] {

	var terminals []TransitionWithSource[S, E, M]

	for _, st := range tt.States {
		for _, t := range st.Transitions {
			if t.IsTerminal {
				terminals = append(
					terminals, TransitionWithSource[S, E, M]{
						FromState:       st.FromState,
						TransitionEntry: t,
					},
				)
			}
		}
	}

	return terminals
}

// RenderMarkdown generates a markdown table showing all transitions.
func (tt *TransitionTable[S, E, M]) RenderMarkdown() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("# %s State Transitions\n\n",
		tt.MachineName))
	sb.WriteString("| From State | Event | To State | " +
		"Outbox Messages | Description |\n")
	sb.WriteString("|------------|-------|----------|" +
		"-----------------|-------------|\n")

	for _, st := range tt.States {
		fromStateName := trimPackagePrefix(
			reflect.TypeOf(st.FromState).String(),
		)

		for _, t := range st.Transitions {
			event := trimPackagePrefix(
				reflect.TypeOf(t.Event).String(),
			)
			toStateName := trimPackagePrefix(
				reflect.TypeOf(t.ToState).String(),
			)

			terminal := ""
			if t.IsTerminal {
				terminal = " (terminal)"
			}

			outbox := ""
			if len(t.EmitsOutbox) > 0 {
				var msgTypes []string
				for _, msg := range t.EmitsOutbox {
					msgType := trimPackagePrefix(
						reflect.TypeOf(msg).String(),
					)
					msgTypes = append(msgTypes, msgType)
				}
				outbox = strings.Join(msgTypes, ", ")
			}

			sb.WriteString(fmt.Sprintf(
				"| %s | %s | %s%s | %s | %s |\n",
				fromStateName, event, toStateName, terminal,
				outbox, t.Description,
			))
		}
	}

	return sb.String()
}

// trimPackagePrefix removes common package prefixes from type names for
// cleaner output.
func trimPackagePrefix(typeName string) string {
	// Remove pointer prefix if present.
	typeName = strings.TrimPrefix(typeName, "*")

	// Find the last dot and take everything after it.
	if idx := strings.LastIndex(typeName, "."); idx != -1 {
		return typeName[idx+1:]
	}

	return typeName
}
