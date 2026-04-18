package darepod

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// TestResolveRecipientOutputPolicyTemplateStandard verifies that directed
// sends can resolve a standard policy template into both a concrete
// pkScript and the owner key needed for collaborative VTXO creation.
func TestResolveRecipientOutputPolicyTemplateStandard(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		ownerPriv.PubKey(), operatorPriv.PubKey(), 144,
	)
	require.NoError(t, err)

	out := &daemonrpc.Output{
		Destination: &daemonrpc.Output_PolicyTemplate{
			PolicyTemplate: policyTemplate,
		},
		AmountSat: 50_000,
	}

	pkScript, clientKey, err := r.resolveRecipientOutput(out)
	require.NoError(t, err)
	require.NotEmpty(t, pkScript)
	require.Equal(
		t, schnorr.SerializePubKey(ownerPriv.PubKey()),
		schnorr.SerializePubKey(clientKey),
	)
}

// TestResolveRecipientOutputPolicyTemplateCustomRejected verifies that
// directed sends reject non-standard policy templates that do not expose
// the collaborative owner key required for VTXO creation.
func TestResolveRecipientOutputPolicyTemplateCustomRejected(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	out := &daemonrpc.Output{
		Destination: &daemonrpc.Output_PolicyTemplate{
			PolicyTemplate: []byte{0x01},
		},
		AmountSat: 50_000,
	}

	_, _, err := r.resolveRecipientOutput(out)
	require.Error(t, err)
	require.Contains(t, err.Error(), "decode policy_template")
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

// encodeStandardRecipientPolicy was hardened in this branch to return gRPC
// status errors on every precondition failure instead of silently returning
// (nil, nil). The silent-passthrough version would have emitted a
// "policyless" VTXO that bypassed admission validation, so regression
// coverage on the three fail-closed paths plus the happy path is essential.

// TestEncodeStandardRecipientPolicyHappy verifies the happy path: valid
// inputs whose compiled pkScript matches the caller's expected pkScript
// return a non-empty policy template and no error.
func TestEncodeStandardRecipientPolicyHappy(t *testing.T) {
	t.Parallel()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const exitDelay uint32 = 144

	// Derive the expected pkScript the way the caller in SendVTXO does:
	// compile the standard VTXO policy and take its P2TR script.
	policy, err := arkscript.NewVTXOPolicy(
		ownerPriv.PubKey(), operatorPriv.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(policy.OutputKey())
	require.NoError(t, err)

	template, err := encodeStandardRecipientPolicy(
		ownerPriv.PubKey(), operatorPriv.PubKey(), exitDelay, pkScript,
	)
	require.NoError(t, err)
	require.NotEmpty(t, template)
}

// TestEncodeStandardRecipientPolicyNilOwner verifies that a nil owner key
// is rejected with codes.InvalidArgument and a descriptive message. A
// silent pass-through here would let a client receive funds on a policy
// that has no collab leaf for any owner.
func TestEncodeStandardRecipientPolicyNilOwner(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	template, err := encodeStandardRecipientPolicy(
		nil, operatorPriv.PubKey(), 144, []byte{0x51},
	)
	require.Error(t, err)
	require.Nil(t, template)

	st, ok := status.FromError(err)
	require.True(t, ok, "expected gRPC status error, got %T", err)
	require.Equal(t, codes.InvalidArgument, st.Code())
	require.Contains(t, st.Message(), "owner key must be provided")
}

// TestEncodeStandardRecipientPolicyNilOperator verifies that a nil
// operator key is rejected with codes.FailedPrecondition. This path
// triggers when operator terms have not been fetched yet; silently
// substituting a nil would emit a policy with no operator cosigner.
func TestEncodeStandardRecipientPolicyNilOperator(t *testing.T) {
	t.Parallel()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	template, err := encodeStandardRecipientPolicy(
		ownerPriv.PubKey(), nil, 144, []byte{0x51},
	)
	require.Error(t, err)
	require.Nil(t, template)

	st, ok := status.FromError(err)
	require.True(t, ok, "expected gRPC status error, got %T", err)
	require.Equal(t, codes.FailedPrecondition, st.Code())
	require.Contains(t, st.Message(), "operator key must be fetched")
}

// TestEncodeStandardRecipientPolicyZeroExitDelay verifies that a zero
// exit delay is rejected fail-closed. A 1-block CSV would break the
// forfeit incentive, and silently encoding with zero would defeat the
// admission validation that downstream forfeit logic depends on.
func TestEncodeStandardRecipientPolicyZeroExitDelay(t *testing.T) {
	t.Parallel()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	template, err := encodeStandardRecipientPolicy(
		ownerPriv.PubKey(), operatorPriv.PubKey(), 0, []byte{0x51},
	)
	require.Error(t, err)
	require.Nil(t, template)

	st, ok := status.FromError(err)
	require.True(t, ok, "expected gRPC status error, got %T", err)
	require.Equal(t, codes.FailedPrecondition, st.Code())
	require.Contains(t, st.Message(), "exit delay must be non-zero")
}

// TestEncodeStandardRecipientPolicyPkScriptMismatch verifies that a
// pkScript that does not match the compiled policy is rejected with
// codes.Internal. Accepting this silently would let a caller quote one
// script while the operator commits the VTXO under a different one.
func TestEncodeStandardRecipientPolicyPkScriptMismatch(t *testing.T) {
	t.Parallel()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// An arbitrary 34-byte P2TR script that is not the one derived from
	// the supplied policy parameters.
	unrelated, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	bogusPkScript, err := txscript.PayToTaprootScript(unrelated.PubKey())
	require.NoError(t, err)

	template, err := encodeStandardRecipientPolicy(
		ownerPriv.PubKey(), operatorPriv.PubKey(), 144, bogusPkScript,
	)
	require.Error(t, err)
	require.Nil(t, template)

	st, ok := status.FromError(err)
	require.True(t, ok, "expected gRPC status error, got %T", err)
	require.Equal(t, codes.Internal, st.Code())
	require.Contains(t, st.Message(), "does not match pk_script")
}

// TestDeriveIdentityPubkeyPreWalletInit verifies that GetInfo's call to
// deriveIdentityPubkey returns a structured error rather than panicking
// when the self-managed wallet Option is still None. GetInfo is
// intentionally callable before InitWallet / UnlockWallet so the
// client can probe WalletReady; the previous implementation unwrapped
// the Option unconditionally on the lw/btcwallet branches, which
// panicked on pre-init callers.
func TestDeriveIdentityPubkeyPreWalletInit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		walletType string
		wantErrMsg string
	}{
		{
			name:       "lwwallet not initialized",
			walletType: WalletTypeLwwallet,
			wantErrMsg: "lwwallet not initialized",
		},
		{
			name:       "btcwallet not initialized",
			walletType: WalletTypeBtcwallet,
			wantErrMsg: "btcwallet not initialized",
		},
		{
			name:       "lnd not connected",
			walletType: WalletTypeLnd,
			wantErrMsg: "lnd wallet not connected",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := &RPCServer{
				server: &Server{
					cfg: &Config{
						Wallet: &WalletConfig{
							Type: tc.walletType,
						},
					},
				},
			}

			// Must not panic: the None Option has to surface
			// as a structured error.
			identity, err := r.deriveIdentityPubkey(
				context.Background(),
			)
			require.Error(t, err)
			require.Empty(t, identity)
			require.Contains(t, err.Error(), tc.wantErrMsg)
		})
	}
}
