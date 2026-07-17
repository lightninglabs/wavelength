package vtxo

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx"
	"github.com/lightninglabs/wavelength/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ProcessEvent handles events in LiveState. The VTXO monitors block epochs for
// expiry and can receive forfeit requests from the round actor.
func (s *LiveState) ProcessEvent(ctx context.Context, event VTXOEvent,
	env *VTXOEnvironment) (*VTXOStateTransition, error) {

	switch evt := event.(type) {
	case *BlockEpochEvent:
		return s.handleBlockEpoch(ctx, evt, env)

	case *SpendReserveEvent:
		return s.handleSpendReserve(ctx, env)

	case *PendingForfeitEvent:
		return s.handlePendingForfeit(ctx, env)

	case *ForfeitRequestEvent:
		return s.handleForfeitRequest(ctx, evt, env)

	case *ResumeVTXOEvent:
		// On resume, stay in LiveState and re-check expiry on next
		// block.
		return &VTXOStateTransition{
			NextState: s,
		}, nil

	case *ForceUnrollEvent:
		return s.handleForceUnroll(ctx, evt)

	case *ExitFailedEvent, *ExitConfirmedEvent:
		// A duplicate or stale exit-outcome event for a VTXO that is
		// already live (e.g. boot reconciliation re-delivering a
		// recovery that already landed). Idempotent no-op: the VTXO is
		// live, which is the recovered state. ExitConfirmedEvent should
		// never reach a live VTXO, but ignoring it is safer than
		// retiring a live coin to spent on a stray signal.
		return &VTXOStateTransition{
			NextState: s,
		}, nil

	case *VTXOFailedEvent:
		return &VTXOStateTransition{
			NextState: &FailedState{
				VTXO:        s.VTXO,
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
			},
		}, nil

	default:
		return nil, fmt.Errorf("live: unexpected event: %T", event)
	}
}

// handlePendingForfeit commits this VTXO to cooperative consumption before the
// round has concrete connector details. The VTXO becomes unavailable for other
// operations while awaiting the later ForfeitRequestEvent.
func (s *LiveState) handlePendingForfeit(_ context.Context,
	_ *VTXOEnvironment) (*VTXOStateTransition, error) {

	return &VTXOStateTransition{
		NextState: &PendingForfeitState{
			VTXO:              s.VTXO,
			RequestedAtHeight: 0,
		},
		NewEvents: fn.Some(VTXOEmittedEvent{
			Outbox: []VTXOOutMsg{
				&VTXOStatusUpdate{
					Outpoint:  s.VTXO.Outpoint,
					NewStatus: VTXOStatusPendingForfeit,
				},
			},
		}),
	}, nil
}

// handleSpendReserve claims this VTXO for an out-of-round (OOR) spend. The
// VTXO enters SpendingState and becomes unavailable for cooperative forfeit
// until the spend completes or is released.
func (s *LiveState) handleSpendReserve(_ context.Context, _ *VTXOEnvironment) (
	*VTXOStateTransition, error) {

	return &VTXOStateTransition{
		NextState: &SpendingState{
			VTXO:              s.VTXO,
			LastCheckedHeight: s.LastCheckedHeight,
		},
		NewEvents: fn.Some(VTXOEmittedEvent{
			Outbox: []VTXOOutMsg{
				&VTXOStatusUpdate{
					Outpoint:  s.VTXO.Outpoint,
					NewStatus: VTXOStatusSpending,
				},
			},
		}),
	}, nil
}

// handleForceUnroll processes a manual unroll request. It produces the same
// transition as critical expiry, converging both manual and automatic paths
// on the same chain resolver seam.
func (s *LiveState) handleForceUnroll(_ context.Context,
	evt *ForceUnrollEvent) (*VTXOStateTransition, error) {

	reason := evt.Reason
	if reason == "" {
		reason = "manual unroll"
	}

	// Hand the VTXO to the chain resolver and mark it exiting, but do NOT
	// emit a VTXOTerminatedNotification: UnilateralExitState is
	// non-terminal now, so the actor stays alive to observe the exit
	// outcome and recover the VTXO if the unroll fails without an on-chain
	// footprint (wavelength#602).
	outbox := []VTXOOutMsg{
		&ExpiringNotification{
			VTXO:            s.VTXO,
			BlocksRemaining: 0,
			Reason:          reason,
			Trigger:         evt.Trigger,
			ExitPolicy:      evt.ExitPolicy,
		},
		&VTXOStatusUpdate{
			Outpoint:  s.VTXO.Outpoint,
			NewStatus: VTXOStatusUnilateralExit,
		},
	}

	return &VTXOStateTransition{
		NextState: &UnilateralExitState{
			VTXO:              s.VTXO,
			Reason:            reason,
			LastCheckedHeight: s.LastCheckedHeight,
		},
		NewEvents: fn.Some(VTXOEmittedEvent{Outbox: outbox}),
	}, nil
}

// handleBlockEpoch processes a new block notification and checks if the VTXO
// needs to be forfeited cooperatively or escalated to unilateral exit.
func (s *LiveState) handleBlockEpoch(_ context.Context, evt *BlockEpochEvent,
	env *VTXOEnvironment) (*VTXOStateTransition, error) {

	s.LastCheckedHeight = evt.Height

	expiryStatus := env.ExpiryConfig.CheckExpiry(s.VTXO, evt.Height)

	switch expiryStatus {
	case ExpiryStatusSafe:
		// Nothing to do, stay in LiveState.
		return &VTXOStateTransition{
			NextState: s,
		}, nil

	case ExpiryStatusNeedsRefresh:
		// Request cooperative forfeit before expiry becomes critical.
		// LastCheckedHeight carries the current block height into
		// the outbox so the actor's operator-fee quoter can compute
		// remaining-blocks without reading its own state (which the
		// Receive loop has already advanced to PendingForfeitState
		// by the time the outbox is dispatched).
		outbox := []VTXOOutMsg{
			&ForfeitRequest{
				VTXOOutpoint:      s.VTXO.Outpoint,
				LastCheckedHeight: evt.Height,
			},
			&VTXOStatusUpdate{
				Outpoint:  s.VTXO.Outpoint,
				NewStatus: VTXOStatusPendingForfeit,
			},
		}

		return &VTXOStateTransition{
			NextState: &PendingForfeitState{
				VTXO:              s.VTXO,
				RequestedAtHeight: evt.Height,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{Outbox: outbox}),
		}, nil

	case ExpiryStatusCritical:
		// Escalate to chain resolver for unilateral exit.
		blocksRemaining := BlocksUntilExpiry(s.VTXO, evt.Height)
		reason := fmt.Sprintf("critical expiry: %d blocks remaining",
			blocksRemaining)

		// Keep the actor alive in the non-terminal exit state (no
		// VTXOTerminatedNotification) so a failed unroll can be rolled
		// back to live; see wavelength#602.
		outbox := []VTXOOutMsg{
			&ExpiringNotification{
				VTXO:            s.VTXO,
				BlocksRemaining: blocksRemaining,
				Reason:          "batch expiry imminent",
			},
			&VTXOStatusUpdate{
				Outpoint:  s.VTXO.Outpoint,
				NewStatus: VTXOStatusUnilateralExit,
			},
		}

		return &VTXOStateTransition{
			NextState: &UnilateralExitState{
				VTXO:              s.VTXO,
				Reason:            reason,
				LastCheckedHeight: evt.Height,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{Outbox: outbox}),
		}, nil

	case ExpiryStatusExpired:
		// Batch has expired - this should not happen if monitoring
		// works correctly.
		return &VTXOStateTransition{
			NextState: &FailedState{
				VTXO: s.VTXO,
				Reason: "batch expired before " +
					"cooperative forfeit",
				Recoverable: false,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unknown expiry: %d", expiryStatus)
	}
}

// handleForfeitRequest builds and signs a forfeit transaction when the round
// actor requests this VTXO be forfeited as part of a batch swap. The forfeit
// atomically links the old VTXO to a new round via the connector output - if
// the new commitment tx confirms, the forfeit becomes valid and pays the VTXO
// value to the operator. This prevents double-spending by ensuring the client
// cannot claim both the old VTXO and a new one in the fresh round.
func (s *LiveState) handleForfeitRequest(ctx context.Context,
	evt *ForfeitRequestEvent, env *VTXOEnvironment) (*VTXOStateTransition,
	error) {

	forfeitSpend, err := resolveForfeitSpendPath(s.VTXO, evt)
	if err != nil {
		return nil, fmt.Errorf("resolve forfeit spend path: %w", err)
	}

	forfeitTx, err := tx.BuildForfeitTxWithContext(
		&s.VTXO.Outpoint, s.VTXO.Amount,
		&evt.ConnectorOutpoint,
		btcutil.Amount(evt.ConnectorAmount),
		evt.ServerForfeitPkScript,
		tx.ForfeitTxContext{
			VTXOSequence: forfeitSpend.RequiredSequence,
			LockTime:     forfeitSpend.RequiredLockTime,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build custom forfeit tx: %w",
			err)
	}

	// Sign our portion of the collaborative 2-of-2 spend on the VTXO input.
	// The operator will complete the multisig witness with their signature.
	sig, err := signForfeitVTXOInput(
		s.VTXO, forfeitSpend, evt, forfeitTx, env,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to sign forfeit tx: %w", err)
	}
	externalSigs, err := externalForfeitParticipantSigs(
		ctx, s.VTXO, forfeitSpend, evt, forfeitTx, env,
	)
	if err != nil {
		return nil, fmt.Errorf("external participant signatures: %w",
			err)
	}
	participantSigs, err := participantForfeitSigs(
		s.VTXO.ClientKey.PubKey, sig, evt.ForfeitSpend != nil,
		externalSigs,
	)
	if err != nil {
		return nil, fmt.Errorf("participant forfeit signatures: %w",
			err)
	}

	forfeitTxID := forfeitTx.TxHash()
	submission := &ForfeitSignatureSubmission{
		VTXOOutpoint:        s.VTXO.Outpoint,
		RoundID:             evt.RoundID,
		ForfeitTx:           forfeitTx,
		Signature:           sig,
		ParticipantVTXOSigs: participantSigs,
		SpendPath:           forfeitSpend,
	}

	return &VTXOStateTransition{
		NextState: &ForfeitingState{
			VTXO:              s.VTXO,
			NewRoundID:        evt.RoundID,
			ConnectorOutpoint: evt.ConnectorOutpoint,
			ForfeitTxID:       forfeitTxID,
			ForfeitTx:         forfeitTx,
		},
		NewEvents: fn.Some(VTXOEmittedEvent{
			Outbox: []VTXOOutMsg{
				submission,
				&VTXOStatusUpdate{
					Outpoint:  s.VTXO.Outpoint,
					NewStatus: VTXOStatusForfeiting,
					RoundID:   evt.RoundID,
					ForfeitTx: forfeitTx,
				},
			},
		}),
	}, nil
}

// resolveForfeitSpendPath chooses the explicit arkscript spend path used for
// the VTXO input of a forfeit transaction.
func resolveForfeitSpendPath(vtxo *Descriptor,
	evt *ForfeitRequestEvent) (*arkscript.SpendPath, error) {

	if evt != nil && evt.ForfeitSpend != nil {
		err := evt.ForfeitSpend.Validate()
		if err != nil {
			return nil, err
		}

		return evt.ForfeitSpend, nil
	}

	if vtxo == nil {
		return nil, fmt.Errorf("vtxo descriptor is required")
	}

	policy, err := arkscript.NewVTXOPolicy(
		vtxo.ClientKey.PubKey, vtxo.OperatorKey, vtxo.RelativeExpiry,
	)
	if err != nil {
		return nil, err
	}

	spendInfo, err := policy.CollabSpendInfo()
	if err != nil {
		return nil, err
	}

	return &arkscript.SpendPath{
		SpendInfo: spendInfo,
	}, nil
}

// signForfeitVTXOInput produces the client's schnorr signature for the VTXO
// input of a forfeit transaction. The VTXO uses a tapscript with a 2-of-2
// collaborative spend path, so both client and operator signatures are needed.
// This function only produces the client's half; the operator adds theirs.
func signForfeitVTXOInput(vtxo *Descriptor, spendPath *arkscript.SpendPath,
	evt *ForfeitRequestEvent, forfeitTx *wire.MsgTx,
	env *VTXOEnvironment) (*schnorr.Signature, error) {

	if env.Wallet == nil {
		return nil, fmt.Errorf("wallet is required for signing")
	}

	// Reconstruct the VTXO output to build proper sighash. BIP-341 requires
	// all prevouts when computing taproot signature hashes.
	vtxoOutput := &wire.TxOut{
		Value: int64(vtxo.Amount), PkScript: vtxo.PkScript,
	}
	vtxoCtx := &tx.VTXOSpendContext{
		Outpoint:  vtxo.Outpoint,
		Output:    vtxoOutput,
		TapScript: vtxo.TapScript,
	}

	connectorOutput := &wire.TxOut{
		Value: evt.ConnectorAmount, PkScript: evt.ConnectorPkScript,
	}
	connectorCtx := &tx.ConnectorSpendContext{
		Outpoint: evt.ConnectorOutpoint,
		Output:   connectorOutput,
	}

	prevFetcher, err := tx.NewForfeitPrevOutFetcher(vtxoCtx, connectorCtx)
	if err != nil {
		return nil, fmt.Errorf("prevout fetcher: %w", err)
	}

	sigHashes := txscript.NewTxSigHashes(forfeitTx, prevFetcher)

	signDesc := spendPath.BuildSignDescriptor(
		vtxo.ClientKey, vtxoOutput, sigHashes, prevFetcher,
		tx.ForfeitVTXOInputIndex,
	)
	if signDesc == nil {
		return nil, fmt.Errorf("sign descriptor: missing spend path")
	}

	sig, err := env.Wallet.SignOutputRaw(forfeitTx, signDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to sign: %w", err)
	}

	// Parse the serialized signature to get a typed schnorr.Signature.
	schnorrSig, err := schnorr.ParseSignature(sig.Serialize())
	if err != nil {
		return nil, fmt.Errorf("parse schnorr signature: %w", err)
	}

	return schnorrSig, nil
}

func externalForfeitParticipantSigs(ctx context.Context, vtxo *Descriptor,
	spendPath *arkscript.SpendPath, evt *ForfeitRequestEvent,
	forfeitTx *wire.MsgTx,
	env *VTXOEnvironment) ([]*types.ForfeitParticipantSig, error) {

	if env == nil || env.ForfeitParticipantSigner == nil ||
		evt == nil || evt.ForfeitSpend == nil {
		return nil, nil
	}

	req := &ForfeitParticipantSignRequest{
		VTXO:                  vtxo,
		SpendPath:             spendPath,
		ForfeitTx:             forfeitTx,
		ConnectorOutpoint:     evt.ConnectorOutpoint,
		ConnectorPkScript:     evt.ConnectorPkScript,
		ConnectorAmount:       evt.ConnectorAmount,
		ServerForfeitPkScript: evt.ServerForfeitPkScript,
	}

	sigs, err := env.ForfeitParticipantSigner(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := verifyForfeitParticipantSignatures(req, sigs); err != nil {
		return nil, err
	}

	return sigs, nil
}

func verifyForfeitParticipantSignatures(req *ForfeitParticipantSignRequest,
	sigs []*types.ForfeitParticipantSig) error {

	if len(sigs) == 0 {
		return nil
	}

	sighash, err := forfeitParticipantSigHash(req)
	if err != nil {
		return err
	}

	for idx, sig := range sigs {
		if sig == nil {
			return fmt.Errorf("participant signature is required")
		}
		if sig.PubKey == nil {
			return fmt.Errorf("participant pubkey is required")
		}
		if sig.Signature == nil {
			return fmt.Errorf("participant schnorr signature is " +
				"required")
		}
		if !sig.Signature.Verify(sighash, sig.PubKey) {
			return fmt.Errorf("invalid participant signature %d",
				idx)
		}
	}

	return nil
}

func forfeitParticipantSigHash(req *ForfeitParticipantSignRequest) ([]byte,
	error) {

	if req == nil || req.VTXO == nil {
		return nil, fmt.Errorf("forfeit participant request is " +
			"required")
	}
	if req.SpendPath == nil {
		return nil, fmt.Errorf("forfeit participant spend path is " +
			"required")
	}
	if req.ForfeitTx == nil {
		return nil, fmt.Errorf("forfeit transaction is required")
	}

	vtxoOutput := &wire.TxOut{
		Value:    int64(req.VTXO.Amount),
		PkScript: req.VTXO.PkScript,
	}
	vtxoCtx := &tx.VTXOSpendContext{
		Outpoint:  req.VTXO.Outpoint,
		Output:    vtxoOutput,
		TapScript: req.VTXO.TapScript,
	}

	connectorOutput := &wire.TxOut{
		Value:    req.ConnectorAmount,
		PkScript: req.ConnectorPkScript,
	}
	connectorCtx := &tx.ConnectorSpendContext{
		Outpoint: req.ConnectorOutpoint,
		Output:   connectorOutput,
	}

	prevFetcher, err := tx.NewForfeitPrevOutFetcher(vtxoCtx, connectorCtx)
	if err != nil {
		return nil, fmt.Errorf("prevout fetcher: %w", err)
	}

	sigHashes := txscript.NewTxSigHashes(req.ForfeitTx, prevFetcher)
	leaf := txscript.NewBaseTapLeaf(req.SpendPath.WitnessScript)

	return txscript.CalcTapscriptSignaturehash(
		sigHashes, txscript.SigHashDefault, req.ForfeitTx,
		tx.ForfeitVTXOInputIndex, prevFetcher, leaf,
	)
}

func participantForfeitSigs(localPubKey *btcec.PublicKey,
	localSig *schnorr.Signature, customSpend bool,
	externalSigs []*types.ForfeitParticipantSig) (
	[]*types.ForfeitParticipantSig, error) {

	needsParticipantSigs := customSpend || len(externalSigs) > 0
	if !needsParticipantSigs {
		return nil, nil
	}
	if localPubKey == nil {
		return nil, fmt.Errorf("local participant pubkey is required")
	}
	if localSig == nil {
		return nil, fmt.Errorf("local participant signature is " +
			"required")
	}

	participantSigs := make(
		[]*types.ForfeitParticipantSig, 0, len(externalSigs)+1,
	)
	seen := make(map[string]struct{}, len(externalSigs)+1)

	for _, sig := range externalSigs {
		if sig == nil {
			return nil, fmt.Errorf("participant signature is " +
				"required")
		}
		if sig.PubKey == nil {
			return nil, fmt.Errorf("participant pubkey is required")
		}
		if sig.Signature == nil {
			return nil, fmt.Errorf("participant schnorr " +
				"signature is required")
		}

		keyID := participantForfeitKeyID(sig.PubKey)
		if _, ok := seen[keyID]; ok {
			return nil, fmt.Errorf("duplicate participant " +
				"signature")
		}
		if sameParticipantForfeitKey(sig.PubKey, localPubKey) {
			return nil, fmt.Errorf("external signature uses " +
				"local pubkey")
		}

		seen[keyID] = struct{}{}
		participantSigs = append(participantSigs, sig)
	}

	localKeyID := participantForfeitKeyID(localPubKey)
	if _, ok := seen[localKeyID]; ok {
		return nil, fmt.Errorf("duplicate local participant signature")
	}

	return append(participantSigs, &types.ForfeitParticipantSig{
		PubKey:    localPubKey,
		Signature: localSig,
	}), nil
}

func participantForfeitKeyID(key *btcec.PublicKey) string {
	if key == nil {
		return ""
	}

	return string(schnorr.SerializePubKey(key))
}

func sameParticipantForfeitKey(a, b *btcec.PublicKey) bool {
	return participantForfeitKeyID(a) == participantForfeitKeyID(b)
}

// ProcessEvent handles events in PendingForfeitState. The VTXO has been
// committed to cooperative consumption and is waiting for the round actor
// to supply forfeit details (connector outpoint, pkscript, etc.).
func (s *PendingForfeitState) ProcessEvent(ctx context.Context, event VTXOEvent,
	env *VTXOEnvironment) (*VTXOStateTransition, error) {

	switch evt := event.(type) {
	case *BlockEpochEvent:
		// Check if we've hit critical expiry while waiting for
		// forfeit details.
		expiryStatus := env.ExpiryConfig.CheckExpiry(s.VTXO, evt.Height)

		if expiryStatus == ExpiryStatusCritical ||
			expiryStatus == ExpiryStatusExpired {

			blocksRemaining := BlocksUntilExpiry(s.VTXO, evt.Height)

			// Non-terminal exit: no VTXOTerminatedNotification, so
			// a failed unroll can recover the VTXO
			// (wavelength#602).
			outbox := []VTXOOutMsg{
				&ExpiringNotification{
					VTXO:            s.VTXO,
					BlocksRemaining: blocksRemaining,
					Reason:          "forfeit timeout",
				},
				&VTXOStatusUpdate{
					Outpoint:  s.VTXO.Outpoint,
					NewStatus: VTXOStatusUnilateralExit,
				},
			}

			return &VTXOStateTransition{
				NextState: &UnilateralExitState{
					VTXO: s.VTXO,
					Reason: "critical expiry pending " +
						"forfeit",
					LastCheckedHeight: evt.Height,
				},
				NewEvents: fn.Some(VTXOEmittedEvent{
					Outbox: outbox,
				}),
			}, nil
		}

		// Still waiting, stay in this state.
		return &VTXOStateTransition{
			NextState: s,
		}, nil

	case *ForceUnrollEvent:
		// Client requested unilateral exit while forfeit is
		// still pending. Transition to exit — the on-chain
		// recovery path doesn't depend on the forfeit.
		reason := evt.Reason
		if reason == "" {
			reason = "manual unroll (pending forfeit)"
		}

		// Non-terminal exit: no VTXOTerminatedNotification, so a failed
		// unroll can recover the VTXO (wavelength#602).
		outbox := []VTXOOutMsg{
			&ExpiringNotification{
				VTXO:            s.VTXO,
				BlocksRemaining: 0,
				Reason:          reason,
				Trigger:         evt.Trigger,
				ExitPolicy:      evt.ExitPolicy,
			},
			&VTXOStatusUpdate{
				Outpoint:  s.VTXO.Outpoint,
				NewStatus: VTXOStatusUnilateralExit,
			},
		}

		return &VTXOStateTransition{
			NextState: &UnilateralExitState{
				VTXO:              s.VTXO,
				Reason:            reason,
				LastCheckedHeight: s.RequestedAtHeight,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{
				Outbox: outbox,
			}),
		}, nil

	case *ForfeitRequestEvent:
		// Round actor is ready for forfeit. Build and sign the forfeit
		// tx to transfer this VTXO to the new round.
		forfeitSpend, err := resolveForfeitSpendPath(s.VTXO, evt)
		if err != nil {
			return nil, fmt.Errorf("resolve forfeit spend path: %w",
				err)
		}

		forfeitTx, err := tx.BuildForfeitTxWithContext(
			&s.VTXO.Outpoint, s.VTXO.Amount,
			&evt.ConnectorOutpoint,
			btcutil.Amount(evt.ConnectorAmount),
			evt.ServerForfeitPkScript,
			tx.ForfeitTxContext{
				VTXOSequence: forfeitSpend.RequiredSequence,
				LockTime:     forfeitSpend.RequiredLockTime,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("build forfeit tx: %w", err)
		}

		sig, err := signForfeitVTXOInput(
			s.VTXO, forfeitSpend, evt, forfeitTx, env,
		)
		if err != nil {
			return nil, fmt.Errorf("sign forfeit tx: %w", err)
		}
		externalSigs, err := externalForfeitParticipantSigs(
			ctx, s.VTXO, forfeitSpend, evt, forfeitTx, env,
		)
		if err != nil {
			return nil, fmt.Errorf("external participant "+
				"signatures: %w", err)
		}
		participantSigs, err := participantForfeitSigs(
			s.VTXO.ClientKey.PubKey, sig, evt.ForfeitSpend != nil,
			externalSigs,
		)
		if err != nil {
			return nil, fmt.Errorf("participant forfeit "+
				"signatures: %w", err)
		}

		forfeitTxID := forfeitTx.TxHash()
		submission := &ForfeitSignatureSubmission{
			VTXOOutpoint:        s.VTXO.Outpoint,
			RoundID:             evt.RoundID,
			ForfeitTx:           forfeitTx,
			Signature:           sig,
			ParticipantVTXOSigs: participantSigs,
			SpendPath:           forfeitSpend,
		}

		return &VTXOStateTransition{
			NextState: &ForfeitingState{
				VTXO:              s.VTXO,
				NewRoundID:        evt.RoundID,
				ConnectorOutpoint: evt.ConnectorOutpoint,
				ForfeitTxID:       forfeitTxID,
				ForfeitTx:         forfeitTx,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{
				Outbox: []VTXOOutMsg{
					submission,
					&VTXOStatusUpdate{
						Outpoint:  s.VTXO.Outpoint,
						NewStatus: VTXOStatusForfeiting,
						RoundID:   evt.RoundID,
						ForfeitTx: forfeitTx,
					},
				},
			}),
		}, nil

	case *PendingForfeitEvent:
		// Duplicate commit while already pending is harmless. The
		// round may re-issue this after restart or replay.
		return &VTXOStateTransition{
			NextState: s,
		}, nil

	case *ForfeitReleasedEvent:
		// Release this VTXO back to LiveState. This happens when
		// cooperative round registration fails after admission.
		// Restore RequestedAtHeight as LastCheckedHeight so expiry
		// checking resumes from where it left off rather than
		// re-evaluating from zero.
		return &VTXOStateTransition{
			NextState: &LiveState{
				VTXO:              s.VTXO,
				LastCheckedHeight: s.RequestedAtHeight,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{
				Outbox: []VTXOOutMsg{
					&VTXOStatusUpdate{
						Outpoint:  s.VTXO.Outpoint,
						NewStatus: VTXOStatusLive,
					},
				},
			}),
		}, nil

	case *SpendReserveEvent:
		// Cannot claim for OOR spend while pending forfeit.
		return nil, fmt.Errorf("pending_forfeit: cannot reserve for " +
			"spend")

	case *ResumeVTXOEvent:
		// On resume, stay in this state. The round actor should
		// re-send forfeit details when it resumes.
		return &VTXOStateTransition{
			NextState: s,
		}, nil

	case *VTXOFailedEvent:
		return &VTXOStateTransition{
			NextState: &FailedState{
				VTXO:        s.VTXO,
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
			},
		}, nil

	default:
		return nil, fmt.Errorf("pending_forfeit: unexpected event: %T",
			event)
	}
}

// ProcessEvent handles events in ForfeitingState. The VTXO is being forfeited
// and waiting for the new commitment transaction to confirm.
func (s *ForfeitingState) ProcessEvent(ctx context.Context, event VTXOEvent,
	env *VTXOEnvironment) (*VTXOStateTransition, error) {

	switch evt := event.(type) {
	case *ForfeitSignedEvent:
		// Forfeit tx has been signed and submitted. Update the txid
		// and stay in this state. Preserve the ForfeitTx for crash
		// recovery.
		return &VTXOStateTransition{
			NextState: &ForfeitingState{
				VTXO:              s.VTXO,
				NewRoundID:        s.NewRoundID,
				ConnectorOutpoint: s.ConnectorOutpoint,
				ForfeitTxID:       evt.ForfeitTxID,
				ForfeitTx:         s.ForfeitTx,
			},
		}, nil

	case *ForfeitConfirmedEvent:
		// New commitment tx confirmed, forfeit is complete. Include
		// the ForfeitTx so the persistence layer can call MarkForfeited
		// with the txid for audit/recovery purposes.
		forfeitTxID := s.ForfeitTxID
		if forfeitTxID == (chainhash.Hash{}) && s.ForfeitTx != nil {
			forfeitTxID = s.ForfeitTx.TxHash()
		}
		statusUpdate := &VTXOStatusUpdate{
			Outpoint:    s.VTXO.Outpoint,
			NewStatus:   VTXOStatusForfeited,
			ForfeitTx:   s.ForfeitTx,
			ForfeitTxID: forfeitTxID,
		}
		statusUpdate.ConsumerBatchTxID = evt.CommitmentTxID

		return &VTXOStateTransition{
			NextState: &ForfeitedState{
				VTXO:           s.VTXO,
				NewRoundID:     s.NewRoundID,
				CommitmentTxID: evt.CommitmentTxID,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{
				Outbox: []VTXOOutMsg{
					statusUpdate,
					&VTXOTerminatedNotification{
						VTXOOutpoint: s.VTXO.Outpoint,
						FinalState:   "Forfeited",
						Reason: fmt.Sprintf(
							"forfeited in round %s",
							s.NewRoundID,
						),
					},
				},
			}),
		}, nil

	case *BlockEpochEvent:
		// Check if we've hit expiry while forfeiting. If critical, we
		// must escalate to chain resolver for unilateral exit.
		expiryStatus := env.ExpiryConfig.CheckExpiry(s.VTXO, evt.Height)

		if expiryStatus == ExpiryStatusCritical ||
			expiryStatus == ExpiryStatusExpired {

			blocksRemaining := BlocksUntilExpiry(s.VTXO, evt.Height)

			// Non-terminal exit: no VTXOTerminatedNotification, so
			// a failed unroll can recover the VTXO
			// (wavelength#602).
			outbox := []VTXOOutMsg{
				&ExpiringNotification{
					VTXO:            s.VTXO,
					BlocksRemaining: blocksRemaining,
					Reason:          "round stalled",
				},
				&VTXOStatusUpdate{
					Outpoint:  s.VTXO.Outpoint,
					NewStatus: VTXOStatusUnilateralExit,
				},
			}

			return &VTXOStateTransition{
				NextState: &UnilateralExitState{
					VTXO:              s.VTXO,
					Reason:            "critical expiry",
					LastCheckedHeight: evt.Height,
				},
				NewEvents: fn.Some(VTXOEmittedEvent{
					Outbox: outbox,
				}),
			}, nil
		}

		// Still waiting for forfeit confirmation.
		return &VTXOStateTransition{
			NextState: s,
		}, nil

	case *ResumeVTXOEvent:
		// On resume in ForfeitingState, we need to wait for the round
		// to complete and send ForfeitConfirmedEvent.
		return &VTXOStateTransition{
			NextState: s,
		}, nil

	case *ForceUnrollEvent:
		// Client requested unilateral exit while a forfeit is
		// mid-flight. The on-chain recovery path doesn't depend
		// on the forfeit signature landing, so we escalate to
		// UnilateralExitState immediately.
		reason := evt.Reason
		if reason == "" {
			reason = "manual unroll (forfeiting)"
		}

		// Non-terminal exit: no VTXOTerminatedNotification, so a failed
		// unroll can recover the VTXO (wavelength#602).
		// ForfeitingState tracks no block height, so LastCheckedHeight
		// stays zero and the next block epoch re-seeds it if the VTXO
		// is rolled back.
		outbox := []VTXOOutMsg{
			&ExpiringNotification{
				VTXO:            s.VTXO,
				BlocksRemaining: 0,
				Reason:          reason,
				Trigger:         evt.Trigger,
				ExitPolicy:      evt.ExitPolicy,
			},
			&VTXOStatusUpdate{
				Outpoint:  s.VTXO.Outpoint,
				NewStatus: VTXOStatusUnilateralExit,
			},
		}

		return &VTXOStateTransition{
			NextState: &UnilateralExitState{
				VTXO:   s.VTXO,
				Reason: reason,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{Outbox: outbox}),
		}, nil

	case *ForfeitReleasedEvent:
		// A pre-signing round failure released this VTXO. We can land
		// here, not just in PendingForfeitState, when the round fails
		// during ForfeitSignaturesCollecting after this VTXO already
		// replied with its forfeit signature and advanced to
		// ForfeitingState. The release is still safe: no forfeit
		// signature reaches the server until the success edge out of
		// ForfeitSignaturesCollecting calls
		// SubmitVTXOForfeitSigsToServer, so a failure before then has
		// handed nothing to the operator. Return the VTXO to LiveState
		// rather than leaving it wedged in FORFEITING. ForfeitingState
		// tracks no block height, so LastCheckedHeight stays zero and
		// the next block epoch re-seeds expiry checking (mirroring the
		// ForceUnrollEvent recovery path above).
		return &VTXOStateTransition{
			NextState: &LiveState{
				VTXO: s.VTXO,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{
				Outbox: []VTXOOutMsg{
					&VTXOStatusUpdate{
						Outpoint:  s.VTXO.Outpoint,
						NewStatus: VTXOStatusLive,
					},
				},
			}),
		}, nil

	case *VTXOFailedEvent:
		return &VTXOStateTransition{
			NextState: &FailedState{
				VTXO:        s.VTXO,
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
			},
		}, nil

	default:
		return nil, fmt.Errorf("forfeiting: bad event: %T", event)
	}
}

// ProcessEvent handles events in SpendingState. The VTXO has been claimed for
// an OOR spend but must still monitor expiry. A spend can be completed,
// released, or escalated to unilateral exit on critical expiry.
func (s *SpendingState) ProcessEvent(_ context.Context, event VTXOEvent,
	env *VTXOEnvironment) (*VTXOStateTransition, error) {

	switch evt := event.(type) {
	case *SpendCompletedEvent:
		// OOR spend completed successfully. Transition to terminal
		// SpentState. The VTXO actor will be cleaned up.
		return &VTXOStateTransition{
			NextState: &SpentState{
				VTXO: s.VTXO,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{
				Outbox: []VTXOOutMsg{
					&VTXOStatusUpdate{
						Outpoint:  s.VTXO.Outpoint,
						NewStatus: VTXOStatusSpent,

						ReleaseSpendReservation: true,
					},
					&VTXOTerminatedNotification{
						VTXOOutpoint: s.VTXO.Outpoint,
						FinalState:   "Spent",
						Reason: "OOR spend " +
							"completed",
					},
				},
			}),
		}, nil

	case *SpendReleasedEvent:
		// OOR operation failed or was cancelled. Return to LiveState
		// so the VTXO can be used again.
		return &VTXOStateTransition{
			NextState: &LiveState{
				VTXO:              s.VTXO,
				LastCheckedHeight: s.LastCheckedHeight,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{
				Outbox: []VTXOOutMsg{
					&VTXOStatusUpdate{
						Outpoint:  s.VTXO.Outpoint,
						NewStatus: VTXOStatusLive,

						ReleaseSpendReservation: true,
					},
				},
			}),
		}, nil

	case *BlockEpochEvent:
		// Expiry safety: even while spending, we must escalate to
		// unilateral exit if critical expiry is reached.
		s.LastCheckedHeight = evt.Height

		expiryStatus := env.ExpiryConfig.CheckExpiry(
			s.VTXO, evt.Height,
		)

		if expiryStatus == ExpiryStatusCritical ||
			expiryStatus == ExpiryStatusExpired {

			blocksRemaining := BlocksUntilExpiry(
				s.VTXO, evt.Height,
			)

			// Non-terminal exit: no VTXOTerminatedNotification, so
			// a failed unroll can recover the VTXO
			// (wavelength#602).
			outbox := []VTXOOutMsg{
				&ExpiringNotification{
					VTXO:            s.VTXO,
					BlocksRemaining: blocksRemaining,
					Reason:          "spend timeout",
				},
				&VTXOStatusUpdate{
					Outpoint:  s.VTXO.Outpoint,
					NewStatus: VTXOStatusUnilateralExit,

					ReleaseSpendReservation: true,
				},
			}

			return &VTXOStateTransition{
				NextState: &UnilateralExitState{
					VTXO: s.VTXO,
					Reason: "critical expiry while " +
						"spending",
					LastCheckedHeight: evt.Height,
				},
				NewEvents: fn.Some(VTXOEmittedEvent{
					Outbox: outbox,
				}),
			}, nil
		}

		// Refresh is intentionally blocked while spending. OOR
		// spends are short-lived; on completion the VTXO moves
		// to SpentState, on release it returns to LiveState
		// where refresh will be re-evaluated.
		return &VTXOStateTransition{
			NextState: s,
		}, nil

	case *PendingForfeitEvent:
		// Cannot claim for cooperative forfeit while spending.
		return nil, fmt.Errorf("spending: cannot accept pending " +
			"forfeit")

	case *SpendReserveEvent:
		// Already reserved for spend.
		return nil, fmt.Errorf("spending: already reserved for spend")

	case *ResumeVTXOEvent:
		// On resume, stay in SpendingState. The OOR session will
		// resume and later release or complete the claim.
		return &VTXOStateTransition{
			NextState: s,
		}, nil

	case *ForceUnrollEvent:
		// Client requested unilateral exit while an OOR spend is
		// in flight. The on-chain recovery path supersedes the
		// OOR claim, so we escalate to UnilateralExitState using
		// the same outbox shape as the critical-expiry branch
		// above to converge manual and automatic exits on a
		// single chain resolver seam.
		reason := evt.Reason
		if reason == "" {
			reason = "manual unroll (spending)"
		}

		// Non-terminal exit: no VTXOTerminatedNotification, so a failed
		// unroll can recover the VTXO (wavelength#602). Leaving
		// SpendingState drops the durable reservation row in the same
		// transaction as the status change (ReleaseSpendReservation),
		// matching the critical-expiry branch above; otherwise the
		// stale row outlives the spend and the startup reservation
		// sweep would re-reserve a recovered-to-Live VTXO that no
		// session owns.
		outbox := []VTXOOutMsg{
			&ExpiringNotification{
				VTXO:            s.VTXO,
				BlocksRemaining: 0,
				Reason:          reason,
				Trigger:         evt.Trigger,
				ExitPolicy:      evt.ExitPolicy,
			},
			&VTXOStatusUpdate{
				Outpoint:  s.VTXO.Outpoint,
				NewStatus: VTXOStatusUnilateralExit,

				ReleaseSpendReservation: true,
			},
		}

		return &VTXOStateTransition{
			NextState: &UnilateralExitState{
				VTXO:              s.VTXO,
				Reason:            reason,
				LastCheckedHeight: s.LastCheckedHeight,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{Outbox: outbox}),
		}, nil

	case *VTXOFailedEvent:
		return &VTXOStateTransition{
			NextState: &FailedState{
				VTXO:        s.VTXO,
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
			},
		}, nil

	default:
		return nil, fmt.Errorf("spending: unexpected event: %T", event)
	}
}

// ProcessEvent for SpentState. This is a terminal state, so all events result
// in staying in the same state.
func (s *SpentState) ProcessEvent(_ context.Context, _ VTXOEvent,
	_ *VTXOEnvironment) (*VTXOStateTransition, error) {

	// Terminal state: self-loop on all events.
	return &VTXOStateTransition{
		NextState: s,
	}, nil
}

// ProcessEvent for ForfeitedState. This is a terminal state, so all events
// result in staying in the same state.
func (s *ForfeitedState) ProcessEvent(_ context.Context, _ VTXOEvent,
	_ *VTXOEnvironment) (*VTXOStateTransition, error) {

	// Terminal state: self-loop on all events.
	return &VTXOStateTransition{
		NextState: s,
	}, nil
}

// ProcessEvent handles events in UnilateralExitState. The VTXO has been
// handed to the chain resolver for on-chain unroll, but the actor stays
// alive to observe the outcome: a clean failure rolls the VTXO back to
// LiveState, an on-chain confirmation retires it to the terminal
// SpentState, and everything else self-loops while the exit is in flight.
func (s *UnilateralExitState) ProcessEvent(_ context.Context, event VTXOEvent,
	_ *VTXOEnvironment) (*VTXOStateTransition, error) {

	switch evt := event.(type) {
	case *ForceUnrollEvent:
		// A ForceUnrollEvent on an already-exiting VTXO is an
		// idempotent re-admission, not a no-op: the manager drives one
		// whenever an external trigger (vHTLC recovery restore, a
		// repeated fraud spend) re-asks for the exit, and the registry
		// record may not have been written yet (the first admission's
		// ExpiringNotification is a best-effort Tell that can be lost
		// to a crash before the registry's UpsertRecord). Re-emit the
		// notification so the chain resolver bridge re-admits under the
		// same trigger/policy; the registry dedups against a live
		// record, so a redundant re-admit is a benign no-op. Stay in
		// UnilateralExitState and do not re-persist the status (already
		// UnilateralExit).
		reason := evt.Reason
		if reason == "" {
			reason = s.Reason
		}

		return &VTXOStateTransition{
			NextState: s,
			NewEvents: fn.Some(VTXOEmittedEvent{
				Outbox: []VTXOOutMsg{
					&ExpiringNotification{
						VTXO:            s.VTXO,
						BlocksRemaining: 0,
						Reason:          reason,
						Trigger:         evt.Trigger,
						ExitPolicy:      evt.ExitPolicy,
					},
				},
			}),
		}, nil

	case *ExitFailedEvent:
		// The unroll job failed without any on-chain footprint, so the
		// VTXO is still live from the operator's perspective. Roll back
		// to LiveState and re-publish the live status so the wallet's
		// view re-converges. Resume expiry monitoring from the height
		// observed when we entered exit handling.
		return &VTXOStateTransition{
			NextState: &LiveState{
				VTXO:              s.VTXO,
				LastCheckedHeight: s.LastCheckedHeight,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{
				Outbox: []VTXOOutMsg{
					&VTXOStatusUpdate{
						Outpoint:  s.VTXO.Outpoint,
						NewStatus: VTXOStatusLive,
					},
				},
			}),
		}, nil

	case *ExitConfirmedEvent:
		// The exit confirmed on-chain. Retire the VTXO to the terminal
		// SpentState and notify the manager so the actor is reaped —
		// now gated on a terminal on-chain event rather than the user's
		// intent to exit.
		return &VTXOStateTransition{
			NextState: &SpentState{
				VTXO: s.VTXO,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{
				Outbox: []VTXOOutMsg{
					&VTXOStatusUpdate{
						Outpoint:  s.VTXO.Outpoint,
						NewStatus: VTXOStatusSpent,
					},
					&VTXOTerminatedNotification{
						VTXOOutpoint: s.VTXO.Outpoint,
						FinalState:   "Spent",
						Reason:       "exit confirmed",
					},
				},
			}),
		}, nil

	default:
		// Still exiting (block epochs, a duplicate ForceUnroll, resume,
		// stray admission requests): self-loop. The exit is already in
		// flight at the chain resolver; we wait for its terminal
		// outcome.
		return &VTXOStateTransition{
			NextState: s,
		}, nil
	}
}

// ProcessEvent for FailedState. This is a terminal state, so all events
// result in staying in the same state.
func (s *FailedState) ProcessEvent(_ context.Context, _ VTXOEvent,
	_ *VTXOEnvironment) (*VTXOStateTransition, error) {

	// Terminal state: self-loop on all events.
	return &VTXOStateTransition{
		NextState: s,
	}, nil
}
