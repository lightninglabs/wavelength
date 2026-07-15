package waved

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type testTaprootAssetOORPreparer struct {
	mu sync.Mutex

	requests []*oor.TaprootAssetOORPrepareRequest
	mutate   func(*oor.TaprootAssetOORPreparation)
	err      error
}

type assetPrepareRequest = oor.TaprootAssetOORPrepareRequest

func (p *testTaprootAssetOORPreparer) PrepareTaprootAssetOOR(_ context.Context,
	request *oor.TaprootAssetOORPrepareRequest) (
	*oor.TaprootAssetOORPreparation, error) {

	p.mu.Lock()
	p.requests = append(p.requests, request)
	prepareErr := p.err
	p.mu.Unlock()

	if prepareErr != nil {
		return nil, prepareErr
	}

	recipients := cloneTestTaprootAssetRecipients(request.Recipients)
	assetRoot := chainhash.HashH([]byte(request.RequestID + "-recipient"))
	template, err := arkscript.DecodePolicyTemplate(
		recipients[0].VTXOPolicyTemplate,
	)
	if err != nil {
		return nil, err
	}
	compiled, err := template.Compile()
	if err != nil {
		return nil, err
	}
	composed, err := arkscript.ComposeWithSiblingRoot(
		compiled, assetRoot,
	)
	if err != nil {
		return nil, err
	}
	recipients[0].PkScript, err = txscript.PayToTaprootScript(
		composed.OutputKey(),
	)
	if err != nil {
		return nil, err
	}
	recipients[0].TaprootAssetRoot = &assetRoot

	arkPSBT, checkpointPSBTs, err := oor.BuildSubmitPackage(
		request.Policy, request.Inputs, recipients,
	)
	if err != nil {
		return nil, err
	}
	preparation := &oor.TaprootAssetOORPreparation{
		PreparedSubmit: &oor.PreparedSubmitPackage{
			ArkPSBT:         arkPSBT,
			CheckpointPSBTs: checkpointPSBTs,
			TaprootAssetTransfer: &oortx.TaprootAssetTransfer{
				Version: oortx.TaprootAssetTransferVersion,
				CheckpointPackages: [][]byte{
					[]byte("checkpoint-package"),
				},
				ArkPackage: []byte("ark-package"),
			},
		},
		Recipients: recipients,
	}
	if p.mutate != nil {
		p.mutate(preparation)
	}

	return preparation, nil
}

func (p *testTaprootAssetOORPreparer) captured() []*assetPrepareRequest {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append(
		[]*oor.TaprootAssetOORPrepareRequest(nil), p.requests...,
	)
}

type taprootAssetOORRPCFixture struct {
	rpcServer *RPCServer
	preparer  *testTaprootAssetOORPreparer
	oorActor  *capturingSendOORActor
	request   *waverpc.SendOORRequest
	desc      *vtxo.Descriptor
}

func newTaprootAssetOORRPCFixture(t *testing.T) *taprootAssetOORRPCFixture {
	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	assetScriptKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		amountSat = btcutil.Amount(50_000)
		exitDelay = uint32(10)
	)

	desc, _ := newSendOORTestVTXO(
		t, operatorKey.PubKey(), 0x61, amountSat,
	)
	inputAssetRoot := chainhash.HashH([]byte("asset-input-root"))
	desc.TaprootAssetRoot = &inputAssetRoot
	desc.PkScript, err = desc.EffectivePkScript()
	require.NoError(t, err)

	vtxoStore, _, sessionStore := newSendOORTestStores(t)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), desc))

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()

		require.NoError(t, system.Shutdown(shutdownCtx))
	})

	testWallet := &sendOORTestWallet{}
	walletKey := actor.NewServiceKey[
		wallet.WalletMsg, wallet.WalletResp,
	](
		"taproot-asset-oor-test-wallet",
	)
	walletRef := walletKey.Spawn(
		system, "taproot-asset-oor-test-wallet", testWallet,
	)

	sessionHash := chainhash.HashH([]byte("taproot-asset-oor-session"))
	oorActor := &capturingSendOORActor{
		response: &oor.StartTransferResponse{
			SessionID: oor.SessionID(sessionHash),
		},
	}
	oor.NewServiceKey().Spawn(
		system, "taproot-asset-oor-test-actor", oorActor,
	)

	preparer := &testTaprootAssetOORPreparer{}
	walletReady := make(chan struct{})
	close(walletReady)
	server := &Server{
		cfg: &Config{
			TaprootAssetOORPreparer: preparer,
		},
		log:             btclog.Disabled,
		walletReady:     walletReady,
		chainParams:     &chaincfg.RegressionNetParams,
		actorSystem:     system,
		vtxoStore:       vtxoStore,
		oorSessionStore: sessionStore,
		walletRef:       fn.Some(walletRef),
		clientKeyDesc:   desc.ClientKey,
		serverConn: newBufconnClient(t, &fakeArkService{
			getInfoResponse: &arkrpc.GetInfoResponse{
				Pubkey: operatorKey.
					PubKey().
					SerializeCompressed(),
				VtxoExitDelay: exitDelay,
				DustLimit:     1,
			},
		}),
	}

	request := &waverpc.SendOORRequest{
		Recipients: []*waverpc.Output{
			sendOORPolicyRecipient(
				t, recipientKey.PubKey(), operatorKey.PubKey(),
				exitDelay, int64(amountSat),
			),
		},
		CustomInputs: []*waverpc.CustomOORInput{{
			Outpoint: desc.Outpoint.String(),
		}},
		IdempotencyKey: "taproot-asset-request-1",
		TaprootAsset: &waverpc.TaprootAssetOORIntent{
			AssetRef:       "tapr1asset",
			AssetAmount:    21,
			InputProofFile: []byte("confirmed-proof"),
			RecipientScriptKey: assetScriptKey.PubKey().
				SerializeCompressed(),
			AcknowledgeUnconfirmed: true,
		},
	}

	return &taprootAssetOORRPCFixture{
		rpcServer: NewRPCServer(server),
		preparer:  preparer,
		oorActor:  oorActor,
		request:   request,
		desc:      desc,
	}
}

// TestSendOORTaprootAssetPreparesBeforeActor proves the daemon turns the
// public asset intent into a root-enriched, immutable actor request.
func TestSendOORTaprootAssetPreparesBeforeActor(t *testing.T) {
	t.Parallel()

	fixture := newTaprootAssetOORRPCFixture(t)
	response, err := fixture.rpcServer.SendOOR(
		t.Context(), fixture.request,
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", response.GetStatus())
	require.Len(t, response.GetRecipientOutpoints(), 1)

	prepareRequests := fixture.preparer.captured()
	require.Len(t, prepareRequests, 1)
	prepareRequest := prepareRequests[0]
	require.Equal(
		t, fixture.request.GetIdempotencyKey(),
		prepareRequest.RequestID,
	)
	require.EqualValues(t, 21, prepareRequest.Intent.AssetAmount)
	require.Equal(
		t, fixture.desc.TaprootAssetRoot,
		prepareRequest.Inputs[0].TaprootAssetRoot,
	)

	actorRequests := fixture.oorActor.capturedRequests()
	require.Len(t, actorRequests, 1)
	actorRequest := actorRequests[0]
	require.NotNil(t, actorRequest.PreparedSubmit)
	require.NotNil(t, actorRequest.Recipients[0].TaprootAssetRoot)
	require.NoError(
		t, actorRequest.Recipients[0].ValidateTaprootAssetCommitment(),
	)
}

// TestSendOORTaprootAssetFailsClosed covers public-shape, feature-gate, BTC
// accounting, and adapter-tamper failures before the durable actor is called.
func TestSendOORTaprootAssetFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		mutate       func(*taprootAssetOORRPCFixture)
		wantCode     codes.Code
		wantContains string
	}{
		{
			name: "missing acknowledgement",
			mutate: func(f *taprootAssetOORRPCFixture) {
				intent := f.request.TaprootAsset
				intent.AcknowledgeUnconfirmed = false
			},
			wantCode:     codes.InvalidArgument,
			wantContains: "acknowledge_unconfirmed=true",
		},
		{
			name: "missing idempotency key",
			mutate: func(f *taprootAssetOORRPCFixture) {
				f.request.IdempotencyKey = ""
			},
			wantCode:     codes.InvalidArgument,
			wantContains: "require an idempotency key",
		},
		{
			name: "preparer disabled",
			mutate: func(f *taprootAssetOORRPCFixture) {
				cfg := f.rpcServer.server.cfg
				cfg.TaprootAssetOORPreparer = nil
			},
			wantCode:     codes.FailedPrecondition,
			wantContains: "preparer is not configured",
		},
		{
			name: "BTC change required",
			mutate: func(f *taprootAssetOORRPCFixture) {
				f.request.Recipients[0].AmountSat--
			},
			wantCode:     codes.InvalidArgument,
			wantContains: "requires exact BTC value",
		},
		{
			name: "adapter changes value",
			mutate: func(f *taprootAssetOORRPCFixture) {
				f.preparer.mutate = incrementAssetRecipientValue
			},
			wantCode:     codes.Internal,
			wantContains: "recipient 0 value changed",
		},
		{
			name: "typed preparer error",
			mutate: func(f *taprootAssetOORRPCFixture) {
				f.preparer.err = status.Error(
					codes.Unavailable, "tapd unavailable",
				)
			},
			wantCode:     codes.Unavailable,
			wantContains: "tapd unavailable",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newTaprootAssetOORRPCFixture(t)
			test.mutate(fixture)

			_, err := fixture.rpcServer.SendOOR(
				t.Context(), fixture.request,
			)
			require.Equal(t, test.wantCode, status.Code(err))
			require.ErrorContains(t, err, test.wantContains)
			require.Empty(t, fixture.oorActor.capturedRequests())
		})
	}
}

func incrementAssetRecipientValue(preparation *oor.TaprootAssetOORPreparation) {
	preparation.Recipients[0].Value++
}

func cloneTestTaprootAssetRecipients(
	recipients []oortx.RecipientOutput) []oortx.RecipientOutput {

	result := make([]oortx.RecipientOutput, len(recipients))
	for idx := range recipients {
		result[idx] = recipients[idx]
		result[idx].PkScript = append(
			[]byte(nil), recipients[idx].PkScript...,
		)
		result[idx].VTXOPolicyTemplate = append(
			[]byte(nil), recipients[idx].VTXOPolicyTemplate...,
		)
	}

	return result
}

var _ oor.TaprootAssetOORPreparer = (*testTaprootAssetOORPreparer)(nil)
