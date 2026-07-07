package darepod

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type customOORRPCFixture struct {
	rpcServer  *RPCServer
	customIn   *daemonrpc.CustomOORInput
	recipient  *daemonrpc.Output
	claimPath  *arkscript.SpendPath
	clientPriv *btcec.PrivateKey
	outpoint   wire.OutPoint
}

func newCustomOORRPCFixture(t *testing.T) *customOORRPCFixture {
	t.Helper()

	policy, preimage, _, receiverPriv, serverPriv :=
		testVHTLCPolicyFixture(t)
	policyTemplate, err := policy.Template.Encode()
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	claimPath, err := policy.ClaimPath(preimage)
	require.NoError(t, err)

	spendPath, err := claimPath.Encode()
	require.NoError(t, err)

	ready := make(chan struct{})
	close(ready)

	serverPubkey := serverPriv.PubKey().SerializeCompressed()
	svc := &fakeArkService{
		getInfoResponse: &arkrpc.GetInfoResponse{
			Pubkey:        serverPubkey,
			VtxoExitDelay: 144,
			DustLimit:     1000,
		},
	}

	server := &Server{
		cfg: &Config{
			Wallet: &WalletConfig{
				Type: WalletTypeLwwallet,
			},
		},
		walletReady: ready,
		vtxoStore:   &db.VTXOPersistenceStore{},
		clientKeyDesc: keychain.KeyDescriptor{
			PubKey: receiverPriv.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: 1,
				Index:  2,
			},
		},
		chainParams: &chaincfg.RegressionNetParams,
		arkClient: arkrpc.NewArkServiceClient(
			newBufconnClient(t, svc),
		),
	}

	outpoint := testWalletOpsOutpoint(6)

	return &customOORRPCFixture{
		rpcServer: &RPCServer{
			server:           server,
			customInputLocks: make(map[wire.OutPoint]struct{}),
		},
		customIn: &daemonrpc.CustomOORInput{
			Outpoint:           outpoint.String(),
			VtxoPolicyTemplate: policyTemplate,
			SpendPath:          spendPath,
			AmountSat:          42_000,
			PkScript:           pkScript,
		},
		recipient: &daemonrpc.Output{
			AmountSat: 42_000,
			Destination: &daemonrpc.Output_Pubkey{
				Pubkey: schnorr.SerializePubKey(
					receiverPriv.PubKey(),
				),
			},
		},
		claimPath:  claimPath,
		clientPriv: receiverPriv,
		outpoint:   outpoint,
	}
}

type heightOnlyChainBackend struct {
	chainsource.ChainBackend

	height int32
}

func (b *heightOnlyChainBackend) BestBlock(ctx context.Context) (int32,
	chainhash.Hash, error) {

	return b.height, chainhash.Hash{}, nil
}

func (b *heightOnlyChainBackend) EstimateFee(ctx context.Context,
	targetConf uint32) (btcutil.Amount, error) {

	return 0, nil
}

// TestRequireCustomSpendsMature verifies the custom-input maturity preflight
// admits height-locked spends at or past their absolute locktime, rejects
// height-locked spends before that point with a retryable precondition error,
// and fails closed for timestamp locktimes because this RPC path only compares
// against the current block height.
func TestRequireCustomSpendsMature(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		height       int32
		lockTime     uint32
		wantCode     codes.Code
		wantContains string
	}{{
		name:     "mature height locktime",
		height:   144,
		lockTime: 144,
		wantCode: codes.OK,
	}, {
		name:         "immature height locktime",
		height:       143,
		lockTime:     144,
		wantCode:     codes.FailedPrecondition,
		wantContains: "not mature",
	}, {
		name:   "timestamp locktime unsupported",
		height: 144,
		lockTime: uint32(
			txscript.LockTimeThreshold,
		),
		wantCode:     codes.InvalidArgument,
		wantContains: "timestamp locktime",
	}}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			rpcServer := &RPCServer{
				server: &Server{
					chainBackend: &heightOnlyChainBackend{
						height: test.height,
					},
				},
			}
			inputs := []oor.TransferInput{{
				CustomSpend: &arkscript.SpendPath{
					RequiredLockTime: test.lockTime,
				},
			}}

			err := rpcServer.requireCustomSpendsMature(
				t.Context(), inputs,
			)

			if test.wantCode == codes.OK {
				require.NoError(t, err)

				return
			}

			require.Equal(t, test.wantCode, status.Code(err))
			require.Contains(
				t, status.Convert(err).Message(),
				test.wantContains,
			)
		})
	}
}

// TestPrepareOORRejectsRecipientBelowFloor verifies prepared custom-input
// OOR packages use the same recipient VTXO-floor guard as submitted OOR sends.
// The swap cooperative-refund flow asks PrepareOOR to build a deterministic
// package before any operator authorization happens, so a sub-floor recipient
// amount must be rejected at this boundary instead of producing checkpoint
// material that would later create an unusable receiver VTXO.
func TestPrepareOORRejectsRecipientBelowFloor(t *testing.T) {
	t.Parallel()

	fixture := newCustomOORRPCFixture(t)
	fixture.recipient.AmountSat = 999

	_, err := fixture.rpcServer.PrepareOOR(
		t.Context(), &daemonrpc.PrepareOORRequest{
			Recipient: fixture.recipient,
			CustomInputs: []*daemonrpc.CustomOORInput{
				fixture.customIn,
			},
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(
		t, err, "amount 999 below operator min_vtxo_amount_sat 1000",
	)
}

// TestPrepareOORCustomInputMapsPreparedInput exercises the daemon-level
// PrepareOOR RPC for an externally described custom input rather than the SDK
// mock seam. A cooperative refund first asks the daemon to build the exact OOR
// checkpoint that the server will authorize, so the response must map the
// selected custom input back to its outpoint, return the matching checkpoint
// PSBT bytes, expose the custom witness script, and preserve the signing key
// list required for final witness assembly.
func TestPrepareOORCustomInputMapsPreparedInput(t *testing.T) {
	t.Parallel()

	fixture := newCustomOORRPCFixture(t)

	resp, err := fixture.rpcServer.PrepareOOR(
		t.Context(), &daemonrpc.PrepareOORRequest{
			Recipient: fixture.recipient,
			CustomInputs: []*daemonrpc.CustomOORInput{
				fixture.customIn,
			},
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, resp.GetSessionId())
	require.NotEmpty(t, resp.GetArkPsbt())
	require.Len(t, resp.GetCheckpointPsbts(), 1)
	require.Len(t, resp.GetCustomInputs(), 1)

	arkPacket, err := psbtutil.Parse(resp.GetArkPsbt())
	require.NoError(t, err)
	require.Len(t, arkPacket.UnsignedTx.TxIn, 1)
	require.Len(t, arkPacket.UnsignedTx.TxOut, 2)

	prepared := resp.GetCustomInputs()[0]
	require.Equal(t, fixture.outpoint.String(), prepared.GetOutpoint())
	require.Equal(
		t, resp.GetCheckpointPsbts()[0], prepared.GetCheckpointPsbt(),
	)
	require.Equal(
		t, fixture.claimPath.SpendInfo.WitnessScript,
		prepared.GetWitnessScript(),
	)
	wantKey := schnorr.SerializePubKey(fixture.clientPriv.PubKey())
	gotKeys := xOnlyPubkeys(t, prepared.GetSigningPubkeys())
	require.Contains(t, gotKeys, wantKey)
}

// TestSignOORCustomInputMapsSignatureAndErrorCodes exercises the daemon-level
// SignOORCustomInput RPC using a real checkpoint PSBT produced by PrepareOOR.
// The happy path verifies the RPC response preserves the signer pubkey, witness
// script, signature bytes, and sighash fields. The negative subcases pin the
// wire contract for malformed caller data: invalid PSBT bytes are caller
// InvalidArgument, while a wallet signing failure is Internal because the
// request passed structural validation and failed inside the daemon signer.
func TestSignOORCustomInputMapsSignatureAndErrorCodes(t *testing.T) {
	t.Parallel()

	fixture := newCustomOORRPCFixture(t)
	prepared, err := fixture.rpcServer.PrepareOOR(
		t.Context(), &daemonrpc.PrepareOORRequest{
			Recipient: fixture.recipient,
			CustomInputs: []*daemonrpc.CustomOORInput{
				fixture.customIn,
			},
		},
	)
	require.NoError(t, err)

	checkpoint := prepared.GetCustomInputs()[0].GetCheckpointPsbt()
	wantScript := fixture.claimPath.SpendInfo.WitnessScript

	sig, err := schnorr.Sign(
		fixture.clientPriv,
		bytes.Repeat(
			[]byte{0x01}, 32,
		),
	)
	require.NoError(t, err)

	signer := &input.MockInputSigner{}
	signer.On(
		"SignOutputRaw", mock.Anything,
		mock.MatchedBy(func(desc *input.SignDescriptor) bool {
			signMethod := input.TaprootScriptSpendSignMethod
			if desc.SignMethod != signMethod {
				return false
			}

			return bytes.Equal(desc.WitnessScript, wantScript)
		}),
	).Return(sig, nil).Once()
	fixture.rpcServer.oorSignerOverride = signer

	resp, err := fixture.rpcServer.SignOORCustomInput(
		t.Context(), &daemonrpc.SignOORCustomInputRequest{
			CustomInput:    fixture.customIn,
			CheckpointPsbt: checkpoint,
		},
	)
	require.NoError(t, err)
	signer.AssertExpectations(t)

	require.Equal(
		t, fixture.clientPriv.PubKey().SerializeCompressed(),
		resp.GetSignature().GetPubkey(),
	)
	require.Equal(
		t, wantScript, resp.GetSignature().GetWitnessScript(),
	)
	require.Equal(t, sig.Serialize(), resp.GetSignature().GetSignature())
	require.EqualValues(
		t, txscript.SigHashDefault, resp.GetSignature().GetSighash(),
	)

	_, err = fixture.rpcServer.SignOORCustomInput(
		t.Context(), &daemonrpc.SignOORCustomInputRequest{
			CustomInput:    fixture.customIn,
			CheckpointPsbt: []byte("not a psbt"),
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	signErr := &input.MockInputSigner{}
	signErr.On(
		"SignOutputRaw", mock.Anything, mock.Anything,
	).Return(nil, assertErr("signer unavailable")).Once()
	fixture.rpcServer.oorSignerOverride = signErr

	_, err = fixture.rpcServer.SignOORCustomInput(
		t.Context(), &daemonrpc.SignOORCustomInputRequest{
			CustomInput:    fixture.customIn,
			CheckpointPsbt: checkpoint,
		},
	)
	require.Equal(t, codes.Internal, status.Code(err))
	signErr.AssertExpectations(t)
}

type assertErr string

func (e assertErr) Error() string {
	return string(e)
}

func xOnlyPubkeys(t *testing.T, pubkeys [][]byte) [][]byte {
	t.Helper()

	xOnly := make([][]byte, 0, len(pubkeys))
	for _, pubkey := range pubkeys {
		parsed, err := btcec.ParsePubKey(pubkey)
		require.NoError(t, err)

		xOnly = append(xOnly, schnorr.SerializePubKey(parsed))
	}

	return xOnly
}
