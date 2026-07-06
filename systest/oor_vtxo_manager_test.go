//go:build systest

package systest

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/actordelivery"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/lib/types"
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

type oorVTXOManagerSystestFixture struct {
	h             *SysTestHarness
	clk           clock.Clock
	roundStore    db.RoundStore
	vtxoStore     vtxo.VTXOStore
	deliveryStore actor.TxAwareDeliveryStore
	managerRef    actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp]
}

func newOORVTXOManagerSystestFixture(t *testing.T,
	name string) *oorVTXOManagerSystestFixture {

	t.Helper()

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

	serviceName := "systest-vtxo-manager-" + name
	managerKey := actor.NewServiceKey[vtxo.ManagerMsg, vtxo.ManagerResp](
		serviceName,
	)
	managerRef := actor.RegisterWithSystem(
		h.ActorSystem(), serviceName, managerKey, manager,
	)

	err = manager.Start(ctx, managerRef)
	require.NoError(t, err)

	return &oorVTXOManagerSystestFixture{
		h:             h,
		clk:           clk,
		roundStore:    sqlDB.Queries,
		vtxoStore:     vtxoStore,
		deliveryStore: deliveryStore,
		managerRef:    managerRef,
	}
}

// TestOORIncomingMaterializationSpawnsVTXOActor verifies the OOR receive flow
// materializes an incoming VTXO, notifies the VTXO manager, and results in a
// live VTXO actor registered in the actor system.
func TestOORIncomingMaterializationSpawnsVTXOActor(t *testing.T) {
	ParallelN(t)

	f := newOORVTXOManagerSystestFixture(t, "incoming")
	ctx := f.h.Context()

	arkPSBT, finalCheckpoints, recipients, metadata, recipientKey,
		operatorKey := buildSystemTestIncomingMaterialization(t)
	sessionID := oor.SessionID(arkPSBT.UnsignedTx.TxHash())
	expectedIndex := recipients[0].OutputIndex

	err := seedIncomingRound(
		ctx, f.roundStore, metadata.RoundID, f.clk.Now().Unix(),
	)
	require.NoError(t, err)

	handler := &oor.LocalPersistenceOutboxHandler{
		Store:       f.vtxoStore,
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
			_ []*psbt.Packet) (oor.IncomingVTXOMetadata, error) {

			require.Equal(t, sessionID, gotSessionID)
			require.Equal(t, expectedIndex, recipient.OutputIndex)

			return metadata, nil
		},
	}

	session, outbox, err := oor.DriveIncomingTransferWithCheckpoints(
		ctx, sessionID, arkPSBT, finalCheckpoints, nil,
	)
	require.NoError(t, err)

	err = driveIncomingOutbox(
		ctx, session, handler, sessionID, f.managerRef, outbox,
	)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash:  arkPSBT.UnsignedTx.TxHash(),
		Index: recipients[0].OutputIndex,
	}

	require.Eventually(t, func() bool {
		count, countErr := activeVTXOCount(ctx, f.managerRef)
		if countErr != nil {
			return false
		}

		return count == 1
	}, oorVTXOManagerEventuallyTimeout, oorVTXOManagerEventuallyPoll)

	require.Eventually(t, func() bool {
		refs := actor.FindInReceptionist(
			f.h.ActorSystem().Receptionist(),
			vtxo.VTXOActorServiceKey(outpoint),
		)

		return len(refs) == 1
	}, oorVTXOManagerEventuallyTimeout, oorVTXOManagerEventuallyPoll)

	desc, err := f.vtxoStore.GetVTXO(ctx, outpoint)
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

// TestOORSelfChangeMaterializationSkipsExternalRecipient verifies the OOR
// receive path can materialize only the wallet-owned change output from a
// multi-recipient Ark tx while ignoring the external recipient output.
func TestOORSelfChangeMaterializationSkipsExternalRecipient(t *testing.T) {
	ParallelN(t)

	f := newOORVTXOManagerSystestFixture(t, "change")
	ctx := f.h.Context()

	arkPSBT, finalCheckpoints, recipients, changeRecipient, metadata,
		changeKey, operatorKey := buildSystemTestChangeMaterialization(
		t,
	)
	sessionID := oor.SessionID(arkPSBT.UnsignedTx.TxHash())

	err := seedIncomingRound(
		ctx, f.roundStore, metadata.RoundID, f.clk.Now().Unix(),
	)
	require.NoError(t, err)

	handler := &oor.LocalPersistenceOutboxHandler{
		Store:       f.vtxoStore,
		OperatorKey: operatorKey,
		ExitDelay:   10,
		NotifyIncomingVTXOs: func(_ context.Context,
			_ []*vtxo.Descriptor) error {

			return nil
		},
		ResolveIncomingClientKey: func(_ context.Context,
			recipient oor.ArkRecipientOutput) (
			keychain.KeyDescriptor, error) {

			if !bytes.Equal(
				recipient.PkScript, changeRecipient.PkScript,
			) {
				return keychain.KeyDescriptor{},
					oor.ErrIncomingRecipientNotOwned
			}

			require.Equal(
				t, changeRecipient.OutputIndex,
				recipient.OutputIndex,
			)

			return keychain.KeyDescriptor{
				PubKey: changeKey.PubKey(),
			}, nil
		},
		ResolveIncomingMetadata: func(_ context.Context,
			gotSessionID oor.SessionID,
			recipient oor.ArkRecipientOutput, _ *psbt.Packet,
			_ []*psbt.Packet) (oor.IncomingVTXOMetadata, error) {

			require.Equal(t, sessionID, gotSessionID)
			require.Equal(
				t, changeRecipient.OutputIndex,
				recipient.OutputIndex,
			)

			return metadata, nil
		},
	}

	session, outbox, err := oor.DriveIncomingTransferWithCheckpoints(
		ctx, sessionID, arkPSBT, finalCheckpoints, nil,
	)
	require.NoError(t, err)

	err = driveIncomingOutbox(
		ctx, session, handler, sessionID, f.managerRef, outbox,
	)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash:  arkPSBT.UnsignedTx.TxHash(),
		Index: changeRecipient.OutputIndex,
	}

	require.Eventually(t, func() bool {
		count, countErr := activeVTXOCount(ctx, f.managerRef)
		if countErr != nil {
			return false
		}

		return count == 1
	}, oorVTXOManagerEventuallyTimeout, oorVTXOManagerEventuallyPoll)

	require.Eventually(t, func() bool {
		refs := actor.FindInReceptionist(
			f.h.ActorSystem().Receptionist(),
			vtxo.VTXOActorServiceKey(outpoint),
		)

		return len(refs) == 1
	}, oorVTXOManagerEventuallyTimeout, oorVTXOManagerEventuallyPoll)

	desc, err := f.vtxoStore.GetVTXO(ctx, outpoint)
	require.NoError(t, err)
	require.Equal(t, changeRecipient.Value, desc.Amount)
	require.Equal(t, metadata.RoundID, desc.RoundID)
	require.Len(t, recipients, 2)

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

		case *oor.ScheduleRetryRequest:
			// The incoming metadata path arms a give-up/backoff
			// retry timer alongside its query (see fab1754c). This
			// synchronous driver resolves metadata immediately, so
			// the timer never needs to fire; ignore it.
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
			return fmt.Errorf("unexpected outbox event: %T",
				typedMsg)
		}
	}

	return nil
}

// activeVTXOCount queries the VTXO manager actor for its current live count.
func activeVTXOCount(ctx context.Context,
	managerRef actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp]) (int,
	error) {

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
					Hash: [32]byte{
						0x11,
					},
					Index: 0,
				},
				Output: &wire.TxOut{
					Value: int64(inputValue),
					PkScript: systemTestTaprootPkScript(
						t, operatorKey.PubKey(),
					),
				},
			},
			OwnerLeafScript: []byte{
				0x51,
			},
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

	commitmentTxID := inputs[0].SpentVTXO.Outpoint.Hash
	metadata := oor.IncomingVTXOMetadata{
		RoundID:        "systest-round",
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    1000,
		ChainDepth:     2,
		CreatedHeight:  700,
		Ancestry: []types.Ancestry{{
			TreePath: &tree.Tree{
				BatchOutpoint: wire.OutPoint{
					Hash:  commitmentTxID,
					Index: 0,
				},
				Root: &tree.Node{
					Input: inputs[0].SpentVTXO.Outpoint,
					Outputs: []*wire.TxOut{
						checkpoint.
							PSBT.UnsignedTx.
							TxOut[0],
					},
					CoSigners: []*btcec.PublicKey{},
					Children:  make(map[uint32]*tree.Node),
				},
			},
			CommitmentTxID: commitmentTxID,
			InputIndices: []uint32{
				0,
			},
			TreeDepth: 1,
		}},
	}

	return arkPSBT, []*psbt.Packet{checkpoint.PSBT}, recipients, metadata,
		recipientKey, operatorKey.PubKey()
}

// buildSystemTestChangeMaterialization constructs an Ark PSBT with an
// external recipient and a wallet-owned change recipient.
func buildSystemTestChangeMaterialization(t *testing.T) (*psbt.Packet,
	[]*psbt.Packet, []oor.ArkRecipientOutput, oor.ArkRecipientOutput,
	oor.IncomingVTXOMetadata, *btcec.PrivateKey, *btcec.PublicKey) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	externalKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	changeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		externalValue btcutil.Amount = 6_000
		changeValue   btcutil.Amount = 4_000
	)
	inputValue := externalValue + changeValue

	inputs := []oortx.CheckpointInput{
		{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: wire.OutPoint{
					Hash: [32]byte{
						0x22,
					},
					Index: 0,
				},
				Output: &wire.TxOut{
					Value: int64(inputValue),
					PkScript: systemTestTaprootPkScript(
						t, operatorKey.PubKey(),
					),
				},
			},
			OwnerLeafScript: []byte{
				0x51,
			},
		},
	}

	externalPkScript := systemTestVTXOPkScript(
		t, externalKey.PubKey(), policy.OperatorKey, 10,
	)
	changePkScript := systemTestVTXOPkScript(
		t, changeKey.PubKey(), policy.OperatorKey, 10,
	)

	outputs := []oortx.RecipientOutput{
		{
			PkScript: externalPkScript,
			Value:    externalValue,
		},
		{
			PkScript: changePkScript,
			Value:    changeValue,
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

	var changeRecipient oor.ArkRecipientOutput
	for _, recipient := range recipients {
		if bytes.Equal(recipient.PkScript, changePkScript) {
			changeRecipient = recipient
			break
		}
	}
	require.NotZero(t, changeRecipient.Value)
	require.Equal(t, changeValue, changeRecipient.Value)

	commitmentTxID := inputs[0].SpentVTXO.Outpoint.Hash
	metadata := oor.IncomingVTXOMetadata{
		RoundID:        "systest-change-round",
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    1000,
		ChainDepth:     2,
		CreatedHeight:  700,
		Ancestry: []types.Ancestry{{
			TreePath: &tree.Tree{
				BatchOutpoint: wire.OutPoint{
					Hash:  commitmentTxID,
					Index: 0,
				},
				Root: &tree.Node{
					Input: inputs[0].SpentVTXO.Outpoint,
					Outputs: []*wire.TxOut{
						checkpoint.
							PSBT.UnsignedTx.
							TxOut[0],
					},
					CoSigners: []*btcec.PublicKey{},
					Children:  make(map[uint32]*tree.Node),
				},
			},
			CommitmentTxID: commitmentTxID,
			InputIndices: []uint32{
				0,
			},
			TreeDepth: 1,
		}},
	}

	return arkPSBT, []*psbt.Packet{checkpoint.PSBT}, recipients,
		changeRecipient, metadata, changeKey, operatorKey.PubKey()
}

func systemTestVTXOPkScript(t *testing.T,
	clientKey, operatorKey *btcec.PublicKey, exitDelay uint32) []byte {

	t.Helper()

	vtxoTapKey, err := arkscript.VTXOTapKey(
		clientKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(vtxoTapKey)
	require.NoError(t, err)

	return pkScript
}

// systemTestTaprootPkScript returns a valid P2TR pkScript for systest
// fixtures.
func systemTestTaprootPkScript(t *testing.T, key *btcec.PublicKey) []byte {
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

// incomingReceive bundles one prepared incoming OOR receive for the concurrent
// materialization systest.
type incomingReceive struct {
	sessionID        oor.SessionID
	arkPSBT          *psbt.Packet
	finalCheckpoints []*psbt.Packet
	handler          *oor.LocalPersistenceOutboxHandler
	outpoint         wire.OutPoint
}

// driveOneIncoming drives a single prepared incoming receive to completion.
func driveOneIncoming(ctx context.Context, r incomingReceive,
	managerRef actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp]) error {

	session, outbox, err := oor.DriveIncomingTransferWithCheckpoints(
		ctx, r.sessionID, r.arkPSBT, r.finalCheckpoints, nil,
	)
	if err != nil {
		return err
	}

	return driveIncomingOutbox(
		ctx, session, r.handler, r.sessionID, managerRef, outbox,
	)
}

// TestOORConcurrentIncomingMaterialization drives several independent incoming
// OOR receives at the same time and verifies they all materialize. This is the
// concurrent-receive shape behind issue #605: under the per-session model each
// receive runs on its own actor, so a burst of receives makes progress in
// parallel rather than serializing behind one global actor.
func TestOORConcurrentIncomingMaterialization(t *testing.T) {
	ParallelN(t)

	f := newOORVTXOManagerSystestFixture(t, "concurrent-incoming")
	ctx := f.h.Context()

	const numReceives = 5

	receives := make([]incomingReceive, 0, numReceives)
	for i := 0; i < numReceives; i++ {
		arkPSBT, finalCheckpoints, recipients, metadata, recipientKey,
			operatorKey := buildSystemTestIncomingMaterialization(t)
		sessionID := oor.SessionID(arkPSBT.UnsignedTx.TxHash())
		expectedIndex := recipients[0].OutputIndex

		err := seedIncomingRound(
			ctx, f.roundStore, metadata.RoundID, f.clk.Now().Unix(),
		)
		require.NoError(t, err)

		recvMetadata := metadata
		recvKey := recipientKey
		handler := &oor.LocalPersistenceOutboxHandler{
			Store:       f.vtxoStore,
			OperatorKey: operatorKey,
			ExitDelay:   10,
			NotifyIncomingVTXOs: func(_ context.Context,
				_ []*vtxo.Descriptor) error {

				return nil
			},
			ResolveIncomingClientKey: func(_ context.Context,
				_ oor.ArkRecipientOutput) (
				keychain.KeyDescriptor, error) {

				return keychain.KeyDescriptor{
					PubKey: recvKey.PubKey(),
				}, nil
			},
			ResolveIncomingMetadata: func(_ context.Context,
				_ oor.SessionID, _ oor.ArkRecipientOutput,
				_ *psbt.Packet, _ []*psbt.Packet) (
				oor.IncomingVTXOMetadata, error) {

				return recvMetadata, nil
			},
		}

		receives = append(receives, incomingReceive{
			sessionID:        sessionID,
			arkPSBT:          arkPSBT,
			finalCheckpoints: finalCheckpoints,
			handler:          handler,
			outpoint: wire.OutPoint{
				Hash:  arkPSBT.UnsignedTx.TxHash(),
				Index: expectedIndex,
			},
		})
	}

	// Drive every receive at once. Each runs on its own session, so the
	// burst should not serialize behind a single actor.
	var wg sync.WaitGroup
	errs := make([]error, numReceives)
	for i := range receives {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			errs[idx] = driveOneIncoming(
				ctx, receives[idx], f.managerRef,
			)
		}(i)
	}
	wg.Wait()

	for i := range errs {
		require.NoError(t, errs[i])
	}

	// Every receive's VTXO must materialize and spawn a live VTXO actor.
	require.Eventually(t, func() bool {
		count, countErr := activeVTXOCount(ctx, f.managerRef)
		if countErr != nil {
			return false
		}

		return count == numReceives
	}, oorVTXOManagerEventuallyTimeout, oorVTXOManagerEventuallyPoll)

	for i := range receives {
		desc, err := f.vtxoStore.GetVTXO(ctx, receives[i].outpoint)
		require.NoError(t, err)
		require.Equal(t, vtxo.VTXOStatusLive, desc.Status)
	}
}
