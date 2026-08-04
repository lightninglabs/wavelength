package waved

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lwwallet"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightninglabs/wavelength/waverpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSignOutSwapHtlcAckValidatesTerms verifies malformed acknowledgement
// terms are rejected before wallet signing is attempted.
func TestSignOutSwapHtlcAckValidatesTerms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  *waverpc.SignOutSwapHtlcAckRequest
	}{
		{
			name: "payment hash",
			req: &waverpc.SignOutSwapHtlcAckRequest{
				AmountSat: 42_000,
				VhtlcPkScript: []byte{
					0x51,
				},
			},
		},
		{
			name: "amount",
			req: &waverpc.SignOutSwapHtlcAckRequest{
				PaymentHash: make([]byte, 32),
				VhtlcPkScript: []byte{
					0x51,
				},
			},
		},
		{
			name: "pkScript",
			req: &waverpc.SignOutSwapHtlcAckRequest{
				PaymentHash: make([]byte, 32),
				AmountSat:   42_000,
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := &RPCServer{}
			_, err := server.SignOutSwapHtlcAck(
				context.Background(), test.req,
			)
			require.Equal(
				t, codes.InvalidArgument, status.Code(err),
			)
		})
	}
}

// TestSignOutSwapHtlcAckSignsTerms verifies the daemon signs the canonical
// acknowledgement digest with its wallet identity key.
func TestSignOutSwapHtlcAckSignsTerms(t *testing.T) {
	t.Parallel()

	wallet := newFundedLwWallet(t)
	keyLocator := keychain.KeyLocator{
		Family: keychain.KeyFamilyNodeKey,
		Index:  0,
	}
	keyDesc, err := wallet.KeyRing().DeriveKey(keyLocator)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(keyDesc.PubKey)
	require.NoError(t, err)

	walletReady := make(chan struct{})
	close(walletReady)
	rpcServer := &RPCServer{
		server: &Server{
			walletReady:   walletReady,
			clientKeyDesc: keyDesc,
			lwWallet:      fn.Some(wallet),
		},
	}

	paymentHash := lntypes.Hash{0x01, 0x02, 0x03}
	const amountSat = uint64(42_000)
	resp, err := rpcServer.SignOutSwapHtlcAck(
		t.Context(), &waverpc.SignOutSwapHtlcAckRequest{
			PaymentHash:   paymentHash[:],
			AmountSat:     amountSat,
			VhtlcPkScript: pkScript,
		},
	)
	require.NoError(t, err)

	sig, err := schnorr.ParseSignature(resp.GetSignature())
	require.NoError(t, err)
	digest := swaprpc.OutSwapHTLCAckDigest(
		keyDesc.PubKey, paymentHash, amountSat, pkScript,
	)
	require.True(t, sig.Verify(digest[:], keyDesc.PubKey))
}

// TestWrongPassphraseErrorMapping pins the wrong-password
// classification that UnlockWallet relies on to return
// codes.InvalidArgument instead of codes.Internal. The waddrmgr error
// must survive errors.As through the real wrap chain: btcwallet's
// unlock failure, lwwallet's "start btcwallet: %w", and the daemon's
// "start lwwallet: %w". A refactor that switches any hop to %v would
// silently degrade the RPC error code; this test catches that.
func TestWrongPassphraseErrorMapping(t *testing.T) {
	t.Parallel()

	// A minimal Esplora stub serving a static genesis-only chain is
	// enough to construct the wallet.
	tipHash := chaincfg.RegressionNetParams.GenesisHash.String()
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/blocks/tip/height":
			_, err := w.Write([]byte("0"))
			require.NoError(t, err)

		case "/block-height/0":
			_, err := w.Write([]byte(tipHash))
			require.NoError(t, err)

		case "/block/" + tipHash:
			err := json.NewEncoder(w).Encode(map[string]any{
				"id":        tipHash,
				"height":    0,
				"timestamp": 1,
			})
			require.NoError(t, err)

		default:
			http.NotFound(w, r)
		}
	}
	esplora := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(esplora.Close)

	dbDir := t.TempDir()
	var seed [rawSeedLen]byte
	for i := range seed {
		seed[i] = byte(i + 3)
	}

	cfg := lwwallet.Config{
		Seed:           seed[:],
		WalletPassword: []byte("the-real-password"),
		EsploraURL:     esplora.URL,
		ChainParams:    &chaincfg.RegressionNetParams,
		PollInterval:   time.Hour,
		RecoveryWindow: 10,
		DBDir:          dbDir,
		Log:            fn.None[btclog.Logger](),
	}

	// Create the wallet, then release it so the reopen below hits
	// the real unlock path.
	w, err := lwwallet.New(cfg)
	require.NoError(t, err)
	require.NoError(t, w.Start())
	w.Stop()

	// Reopen with a wrong password: New succeeds (the public pass
	// is static) and Start fails at btcwallet's unlock.
	cfg.Seed = nil
	cfg.WalletPassword = []byte("not-the-password")
	w, err = lwwallet.New(cfg)
	require.NoError(t, err)

	startErr := w.Start()
	require.Error(t, startErr)

	// Mirror the daemon-side wrap applied by startLwwallet before
	// the RPC layer classifies the error.
	wrapped := fmt.Errorf("start lwwallet: %w", startErr)
	require.True(t, isWrongPassphraseErr(wrapped))

	// A generic start failure must not classify as a wrong
	// passphrase, or real daemon faults would surface as
	// InvalidArgument.
	generic := fmt.Errorf("start lwwallet: %w",
		errors.New("esplora unreachable"))
	require.False(t, isWrongPassphraseErr(generic))
}
