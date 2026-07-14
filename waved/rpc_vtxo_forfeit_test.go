package waved

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	forfeittx "github.com/lightninglabs/wavelength/lib/tx"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type signVTXOForfeitFixture struct {
	rpcServer *RPCServer

	req *waverpc.SignVTXOForfeitRequest

	forfeitTx *wire.MsgTx
	vtxoOut   wire.OutPoint

	vtxoOutput      *wire.TxOut
	connectorOut    wire.OutPoint
	connectorOutput *wire.TxOut

	localSigBytes []byte
}

func newSignVTXOForfeitFixture(t *testing.T) *signVTXOForfeitFixture {
	return newSignVTXOForfeitFixtureWithLocalVTXO(t, true)
}

func newSignVTXOForfeitFixtureWithLocalVTXO(t *testing.T,
	saveLocalVTXO bool) *signVTXOForfeitFixture {

	t.Helper()

	policy, preimage, senderPriv, receiverPriv, serverPriv :=
		testVHTLCPolicyFixture(t)
	policyTemplate, err := policy.Template.Encode()
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	claimPath, err := policy.ClaimPath(preimage)
	require.NoError(t, err)

	spendPath, err := claimPath.Encode()
	require.NoError(t, err)

	vtxoOutpoint := testWalletOpsOutpoint(11)
	connectorOutpoint := testWalletOpsOutpoint(12)
	roundID := "sign-vtxo-forfeit-round"
	connectorPkScript, err := txscript.PayToTaprootScript(
		serverPriv.PubKey(),
	)
	require.NoError(t, err)
	connectorOutput := &wire.TxOut{
		Value:    546,
		PkScript: connectorPkScript,
	}

	serverForfeitPkScript, err := txscript.PayToTaprootScript(
		senderPriv.PubKey(),
	)
	require.NoError(t, err)

	vtxoAmount := btcutil.Amount(42_000)
	forfeitTx, err := forfeittx.BuildForfeitTxWithContext(
		&vtxoOutpoint, vtxoAmount, &connectorOutpoint,
		btcutil.Amount(connectorOutput.Value), serverForfeitPkScript,
		forfeittx.ForfeitTxContext{
			VTXOSequence: claimPath.RequiredSequence,
			LockTime:     claimPath.RequiredLockTime,
		},
	)
	require.NoError(t, err)

	rawForfeitTx := serializeMsgTx(t, forfeitTx)
	vtxoOutput := &wire.TxOut{
		Value:    int64(vtxoAmount),
		PkScript: pkScript,
	}
	prevFetcher, err := forfeittx.NewForfeitPrevOutFetcher(
		&forfeittx.VTXOSpendContext{
			Outpoint: vtxoOutpoint,
			Output:   vtxoOutput,
		},
		&forfeittx.ConnectorSpendContext{
			Outpoint: connectorOutpoint,
			Output:   connectorOutput,
		},
	)
	require.NoError(t, err)

	sigHashes := txscript.NewTxSigHashes(forfeitTx, prevFetcher)
	leaf := txscript.NewBaseTapLeaf(claimPath.WitnessScript)
	sighash, err := txscript.CalcTapscriptSignaturehash(
		sigHashes, txscript.SigHashDefault, forfeitTx,
		forfeittx.ForfeitVTXOInputIndex, prevFetcher, leaf,
	)
	require.NoError(t, err)

	localSig, err := schnorr.Sign(receiverPriv, sighash)
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

	vtxoStore, _, _ := newSendOORTestStores(t)

	server := &Server{
		cfg: &Config{
			Wallet: &WalletConfig{
				Type: WalletTypeLwwallet,
			},
		},
		walletReady: ready,
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
	server.vtxoStore = vtxoStore
	if saveLocalVTXO {
		require.NoError(
			t,
			vtxoStore.SaveVTXO(
				t.Context(), &vtxo.Descriptor{
					Outpoint:       vtxoOutpoint,
					Amount:         vtxoAmount,
					PkScript:       pkScript,
					PolicyTemplate: policyTemplate,
					ClientKey:      server.clientKeyDesc,
					OperatorKey:    serverPriv.PubKey(),
					RoundID:        roundID,
					BatchExpiry:    1000,
					RelativeExpiry: 144,
				},
			),
		)
	}

	req := &waverpc.SignVTXOForfeitRequest{
		VtxoOutpoint:          vtxoOutpoint.String(),
		VtxoAmountSat:         int64(vtxoAmount),
		VtxoPkScript:          pkScript,
		VtxoPolicyTemplate:    policyTemplate,
		SpendPath:             spendPath,
		UnsignedForfeitTx:     rawForfeitTx,
		ConnectorOutpoint:     connectorOutpoint.String(),
		ConnectorAmountSat:    connectorOutput.Value,
		ConnectorPkScript:     connectorOutput.PkScript,
		ServerForfeitPkScript: serverForfeitPkScript,
	}

	return &signVTXOForfeitFixture{
		rpcServer: &RPCServer{
			server:           server,
			customInputLocks: make(map[wire.OutPoint]struct{}),
		},
		req:             req,
		forfeitTx:       forfeitTx,
		vtxoOut:         vtxoOutpoint,
		vtxoOutput:      vtxoOutput,
		connectorOut:    connectorOutpoint,
		connectorOutput: connectorOutput,
		localSigBytes:   localSig.Serialize(),
	}
}

// TestSignVTXOForfeitSignsExactForfeitTx exercises the daemon-level
// SignVTXOForfeit RPC with a real vHTLC spend path and exact post-connector
// forfeit transaction. The RPC is the primitive swapd will call only after it
// has authorized the swap state; at this layer the important contract is that
// waved validates the semantic policy, spend path, connector input, and
// server penalty output before returning a signature under the local
// participant key.
func TestSignVTXOForfeitSignsExactForfeitTx(t *testing.T) {
	t.Parallel()

	fixture := newSignVTXOForfeitFixture(t)

	signer := &input.MockInputSigner{}
	signer.On(
		"SignOutputRaw", mock.Anything,
		mock.MatchedBy(func(desc *input.SignDescriptor) bool {
			matchesInput := desc.InputIndex ==
				forfeittx.ForfeitVTXOInputIndex

			return matchesInput &&
				desc.SignMethod ==
					input.TaprootScriptSpendSignMethod &&
				bytes.Equal(
					desc.Output.PkScript,
					fixture.req.GetVtxoPkScript(),
				)
		}),
	).Return(mustParseSchnorrSig(t, fixture.localSigBytes), nil).Once()
	fixture.rpcServer.oorSignerOverride = signer

	resp, err := fixture.rpcServer.SignVTXOForfeit(
		t.Context(), fixture.req,
	)
	require.NoError(t, err)
	signer.AssertExpectations(t)

	require.Equal(
		t, fixture.rpcServer.server.clientKeyDesc.PubKey.
			SerializeCompressed(),
		resp.GetPubkey(),
	)

	gotSig := mustParseSchnorrSig(t, resp.GetSignature())
	require.True(
		t,
		forfeitSignatureVerifies(
			t, gotSig, fixture.forfeitTx, fixture.vtxoOut,
			fixture.vtxoOutput, fixture.connectorOut,
			fixture.connectorOutput,
			fixture.rpcServer.server.clientKeyDesc.PubKey,
			fixture.req.GetSpendPath(),
		),
	)
}

// TestSignVTXOForfeitSignsExternalParticipantTranscript covers the custom
// refresh case where this daemon is a required participant in the VTXO policy
// but does not own the VTXO row locally. The RPC must not require local
// ownership, but it must still validate the caller-supplied transcript before
// signing with the daemon identity key.
func TestSignVTXOForfeitSignsExternalParticipantTranscript(t *testing.T) {
	t.Parallel()

	fixture := newSignVTXOForfeitFixtureWithLocalVTXO(t, false)

	signer := &input.MockInputSigner{}
	signer.On(
		"SignOutputRaw", mock.Anything,
		mock.MatchedBy(func(desc *input.SignDescriptor) bool {
			matchesInput := desc.InputIndex ==
				forfeittx.ForfeitVTXOInputIndex

			return matchesInput &&
				desc.SignMethod ==
					input.TaprootScriptSpendSignMethod &&
				bytes.Equal(
					desc.Output.PkScript,
					fixture.req.GetVtxoPkScript(),
				)
		}),
	).Return(mustParseSchnorrSig(t, fixture.localSigBytes), nil).Once()
	fixture.rpcServer.oorSignerOverride = signer

	resp, err := fixture.rpcServer.SignVTXOForfeit(
		t.Context(), fixture.req,
	)
	require.NoError(t, err)
	signer.AssertExpectations(t)

	gotSig := mustParseSchnorrSig(t, resp.GetSignature())
	require.True(
		t,
		forfeitSignatureVerifies(
			t, gotSig, fixture.forfeitTx, fixture.vtxoOut,
			fixture.vtxoOutput, fixture.connectorOut,
			fixture.connectorOutput,
			fixture.rpcServer.server.clientKeyDesc.PubKey,
			fixture.req.GetSpendPath(),
		),
	)
}

// TestSignVTXOForfeitRejectsMalformedRequests pins the signing oracle's
// fail-closed boundary. Each case gets far enough to build the same exact
// request shape used by swapd, then mutates one critical field: local VTXO
// state when available, a penalty output that does not match the server
// script, a spend path that does not require this daemon's key, or transaction
// bytes that already contain a witness. All must be caller errors and must not
// invoke the signer.
func TestSignVTXOForfeitRejectsMalformedRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		mutate       func(*testing.T, *signVTXOForfeitFixture)
		wantContains string
	}{{
		name: "local vtxo mismatch",
		mutate: func(t *testing.T, f *signVTXOForfeitFixture) {
			t.Helper()

			f.req.VtxoAmountSat++
		},
		wantContains: "does not match local vtxo",
	}, {
		name: "wrong server forfeit script",
		mutate: func(t *testing.T, f *signVTXOForfeitFixture) {
			t.Helper()

			_, otherPub := btcecPrivKey(t)
			wrongScript, err := txscript.PayToTaprootScript(
				otherPub,
			)
			require.NoError(t, err)
			f.req.ServerForfeitPkScript = wrongScript
		},
		wantContains: "server forfeit script",
	}, {
		name: "local key not required",
		mutate: func(t *testing.T, f *signVTXOForfeitFixture) {
			t.Helper()

			_, otherPub := btcecPrivKey(t)
			f.rpcServer.server.clientKeyDesc.PubKey = otherPub
		},
		wantContains: "daemon identity key",
	}, {
		name: "transaction already witnessed",
		mutate: func(t *testing.T, f *signVTXOForfeitFixture) {
			t.Helper()

			tx := f.forfeitTx.Copy()
			tx.TxIn[0].Witness = wire.TxWitness{
				[]byte{
					1,
				},
			}
			f.req.UnsignedForfeitTx = serializeMsgTx(t, tx)
		},
		wantContains: "has witness",
	}}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newSignVTXOForfeitFixture(t)
			test.mutate(t, fixture)

			_, err := fixture.rpcServer.SignVTXOForfeit(
				t.Context(), fixture.req,
			)
			require.Equal(
				t, codes.InvalidArgument, status.Code(err),
			)
			require.Contains(
				t, status.Convert(err).Message(),
				test.wantContains,
			)
		})
	}
}

func serializeMsgTx(t *testing.T, tx *wire.MsgTx) []byte {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, tx.Serialize(&buf))

	return buf.Bytes()
}

func mustParseSchnorrSig(t *testing.T, raw []byte) *schnorr.Signature {
	t.Helper()

	sig, err := schnorr.ParseSignature(raw)
	require.NoError(t, err)

	return sig
}

func forfeitSignatureVerifies(t *testing.T, sig *schnorr.Signature,
	forfeitTx *wire.MsgTx, vtxoOutpoint wire.OutPoint,
	vtxoOutput *wire.TxOut, connectorOutpoint wire.OutPoint,
	connectorOutput *wire.TxOut, pubkey *btcec.PublicKey,
	rawSpendPath []byte) bool {

	t.Helper()

	spendPath, err := arkscript.DecodeSpendPath(rawSpendPath)
	require.NoError(t, err)

	prevFetcher, err := forfeittx.NewForfeitPrevOutFetcher(
		&forfeittx.VTXOSpendContext{
			Outpoint: vtxoOutpoint,
			Output:   vtxoOutput,
		},
		&forfeittx.ConnectorSpendContext{
			Outpoint: connectorOutpoint,
			Output:   connectorOutput,
		},
	)
	require.NoError(t, err)

	sigHashes := txscript.NewTxSigHashes(forfeitTx, prevFetcher)
	leaf := txscript.NewBaseTapLeaf(spendPath.WitnessScript)
	sighash, err := txscript.CalcTapscriptSignaturehash(
		sigHashes, txscript.SigHashDefault, forfeitTx,
		forfeittx.ForfeitVTXOInputIndex, prevFetcher, leaf,
	)
	require.NoError(t, err)

	return sig.Verify(sighash, pubkey)
}

func btcecPrivKey(t *testing.T) (*btcec.PrivateKey, *btcec.PublicKey) {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv, priv.PubKey()
}
