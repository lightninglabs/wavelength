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

	unlockedOutpoints []wire.OutPoint
	unlockCh          chan []wire.OutPoint
}

func (m *mockAssetWalletBehavior) Receive(_ context.Context,
	msg wallet.WalletMsg) fn.Result[wallet.WalletResp] {

	switch req := msg.(type) {
	case *wallet.SelectAndLockAssetVTXOsRequest:
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
