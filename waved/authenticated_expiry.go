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

const expiryConfirmationCleanupTimeout = 5 * time.Second

// batchExpiryAuthenticator adapts the chain-source actor's one-shot
// confirmation API to the pure VTXO expiry verifier. A fresh caller id keeps
// concurrent acceptance checks independent; each one-shot watch is removed
// after its future resolves.
func batchExpiryAuthenticator(chainSource actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
]) oor.IncomingExpiryAuthenticator {

	return func(ctx context.Context, ancestry []vtxo.Ancestry) (int32,
		error) {

		if chainSource == nil {
			return 0, fmt.Errorf("chain source must be provided")
		}

		callerID := "expiry-auth-" + uuid.NewString()
		return vtxo.AuthenticateBatchExpiry(
			ctx, ancestry,
			func(ctx context.Context, txid chainhash.Hash,
				pkScript []byte, heightHint uint32) (
				vtxo.CommitmentConfirmation, error) {

				request := &chainsource.RegisterConfRequest{
					CallerID:    callerID,
					Txid:        &txid,
					PkScript:    pkScript,
					TargetConfs: 1,
					HeightHint:  heightHint,
					NotifyActor: fn.None[actor.TellOnlyRef[chainsource.ConfirmationEvent]](),
				}

				response, err := chainSource.Ask(
					ctx, request,
				).Await(ctx).Unpack()
				if err != nil {
					return vtxo.CommitmentConfirmation{}, err
				}

				registered, ok := response.(*chainsource.RegisterConfResponse)
				if !ok || registered.Future == nil {
					return vtxo.CommitmentConfirmation{}, fmt.Errorf(
						"unexpected confirmation registration response %T",
						response,
					)
				}

				defer unregisterExpiryConfirmation(
					chainSource, callerID, txid, pkScript,
				)

				confirmation, err := registered.Future.Await(ctx).Unpack()
				if err != nil {
					return vtxo.CommitmentConfirmation{}, err
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
func unregisterExpiryConfirmation(chainSource actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
], callerID string, txid chainhash.Hash, pkScript []byte) {

	ctx, cancel := context.WithTimeout(
		context.Background(), expiryConfirmationCleanupTimeout,
	)
	defer cancel()

	_ = chainSource.Tell(ctx, &chainsource.UnregisterConfRequest{
		CallerID:    callerID,
		Txid:        &txid,
		PkScript:    pkScript,
		TargetConfs: 1,
	})
}
