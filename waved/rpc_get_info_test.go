package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/btcwbackend"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/waverpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// heightChainBackend is a minimal chain backend that only answers best-block
// queries, which is all the GetInfo height lookup needs.
type heightChainBackend struct {
	chainsource.ChainBackend

	height int32
	err    error
}

// BestBlock returns the scripted best block height or lookup failure.
func (b *heightChainBackend) BestBlock(context.Context) (int32, chainhash.Hash,
	error) {

	return b.height, chainhash.Hash{}, b.err
}

// TestGetInfoBestBlockHeightRequiresLookup verifies GetInfo reports the best
// block height from the active local wallet backend and fails the call when
// that lookup fails, rather than returning a zero height that callers would
// read as chain state. A backend that does answer with genesis still succeeds.
func TestGetInfoBestBlockHeightRequiresLookup(t *testing.T) {
	wallet := newFundedLwWallet(t)
	lookupErr := errors.New("chain backend unavailable")

	tests := []struct {
		name       string
		walletType string
		height     int32
		lookupErr  error
	}{
		{
			name:       "lwwallet lookup failure",
			walletType: WalletTypeLwwallet,
			lookupErr:  lookupErr,
		},
		{
			name:       "btcwallet lookup failure",
			walletType: WalletTypeBtcwallet,
			lookupErr:  lookupErr,
		},
		{
			name:       "lwwallet genesis height",
			walletType: WalletTypeLwwallet,
			height:     0,
		},
		{
			name:       "btcwallet tip height",
			walletType: WalletTypeBtcwallet,
			height:     812_345,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := &Server{
				cfg: &Config{
					Wallet: &WalletConfig{
						Type: tc.walletType,
					},
				},
				chainBackend: &heightChainBackend{
					height: tc.height,
					err:    tc.lookupErr,
				},
				log: btclog.Disabled,
			}
			if tc.walletType == WalletTypeLwwallet {
				server.lwWallet = fn.Some(wallet)
			} else {
				server.btcwWallet = fn.Some(
					&btcwbackend.Wallet{
						Wallet: wallet.Wallet,
					},
				)
			}

			resp, err := NewRPCServer(server).GetInfo(
				t.Context(), &waverpc.GetInfoRequest{},
			)
			if tc.lookupErr != nil {
				require.ErrorIs(t, err, tc.lookupErr)
				require.Nil(t, resp)

				return
			}

			require.NoError(t, err)
			require.Equal(t, uint32(tc.height), resp.BlockHeight)
		})
	}
}
