package rounds

import (
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightningnetwork/lnd/keychain"
)

// State is a sealed interface for all states in the round state machine.
// Each state implements ProcessEvent to handle events and transition to the
// next state.
type State interface {
	protofsm.State[Event, OutboxEvent, *Environment]

	// stateSealed is an unexported method that marks this interface as
	// sealed, preventing external implementations.
	stateSealed()
}

// CreatedState is the initial state of the round. No clients have joined yet.
type CreatedState struct{}

// String returns a human-readable representation of CreatedState.
func (s *CreatedState) String() string {
	return "CreatedState"
}

// IsTerminal returns false as CreatedState is not a terminal state.
func (s *CreatedState) IsTerminal() bool {
	return false
}

// stateSealed marks CreatedState as implementing the sealed State interface.
func (s *CreatedState) stateSealed() {}

// IntentCollectingState is the state where the FSM is accepting client join
// requests. The FSM accumulates client requests until a SealEvent is
// received.
type IntentCollectingState struct {
	// ClientRegistrations maps client IDs to their registration data.
	// This allows tracking which client submitted which requests, so we
	// can send appropriate data back to each client later.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration
}

// String returns a human-readable representation of the IntentCollectingState.
func (s *IntentCollectingState) String() string {
	return "IntentCollectingState"
}

// IsTerminal returns false as IntentCollectingState is not a terminal state.
func (s *IntentCollectingState) IsTerminal() bool {
	return false
}

// stateSealed marks IntentCollectingState as implementing the sealed State
// interface.
func (s *IntentCollectingState) stateSealed() {}

// newIntentCollectingState creates a new IntentCollectingState with
// the given client registrations.
func newIntentCollectingState(
	regs map[clientconn.ClientID]*ClientRegistration,
) *IntentCollectingState {

	return &IntentCollectingState{
		ClientRegistrations: regs,
	}
}

// withNewClient returns a new IntentCollectingState with the given
// client added. The original state is not modified (immutable pattern).
func (s *IntentCollectingState) withNewClient(clientID clientconn.ClientID,
	result *JoinRequestResult) *IntentCollectingState {

	newRegs := make(map[clientconn.ClientID]*ClientRegistration)
	for id, reg := range s.ClientRegistrations {
		newRegs[id] = reg
	}

	newRegs[clientID] = newClientRegistration(clientID, result)

	return newIntentCollectingState(newRegs)
}

// isClientRegistered checks if a client is already registered in this round.
func (s *IntentCollectingState) isClientRegistered(
	clientID clientconn.ClientID) bool {

	_, exists := s.ClientRegistrations[clientID]

	return exists
}

// getAllBoardingInputs returns all boarding inputs from all clients.
func (s *IntentCollectingState) getAllBoardingInputs() []*BoardingInput {
	var all []*BoardingInput
	for _, reg := range s.ClientRegistrations {
		all = append(all, reg.BoardingInputs...)
	}

	return all
}

// QuoteStatus identifies whether a client has accepted, rejected, timed
// out, or is still pending on a seal-time quote. Values are set by the
// QuoteSentState event handler as nonces / rejects / timeouts arrive and
// read by tryResolve to decide whether to advance, reseal, or fail the
// round.
type QuoteStatus uint8

const (
	// QuotePending indicates the client has been sent a quote and the
	// server is still waiting for acceptance (a nonce) or an explicit
	// reject.
	QuotePending QuoteStatus = iota

	// QuoteAccepted indicates the client implicitly accepted the quote
	// by submitting the first MuSig2 nonce / forfeit signature for
	// this pass. Accepting clients roll forward into BatchBuildingState
	// with their buffered nonces replayed.
	QuoteAccepted

	// QuoteRejected indicates the client sent an explicit
	// JoinRoundReject for the active quote_id. Triggers a reseal over
	// the surviving accepted set, subject to MaxSealPasses and
	// MaxClientRejects.
	QuoteRejected

	// QuoteTimedOut indicates the client failed to respond within
	// QuoteTTL. Treated the same as a reject for reseal-decision
	// purposes, but the client's reject counter is NOT incremented
	// (timeouts can be network flakes).
	QuoteTimedOut
)

// String returns a human-readable representation of QuoteStatus.
func (s QuoteStatus) String() string {
	switch s {
	case QuotePending:
		return "pending"

	case QuoteAccepted:
		return "accepted"

	case QuoteRejected:
		return "rejected"

	case QuoteTimedOut:
		return "timed_out"

	default:
		return "unknown"
	}
}

// IsTerminal returns true when the status will not change further
// within a single seal pass.
func (s QuoteStatus) IsTerminal() bool {
	return s != QuotePending
}

// QuoteSentState is the state entered after SealEvent fires. A
// per-client JoinRoundQuote has been fanned out and the FSM is
// waiting for every pending client to explicitly accept (via
// JoinRoundAccept), explicitly reject (via JoinRoundReject), or time
// out. Acceptance is explicit — the VTXO tree does not exist yet at
// this point (it is constructed in BatchBuildingState from the
// accepted set), so clients cannot produce meaningful MuSig2 nonces
// until after this state resolves. Nonces and forfeit signatures
// flow through AwaitingVTXONoncesState / AwaitingInputSigsState in
// the normal post-batch-built order.
type QuoteSentState struct {
	// ClientRegistrations maps client IDs to their registration data.
	// Survivors of any reseal are filtered by seal_time_fee_builder so
	// this map only contains clients for which a quote was fanned out
	// in the current pass.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// Quotes maps client IDs to their current pass's quote. The
	// authoritative record of what the server offered each client; the
	// QuoteID is echoed back on accept / reject / timeout so stale
	// responses can be dropped after a reseal.
	Quotes map[clientconn.ClientID]*Quote

	// Status maps client IDs to their current quote status. Starts as
	// QuotePending for every registered client and flips as responses
	// arrive. Used by tryResolve to pick the post-wait transition.
	Status map[clientconn.ClientID]QuoteStatus

	// SealPass is the zero-indexed pass number. Incremented on every
	// reseal. When SealPass+1 would exceed MaxSealPasses the FSM
	// finalizes with the pass's accepted set instead of resealing
	// again.
	SealPass uint32

	// QuoteExpires is the wall-clock deadline after which pending
	// clients are flipped to QuoteTimedOut. Surfaced via
	// GetRoundStatus for admin observability.
	QuoteExpires time.Time

	// RejectCounts tracks per-client rejects across passes. A client
	// that hits MaxClientRejects is permanently dropped for the round
	// (forfeit / boarding locks released).
	RejectCounts map[clientconn.ClientID]uint32

	// DroppedClients records clients that have been permanently
	// dropped from the round (because of reject-cap exceedance or
	// reject-reason filtering). Their locks are released and their
	// registrations do not carry forward into the next pass.
	DroppedClients map[clientconn.ClientID]struct{}
}

// String returns a human-readable representation of QuoteSentState.
func (s *QuoteSentState) String() string {
	return "QuoteSentState"
}

// IsTerminal returns false as QuoteSentState is not a terminal state.
func (s *QuoteSentState) IsTerminal() bool {
	return false
}

// stateSealed marks QuoteSentState as implementing the sealed State
// interface.
func (s *QuoteSentState) stateSealed() {}

// allResolved returns true when every tracked client has left
// QuotePending (either by acceptance, rejection, or timeout).
func (s *QuoteSentState) allResolved() bool {
	for _, st := range s.Status {
		if st == QuotePending {
			return false
		}
	}

	return true
}

// acceptedClients returns the client IDs that accepted the current
// pass's quote (via nonce / sig arrival) and have not been dropped.
func (s *QuoteSentState) acceptedClients() []clientconn.ClientID {
	accepted := make([]clientconn.ClientID, 0, len(s.Status))
	for cid, st := range s.Status {
		if st != QuoteAccepted {
			continue
		}
		if _, dropped := s.DroppedClients[cid]; dropped {
			continue
		}

		accepted = append(accepted, cid)
	}

	return accepted
}

// hasAnyUnresolvedReject returns true when at least one client
// rejected or timed out on the current pass, which triggers a reseal
// (subject to MaxSealPasses).
func (s *QuoteSentState) hasAnyUnresolvedReject() bool {
	for _, st := range s.Status {
		if st == QuoteRejected || st == QuoteTimedOut {
			return true
		}
	}

	return false
}

// BatchBuildingState is a transitional state where the commitment transaction
// PSBT is being constructed. This state processes BuildBatchTxEvent to build
// the PSBT and immediately transitions to BatchBuiltState.
type BatchBuildingState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration
}

// String returns a human-readable representation of BatchBuildingState.
func (s *BatchBuildingState) String() string {
	return "BatchBuildingState"
}

// IsTerminal returns false as BatchBuildingState is not a terminal state.
func (s *BatchBuildingState) IsTerminal() bool {
	return false
}

// stateSealed marks BatchBuildingState as implementing the sealed State
// interface.
func (s *BatchBuildingState) stateSealed() {}

// BatchBuiltState holds the funded PSBT after successful construction.
// The PSBT contains boarding inputs and leave outputs, plus wallet inputs
// and change added by FundPsbt. All inputs are unsigned at this point.
type BatchBuiltState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// PSBT is the funded but unsigned commitment transaction.
	PSBT *psbt.Packet

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	// This is nil if no VTXOs exist in the round.
	VTXOTrees map[int]*tree.Tree

	// ConnectorTrees maps commitment tx output indices to connector trees.
	// This is nil if no forfeits exist in the round.
	ConnectorTrees map[int]*tree.Tree

	// ConnectorAssignments maps forfeited outpoints to connector leaves.
	// This is nil if no forfeits exist in the round.
	ConnectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment

	// ConnectorDescriptors describe connector outputs for this round.
	// This is nil if no forfeits exist in the round.
	ConnectorDescriptors []*ConnectorTreeDescriptor

	// ChangeOutputIdx is the PSBT output index where FundPsbt put
	// the wallet change, or -1 when no change output was added.
	// Propagated forward verbatim through every subsequent state
	// so FinalizedState can record it for ledger attribution.
	ChangeOutputIdx int32

	// LockedOutpoints lists the wallet UTXOs that were leased during
	// coin selection. Propagated forward so the failure path can
	// release them.
	LockedOutpoints []wire.OutPoint
}

// String returns a human-readable representation of BatchBuiltState.
func (s *BatchBuiltState) String() string {
	return "BatchBuiltState"
}

// IsTerminal returns false as BatchBuiltState is not a terminal state.
func (s *BatchBuiltState) IsTerminal() bool {
	return false
}

// stateSealed marks BatchBuiltState as implementing the sealed State interface.
func (s *BatchBuiltState) stateSealed() {}

// AwaitingInputSigsState waits for clients to submit their boarding
// input signatures. Each client must sign their boarding inputs so the
// commitment transaction can be finalized.
type AwaitingInputSigsState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// PSBT is the funded but unsigned commitment transaction.
	PSBT *psbt.Packet

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	// This is nil if no VTXOs exist in the round.
	VTXOTrees map[int]*tree.Tree

	// ConnectorTrees maps commitment tx output indices to connector trees.
	// This is nil if no forfeits exist in the round.
	ConnectorTrees map[int]*tree.Tree

	// ConnectorAssignments maps forfeited outpoints to connector leaves.
	// This is nil if no forfeits exist in the round.
	ConnectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment

	// ConnectorDescriptors describe connector outputs for this round.
	// This is nil if no forfeits exist in the round.
	ConnectorDescriptors []*ConnectorTreeDescriptor

	// ClientsSubmitted tracks which clients have submitted all expected
	// boarding signatures and forfeit transactions. Once all registered
	// clients have submitted, the round transitions to ServerSigningState.
	ClientsSubmitted map[clientconn.ClientID]struct{}

	// CollectedSignatures stores the boarding signatures submitted by each
	// client. These are validated but not yet applied to the PSBT - that
	// happens during server signing.
	CollectedSignatures InputSigsMap

	// CollectedForfeitTxs stores the forfeit transactions submitted by each
	// client. These are validated but not yet signed by the server.
	CollectedForfeitTxs ForfeitTxsMap

	// ChangeOutputIdx is the PSBT output index where FundPsbt put
	// the wallet change, or -1 when no change output was added.
	// Propagated forward verbatim.
	ChangeOutputIdx int32

	// LockedOutpoints lists the wallet UTXOs leased during coin
	// selection. Propagated forward for the failure path.
	LockedOutpoints []wire.OutPoint
}

// String returns a human-readable representation of
// AwaitingInputSigsState.
func (s *AwaitingInputSigsState) String() string {
	return "AwaitingInputSigsState"
}

// IsTerminal returns false as AwaitingInputSigsState is not a terminal
// state.
func (s *AwaitingInputSigsState) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingInputSigsState as implementing the sealed
// State interface.
func (s *AwaitingInputSigsState) stateSealed() {}

// requiresInputSubmission returns true when the client must submit boarding
// signatures and/or forfeit transactions before the round can progress.
func (s *AwaitingInputSigsState) requiresInputSubmission(
	clientID clientconn.ClientID,
) bool {

	reg, exists := s.ClientRegistrations[clientID]
	if !exists {
		return false
	}

	return len(reg.BoardingInputs) > 0 || len(reg.ForfeitInputs) > 0
}

// hasCompleteInputSubmission returns true once the client has submitted every
// required boarding signature and forfeit transaction for this round.
func (s *AwaitingInputSigsState) hasCompleteInputSubmission(
	clientID clientconn.ClientID,
) bool {

	reg, exists := s.ClientRegistrations[clientID]
	if !exists {
		return false
	}

	if len(reg.BoardingInputs) > 0 {
		sigs, ok := s.CollectedSignatures[clientID]
		if !ok || len(sigs) != len(reg.BoardingInputs) {
			return false
		}
	}

	if len(reg.ForfeitInputs) > 0 {
		forfeitTxs, ok := s.CollectedForfeitTxs[clientID]
		if !ok || len(forfeitTxs) != len(reg.ForfeitInputs) {
			return false
		}
	}

	return s.requiresInputSubmission(clientID)
}

// allClientsSubmitted returns true if all registered clients that need to
// submit boarding signatures and/or forfeit transactions have completed their
// submissions.
func (s *AwaitingInputSigsState) allClientsSubmitted() bool {
	requiredClients := 0
	for clientID := range s.ClientRegistrations {
		if s.requiresInputSubmission(clientID) {
			requiredClients++
		}
	}

	return len(s.ClientsSubmitted) >= requiredClients
}

// hasClientSubmitted checks if a client has already submitted their
// signatures.
func (s *AwaitingInputSigsState) hasClientSubmitted(
	clientID clientconn.ClientID) bool {

	_, exists := s.ClientsSubmitted[clientID]

	return exists
}

// AwaitingVTXONoncesState waits for clients to submit their MuSig2 nonces for
// VTXO tree transactions. This state is only entered if the round has VTXOs.
// Once all clients with VTXOs have submitted nonces, the FSM transitions to
// AwaitingVTXOSignaturesState for partial signature collection.
type AwaitingVTXONoncesState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// PSBT is the funded but unsigned commitment transaction.
	PSBT *psbt.Packet

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	VTXOTrees map[int]*tree.Tree

	// ConnectorTrees maps commitment tx output indices to connector trees.
	// This is nil if no forfeits exist in the round.
	ConnectorTrees map[int]*tree.Tree

	// ConnectorAssignments maps forfeited outpoints to connector leaves.
	// This is nil if no forfeits exist in the round.
	ConnectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment

	// TreeSignCoordinators maps commitment tx output indices to their
	// MuSig2 signing coordinators. Each coordinator manages nonce and
	// signature collection for one VTXO tree.
	TreeSignCoordinators map[int]*batch.TreeSignCoordinator

	// ClientsWithNonces tracks which clients have submitted nonces.
	ClientsWithNonces map[clientconn.ClientID]struct{}

	// ChangeOutputIdx is the PSBT output index where FundPsbt put
	// the wallet change, or -1 when no change output was added.
	// Propagated forward verbatim.
	ChangeOutputIdx int32

	// LockedOutpoints lists the wallet UTXOs leased during coin
	// selection. Propagated forward for the failure path.
	LockedOutpoints []wire.OutPoint
}

// String returns a human-readable representation of AwaitingVTXONoncesState.
func (s *AwaitingVTXONoncesState) String() string {
	return "AwaitingVTXONoncesState"
}

// IsTerminal returns false as AwaitingVTXONoncesState is not a terminal state.
func (s *AwaitingVTXONoncesState) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingVTXONoncesState as implementing the sealed State
// interface.
func (s *AwaitingVTXONoncesState) stateSealed() {}

// allClientsSubmittedNonces returns true if all registered clients with VTXOs
// have submitted their nonces.
func (s *AwaitingVTXONoncesState) allClientsSubmittedNonces() bool {
	for clientID, reg := range s.ClientRegistrations {
		if len(reg.VTXODescriptors) == 0 {
			continue
		}

		if !s.hasClientSubmittedNonces(clientID) {
			return false
		}
	}

	return true
}

// hasClientSubmittedNonces checks if a client has submitted any nonces.
func (s *AwaitingVTXONoncesState) hasClientSubmittedNonces(
	clientID clientconn.ClientID) bool {

	_, exists := s.ClientsWithNonces[clientID]

	return exists
}

// AwaitingVTXOSignaturesState waits for clients to submit their MuSig2 partial
// signatures for VTXO tree transactions. This state is entered after all
// clients with VTXOs have submitted their nonces and the aggregated nonces have
// been distributed. Once all clients have submitted partial signatures, the
// final signatures are aggregated and distributed, then the FSM transitions to
// AwaitingInputSigsState.
type AwaitingVTXOSignaturesState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// PSBT is the funded but unsigned commitment transaction.
	PSBT *psbt.Packet

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	VTXOTrees map[int]*tree.Tree

	// ConnectorTrees maps commitment tx output indices to connector trees.
	// This is nil if no forfeits exist in the round.
	ConnectorTrees map[int]*tree.Tree

	// ConnectorAssignments maps forfeited outpoints to connector leaves.
	// This is nil if no forfeits exist in the round.
	ConnectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment

	// TreeSignCoordinators maps commitment tx output indices to their
	// MuSig2 signing coordinators. Each coordinator manages signature
	// collection for one VTXO tree.
	TreeSignCoordinators map[int]*batch.TreeSignCoordinator

	// ClientsWithSignatures tracks which clients have submitted their
	// partial signatures.
	ClientsWithSignatures map[clientconn.ClientID]struct{}

	// ChangeOutputIdx is the PSBT output index where FundPsbt put
	// the wallet change, or -1 when no change output was added.
	// Propagated forward verbatim.
	ChangeOutputIdx int32

	// LockedOutpoints lists the wallet UTXOs leased during coin
	// selection. Propagated forward for the failure path.
	LockedOutpoints []wire.OutPoint
}

// String returns a human-readable representation of
// AwaitingVTXOSignaturesState.
func (s *AwaitingVTXOSignaturesState) String() string {
	return "AwaitingVTXOSignaturesState"
}

// IsTerminal returns false as AwaitingVTXOSignaturesState is not a terminal
// state.
func (s *AwaitingVTXOSignaturesState) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingVTXOSignaturesState as implementing the sealed
// State interface.
func (s *AwaitingVTXOSignaturesState) stateSealed() {}

// allClientsSubmittedSignatures returns true if all registered clients with
// VTXOs have submitted their partial signatures.
func (s *AwaitingVTXOSignaturesState) allClientsSubmittedSignatures() bool {
	for clientID, reg := range s.ClientRegistrations {
		if len(reg.VTXODescriptors) == 0 {
			continue
		}

		if !s.hasClientSubmittedSignatures(clientID) {
			return false
		}
	}

	return true
}

// hasClientSubmittedSignatures checks if a client has already submitted any
// partial signatures.
func (s *AwaitingVTXOSignaturesState) hasClientSubmittedSignatures(
	clientID clientconn.ClientID) bool {

	_, exists := s.ClientsWithSignatures[clientID]

	return exists
}

// ServerSigningState is where the server signs its wallet inputs on the
// commitment transaction. This occurs after all clients have submitted their
// boarding input signatures.
type ServerSigningState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// PSBT is the funded but unsigned commitment transaction.
	PSBT *psbt.Packet

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	VTXOTrees map[int]*tree.Tree

	// ConnectorAssignments maps forfeited outpoints to connector leaves.
	// This is nil if no forfeits exist in the round.
	ConnectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment

	// ConnectorDescriptors describe connector outputs for this round.
	// This is nil if no forfeits exist in the round.
	ConnectorDescriptors []*ConnectorTreeDescriptor

	// CollectedSignatures contains all validated client boarding
	// signatures. These will be applied to the PSBT along with the
	// server's signatures.
	CollectedSignatures InputSigsMap

	// CollectedForfeitTxs contains all validated client forfeit
	// transactions. The server will sign these before finalization.
	CollectedForfeitTxs ForfeitTxsMap

	// ConnectorTrees maps commitment tx output indices to connector
	// trees. Carried forward into ServerSigningState so the
	// FinalizedState transition can record the connector output
	// indices alongside the change index for ledger attribution.
	ConnectorTrees map[int]*tree.Tree

	// ChangeOutputIdx is the PSBT output index where FundPsbt put
	// the wallet change, or -1 when no change output was added.
	// Propagated forward verbatim.
	ChangeOutputIdx int32

	// LockedOutpoints lists the wallet UTXOs leased during coin
	// selection. Propagated forward for the failure path.
	LockedOutpoints []wire.OutPoint
}

// String returns a human-readable representation of ServerSigningState.
func (s *ServerSigningState) String() string {
	return "ServerSigningState"
}

// IsTerminal returns false as ServerSigningState is not a terminal state.
func (s *ServerSigningState) IsTerminal() bool {
	return false
}

// stateSealed marks ServerSigningState as implementing the sealed State
// interface.
func (s *ServerSigningState) stateSealed() {}

// FinalizedState holds the fully signed transaction ready for broadcast. The
// transaction has all boarding input signatures (client + operator) and wallet
// input signatures applied.
type FinalizedState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// FinalTx is the fully signed commitment transaction ready for
	// broadcast.
	FinalTx *wire.MsgTx

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	VTXOTrees map[int]*tree.Tree

	// ForfeitInfos maps forfeited VTXO outpoints to forfeit metadata.
	ForfeitInfos map[wire.OutPoint]*ForfeitInfo

	// ChangeOutputIdx is the FinalTx output index that holds the
	// wallet change added by FundPsbt, or -1 when the round produced
	// no change (VTXO amounts + mining fee == funding value). Used by
	// the ledger notify path to pre-attribute the change output to
	// the round so the UTXO diff classifier does not double-book an
	// external_deposit on top of RecordCapitalCommitted.
	ChangeOutputIdx int32

	// ConnectorOutputIndices is the sorted set of FinalTx output
	// indices that hold operator-controlled connector outputs (dust
	// outputs spent by forfeit transactions). Captured here so the
	// ledger notify path can attribute them alongside the change
	// output without reconstructing the PSBT. Empty when the round
	// has no forfeits.
	ConnectorOutputIndices []int32

	// MiningFeeSat is the absolute on-chain fee paid for the
	// commitment transaction, computed from the PSBT as
	// sum(PInput.WitnessUtxo.Value) - sum(TxOut.Value) at the
	// ServerSigning -> Finalized transition where the PSBT is
	// still in scope. Propagated through to the ledger notify
	// path so handleRoundConfirmed can book the mining_fees
	// expense leg against treasury_wallet. Zero when the FSM is
	// reloaded from persistence without the PSBT (the ledger
	// handler skips the leg on zero).
	MiningFeeSat int64

	// SweepKey captures the operator key descriptor that derived the
	// sweep tapleaf committed in this round's VTXO trees at finalization
	// time. It is threaded through to ConfirmedState and on to the batch
	// watcher so the per-batch sweeper signs the timeout spend with the
	// historical key descriptor rather than whatever sweep key the
	// rounds actor happens to be configured with at restart. A nil
	// PubKey signals "locator unknown" -- e.g. a pre-migration round
	// reloaded without a persisted locator -- and downstream callers
	// must refuse to fall back to the configured key when its pubkey
	// no longer matches the persisted one.
	SweepKey keychain.KeyDescriptor
}

// String returns a human-readable representation of FinalizedState.
func (s *FinalizedState) String() string {
	return "FinalizedState"
}

// IsTerminal returns false as FinalizedState is not a terminal state. The
// round waits for confirmation before completing.
func (s *FinalizedState) IsTerminal() bool {
	return false
}

// stateSealed marks FinalizedState as implementing the sealed State interface.
func (s *FinalizedState) stateSealed() {}

// FailedState is a terminal state indicating the round has failed. When
// entering this state, the FSM emits events to notify clients, unlock
// boarding inputs, and inform the actor of the failure.
type FailedState struct {
	// Reason describes why the round failed.
	Reason string
}

// String returns a human-readable representation of FailedState.
func (s *FailedState) String() string {
	return "FailedState"
}

// IsTerminal returns true as FailedState is a terminal state.
func (s *FailedState) IsTerminal() bool {
	return true
}

// stateSealed marks FailedState as implementing the sealed State interface.
func (s *FailedState) stateSealed() {}

// ConfirmedState is a terminal state reached after the commitment transaction
// has been confirmed on-chain with the required number of confirmations.
type ConfirmedState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// FinalTx is the fully signed commitment transaction.
	FinalTx *wire.MsgTx

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	// This is nil if no VTXOs exist in the round.
	VTXOTrees map[int]*tree.Tree

	// BlockHeight is the height of the block containing the transaction.
	BlockHeight int32

	// BlockHash is the hash of the block containing the transaction.
	BlockHash chainhash.Hash

	// SweepKey carries the operator key descriptor that derived the
	// sweep tapleaf committed in VTXOTrees, propagated from
	// FinalizedState so the post-confirmation batch-watcher
	// registration uses the historical descriptor rather than the
	// currently-configured sweep key. A nil PubKey signals
	// "locator unknown" for a pre-migration round.
	SweepKey keychain.KeyDescriptor
}

// String returns a human-readable representation of ConfirmedState.
func (s *ConfirmedState) String() string {
	return "ConfirmedState"
}

// IsTerminal returns true as ConfirmedState is a terminal state.
func (s *ConfirmedState) IsTerminal() bool {
	return true
}

// stateSealed marks ConfirmedState as implementing the sealed State interface.
func (s *ConfirmedState) stateSealed() {}
