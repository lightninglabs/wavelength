package darepod

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
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

	resp, err := r.Unroll(ctx, &daemonrpc.UnrollRequest{
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

	t.Run("dust change rejected", func(t *testing.T) {
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
		require.Contains(t, st.Message(), "below dust limit")
	})

	t.Run("limit change accepted", func(t *testing.T) {
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

	out := &daemonrpc.Output{
		Destination: &daemonrpc.Output_PolicyTemplate{
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
		t, daemonrpc.WalletState_WALLET_STATE_SYNCING,
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

	sweepPriv, err := btcec.NewPrivateKey()
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
		PubKey:            operatorPriv.PubKey(),
		BoardingExitDelay: 144,
		VTXOExitDelay:     288,
		ForfeitScript:     []byte{0x51, 0x20, 0x01},
		SweepKey:          sweepPriv.PubKey(),
		SweepDelay:        432,
		DustLimit:         btcutil.Amount(546),
		MinBoardingAmount: btcutil.Amount(10_000),
		MaxBoardingAmount: btcutil.Amount(500_000),
		FeeRate:           btcutil.Amount(12),
		MinOperatorFee:    btcutil.Amount(34),
		MinConfirmations:  2,
	})
	r := &RPCServer{server: server}

	resp, err := r.GetInfo(
		context.Background(), &daemonrpc.GetInfoRequest{},
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
	require.Equal(
		t, []byte{0x51, 0x20, 0x01}, resp.ServerInfo.ForfeitScript,
	)
	require.Equal(
		t, sweepPriv.PubKey().SerializeCompressed(),
		resp.ServerInfo.SweepKey,
	)
	require.Equal(t, uint32(432), resp.ServerInfo.SweepDelay)
	require.Equal(t, uint64(546), resp.ServerInfo.DustLimit)
	require.Equal(t, uint64(10_000),
		resp.ServerInfo.MinBoardingAmount,
	)
	require.Equal(t, uint64(500_000),
		resp.ServerInfo.MaxBoardingAmount,
	)
	require.Equal(t, uint64(12), resp.ServerInfo.FeeRate)
	require.Equal(t, uint64(34), resp.ServerInfo.MinOperatorFee)
	require.Equal(t, uint32(2), resp.ServerInfo.MinConfirmations)
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
				ForfeitScript:     []byte{0x51, byte(i)},
				SweepDelay:        300 + i,
				DustLimit:         btcutil.Amount(546),
				MinBoardingAmount: btcutil.Amount(10_000),
				MaxBoardingAmount: btcutil.Amount(500_000),
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
			context.Background(), &daemonrpc.GetInfoRequest{},
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
