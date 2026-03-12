package darepod

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/stretchr/testify/require"
)

// newTestRPCServer creates a minimal RPCServer with chain params set
// for regtest. Only resolveRecipientOutput is usable.
func newTestRPCServer() *RPCServer {
	return &RPCServer{
		server: &Server{
			chainParams: &chaincfg.RegressionNetParams,
		},
	}
}

// TestResolveRecipientOutputPubkey verifies that a raw x-only pubkey
// destination correctly yields both a taproot pkScript and the parsed
// public key.
func TestResolveRecipientOutputPubkey(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	_, pub := btcec.PrivKeyFromBytes(
		[]byte("test-key-data-for-resolve-output"),
	)
	xOnly := pub.SerializeCompressed()[1:]

	out := &daemonrpc.Output{
		Destination: &daemonrpc.Output_Pubkey{
			Pubkey: xOnly,
		},
		AmountSat: 50_000,
	}

	pkScript, clientKey, err := r.resolveRecipientOutput(out)
	require.NoError(t, err)
	require.NotEmpty(t, pkScript)
	require.NotNil(t, clientKey)

	// The pkScript should be a valid P2TR output.
	require.Len(t, pkScript, 34)
	require.Equal(t, byte(0x51), pkScript[0]) // OP_1
	require.Equal(t, byte(0x20), pkScript[1]) // push 32

	// The client key should match the input pubkey.
	require.True(t, clientKey.IsEqual(pub))
}

// TestResolveRecipientOutputAddress verifies that a taproot address
// destination extracts the correct pkScript and client key.
func TestResolveRecipientOutputAddress(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	_, pub := btcec.PrivKeyFromBytes(
		[]byte("test-key-data-for-resolve-addr."),
	)
	xOnly := pub.SerializeCompressed()[1:]

	addr, err := btcutil.NewAddressTaproot(
		xOnly, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	out := &daemonrpc.Output{
		Destination: &daemonrpc.Output_Address{
			Address: addr.EncodeAddress(),
		},
		AmountSat: 100_000,
	}

	pkScript, clientKey, err := r.resolveRecipientOutput(out)
	require.NoError(t, err)
	require.NotEmpty(t, pkScript)

	// The taproot witness program IS the x-only pubkey, so the
	// extracted key matches the original (not tweaked).
	require.Equal(t, xOnly, clientKey.SerializeCompressed()[1:])
}

// TestResolveRecipientOutputPkScriptRejected verifies that raw
// pk_script destinations are rejected for directed sends.
func TestResolveRecipientOutputPkScriptRejected(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	out := &daemonrpc.Output{
		Destination: &daemonrpc.Output_PkScript{
			PkScript: []byte{0x51, 0x20, 0x01},
		},
		AmountSat: 50_000,
	}

	_, _, err := r.resolveRecipientOutput(out)
	require.Error(t, err)
	require.Contains(t, err.Error(), "directed sends require")
}

// TestResolveRecipientOutputNonTaprootRejected verifies that
// non-taproot addresses are rejected for directed sends.
func TestResolveRecipientOutputNonTaprootRejected(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	out := &daemonrpc.Output{
		Destination: &daemonrpc.Output_Address{
			Address: "bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080",
		},
		AmountSat: 50_000,
	}

	_, _, err := r.resolveRecipientOutput(out)
	require.Error(t, err)
	require.Contains(t, err.Error(), "taproot address")
}

// TestResolveRecipientOutputInvalidPubkey verifies that a malformed
// pubkey is rejected.
func TestResolveRecipientOutputInvalidPubkey(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	out := &daemonrpc.Output{
		Destination: &daemonrpc.Output_Pubkey{
			Pubkey: []byte{0x01, 0x02, 0x03},
		},
		AmountSat: 50_000,
	}

	_, _, err := r.resolveRecipientOutput(out)
	require.Error(t, err)
	require.Contains(t, err.Error(), "32 bytes")
}
