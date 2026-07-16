package waved

import (
	"context"
	"errors"
	"fmt"
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
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

// TestVTXOAdmissionCode verifies wallet admission errors surface as
// caller-actionable gRPC codes instead of being collapsed into Internal.
func TestVTXOAdmissionCode(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, codes.Canceled,
		vtxoAdmissionCode(
			fmt.Errorf("select and reserve: %w", context.Canceled),
		),
	)
	require.Equal(
		t, codes.DeadlineExceeded,
		vtxoAdmissionCode(
			fmt.Errorf("select and reserve: %w",
				context.DeadlineExceeded),
		),
	)
	require.Equal(
		t, codes.Aborted,
		vtxoAdmissionCode(
			fmt.Errorf("select and reserve: %w",
				vtxo.ErrVTXOLiquidityLocked),
		),
	)
	require.Equal(
		t, codes.ResourceExhausted,
		vtxoAdmissionCode(
			fmt.Errorf("select and reserve: %w",
				vtxo.ErrInsufficientSpendableFunds),
		),
	)
	require.Equal(
		t, codes.Internal,
		vtxoAdmissionCode(
			errors.New("actor system unavailable"),
		),
	)
}

// TestUnrollAdmissionSurvivesCallerCancellation verifies that a caller
// disconnect does not cancel the daemon-local manual unroll admission path.
func TestUnrollAdmissionSurvivesCallerCancellation(t *testing.T) {
	t.Parallel()

	walletReady := make(chan struct{})
	close(walletReady)

	receivedCtxErr := make(chan error, 1)
	managerBehavior := actor.NewFunctionBehavior(
		func(ctx context.Context,
			msg vtxo.ManagerMsg) fn.Result[vtxo.ManagerResp] {

			_, ok := msg.(*actormsg.ForceUnrollRequest)
			if !ok {
				return fn.Err[vtxo.ManagerResp](
					errors.New("unexpected message"),
				)
			}

			receivedCtxErr <- ctx.Err()

			return fn.Ok[vtxo.ManagerResp](
				&actormsg.ForceUnrollResponse{
					Accepted: true,
				},
			)
		},
	)

	manager := actor.NewActor(actor.ActorConfig[
		vtxo.ManagerMsg, vtxo.ManagerResp,
	]{
		ID:          "unroll-admission-test-manager",
		Behavior:    managerBehavior,
		MailboxSize: 1,
	})
	manager.Start()
	t.Cleanup(manager.Stop)

	server := &Server{
		walletReady: walletReady,
		vtxoMgrRef:  fn.Some(manager.Ref()),
	}
	r := &RPCServer{server: server}

	var hash chainhash.Hash
	hash[0] = 1
	outpoint := wire.OutPoint{Hash: hash, Index: 0}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := r.Unroll(ctx, &waverpc.UnrollRequest{
		Outpoint: outpoint.String(),
	})
	require.NoError(t, err)
	require.True(t, resp.Created)

	require.NoError(t, <-receivedCtxErr)
}

func TestSumOORInputAmounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		inputs  []oor.TransferInput
		want    btcutil.Amount
		wantErr string
	}{
		{
			name: "sums valid inputs",
			inputs: []oor.TransferInput{
				{
					VTXO: &vtxo.Descriptor{
						Amount: 1_000,
					},
				},
				{
					VTXO: &vtxo.Descriptor{
						Amount: 2_500,
					},
				},
			},
			want: 3_500,
		},
		{
			name: "missing descriptor",
			inputs: []oor.TransferInput{
				{},
			},
			wantErr: "input 0 missing VTXO",
		},
		{
			name: "non-positive amount",
			inputs: []oor.TransferInput{
				{
					VTXO: &vtxo.Descriptor{},
				},
			},
			wantErr: "input 0 amount must be positive",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := sumOORInputAmounts(tc.inputs)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestAppendOORChangeRecipient(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	baseRecipient := oortx.RecipientOutput{
		PkScript: []byte{
			0x51,
			0x20,
			0x01,
		},
		Value: 1_000,
	}

	t.Run("exact input needs no change", func(t *testing.T) {
		t.Parallel()

		called := false
		recipients, change, err := appendOORChangeRecipient(
			ctx, []oortx.RecipientOutput{baseRecipient},
			1_000, 546,
			func(context.Context, btcutil.Amount) (
				oortx.RecipientOutput, error) {

				called = true

				return oortx.RecipientOutput{}, nil
			},
		)
		require.NoError(t, err)
		require.False(t, called)
		require.Zero(t, change)
		require.Len(t, recipients, 1)
		require.Equal(t, baseRecipient, recipients[0])
	})

	t.Run("overselection appends change", func(t *testing.T) {
		t.Parallel()

		recipients, change, err := appendOORChangeRecipient(
			ctx, []oortx.RecipientOutput{baseRecipient},
			2_500, 546,
			func(_ context.Context, got btcutil.Amount) (
				oortx.RecipientOutput, error) {

				require.Equal(t, btcutil.Amount(1_500), got)

				return oortx.RecipientOutput{
					PkScript: []byte{
						0x51,
						0x20,
						0x02,
					},
				}, nil
			},
		)
		require.NoError(t, err)
		require.Equal(t, btcutil.Amount(1_500), change)
		require.Len(t, recipients, 2)
		require.Equal(t, btcutil.Amount(1_500), recipients[1].Value)
		require.Equal(
			t, []byte{0x51, 0x20, 0x02}, recipients[1].PkScript,
		)
	})

	t.Run("below-floor change rejected", func(t *testing.T) {
		t.Parallel()

		_, change, err := appendOORChangeRecipient(
			ctx, []oortx.RecipientOutput{baseRecipient}, 1_545, 546,
			nil,
		)
		require.Error(t, err)
		require.Equal(t, btcutil.Amount(545), change)

		st, ok := status.FromError(err)
		require.True(t, ok)
		require.Equal(t, codes.InvalidArgument, st.Code())
		require.Contains(t, st.Message(), "below VTXO minimum")
	})

	t.Run("floor change accepted", func(t *testing.T) {
		t.Parallel()

		recipients, change, err := appendOORChangeRecipient(
			ctx, []oortx.RecipientOutput{baseRecipient},
			1_546, 546,
			func(context.Context, btcutil.Amount) (
				oortx.RecipientOutput, error) {

				return oortx.RecipientOutput{
					PkScript: []byte{
						0x51,
						0x20,
						0x02,
					},
				}, nil
			},
		)
		require.NoError(t, err)
		require.Equal(t, btcutil.Amount(546), change)
		require.Len(t, recipients, 2)
		require.Equal(t, btcutil.Amount(546), recipients[1].Value)
	})

	t.Run("insufficient inputs rejected", func(t *testing.T) {
		t.Parallel()

		_, _, err := appendOORChangeRecipient(
			ctx, []oortx.RecipientOutput{baseRecipient}, 999, 546,
			nil,
		)
		require.Error(t, err)

		st, ok := status.FromError(err)
		require.True(t, ok)
		require.Equal(t, codes.InvalidArgument, st.Code())
		require.Contains(t, st.Message(), "below recipient amount")
	})

	t.Run("builder amount mismatch rejected", func(t *testing.T) {
		t.Parallel()

		_, change, err := appendOORChangeRecipient(
			ctx, []oortx.RecipientOutput{baseRecipient},
			2_000, 546,
			func(context.Context, btcutil.Amount) (
				oortx.RecipientOutput, error) {

				return oortx.RecipientOutput{
					PkScript: []byte{
						0x51,
						0x20,
						0x03,
					},
					Value: 999,
				}, nil
			},
		)
		require.Error(t, err)
		require.Equal(t, btcutil.Amount(1_000), change)

		st, ok := status.FromError(err)
		require.True(t, ok)
		require.Equal(t, codes.Internal, st.Code())
		require.Contains(t, st.Message(), "builder returned")
	})

	t.Run("empty recipients rejected", func(t *testing.T) {
		t.Parallel()

		_, _, err := appendOORChangeRecipient(ctx, nil, 1_000, 546, nil)
		require.Error(t, err)

		st, ok := status.FromError(err)
		require.True(t, ok)
		require.Equal(t, codes.InvalidArgument, st.Code())
		require.Contains(t, st.Message(), "recipient must be provided")
	})

	t.Run("zero recipient amount rejected", func(t *testing.T) {
		t.Parallel()

		_, _, err := appendOORChangeRecipient(
			ctx, []oortx.RecipientOutput{{
				PkScript: []byte{0x51, 0x20, 0x01},
			}}, 1_000, 546, nil,
		)
		require.Error(t, err)

		st, ok := status.FromError(err)
		require.True(t, ok)
		require.Equal(t, codes.InvalidArgument, st.Code())
		require.Contains(t, st.Message(), "amount must be positive")
	})
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

	out := &waverpc.Output{
		Destination: &waverpc.Output_Pubkey{
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

	addr, err := btcaddr.NewAddressTaproot(
		xOnly, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	out := &waverpc.Output{
		Destination: &waverpc.Output_Address{
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

	out := &waverpc.Output{
		Destination: &waverpc.Output_PolicyTemplate{
			PolicyTemplate: policyTemplate,
		},
		AmountSat: 50_000,
	}

	pkScript, clientKey, err := r.resolveRecipientOutput(out)
	require.NoError(t, err)
	require.NotEmpty(t, pkScript)
	require.Equal(
		t,
		schnorr.SerializePubKey(
			ownerPriv.PubKey(),
		),
		schnorr.SerializePubKey(clientKey),
	)
}

// TestResolveRecipientOutputPolicyTemplateCustomRejected verifies that
// directed sends reject non-standard policy templates that do not expose
// the collaborative owner key required for VTXO creation.
func TestResolveRecipientOutputPolicyTemplateCustomRejected(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	out := &waverpc.Output{
		Destination: &waverpc.Output_PolicyTemplate{
			PolicyTemplate: []byte{
				0x01,
			},
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

	out := &waverpc.Output{
		Destination: &waverpc.Output_Address{
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

	out := &waverpc.Output{
		Destination: &waverpc.Output_Pubkey{
			Pubkey: []byte{
				0x01,
				0x02,
				0x03,
			},
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

// TestWalletStateToProtoIncludesSyncing verifies the public GetInfo
// state can distinguish an unlocked wallet that is still catching up
// from a wallet that still needs its password.
func TestWalletStateToProtoIncludesSyncing(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, waverpc.WalletState_WALLET_STATE_SYNCING,
		walletStateToProto(WalletStateSyncing),
	)
}

// TestRequireWalletReadyErrorsExplainLifecycleState verifies callers
// get actionable setup guidance for each non-ready wallet state.
func TestRequireWalletReadyErrorsExplainLifecycleState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		state      WalletState
		closeReady bool
		wantMsg    string
		wantErr    bool
	}{
		{
			name:    "none",
			state:   WalletStateNone,
			wantMsg: "wallet is not ready (create first)",
			wantErr: true,
		},
		{
			name:    "locked",
			state:   WalletStateLocked,
			wantMsg: "wallet is not ready (unlock first)",
			wantErr: true,
		},
		{
			name:    "unlocking",
			state:   WalletStateUnlocking,
			wantMsg: "wallet unlock is in progress",
			wantErr: true,
		},
		{
			name:  "syncing",
			state: WalletStateSyncing,
			wantMsg: "wallet is syncing; try again once sync " +
				"completes",
			wantErr: true,
		},
		{
			name:       "ready channel closed",
			state:      WalletStateReady,
			closeReady: true,
			wantErr:    false,
		},
		{
			name:    "ready state before channel close",
			state:   WalletStateReady,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			walletReady := make(chan struct{})
			if tc.closeReady {
				close(walletReady)
			}

			r := &RPCServer{
				server: &Server{
					walletReady: walletReady,
				},
			}
			r.server.walletState.Store(int32(tc.state))

			err := r.requireWalletReady()
			if !tc.wantErr {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			st, ok := status.FromError(err)
			require.True(t, ok)
			require.Equal(t, codes.FailedPrecondition, st.Code())
			require.Equal(t, tc.wantMsg, st.Message())
		})
	}
}

// TestSumOnchainWalletConfirmed locks in the invariant that the on-chain
// wallet balance accumulates across every registered backend fetcher and
// that a failing fetcher does not erase the contribution of its
// siblings. A regression to a simple `=` assignment would overwrite the
// running total and trip this test.
func TestSumOnchainWalletConfirmed(t *testing.T) {
	t.Parallel()

	makeFetcher := func(amount btcutil.Amount,
		err error) onchainWalletConfirmedFetcher {

		return func(context.Context) (btcutil.Amount, error) {
			return amount, err
		}
	}

	boom := errors.New("boom")
	tests := []struct {
		name     string
		fetchers []onchainWalletConfirmedFetcher
		want     btcutil.Amount
		wantErr  error
	}{
		{
			name:     "no fetchers returns zero",
			fetchers: nil,
			want:     0,
		},
		{
			name: "single backend returns its balance",
			fetchers: []onchainWalletConfirmedFetcher{
				makeFetcher(100_000, nil),
			},
			want: 100_000,
		},
		{
			name: "multiple backends accumulate",
			fetchers: []onchainWalletConfirmedFetcher{
				makeFetcher(100_000, nil),
				makeFetcher(250_000, nil),
				makeFetcher(42, nil),
			},
			want: 350_042,
		},
		{
			name: "first fetcher error short-circuits",
			fetchers: []onchainWalletConfirmedFetcher{
				makeFetcher(100_000, nil),
				makeFetcher(0, boom),
				makeFetcher(50_000, nil),
			},
			want:    0,
			wantErr: boom,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			total, err := sumOnchainWalletConfirmed(
				context.Background(), tc.fetchers,
			)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.Zero(t, total)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, total)
		})
	}
}

// TestGetInfoIncludesServerInfo verifies that GetInfo surfaces cached
// operator terms once the daemon has connected to the remote Ark server
// and learned its current policy.
func TestGetInfoIncludesServerInfo(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	server := &Server{
		cfg: &Config{
			Network: "regtest",
			Wallet: &WalletConfig{
				Type: WalletTypeBtcwallet,
			},
		},
		log: btclog.Disabled,
	}
	server.setServerConnected(true)
	server.storeOperatorTerms(&types.OperatorTerms{
		PubKey:                  operatorPriv.PubKey(),
		BoardingExitDelay:       144,
		VTXOExitDelay:           288,
		DustLimit:               btcutil.Amount(546),
		MinVTXOAmount:           btcutil.Amount(1234),
		MinBoardingAmount:       btcutil.Amount(10_000),
		MaxVTXOAmount:           btcutil.Amount(500_000),
		FeeRate:                 btcutil.Amount(12),
		MinOperatorFee:          btcutil.Amount(34),
		FreeRefreshWindowBlocks: 72,
		MinConfirmations:        2,
	})
	r := &RPCServer{server: server}

	resp, err := r.GetInfo(
		context.Background(), &waverpc.GetInfoRequest{},
	)
	require.NoError(t, err)
	require.True(t, resp.ServerConnected)
	require.NotNil(t, resp.ServerInfo)
	require.Equal(
		t, operatorPriv.PubKey().SerializeCompressed(),
		resp.ServerInfo.OperatorPubkey,
	)
	require.Equal(t, uint32(144), resp.ServerInfo.BoardingExitDelay)
	require.Equal(t, uint32(288), resp.ServerInfo.VtxoExitDelay)
	require.Equal(t, uint64(546), resp.ServerInfo.DustLimit)
	require.Equal(t, uint64(1234), resp.ServerInfo.MinVtxoAmountSat)
	require.Equal(t, uint64(10_000),
		resp.ServerInfo.MinBoardingAmount,
	)
	require.Equal(t, uint64(500_000),
		resp.ServerInfo.MaxVtxoAmount,
	)
	require.Equal(t, uint64(12), resp.ServerInfo.FeeRate)
	require.Equal(t, uint64(34), resp.ServerInfo.MinOperatorFee)
	require.Equal(
		t, uint32(72), resp.ServerInfo.FreeRefreshWindowBlocks,
	)
	require.Equal(t, uint32(2), resp.ServerInfo.MinConfirmations)
}

// TestGetInfoFloorsMinVTXOAmountAtDust verifies GetInfo never exposes a
// VTXO minimum below the cached dust limit.
func TestGetInfoFloorsMinVTXOAmountAtDust(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	server := &Server{
		cfg: &Config{
			Network: "regtest",
			Wallet: &WalletConfig{
				Type: WalletTypeBtcwallet,
			},
		},
		log: btclog.Disabled,
	}
	server.setServerConnected(true)
	server.storeOperatorTerms(&types.OperatorTerms{
		PubKey:        operatorPriv.PubKey(),
		DustLimit:     btcutil.Amount(546),
		MinVTXOAmount: btcutil.Amount(100),
	})
	r := &RPCServer{server: server}

	resp, err := r.GetInfo(
		context.Background(), &waverpc.GetInfoRequest{},
	)
	require.NoError(t, err)
	require.NotNil(t, resp.ServerInfo)
	require.Equal(t, uint64(546), resp.ServerInfo.MinVtxoAmountSat)
}

// TestOperatorPubKeyFetchesFreshTerms verifies callers that construct new
// policy scripts can bypass GetInfo's cached server-info snapshot and refresh
// it after a successful direct operator fetch.
func TestOperatorPubKeyFetchesFreshTerms(t *testing.T) {
	t.Parallel()

	stalePriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	freshPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	server := &Server{
		cfg: &Config{
			Network: "regtest",
			Wallet: &WalletConfig{
				Type: WalletTypeBtcwallet,
			},
		},
		log: btclog.Disabled,
		serverConn: newBufconnClient(t, &fakeArkService{
			getInfoResponse: &arkrpc.GetInfoResponse{
				Pubkey: freshPriv.
					PubKey().
					SerializeCompressed(),
				BoardingExitDelay:   144,
				VtxoExitDelay:       288,
				DustLimit:           546,
				MinVtxoAmountSat:    1234,
				MinBoardingAmount:   10_000,
				MaxVtxoAmount:       500_000,
				FeeRate:             12,
				MinOperatorFee:      34,
				MinConfirmations:    2,
				MaxOorLineageVbytes: 99,
			},
		}),
	}
	server.storeOperatorTerms(&types.OperatorTerms{
		PubKey: stalePriv.PubKey(),
	})
	r := &RPCServer{server: server}

	key, err := r.OperatorPubKey(context.Background())
	require.NoError(t, err)
	require.True(t, key.IsEqual(freshPriv.PubKey()))

	info, err := r.GetInfo(
		context.Background(), &waverpc.GetInfoRequest{},
	)
	require.NoError(t, err)
	require.Equal(
		t, freshPriv.PubKey().SerializeCompressed(),
		info.GetServerInfo().GetOperatorPubkey(),
	)
	require.Equal(
		t, uint64(1234), info.GetServerInfo().GetMinVtxoAmountSat(),
	)
	require.Equal(
		t, btcutil.Amount(1234),
		server.loadOperatorTerms().MinVTXOAmount,
	)
	require.Equal(
		t, uint32(99), server.loadOperatorTerms().MaxOORLineageVBytes,
	)
}

// TestOperatorPubKeyFloorsFetchedMinVTXOAmountAtDust verifies direct
// operator refreshes normalize below-dust VTXO floors before caching them.
func TestOperatorPubKeyFloorsFetchedMinVTXOAmountAtDust(t *testing.T) {
	t.Parallel()

	freshPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	server := &Server{
		cfg: &Config{
			Network: "regtest",
			Wallet: &WalletConfig{
				Type: WalletTypeBtcwallet,
			},
		},
		log: btclog.Disabled,
		serverConn: newBufconnClient(t, &fakeArkService{
			getInfoResponse: &arkrpc.GetInfoResponse{
				Pubkey: freshPriv.
					PubKey().
					SerializeCompressed(),
				BoardingExitDelay: 144,
				VtxoExitDelay:     288,
				DustLimit:         546,
				MinVtxoAmountSat:  100,
			},
		}),
	}
	r := &RPCServer{server: server}

	key, err := r.OperatorPubKey(context.Background())
	require.NoError(t, err)
	require.True(t, key.IsEqual(freshPriv.PubKey()))

	require.Equal(
		t, btcutil.Amount(546),
		server.loadOperatorTerms().MinVTXOAmount,
	)

	info, err := r.GetInfo(
		context.Background(), &waverpc.GetInfoRequest{},
	)
	require.NoError(t, err)
	require.Equal(
		t, uint64(546), info.GetServerInfo().GetMinVtxoAmountSat(),
	)
}

// TestGetInfoConcurrentOperatorTermsAccess verifies that GetInfo can read the
// cached operator terms safely while another goroutine swaps in new snapshots.
func TestGetInfoConcurrentOperatorTermsAccess(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	server := &Server{
		cfg: &Config{
			Network: "regtest",
			Wallet: &WalletConfig{
				Type: WalletTypeBtcwallet,
			},
		},
		log: btclog.Disabled,
	}
	r := &RPCServer{server: server}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)

		for i := uint32(0); i < 256; i++ {
			server.storeOperatorTerms(&types.OperatorTerms{
				PubKey:            operatorPriv.PubKey(),
				BoardingExitDelay: 100 + i,
				VTXOExitDelay:     200 + i,
				DustLimit:         btcutil.Amount(546),
				MinBoardingAmount: btcutil.Amount(10_000),
				MaxVTXOAmount:     btcutil.Amount(500_000),
				FeeRate:           btcutil.Amount(12),
				MinOperatorFee:    btcutil.Amount(34),
				MinConfirmations:  2,
			})

			select {
			case <-ctx.Done():
				return

			default:
			}
		}
	}()

	for i := 0; i < 256; i++ {
		resp, err := r.GetInfo(
			context.Background(), &waverpc.GetInfoRequest{},
		)
		require.NoError(t, err)

		if resp.ServerInfo != nil {
			require.Equal(
				t, operatorPriv.PubKey().SerializeCompressed(),
				resp.ServerInfo.OperatorPubkey,
			)
		}
	}

	cancel()
	<-writerDone
}

// newRoundActorWithStates registers a stub round actor under the
// standard round service key that responds to GetClientStateRequest
// with the supplied map of FSM states. The caller must shut down the
// returned actor system.
func newRoundActorWithStates(t *testing.T,
	states map[string]round.FSMStateInfo) *actor.ActorSystem {

	t.Helper()

	system := actor.NewActorSystem()

	behavior := actor.NewFunctionBehavior(
		func(_ context.Context, msg actormsg.RoundReceivable,
		) fn.Result[actormsg.RoundActorResp] {

			if _, ok := msg.(*round.GetClientStateRequest); !ok {
				t.Fatalf("unexpected message: %T", msg)
			}

			return fn.Ok[actormsg.RoundActorResp](
				&round.GetClientStateResponse{
					States: states,
				},
			)
		},
	)
	_ = actor.RegisterWithSystem(
		system, "round-stub", round.NewServiceKey(), behavior,
	)

	return system
}

// localOwnerKey returns a non-nil owner key descriptor — what
// types.VTXORequest.HasLocalOwner uses as the locally-owned
// sentinel. The actual pubkey value does not matter for these
// projection-only tests; only its non-nilness does.
func localOwnerKey(t *testing.T) keychain.KeyDescriptor {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return keychain.KeyDescriptor{
		PubKey: priv.PubKey(),
	}
}

// TestQueryRoundStatesPopulatesUpcomingVTXOs verifies that a live
// (actor-served) round in the InputSigSentState surfaces both its
// commitment txid and the amounts of every VTXO the wallet is about
// to receive. This pins issue #500: prior to the fix, the live path
// returned a mostly-empty RoundInfo and the commitment txid only
// appeared in the daemon log.
func TestQueryRoundStatesPopulatesUpcomingVTXOs(t *testing.T) {
	t.Parallel()

	const (
		amountA = btcutil.Amount(123_456)
		amountB = btcutil.Amount(78_910)
		// notOurs sits in the same intent list with a nil OwnerKey
		// and must NOT appear in the response; the round may carry
		// outputs destined for other clients and surfacing them
		// here would inflate the wallet's view of upcoming
		// balance.
		notOurs = btcutil.Amount(999_000)
	)

	// Build a minimal commitment PSBT so liveRoundDetails can recover
	// its txid the same way the production code does.
	commitTx := wire.NewMsgTx(2)
	commitTx.AddTxIn(&wire.TxIn{})
	commitTx.AddTxOut(&wire.TxOut{Value: 250_000})
	packet, err := psbt.NewFromUnsignedTx(commitTx)
	require.NoError(t, err)
	expectedTxid := commitTx.TxHash().String()

	roundID := round.RoundID(uuid.New())
	state := &round.InputSigSentState{
		RoundID:      roundID,
		CommitmentTx: packet,
		Intents: round.Intents{
			VTXOs: []types.VTXORequest{
				{
					Amount:   amountA,
					OwnerKey: localOwnerKey(t),
				},
				{
					Amount:   amountB,
					OwnerKey: localOwnerKey(t),
				},
				{
					// notOurs: no OwnerKey set →
					// HasLocalOwner returns false →
					// filtered out.
					Amount: notOurs,
				},
			},
		},
	}

	system := newRoundActorWithStates(t,
		map[string]round.FSMStateInfo{
			roundID.String(): {
				State:   state,
				RoundID: roundID,
			},
		},
	)
	defer func() {
		require.NoError(t, system.Shutdown(t.Context()))
	}()

	srv := &RPCServer{server: &Server{actorSystem: system}}

	rounds, err := srv.queryRoundStates(t.Context())
	require.NoError(t, err)
	require.Len(t, rounds, 1)

	got := rounds[0]
	require.Equal(t, roundID.String(), got.RoundId)
	require.Equal(
		t, waverpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, got.State,
	)
	require.False(t, got.IsTemp)
	require.Equal(t, expectedTxid, got.CommitmentTxid)

	// Only the two locally-owned entries should be surfaced.
	require.Len(t, got.Vtxos, 2)
	require.Equal(t, int64(amountA), got.Vtxos[0].AmountSat)
	require.Equal(t, int64(amountB), got.Vtxos[1].AmountSat)
}

// TestListVTXOsPendingRoundProjectsLiveRounds verifies that
// ListVTXOs with status_filter=VTXO_STATUS_PENDING_ROUND projects
// the upcoming VTXOs from in-flight rounds as synthetic VTXO
// entries. This pins issue #501: prior to the fix there was no way
// to see VTXOs the wallet had signed for between commitment-tx
// signing and confirmation.
func TestListVTXOsPendingRoundProjectsLiveRounds(t *testing.T) {
	t.Parallel()

	const (
		amountA = btcutil.Amount(50_000)
		amountB = btcutil.Amount(25_000)
		// belowMin sits under the min_amount_sat filter and must
		// be elided to confirm the filter still applies on the
		// synthetic path.
		belowMin = btcutil.Amount(1_000)
	)

	commitTx := wire.NewMsgTx(2)
	commitTx.AddTxIn(&wire.TxIn{})
	commitTx.AddTxOut(&wire.TxOut{Value: 100_000})
	packet, err := psbt.NewFromUnsignedTx(commitTx)
	require.NoError(t, err)
	expectedTxid := commitTx.TxHash().String()

	roundID := round.RoundID(uuid.New())
	state := &round.InputSigSentState{
		RoundID:      roundID,
		CommitmentTx: packet,
		Intents: round.Intents{
			VTXOs: []types.VTXORequest{
				{
					Amount:   amountA,
					OwnerKey: localOwnerKey(t),
				},
				{
					Amount:   amountB,
					OwnerKey: localOwnerKey(t),
				},
				{
					Amount:   belowMin,
					OwnerKey: localOwnerKey(t),
				},
			},
		},
	}

	system := newRoundActorWithStates(t,
		map[string]round.FSMStateInfo{
			roundID.String(): {
				State:   state,
				RoundID: roundID,
			},
		},
	)
	defer func() {
		require.NoError(t, system.Shutdown(t.Context()))
	}()

	// Force the wallet-ready gate open without standing up a real
	// wallet — listPendingRoundVTXOs only consults the round
	// actor.
	walletReady := make(chan struct{})
	close(walletReady)
	srv := &RPCServer{server: &Server{
		actorSystem: system,
		walletReady: walletReady,
	}}

	resp, err := srv.ListVTXOs(t.Context(), &waverpc.ListVTXOsRequest{
		StatusFilter: waverpc.VTXOStatus_VTXO_STATUS_PENDING_ROUND,
		MinAmountSat: int64(amountB),
	})
	require.NoError(t, err)
	require.Len(t, resp.Vtxos, 2)

	for _, v := range resp.Vtxos {
		require.Equal(
			t, waverpc.VTXOStatus_VTXO_STATUS_PENDING_ROUND,
			v.Status,
		)
		require.Equal(t, roundID.String(), v.RoundId)
		require.Equal(t, expectedTxid, v.CommitmentTxid)
		require.Empty(t, v.Outpoint)
	}
	require.Equal(t, int64(amountA), resp.Vtxos[0].AmountSat)
	require.Equal(t, int64(amountB), resp.Vtxos[1].AmountSat)
}

// TestListVTXOsPendingRoundSkipsConfirmedRounds pins the guard against
// double-reporting: between a round's commitment-tx confirmation and
// the actor cleaning the round out of its in-memory map, the same
// VTXOs are already in the on-disk store as VTXO_STATUS_LIVE.
// Surfacing them under VTXO_STATUS_PENDING_ROUND too would show the
// upcoming-balance twice for that short window.
func TestListVTXOsPendingRoundSkipsConfirmedRounds(t *testing.T) {
	t.Parallel()

	roundID := round.RoundID(uuid.New())
	confirmed := &round.ConfirmedState{
		TxID: chainhash.Hash{
			0xab,
			0xcd,
		},
		VTXOs: []*round.ClientVTXO{
			{
				Outpoint: wire.OutPoint{
					Hash: chainhash.Hash{
						0x11,
					},
					Index: 0,
				},
				Amount: btcutil.Amount(50_000),
			},
		},
	}

	system := newRoundActorWithStates(t,
		map[string]round.FSMStateInfo{
			roundID.String(): {
				State:   confirmed,
				RoundID: roundID,
			},
		},
	)
	defer func() {
		require.NoError(t, system.Shutdown(t.Context()))
	}()

	walletReady := make(chan struct{})
	close(walletReady)
	srv := &RPCServer{server: &Server{
		actorSystem: system,
		walletReady: walletReady,
	}}

	resp, err := srv.ListVTXOs(
		t.Context(), &waverpc.ListVTXOsRequest{
			StatusFilter: waverpc.
				VTXOStatus_VTXO_STATUS_PENDING_ROUND,
		},
	)
	require.NoError(t, err)
	require.Empty(
		t, resp.Vtxos, "ConfirmedState VTXOs live under "+
			"VTXO_STATUS_LIVE in the store; they must not "+
			"double-report here",
	)
}

// TestQueryRoundStatesEarlyStateOmitsCommitmentTxid verifies that
// rounds before the CommitmentTxReceived transition still surface
// their upcoming VTXO amounts but leave the commitment txid empty:
// the daemon does not know it yet.
func TestQueryRoundStatesEarlyStateOmitsCommitmentTxid(t *testing.T) {
	t.Parallel()

	roundID := round.RoundID(uuid.New())
	state := &round.RoundJoinedState{
		RoundID: roundID,
		Intents: round.Intents{
			VTXOs: []types.VTXORequest{
				{
					Amount:   btcutil.Amount(42_000),
					OwnerKey: localOwnerKey(t),
				},
			},
		},
	}

	system := newRoundActorWithStates(t,
		map[string]round.FSMStateInfo{
			roundID.String(): {
				State:   state,
				RoundID: roundID,
			},
		},
	)
	defer func() {
		require.NoError(t, system.Shutdown(t.Context()))
	}()

	srv := &RPCServer{server: &Server{actorSystem: system}}

	rounds, err := srv.queryRoundStates(t.Context())
	require.NoError(t, err)
	require.Len(t, rounds, 1)

	got := rounds[0]
	require.Empty(t, got.CommitmentTxid)
	require.Len(t, got.Vtxos, 1)
	require.Equal(t, int64(42_000), got.Vtxos[0].AmountSat)
}

// TestUnrollInfeasibleError verifies each infeasibility reason maps to a
// codes.FailedPrecondition gRPC error whose message names the concrete
// figures the caller needs to act on.
func TestUnrollInfeasibleError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		verdict  unroll.ExitFeasibility
		contains []string
	}{
		{
			name: "sweep below dust names value, fee, and floor",
			verdict: unroll.ExitFeasibility{
				Reason:             unroll.ExitSweepBelowDust,
				VTXOAmountSat:      1,
				SweepFeeSat:        200,
				FeeRateSatPerVByte: 1,
				NetRecoveredSat:    -199,
				DustLimitSat:       330,
			},
			contains: []string{
				"not viable", "cooperative leave", "330 sat",
			},
		},
		{
			name: "uneconomical names tx count and total cost",
			verdict: unroll.ExitFeasibility{
				Reason:               unroll.ExitUneconomical,
				NumRecoveryTxs:       50,
				TotalRecoveryCostSat: 30_000,
				VTXOAmountSat:        20_000,
			},
			contains: []string{
				"uneconomical", "50 transaction",
				"cooperative leave",
			},
		},
		{
			name: "underfunded names required and confirmed sats",
			verdict: unroll.ExitFeasibility{
				Reason: unroll.ExitWalletUnderfunded,

				CPFPFeeTotalSat:    6_200,
				NumRecoveryTxs:     4,
				WalletConfirmedSat: 100,
			},
			contains: []string{
				"wallet balance too low", "Call GetExitPlan",
			},
		},
		{
			name: "too few inputs names required and usable count",
			verdict: unroll.ExitFeasibility{
				Reason: unroll.ExitWalletTooFewInputs,

				RequiredWalletInputs: 2,
				WalletUsableInputs:   1,
			},
			contains: []string{
				"insufficient", "one per ancestry path",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := unrollInfeasibleError(tc.verdict)
			require.Equal(
				t, codes.FailedPrecondition, status.Code(err),
			)
			for _, sub := range tc.contains {
				require.Contains(t, err.Error(), sub)
			}
		})
	}
}
