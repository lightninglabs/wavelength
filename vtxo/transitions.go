package vtxo

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tx"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ProcessEvent handles events in LiveState. The VTXO monitors block epochs for
// expiry and can receive forfeit requests from the round actor.
func (s *LiveState) ProcessEvent(
	ctx context.Context, event VTXOEvent, env *VTXOEnvironment,
) (*VTXOStateTransition, error) {

	switch evt := event.(type) {
	case *BlockEpochEvent:
		return s.handleBlockEpoch(ctx, evt, env)

	case *ForfeitRequestEvent:
		return s.handleForfeitRequest(ctx, evt, env)

	case *ResumeVTXOEvent:
		// On resume, stay in LiveState and re-check expiry on next
		// block.
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

// handleBlockEpoch processes a new block notification and checks if the VTXO
// needs to be refreshed or escalated.
func (s *LiveState) handleBlockEpoch(
	_ context.Context, evt *BlockEpochEvent, env *VTXOEnvironment,
) (*VTXOStateTransition, error) {

	s.LastCheckedHeight = evt.Height

	expiryStatus := env.ExpiryConfig.CheckExpiry(s.VTXO, evt.Height)

	switch expiryStatus {
	case ExpiryStatusSafe:
		// Nothing to do, stay in LiveState.
		return &VTXOStateTransition{
			NextState: s,
		}, nil

	case ExpiryStatusNeedsRefresh:
		// Request refresh from round actor before expiry becomes critical.
		blocksRemaining := BlocksUntilExpiry(s.VTXO, evt.Height)
		urgency := env.ExpiryConfig.DetermineRefreshUrgency(
			blocksRemaining,
		)

		outbox := []VTXOOutMsg{
			&RefreshRequest{
				VTXOOutpoint: s.VTXO.Outpoint,
				Amount:       int64(s.VTXO.Amount),
				Urgency:      urgency,
			},
			&VTXOStatusUpdate{
				Outpoint:  s.VTXO.Outpoint,
				NewStatus: VTXOStatusRefreshRequested,
			},
		}

		return &VTXOStateTransition{
			NextState: &RefreshRequestedState{
				VTXO:              s.VTXO,
				RequestedAtHeight: evt.Height,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{Outbox: outbox}),
		}, nil

	case ExpiryStatusCritical:
		// Escalate to chain resolver for unilateral exit.
		blocksRemaining := BlocksUntilExpiry(s.VTXO, evt.Height)
		reason := fmt.Sprintf(
			"critical expiry: %d blocks remaining", blocksRemaining,
		)

		outbox := []VTXOOutMsg{
			&ExpiringNotification{
				VTXO:            s.VTXO,
				BlocksRemaining: blocksRemaining,
				Reason:          "batch expiry imminent",
			},
			&VTXOStatusUpdate{
				Outpoint:  s.VTXO.Outpoint,
				NewStatus: VTXOStatusExpiring,
			},
			&VTXOTerminatedNotification{
				VTXOOutpoint: s.VTXO.Outpoint,
				FinalState:   "Expiring",
				Reason:       "sent to chain resolver",
			},
		}

		return &VTXOStateTransition{
			NextState:  &ExpiringState{VTXO: s.VTXO, Reason: reason},
			NewEvents: fn.Some(VTXOEmittedEvent{Outbox: outbox}),
		}, nil

	case ExpiryStatusExpired:
		// Batch has expired - this should not happen if monitoring
		// works correctly.
		return &VTXOStateTransition{
			NextState: &FailedState{
				VTXO:        s.VTXO,
				Reason:      "batch expired before refresh",
				Recoverable: false,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unknown expiry status: %d", expiryStatus)
	}
}

// handleForfeitRequest builds and signs a forfeit transaction when the round
// actor requests this VTXO be forfeited as part of a batch swap. The forfeit
// atomically links the old VTXO to a new round via the connector output - if
// the new commitment tx confirms, the forfeit becomes valid and pays the VTXO
// value to the operator. This prevents double-spending by ensuring the client
// cannot claim both the old VTXO and a new one in the fresh round.
func (s *LiveState) handleForfeitRequest(
	_ context.Context, evt *ForfeitRequestEvent, env *VTXOEnvironment,
) (*VTXOStateTransition, error) {

	forfeitTx, err := tx.BuildForfeitTx(
		&s.VTXO.Outpoint, s.VTXO.Amount,
		&evt.ConnectorOutpoint, evt.ServerForfeitPkScript,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build forfeit tx: %w", err)
	}

	// Sign our portion of the collaborative 2-of-2 spend on the VTXO input.
	// The operator will add their signature to complete the multisig witness.
	sig, err := signForfeitVTXOInput(s.VTXO, evt, forfeitTx, env)
	if err != nil {
		return nil, fmt.Errorf("failed to sign forfeit tx: %w", err)
	}

	forfeitTxID := forfeitTx.TxHash()

	return &VTXOStateTransition{
		NextState: &ForfeitingState{
			VTXO:              s.VTXO,
			NewRoundID:        evt.RoundID,
			ConnectorOutpoint: evt.ConnectorOutpoint,
			ForfeitTxID:       forfeitTxID,
		},
		NewEvents: fn.Some(VTXOEmittedEvent{
			Outbox: []VTXOOutMsg{
				&ForfeitSignatureSubmission{
					VTXOOutpoint: s.VTXO.Outpoint,
					RoundID:      evt.RoundID,
					ForfeitTx:    forfeitTx,
					Signature:    sig,
				},
				&VTXOStatusUpdate{
					Outpoint:  s.VTXO.Outpoint,
					NewStatus: VTXOStatusForfeiting,
				},
			},
		}),
	}, nil
}

// signForfeitVTXOInput produces the client's schnorr signature for the VTXO
// input of a forfeit transaction. The VTXO uses a tapscript with a 2-of-2
// collaborative spend path, so both client and operator signatures are needed.
// This function only produces the client's half; the operator adds theirs.
func signForfeitVTXOInput(vtxo *VTXODescriptor, evt *ForfeitRequestEvent,
	forfeitTx *wire.MsgTx, env *VTXOEnvironment) ([]byte, error) {

	if vtxo.TapScript == nil {
		return nil, fmt.Errorf("VTXO tapscript is required for signing")
	}
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

	// NewVTXOCollabSignDescriptor extracts the collaborative leaf's witness
	// script and control block from the tapscript, which are needed for
	// script-path spending.
	signDesc, _, err := tx.NewVTXOCollabSignDescriptor(
		vtxoCtx, vtxo.ClientKey, tx.ForfeitVTXOInputIndex,
		sigHashes, prevFetcher,
	)
	if err != nil {
		return nil, fmt.Errorf("sign descriptor: %w", err)
	}

	sig, err := env.Wallet.SignOutputRaw(forfeitTx, signDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to sign: %w", err)
	}

	return sig.Serialize(), nil
}

// ProcessEvent handles events in RefreshRequestedState. The VTXO is waiting
// for acknowledgment or a forfeit request from the round actor.
func (s *RefreshRequestedState) ProcessEvent(
	ctx context.Context, event VTXOEvent, env *VTXOEnvironment,
) (*VTXOStateTransition, error) {

	switch evt := event.(type) {
	case *BlockEpochEvent:
		// Check if we've hit critical expiry while waiting for refresh.
		expiryStatus := env.ExpiryConfig.CheckExpiry(s.VTXO, evt.Height)

		if expiryStatus == ExpiryStatusCritical ||
			expiryStatus == ExpiryStatusExpired {

			blocksRemaining := BlocksUntilExpiry(s.VTXO, evt.Height)

			outbox := []VTXOOutMsg{
				&ExpiringNotification{
					VTXO:            s.VTXO,
					BlocksRemaining: blocksRemaining,
					Reason:          "refresh not completed in time",
				},
				&VTXOStatusUpdate{
					Outpoint:  s.VTXO.Outpoint,
					NewStatus: VTXOStatusExpiring,
				},
				&VTXOTerminatedNotification{
					VTXOOutpoint: s.VTXO.Outpoint,
					FinalState:   "Expiring",
					Reason:       "refresh timeout",
				},
			}

			return &VTXOStateTransition{
				NextState: &ExpiringState{
					VTXO:   s.VTXO,
					Reason: "critical expiry during refresh wait",
				},
				NewEvents: fn.Some(VTXOEmittedEvent{Outbox: outbox}),
			}, nil
		}

		// Still waiting, stay in this state.
		return &VTXOStateTransition{
			NextState: s,
		}, nil

	case *ForfeitRequestEvent:
		// Round actor is ready for forfeit. Build and sign the forfeit
		// tx to transfer this VTXO to the new round.
		forfeitTx, err := tx.BuildForfeitTx(
			&s.VTXO.Outpoint, s.VTXO.Amount,
			&evt.ConnectorOutpoint, evt.ServerForfeitPkScript,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to build forfeit tx: %w", err)
		}

		sig, err := signForfeitVTXOInput(s.VTXO, evt, forfeitTx, env)
		if err != nil {
			return nil, fmt.Errorf("failed to sign forfeit tx: %w", err)
		}

		forfeitTxID := forfeitTx.TxHash()

		return &VTXOStateTransition{
			NextState: &ForfeitingState{
				VTXO:              s.VTXO,
				NewRoundID:        evt.RoundID,
				ConnectorOutpoint: evt.ConnectorOutpoint,
				ForfeitTxID:       forfeitTxID,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{
				Outbox: []VTXOOutMsg{
					&ForfeitSignatureSubmission{
						VTXOOutpoint: s.VTXO.Outpoint,
						RoundID:      evt.RoundID,
						ForfeitTx:    forfeitTx,
						Signature:    sig,
					},
					&VTXOStatusUpdate{
						Outpoint:  s.VTXO.Outpoint,
						NewStatus: VTXOStatusForfeiting,
					},
				},
			}),
		}, nil

	case *RefreshAcknowledgedEvent:
		// Round actor acknowledged but no forfeit request yet.
		// Stay in RefreshRequestedState.
		return &VTXOStateTransition{
			NextState: s,
		}, nil

	case *ResumeVTXOEvent:
		// On resume, stay in this state. The round actor should have
		// persisted the refresh request.
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
		return nil, fmt.Errorf(
			"refresh_requested: unexpected event: %T", event,
		)
	}
}

// ProcessEvent handles events in ForfeitingState. The VTXO is being forfeited
// and waiting for the new commitment transaction to confirm.
func (s *ForfeitingState) ProcessEvent(
	ctx context.Context, event VTXOEvent, env *VTXOEnvironment,
) (*VTXOStateTransition, error) {

	switch evt := event.(type) {
	case *ForfeitSignedEvent:
		// Forfeit tx has been signed and submitted. Update the txid
		// and stay in this state.
		return &VTXOStateTransition{
			NextState: &ForfeitingState{
				VTXO:              s.VTXO,
				NewRoundID:        s.NewRoundID,
				ConnectorOutpoint: s.ConnectorOutpoint,
				ForfeitTxID:       evt.ForfeitTxID,
			},
		}, nil

	case *ForfeitConfirmedEvent:
		// New commitment tx confirmed, forfeit is complete.
		return &VTXOStateTransition{
			NextState: &ForfeitedState{
				VTXO:           s.VTXO,
				NewRoundID:     s.NewRoundID,
				CommitmentTxID: evt.CommitmentTxID,
			},
			NewEvents: fn.Some(VTXOEmittedEvent{
				Outbox: []VTXOOutMsg{
					&VTXOStatusUpdate{
						Outpoint:  s.VTXO.Outpoint,
						NewStatus: VTXOStatusForfeited,
					},
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
		// Check if we've hit expiry while forfeiting.
		expiryStatus := env.ExpiryConfig.CheckExpiry(s.VTXO, evt.Height)

		if expiryStatus == ExpiryStatusExpired {
			// This is a serious problem - the batch expired during
			// forfeit.
			return &VTXOStateTransition{
				NextState: &FailedState{
					VTXO:        s.VTXO,
					Reason:      "batch expired during forfeit",
					Recoverable: false,
				},
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
		return nil, fmt.Errorf("forfeiting: unexpected event: %T", event)
	}
}

// ProcessEvent for ForfeitedState. This is a terminal state, so all events
// result in staying in the same state.
func (s *ForfeitedState) ProcessEvent(
	_ context.Context, _ VTXOEvent, _ *VTXOEnvironment,
) (*VTXOStateTransition, error) {

	// Terminal state: self-loop on all events.
	return &VTXOStateTransition{
		NextState: s,
	}, nil
}

// ProcessEvent for ExpiringState. This is a terminal state, so all events
// result in staying in the same state.
func (s *ExpiringState) ProcessEvent(
	_ context.Context, _ VTXOEvent, _ *VTXOEnvironment,
) (*VTXOStateTransition, error) {

	// Terminal state: self-loop on all events.
	return &VTXOStateTransition{
		NextState: s,
	}, nil
}

// ProcessEvent for FailedState. This is a terminal state, so all events
// result in staying in the same state.
func (s *FailedState) ProcessEvent(
	_ context.Context, _ VTXOEvent, _ *VTXOEnvironment,
) (*VTXOStateTransition, error) {

	// Terminal state: self-loop on all events.
	return &VTXOStateTransition{
		NextState: s,
	}, nil
}
