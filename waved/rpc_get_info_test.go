package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/btcwbackend"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/lwwallet"
	"github.com/lightninglabs/wavelength/waverpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// heightChainKit answers best-block queries through the lnd wallet arm.
type heightChainKit struct {
	lndclient.ChainKitClient

	height int32
	err    error
}

// GetBestBlock returns the scripted lnd height or lookup failure.
func (b *heightChainKit) GetBestBlock(context.Context) (chainhash.Hash, int32,
	error) {

	return chainhash.Hash{}, b.height, b.err
}

// TestGetInfoBestBlockHeightRequiresLookup verifies GetInfo reports the best
// block height from the active local wallet backend and fails the call when
// that lookup fails, rather than returning a zero height that callers would
// read as chain state. A backend that does answer with genesis still succeeds.
func TestGetInfoBestBlockHeightRequiresLookup(t *testing.T) {
	t.Parallel()

	identityKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	lookupErr := errors.New("chain backend unavailable")

	tests := []struct {
		name       string
		walletType string
		height     int32
		lookupErr  error
	}{
		{
			name:       "lnd lookup failure",
			walletType: WalletTypeLnd,
			lookupErr:  lookupErr,
		},
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
			name:       "lnd genesis height",
			walletType: WalletTypeLnd,
		},
		{
			name:       "lwwallet genesis height",
			walletType: WalletTypeLwwallet,
			height:     0,
		},
		{
			name:       "btcwallet genesis height",
			walletType: WalletTypeBtcwallet,
		},
		{
			name:       "lnd tip height",
			walletType: WalletTypeLnd,
			height:     812_345,
		},
		{
			name:       "lwwallet tip height",
			walletType: WalletTypeLwwallet,
			height:     812_345,
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

			// GetInfo uses the stable identity descriptor after
			// startup; height reads need no real wallet database in
			// this test.
			server.storeClientKeyDesc(keychain.KeyDescriptor{
				PubKey: identityKey.PubKey(),
			})
			switch tc.walletType {
			case WalletTypeLnd:
				server.lnd = fn.Some(&lndclient.GrpcLndServices{
					ChainKit: &heightChainKit{
						height: tc.height,
						err:    tc.lookupErr,
					},
				})

			case WalletTypeLwwallet:
				server.lwWallet = fn.Some(&lwwallet.Wallet{})

			case WalletTypeBtcwallet:
				server.btcwWallet = fn.Some(
					&btcwbackend.Wallet{},
				)
			}

			resp, err := NewRPCServer(server).GetInfo(
				t.Context(), &waverpc.GetInfoRequest{},
			)
			if tc.lookupErr != nil {
				require.ErrorIs(t, err, tc.lookupErr)
				require.Equal(
					t, codes.Unavailable, status.Code(err),
				)
				require.Nil(t, resp)

				return
			}

			require.NoError(t, err)
			require.Equal(t, uint32(tc.height), resp.BlockHeight)
		})
	}
}
