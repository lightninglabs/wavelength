package credit

import (
	"github.com/lightninglabs/wavelength/baselib/protofsm"
)

// CreditTransitionTable is the static transition table for the credit operation
// state machine. Every credit operation (pay, receive, redeem) is driven by a
// single opDrive event that advances the current state by one step; the table
// records, per state, where that step can lead and which outbox directives it
// emits. It documents the machine in one place, mirroring round's
// BoardingClientTransitions. The live dispatch is each state's ProcessEvent in
// transitions.go; the table is maintained alongside it by hand. The credited
// receive edge lists triggerRedeem because that is its only conditional
// directive, but a settled receive emits it only when the auto-redeem watermark
// clears (see awaitingSettlementState).
type CreditTransitionTable = protofsm.TransitionTable[
	CreditState, CreditEvent, CreditOutMsg,
]

// creditEntry is one transition row.
type creditEntry = protofsm.TransitionEntry[
	CreditState, CreditEvent, CreditOutMsg,
]

// creditStateTransitions groups the transitions out of one state.
type creditStateTransitions = protofsm.StateTransitions[
	CreditState, CreditEvent, CreditOutMsg,
]

// CreditTransitions enumerates every valid credit FSM transition.
var CreditTransitions = CreditTransitionTable{
	MachineName: "CreditOperation",
	States: []creditStateTransitions{
		{
			FromState: &quotingState{},
			Transitions: []creditEntry{{
				Event:       &opDrive{},
				ToState:     &topupCreatingState{},
				Description: "shortfall needs a top-up",
			}, {
				Event:       &opDrive{},
				ToState:     &payingState{},
				Description: "credit already sufficient",
			}},
		},
		{
			FromState: &topupCreatingState{},
			Transitions: []creditEntry{{
				Event:       &opDrive{},
				ToState:     &topupFundingState{},
				Description: "top-up created; stage dest",
				EmitsOutbox: []CreditOutMsg{&stageRecord{}},
			}, {
				Event:       &opDrive{},
				ToState:     &failedState{},
				Description: "top-up failed or no dest",
				IsTerminal:  true,
			}},
		},
		{
			FromState: &topupFundingState{},
			Transitions: []creditEntry{{
				Event:       &opDrive{},
				ToState:     &topupAwaitingCreditState{},
				Description: "OOR funding submitted",
			}},
		},
		{
			FromState: &topupAwaitingCreditState{},
			Transitions: []creditEntry{{
				Event:       &opDrive{},
				ToState:     &payingState{},
				Description: "top-up credited",
			}, {
				Event:       &opDrive{},
				ToState:     &topupAwaitingCreditState{},
				Description: "awaiting top-up credit",
				EmitsOutbox: []CreditOutMsg{&parkOp{}},
			}, {
				Event:       &opDrive{},
				ToState:     &failedState{},
				Description: "top-up failed or poll cap",
				IsTerminal:  true,
			}},
		},
		{
			FromState: &payingState{},
			Transitions: []creditEntry{{
				Event:       &opDrive{},
				ToState:     &payAwaitingSettlementState{},
				Description: "credit-only pay; settle",
			}, {
				Event:       &opDrive{},
				ToState:     &completedState{},
				Description: "mixed pay; monitor owns",
				IsTerminal:  true,
			}},
		},
		{
			FromState: &payAwaitingSettlementState{},
			Transitions: []creditEntry{{
				Event:       &opDrive{},
				ToState:     &completedState{},
				Description: "pay debited",
				IsTerminal:  true,
			}, {
				Event:       &opDrive{},
				ToState:     &payAwaitingSettlementState{},
				Description: "awaiting pay settlement",
				EmitsOutbox: []CreditOutMsg{&parkOp{}},
			}, {
				Event:       &opDrive{},
				ToState:     &failedState{},
				Description: "pay failed or poll cap",
				IsTerminal:  true,
			}},
		},
		{
			FromState: &receiveCreatingState{},
			Transitions: []creditEntry{{
				Event:       &opDrive{},
				ToState:     &awaitingSettlementState{},
				Description: "invoice created",
			}, {
				Event:       &opDrive{},
				ToState:     &failedState{},
				Description: "receive failed or no invoice",
				IsTerminal:  true,
			}},
		},
		{
			FromState: &awaitingSettlementState{},
			Transitions: []creditEntry{{
				Event:       &opDrive{},
				ToState:     &completedState{},
				Description: "credited; maybe redeem",
				EmitsOutbox: []CreditOutMsg{&triggerRedeem{}},
				IsTerminal:  true,
			}, {
				Event:       &opDrive{},
				ToState:     &awaitingSettlementState{},
				Description: "awaiting receive credit",
				EmitsOutbox: []CreditOutMsg{&parkOp{}},
			}, {
				Event:       &opDrive{},
				ToState:     &failedState{},
				Description: "receive failed or poll cap",
				IsTerminal:  true,
			}},
		},
		{
			FromState: &redeemReservingState{},
			Transitions: []creditEntry{{
				Event:       &opDrive{},
				ToState:     &redeemSubmittingState{},
				Description: "dest allocated; stage it",
				EmitsOutbox: []CreditOutMsg{&stageRecord{}},
			}, {
				Event:       &opDrive{},
				ToState:     &failedState{},
				Description: "empty destination",
				IsTerminal:  true,
			}},
		},
		{
			FromState: &redeemSubmittingState{},
			Transitions: []creditEntry{{
				Event:       &opDrive{},
				ToState:     &awaitingOORState{},
				Description: "redemption reserved",
			}, {
				Event:       &opDrive{},
				ToState:     &failedState{},
				Description: "reserve failed",
				IsTerminal:  true,
			}},
		},
		{
			FromState: &awaitingOORState{},
			Transitions: []creditEntry{{
				Event:       &opDrive{},
				ToState:     &completedState{},
				Description: "redeemed vTXO landed",
				IsTerminal:  true,
			}, {
				Event:       &opDrive{},
				ToState:     &awaitingOORState{},
				Description: "awaiting redeemed vTXO",
				EmitsOutbox: []CreditOutMsg{&parkOp{}},
			}, {
				Event:       &opDrive{},
				ToState:     &failedState{},
				Description: "redeem failed or poll cap",
				IsTerminal:  true,
			}},
		},
		{
			FromState: &completedState{},
			Transitions: []creditEntry{{
				Event:       &opDrive{},
				ToState:     &completedState{},
				Description: "terminal success",
				IsTerminal:  true,
			}},
		},
		{
			FromState: &failedState{},
			Transitions: []creditEntry{{
				Event:       &opDrive{},
				ToState:     &failedState{},
				Description: "terminal failure",
				IsTerminal:  true,
			}},
		},
	},
}
