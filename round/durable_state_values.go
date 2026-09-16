package round

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
)

// Persisted state kinds are independent of display names and transition order.
const (
	snapshotIdle                       uint8 = 1
	snapshotPendingRoundAssembly       uint8 = 2
	snapshotIntentSentState            uint8 = 3
	snapshotQuoteReceivedState         uint8 = 4
	snapshotRoundJoinedState           uint8 = 5
	snapshotCommitmentTxReceivedState  uint8 = 6
	snapshotCommitmentTxValidatedState uint8 = 7
	snapshotForfeitCollecting          uint8 = 8
	snapshotNoncesSentState            uint8 = 9
	snapshotNoncesAggregatedState      uint8 = 10
	snapshotPartialSigsSentState       uint8 = 11
	snapshotInputSigSentState          uint8 = 12
	snapshotConfirmedState             uint8 = 13
	snapshotClientFailedState          uint8 = 14
	snapshotRecoveryInitiatedState     uint8 = 15
	snapshotServiceReconcileState      uint8 = 16
)

// clientStateValues holds all serializable round state. MuSig2 session objects
// belong to the signer process and are deliberately excluded. Restore reports
// that loss explicitly so the actor must reconcile before further signing.
type clientStateValues struct {
	kind              uint8
	roundID           RoundID
	intents           Intents
	quote             *ClientQuote
	commitment        *psbt.Packet
	txID              chainhash.Hash
	trees             map[int]*tree.Tree
	assetLeaves       map[wire.OutPoint][]byte
	treeKey           *btcec.PublicKey
	connectorKey      *btcec.PublicKey
	sweepKey          *btcec.PublicKey
	sweepDelay        uint32
	flowVersion       roundpb.FlowVersion
	forfeitKey        *btcec.PublicKey
	clientTrees       map[SignerKey]*tree.Tree
	boardingIndices   map[wire.OutPoint]int
	forfeitMappings   map[wire.OutPoint]*ConnectorLeafInfo
	collectedForfeits map[wire.OutPoint]*ForfeitSignatureResponse
	aggNonces         map[tree.TxID]tree.Musig2PubNonce
	inputSigs         []*types.BoardingInputSignature
	forfeited         []wire.OutPoint
	failure           *BoardingFailed
	probes            uint32
	blockHeight       int32
	blockHash         chainhash.Hash
	confirmations     int32
	vtxos             []*ClientVTXO
	recoveryOutpoint  wire.OutPoint
	sweepTxID         chainhash.Hash
	reason            string
	cold              bool
}

// clientStateSnapshot extracts state without mutating protocol or wallet data.
func clientStateSnapshot(state ClientState) (clientStateValues, error) {
	switch s := state.(type) {
	case *Idle:
		return clientStateValues{
			kind: snapshotIdle,
		}, nil

	case *PendingRoundAssembly:
		return clientStateValues{
			kind: snapshotPendingRoundAssembly,
			intents: Intents{Service: s.Service,
				Boarding: s.Boarding,
				VTXOs:    s.VTXOs,
				Forfeits: s.Forfeits,
				Leaves:   s.Leaves},
		}, nil

	case *IntentSentState:
		return clientStateValues{
			kind:    snapshotIntentSentState,
			roundID: s.AdmittedRoundID,
			intents: s.Intents,
		}, nil

	case *QuoteReceivedState:
		return clientStateValues{
			kind:    snapshotQuoteReceivedState,
			roundID: s.RoundID,
			quote:   s.Quote,
			intents: s.Intents,
		}, nil

	case *RoundJoinedState:
		return clientStateValues{
			kind:    snapshotRoundJoinedState,
			roundID: s.RoundID,
			intents: s.Intents,
			quote:   s.Quote,
		}, nil

	case *CommitmentTxReceivedState:
		return clientStateValues{
			kind:         snapshotCommitmentTxReceivedState,
			roundID:      s.RoundID,
			commitment:   s.CommitmentTx,
			trees:        s.VTXOTreePaths,
			sweepDelay:   s.SweepDelay,
			flowVersion:  s.FlowVersion,
			forfeitKey:   s.ForfeitKey,
			intents:      s.Intents,
			clientTrees:  s.ClientTrees,
			txID:         s.TxID,
			assetLeaves:  s.AssetLeafPackages,
			treeKey:      s.TreeCosignKey,
			connectorKey: s.ConnectorOperatorKey,
			sweepKey:     s.SweepKey,
			quote:        s.Quote,
		}, nil

	case *CommitmentTxValidatedState:
		return clientStateValues{
			kind:            snapshotCommitmentTxValidatedState,
			roundID:         s.RoundID,
			commitment:      s.CommitmentTx,
			trees:           s.VTXOTreePaths,
			sweepDelay:      s.SweepDelay,
			flowVersion:     s.FlowVersion,
			forfeitKey:      s.ForfeitKey,
			intents:         s.Intents,
			clientTrees:     s.ClientTrees,
			boardingIndices: s.BoardingInputIndices,
			forfeitMappings: s.ForfeitMappings,
		}, nil

	case *ForfeitSignaturesCollectingState:
		return clientStateValues{
			kind:              snapshotForfeitCollecting,
			roundID:           s.RoundID,
			commitment:        s.CommitmentTx,
			trees:             s.VTXOTreePaths,
			sweepDelay:        s.SweepDelay,
			flowVersion:       s.FlowVersion,
			forfeitKey:        s.ForfeitKey,
			intents:           s.Intents,
			clientTrees:       s.ClientTrees,
			boardingIndices:   s.BoardingInputIndices,
			forfeitMappings:   s.ExpectedForfeits,
			collectedForfeits: s.CollectedForfeits,
		}, nil

	case *NoncesSentState:
		return clientStateValues{
			kind:            snapshotNoncesSentState,
			roundID:         s.RoundID,
			commitment:      s.CommitmentTx,
			trees:           s.VTXOTreePaths,
			sweepDelay:      s.SweepDelay,
			flowVersion:     s.FlowVersion,
			forfeitKey:      s.ForfeitKey,
			intents:         s.Intents,
			clientTrees:     s.ClientTrees,
			boardingIndices: s.BoardingInputIndices,
			forfeitMappings: s.ForfeitMappings,
		}, nil

	case *NoncesAggregatedState:
		return clientStateValues{
			kind:            snapshotNoncesAggregatedState,
			roundID:         s.RoundID,
			commitment:      s.CommitmentTx,
			trees:           s.VTXOTreePaths,
			sweepDelay:      s.SweepDelay,
			flowVersion:     s.FlowVersion,
			forfeitKey:      s.ForfeitKey,
			intents:         s.Intents,
			clientTrees:     s.ClientTrees,
			boardingIndices: s.BoardingInputIndices,
			forfeitMappings: s.ForfeitMappings,
			aggNonces:       s.AggNonces,
		}, nil

	case *PartialSigsSentState:
		return clientStateValues{
			kind:            snapshotPartialSigsSentState,
			roundID:         s.RoundID,
			commitment:      s.CommitmentTx,
			trees:           s.VTXOTreePaths,
			sweepDelay:      s.SweepDelay,
			flowVersion:     s.FlowVersion,
			forfeitKey:      s.ForfeitKey,
			intents:         s.Intents,
			clientTrees:     s.ClientTrees,
			boardingIndices: s.BoardingInputIndices,
			forfeitMappings: s.ForfeitMappings,
		}, nil

	case *InputSigSentState:
		return clientStateValues{
			kind:        snapshotInputSigSentState,
			roundID:     s.RoundID,
			commitment:  s.CommitmentTx,
			trees:       s.VTXOTreePaths,
			sweepDelay:  s.SweepDelay,
			flowVersion: s.FlowVersion,
			forfeitKey:  s.ForfeitKey,
			intents:     s.Intents,
			clientTrees: s.ClientTrees,
			inputSigs:   s.InputSigs,
			forfeited:   s.ForfeitedVTXOs,
			failure:     s.PendingFailure,
			probes:      s.ReconcileProbes,
		}, nil

	case *ConfirmedState:
		return clientStateValues{
			kind:          snapshotConfirmedState,
			txID:          s.TxID,
			blockHeight:   s.BlockHeight,
			blockHash:     s.BlockHash,
			confirmations: s.Confirmations,
			vtxos:         s.VTXOs,
		}, nil

	case *ClientFailedState:
		return clientStateValues{
			kind: snapshotClientFailedState,
			failure: &BoardingFailed{Reason: s.Reason,
				Error:       s.Error,
				Recoverable: s.Recoverable,
				FailureCode: s.FailureCode},
		}, nil

	case *RecoveryInitiatedState:
		return clientStateValues{
			kind:             snapshotRecoveryInitiatedState,
			recoveryOutpoint: s.Outpoint,
			sweepTxID:        s.SweepTxID,
			reason:           s.Reason,
		}, nil

	case *ServiceReconcileState:
		return clientStateValues{
			kind:    snapshotServiceReconcileState,
			cold:    s.Cold,
			intents: s.Intents,
			roundID: s.RoundID,
			probes:  s.Probes,
		}, nil

	default:
		return clientStateValues{}, fmt.Errorf("unsupported client "+
			"snapshot state %T", state)
	}
}

// clientState restores the concrete state and reports lost signer sessions.
// A true result is a mandatory recovery boundary, never permission to derive
// replacement nonces or continue signing the old attempt.
func (v clientStateValues) clientState() (ClientState, bool, error) {
	switch v.kind {
	case snapshotIdle:
		return &Idle{}, false, nil

	case snapshotPendingRoundAssembly:
		return &PendingRoundAssembly{
			Service:  v.intents.Service,
			Boarding: v.intents.Boarding,
			VTXOs:    v.intents.VTXOs,
			Forfeits: v.intents.Forfeits,
			Leaves:   v.intents.Leaves,
		}, false, nil

	case snapshotIntentSentState:
		return &IntentSentState{
			AdmittedRoundID: v.roundID,
			Intents:         v.intents,
		}, false, nil

	case snapshotQuoteReceivedState:
		return &QuoteReceivedState{
			RoundID: v.roundID,
			Quote:   v.quote,
			Intents: v.intents,
		}, false, nil

	case snapshotRoundJoinedState:
		return &RoundJoinedState{
			RoundID: v.roundID,
			Intents: v.intents,
			Quote:   v.quote,
		}, false, nil

	case snapshotCommitmentTxReceivedState:
		return &CommitmentTxReceivedState{
			RoundID:              v.roundID,
			CommitmentTx:         v.commitment,
			VTXOTreePaths:        v.trees,
			SweepDelay:           v.sweepDelay,
			FlowVersion:          v.flowVersion,
			ForfeitKey:           v.forfeitKey,
			Intents:              v.intents,
			ClientTrees:          v.clientTrees,
			TxID:                 v.txID,
			AssetLeafPackages:    v.assetLeaves,
			TreeCosignKey:        v.treeKey,
			ConnectorOperatorKey: v.connectorKey,
			SweepKey:             v.sweepKey,
			Quote:                v.quote,
		}, false, nil

	case snapshotCommitmentTxValidatedState:
		return &CommitmentTxValidatedState{
			RoundID:              v.roundID,
			CommitmentTx:         v.commitment,
			VTXOTreePaths:        v.trees,
			SweepDelay:           v.sweepDelay,
			FlowVersion:          v.flowVersion,
			ForfeitKey:           v.forfeitKey,
			Intents:              v.intents,
			ClientTrees:          v.clientTrees,
			BoardingInputIndices: v.boardingIndices,
			ForfeitMappings:      v.forfeitMappings,
		}, false, nil

	case snapshotForfeitCollecting:
		return &ForfeitSignaturesCollectingState{
			RoundID:              v.roundID,
			CommitmentTx:         v.commitment,
			VTXOTreePaths:        v.trees,
			SweepDelay:           v.sweepDelay,
			FlowVersion:          v.flowVersion,
			ForfeitKey:           v.forfeitKey,
			Intents:              v.intents,
			ClientTrees:          v.clientTrees,
			BoardingInputIndices: v.boardingIndices,
			ExpectedForfeits:     v.forfeitMappings,
			CollectedForfeits:    v.collectedForfeits,
		}, false, nil

	case snapshotNoncesSentState:
		return &NoncesSentState{
			RoundID:              v.roundID,
			CommitmentTx:         v.commitment,
			VTXOTreePaths:        v.trees,
			SweepDelay:           v.sweepDelay,
			FlowVersion:          v.flowVersion,
			ForfeitKey:           v.forfeitKey,
			Intents:              v.intents,
			ClientTrees:          v.clientTrees,
			BoardingInputIndices: v.boardingIndices,
			ForfeitMappings:      v.forfeitMappings,
		}, true, nil

	case snapshotNoncesAggregatedState:
		return &NoncesAggregatedState{
			RoundID:              v.roundID,
			CommitmentTx:         v.commitment,
			VTXOTreePaths:        v.trees,
			SweepDelay:           v.sweepDelay,
			FlowVersion:          v.flowVersion,
			ForfeitKey:           v.forfeitKey,
			Intents:              v.intents,
			ClientTrees:          v.clientTrees,
			BoardingInputIndices: v.boardingIndices,
			ForfeitMappings:      v.forfeitMappings,
			AggNonces:            v.aggNonces,
		}, true, nil

	case snapshotPartialSigsSentState:
		return &PartialSigsSentState{
			RoundID:              v.roundID,
			CommitmentTx:         v.commitment,
			VTXOTreePaths:        v.trees,
			SweepDelay:           v.sweepDelay,
			FlowVersion:          v.flowVersion,
			ForfeitKey:           v.forfeitKey,
			Intents:              v.intents,
			ClientTrees:          v.clientTrees,
			BoardingInputIndices: v.boardingIndices,
			ForfeitMappings:      v.forfeitMappings,
		}, true, nil

	case snapshotInputSigSentState:
		return &InputSigSentState{
			RoundID:         v.roundID,
			CommitmentTx:    v.commitment,
			VTXOTreePaths:   v.trees,
			SweepDelay:      v.sweepDelay,
			FlowVersion:     v.flowVersion,
			ForfeitKey:      v.forfeitKey,
			Intents:         v.intents,
			ClientTrees:     v.clientTrees,
			InputSigs:       v.inputSigs,
			ForfeitedVTXOs:  v.forfeited,
			PendingFailure:  v.failure,
			ReconcileProbes: v.probes,
		}, false, nil

	case snapshotConfirmedState:
		return &ConfirmedState{
			TxID:          v.txID,
			BlockHeight:   v.blockHeight,
			BlockHash:     v.blockHash,
			Confirmations: v.confirmations,
			VTXOs:         v.vtxos,
		}, false, nil

	case snapshotClientFailedState:
		if v.failure == nil {
			return nil, false, fmt.Errorf("missing failed state " +
				"details")
		}

		return &ClientFailedState{
			Reason:      v.failure.Reason,
			Error:       v.failure.Error,
			Recoverable: v.failure.Recoverable,
			FailureCode: v.failure.FailureCode,
		}, false, nil

	case snapshotRecoveryInitiatedState:
		return &RecoveryInitiatedState{
			Outpoint:  v.recoveryOutpoint,
			SweepTxID: v.sweepTxID,
			Reason:    v.reason,
		}, false, nil

	case snapshotServiceReconcileState:
		return &ServiceReconcileState{
			Cold:    v.cold,
			Intents: v.intents,
			RoundID: v.roundID,
			Probes:  v.probes,
		}, false, nil

	default:
		return nil, false, fmt.Errorf("unknown client snapshot "+
			"state %d", v.kind)
	}
}
