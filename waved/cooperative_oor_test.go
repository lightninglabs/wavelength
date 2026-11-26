package waved

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// TestCooperativeOORWalletRoundTrip prepares as the sender, signs with the
// receiver's wallet and sends with both independently verifiable signatures.
// Invalid signing intents release neither signature; invalid authorizations
// never reach the transfer actor.
func TestCooperativeOORWalletRoundTrip(t *testing.T) {
	t.Parallel()

	f := newCustomOORRPCFixture(t)
	r := f.rpcServer
	ctx := t.Context()
	path, err := f.policy.RefundPath()
	require.NoError(t, err)
	f.customIn.SpendPath, err = path.Encode()
	require.NoError(t, err)
	f.customIn.PreserveSourceOwners = true
	f.recipient.Destination = &waverpc.Output_Pubkey{
		Pubkey: schnorr.SerializePubKey(f.senderPriv.PubKey()),
	}

	r.server.clientKeyDesc.PubKey = f.senderPriv.PubKey()
	prepared, err := r.PrepareOOR(ctx, &waverpc.PrepareOORRequest{
		CustomInputs: []*waverpc.CustomOORInput{f.customIn},
		Recipient:    f.recipient,
	})
	require.NoError(t, err)
	require.Len(t, prepared.CheckpointPsbts, 1)
	ark, err := psbtutil.Parse(prepared.ArkPsbt)
	require.NoError(t, err)
	require.Empty(t, ark.Inputs[0].TaprootScriptSpendSig)
	require.Len(t, ark.Inputs[0].TaprootLeafScript, 1)
	require.Equal(
		t, path.WitnessScript,
		ark.Inputs[0].TaprootLeafScript[0].Script,
	)
	cp, err := psbtutil.Parse(prepared.CheckpointPsbts[0])
	require.NoError(t, err)
	require.Equal(t, f.outpoint, cp.UnsignedTx.TxIn[0].PreviousOutPoint)
	require.Equal(t, f.customIn.AmountSat, cp.Inputs[0].WitnessUtxo.Value)
	require.Equal(t, f.customIn.PkScript, cp.Inputs[0].WitnessUtxo.PkScript)
	require.Equal(
		t, cp.UnsignedTx.TxHash(),
		ark.UnsignedTx.TxIn[0].PreviousOutPoint.Hash,
	)

	r.server.clientKeyDesc.PubKey = f.clientPriv.PubKey()
	pkg := &waverpc.CooperativeOORPackage{
		CustomInput: f.customIn, Recipient: f.recipient,
		CheckpointPsbt: prepared.CheckpointPsbts[0],
		ArkPsbt:        prepared.ArkPsbt,
	}
	for _, tc := range []struct {
		name   string
		change func(*waverpc.CooperativeOORPackage)
	}{
		{"funded outpoint", func(p *waverpc.CooperativeOORPackage) {
			op := testWalletOpsOutpoint(88)
			p.CustomInput.Outpoint = op.String()
		}},
		{"payout destination", func(p *waverpc.CooperativeOORPackage) {
			p.Recipient.Destination = &waverpc.Output_Pubkey{
				Pubkey: schnorr.SerializePubKey(
					f.clientPriv.PubKey(),
				),
			}
		}},
		{"funded amount", func(p *waverpc.CooperativeOORPackage) {
			p.CustomInput.AmountSat++
			p.Recipient.AmountSat++
		}},
		{"partial payout", func(p *waverpc.CooperativeOORPackage) {
			p.Recipient.AmountSat--
		}},
		{"funded script", func(p *waverpc.CooperativeOORPackage) {
			p.CustomInput.PkScript[2] ^= 1
		}},
		{"wrong checkpoint", func(p *waverpc.CooperativeOORPackage) {
			p.CheckpointPsbt = p.ArkPsbt
		}},
		{"checkpoint prevout metadata", func(
			p *waverpc.CooperativeOORPackage) {

			packet, err := psbtutil.Parse(p.CheckpointPsbt)
			require.NoError(t, err)
			packet.Inputs[0].WitnessUtxo.Value++
			p.CheckpointPsbt, err = psbtutil.Serialize(packet)
			require.NoError(t, err)
		}},
		{"final prevout metadata", func(
			p *waverpc.CooperativeOORPackage) {

			packet, err := psbtutil.Parse(p.ArkPsbt)
			require.NoError(t, err)
			packet.Inputs[0].WitnessUtxo.Value++
			p.ArkPsbt, err = psbtutil.Serialize(packet)
			require.NoError(t, err)
		}},
		{"wrong final packet", func(p *waverpc.CooperativeOORPackage) {
			p.ArkPsbt = p.CheckpointPsbt
		}},
		{"conditioned leaf", func(p *waverpc.CooperativeOORPackage) {
			p.CustomInput.SpendPath, err = f.claimPath.Encode()
			require.NoError(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := &waverpc.CooperativeOORPackage{}
			proto.Merge(invalid, pkg)
			tc.change(invalid)
			// An unconfigured mock fails the test on any signing
			// call.
			r.oorSignerOverride = &input.MockInputSigner{}
			resp, err := r.SignOORCustomInput(
				ctx, &waverpc.SignOORCustomInputRequest{
					CooperativePackage: invalid,
				},
			)
			require.Nil(t, resp)
			require.Equal(
				t, codes.InvalidArgument, status.Code(err),
			)
		})
	}

	r.oorSignerOverride = input.NewMockSigner(
		[]*btcec.PrivateKey{f.clientPriv}, nil,
	)
	sigs, err := r.SignOORCustomInput(
		ctx, &waverpc.SignOORCustomInputRequest{
			CooperativePackage: pkg,
		},
	)
	require.NoError(t, err)
	require.NotEqual(
		t, sigs.Signature.Signature, sigs.PayoutSignature.Signature,
	)
	require.Equal(t, path.WitnessScript, sigs.PayoutSignature.WitnessScript)
	require.Equal(t, sigs.Signature.Pubkey, sigs.PayoutSignature.Pubkey)

	r.server.clientKeyDesc.PubKey = f.senderPriv.PubKey()
	target := attachCooperativeTestActor(t, r)
	f.customIn.ExternalSignatures = []*waverpc.TaprootScriptSignature{
		sigs.Signature,
	}
	f.customIn.PayoutSignatures = []*waverpc.TaprootScriptSignature{
		sigs.PayoutSignature,
	}
	send := &waverpc.SendOORRequest{
		Recipients: []*waverpc.Output{
			f.recipient,
		},
		CustomInputs: []*waverpc.CustomOORInput{
			f.customIn,
		},
	}
	for _, tc := range []struct {
		name   string
		change func(*waverpc.CustomOORInput)
	}{
		{
			"missing payout",
			func(in *waverpc.CustomOORInput) {
				in.PayoutSignatures = nil
			},
		},
		{
			"missing checkpoint",
			func(in *waverpc.CustomOORInput) {
				in.ExternalSignatures = nil
			},
		},
		{
			"checkpoint reused",
			func(in *waverpc.CustomOORInput) {
				in.PayoutSignatures = in.ExternalSignatures
			},
		},
		{
			"payout reused",
			func(in *waverpc.CustomOORInput) {
				in.ExternalSignatures = in.PayoutSignatures
			},
		},
		{
			"partial sighash",
			func(in *waverpc.CustomOORInput) {
				in.PayoutSignatures[0].Sighash = 2
			},
		},
		{"wrong signer", func(in *waverpc.CustomOORInput) {
			in.PayoutSignatures[0].Pubkey = f.senderPriv.
				PubKey().
				SerializeCompressed()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &waverpc.SendOORRequest{}
			proto.Merge(req, send)
			tc.change(req.CustomInputs[0])
			resp, err := r.SendOOR(ctx, req)
			require.Nil(t, resp)
			require.Equal(
				t, codes.InvalidArgument, status.Code(err),
			)
			require.Empty(t, target.capturedRequests())
		})
	}
	resp, err := r.SendOOR(ctx, send)
	require.NoError(t, err)
	require.Equal(t, "submitted", resp.Status)
	requests := target.capturedRequests()
	require.Len(t, requests, 1)
	require.Equal(
		t, path.WitnessScript, requests[0].Inputs[0].OwnerLeafScript,
	)
	require.Len(t, requests[0].Inputs[0].ExternalArkSignatures, 1)
}

// attachCooperativeTestActor captures successful RPC dispatch without sending
// anything to the operator or requiring an integration network.
func attachCooperativeTestActor(t *testing.T,
	r *RPCServer) *capturingSendOORActor {

	t.Helper()
	system := actor.NewActorSystem()
	t.Cleanup(
		func() {
			require.NoError(
				t,
				system.Shutdown(
					context.Background(),
				),
			)
		},
	)
	key := actor.NewServiceKey[wallet.WalletMsg, wallet.WalletResp](
		"refund-test-wallet",
	)
	r.server.walletRef = fn.Some(
		key.Spawn(
			system, "refund-wallet", &sendOORTestWallet{},
		),
	)
	r.server.actorSystem = system
	r.server.log = btclog.Disabled
	r.server.walletServicesReady = resolvedSignal()
	id := chainhash.HashH([]byte("test refund"))
	target := &capturingSendOORActor{response: &oor.StartTransferResponse{
		SessionID: oor.SessionID(id),
	}}
	oor.NewServiceKey().Spawn(system, "refund-oor", target)

	return target
}

// TestCooperativeFundingRecordBinding rejects caller funding data that would
// shadow a different record already held by the wallet.
func TestCooperativeFundingRecordBinding(t *testing.T) {
	t.Parallel()
	f := newCustomOORRPCFixture(t)
	stored := &vtxo.Descriptor{
		Amount: 1, PkScript: bytes.Clone(f.customIn.PkScript),
	}
	require.Error(t, checkCooperativeFunding(f.customIn, stored, nil))
	stored.Amount = 42_000
	require.NoError(t, checkCooperativeFunding(f.customIn, stored, nil))
	stored.OperatorKey = f.clientPriv.PubKey()
	require.Error(
		t,
		checkCooperativeFunding(
			f.customIn, stored, f.senderPriv.PubKey(),
		),
	)
	stored.OperatorKey = nil
	stored.PkScript[2] ^= 1
	require.Error(t, checkCooperativeFunding(f.customIn, stored, nil))
}
