package chainsource

import (
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// TestChainSourceActorBroadcastTxRejectsUnprovenPublication proves that spent
// inputs and insufficient replacement fees do not establish that the submitted
// transaction is known. Without read-only evidence, preserve the original
// error.
func TestChainSourceActorBroadcastTxRejectsUnprovenPublication(t *testing.T) {
	t.Parallel()

	for _, broadcastErr := range []error{
		errors.New("transaction rejected: output already spent"),
		fmt.Errorf("publish: %w", chain.ErrInsufficientFee),
	} {
		t.Run(broadcastErr.Error(), func(t *testing.T) {
			backend := &broadcastErrorBackend{
				mockBackend:  newMockBackend(),
				broadcastErr: broadcastErr,
				mempoolErr: errors.New(
					"testmempoolaccept not " +
						"supported",
				),
			}

			system := actor.NewActorSystem()
			defer func() {
				_ = system.Shutdown(t.Context())
			}()

			chainSource := NewChainSourceActor(ChainSourceConfig{
				Backend: backend,
				System:  system,
			})
			ref := ChainSourceKey.Spawn(
				system, "chainsource-broadcast-spent-input",
				chainSource,
			)

			tx := wire.NewMsgTx(2)
			future := ref.Ask(t.Context(), &BroadcastTxRequest{
				Tx:    tx,
				Label: "rejected-publication",
			})

			result := future.Await(t.Context())
			require.True(t, result.IsErr())
			require.ErrorIs(t, result.Err(), broadcastErr)
		})
	}
}
