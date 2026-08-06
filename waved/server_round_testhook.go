package waved

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/round"
)

// TriggerRoundRegistration injects an IntentRequested event into the client
// round actor. The RPC server and integration harness use this to advance a
// queued round intent without reaching through private actor internals
// directly.
//
// IntentRequested is fire-and-forget from the caller's perspective: once the
// FSM accepts the event it runs the registration handshake against the
// operator in the round actor's own turn loop. The caller's context is
// plumbed into the actor envelope as callerCtx and is what the FSM's
// downstream forfeit-VTXO lookup (during JoinRoundRequest validation)
// observes. Reusing the caller's ctx there means an RPC return cancels the
// ctx mid-handshake and the round transitions to ClientFailed with "context
// canceled". Detach the Ask with context.WithoutCancel so the trigger
// reaches the actor regardless of the caller's lifetime, while the round's
// own lifetime continues to be governed by the actor system's shutdown.
//
// The Await keeps the original ctx because Await's ctx is purely local — it
// only controls how long this goroutine blocks at the promise's channel and
// never reaches the actor. Using the caller's ctx here lets the RPC handler
// unblock promptly if the caller disconnects, while the FSM continues
// processing in the background under askCtx.
func (s *Server) TriggerRoundRegistration(ctx context.Context) error {
	if s.actorSystem == nil {
		return fmt.Errorf("actor system not initialized")
	}

	askCtx := context.WithoutCancel(ctx)

	roundRef := round.NewServiceKey().Ref(s.actorSystem)
	future := roundRef.Ask(askCtx, &round.ServerMessageNotification{
		Message: &round.IntentRequested{},
	})
	result := future.Await(ctx)
	if err := result.Err(); err != nil {
		return fmt.Errorf("failed to trigger round registration: %w",
			err)
	}

	return nil
}

// RegisterAssetVTXORequest registers one Taproot Asset VTXO request with
// the client round actor: the next round intent asks the operator's asset
// round for a leaf carrying assetAmount units anchored by amountSat. The
// same context-detachment rationale as TriggerRoundRegistration applies.
func (s *Server) RegisterAssetVTXORequest(ctx context.Context,
	amountSat btcutil.Amount, assetRef string, assetAmount uint64) error {

	if s.actorSystem == nil {
		return fmt.Errorf("actor system not initialized")
	}

	askCtx := context.WithoutCancel(ctx)

	roundRef := round.NewServiceKey().Ref(s.actorSystem)
	future := roundRef.Ask(askCtx, &round.RegisterVTXORequestsRequest{
		AssetRequests: []round.AssetVTXORequest{{
			AmountSat:   amountSat,
			AssetRef:    assetRef,
			AssetAmount: assetAmount,
		}},
	})
	result := future.Await(ctx)
	if err := result.Err(); err != nil {
		return fmt.Errorf("failed to register asset VTXO request: %w",
			err)
	}

	return nil
}

// RegisterAssetBoarding registers a confirmed asset boarding output with
// the client round actor: the next round intent boards it, disclosing
// the asset material the operator authenticates. The same
// context-detachment rationale as TriggerRoundRegistration applies.
func (s *Server) RegisterAssetBoarding(ctx context.Context,
	req *round.RegisterAssetBoardingRequest) error {

	if s.actorSystem == nil {
		return fmt.Errorf("actor system not initialized")
	}

	askCtx := context.WithoutCancel(ctx)

	roundRef := round.NewServiceKey().Ref(s.actorSystem)
	future := roundRef.Ask(askCtx, req)
	result := future.Await(ctx)
	if err := result.Err(); err != nil {
		return fmt.Errorf("failed to register asset boarding: %w", err)
	}

	return nil
}
