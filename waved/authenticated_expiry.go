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
	expiryAuthenticationTimeout         = 30 * time.Second
	incomingExpiryAuthenticationTimeout = 2 * time.Second
	expiryConfirmationCleanupTimeout    = 5 * time.Second
)

type chainSourceRef = actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
]

type confRef = actor.TellOnlyRef[chainsource.ConfirmationEvent]

type confirmationRegistration = chainsource.RegisterConfResponse

// NewBatchExpiryAuthenticator adapts the chain-source actor's one-shot
// confirmation API to the VTXO batch-evidence verifier. A fresh caller id
// keeps concurrent acceptance checks independent; each one-shot watch is
// removed after its future resolves. Unknown heights use the network deployment
// scanFloor when it is usable at the current tip, or block 1 otherwise,
// matching legacy unroll scans. Nonzero hints pass through unchanged.
func NewBatchExpiryAuthenticator(chainSource actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
], scanFloor uint32) oor.IncomingExpiryAuthenticator {

	return batchExpiryAuthenticatorWithTimeout(
		chainSource, scanFloor, expiryAuthenticationTimeout,
	)
}

// batchExpiryAuthenticatorWithTimeout builds the batch-evidence authenticator
// with a bounded chain lookup. Durable callers retry a timed-out lookup
// instead of holding their actor turn and delivery lease indefinitely.
func batchExpiryAuthenticatorWithTimeout(chainSource chainSourceRef,
	scanFloor uint32,
	timeout time.Duration) oor.IncomingExpiryAuthenticator {

	return func(ctx context.Context, ancestry []vtxo.Ancestry) (
		vtxo.AuthenticatedBatchEvidence, error) {

		var empty vtxo.AuthenticatedBatchEvidence

		if chainSource == nil {
			return empty, fmt.Errorf("chain source must be " +
				"provided")
		}
		if timeout <= 0 {
			return empty, fmt.Errorf("expiry authentication " +
				"timeout must be positive")
		}

		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		callerID := "expiry-auth-" + uuid.NewString()
		noNotify := fn.None[confRef]()

		return vtxo.AuthenticateBatchEvidence(
			ctx, ancestry,
			func(ctx context.Context, txid chainhash.Hash,
				pkScript []byte, heightHint uint32) (
				vtxo.CommitmentConfirmation, error) {

				var empty vtxo.CommitmentConfirmation

				if heightHint == 0 {
					var err error
					heightHint, err = expiryScanFloor(
						ctx, chainSource, scanFloor,
					)
					if err != nil {
						return empty, err
					}
				}

				request := &chainsource.RegisterConfRequest{
					CallerID:    callerID,
					Txid:        &txid,
					PkScript:    pkScript,
					TargetConfs: 1,
					HeightHint:  heightHint,
					NotifyActor: noNotify,
				}
				defer unregisterExpiryConfirmation(
					ctx, chainSource, callerID, txid,
					pkScript,
				)

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

// expiryScanFloor bounds unknown-height scans by the network's
// deployment floor only when the local tip has reached it. Local networks,
// unusable floors, and nonpositive tips use block 1, as unroll does. Tip lookup
// failures remain retryable within the caller's existing authentication budget.
func expiryScanFloor(ctx context.Context, chainSource chainSourceRef,
	floor uint32) (uint32, error) {

	if floor <= 1 {
		return 1, nil
	}

	response, err := chainSource.Ask(
		ctx, &chainsource.BestHeightRequest{},
	).Await(ctx).Unpack()
	if err != nil {
		return 0, fmt.Errorf("get expiry scan tip: %w", err)
	}

	tip, ok := response.(*chainsource.BestHeightResponse)
	if !ok {
		return 0, fmt.Errorf("unexpected best height response %T",
			response)
	}
	if tip.Height <= 0 || floor > uint32(tip.Height) {
		return 1, nil
	}

	return floor, nil
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
