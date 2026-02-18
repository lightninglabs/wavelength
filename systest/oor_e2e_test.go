//go:build systest

package systest

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	clientdb "github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/actordelivery"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	clienttree "github.com/lightninglabs/darepo-client/lib/tree"
	clienttx "github.com/lightninglabs/darepo-client/lib/tx"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	clientvtxo "github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo/db"
	serveroor "github.com/lightninglabs/darepo/oor"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

func newClientVTXOStore(t *testing.T,
	descs []*clientvtxo.Descriptor) *clientdb.VTXOPersistenceStore {

	t.Helper()

	const roundStatusInputSigSent = "input_sig_sent"

	sqlDB := clientdb.NewTestDB(t)
	roundDB := clientdb.NewTransactionExecutor[clientdb.RoundStore](
		sqlDB,
		func(tx *sql.Tx) clientdb.RoundStore {
			return sqlDB.Queries.WithTx(tx)
		},
		btclog.Disabled,
	)

	store := clientdb.NewVTXOPersistenceStore(
		roundDB, clock.NewDefaultClock(),
	)

	ctx := t.Context()
	nowUnix := time.Now().Unix()

	for i := range descs {
		desc := descs[i]
		if desc.RoundID == "" {
			desc.RoundID = fmt.Sprintf("oor_fixture:%s",
				desc.Outpoint.Hash.String())
		}

		// Ensure the round exists so the VTXO foreign key constraint can be
		// satisfied in the client DB.
		err := roundDB.InsertRound(ctx, clientdb.InsertRoundParams{
			RoundID:               desc.RoundID,
			ConfirmationHeight:    sql.NullInt32{},
			ConfirmationBlockHash: nil,
			CommitmentTx:          nil,
			CommitmentTxid:        nil,
			VtxtTree:              nil,
			Status:                roundStatusInputSigSent,
			CreationTime:          nowUnix,
			LastUpdateTime:        nowUnix,
			StartHeight:           0,
		})
		require.NoError(t, err)

		// The client-side vtxo persistence schema currently requires a
		// TLV-encoded tree path blob. OOR fixtures mint standalone VTXOs
		// without a tree, so we stub an empty tree for persistence tests.
		if desc.TreePath == nil {
			desc.TreePath = &clienttree.Tree{}
		}

		err = store.SaveVTXO(ctx, desc)
		require.NoError(t, err)
	}

	return store
}

// clientDeliveryStoreShim wraps a TxAwareDeliveryStore, exposing only the
// non-transactional DeliveryStore interface. The durable actor checks for
// TxAwareDeliveryStore at runtime to decide between processInTransaction and
// processWithoutTransaction. The client OOR behavior calls persistCheckpoint
// (which starts its own DB transaction) inside the behavior handler, so wrapping
// the entire handler in an outer transaction causes SQLite deadlocks. The shim
// forces the non-transactional path, matching the server-side deliveryStoreShim.
type clientDeliveryStoreShim struct {
	actor.DeliveryStore
}

// newClientDeliveryStore creates a client-side actor delivery store backed by a
// fresh test database. Each client actor needs its own delivery store to avoid
// mailbox interference with other actors.
func newClientDeliveryStore(t *testing.T) actor.DeliveryStore {
	t.Helper()

	testDB := clientdb.NewTestDB(t)
	store, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		testDB.DB, testDB.Backend(),
		clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	return &clientDeliveryStoreShim{DeliveryStore: store}
}

// newServerDeliveryStore creates a server-side actor delivery store backed by
// its own test database. Isolating the delivery store from the business stores
// (vtxo, session) avoids SQLite single-writer contention during durable actor
// processing.
func newServerDeliveryStore(t *testing.T) actor.DeliveryStore {
	t.Helper()

	sqlStore := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	store, err := db.NewActorDeliveryStoreFromDB(
		dbStore, clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	return store
}

// clientVTXOOutboxHandler is a test-only outbox handler that persists
// client-side VTXO state transitions and forwards transport requests to the
// next handler.
type clientVTXOOutboxHandler struct {
	store *clientdb.VTXOPersistenceStore
	next  clientoor.OutboxHandler
}

func (h *clientVTXOOutboxHandler) Handle(ctx context.Context,
	sessionID clientoor.SessionID,
	outbox clientoor.OutboxEvent) ([]clientoor.Event, error) {

	_ = sessionID

	switch msg := outbox.(type) {
	case *clientoor.MarkInputsSpentRequest:
		if h.store == nil {
			return nil, fmt.Errorf("vtxo store must be provided")
		}

		for i := range msg.Outpoints {
			err := h.store.UpdateVTXOStatus(
				ctx, msg.Outpoints[i],
				clientvtxo.VTXOStatusSpent,
			)
			if err != nil {
				return nil, err
			}
		}

		return []clientoor.Event{
			&clientoor.InputsMarkedSpentEvent{},
		}, nil

	default:
		if h.next == nil {
			return nil, nil
		}

		return h.next.Handle(ctx, sessionID, outbox)
	}
}

var _ clientoor.OutboxHandler = (*clientVTXOOutboxHandler)(nil)

// incomingReceiveOutboxHandler is a test-only outbox handler for the incoming
// transfer FSM. It materializes recipient outputs as client VTXO descriptors so
// tests can assert receiver-side completion.
type incomingReceiveOutboxHandler struct {
	t *testing.T

	recipientKey keychain.KeyDescriptor

	operatorKey *btcec.PublicKey

	exitDelay uint32

	materialized []*clientvtxo.Descriptor
}

func (h *incomingReceiveOutboxHandler) Handle(_ context.Context,
	_ clientoor.SessionID,
	outbox clientoor.OutboxEvent) ([]clientoor.Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *clientoor.IncomingTransferNotification:
		return nil, nil

	case *clientoor.MaterializeIncomingVTXOsRequest:
		if msg.ArkPSBT == nil || msg.ArkPSBT.UnsignedTx == nil {
			return nil, fmt.Errorf("ark psbt must be provided")
		}

		arkTxid := msg.ArkPSBT.UnsignedTx.TxHash()

		for i := range msg.Recipients {
			recipient := msg.Recipients[i]
			desc := &clientvtxo.Descriptor{
				Outpoint: wire.OutPoint{
					Hash:  arkTxid,
					Index: recipient.OutputIndex,
				},
				Amount:         recipient.Value,
				PkScript:       recipient.PkScript,
				ClientKey:      h.recipientKey,
				OperatorKey:    h.operatorKey,
				RelativeExpiry: h.exitDelay,
				RoundID:        "oor_receive_systest",
				CommitmentTxID: arkTxid,
				Status:         clientvtxo.VTXOStatusLive,
			}
			h.materialized = append(h.materialized, desc)
		}

		return []clientoor.Event{
			&clientoor.IncomingHandledEvent{},
		}, nil

	case *clientoor.SendIncomingAckRequest:
		return []clientoor.Event{
			&clientoor.IncomingAckSentEvent{},
		}, nil

	default:
		return nil, nil
	}
}

var _ clientoor.OutboxHandler = (*incomingReceiveOutboxHandler)(nil)

// driveOutboxToFSM feeds outbox requests into a handler and recursively applies
// follow-up events to the FSM until no more outbox actions are emitted.
func driveOutboxToFSM(ctx context.Context, sessionID clientoor.SessionID,
	fsm *clientoor.StateMachine, handler clientoor.OutboxHandler,
	outbox []clientoor.OutboxEvent) error {

	for _, msg := range outbox {
		followUps, err := handler.Handle(ctx, sessionID, msg)
		if err != nil {
			return err
		}

		for _, evt := range followUps {
			fut := fsm.AskEvent(ctx, evt)
			result := fut.Await(ctx)
			if result.IsErr() {
				return result.Err()
			}

			next := result.UnwrapOr(nil)
			err = driveOutboxToFSM(ctx, sessionID, fsm, handler, next)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// checkpointSignInput captures the client-side signing metadata required to
// attach a client signature to a co-signed checkpoint PSBT.
type checkpointSignInput struct {
	Outpoint wire.OutPoint

	ClientKey keychain.KeyDescriptor

	OperatorKey *btcec.PublicKey

	ExitDelay uint32
}

// signCheckpointPSBTs attaches the client signature to each checkpoint PSBT
// (input 0) using the standard collaborative VTXO leaf.
func signCheckpointPSBTs(signer input.Signer, inputs []checkpointSignInput,
	checkpoints []*psbt.Packet) error {

	switch {
	case signer == nil:
		return fmt.Errorf("signer must be provided")

	case len(checkpoints) == 0:
		return fmt.Errorf("checkpoint psbts must be provided")
	}

	inputByOutpoint := make(map[wire.OutPoint]*checkpointSignInput, len(inputs))
	for i := range inputs {
		in := inputs[i]
		inputByOutpoint[in.Outpoint] = &in
	}

	for i := range checkpoints {
		checkpoint := checkpoints[i]
		if checkpoint == nil || checkpoint.UnsignedTx == nil {
			return fmt.Errorf("checkpoint psbt must include unsigned tx")
		}

		if len(checkpoint.UnsignedTx.TxIn) != 1 ||
			len(checkpoint.Inputs) != 1 {

			return fmt.Errorf("checkpoint must have exactly one input")
		}

		prevOutpoint := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
		in := inputByOutpoint[prevOutpoint]
		if in == nil {
			return fmt.Errorf("missing signing input for outpoint %s",
				prevOutpoint)
		}

		if in.ClientKey.PubKey == nil || in.OperatorKey == nil {
			return fmt.Errorf("missing pubkeys for checkpoint input")
		}

		tapscript, err := scripts.VTXOTapScript(
			in.ClientKey.PubKey, in.OperatorKey, in.ExitDelay,
		)
		if err != nil {
			return fmt.Errorf("derive vtxo tapscript: %w", err)
		}

		prevOut := checkpoint.Inputs[0].WitnessUtxo
		if prevOut == nil {
			return fmt.Errorf("checkpoint must include witness utxo")
		}

		prevFetcher := txscript.NewCannedPrevOutputFetcher(
			prevOut.PkScript, prevOut.Value,
		)
		sigHashes := txscript.NewTxSigHashes(
			checkpoint.UnsignedTx, prevFetcher,
		)

		signDesc, spendInfo, err := clienttx.NewVTXOCollabSignDescriptor(
			&clienttx.VTXOSpendContext{
				Outpoint:  prevOutpoint,
				Output:    prevOut,
				TapScript: tapscript,
			},
			in.ClientKey,
			0,
			sigHashes,
			prevFetcher,
		)
		if err != nil {
			return fmt.Errorf("build sign descriptor: %w", err)
		}

		sig, err := signer.SignOutputRaw(checkpoint.UnsignedTx, signDesc)
		if err != nil {
			return fmt.Errorf("sign output: %w", err)
		}

		err = addTapLeafScript(&checkpoint.Inputs[0], spendInfo)
		if err != nil {
			return err
		}

		err = addTaprootScriptSpendSig(
			&checkpoint.Inputs[0],
			in.ClientKey.PubKey,
			spendInfo.WitnessScript,
			sig.Serialize(),
			signDesc.HashType,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// addTapLeafScript ensures the checkpoint PSBT input includes the leaf script
// and control block for the collaborative VTXO leaf.
func addTapLeafScript(in *psbt.PInput, spendInfo *scripts.VTXOSpendData) error {
	if in == nil {
		return fmt.Errorf("psbt input must be provided")
	}

	if spendInfo == nil {
		return fmt.Errorf("spend info must be provided")
	}

	needle := &psbt.TaprootTapLeafScript{
		ControlBlock: spendInfo.ControlBlock,
		Script:       spendInfo.WitnessScript,
		LeafVersion:  txscript.BaseLeafVersion,
	}

	for i := range in.TaprootLeafScript {
		existing := in.TaprootLeafScript[i]
		if existing == nil {
			continue
		}

		if bytes.Equal(existing.ControlBlock, needle.ControlBlock) &&
			bytes.Equal(existing.Script, needle.Script) &&
			existing.LeafVersion == needle.LeafVersion {

			return nil
		}
	}

	in.TaprootLeafScript = append(in.TaprootLeafScript, needle)

	return nil
}

// addTaprootScriptSpendSig adds/replaces a taproot script spend signature in
// the PSBT input.
func addTaprootScriptSpendSig(in *psbt.PInput, pubKey *btcec.PublicKey,
	leafScript []byte, sig []byte,
	sigHash txscript.SigHashType) error {

	switch {
	case in == nil:
		return fmt.Errorf("psbt input must be provided")

	case pubKey == nil:
		return fmt.Errorf("pubkey must be provided")

	case len(leafScript) == 0:
		return fmt.Errorf("leaf script must be provided")

	case len(sig) == 0:
		return fmt.Errorf("signature must be provided")
	}

	leafHash := txscript.NewBaseTapLeaf(leafScript).TapHash()
	leafHashBytes := make([]byte, 0, len(leafHash))
	leafHashBytes = append(leafHashBytes, leafHash[:]...)

	needle := &psbt.TaprootScriptSpendSig{
		XOnlyPubKey: schnorr.SerializePubKey(pubKey),
		LeafHash:    leafHashBytes,
		Signature:   sig,
		SigHash:     sigHash,
	}

	for i := range in.TaprootScriptSpendSig {
		existing := in.TaprootScriptSpendSig[i]
		if existing == nil {
			continue
		}

		if existing.EqualKey(needle) {
			existing.Signature = needle.Signature
			existing.SigHash = needle.SigHash

			return nil
		}
	}

	in.TaprootScriptSpendSig = append(in.TaprootScriptSpendSig, needle)

	return nil
}

// oorClientToServerOutbox is a test-only adaptor that connects the client OOR
// FSM outbox messages to the server OOR coordinator actor, without RPC.
//
// Unlike the pure in-process adaptor, this variant uses real signer backends
// (LND signrpc) and captures the finalized checkpoint PSBT so the test can
// broadcast it to bitcoind.
type oorClientToServerOutbox struct {
	t *testing.T

	server *serveroor.Actor

	senderSigner input.Signer

	serverSignDescs []serveroor.VTXOSigningDescriptor

	signingInputs []checkpointSignInput

	finalCheckpointPSBTs []*psbt.Packet

	coSignedBeforeClientSign [][]byte
}

// Handle processes a client outbox request and returns follow-up events.
func (h *oorClientToServerOutbox) Handle(ctx context.Context,
	sessionID clientoor.SessionID,
	outbox clientoor.OutboxEvent) ([]clientoor.Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *clientoor.RequestArkSignatures:
		return []clientoor.Event{
			&clientoor.ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *clientoor.SendSubmitPackageRequest:
		resp := h.server.Receive(ctx, &serveroor.SubmitOORRequest{
			ArkPSBT:                msg.ArkPSBT,
			CheckpointPSBTs:        msg.CheckpointPSBTs,
			VTXOSigningDescriptors: h.serverSignDescs,
		})
		if resp.IsErr() {
			return nil, resp.Err()
		}

		unwrapped := resp.UnwrapOr(nil)
		serverMsg, ok := unwrapped.(*serveroor.SubmitOORResponse)
		require.True(h.t, ok)

		return []clientoor.Event{
			&clientoor.SubmitAcceptedEvent{
				SessionID: clientoor.SessionID(serverMsg.SessionID),
				ArkPSBT:   msg.ArkPSBT,
				CoSignedCheckpointPSBTs: serverMsg.
					CoSignedCheckpointPSBTs,
			},
		}, nil

	case *clientoor.RequestCheckpointSignatures:
		coSigned := make([][]byte, 0, len(msg.CoSignedCheckpointPSBTs))
		for i := range msg.CoSignedCheckpointPSBTs {
			raw, err := oorSerializePSBT(
				msg.CoSignedCheckpointPSBTs[i],
			)
			require.NoError(h.t, err)

			coSigned = append(coSigned, raw)
		}

		h.coSignedBeforeClientSign = coSigned

		err := signCheckpointPSBTs(
			h.senderSigner, h.signingInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []clientoor.Event{
			&clientoor.CheckpointsSignedEvent{
				FinalCheckpointPSBTs: msg.CoSignedCheckpointPSBTs,
			},
		}, nil

	case *clientoor.SendFinalizePackageRequest:
		if msg.ArkPSBT == nil || msg.ArkPSBT.UnsignedTx == nil {
			return nil, fmt.Errorf("ark psbt must be provided")
		}

		h.finalCheckpointPSBTs = msg.FinalCheckpointPSBTs

		resp := h.server.Receive(ctx, &serveroor.FinalizeOORRequest{
			SessionID: serveroor.SessionID(sessionID),
			FinalCheckpointPSBTs: msg.
				FinalCheckpointPSBTs,
		})
		if resp.IsErr() {
			return nil, resp.Err()
		}

		_, ok := resp.UnwrapOr(nil).(*serveroor.FinalizeOORResponse)
		require.True(h.t, ok)

		return []clientoor.Event{&clientoor.FinalizeAcceptedEvent{}}, nil

	case *clientoor.MarkInputsSpentRequest:
		return nil, fmt.Errorf("unexpected MarkInputsSpentRequest in " +
			"transport adaptor (missing persistence handler)")

	default:
		return nil, nil
	}
}

var _ clientoor.OutboxHandler = (*oorClientToServerOutbox)(nil)

// dropSubmitAcceptedOutbox is a test-only outbox adaptor that simulates a crash
// after the server has co-signed a submit package but before the client
// receives SubmitAcceptedEvent.
type dropSubmitAcceptedOutbox struct {
	t *testing.T

	server *serveroor.Actor

	serverSignDescs []serveroor.VTXOSigningDescriptor

	coSignedCheckpointBytes [][]byte
}

// Handle processes only submit requests and drops the response.
func (h *dropSubmitAcceptedOutbox) Handle(ctx context.Context,
	sessionID clientoor.SessionID,
	outbox clientoor.OutboxEvent) ([]clientoor.Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *clientoor.RequestArkSignatures:
		return []clientoor.Event{
			&clientoor.ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *clientoor.SendSubmitPackageRequest:
		resp := h.server.Receive(ctx, &serveroor.SubmitOORRequest{
			ArkPSBT:                msg.ArkPSBT,
			CheckpointPSBTs:        msg.CheckpointPSBTs,
			VTXOSigningDescriptors: h.serverSignDescs,
		})
		if resp.IsErr() {
			return nil, resp.Err()
		}

		unwrapped := resp.UnwrapOr(nil)
		serverMsg, ok := unwrapped.(*serveroor.SubmitOORResponse)
		require.True(h.t, ok)

		raw := make([][]byte, 0, len(serverMsg.CoSignedCheckpointPSBTs))
		for i := range serverMsg.CoSignedCheckpointPSBTs {
			b, err := oorSerializePSBT(
				serverMsg.CoSignedCheckpointPSBTs[i],
			)
			require.NoError(h.t, err)

			raw = append(raw, b)
		}
		h.coSignedCheckpointBytes = raw

		// Drop the response to simulate a crash/lost RPC response.
		return nil, nil

	default:
		return nil, fmt.Errorf("unexpected outbox type: %T", msg)
	}
}

var _ clientoor.OutboxHandler = (*dropSubmitAcceptedOutbox)(nil)

// TestOORClientServerCheckpointE2E drives the client-side outgoing transfer FSM
// against the server coordinator actor, using a real regtest chain and real
// signer backends, and confirms the finalized checkpoint tx.
func TestOORClientServerCheckpointE2E(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := t.Context()

	senderLND := h.PrimaryLND()
	operatorLND := h.ServerLND()

	senderKeyDesc, err := senderLND.WalletKit.DeriveNextKey(
		ctx, int32(987_100),
	)
	require.NoError(t, err)

	operatorKeyDesc := h.OperatorPubKey()
	require.NotNil(t, operatorKeyDesc)

	senderSigner := NewLNDRPCSigner(senderLND.Signer, 30*time.Second)
	operatorSigner := NewLNDRPCSigner(operatorLND.Signer, 30*time.Second)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKeyDesc.PubKey,
		CSVDelay:    oorExitDelay,
	}

	inputValue := btcutil.Amount(50_000)
	senderKey := keychain.KeyDescriptor{
		KeyLocator: senderKeyDesc.KeyLocator,
		PubKey:     senderKeyDesc.PubKey,
	}
	operatorKey := keychain.KeyDescriptor{
		KeyLocator: operatorKeyDesc.KeyLocator,
		PubKey:     operatorKeyDesc.PubKey,
	}
	minted := oorMintRealVTXO(
		t, h, operatorSigner, operatorKey, senderKey, oorExitDelay,
		inputValue,
	)

	inputs := []clientoor.TransferInput{minted.TransferInput()}
	vtxoDescs := []*clientvtxo.Descriptor{minted.VTXO}

	recipientKeyDesc, err := senderLND.WalletKit.DeriveNextKey(
		ctx, int32(987_102),
	)
	require.NoError(t, err)

	recipients := []oortx.RecipientOutput{
		{
			PkScript: oorVTXOPkScript(
				t,
				recipientKeyDesc.PubKey,
				operatorKeyDesc.PubKey,
				oorExitDelay,
			),
			Value: inputValue,
		},
	}

	sqlStore := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	locker := db.NewVTXOLockerDB(dbStore, btclog.Disabled)
	store := dbStore.NewVTXORecordStore()

	driver := serveroor.NewDriver(serveroor.DriverCfg{
		Locker:         locker,
		Store:          store,
		OperatorSigner: operatorSigner,
		OperatorKey: keychain.KeyDescriptor{
			KeyLocator: operatorKeyDesc.KeyLocator,
			PubKey:     operatorKeyDesc.PubKey,
		},
	})

	server := serveroor.NewActor(serveroor.ActorCfg{
		CheckpointPolicy: policy,
		OutboxHandler:    driver,
		DeliveryStore:    newServerDeliveryStore(t),
	})

	err = server.Start(ctx)
	require.NoError(t, err)
	defer server.Stop()

	err = store.Create(ctx, &vtxo.Record{
		Outpoint: minted.Outpoint,
		Value:    int64(inputValue),
		PkScript: minted.VTXO.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	adaptor := &oorClientToServerOutbox{
		t:            t,
		server:       server,
		senderSigner: senderSigner,
		serverSignDescs: []serveroor.VTXOSigningDescriptor{{
			Outpoint:  minted.Outpoint,
			OwnerKey:  minted.VTXO.ClientKey.PubKey,
			ExitDelay: minted.VTXO.RelativeExpiry,
		}},
		signingInputs: []checkpointSignInput{{
			Outpoint:    minted.Outpoint,
			ClientKey:   minted.VTXO.ClientKey,
			OperatorKey: minted.VTXO.OperatorKey,
			ExitDelay:   minted.VTXO.RelativeExpiry,
		}},
	}

	clientStore := newClientVTXOStore(t, vtxoDescs)
	client := clientoor.NewOORClientActor(clientoor.ClientActorCfg{
		OutboxHandler: &clientVTXOOutboxHandler{
			store: clientStore,
			next:  adaptor,
		},
		DeliveryStore: newClientDeliveryStore(t),
	})

	startResp := client.Receive(ctx, &clientoor.StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	_, ok := startResp.UnwrapOr(nil).(*clientoor.StartTransferResponse)
	require.True(t, ok)

	localVTXO, err := clientStore.GetVTXO(ctx, inputs[0].VTXO.Outpoint)
	require.NoError(t, err)
	require.Equal(t, clientvtxo.VTXOStatusSpent, localVTXO.Status)

	require.Len(t, adaptor.finalCheckpointPSBTs, 1)
	finalCheckpoint := adaptor.finalCheckpointPSBTs[0]
	require.NotNil(t, finalCheckpoint)

	err = psbt.MaybeFinalizeAll(finalCheckpoint)
	require.NoError(t, err)

	checkpointTx, err := psbt.Extract(finalCheckpoint)
	require.NoError(t, err)
	require.NotNil(t, checkpointTx)

	// The v0 OOR primitives build a fee-less checkpoint tx. Since bitcoind's
	// mempool policy rejects 0-fee txs, we CPFP it by submitting a package:
	//
	//   submitpackage {checkpoint, cpfp-child}
	//
	// The child spends the checkpoint output via the owner leaf. For this test,
	// the leaf is OP_TRUE so no signature is required.
	bitcoind, err := h.BitcoindClient()
	require.NoError(t, err)

	ownerLeafScript := inputs[0].OwnerLeafScript

	checkpointTapscript, err := scripts.CheckpointTapScript(
		policy, ownerLeafScript,
	)
	require.NoError(t, err)

	checkpointInternalKey := &scripts.ARKNUMSKey

	cpLeaf := txscript.NewBaseTapLeaf(ownerLeafScript)
	tree := txscript.AssembleTaprootScriptTree(checkpointTapscript.Leaves...)
	proofIdx, ok := tree.LeafProofIndex[cpLeaf.TapHash()]
	require.True(t, ok)
	proof := tree.LeafMerkleProofs[proofIdx]
	ctrl := proof.ToControlBlock(checkpointInternalKey)
	ctrlBytes, err := ctrl.ToBytes()
	require.NoError(t, err)

	cpfpKeyDesc, err := operatorLND.WalletKit.DeriveNextKey(
		ctx, int32(987_103),
	)
	require.NoError(t, err)

	cpfpAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		btcutil.Hash160(cpfpKeyDesc.PubKey.SerializeCompressed()),
		oorChainParams,
	)
	require.NoError(t, err)

	cpfpPkScript, err := txscript.PayToAddrScript(cpfpAddr)
	require.NoError(t, err)

	const cpfpFeeSat = int64(5_000)
	cpfpChange := checkpointTx.TxOut[0].Value - cpfpFeeSat
	require.Greater(t, cpfpChange, int64(0),
		"checkpoint value too small for cpfp fee",
	)

	child := wire.NewMsgTx(3)
	child.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointTx.TxHash(),
			Index: 0,
		},
		Sequence: wire.MaxTxInSequenceNum,
	})
	child.AddTxOut(&wire.TxOut{
		Value:    cpfpChange,
		PkScript: cpfpPkScript,
	})
	child.TxIn[0].Witness = wire.TxWitness{
		ownerLeafScript,
		ctrlBytes,
	}

	pkgResult, err := bitcoind.SubmitPackage(
		[]*wire.MsgTx{checkpointTx}, child, nil,
	)
	require.NoError(t, err)
	require.Equal(t, "success", pkgResult.PackageMsg)

	blocks := h.Harness.GenerateAndWait(1)
	require.NotEmpty(t, blocks)
}

// TestOORAliceBobRoundTripE2E verifies a true wallet-to-wallet OOR round-trip:
// 1) Alice sends to Bob and Bob completes incoming receive.
// 2) Bob sends that received output back to Alice and Alice completes receive.
func TestOORAliceBobRoundTripE2E(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := t.Context()

	aliceLND := h.PrimaryLND()
	operatorLND := h.ServerLND()
	bobInstance := h.StartClientLND("bob")
	bobLND := bobInstance.Client

	aliceKeyDesc, err := aliceLND.WalletKit.DeriveNextKey(
		ctx, int32(987_150),
	)
	require.NoError(t, err)

	bobKeyDesc, err := bobLND.WalletKit.DeriveNextKey(
		ctx, int32(987_151),
	)
	require.NoError(t, err)

	operatorKeyDesc := h.OperatorPubKey()
	require.NotNil(t, operatorKeyDesc)

	aliceSigner := NewLNDRPCSigner(aliceLND.Signer, 30*time.Second)
	operatorSigner := NewLNDRPCSigner(operatorLND.Signer, 30*time.Second)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKeyDesc.PubKey,
		CSVDelay:    oorExitDelay,
	}

	inputValue := btcutil.Amount(50_000)
	aliceKey := keychain.KeyDescriptor{
		KeyLocator: aliceKeyDesc.KeyLocator,
		PubKey:     aliceKeyDesc.PubKey,
	}
	operatorKey := keychain.KeyDescriptor{
		KeyLocator: operatorKeyDesc.KeyLocator,
		PubKey:     operatorKeyDesc.PubKey,
	}
	minted := oorMintRealVTXO(
		t, h, operatorSigner, operatorKey, aliceKey, oorExitDelay,
		inputValue,
	)

	inputs := []clientoor.TransferInput{minted.TransferInput()}
	vtxoDescs := []*clientvtxo.Descriptor{minted.VTXO}

	recipients := []oortx.RecipientOutput{
		{
			PkScript: oorVTXOPkScript(
				t,
				bobKeyDesc.PubKey,
				operatorKeyDesc.PubKey,
				oorExitDelay,
			),
			Value: inputValue,
		},
	}

	sqlStore := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	locker := db.NewVTXOLockerDB(dbStore, btclog.Disabled)
	store := dbStore.NewVTXORecordStore()
	sessionStore := serveroor.NewDBSessionStore(
		dbStore, clock.NewDefaultClock(), btclog.Disabled,
	)
	recipientEvents := serveroor.NewDBRecipientEventStore(
		dbStore, clock.NewDefaultClock(), btclog.Disabled,
	)

	driver := serveroor.NewDriver(serveroor.DriverCfg{
		Locker:          locker,
		Store:           store,
		SessionStore:    sessionStore,
		RecipientEvents: recipientEvents,
		OperatorSigner:  operatorSigner,
		OperatorKey: keychain.KeyDescriptor{
			KeyLocator: operatorKeyDesc.KeyLocator,
			PubKey:     operatorKeyDesc.PubKey,
		},
	})

	server := serveroor.NewActor(serveroor.ActorCfg{
		CheckpointPolicy: policy,
		OutboxHandler:    driver,
		DeliveryStore:    newServerDeliveryStore(t),
	})

	err = server.Start(ctx)
	require.NoError(t, err)
	defer server.Stop()

	err = store.Create(ctx, &vtxo.Record{
		Outpoint: minted.Outpoint,
		Value:    int64(inputValue),
		PkScript: minted.VTXO.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	adaptor := &oorClientToServerOutbox{
		t:            t,
		server:       server,
		senderSigner: aliceSigner,
		serverSignDescs: []serveroor.VTXOSigningDescriptor{{
			Outpoint:  minted.Outpoint,
			OwnerKey:  minted.VTXO.ClientKey.PubKey,
			ExitDelay: minted.VTXO.RelativeExpiry,
		}},
		signingInputs: []checkpointSignInput{{
			Outpoint:    minted.Outpoint,
			ClientKey:   minted.VTXO.ClientKey,
			OperatorKey: minted.VTXO.OperatorKey,
			ExitDelay:   minted.VTXO.RelativeExpiry,
		}},
	}

	aliceStore := newClientVTXOStore(t, vtxoDescs)
	aliceClient := clientoor.NewOORClientActor(clientoor.ClientActorCfg{
		OutboxHandler: &clientVTXOOutboxHandler{
			store: aliceStore,
			next:  adaptor,
		},
		DeliveryStore: newClientDeliveryStore(t),
	})

	startResp := aliceClient.Receive(ctx, &clientoor.StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.Truef(t, startResp.IsOk(), "start transfer failed: %v",
		startResp.Err(),
	)

	startMsg, ok := startResp.UnwrapOr(nil).(*clientoor.StartTransferResponse)
	require.True(t, ok)

	// The server cleans up finalized sessions from its in-memory map,
	// so we derive the session ID from the client response instead.
	sessionID := serveroor.SessionID(startMsg.SessionID)

	aliceVTXO, err := aliceStore.GetVTXO(ctx, inputs[0].VTXO.Outpoint)
	require.NoError(t, err)
	require.Equal(t, clientvtxo.VTXOStatusSpent, aliceVTXO.Status)

	events, err := recipientEvents.ListRecipientEvents(
		ctx, recipients[0].PkScript, 0, 10,
	)
	require.NoError(t, err)
	require.Len(t, events, 1)

	event := events[0]
	require.Equal(t, sessionID, event.SessionID)
	require.Equal(t, recipients[0].PkScript, event.RecipientPkScript)
	require.Equal(t, inputValue, event.Value)
	require.NotNil(t, event.ArkPSBT)

	bobReceive := &incomingReceiveOutboxHandler{
		t: t,
		recipientKey: keychain.KeyDescriptor{
			KeyLocator: bobKeyDesc.KeyLocator,
			PubKey:     bobKeyDesc.PubKey,
		},
		operatorKey: operatorKeyDesc.PubKey,
		exitDelay:   oorExitDelay,
	}

	receiveSession, receiveOutbox, err := clientoor.DriveIncomingTransfer(
		ctx, clientoor.SessionID(event.SessionID), event.ArkPSBT,
	)
	require.NoError(t, err)

	err = driveOutboxToFSM(
		ctx, receiveSession.ID, receiveSession.FSM, bobReceive, receiveOutbox,
	)
	require.NoError(t, err)

	recvState, err := receiveSession.FSM.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &clientoor.ReceiveCompleted{}, recvState)

	require.Len(t, bobReceive.materialized, 1)
	received := bobReceive.materialized[0]
	require.Equal(t, recipients[0].PkScript, received.PkScript)
	require.Equal(t, inputValue, received.Amount)
	require.Equal(t, clientvtxo.VTXOStatusLive, received.Status)

	// Round-trip leg: Bob sends the received output back to Alice.
	aliceReceiveKeyDesc, err := aliceLND.WalletKit.DeriveNextKey(
		ctx, int32(987_152),
	)
	require.NoError(t, err)

	require.NotNil(t, event.ArkPSBT)
	require.NotNil(t, event.ArkPSBT.UnsignedTx)
	require.Less(t, int(event.OutputIndex), len(event.ArkPSBT.UnsignedTx.TxOut))

	bobOutpoint := wire.OutPoint{
		Hash:  event.ArkPSBT.UnsignedTx.TxHash(),
		Index: event.OutputIndex,
	}
	bobPrevOut := event.ArkPSBT.UnsignedTx.TxOut[event.OutputIndex]
	require.NotNil(t, bobPrevOut)

	bobTapScript, err := scripts.VTXOTapScript(
		bobKeyDesc.PubKey, operatorKeyDesc.PubKey, oorExitDelay,
	)
	require.NoError(t, err)

	bobDesc := &clientvtxo.Descriptor{
		Outpoint: bobOutpoint,
		Amount:   btcutil.Amount(bobPrevOut.Value),
		PkScript: bobPrevOut.PkScript,
		ClientKey: keychain.KeyDescriptor{
			KeyLocator: bobKeyDesc.KeyLocator,
			PubKey:     bobKeyDesc.PubKey,
		},
		OperatorKey:    operatorKeyDesc.PubKey,
		TapScript:      bobTapScript,
		RelativeExpiry: oorExitDelay,
		Status:         clientvtxo.VTXOStatusLive,
	}

	bobStore := newClientVTXOStore(t, []*clientvtxo.Descriptor{bobDesc})
	bobSigner := NewLNDRPCSigner(bobLND.Signer, 30*time.Second)

	returnInputs := []clientoor.TransferInput{{
		VTXO:            bobDesc,
		OwnerLeafScript: []byte{txscript.OP_1},
	}}

	returnRecipients := []oortx.RecipientOutput{
		{
			PkScript: oorVTXOPkScript(
				t,
				aliceReceiveKeyDesc.PubKey,
				operatorKeyDesc.PubKey,
				oorExitDelay,
			),
			Value: inputValue,
		},
	}

	returnAdaptor := &oorClientToServerOutbox{
		t:            t,
		server:       server,
		senderSigner: bobSigner,
		serverSignDescs: []serveroor.VTXOSigningDescriptor{{
			Outpoint:  bobOutpoint,
			OwnerKey:  bobKeyDesc.PubKey,
			ExitDelay: oorExitDelay,
		}},
		signingInputs: []checkpointSignInput{{
			Outpoint: bobOutpoint,
			ClientKey: keychain.KeyDescriptor{
				KeyLocator: bobKeyDesc.KeyLocator,
				PubKey:     bobKeyDesc.PubKey,
			},
			OperatorKey: operatorKeyDesc.PubKey,
			ExitDelay:   oorExitDelay,
		}},
	}

	bobClient := clientoor.NewOORClientActor(clientoor.ClientActorCfg{
		OutboxHandler: &clientVTXOOutboxHandler{
			store: bobStore,
			next:  returnAdaptor,
		},
		DeliveryStore: newClientDeliveryStore(t),
	})

	returnResp := bobClient.Receive(ctx, &clientoor.StartTransferRequest{
		Policy:     policy,
		Inputs:     returnInputs,
		Recipients: returnRecipients,
	})
	require.Truef(t, returnResp.IsOk(), "return transfer failed: %v",
		returnResp.Err(),
	)

	returnMsg, ok := returnResp.UnwrapOr(nil).(*clientoor.StartTransferResponse)
	require.True(t, ok)

	returnSessionID := serveroor.SessionID(returnMsg.SessionID)

	bobSpent, err := bobStore.GetVTXO(ctx, bobOutpoint)
	require.NoError(t, err)
	require.Equal(t, clientvtxo.VTXOStatusSpent, bobSpent.Status)

	aliceEvents, err := recipientEvents.ListRecipientEvents(
		ctx, returnRecipients[0].PkScript, 0, 10,
	)
	require.NoError(t, err)
	require.Len(t, aliceEvents, 1)

	aliceEvent := aliceEvents[0]
	require.Equal(t, returnSessionID, aliceEvent.SessionID)
	require.Equal(t, returnRecipients[0].PkScript, aliceEvent.RecipientPkScript)
	require.Equal(t, inputValue, aliceEvent.Value)
	require.NotNil(t, aliceEvent.ArkPSBT)

	aliceReceive := &incomingReceiveOutboxHandler{
		t: t,
		recipientKey: keychain.KeyDescriptor{
			KeyLocator: aliceReceiveKeyDesc.KeyLocator,
			PubKey:     aliceReceiveKeyDesc.PubKey,
		},
		operatorKey: operatorKeyDesc.PubKey,
		exitDelay:   oorExitDelay,
	}

	aliceReceiveSession, aliceReceiveOutbox, err := clientoor.DriveIncomingTransfer(
		ctx, clientoor.SessionID(aliceEvent.SessionID), aliceEvent.ArkPSBT,
	)
	require.NoError(t, err)

	err = driveOutboxToFSM(
		ctx, aliceReceiveSession.ID, aliceReceiveSession.FSM,
		aliceReceive, aliceReceiveOutbox,
	)
	require.NoError(t, err)

	aliceRecvState, err := aliceReceiveSession.FSM.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &clientoor.ReceiveCompleted{}, aliceRecvState)

	require.Len(t, aliceReceive.materialized, 1)
	receivedBack := aliceReceive.materialized[0]
	require.Equal(t, returnRecipients[0].PkScript, receivedBack.PkScript)
	require.Equal(t, inputValue, receivedBack.Amount)
	require.Equal(t, clientvtxo.VTXOStatusLive, receivedBack.Status)
}

// TestOORClientResumeAfterServerCoSignE2E simulates the mobile-safety edge where
// the server co-signs a submit package but the client crashes before observing
// SubmitAcceptedEvent, then resumes and completes to finalization.
func TestOORClientResumeAfterServerCoSignE2E(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := t.Context()

	senderLND := h.PrimaryLND()
	operatorLND := h.ServerLND()

	// Derive deterministic keys so the test remains stable across runs.
	senderKeyDesc, err := senderLND.WalletKit.DeriveNextKey(
		ctx, int32(987_200),
	)
	require.NoError(t, err)

	operatorKeyDesc := h.OperatorPubKey()
	require.NotNil(t, operatorKeyDesc)

	// Use real signers so we exercise PSBT signing paths end-to-end.
	senderSigner := NewLNDRPCSigner(senderLND.Signer, 30*time.Second)
	operatorSigner := NewLNDRPCSigner(operatorLND.Signer, 30*time.Second)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKeyDesc.PubKey,
		CSVDelay:    oorExitDelay,
	}

	inputValue := btcutil.Amount(50_000)
	senderKey := keychain.KeyDescriptor{
		KeyLocator: senderKeyDesc.KeyLocator,
		PubKey:     senderKeyDesc.PubKey,
	}
	operatorKey := keychain.KeyDescriptor{
		KeyLocator: operatorKeyDesc.KeyLocator,
		PubKey:     operatorKeyDesc.PubKey,
	}
	minted := oorMintRealVTXO(
		t, h, operatorSigner, operatorKey, senderKey, oorExitDelay,
		inputValue,
	)

	inputs := []clientoor.TransferInput{minted.TransferInput()}
	vtxoDescs := []*clientvtxo.Descriptor{minted.VTXO}

	recipientKeyDesc, err := senderLND.WalletKit.DeriveNextKey(
		ctx, int32(987_202),
	)
	require.NoError(t, err)

	recipients := []oortx.RecipientOutput{
		{
			PkScript: oorVTXOPkScript(
				t,
				recipientKeyDesc.PubKey,
				operatorKeyDesc.PubKey,
				oorExitDelay,
			),
			Value: inputValue,
		},
	}

	// Create a minimal in-process server actor. The outbox driver does signing
	// and persistence locally so the test can focus on the state-machine
	// semantics.
	sqlStore := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	locker := db.NewVTXOLockerDB(dbStore, btclog.Disabled)
	store := dbStore.NewVTXORecordStore()

	driver := serveroor.NewDriver(serveroor.DriverCfg{
		Locker:         locker,
		Store:          store,
		OperatorSigner: operatorSigner,
		OperatorKey: keychain.KeyDescriptor{
			KeyLocator: operatorKeyDesc.KeyLocator,
			PubKey:     operatorKeyDesc.PubKey,
		},
	})

	server := serveroor.NewActor(serveroor.ActorCfg{
		CheckpointPolicy: policy,
		OutboxHandler:    driver,
		DeliveryStore:    newServerDeliveryStore(t),
	})

	err = server.Start(ctx)
	require.NoError(t, err)
	defer server.Stop()

	err = store.Create(ctx, &vtxo.Record{
		Outpoint: minted.Outpoint,
		Value:    int64(inputValue),
		PkScript: minted.VTXO.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	// 1) Start transfer using an adaptor that drops SubmitAcceptedEvent.
	//
	// This simulates the mobile-safety edge case:
	// - server has co-signed and persisted
	// - client did not receive the response
	// - client must resume later and still obtain the same co-signed bytes
	clientStore := newClientVTXOStore(t, vtxoDescs)

	dropper := &dropSubmitAcceptedOutbox{
		t:      t,
		server: server,
		serverSignDescs: []serveroor.VTXOSigningDescriptor{{
			Outpoint:  minted.Outpoint,
			OwnerKey:  minted.VTXO.ClientKey.PubKey,
			ExitDelay: minted.VTXO.RelativeExpiry,
		}},
	}

	client1 := clientoor.NewOORClientActor(clientoor.ClientActorCfg{
		OutboxHandler: &clientVTXOOutboxHandler{
			store: clientStore,
			next:  dropper,
		},
		DeliveryStore: newClientDeliveryStore(t),
	})

	startResp := client1.Receive(ctx, &clientoor.StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.Truef(t, startResp.IsOk(), "start transfer failed: %v",
		startResp.Err(),
	)

	startMsg, ok := startResp.UnwrapOr(nil).(*clientoor.StartTransferResponse)
	require.True(t, ok)
	require.NotEqual(t, clientoor.SessionID{}, startMsg.SessionID)

	require.NotEmpty(t, dropper.coSignedCheckpointBytes)

	// 2) Simulate crash: create a new actor and attempt the same deterministic
	// transfer again. The server should return the same co-signed checkpoint
	// bytes, and the client should complete to finalization.
	adaptor := &oorClientToServerOutbox{
		t:            t,
		server:       server,
		senderSigner: senderSigner,
		serverSignDescs: []serveroor.VTXOSigningDescriptor{{
			Outpoint:  minted.Outpoint,
			OwnerKey:  minted.VTXO.ClientKey.PubKey,
			ExitDelay: minted.VTXO.RelativeExpiry,
		}},
		signingInputs: []checkpointSignInput{{
			Outpoint:    minted.Outpoint,
			ClientKey:   minted.VTXO.ClientKey,
			OperatorKey: minted.VTXO.OperatorKey,
			ExitDelay:   minted.VTXO.RelativeExpiry,
		}},
	}

	client2 := clientoor.NewOORClientActor(clientoor.ClientActorCfg{
		OutboxHandler: &clientVTXOOutboxHandler{
			store: clientStore,
			next:  adaptor,
		},
		DeliveryStore: newClientDeliveryStore(t),
	})

	startResp2 := client2.Receive(ctx, &clientoor.StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.Truef(t, startResp2.IsOk(), "start transfer failed: %v",
		startResp2.Err(),
	)

	startMsg2, ok := startResp2.UnwrapOr(nil).(*clientoor.StartTransferResponse)
	require.True(t, ok)
	require.Equal(t, startMsg.SessionID, startMsg2.SessionID)

	stateResp := client2.Receive(ctx, &clientoor.GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*clientoor.GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &clientoor.Completed{}, stateMsg.State)

	localVTXO, err := clientStore.GetVTXO(ctx, inputs[0].VTXO.Outpoint)
	require.NoError(t, err)
	require.Equal(t, clientvtxo.VTXOStatusSpent, localVTXO.Status)

	require.Equal(t, dropper.coSignedCheckpointBytes,
		adaptor.coSignedBeforeClientSign,
	)

	require.Len(t, adaptor.finalCheckpointPSBTs, 1)
	finalCheckpoint := adaptor.finalCheckpointPSBTs[0]
	require.NotNil(t, finalCheckpoint)

	err = psbt.MaybeFinalizeAll(finalCheckpoint)
	require.NoError(t, err)

	checkpointTx, err := psbt.Extract(finalCheckpoint)
	require.NoError(t, err)
	require.NotNil(t, checkpointTx)

	// Confirm the finalized checkpoint tx by CPFP'ing it in a package. The child
	// spends via the owner OP_TRUE leaf, so no signatures required.
	bitcoind, err := h.BitcoindClient()
	require.NoError(t, err)

	ownerLeafScript := inputs[0].OwnerLeafScript

	checkpointTapscript, err := scripts.CheckpointTapScript(
		policy, ownerLeafScript,
	)
	require.NoError(t, err)

	checkpointInternalKey := &scripts.ARKNUMSKey

	cpLeaf := txscript.NewBaseTapLeaf(ownerLeafScript)
	tree := txscript.AssembleTaprootScriptTree(checkpointTapscript.Leaves...)
	proofIdx, ok := tree.LeafProofIndex[cpLeaf.TapHash()]
	require.True(t, ok)
	proof := tree.LeafMerkleProofs[proofIdx]
	ctrl := proof.ToControlBlock(checkpointInternalKey)
	ctrlBytes, err := ctrl.ToBytes()
	require.NoError(t, err)

	cpfpAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		btcutil.Hash160(operatorKeyDesc.PubKey.SerializeCompressed()),
		oorChainParams,
	)
	require.NoError(t, err)

	cpfpPkScript, err := txscript.PayToAddrScript(cpfpAddr)
	require.NoError(t, err)

	const cpfpFeeSat = int64(5_000)
	cpfpChange := checkpointTx.TxOut[0].Value - cpfpFeeSat
	require.Greater(t, cpfpChange, int64(0),
		"checkpoint value too small for cpfp fee",
	)

	child := wire.NewMsgTx(3)
	child.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointTx.TxHash(),
			Index: 0,
		},
		Sequence: wire.MaxTxInSequenceNum,
	})
	child.AddTxOut(&wire.TxOut{
		Value:    cpfpChange,
		PkScript: cpfpPkScript,
	})
	child.TxIn[0].Witness = wire.TxWitness{
		ownerLeafScript,
		ctrlBytes,
	}

	pkgResult, err := bitcoind.SubmitPackage(
		[]*wire.MsgTx{checkpointTx}, child, nil,
	)
	require.NoError(t, err)
	if pkgResult.PackageMsg != "success" {
		for wtxid, txRes := range pkgResult.TxResults {
			if txRes.Error == nil || *txRes.Error == "" {
				continue
			}

			t.Logf("submitpackage tx wtxid=%s txid=%s err=%s",
				wtxid, txRes.TxID.String(), *txRes.Error)
		}

		t.Fatalf("submitpackage failed: %s", pkgResult.PackageMsg)
	}

	blocks := h.Harness.GenerateAndWait(1)
	require.NotEmpty(t, blocks)
}

// TestOORClientResumeAfterServerRestartE2E simulates the same crash scenario as
// TestOORClientResumeAfterServerCoSignE2E, but also restarts the server
// coordinator after the point-of-no-return and verifies it can restore active
// sessions from its SessionStore.
func TestOORClientResumeAfterServerRestartE2E(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := t.Context()

	senderLND := h.PrimaryLND()
	operatorLND := h.ServerLND()

	senderKeyDesc, err := senderLND.WalletKit.DeriveNextKey(
		ctx, int32(987_300),
	)
	require.NoError(t, err)

	operatorKeyDesc := h.OperatorPubKey()
	require.NotNil(t, operatorKeyDesc)

	senderSigner := NewLNDRPCSigner(senderLND.Signer, 30*time.Second)
	operatorSigner := NewLNDRPCSigner(operatorLND.Signer, 30*time.Second)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKeyDesc.PubKey,
		CSVDelay:    oorExitDelay,
	}

	inputValue := btcutil.Amount(50_000)
	senderKey := keychain.KeyDescriptor{
		KeyLocator: senderKeyDesc.KeyLocator,
		PubKey:     senderKeyDesc.PubKey,
	}
	operatorKey := keychain.KeyDescriptor{
		KeyLocator: operatorKeyDesc.KeyLocator,
		PubKey:     operatorKeyDesc.PubKey,
	}
	minted := oorMintRealVTXO(
		t, h, operatorSigner, operatorKey, senderKey, oorExitDelay,
		inputValue,
	)

	inputs := []clientoor.TransferInput{minted.TransferInput()}
	vtxoDescs := []*clientvtxo.Descriptor{minted.VTXO}

	recipientKeyDesc, err := senderLND.WalletKit.DeriveNextKey(
		ctx, int32(987_302),
	)
	require.NoError(t, err)

	recipients := []oortx.RecipientOutput{
		{
			PkScript: oorVTXOPkScript(
				t,
				recipientKeyDesc.PubKey,
				operatorKeyDesc.PubKey,
				oorExitDelay,
			),
			Value: inputValue,
		},
	}

	dbh := db.NewTestDB(t)
	dbStore := db.NewStore(
		dbh.DB, dbh.Queries, dbh.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	locker := db.NewVTXOLockerDB(dbStore, btclog.Disabled)
	store := dbStore.NewVTXORecordStore()
	sessionStore1 := serveroor.NewDBSessionStore(dbStore, clock.NewDefaultClock(), btclog.Disabled)

	driver1 := serveroor.NewDriver(serveroor.DriverCfg{
		Locker:         locker,
		Store:          store,
		SessionStore:   sessionStore1,
		OperatorSigner: operatorSigner,
		OperatorKey: keychain.KeyDescriptor{
			KeyLocator: operatorKeyDesc.KeyLocator,
			PubKey:     operatorKeyDesc.PubKey,
		},
	})

	server1 := serveroor.NewActor(serveroor.ActorCfg{
		CheckpointPolicy: policy,
		OutboxHandler:    driver1,
		DeliveryStore:    newServerDeliveryStore(t),
		SessionStore:     sessionStore1,
	})

	err = server1.Start(ctx)
	require.NoError(t, err)
	defer server1.Stop()

	err = store.Create(ctx, &vtxo.Record{
		Outpoint: minted.Outpoint,
		Value:    int64(inputValue),
		PkScript: minted.VTXO.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	clientStore := newClientVTXOStore(t, vtxoDescs)

	dropper := &dropSubmitAcceptedOutbox{
		t:      t,
		server: server1,
		serverSignDescs: []serveroor.VTXOSigningDescriptor{{
			Outpoint:  minted.Outpoint,
			OwnerKey:  minted.VTXO.ClientKey.PubKey,
			ExitDelay: minted.VTXO.RelativeExpiry,
		}},
	}

	client1 := clientoor.NewOORClientActor(clientoor.ClientActorCfg{
		OutboxHandler: &clientVTXOOutboxHandler{
			store: clientStore,
			next:  dropper,
		},
		DeliveryStore: newClientDeliveryStore(t),
	})

	startResp := client1.Receive(ctx, &clientoor.StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*clientoor.StartTransferResponse)
	require.True(t, ok)
	require.NotEqual(t, clientoor.SessionID{}, startMsg.SessionID)

	serverSessionID := serveroor.SessionID(startMsg.SessionID)

	require.NotEmpty(t, dropper.coSignedCheckpointBytes)

	// Restart server: re-create actor, restore co-signed sessions from DB.
	server1.Stop()

	sessionStore2 := serveroor.NewDBSessionStore(dbStore, clock.NewDefaultClock(), btclog.Disabled)
	driver2 := serveroor.NewDriver(serveroor.DriverCfg{
		Locker:         locker,
		Store:          store,
		SessionStore:   sessionStore2,
		OperatorSigner: operatorSigner,
		OperatorKey: keychain.KeyDescriptor{
			KeyLocator: operatorKeyDesc.KeyLocator,
			PubKey:     operatorKeyDesc.PubKey,
		},
	})
	server2 := serveroor.NewActor(serveroor.ActorCfg{
		CheckpointPolicy: policy,
		OutboxHandler:    driver2,
		DeliveryStore:    newServerDeliveryStore(t),
		SessionStore:     sessionStore2,
	})
	defer server2.Stop()

	err = server2.Start(ctx)
	require.NoError(t, err)

	// Assert the server has rehydrated the session into the CoSigned point-of-no-
	// return state before any client resume traffic arrives.
	restoredSnapshots, err := sessionStore2.LoadActiveSessions(ctx)
	require.NoError(t, err)
	require.Len(t, restoredSnapshots, 1)
	require.Equal(t, serverSessionID, restoredSnapshots[0].SessionID)

	// Durable restart delivery is asynchronous. Wait until the restarted actor
	// has replayed its checkpoint and rehydrated the session in memory.
	var restoredState serveroor.State
	require.Eventually(t, func() bool {
		state, stateErr := server2.CurrentState(ctx, serverSessionID)
		if stateErr != nil {
			return false
		}

		restoredState = state
		return true
	}, 3*time.Second, 25*time.Millisecond)

	coSignedState, ok := restoredState.(*serveroor.CoSignedState)
	require.True(t, ok)
	require.NotEmpty(t, coSignedState.CoSignedCheckpointPSBTs)

	restoredCoSignedBytes := make([][]byte, 0,
		len(coSignedState.CoSignedCheckpointPSBTs),
	)
	for i := range coSignedState.CoSignedCheckpointPSBTs {
		b, err := oorSerializePSBT(
			coSignedState.CoSignedCheckpointPSBTs[i],
		)
		require.NoError(t, err)

		restoredCoSignedBytes = append(restoredCoSignedBytes, b)
	}
	require.Equal(t, dropper.coSignedCheckpointBytes, restoredCoSignedBytes)

	adaptor := &oorClientToServerOutbox{
		t:            t,
		server:       server2,
		senderSigner: senderSigner,
		serverSignDescs: []serveroor.VTXOSigningDescriptor{{
			Outpoint:  minted.Outpoint,
			OwnerKey:  minted.VTXO.ClientKey.PubKey,
			ExitDelay: minted.VTXO.RelativeExpiry,
		}},
		signingInputs: []checkpointSignInput{{
			Outpoint:    minted.Outpoint,
			ClientKey:   minted.VTXO.ClientKey,
			OperatorKey: minted.VTXO.OperatorKey,
			ExitDelay:   minted.VTXO.RelativeExpiry,
		}},
	}

	client2 := clientoor.NewOORClientActor(clientoor.ClientActorCfg{
		OutboxHandler: &clientVTXOOutboxHandler{
			store: clientStore,
			next:  adaptor,
		},
		DeliveryStore: newClientDeliveryStore(t),
	})

	startResp2 := client2.Receive(ctx, &clientoor.StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.Truef(t, startResp2.IsOk(), "start transfer failed: %v",
		startResp2.Err(),
	)

	startMsg2, ok := startResp2.UnwrapOr(nil).(*clientoor.StartTransferResponse)
	require.True(t, ok)
	require.Equal(t, startMsg.SessionID, startMsg2.SessionID)

	stateResp := client2.Receive(ctx, &clientoor.GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*clientoor.GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &clientoor.Completed{}, stateMsg.State)

	localVTXO, err := clientStore.GetVTXO(ctx, inputs[0].VTXO.Outpoint)
	require.NoError(t, err)
	require.Equal(t, clientvtxo.VTXOStatusSpent, localVTXO.Status)

	require.Equal(t, dropper.coSignedCheckpointBytes,
		adaptor.coSignedBeforeClientSign,
	)

	require.Len(t, adaptor.finalCheckpointPSBTs, 1)
	finalCheckpoint := adaptor.finalCheckpointPSBTs[0]
	require.NotNil(t, finalCheckpoint)

	err = psbt.MaybeFinalizeAll(finalCheckpoint)
	require.NoError(t, err)

	checkpointTx, err := psbt.Extract(finalCheckpoint)
	require.NoError(t, err)
	require.NotNil(t, checkpointTx)

	// Confirm the finalized checkpoint tx by CPFP'ing it in a package.
	bitcoind, err := h.BitcoindClient()
	require.NoError(t, err)

	ownerLeafScript := inputs[0].OwnerLeafScript

	checkpointTapscript, err := scripts.CheckpointTapScript(
		policy, ownerLeafScript,
	)
	require.NoError(t, err)

	checkpointInternalKey := &scripts.ARKNUMSKey

	cpLeaf := txscript.NewBaseTapLeaf(ownerLeafScript)
	tree := txscript.AssembleTaprootScriptTree(checkpointTapscript.Leaves...)
	proofIdx, ok := tree.LeafProofIndex[cpLeaf.TapHash()]
	require.True(t, ok)
	proof := tree.LeafMerkleProofs[proofIdx]
	ctrl := proof.ToControlBlock(checkpointInternalKey)
	ctrlBytes, err := ctrl.ToBytes()
	require.NoError(t, err)

	cpfpAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		btcutil.Hash160(operatorKeyDesc.PubKey.SerializeCompressed()),
		oorChainParams,
	)
	require.NoError(t, err)

	cpfpPkScript, err := txscript.PayToAddrScript(cpfpAddr)
	require.NoError(t, err)

	const cpfpFeeSat = int64(5_000)
	cpfpChange := checkpointTx.TxOut[0].Value - cpfpFeeSat
	require.Greater(t, cpfpChange, int64(0),
		"checkpoint value too small for cpfp fee",
	)

	child := wire.NewMsgTx(3)
	child.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointTx.TxHash(),
			Index: 0,
		},
		Sequence: wire.MaxTxInSequenceNum,
	})
	child.AddTxOut(&wire.TxOut{
		Value:    cpfpChange,
		PkScript: cpfpPkScript,
	})
	child.TxIn[0].Witness = wire.TxWitness{
		ownerLeafScript,
		ctrlBytes,
	}

	pkgResult, err := bitcoind.SubmitPackage(
		[]*wire.MsgTx{checkpointTx}, child, nil,
	)
	require.NoError(t, err)
	if pkgResult.PackageMsg != "success" {
		for wtxid, txRes := range pkgResult.TxResults {
			if txRes.Error == nil || *txRes.Error == "" {
				continue
			}

			t.Logf("submitpackage tx wtxid=%s txid=%s err=%s",
				wtxid, txRes.TxID.String(), *txRes.Error)
		}

		t.Fatalf("submitpackage failed: %s", pkgResult.PackageMsg)
	}

	blocks := h.Harness.GenerateAndWait(1)
	require.NotEmpty(t, blocks)
}
