package waved

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
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

	requests       []*oor.TaprootAssetOORPrepareRequest
	resumeRequests []*oor.TaprootAssetOORResumeRequest
	resume         *oor.TaprootAssetOORResume
	resumeErr      error
	mutate         func(*oor.TaprootAssetOORPreparation)
	err            error
}

type assetPrepareRequest = oor.TaprootAssetOORPrepareRequest
type assetResumeRequest = oor.TaprootAssetOORResumeRequest
type assetPreparationResumer = oor.TaprootAssetOORPreparationResumer

// TestOORReservationStoreShared proves the optional Taproot Asset registrar
// receives the exact durable store cached for the manager and OOR runtime.
func TestOORReservationStoreShared(t *testing.T) {
	t.Parallel()

	shared := &db.SpendingReservationPersistenceStore{}
	server := &Server{reservationStore: shared}
	require.Same(t, shared, server.spendingReservationStore(nil))

	store, err := NewRPCServer(server).OORReservationStore()
	require.NoError(t, err)
	require.Same(t, shared, store)
}

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
	setAssetRecipient := func(recipient *oortx.RecipientOutput,
		label string, amount uint64) error {

		assetRoot := chainhash.HashH([]byte(request.RequestID + label))
		template, err := arkscript.DecodePolicyTemplate(
			recipient.VTXOPolicyTemplate,
		)
		if err != nil {
			return err
		}
		compiled, err := template.Compile()
		if err != nil {
			return err
		}
		composed, err := arkscript.ComposeWithSiblingRoot(
			compiled, assetRoot,
		)
		if err != nil {
			return err
		}
		recipient.PkScript, err = txscript.PayToTaprootScript(
			composed.OutputKey(),
		)
		if err != nil {
			return err
		}
		recipient.TaprootAssetRoot = &assetRoot
		recipient.TaprootAssetRef = request.Intent.AssetRef
		recipient.TaprootAssetAmount = amount

		return nil
	}

	plan, err := request.CarrierAllocation()
	if err != nil {
		return nil, err
	}

	receiverAmount := request.Intent.EffectiveRecipientAssetAmount()
	if err := setAssetRecipient(
		&recipients[0], "-receiver", receiverAmount,
	); err != nil {
		return nil, err
	}

	if receiverAmount < request.Intent.AssetAmount {
		assetChange := cloneTestTaprootAssetRecipients(
			request.Recipients,
		)[0]
		assetChange.Value = plan.AssetChange
		if err := setAssetRecipient(
			&assetChange, "-asset-change",
			request.Intent.AssetAmount-receiverAmount,
		); err != nil {
			return nil, err
		}
		recipients = append(recipients, assetChange)
	}

	senderChange := cloneTestTaprootAssetRecipients(request.Recipients)[0]
	senderChange.Value = plan.SenderChange
	recipients = append(recipients, senderChange)

	if plan.OperatorChange > 0 {
		recipients = append(recipients, oortx.RecipientOutput{
			PkScript:           request.Lease.PkScript,
			Value:              plan.OperatorChange,
			VTXOPolicyTemplate: request.Lease.PolicyTemplate,
		})
	}

	arkPSBT, checkpointPSBTs, err := oor.BuildSubmitPackage(
		request.Policy, request.Inputs, recipients,
	)
	if err != nil {
		return nil, err
	}
	checkpointPackages := make([][]byte, len(request.Inputs))
	assetInputIndex, err := request.AssetInputIndex()
	if err != nil {
		return nil, err
	}
	checkpointPackages[assetInputIndex] = []byte("checkpoint-package")
	preparation := &oor.TaprootAssetOORPreparation{
		PreparedSubmit: &oor.PreparedSubmitPackage{
			ArkPSBT:         arkPSBT,
			CheckpointPSBTs: checkpointPSBTs,
			TaprootAssetTransfer: &oortx.TaprootAssetTransfer{
				Version: oortx.
					TaprootAssetTransferVersion,
				CheckpointPackages: checkpointPackages,
				ArkPackage:         []byte("ark-package"),
			},
		},
		Recipients: recipients,
		Receiver:   recipients[0],
	}
	if p.mutate != nil {
		p.mutate(preparation)
	}

	return preparation, nil
}

func (p *testTaprootAssetOORPreparer) ResumeTaprootAssetOOR(_ context.Context,
	request *oor.TaprootAssetOORResumeRequest) (*oor.TaprootAssetOORResume,
	error) {

	p.mu.Lock()
	defer p.mu.Unlock()

	p.resumeRequests = append(p.resumeRequests, request)
	if p.resumeErr != nil {
		return nil, p.resumeErr
	}
	if p.resume == nil {
		return nil, nil
	}

	return &oor.TaprootAssetOORResume{
		InputOutpoints: append(
			[]wire.OutPoint(nil), p.resume.InputOutpoints...,
		),
		Lease: p.resume.Lease.Clone(),
	}, nil
}

func (p *testTaprootAssetOORPreparer) captured() []*assetPrepareRequest {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append(
		[]*oor.TaprootAssetOORPrepareRequest(nil), p.requests...,
	)
}

func (p *testTaprootAssetOORPreparer) capturedResumes() []*assetResumeRequest {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append(
		[]*oor.TaprootAssetOORResumeRequest(nil), p.resumeRequests...,
	)
}

type taprootAssetOORRPCFixture struct {
	rpcServer     *RPCServer
	preparer      *testTaprootAssetOORPreparer
	oorActor      *capturingSendOORActor
	request       *waverpc.SendOORRequest
	desc          *vtxo.Descriptor
	wallet        *sendOORTestWallet
	artifactStore *db.OORArtifactPersistenceStore
	arkService    *fakeArkService
	lease         *oor.OORCarrierLease
}

func newTaprootAssetOORRPCFixture(t *testing.T) *taprootAssetOORRPCFixture {
	return newTaprootAssetOORRPCFixtureWithActor(t, nil)
}

func newTaprootAssetOORRPCFixtureWithActor(t *testing.T,
	blockingActor *blockingSendOORActor) *taprootAssetOORRPCFixture {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipientKey, err := btcec.NewPrivateKey()
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
	desc.TaprootAssetRef = "tapr1asset"
	desc.TaprootAssetAmount = 21
	desc.PkScript, err = desc.EffectivePkScript()
	require.NoError(t, err)

	vtxoStore, _, sessionStore, artifactStore :=
		newSendOORTestStoresWithArtifacts(t)
	require.NoError(t, vtxoStore.SaveVTXO(t.Context(), desc))

	// The mock preparer derives change recipients from the request
	// recipient's policy script, so registering that script as owned lets
	// the daemon's change-alias registration resolve them like the real
	// wallet-derived change scripts.
	recipientPolicy, err := arkscript.EncodeStandardVTXOTemplate(
		recipientKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)
	recipientTemplate, err := arkscript.DecodePolicyTemplate(
		recipientPolicy,
	)
	require.NoError(t, err)
	recipientPkScript, err := recipientTemplate.PkScript()
	require.NoError(t, err)
	require.NoError(
		t,
		artifactStore.UpsertOwnedReceiveScript(
			t.Context(), db.OwnedReceiveScriptRecord{
				PkScript:       recipientPkScript,
				ClientKey:      desc.ClientKey,
				OperatorPubKey: operatorKey.PubKey(),
				ExitDelay:      int64(exitDelay),
				Source:         db.OwnedReceiveScriptSourceRPC,
				CreatedAt:      time.Now(),
			},
		),
	)

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()

		require.NoError(t, system.Shutdown(shutdownCtx))
	})

	testWallet := &sendOORTestWallet{
		selections: [][]wallet.SelectedVTXO{{
			selectedVTXOFromDescriptor(desc),
		}},
	}
	walletKey := actor.NewServiceKey[
		wallet.WalletMsg, wallet.WalletResp,
	](
		"taproot-asset-oor-test-wallet",
	)
	walletRef := walletKey.Spawn(
		system, "taproot-asset-oor-test-wallet", testWallet,
	)

	sessionHash := chainhash.HashH([]byte("taproot-asset-oor-session"))
	var oorActor *capturingSendOORActor
	if blockingActor != nil {
		oor.NewServiceKey().Spawn(
			system, "taproot-asset-oor-test-actor", blockingActor,
		)
	} else {
		oorActor = &capturingSendOORActor{
			response: &oor.StartTransferResponse{
				SessionID: oor.SessionID(sessionHash),
			},
		}
		oor.NewServiceKey().Spawn(
			system, "taproot-asset-oor-test-actor", oorActor,
		)
	}

	// The operator advertises a carrier float and grants a lease whose
	// policy binds the float owner and current operator keys to the
	// leased pkScript.
	floatKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	leasePolicy, leasePkScript, err := arkscript.
		EncodeStandardVTXOArtifacts(
			floatKey.PubKey(), operatorKey.PubKey(), exitDelay,
		)
	require.NoError(t, err)
	lease := &oor.OORCarrierLease{
		Outpoint: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("carrier-float")),
			Index: 0,
		},
		Value:          25_000,
		PolicyTemplate: leasePolicy,
		PkScript:       leasePkScript,
		ExpiresAtUnix:  1_700_000_000,
	}
	arkService := &fakeArkService{
		getInfoResponse: &arkrpc.GetInfoResponse{
			Pubkey: operatorKey.
				PubKey().
				SerializeCompressed(),
			VtxoExitDelay: exitDelay,
			DustLimit:     1000,
			OorCarrierPubkey: schnorr.SerializePubKey(
				floatKey.PubKey(),
			),
		},
		leaseResponse: &arkrpc.LeaseOORCarrierResponse{
			Outpoint:           lease.Outpoint.String(),
			ValueSat:           int64(lease.Value),
			VtxoPolicyTemplate: lease.PolicyTemplate,
			PkScript:           lease.PkScript,
			ExpiresAtUnix:      lease.ExpiresAtUnix,
		},
	}

	preparer := &testTaprootAssetOORPreparer{}
	walletReady := make(chan struct{})
	close(walletReady)
	server := &Server{
		cfg: &Config{
			TaprootAssetOORPreparer: preparer,
		},
		log:              btclog.Disabled,
		walletReady:      walletReady,
		chainParams:      &chaincfg.RegressionNetParams,
		actorSystem:      system,
		oorArtifactStore: artifactStore,
		vtxoStore:        vtxoStore,
		oorSessionStore:  sessionStore,
		walletRef:        fn.Some(walletRef),
		clientKeyDesc:    desc.ClientKey,
		serverConn:       newBufconnClient(t, arkService),
	}

	request := &waverpc.SendOORRequest{
		Recipients: []*waverpc.Output{
			sendOORPolicyRecipient(
				t, recipientKey.PubKey(), operatorKey.PubKey(),
				exitDelay, 0,
			),
		},
		IdempotencyKey: "taproot-asset-request-1",
		TaprootAsset: &waverpc.TaprootAssetOORIntent{
			InputVtxoOutpoint:      desc.Outpoint.String(),
			AssetRef:               "tapr1asset",
			AssetAmount:            21,
			InputProofFile:         []byte("confirmed-proof"),
			AcknowledgeUnconfirmed: true,
		},
	}

	return &taprootAssetOORRPCFixture{
		rpcServer:     NewRPCServer(server),
		preparer:      preparer,
		oorActor:      oorActor,
		request:       request,
		desc:          desc,
		wallet:        testWallet,
		artifactStore: artifactStore,
		arkService:    arkService,
		lease:         lease,
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
	require.Zero(t, prepareRequest.Intent.RecipientAssetAmount)
	require.EqualValues(
		t, 21, prepareRequest.Intent.EffectiveRecipientAssetAmount(),
	)
	require.Equal(t, btcutil.Amount(1000), prepareRequest.OutputFloor)
	require.NotNil(t, prepareRequest.BuildChangeRecipient)
	require.Equal(
		t, fixture.desc.Outpoint,
		prepareRequest.Intent.InputVTXOOutpoint,
	)
	require.Equal(
		t, fixture.desc.TaprootAssetRoot,
		prepareRequest.Inputs[0].TaprootAssetRoot,
	)

	// The daemon stamps the receiver leaf at the floor, leases one float
	// covering it, and rides the float along as a foreign input.
	require.Equal(
		t, btcutil.Amount(1000), prepareRequest.Recipients[0].Value,
	)
	require.True(t, fixture.lease.FundingEquals(prepareRequest.Lease))
	require.Len(t, prepareRequest.Inputs, 2)
	require.True(t, prepareRequest.Inputs[1].OperatorFunded)
	require.Equal(
		t, fixture.lease.Outpoint,
		prepareRequest.Inputs[1].VTXO.Outpoint,
	)
	require.Len(t, fixture.arkService.leaseRequests, 1)
	require.EqualValues(
		t, 1000, fixture.arkService.leaseRequests[0].GetRequiredSat(),
	)

	actorRequests := fixture.oorActor.capturedRequests()
	require.Len(t, actorRequests, 1)
	actorRequest := actorRequests[0]
	require.NotNil(t, actorRequest.PreparedSubmit)
	require.NotNil(t, actorRequest.Recipients[0].TaprootAssetRoot)
	require.NoError(
		t, actorRequest.Recipients[0].ValidateTaprootAssetCommitment(),
	)

	// Full send: floor-valued receiver, the sender's returned carrier,
	// and the operator's float residual.
	require.Len(t, actorRequest.Recipients, 3)
	require.Equal(
		t, btcutil.Amount(1000), actorRequest.Recipients[0].Value,
	)
	require.Equal(
		t, btcutil.Amount(50_000), actorRequest.Recipients[1].Value,
	)
	require.Equal(
		t, btcutil.Amount(24_000), actorRequest.Recipients[2].Value,
	)
	require.Equal(
		t, fixture.lease.PkScript, actorRequest.Recipients[2].PkScript,
	)

	selectRequests := fixture.wallet.selectionRequests()
	require.Len(t, selectRequests, 1)
	require.Equal(
		t, []wire.OutPoint{fixture.desc.Outpoint},
		selectRequests[0].RequiredOutpoints,
	)
	require.Equal(
		t, btcutil.Amount(50_000), selectRequests[0].TargetAmount,
	)
	require.True(t, selectRequests[0].WaitForDurable)
	require.Empty(t, fixture.wallet.unlockBatches())
}

// TestSendOORTaprootAssetFiltersChangeOutpoints proves a partial allocation
// funds local asset change but returns only the caller's receiver.
func TestSendOORTaprootAssetFiltersChangeOutpoints(t *testing.T) {
	t.Parallel()

	fixture := newTaprootAssetOORRPCFixture(t)
	fixture.request.TaprootAsset.RecipientAssetAmount = 13

	response, err := fixture.rpcServer.SendOOR(
		t.Context(), fixture.request,
	)
	require.NoError(t, err)
	require.Len(t, response.GetRecipientOutpoints(), 1)

	prepareRequests := fixture.preparer.captured()
	require.Len(t, prepareRequests, 1)
	require.Len(t, prepareRequests[0].Recipients, 1)
	require.EqualValues(
		t, 13,
		prepareRequests[0].Intent.EffectiveRecipientAssetAmount(),
	)

	// Both new asset leaves are leased from the operator float.
	require.Len(t, fixture.arkService.leaseRequests, 1)
	require.EqualValues(
		t, 2000, fixture.arkService.leaseRequests[0].GetRequiredSat(),
	)

	selectRequests := fixture.wallet.selectionRequests()
	require.Len(t, selectRequests, 1)
	require.Equal(t, btcutil.Amount(50_000), selectRequests[0].TargetAmount)

	actorRequests := fixture.oorActor.capturedRequests()
	require.Len(t, actorRequests, 1)
	require.Len(t, actorRequests[0].Recipients, 4)
	require.EqualValues(
		t, 13, actorRequests[0].Recipients[0].TaprootAssetAmount,
	)
	require.EqualValues(
		t, 8, actorRequests[0].Recipients[1].TaprootAssetAmount,
	)
	require.Equal(
		t, btcutil.Amount(1000), actorRequests[0].Recipients[1].Value,
	)
	require.Nil(t, actorRequests[0].Recipients[2].TaprootAssetRoot)
	require.Equal(
		t, btcutil.Amount(50_000), actorRequests[0].Recipients[2].Value,
	)
	require.Nil(t, actorRequests[0].Recipients[3].TaprootAssetRoot)
	require.Equal(
		t, btcutil.Amount(23_000), actorRequests[0].Recipients[3].Value,
	)
	require.Equal(
		t, fixture.lease.PkScript,
		actorRequests[0].Recipients[3].PkScript,
	)

	sessionHash := chainhash.HashH([]byte("taproot-asset-oor-session"))
	receiverOutpoint, err := oortx.RecipientOutPoint(
		sessionHash, actorRequests[0].Recipients,
		actorRequests[0].Recipients[0],
	)
	require.NoError(t, err)
	require.Equal(
		t, receiverOutpoint.String(), response.RecipientOutpoints[0],
	)
}

// TestSendOORTaprootAssetFundsCarriersFromFloat proves a partial send funds
// both new asset leaves at the operator floor out of the leased float while
// the sender's full carrier returns as one plain change output.
func TestSendOORTaprootAssetFundsCarriersFromFloat(t *testing.T) {
	t.Parallel()

	fixture := newTaprootAssetOORRPCFixture(t)
	fixture.request.TaprootAsset.RecipientAssetAmount = 20

	response, err := fixture.rpcServer.SendOOR(
		t.Context(), fixture.request,
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", response.GetStatus())

	// Selection targets exactly the asset input; no Bitcoin filler VTXO
	// is selected.
	selectRequests := fixture.wallet.selectionRequests()
	require.Len(t, selectRequests, 1)
	require.Equal(t, btcutil.Amount(50_000), selectRequests[0].TargetAmount)
	require.Equal(
		t, []wire.OutPoint{fixture.desc.Outpoint},
		selectRequests[0].RequiredOutpoints,
	)

	prepareRequests := fixture.preparer.captured()
	require.Len(t, prepareRequests, 1)
	prepareRequest := prepareRequests[0]
	require.Zero(t, prepareRequest.Intent.AssetChangeCarrierValueSat)

	plan, err := prepareRequest.CarrierAllocation()
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(1000), plan.AssetChange)
	require.Equal(t, btcutil.Amount(50_000), plan.SenderChange)
	require.Equal(t, btcutil.Amount(23_000), plan.OperatorChange)

	// The receiver and the floor-valued asset-change carrier are
	// asset-bearing; the sender's carrier and the operator's residual
	// return as plain Bitcoin outputs.
	actorRequests := fixture.oorActor.capturedRequests()
	require.Len(t, actorRequests, 1)
	recipients := actorRequests[0].Recipients
	require.Len(t, recipients, 4)
	require.NotNil(t, recipients[0].TaprootAssetRoot)
	require.Equal(t, btcutil.Amount(1000), recipients[0].Value)
	require.NotNil(t, recipients[1].TaprootAssetRoot)
	require.Equal(t, btcutil.Amount(1000), recipients[1].Value)
	require.Nil(t, recipients[2].TaprootAssetRoot)
	require.Equal(t, btcutil.Amount(50_000), recipients[2].Value)
	require.Nil(t, recipients[3].TaprootAssetRoot)
	require.Equal(t, btcutil.Amount(23_000), recipients[3].Value)
	require.Equal(t, fixture.lease.PkScript, recipients[3].PkScript)

	// The composed asset-change script is registered as an owned alias
	// before admission, so the incoming self-notification resolves it
	// regardless of which recipient event drives the receive session.
	alias, err := fixture.artifactStore.LookupOwnedReceiveScript(
		t.Context(), recipients[1].PkScript,
	)
	require.NoError(t, err)
	require.Equal(
		t, db.OwnedReceiveScriptSourceAssetAlias, alias.Source,
	)

	// The operator's float residual pays a foreign script and must not
	// be registered as an owned alias.
	_, err = fixture.artifactStore.LookupOwnedReceiveScript(
		t.Context(), recipients[3].PkScript,
	)
	require.Error(t, err)
}

// TestSendOORTaprootAssetAdoptsPreparedInputs proves an RPC retry after the
// asset commits can rebuild the exact journaled inputs without asking wallet
// selection to reserve an already-Spending asset VTXO again.
func TestSendOORTaprootAssetAdoptsPreparedInputs(t *testing.T) {
	t.Parallel()

	fixture := newTaprootAssetOORRPCFixture(t)
	fixture.preparer.resume = &oor.TaprootAssetOORResume{
		InputOutpoints: []wire.OutPoint{
			fixture.desc.Outpoint,
		},
		Lease: fixture.lease,
	}
	require.NoError(
		t,
		fixture.rpcServer.server.vtxoStore.UpdateVTXOStatus(
			t.Context(), fixture.desc.Outpoint,
			vtxo.VTXOStatusSpending,
		),
	)

	response, err := fixture.rpcServer.SendOOR(
		t.Context(), fixture.request,
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", response.GetStatus())
	require.Zero(t, fixture.wallet.selectCount())
	require.Empty(t, fixture.wallet.unlockBatches())
	require.Len(t, fixture.preparer.capturedResumes(), 1)
	require.Len(t, fixture.preparer.captured(), 1)
	require.Len(t, fixture.oorActor.capturedRequests(), 1)

	// A resumed request reuses the journaled lease; it never re-leases.
	require.Empty(t, fixture.arkService.leaseRequests)
	prepareRequests := fixture.preparer.captured()
	require.True(
		t, fixture.lease.FundingEquals(prepareRequests[0].Lease),
	)
}

// TestSendOORTaprootAssetResumeRequiresJournaledLease proves an adopted
// preparation without a journaled carrier lease fails into reconciliation
// instead of leasing a fresh float under a committed graph.
func TestSendOORTaprootAssetResumeRequiresJournaledLease(t *testing.T) {
	t.Parallel()

	fixture := newTaprootAssetOORRPCFixture(t)
	fixture.preparer.resume = &oor.TaprootAssetOORResume{
		InputOutpoints: []wire.OutPoint{
			fixture.desc.Outpoint,
		},
	}
	require.NoError(
		t,
		fixture.rpcServer.server.vtxoStore.UpdateVTXOStatus(
			t.Context(), fixture.desc.Outpoint,
			vtxo.VTXOStatusSpending,
		),
	)

	_, err := fixture.rpcServer.SendOOR(t.Context(), fixture.request)
	require.Equal(t, codes.Aborted, status.Code(err))
	require.ErrorContains(t, err, "no journaled carrier lease")
	require.Empty(t, fixture.arkService.leaseRequests)
	require.Zero(t, fixture.wallet.selectCount())
	require.Empty(t, fixture.oorActor.capturedRequests())
}

// TestSendOORTaprootAssetQuarantinesAfterPreparation proves a later OOR actor
// admission failure cannot unlock carrier inputs after tapd has committed both
// asset transitions.
func TestSendOORTaprootAssetQuarantinesAfterPreparation(t *testing.T) {
	t.Parallel()

	fixture := newTaprootAssetOORRPCFixture(t)
	fixture.oorActor.err = fmt.Errorf("admission failed")

	_, err := fixture.rpcServer.SendOOR(t.Context(), fixture.request)
	require.Equal(t, codes.Internal, status.Code(err))
	require.ErrorContains(t, err, "admission failed")
	require.Equal(t, 1, fixture.wallet.selectCount())
	require.Empty(t, fixture.wallet.unlockBatches())
	require.Len(t, fixture.preparer.captured(), 1)
	require.Len(t, fixture.oorActor.capturedRequests(), 1)
}

// TestSendOORTaprootAssetFailedSessionRequiresReconciliation verifies a
// terminal deterministic asset session is surfaced as Aborted rather than a
// new submission, while its already-committed carrier set stays quarantined.
func TestSendOORTaprootAssetFailedSessionRequiresReconciliation(t *testing.T) {
	t.Parallel()

	fixture := newTaprootAssetOORRPCFixture(t)
	fixture.oorActor.err = fmt.Errorf("terminal asset session: %w",
		oor.ErrTaprootAssetCommitOutcomeUnknown)

	_, err := fixture.rpcServer.SendOOR(t.Context(), fixture.request)
	require.Equal(t, codes.Aborted, status.Code(err))
	require.ErrorContains(t, err, "requires reconciliation")
	require.Equal(t, 1, fixture.wallet.selectCount())
	require.Empty(t, fixture.wallet.unlockBatches())
	require.Len(t, fixture.preparer.captured(), 1)
	require.Len(t, fixture.oorActor.capturedRequests(), 1)
}

// TestSendOORTaprootAssetCancelKeepsInputsQuarantined proves a caller that
// stops waiting after asset preparation cannot transfer unlock ownership to
// the detached actor cleanup path. This remains true when that actor later
// reports an existing session, which releases freshly selected BTC inputs.
func TestSendOORTaprootAssetCancelKeepsInputsQuarantined(t *testing.T) {
	t.Parallel()

	sessionHash := chainhash.HashH([]byte("taproot-asset-existing"))
	blockingActor := &blockingSendOORActor{
		started:   make(chan struct{}),
		release:   make(chan struct{}),
		completed: make(chan struct{}),
		response: &oor.StartTransferResponse{
			SessionID: oor.SessionID(sessionHash),
			Existing:  true,
		},
	}
	fixture := newTaprootAssetOORRPCFixtureWithActor(t, blockingActor)

	waitCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errChan := make(chan error, 1)
	go func() {
		_, err := fixture.rpcServer.SendOOR(waitCtx, fixture.request)
		errChan <- err
	}()

	select {
	case <-blockingActor.started:
	case err := <-errChan:
		require.NoError(t, err)
		require.FailNow(t, "SendOOR returned before actor start")

	case <-time.After(time.Second):
		require.FailNow(t, "OOR actor did not start")
	}

	cancel()
	select {
	case err := <-errChan:
		require.Equal(t, codes.Canceled, status.Code(err))

	case <-time.After(time.Second):
		require.FailNow(t, "SendOOR did not observe caller cancel")
	}
	require.Empty(t, fixture.wallet.unlockBatches())

	close(blockingActor.release)
	select {
	case <-blockingActor.completed:
	case <-time.After(time.Second):
		require.FailNow(t, "detached OOR actor did not complete")
	}

	require.Never(t, func() bool {
		return len(fixture.wallet.unlockBatches()) > 0
	}, 250*time.Millisecond, 10*time.Millisecond)
}

// TestSendOORTaprootAssetRejectsInvalidResume proves a corrupted journal or an
// unresolved tapd attempt fails before wallet selection and cannot release the
// quarantined VTXO.
func TestSendOORTaprootAssetRejectsInvalidResume(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		resume       func(*taprootAssetOORRPCFixture)
		wantCode     codes.Code
		wantContains string
	}{
		{
			name: "live input is not adopted",
			resume: func(f *taprootAssetOORRPCFixture) {
				f.preparer.resume = &oor.TaprootAssetOORResume{
					InputOutpoints: []wire.OutPoint{
						f.desc.Outpoint,
					},
				}
			},
			wantCode:     codes.Aborted,
			wantContains: "has status live",
		},
		{
			name: "missing asset input",
			resume: func(f *taprootAssetOORRPCFixture) {
				f.preparer.resume = &oor.TaprootAssetOORResume{
					InputOutpoints: []wire.OutPoint{{
						Hash: chainhash.HashH(
							[]byte("other"),
						),
					}},
				}
			},
			wantCode:     codes.Internal,
			wantContains: "requested asset input exactly once",
		},
		{
			name: "duplicate input",
			resume: func(f *taprootAssetOORRPCFixture) {
				f.preparer.resume = &oor.TaprootAssetOORResume{
					InputOutpoints: []wire.OutPoint{
						f.desc.Outpoint,
						f.desc.Outpoint,
					},
				}
			},
			wantCode:     codes.Internal,
			wantContains: "duplicate input",
		},
		{
			name: "ambiguous commit",
			resume: func(f *taprootAssetOORRPCFixture) {
				f.preparer.resumeErr = fmt.Errorf("lost "+
					"response: %w",
					oor.ErrTaprootAssetCommitOutcomeUnknown)
			},
			wantCode:     codes.Aborted,
			wantContains: "requires reconciliation",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newTaprootAssetOORRPCFixture(t)
			test.resume(fixture)
			_, err := fixture.rpcServer.SendOOR(
				t.Context(), fixture.request,
			)
			require.Equal(t, test.wantCode, status.Code(err))
			require.ErrorContains(t, err, test.wantContains)
			require.Zero(t, fixture.wallet.selectCount())
			require.Empty(t, fixture.wallet.unlockBatches())
			require.Empty(t, fixture.oorActor.capturedRequests())
		})
	}
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
		wantSelect   bool
		wantUnlock   bool
	}{
		{
			name: "missing managed input outpoint",
			mutate: func(f *taprootAssetOORRPCFixture) {
				f.request.TaprootAsset.InputVtxoOutpoint = ""
			},
			wantCode:     codes.InvalidArgument,
			wantContains: "input VTXO outpoint",
		},
		{
			name: "malformed managed input outpoint",
			mutate: func(f *taprootAssetOORRPCFixture) {
				f.request.TaprootAsset.InputVtxoOutpoint = "bad"
			},
			wantCode:     codes.InvalidArgument,
			wantContains: "input VTXO outpoint",
		},
		{
			name: "custom input bypass rejected",
			mutate: func(f *taprootAssetOORRPCFixture) {
				customInput := &waverpc.CustomOORInput{
					Outpoint: f.desc.Outpoint.String(),
				}
				customInputs := []*waverpc.CustomOORInput{
					customInput,
				}
				f.request.CustomInputs = customInputs
			},
			wantCode:     codes.InvalidArgument,
			wantContains: "do not support custom inputs",
		},
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
			name: "recipient asset amount exceeds input",
			mutate: func(f *taprootAssetOORRPCFixture) {
				f.request.TaprootAsset.RecipientAssetAmount = 22
			},
			wantCode:     codes.InvalidArgument,
			wantContains: "recipient amount exceeds input amount",
		},
		{
			name: "caller-set change carrier",
			mutate: func(f *taprootAssetOORRPCFixture) {
				f.request.TaprootAsset.RecipientAssetAmount = 20
				f.request.TaprootAsset.
					AssetChangeCarrierValueSat = 1000
			},
			wantCode: codes.InvalidArgument,
			wantContains: "asset_change_carrier_value_sat must " +
				"be zero",
		},
		{
			name: "caller-set recipient amount",
			mutate: func(f *taprootAssetOORRPCFixture) {
				f.request.Recipients[0].AmountSat = 48_000
			},
			wantCode:     codes.InvalidArgument,
			wantContains: "recipient amount_sat must be zero",
		},
		{
			name: "unknown asset input VTXO",
			mutate: func(f *taprootAssetOORRPCFixture) {
				f.request.TaprootAsset.InputVtxoOutpoint =
					"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
						"aaaaaaaaaaaaaaaaaaaaaaaa" +
						"aaaaaaaa:0"
			},
			wantCode:     codes.InvalidArgument,
			wantContains: "unknown Taproot Asset input VTXO",
		},
		{
			name: "carrier funding disabled",
			mutate: func(f *taprootAssetOORRPCFixture) {
				f.arkService.getInfoResponse.
					OorCarrierPubkey = nil
			},
			wantCode:     codes.FailedPrecondition,
			wantContains: "does not fund OOR carriers",
		},
		{
			name: "leased float below the floors",
			mutate: func(f *taprootAssetOORRPCFixture) {
				f.arkService.leaseResponse.ValueSat = 500
			},
			wantCode:     codes.FailedPrecondition,
			wantContains: "below required",
		},
		{
			name: "adapter changes value",
			mutate: func(f *taprootAssetOORRPCFixture) {
				f.preparer.mutate = incrementAssetRecipientValue
			},
			wantCode:     codes.Internal,
			wantContains: "recipient 0 value changed",
			wantSelect:   true,
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
			wantSelect:   true,
			wantUnlock:   true,
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
			if test.wantSelect {
				require.Equal(
					t, 1, fixture.wallet.selectCount(),
				)
			} else {
				require.Zero(t, fixture.wallet.selectCount())
			}
			if test.wantUnlock {
				require.Eventually(t, func() bool {
					return len(
						fixture.wallet.unlockBatches(),
					) == 1
				}, time.Second, 10*time.Millisecond)
			} else {
				require.Empty(t, fixture.wallet.unlockBatches())
			}
		})
	}
}

// TestSendOORTaprootAssetAmbiguousCommitKeepsReservation proves an unknown
// tapd outcome is surfaced distinctly and never releases the managed VTXO for
// a competing spend.
func TestSendOORTaprootAssetAmbiguousCommitKeepsReservation(t *testing.T) {
	t.Parallel()

	fixture := newTaprootAssetOORRPCFixture(t)
	fixture.preparer.err = fmt.Errorf("tapd response lost: %w",
		oor.ErrTaprootAssetCommitOutcomeUnknown)

	_, err := fixture.rpcServer.SendOOR(t.Context(), fixture.request)
	require.Equal(t, codes.Aborted, status.Code(err))
	require.ErrorContains(t, err, "requires reconciliation")
	require.Equal(t, 1, fixture.wallet.selectCount())
	require.Empty(t, fixture.oorActor.capturedRequests())
	require.Never(t, func() bool {
		return len(fixture.wallet.unlockBatches()) != 0
	}, 200*time.Millisecond, 10*time.Millisecond)
}

func incrementAssetRecipientValue(preparation *oor.TaprootAssetOORPreparation) {
	preparation.Receiver.Value++
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
var _ assetPreparationResumer = (*testTaprootAssetOORPreparer)(nil)
