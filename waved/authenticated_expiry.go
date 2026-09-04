package waved

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	expiryAuthenticationTimeout      = 30 * time.Second
	expiryConfirmationCleanupTimeout = 5 * time.Second
)

type chainSourceRef = actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
]

type confRef = actor.TellOnlyRef[chainsource.ConfirmationEvent]

type confirmationRegistration = chainsource.RegisterConfResponse

// batchExpiryAuthenticator adapts the chain-source actor's one-shot
// confirmation API to the pure VTXO expiry verifier. A fresh caller id keeps
// concurrent acceptance checks independent; each one-shot watch is removed
// after its future resolves.
func batchExpiryAuthenticator(
	chainSource chainSourceRef) oor.IncomingExpiryAuthenticator {

	return batchExpiryAuthenticatorWithTimeout(
		chainSource, expiryAuthenticationTimeout,
	)
}

// batchExpiryAuthenticatorWithTimeout builds the expiry authenticator with a
// bounded chain lookup. Durable callers retry a timed-out lookup instead of
// holding their actor turn and delivery lease indefinitely.
func batchExpiryAuthenticatorWithTimeout(chainSource chainSourceRef,
	timeout time.Duration) oor.IncomingExpiryAuthenticator {

	return func(ctx context.Context, ancestry []vtxo.Ancestry) (int32,
		error) {

		if chainSource == nil {
			return 0, fmt.Errorf("chain source must be provided")
		}
		if timeout <= 0 {
			return 0, fmt.Errorf("expiry authentication timeout " +
				"must be positive")
		}

		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		callerID := "expiry-auth-" + uuid.NewString()
		noNotify := fn.None[confRef]()

		return vtxo.AuthenticateBatchExpiry(
			ctx, ancestry,
			func(ctx context.Context, txid chainhash.Hash,
				pkScript []byte, heightHint uint32) (
				vtxo.CommitmentConfirmation, error) {

				var empty vtxo.CommitmentConfirmation

				request := &chainsource.RegisterConfRequest{
					CallerID:    callerID,
					Txid:        &txid,
					PkScript:    pkScript,
					TargetConfs: 1,
					HeightHint:  heightHint,
					NotifyActor: noNotify,
				}

				response, err := chainSource.Ask(
					ctx, request,
				).Await(ctx).Unpack()
				if err != nil {
					return empty, err
				}

				registered, ok :=
					response.(*confirmationRegistration)
				if !ok || registered.Future == nil {
					return empty, fmt.Errorf("unexpected "+
						"response %T", response)
				}

				defer unregisterExpiryConfirmation(
					ctx, chainSource, callerID, txid,
					pkScript,
				)

				confirmation, err := registered.Future.
					Await(ctx).
					Unpack()
				if err != nil {
					return empty, err
				}

				return vtxo.CommitmentConfirmation{
					Tx:          confirmation.Tx,
					BlockHeight: confirmation.BlockHeight,
				}, nil
			},
		)
	}
}

// unregisterExpiryConfirmation releases a one-shot confirmation watch with a
// cleanup deadline independent of the completed acceptance request.
func unregisterExpiryConfirmation(ctx context.Context,
	chainSource chainSourceRef, callerID string, txid chainhash.Hash,
	pkScript []byte) {

	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), expiryConfirmationCleanupTimeout,
	)
	defer cancel()

	_ = chainSource.Tell(cleanupCtx, &chainsource.UnregisterConfRequest{
		CallerID:    callerID,
		Txid:        &txid,
		PkScript:    pkScript,
		TargetConfs: 1,
	})
}
