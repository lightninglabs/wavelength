//go:build systest

package systest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/actordelivery"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

const (
	oorVTXOManagerEventuallyTimeout = 10 * time.Second
	oorVTXOManagerEventuallyPoll    = 100 * time.Millisecond
)

// TestOORIncomingMaterializationSpawnsVTXOActor verifies the OOR receive flow
// materializes an incoming VTXO, notifies the VTXO manager, and results in a
// live VTXO actor registered in the actor system.
func TestOORIncomingMaterializationSpawnsVTXOActor(t *testing.T) {
	ParallelN(t)

	h := NewSysTestHarness(t)
	ctx := h.Context()

	sqlDB := db.NewTestDB(t)
	clk := clock.NewDefaultClock()
	dbStore := db.NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), h.Logger(),
	)
	vtxoStore := dbStore.NewVTXOStore(clk)

	deliveryStore, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		sqlDB.DB, sqlDB.Backend(), clk, h.Logger(),
	)
	require.NoError(t, err)

	chainSourceRef := h.NewChainSourceActor()
	vtxoWallet := lndbackend.NewClientWallet(
		h.Harness.LND.Signer, h.Harness.LND.WalletKit,
	)

	manager := vtxo.NewManager(&vtxo.ManagerConfig{
		Store:       vtxoStore,
		Wallet:      vtxoWallet,
		ChainSource: chainSourceRef,
		ActorSystem: h.ActorSystem(),
		ChainParams: h.ChainParams(),
		Log:         fn.Some(h.SubLogger(vtxo.Subsystem)),
	})
	managerKey := actor.NewServiceKey[vtxo.ManagerMsg, vtxo.ManagerResp](
		"systest-vtxo-manager",
	)
	managerRef := actor.RegisterWithSystem(
		h.ActorSystem(), "systest-vtxo-manager", managerKey, manager,
	)

	err = manager.Start(ctx, managerRef)
	require.NoError(t, err)

	arkPSBT, finalCheckpoints, recipients, metadata, recipientKey,
		operatorKey := buildSystemTestIncomingMaterialization(t)
	sessionID := oor.SessionID(arkPSBT.UnsignedTx.TxHash())
	expectedIndex := recipients[0].OutputIndex

	err = seedIncomingRound(
		ctx, sqlDB.Queries, metadata.RoundID, clk.Now().Unix(),
	)
	require.NoError(t, err)

	handler := &oor.LocalPersistenceOutboxHandler{
		Store:       vtxoStore,
		OperatorKey: operatorKey,
		ExitDelay:   10,
		NotifyIncomingVTXOs: func(_ context.Context,
			_ []*vtxo.Descriptor) error {

			return nil
		},
		ResolveIncomingClientKey: func(_ context.Context,
			recipient oor.ArkRecipientOutput) (
			keychain.KeyDescriptor, error) {

			require.Equal(t, expectedIndex, recipient.OutputIndex)

			return keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(_ context.Context,
			gotSessionID oor.SessionID,
			recipient oor.ArkRecipientOutput, _ *psbt.Packet,
			_ []*psbt.Packet) (
			oor.IncomingVTXOMetadata, error) {

			require.Equal(t, sessionID, gotSessionID)
			require.Equal(t, expectedIndex, recipient.OutputIndex)

			return metadata, nil
		},
	}

	oorActor := oor.NewOORClientActor(oor.ClientActorCfg{
		Log:           fn.Some(h.SubLogger(oor.Subsystem)),
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorSystem:   h.ActorSystem(),
		ActorID:       "systest-oor-vtxo-manager",
		VTXOManager:   managerRef,
	})
	defer oorActor.Stop()

	session, outbox, err := oor.DriveIncomingTransferWithCheckpoints(
		ctx, sessionID, arkPSBT, finalCheckpoints,
	)
	require.NoError(t, err)

	err = driveIncomingOutbox(
		ctx, session, handler, sessionID, managerRef, outbox,
	)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash:  arkPSBT.UnsignedTx.TxHash(),
		Index: recipients[0].OutputIndex,
	}

	require.Eventually(t, func() bool {
		count, countErr := activeVTXOCount(ctx, managerRef)
		if countErr != nil {
			return false
		}

		return count == 1
	}, oorVTXOManagerEventuallyTimeout, oorVTXOManagerEventuallyPoll)

	require.Eventually(t, func() bool {
		refs := actor.FindInReceptionist(
			h.ActorSystem().Receptionist(),
			vtxo.VTXOActorServiceKey(outpoint),
		)

		return len(refs) == 1
	}, oorVTXOManagerEventuallyTimeout, oorVTXOManagerEventuallyPoll)

	desc, err := vtxoStore.GetVTXO(ctx, outpoint)
	require.NoError(t, err)
	require.Equal(t, metadata.RoundID, desc.RoundID)
	require.Equal(t, metadata.CommitmentTxID, desc.CommitmentTxID)
	require.Equal(t, metadata.BatchExpiry, desc.BatchExpiry)
	require.Equal(t, metadata.CreatedHeight, desc.CreatedHeight)
	require.Equal(t, metadata.ChainDepth, desc.ChainDepth)

	state, err := session.FSM.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &oor.ReceiveCompleted{}, state)
}

// driveIncomingOutbox executes the receive-flow outbox through the local
// persistence handler until the FSM reaches its terminal acked state.
// When a VTXOManager ref is provided, materialized VTXOs are forwarded
// to the manager, mirroring the actor's driveOutbox behavior.
func driveIncomingOutbox(ctx context.Context, session *oor.ReceiveSession,
	handler *oor.LocalPersistenceOutboxHandler, sessionID oor.SessionID,
	managerRef actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp],
	outbox []oor.OutboxEvent) error {

	for _, msg := range outbox {
		switch typedMsg := msg.(type) {
		case *oor.IncomingTransferNotification:
			continue

		case *oor.MaterializeIncomingVTXOsRequest:
			followUps, err := handler.Handle(
				ctx, sessionID, typedMsg,
			)
			if err != nil {
				return err
			}

			for _, followUp := range followUps {
				// Mirror actor notification: forward
				// materialized VTXOs to the manager.
				err = notifyMaterialized(
					ctx, followUp, managerRef,
				)
				if err != nil {
					return err
				}

				fut := session.FSM.AskEvent(ctx, followUp)
				result := fut.Await(ctx)
				if result.IsErr() {
					return result.Err()
				}

				nextOutbox := result.UnwrapOr(nil)
				if err := driveIncomingOutbox(
					ctx, session, handler, sessionID,
					managerRef, nextOutbox,
				); err != nil {
					return err
				}
			}

		case *oor.QueryIncomingMetadataRequest:
			followUps, err := handler.Handle(
				ctx, sessionID, typedMsg,
			)
			if err != nil {
				return err
			}

			for _, followUp := range followUps {
				fut := session.FSM.AskEvent(ctx, followUp)
				result := fut.Await(ctx)
				if result.IsErr() {
					return result.Err()
				}

				nextOutbox := result.UnwrapOr(nil)
				if err := driveIncomingOutbox(
					ctx, session, handler, sessionID,
					managerRef, nextOutbox,
				); err != nil {
					return err
				}
			}

		case *oor.SendIncomingAckRequest:
			followUps, err := handler.Handle(
				ctx, sessionID, typedMsg,
			)
			if err != nil {
				return err
			}

			for _, followUp := range followUps {
				fut := session.FSM.AskEvent(ctx, followUp)
				result := fut.Await(ctx)
				if result.IsErr() {
					return result.Err()
				}
			}

		default:
			return fmt.Errorf(
				"unexpected outbox event: %T", typedMsg,
			)
		}
	}

	return nil
}

// activeVTXOCount queries the VTXO manager actor for its current live count.
func activeVTXOCount(ctx context.Context,
	managerRef actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp]) (
	int, error) {

	fut := managerRef.Ask(ctx, &vtxo.GetActiveVTXOCountRequest{})
	result := fut.Await(ctx)
	resp, err := result.Unpack()
	if err != nil {
		return 0, err
	}

	countResp, ok := resp.(*vtxo.GetActiveVTXOCountResponse)
	if !ok {
		return 0, fmt.Errorf("unexpected manager response: %T", resp)
	}

	return countResp.Count, nil
}

// seedIncomingRound inserts the round row referenced by the incoming VTXO
// fixture so the VTXO insert satisfies its foreign-key constraint regardless
// of the active test database backend.
func seedIncomingRound(ctx context.Context, roundStore db.RoundStore,
	roundID string, nowUnix int64) error {

	return roundStore.InsertRound(ctx, db.InsertRoundParams{
		RoundID:               roundID,
		ConfirmationHeight:    sql.NullInt32{},
		ConfirmationBlockHash: nil,
		CommitmentTx:          nil,
		CommitmentTxid:        nil,
		VtxtTree:              nil,
		Status:                "confirmed",
		CreationTime:          nowUnix,
		LastUpdateTime:        nowUnix,
		StartHeight:           0,
	})
}

// buildSystemTestIncomingMaterialization constructs a canonical Ark PSBT and
// metadata suitable for exercising the OOR receive materialization path.
func buildSystemTestIncomingMaterialization(t *testing.T) (*psbt.Packet,
	[]*psbt.Packet, []oor.ArkRecipientOutput, oor.IncomingVTXOMetadata,
	*btcec.PrivateKey, *btcec.PublicKey) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inputValue := btcutil.Amount(10000)
	inputs := []oortx.CheckpointInput{
		{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: wire.OutPoint{
					Hash:  [32]byte{0x11},
					Index: 0,
				},
				Output: &wire.TxOut{
					Value: int64(inputValue),
					PkScript: systemTestTaprootPkScript(
						t, operatorKey.PubKey(),
					),
				},
			},
			OwnerLeafScript: []byte{0x51},
		},
	}

	vtxoTapKey, err := arkscript.VTXOTapKey(
		recipientKey.PubKey(), policy.OperatorKey, 10,
	)
	require.NoError(t, err)

	recipientPkScript, err := txscript.PayToTaprootScript(vtxoTapKey)
	require.NoError(t, err)

	outputs := []oortx.RecipientOutput{
		{
			PkScript: recipientPkScript,
			Value:    inputValue,
		},
	}

	checkpoint, err := oortx.BuildCheckpointPSBT(policy, inputs[0])
	require.NoError(t, err)
	checkpointTxHash := checkpoint.PSBT.UnsignedTx.TxHash()
	checkpointTxOut := checkpoint.PSBT.UnsignedTx.TxOut[0]

	arkPSBT, err := oortx.BuildArkPSBT(
		[]oortx.CheckpointOutput{
			{
				Txid:           checkpointTxHash,
				Output:         checkpointTxOut,
				TapTreeEncoded: checkpoint.TapTreeEncoded,
			},
		},
		outputs,
	)
	require.NoError(t, err)

	recipients, err := oor.ExtractArkRecipients(arkPSBT)
	require.NoError(t, err)

	metadata := oor.IncomingVTXOMetadata{
		RoundID:        "systest-round",
		CommitmentTxID: inputs[0].SpentVTXO.Outpoint.Hash,
		BatchExpiry:    1000,
		TreeDepth:      1,
		ChainDepth:     2,
		CreatedHeight:  700,
		TreePath: &tree.Tree{
			BatchOutpoint: wire.OutPoint{
				Hash:  inputs[0].SpentVTXO.Outpoint.Hash,
				Index: 0,
			},
			Root: &tree.Node{
				Input: inputs[0].SpentVTXO.Outpoint,
				Outputs: []*wire.TxOut{
					checkpoint.PSBT.UnsignedTx.TxOut[0],
				},
				CoSigners: []*btcec.PublicKey{},
				Children:  make(map[uint32]*tree.Node),
			},
		},
	}

	return arkPSBT, []*psbt.Packet{checkpoint.PSBT}, recipients, metadata,
		recipientKey, operatorKey.PubKey()
}

// systemTestTaprootPkScript returns a valid P2TR pkScript for systest
// fixtures.
func systemTestTaprootPkScript(t *testing.T,
	key *btcec.PublicKey) []byte {

	t.Helper()

	pkScript, err := txscript.PayToTaprootScript(key)
	require.NoError(t, err)

	return pkScript
}

// notifyMaterialized forwards materialized VTXOs from an
// IncomingHandledEvent to the VTXO manager, mirroring the
// actor's driveOutbox notification path.
func notifyMaterialized(ctx context.Context,
	ev oor.Event,
	mgr actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp],
) error {

	handled, ok := ev.(*oor.IncomingHandledEvent)
	if !ok || len(handled.MaterializedVTXOs) == 0 {
		return nil
	}

	if mgr == nil {
		return nil
	}

	return mgr.Tell(ctx, &vtxo.VTXOsMaterializedNotification{
		VTXOs: handled.MaterializedVTXOs,
	})
}
