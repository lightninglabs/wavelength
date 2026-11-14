package txconfirm

import (
	"bytes"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainbackends"
	"github.com/lightninglabs/wavelength/walletcore"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestFeeBumpFailureRecovery verifies that a known child's replacement floor
// stays diagnostic while unrelated failures warn. Both paths retain the
// confirmation watch and retry only after the next full block interval.
func TestFeeBumpFailureRecovery(t *testing.T) {
	tx := makeFundedAnchorTx(42, 330)
	txid := tx.TxHash()
	tests := []struct {
		name         string
		packageErr   error
		diagnostic   string
		errorDetail  string
		warning      bool
		foreignFloor int64
	}{
		{
			name: "known child fee floor",
			packageErr: errors.Join(
				chainbackends.NewPackageTxError(
					"parent", txid, "txn-already-known",
				),
				chainbackends.NewPackageTxError(
					"child", chainhash.Hash{0xab}, "insu"+
						"fficient fee, rejecting "+
						"replacement; new feerate "+
						"0.00002 BTC/kvB <= old "+
						"feerate 0.00004925 BTC/kvB",
				),
			),
			diagnostic: "[DBG]: Fee bump child conflicts with an " +
				"existing child; retaining fee floor " +
				"for retry",
			errorDetail:  "old feerate 0.00004925 BTC/kvB",
			foreignFloor: 4,
		},
		{
			name:        "backend unavailable",
			packageErr:  errors.New("backend unavailable"),
			diagnostic:  "[WRN]: Fee bump failed, will retry",
			errorDetail: "backend unavailable",
			warning:     true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := btclog.NewSLogger(
				btclog.NewDefaultHandler(&buf),
			)
			logger.SetLevel(btclog.LevelTrace)

			chain := newFakeChainSourceRef(100)
			chain.feeRate = 1
			ref, behavior := newTestActor(t, Config{
				ChainSource: chain,
				Wallet: &fakeWallet{
					utxos: []*walletcore.Utxo{
						makeWalletUTXOWithAmount(
							1_000_000, 0xac,
						),
					},
				},
				FeeBumpIntervalBlocks: 2,
				Log: fn.Some[btclog.Logger](
					logger,
				),
			})

			// A funded parent first reaches the network without a
			// child.
			sub := actor.NewChannelTellOnlyRef[Notification](
				"sub", 4,
			)
			resp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
				Tx: tx, Subscriber: sub,
			})
			require.Equal(
				t, TxStateAwaitingConfirmation, resp.State,
			)
			require.Equal(t, 1, chain.broadcastCallCount())
			require.Zero(t, chain.packageCallCount())

			chain.mu.Lock()
			chain.packageErr = test.packageErr
			chain.mu.Unlock()
			chain.emitBlock(t, 102)
			require.Equal(
				t, TxStateAwaitingConfirmation,
				mustTrackedState(
					t, ref.Ref(), tx, sub,
				),
			)
			require.Equal(t, 1, chain.packageCallCount())
			require.Equal(t, 1, chain.registerConfCount())
			require.Zero(t, chain.unregisterConfCount())
			mustHaveNoNotification(t, sub)

			// Assert the diagnostic is present as well as its
			// severity, so deleting the log cannot satisfy the
			// noise regression.
			require.Contains(t, buf.String(), test.diagnostic)
			require.Contains(t, buf.String(), test.errorDetail)
			require.Equal(
				t, test.warning,
				bytes.Contains(
					buf.Bytes(), []byte("[WRN]"),
				),
			)
			if test.foreignFloor > 0 {
				require.Equal(
					t, test.foreignFloor,
					behavior.broadcaster.parentStates[txid].
						ForeignFeeRateFloor,
				)
			}

			chain.mu.Lock()
			chain.packageErr = nil
			chain.mu.Unlock()

			// Recovery must not turn a duplicate block into a hot
			// retry.
			for _, height := range []int32{102, 103} {
				chain.emitBlock(t, height)
				require.Equal(
					t, TxStateAwaitingConfirmation,
					mustTrackedState(
						t, ref.Ref(), tx, sub,
					),
				)
				require.Equal(t, 1, chain.packageCallCount())
			}
			chain.emitBlock(t, 104)
			require.Equal(
				t, TxStateAwaitingConfirmation,
				mustTrackedState(
					t, ref.Ref(), tx, sub,
				),
			)
			require.Equal(t, 2, chain.packageCallCount())
			require.Greater(
				t, behavior.broadcaster.parentStates[txid].
					LastFeeRate,
				test.foreignFloor,
			)

			// The original watch still delivers confirmation after
			// the rejected child and the later successful
			// replacement.
			require.Equal(t, 1, chain.registerConfCount())
			require.Zero(t, chain.unregisterConfCount())
			chain.emitConfirmation(t, txid, 105)
			require.IsType(
				t, &TxConfirmed{},
				mustAwaitNotification(t, sub),
			)
		})
	}
}
