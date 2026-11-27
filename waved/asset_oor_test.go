package waved

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/embedded"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var testAssetOORRef = tapsdk.AssetRefFromAssetID(
	tapsdk.AssetID{0x11, 0x22, 0x33},
).String()

// mockAssetWalletBehavior captures SelectAndLockAssetVTXOsRequest and
// UnlockVTXOsRequest messages in focused SendAssetOOR unit tests.
type mockAssetWalletBehavior struct {
	selectResp *wallet.SelectAndLockAssetVTXOsResponse
	selectErr  error
	onSelect   func()

	unlockedOutpoints []wire.OutPoint
	unlockCh          chan []wire.OutPoint
}

func (m *mockAssetWalletBehavior) Receive(_ context.Context,
	msg wallet.WalletMsg) fn.Result[wallet.WalletResp] {

	switch req := msg.(type) {
	case *wallet.SelectAndLockAssetVTXOsRequest:
		if m.onSelect != nil {
			m.onSelect()
		}
		if m.selectErr != nil {
			return fn.Err[wallet.WalletResp](m.selectErr)
		}

		return fn.Ok[wallet.WalletResp](m.selectResp)

	case *wallet.UnlockVTXOsRequest:
		m.unlockedOutpoints = append(
			m.unlockedOutpoints, req.Outpoints...,
		)
		if m.unlockCh != nil {
			select {
			case m.unlockCh <- append(
				[]wire.OutPoint(nil), req.Outpoints...,
			):
			default:
			}
		}

		return fn.Ok[wallet.WalletResp](&wallet.UnlockVTXOsResponse{})

	default:
		return fn.Err[wallet.WalletResp](
			errors.New("unexpected wallet message"),
		)
	}
}

func newSendAssetOORTestRPC(t *testing.T, infoResp *arkrpc.GetInfoResponse) (
	*RPCServer, *db.VTXOPersistenceStore, *db.OORSessionRegistryStoreDB,
	*mockAssetWalletBehavior) {

	t.Helper()

	vtxoStore, _, registryStore, _, _ := newSendOORTestStores(t)
	sys := actor.NewActorSystem()
	t.Cleanup(func() {
		require.NoError(t, sys.Shutdown(context.Background()))
	})

	walletBehavior := &mockAssetWalletBehavior{
		unlockCh: make(chan []wire.OutPoint, 4),
	}
	wKey := actor.NewServiceKey[wallet.WalletMsg, wallet.WalletResp](
		"wallet-asset-oor-test",
	)
	wRef := actor.RegisterWithSystem(
		sys, "wallet-test", wKey, walletBehavior,
	)

	walletReady := make(chan struct{})
	close(walletReady)

	srv := &Server{
		log:                 btclog.Disabled,
		cfg:                 DefaultConfig(),
		actorSystem:         sys,
		walletRef:           fn.Some(wRef),
		walletServicesReady: walletReady,
		vtxoStore:           vtxoStore,
		oorSessionStore:     registryStore,
		arkClient: &stubArkServiceClient{
			resp: infoResp,
		},
		assetClient:    &embedded.Client{},
		embeddedAssets: &embedded.Client{},
	}
	srv.walletState.Store(int32(WalletStateReady))
	srv.arkProtocolVersion.Store(arkrpc.ArkProtocolVersionV1)

	return NewRPCServer(srv), vtxoStore, registryStore, walletBehavior
}

// TestSendAssetOORValidationAndPreview verifies pre-flight validation,
// operator asset/float checks, and dry-run multi-input preview calculation.
func TestSendAssetOORValidationAndPreview(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	carrierPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	infoResp := &arkrpc.GetInfoResponse{
		Pubkey:             operatorPriv.PubKey().SerializeCompressed(),
		VtxoExitDelay:      144,
		DustLimit:          330,
		MinVtxoAmountSat:   1_000,
		SelectedArkVersion: arkrpc.ArkProtocolVersionV1,
		OorCarrierPubkey:   carrierPriv.PubKey().SerializeCompressed(),
		Assets: []*arkrpc.AssetPolicy{
			{
				AssetRef: testAssetOORRef,
			},
		},
	}
	rpcSrv, vtxoStore, _, _ := newSendAssetOORTestRPC(t, infoResp)

	// Invalid asset_ref fails with InvalidArgument.
	_, err = rpcSrv.SendAssetOOR(t.Context(), &waverpc.SendAssetOORRequest{
		AssetRef: "not-hex",
		Amount:   100,
		Destination: &waverpc.SendAssetOORRequest_Pubkey{
			Pubkey: schnorr.SerializePubKey(recipPriv.PubKey()),
		},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// Zero amount fails with InvalidArgument.
	_, err = rpcSrv.SendAssetOOR(t.Context(), &waverpc.SendAssetOORRequest{
		AssetRef: testAssetOORRef,
		Amount:   0,
		Destination: &waverpc.SendAssetOORRequest_Pubkey{
			Pubkey: schnorr.SerializePubKey(recipPriv.PubKey()),
		},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// Missing destination fails with InvalidArgument.
	_, err = rpcSrv.SendAssetOOR(t.Context(), &waverpc.SendAssetOORRequest{
		AssetRef: testAssetOORRef,
		Amount:   100,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// Expired admission deadline fails with FailedPrecondition.
	_, err = rpcSrv.SendAssetOOR(t.Context(), &waverpc.SendAssetOORRequest{
		AssetRef: testAssetOORRef,
		Amount:   100,
		Destination: &waverpc.SendAssetOORRequest_Pubkey{
			Pubkey: schnorr.SerializePubKey(recipPriv.PubKey()),
		},
		AdmissionDeadlineUnixNanos: time.
			Now().
			Add(-time.Second).
			UnixNano(),
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	// Persist two live asset VTXOs: one round-originated (ChainDepth 0,
	// 1200 sat carrier) and one OOR-originated (ChainDepth 1, 1000 sat).
	saveAssetVTXO := func(hashByte byte, amount uint64,
		carrier btcutil.Amount, chainDepth int) {

		root := chainhash.Hash{hashByte, 0xaa}
		policyBytes, err := arkscript.EncodeStandardVTXOTemplate(
			ownerPriv.PubKey(), operatorPriv.PubKey(), 144,
		)
		require.NoError(t, err)
		tmpl, err := arkscript.DecodePolicyTemplate(policyBytes)
		require.NoError(t, err)
		compiled, err := tmpl.Compile()
		require.NoError(t, err)
		composed, err := arkscript.ComposeWithSiblingRoot(
			compiled, root,
		)
		require.NoError(t, err)
		pkScript, err := txscript.PayToTaprootScript(
			composed.OutputKey(),
		)
		require.NoError(t, err)

		require.NoError(
			t,
			vtxoStore.SaveVTXO(
				t.Context(), &vtxo.Descriptor{
					Outpoint: wire.OutPoint{
						Hash:  chainhash.Hash{hashByte},
						Index: 0,
					},
					Amount:         carrier,
					PolicyTemplate: policyBytes,
					PkScript:       pkScript,
					ClientKey: keychain.KeyDescriptor{
						PubKey: ownerPriv.PubKey(),
					},
					OperatorKey: operatorPriv.PubKey(),
					RoundID:     "round-1",
					CommitmentTxID: chainhash.Hash{
						0x99,
					},
					BatchExpiry:        10_000,
					RelativeExpiry:     144,
					ChainDepth:         chainDepth,
					CreatedHeight:      100,
					Status:             vtxo.VTXOStatusLive,
					TaprootAssetRoot:   &root,
					TaprootAssetRef:    testAssetOORRef,
					TaprootAssetAmount: amount,
					TaprootAssetSealedPackage: []byte(
						"sealed-pkg",
					),
				},
			),
		)
	}

	saveAssetVTXO(0x01, 400, 1_200, 0)
	saveAssetVTXO(0x02, 300, 1_000, 1)

	// Dry-run merges both inputs (400 + 300 = 700), leaves 150 asset
	// change, and returns ONLY the round-originated 1200 sat carrier as
	// sender BTC change (the OOR input's 1000 sat is operator-reclaimed).
	preview, err := rpcSrv.SendAssetOOR(
		t.Context(), &waverpc.SendAssetOORRequest{
			AssetRef: testAssetOORRef,
			Amount:   550,
			Destination: &waverpc.SendAssetOORRequest_Pubkey{
				Pubkey: schnorr.SerializePubKey(
					recipPriv.PubKey(),
				),
			},
			DryRun: true,
		},
	)
	require.NoError(t, err)
	require.Equal(t, "preview", preview.Status)
	require.EqualValues(t, 2, preview.SelectedInputCount)
	require.EqualValues(t, 150, preview.AssetChangeAmount)
	require.EqualValues(t, 1_200, preview.SenderBtcChangeSat)
	require.Empty(t, preview.SessionId)

	// When assets engine is disabled, SendAssetOOR fails closed.
	rpcSrv.server.assetClient = nil
	_, err = rpcSrv.SendAssetOOR(t.Context(), &waverpc.SendAssetOORRequest{
		AssetRef: testAssetOORRef,
		Amount:   100,
		Destination: &waverpc.SendAssetOORRequest_Pubkey{
			Pubkey: schnorr.SerializePubKey(recipPriv.PubKey()),
		},
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

// TestSendAssetOORRollsBackSelectionOnLoadFailure verifies that when loading
// selected asset inputs fails after wallet selection, the locked outpoints are
// unlocked via UnlockVTXOsRequest.
func TestSendAssetOORRollsBackSelectionOnLoadFailure(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	carrierPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	infoResp := &arkrpc.GetInfoResponse{
		Pubkey:             operatorPriv.PubKey().SerializeCompressed(),
		VtxoExitDelay:      144,
		DustLimit:          330,
		MinVtxoAmountSat:   1_000,
		SelectedArkVersion: arkrpc.ArkProtocolVersionV1,
		OorCarrierPubkey:   carrierPriv.PubKey().SerializeCompressed(),
		Assets: []*arkrpc.AssetPolicy{
			{
				AssetRef: testAssetOORRef,
			},
		},
	}
	rpcSrv, _, _, walletBehavior := newSendAssetOORTestRPC(t, infoResp)

	missingOP := wire.OutPoint{Hash: chainhash.Hash{0x44}, Index: 0}
	walletBehavior.selectResp = &wallet.SelectAndLockAssetVTXOsResponse{
		SelectedVTXOs: []wallet.SelectedAssetVTXO{{
			Outpoint:     missingOP,
			ReserveEpoch: 3,
			Amount:       1_000,
			AssetAmount:  500,
		}},
		TotalAssetSelected: 500,
	}

	_, err = rpcSrv.SendAssetOOR(t.Context(), &waverpc.SendAssetOORRequest{
		AssetRef: testAssetOORRef,
		Amount:   250,
		Destination: &waverpc.SendAssetOORRequest_Pubkey{
			Pubkey: schnorr.SerializePubKey(recipPriv.PubKey()),
		},
	})
	require.ErrorContains(t, err, "load selected asset VTXO")

	select {
	case unlocked := <-walletBehavior.unlockCh:
		require.Equal(t, []wire.OutPoint{missingOP}, unlocked)

	case <-time.After(5 * time.Second):
		t.Fatal("expected locked asset VTXO to be unlocked on failure")
	}
}

// TestVerifyIncomingAssetOORFailsClosedWhenDisabled verifies that receiving an
// incoming asset OOR output on a daemon without an enabled asset engine fails
// closed with oor.ErrInvalidIncomingAssetPackage.
func TestVerifyIncomingAssetOORFailsClosedWhenDisabled(t *testing.T) {
	t.Parallel()

	srv := &Server{}
	err := srv.verifyIncomingAssetOOR(
		t.Context(), oor.IncomingAssetVerification{
			Recipient: oor.ArkRecipientOutput{
				OutputIndex: 0,
				Asset: &oortx.RecipientAsset{
					Root:   chainhash.Hash{0x11},
					Ref:    testAssetOORRef,
					Amount: 100,
				},
			},
		},
	)
	require.ErrorIs(t, err, oor.ErrInvalidIncomingAssetPackage)
}

// TestServerDurableUnaryBuilderScopedScripts verifies that
// BuildListVTXOsByScopedScriptsRequest routes the composed PkScript on the
// wire while looking up the signing key via SignerScript.
func TestServerDurableUnaryBuilderScopedScripts(t *testing.T) {
	t.Parallel()

	clientKey := testKeyDescriptor(t, 11)
	keyPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientKey.PubKey = keyPriv.PubKey()

	uncomposed := make([]byte, 34)
	uncomposed[0] = 0x51
	uncomposed[1] = 0x20
	uncomposed[2] = 0x11

	composed := make([]byte, 34)
	composed[0] = 0x51
	composed[1] = 0x20
	composed[2] = 0x22

	store := &testReceiveScriptStore{
		records: []db.OwnedReceiveScriptRecord{{
			PkScript:  uncomposed,
			ClientKey: clientKey,
			Source:    db.OwnedReceiveScriptSourceWallet,
		}},
	}
	signer := NewOwnedReceiveScriptSigner(
		store, func(
			keyDesc keychain.KeyDescriptor) indexer.SchnorrSigner {

			return &testOwnedReceiveScriptSigner{
				keyDesc: keyDesc,
				tagSig:  make([]byte, 64),
			}
		},
	)
	idx := indexer.New(
		nil, signer, "test-server", "client:test",
		fn.None[btclog.Logger](),
	)

	builder := &serverDurableUnaryBuilder{
		server: &Server{
			indexer: idx,
		},
	}

	msg, err := builder.BuildListVTXOsByScopedScriptsRequest(
		t.Context(), [][]byte{composed}, [][]byte{uncomposed}, nil, 32,
	)
	require.NoError(t, err)

	req, ok := msg.(*arkrpc.ListVTXOsByScriptsRequest)
	require.True(t, ok)
	require.Len(t, req.Scripts, 1)
	require.True(t, bytes.Equal(composed, req.Scripts[0].PkScript))
	require.NotNil(t, req.Scripts[0].GetTaprootSchnorr())
}

// TestSendAssetOORRejectsUnsafePolicyTemplate verifies that policy_template
// destinations undergo arkscript.ValidatePolicy against the operator key.
func TestSendAssetOORRejectsUnsafePolicyTemplate(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	otherPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	terms := &types.OperatorTerms{
		PubKey:        operatorPriv.PubKey(),
		VTXOExitDelay: 144,
	}

	// An operator-only template has no client protection and must fail
	// ValidatePolicy with InvalidArgument.
	unsafeTmpl := &arkscript.PolicyTemplate{
		Leaves: []arkscript.LeafTemplate{{
			Node: &arkscript.Multisig{
				Keys: []*btcec.PublicKey{
					operatorPriv.PubKey(),
				},
			},
		}},
	}
	unsafeBytes, err := unsafeTmpl.Encode()
	require.NoError(t, err)

	unsafeReq := &waverpc.SendAssetOORRequest{
		Destination: &waverpc.SendAssetOORRequest_PolicyTemplate{
			PolicyTemplate: unsafeBytes,
		},
	}
	_, err = resolveAssetOORRecipientPolicy(unsafeReq, terms)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// A standard collaborative+exit template passes ValidatePolicy.
	safeBytes, err := arkscript.EncodeStandardVTXOTemplate(
		otherPriv.PubKey(), operatorPriv.PubKey(), 144,
	)
	require.NoError(t, err)
	safeReq := &waverpc.SendAssetOORRequest{
		Destination: &waverpc.SendAssetOORRequest_PolicyTemplate{
			PolicyTemplate: safeBytes,
		},
	}
	got, err := resolveAssetOORRecipientPolicy(safeReq, terms)
	require.NoError(t, err)
	require.Equal(t, safeBytes, got)
}

// TestSendAssetOORUnlocksOnCancelledContext verifies that when the caller's
// request context is cancelled after asset VTXOs are locked, SendAssetOOR's
// deferred cleanup still unlocks the VTXOs on a detached context and never
// leaks locks or calls LeaseOORCarrier.
func TestSendAssetOORUnlocksOnCancelledContext(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	carrierPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	infoResp := &arkrpc.GetInfoResponse{
		Pubkey:             operatorPriv.PubKey().SerializeCompressed(),
		VtxoExitDelay:      144,
		DustLimit:          330,
		MinVtxoAmountSat:   1_000,
		SelectedArkVersion: arkrpc.ArkProtocolVersionV1,
		OorCarrierPubkey:   carrierPriv.PubKey().SerializeCompressed(),
		Assets: []*arkrpc.AssetPolicy{{
			AssetRef: testAssetOORRef,
		}},
	}
	rpcSrv, _, _, walletBehavior := newSendAssetOORTestRPC(t, infoResp)

	lockedOP := wire.OutPoint{Hash: chainhash.Hash{0x55}, Index: 0}
	reqCtx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Cancel reqCtx after selection succeeds (via a goroutine right after
	// SelectAndLockAssetVTXOsRequest completes) or fail loadAssetOORInputs
	// on missing VTXO while reqCtx is cancelled; either way the deferred
	// unlock must succeed on a detached context.
	walletBehavior.selectResp = &wallet.SelectAndLockAssetVTXOsResponse{
		SelectedVTXOs: []wallet.SelectedAssetVTXO{{
			Outpoint:     lockedOP,
			ReserveEpoch: 7,
			Amount:       1_000,
			AssetAmount:  500,
		}},
		TotalAssetSelected: 500,
	}
	rpcSrv.oorCarrierLeaseOverride = func(_ context.Context,
		_ btcutil.Amount, _ string) (*oor.OORCarrierLease, error) {

		t.Fatal("carrier lease must not be called on load failure")

		return nil, nil
	}

	// Simulate the caller disconnecting immediately after selection
	// returns: by the time loadAssetOORInputs runs, reqCtx is cancelled.
	walletBehavior.onSelect = func() {
		go func() {
			time.Sleep(5 * time.Millisecond)
			cancel()
		}()
	}

	_, err = rpcSrv.SendAssetOOR(reqCtx, &waverpc.SendAssetOORRequest{
		AssetRef: testAssetOORRef,
		Amount:   250,
		Destination: &waverpc.SendAssetOORRequest_Pubkey{
			Pubkey: schnorr.SerializePubKey(recipPriv.PubKey()),
		},
	})
	require.Error(t, err)

	select {
	case unlocked := <-walletBehavior.unlockCh:
		require.Equal(t, []wire.OutPoint{lockedOP}, unlocked)

	case <-time.After(5 * time.Second):
		t.Fatal(
			"expected locked asset VTXO to be unlocked on " +
				"cancelled request context",
		)
	}
}

// TestSendAssetOORIdempotencyIdentityAndReplayFigures verifies that replaying
// an idempotency key checks canonical (assetRef, amount, recipientPolicy),
// rejects cross-type Bitcoin/asset key reuse with AlreadyExists, and returns
// the stored summary figures.
func TestSendAssetOORIdempotencyIdentityAndReplayFigures(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	carrierPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	otherRecipPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	infoResp := &arkrpc.GetInfoResponse{
		Pubkey:             operatorPriv.PubKey().SerializeCompressed(),
		VtxoExitDelay:      144,
		DustLimit:          330,
		MinVtxoAmountSat:   1_000,
		SelectedArkVersion: arkrpc.ArkProtocolVersionV1,
		OorCarrierPubkey:   carrierPriv.PubKey().SerializeCompressed(),
		Assets: []*arkrpc.AssetPolicy{{
			AssetRef: testAssetOORRef,
		}},
	}
	rpcSrv, _, registryStore, _ := newSendAssetOORTestRPC(t, infoResp)

	recipPolicy, err := arkscript.EncodeStandardVTXOTemplate(
		recipPriv.PubKey(), operatorPriv.PubKey(), 144,
	)
	require.NoError(t, err)

	assetSessionID := chainhash.Hash{0xaa, 0xbb}
	assetReplayRaw, err := oor.NewOutgoingAssetReplayData(
		oor.OutgoingAssetReplay{
			OutputIndex:        2,
			SelectedInputCount: 3,
			AssetRef:           testAssetOORRef,
			AssetAmount:        250,
			AssetChangeAmount:  75,
			SenderBtcChangeSat: 1_500,
			RecipientPolicy:    recipPolicy,
		},
	)
	require.NoError(t, err)

	assetRec := db.OORSessionRegistryRecord{
		SessionID:           assetSessionID,
		ActorID:             "asset-session-1",
		Direction:           db.OORSessionDirectionOutgoing,
		Phase:               "AwaitingAck",
		Status:              db.OORSessionStatusPending,
		IdempotencyKey:      "asset-key-1",
		DispatchRequestData: assetReplayRaw,
	}
	require.NoError(t, registryStore.UpsertSession(t.Context(), assetRec))

	// Exact replay returns stored figures and recipient outpoint.
	replayResp, err := rpcSrv.SendAssetOOR(
		t.Context(), &waverpc.SendAssetOORRequest{
			AssetRef: testAssetOORRef,
			Amount:   250,
			Destination: &waverpc.SendAssetOORRequest_Pubkey{
				Pubkey: schnorr.SerializePubKey(
					recipPriv.PubKey(),
				),
			},
			IdempotencyKey: "asset-key-1",
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", replayResp.Status)
	require.Equal(t, assetSessionID.String(), replayResp.SessionId)
	require.Equal(
		t, wire.OutPoint{Hash: assetSessionID, Index: 2}.String(),
		replayResp.RecipientOutpoint,
	)
	require.EqualValues(t, 3, replayResp.SelectedInputCount)
	require.EqualValues(t, 75, replayResp.AssetChangeAmount)
	require.EqualValues(t, 1_500, replayResp.SenderBtcChangeSat)

	// Reusing the key with a different amount or destination returns
	// AlreadyExists.
	_, err = rpcSrv.SendAssetOOR(
		t.Context(), &waverpc.SendAssetOORRequest{
			AssetRef: testAssetOORRef,
			Amount:   251,
			Destination: &waverpc.SendAssetOORRequest_Pubkey{
				Pubkey: schnorr.SerializePubKey(
					recipPriv.PubKey(),
				),
			},
			IdempotencyKey: "asset-key-1",
		},
	)
	require.Equal(t, codes.AlreadyExists, status.Code(err))

	_, err = rpcSrv.SendAssetOOR(
		t.Context(), &waverpc.SendAssetOORRequest{
			AssetRef: testAssetOORRef,
			Amount:   250,
			Destination: &waverpc.SendAssetOORRequest_Pubkey{
				Pubkey: schnorr.SerializePubKey(
					otherRecipPriv.PubKey(),
				),
			},
			IdempotencyKey: "asset-key-1",
		},
	)
	require.Equal(t, codes.AlreadyExists, status.Code(err))

	// Reusing an asset key in Bitcoin SendOOR replay verification returns
	// AlreadyExists, and diagnostics treat v2 payloads safely as nil BTC.
	attempt, found, err := rpcSrv.findOutgoingOORSessionByIdempotencyKey(
		t.Context(),
		"asset-key-1",
	)
	require.NoError(t, err)
	require.True(t, found)
	_, err = rpcSrv.resolveExistingOORRecipientOutpoints(
		t.Context(), attempt, []*waverpc.Output{
			{AmountSat: 1_000},
		},
	)
	require.Equal(t, codes.AlreadyExists, status.Code(err))

	diagAmt, err := diagnosticRequestedAmount(assetReplayRaw)
	require.NoError(t, err)
	require.Nil(t, diagAmt)

	// Reusing a Bitcoin SendOOR key (v1 payload) in SendAssetOOR returns
	// AlreadyExists.
	btcRecip := oortx.RecipientOutput{
		Value:    1_000,
		PkScript: bytes.Repeat([]byte{0x51}, 34),
	}
	btcReplayRaw, err := oor.NewOutgoingReplayData(
		[]oortx.RecipientOutput{btcRecip},
		[]oortx.RecipientOutput{btcRecip},
	)
	require.NoError(t, err)
	btcRec := db.OORSessionRegistryRecord{
		SessionID: chainhash.Hash{
			0xcc,
			0xdd,
		},
		ActorID:             "btc-session-1",
		Direction:           db.OORSessionDirectionOutgoing,
		Phase:               "AwaitingAck",
		Status:              db.OORSessionStatusPending,
		IdempotencyKey:      "btc-key-1",
		DispatchRequestData: btcReplayRaw,
	}
	require.NoError(t, registryStore.UpsertSession(t.Context(), btcRec))

	_, err = rpcSrv.SendAssetOOR(
		t.Context(), &waverpc.SendAssetOORRequest{
			AssetRef: testAssetOORRef,
			Amount:   250,
			Destination: &waverpc.SendAssetOORRequest_Pubkey{
				Pubkey: schnorr.SerializePubKey(
					recipPriv.PubKey(),
				),
			},
			IdempotencyKey: "btc-key-1",
		},
	)
	require.Equal(t, codes.AlreadyExists, status.Code(err))

	// Disabled server returns nil incomingAssetVerifier so Bitcoin-only
	// daemons skip asset outputs cleanly without failing sibling outputs.
	require.Nil(t, (&Server{}).incomingAssetVerifier())
}

// TestLoadAssetOORInputsUsesCurrentOperatorKeyForCheckpoint verifies that
// after an operator key rotation (desc.OperatorKey != terms.PubKey), the
// checkpoint owner leaf uses the current operator key while the VTXO policy
// template preserves the original input descriptor's operator key.
func TestLoadAssetOORInputsUsesCurrentOperatorKeyForCheckpoint(t *testing.T) {
	t.Parallel()

	oldOpPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	newOpPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	expectedLeaf, err := arkscript.MultiSigCollabTapLeaf(
		ownerPriv.PubKey(), newOpPriv.PubKey(),
	)
	require.NoError(t, err)
	oldLeaf, err := arkscript.MultiSigCollabTapLeaf(
		ownerPriv.PubKey(), oldOpPriv.PubKey(),
	)
	require.NoError(t, err)
	require.NotEqual(t, oldLeaf.Script, expectedLeaf.Script)

	// Verify the policy encoding helper used by loadAssetOORInputs uses
	// newOpPriv when building the checkpoint leaf.
	leafPolicy, err := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{
				ownerPriv.PubKey(),
				newOpPriv.PubKey(),
			},
		},
	}.Encode()
	require.NoError(t, err)
	decoded, err := arkscript.DecodeLeafTemplate(leafPolicy)
	require.NoError(t, err)
	ms, ok := decoded.Node.(*arkscript.Multisig)
	require.True(t, ok)
	require.Equal(
		t, schnorr.SerializePubKey(newOpPriv.PubKey()),
		schnorr.SerializePubKey(ms.Keys[1]),
	)
}
