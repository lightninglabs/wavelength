package oor_test

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	clientactor "github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/serverconn"
	clientvtxo "github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo/db"
	serveroor "github.com/lightninglabs/darepo/oor"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newClientTransferInput constructs a minimally valid transfer input for
// in-process E2E tests.
func newClientTransferInput(t *testing.T, clientKey *btcec.PrivateKey,
	operatorKey *btcec.PublicKey, exitDelay uint32,
	outpoint wire.OutPoint,
	amount btcutil.Amount) clientoor.TransferInput {

	t.Helper()

	tapKey, err := arkscript.VTXOTapKey(
		clientKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	tapscript, err := arkscript.VTXOTapScript(
		clientKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	ownerLeaf := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{
				clientKey.PubKey(), operatorKey,
			},
		},
	}
	ownerLeafScript, err := ownerLeaf.Script()
	require.NoError(t, err)

	ownerLeafPolicy, err := ownerLeaf.Encode()
	require.NoError(t, err)

	return clientoor.TransferInput{
		VTXO: &clientvtxo.Descriptor{
			Outpoint: outpoint,
			Amount:   amount,
			PkScript: pkScript,
			ClientKey: keychain.KeyDescriptor{
				PubKey: clientKey.PubKey(),
			},
			OperatorKey:    operatorKey,
			TapScript:      tapscript,
			RelativeExpiry: exitDelay,
			Status:         clientvtxo.VTXOStatusLive,
		},
		VTXOPolicyTemplate: mustStandardVTXOPolicyTemplate(
			t, clientKey.PubKey(), operatorKey, exitDelay,
		),
		OwnerLeafScript: ownerLeafScript,
		OwnerLeafPolicy: ownerLeafPolicy,
	}
}

// mustStandardVTXOPolicyTemplate encodes the canonical standard Ark VTXO
// policy used by tests.
func mustStandardVTXOPolicyTemplate(t *testing.T, ownerKey,
	operatorKey *btcec.PublicKey, exitDelay uint32) []byte {

	t.Helper()

	policy, err := arkscript.EncodeStandardVTXOTemplate(
		ownerKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	return policy
}

// mustStandardCollabSpendPath encodes the collaborative spend path for the
// canonical standard Ark VTXO test policy.
func mustStandardCollabSpendPath(t *testing.T, ownerKey,
	operatorKey *btcec.PublicKey, exitDelay uint32) []byte {

	t.Helper()

	policy, err := arkscript.NewVTXOPolicy(
		ownerKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	info, err := policy.CollabSpendInfo()
	require.NoError(t, err)

	path := &arkscript.SpendPath{
		SpendInfo: info,
	}
	raw, err := path.Encode()
	require.NoError(t, err)

	return raw
}

// finalizeCheckpointPSBTsForTest clones co-signed checkpoints and
// attaches client signatures so the finalize package is spendable and
// deterministic.
func finalizeCheckpointPSBTsForTest(t *testing.T,
	clientSigner input.Signer, inputs []clientoor.TransferInput,
	coSigned []*psbt.Packet) []*psbt.Packet {

	t.Helper()

	finalized := make([]*psbt.Packet, 0, len(coSigned))
	for _, pkt := range coSigned {
		require.NotNil(t, pkt)
		require.NotNil(t, pkt.UnsignedTx)

		raw, err := psbtutil.Serialize(pkt)
		require.NoError(t, err)

		finalPkt, err := psbtutil.Parse(raw)
		require.NoError(t, err)
		require.NotEmpty(t, finalPkt.Inputs)
		finalized = append(finalized, finalPkt)
	}

	err := clientoor.SignCheckpointPSBTs(clientSigner, inputs, finalized)
	require.NoError(t, err)

	return finalized
}

// startE2EServerActor creates and starts a server actor for E2E tests,
// registering cleanup to stop it when the test finishes.
func startE2EServerActor(t *testing.T,
	cfg serveroor.ActorCfg) *serveroor.Actor {

	t.Helper()

	server := serveroor.NewActor(cfg)

	err := server.Start(t.Context())
	require.NoError(t, err)

	t.Cleanup(server.Stop)

	return server
}

// attachArkLeafScriptsForTest attaches the owner leaf spend path to
// each Ark input so submit package validation can reconstruct a
// spendable witness.
//
// The Ark inputs are keyed by checkpoint txid while transfer inputs are
// keyed by spent VTXO outpoint. This helper bridges them through the
// checkpoint mapping and uses the canonical checkpoint output tap tree to
// derive the concrete tapleaf path.
func attachArkLeafScriptsForTest(t *testing.T, ark *psbt.Packet,
	transferInputs []clientoor.TransferInput,
	checkpoints []*psbt.Packet) {

	t.Helper()

	require.NotNil(t, ark)
	require.NotNil(t, ark.UnsignedTx)
	require.NotEmpty(t, transferInputs)
	require.NotEmpty(t, checkpoints)
	require.Len(t, ark.Inputs, len(ark.UnsignedTx.TxIn))

	inputBySpentOutpoint := make(
		map[wire.OutPoint]clientoor.TransferInput,
		len(transferInputs),
	)
	for i := range transferInputs {
		in := transferInputs[i]
		require.NoError(t, in.Validate())
		inputBySpentOutpoint[in.VTXO.Outpoint] = in
	}

	leafByCheckpointTxid := make(
		map[chainhash.Hash][]byte, len(checkpoints),
	)
	for i := range checkpoints {
		checkpoint := checkpoints[i]
		require.NotNil(t, checkpoint)
		require.NotNil(t, checkpoint.UnsignedTx)
		require.Len(t, checkpoint.UnsignedTx.TxIn, 1)

		spentOutpoint := checkpoint.UnsignedTx.
			TxIn[0].PreviousOutPoint

		transferInput, ok := inputBySpentOutpoint[spentOutpoint]
		require.True(
			t, ok,
			"missing transfer input for spent outpoint",
		)

		checkpointTxid := checkpoint.UnsignedTx.TxHash()
		leafByCheckpointTxid[checkpointTxid] = append(
			[]byte(nil),
			transferInput.OwnerLeafScript...,
		)
	}

	for i := range ark.Inputs {
		prevOut := ark.UnsignedTx.TxIn[i].PreviousOutPoint
		ownerLeaf, ok := leafByCheckpointTxid[prevOut.Hash]
		require.True(
			t, ok, "missing owner leaf for ark input",
		)

		checkpoint := checkpoints[i]
		require.NotNil(t, checkpoint)
		require.Greater(t, len(checkpoint.Outputs), 0)
		tapTreeEncoded := checkpoint.Outputs[0].TaprootTapTree
		require.NotEmpty(t, tapTreeEncoded)

		leaf, err := oortx.BuildTaprootTapLeafScript(
			tapTreeEncoded, ownerLeaf,
		)
		require.NoError(t, err)

		ark.Inputs[i].TaprootLeafScript =
			[]*psbt.TaprootTapLeafScript{leaf}
	}
}

// clonePSBTSliceForTest deep-copies a PSBT slice via serialize/parse
// so tests do not share mutable packet pointers across actor
// boundaries.
func clonePSBTSliceForTest(t *testing.T,
	packets []*psbt.Packet) []*psbt.Packet {

	t.Helper()

	clones := make([]*psbt.Packet, 0, len(packets))
	for i := range packets {
		raw, err := psbtutil.Serialize(packets[i])
		require.NoError(t, err)

		clone, err := psbtutil.Parse(raw)
		require.NoError(t, err)

		clones = append(clones, clone)
	}

	return clones
}

// localClientOutboxHandler handles only local client OOR outbox events. The
// transport edge is exercised through the ServerConn bridge instead.
type localClientOutboxHandler struct {
	t *testing.T

	clientSigner input.Signer
}

// Handle processes only local client-side outbox events.
func (h *localClientOutboxHandler) Handle(_ context.Context,
	_ clientoor.SessionID,
	outbox clientoor.OutboxEvent) ([]clientoor.Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *clientoor.RequestArkSignatures:
		err := clientoor.SignArkPSBT(
			h.clientSigner, msg.ArkPSBT,
			msg.CheckpointPSBTs, msg.TransferInputs,
		)
		require.NoError(h.t, err)

		return []clientoor.Event{
			&clientoor.ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *clientoor.RequestCheckpointSignatures:
		err := clientoor.SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []clientoor.Event{
			&clientoor.CheckpointsSignedEvent{
				FinalCheckpointPSBTs: msg.
					CoSignedCheckpointPSBTs,
			},
		}, nil

	case *clientoor.MarkInputsSpentRequest:
		return []clientoor.Event{
			&clientoor.InputsMarkedSpentEvent{},
		}, nil

	case *clientoor.SendSubmitPackageRequest,
		*clientoor.SendFinalizePackageRequest,
		*clientoor.SendIncomingAckRequest:

		h.t.Fatalf("transport event %T should not reach "+
			"local handler", outbox)

		return nil, nil

	default:
		h.t.Fatalf("unhandled local event %T", outbox)

		return nil, nil
	}
}

var _ clientoor.OutboxHandler = (*localClientOutboxHandler)(nil)

// inProcessServerConnBridge is a test-only serverconn bridge that forwards
// client transport messages to the real server actor outside the client
// durable actor transaction, then injects the resulting response events back
// into the client actor.
type inProcessServerConnBridge struct {
	t *testing.T

	mu sync.Mutex

	id string

	server *serveroor.Actor
	client *clientoor.OORClientActor

	pauseFinalize bool

	lastFinalizeArkPSBT  *psbt.Packet
	finalCheckpointPSBTs []*psbt.Packet

	asyncErr error
}

// newInProcessServerConnBridge creates a new test transport bridge.
func newInProcessServerConnBridge(t *testing.T,
	server *serveroor.Actor) *inProcessServerConnBridge {

	return &inProcessServerConnBridge{
		t:      t,
		id:     "oor-e2e-serverconn",
		server: server,
	}
}

// ID returns the stable test bridge identifier.
func (b *inProcessServerConnBridge) ID() string {
	return b.id
}

// Tell forwards transport messages asynchronously to the server actor.
func (b *inProcessServerConnBridge) Tell(ctx context.Context,
	msg serverconn.ServerConnMsg) error {

	sendReq, ok := msg.(*serverconn.SendClientEventRequest)
	if !ok {
		return fmt.Errorf("unexpected serverconn message: %T", msg)
	}

	switch req := sendReq.Message.(type) {
	case *clientoor.SendSubmitPackageRequest:
		go b.handleSubmit(b.asyncContext(ctx), req)
		return nil

	case *clientoor.SendFinalizePackageRequest:
		b.mu.Lock()
		b.lastFinalizeArkPSBT = req.ArkPSBT
		b.finalCheckpointPSBTs = req.FinalCheckpointPSBTs
		pauseFinalize := b.pauseFinalize
		b.mu.Unlock()

		if pauseFinalize {
			return nil
		}

		go b.handleFinalize(b.asyncContext(ctx), req)

		return nil

	default:
		return fmt.Errorf("unexpected transport request: %T",
			sendReq.Message)
	}
}

// setClient binds the bridge to the client actor that should receive server
// response events.
func (b *inProcessServerConnBridge) setClient(
	client *clientoor.OORClientActor) {

	b.mu.Lock()
	defer b.mu.Unlock()

	b.client = client
}

// setServer swaps the active server actor used by future bridge requests.
func (b *inProcessServerConnBridge) setServer(server *serveroor.Actor) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.server = server
}

// setPauseFinalize controls whether finalize transport is held for manual
// replay by the test.
func (b *inProcessServerConnBridge) setPauseFinalize(pause bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.pauseFinalize = pause
}

// asyncError returns any asynchronous bridge failure seen so far.
func (b *inProcessServerConnBridge) asyncError() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.asyncErr
}

// asyncContext strips transaction-scoped actor metadata from an outbound
// transport callback and detaches it from the originating request lifetime.
func (b *inProcessServerConnBridge) asyncContext(
	ctx context.Context,
) context.Context {

	strippedCtx := clientactor.WithoutTx(ctx)
	strippedCtx = clientactor.WithoutOutboxID(strippedCtx)

	return context.WithoutCancel(strippedCtx)
}

// lastFinalizePackage returns the most recent finalize payload captured by the
// bridge.
func (b *inProcessServerConnBridge) lastFinalizePackage() (*psbt.Packet,
	[]*psbt.Packet) {

	b.mu.Lock()
	defer b.mu.Unlock()

	return b.lastFinalizeArkPSBT, b.finalCheckpointPSBTs
}

// handleSubmit sends the submit package to the server actor and injects the
// response back into the client actor.
func (b *inProcessServerConnBridge) handleSubmit(ctx context.Context,
	req *clientoor.SendSubmitPackageRequest) {

	attachArkLeafScriptsForTest(
		b.t, req.ArkPSBT, req.TransferInputs,
		req.CheckpointPSBTs,
	)

	descs := make([]serveroor.VTXOSigningDescriptor, 0,
		len(req.TransferInputs))
	for i := range req.TransferInputs {
		vtxoPolicyTemplate, err := req.TransferInputs[i].
			EffectiveVTXOPolicyTemplate()
		require.NoError(b.t, err)

		spendPath, err := req.TransferInputs[i].EffectiveSpendPath()
		require.NoError(b.t, err)

		spendPathRaw, err := spendPath.Encode()
		require.NoError(b.t, err)

		descs = append(descs, serveroor.VTXOSigningDescriptor{
			Outpoint:           req.TransferInputs[i].VTXO.Outpoint,
			VTXOPolicyTemplate: vtxoPolicyTemplate,
			SpendPath:          spendPathRaw,
			OwnerLeafPolicy: req.TransferInputs[i].
				OwnerLeafPolicy,
		})
	}

	b.mu.Lock()
	server := b.server
	client := b.client
	b.mu.Unlock()

	resp := server.Ref().Ask(
		ctx, &serveroor.SubmitOORRequest{
			ArkPSBT:                req.ArkPSBT,
			CheckpointPSBTs:        req.CheckpointPSBTs,
			VTXOSigningDescriptors: descs,
		},
	).Await(ctx)
	if resp.IsErr() {
		b.setAsyncErr(resp.Err())
		return
	}

	unwrapped := resp.UnwrapOr(nil)
	serverMsg, ok := unwrapped.(*serveroor.SubmitOORResponse)
	if !ok {
		b.setAsyncErr(fmt.Errorf("unexpected submit response: %T",
			unwrapped))
		return
	}

	coSigned := clonePSBTSliceForTest(
		b.t, serverMsg.CoSignedCheckpointPSBTs,
	)

	driveResp := client.Receive(ctx, &clientoor.DriveEventRequest{
		SessionID: clientoor.SessionID(serverMsg.SessionID),
		Event: &clientoor.SubmitAcceptedEvent{
			SessionID: clientoor.SessionID(
				serverMsg.SessionID,
			),
			ArkPSBT:                 req.ArkPSBT,
			CoSignedCheckpointPSBTs: coSigned,
		},
	})
	if driveResp.IsErr() {
		b.setAsyncErr(driveResp.Err())
	}
}

// handleFinalize sends the finalize package to the server actor and injects
// finalize acceptance back into the client actor.
func (b *inProcessServerConnBridge) handleFinalize(ctx context.Context,
	req *clientoor.SendFinalizePackageRequest) {

	b.mu.Lock()
	server := b.server
	client := b.client
	b.mu.Unlock()

	sessionID := serveroor.SessionID(
		req.ArkPSBT.UnsignedTx.TxHash(),
	)
	resp := server.Ref().Ask(
		ctx, &serveroor.FinalizeOORRequest{
			SessionID:            sessionID,
			FinalCheckpointPSBTs: req.FinalCheckpointPSBTs,
		},
	).Await(ctx)
	if resp.IsErr() {
		b.setAsyncErr(resp.Err())
		return
	}

	driveResp := client.Receive(ctx, &clientoor.DriveEventRequest{
		SessionID: clientoor.SessionID(sessionID),
		Event:     &clientoor.FinalizeAcceptedEvent{},
	})
	if driveResp.IsErr() {
		b.setAsyncErr(driveResp.Err())
	}
}

// setAsyncErr records the first asynchronous bridge error.
func (b *inProcessServerConnBridge) setAsyncErr(err error) {
	if err == nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.asyncErr == nil {
		b.asyncErr = err
	}
}

// inProcessClientToServerOutbox is a test-only adaptor that connects
// the client OOR FSM outbox messages to the server OOR coordinator
// actor, without RPC.
type inProcessClientToServerOutbox struct {
	t *testing.T

	server    *serveroor.Actor
	sessionID serveroor.SessionID

	lastFinalizeArkPSBT *psbt.Packet

	signDescs    []serveroor.VTXOSigningDescriptor
	clientSigner input.Signer
}

// Handle processes a client outbox request and returns follow-up
// events.
func (h *inProcessClientToServerOutbox) Handle(ctx context.Context,
	sessionID clientoor.SessionID,
	outbox clientoor.OutboxEvent) ([]clientoor.Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *clientoor.RequestArkSignatures:
		// Client split-4 introduces an explicit Ark signing
		// step before submit. In tests the Ark PSBT does not
		// require additional client signatures, so pass it
		// through.
		return []clientoor.Event{
			&clientoor.ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *clientoor.SendSubmitPackageRequest:
		attachArkLeafScriptsForTest(
			h.t, msg.ArkPSBT, msg.TransferInputs,
			msg.CheckpointPSBTs,
		)

		// This package-local harness is white-box by design:
		// drive the server behavior directly instead of
		// nesting a request-response
		// actor future inside the client durable actor's outbox walk.
		resp := h.server.Receive(
			ctx, &serveroor.SubmitOORRequest{
				ArkPSBT:                msg.ArkPSBT,
				CheckpointPSBTs:        msg.CheckpointPSBTs,
				VTXOSigningDescriptors: h.signDescs,
			},
		)
		require.True(h.t, resp.IsOk(), resp.Err())

		unwrapped := resp.UnwrapOr(nil)
		serverMsg, ok := unwrapped.(*serveroor.SubmitOORResponse)
		require.True(h.t, ok)
		h.sessionID = serverMsg.SessionID

		coSigned := clonePSBTSliceForTest(
			h.t, serverMsg.CoSignedCheckpointPSBTs,
		)

		return []clientoor.Event{
			&clientoor.SubmitAcceptedEvent{
				SessionID: clientoor.SessionID(
					serverMsg.SessionID,
				),
				ArkPSBT:                 msg.ArkPSBT,
				CoSignedCheckpointPSBTs: coSigned,
			},
		}, nil

	case *clientoor.RequestCheckpointSignatures:
		finalCheckpointPSBTs := finalizeCheckpointPSBTsForTest(
			h.t, h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)

		return []clientoor.Event{
			&clientoor.CheckpointsSignedEvent{
				FinalCheckpointPSBTs: finalCheckpointPSBTs,
			},
		}, nil

	case *clientoor.SendFinalizePackageRequest:
		if msg.ArkPSBT == nil || msg.ArkPSBT.UnsignedTx == nil {
			return nil, fmt.Errorf(
				"ark psbt must be provided",
			)
		}

		h.lastFinalizeArkPSBT = msg.ArkPSBT

		resp := h.server.Receive(
			ctx, &serveroor.FinalizeOORRequest{
				SessionID: serveroor.SessionID(
					sessionID,
				),
				FinalCheckpointPSBTs: msg.
					FinalCheckpointPSBTs,
			},
		)
		require.True(h.t, resp.IsOk())

		_, ok := resp.UnwrapOr(nil).(*serveroor.FinalizeOORResponse)
		require.True(h.t, ok)

		return []clientoor.Event{
			&clientoor.FinalizeAcceptedEvent{},
		}, nil

	case *clientoor.MarkInputsSpentRequest:
		// Outgoing OOR transfers are off-chain. After finalize
		// is accepted, the client updates its local VTXO
		// persistence state and then completes the session.
		_ = msg
		return []clientoor.Event{
			&clientoor.InputsMarkedSpentEvent{},
		}, nil

	default:
		return nil, nil
	}
}

var _ clientoor.OutboxHandler = (*inProcessClientToServerOutbox)(nil)

// inProcessReceiveOutbox is a test-only outbox handler for incoming
// transfer sessions.
type inProcessReceiveOutbox struct {
	t *testing.T

	recipientKey keychain.KeyDescriptor

	operatorKey *btcec.PublicKey

	exitDelay uint32

	materialized []*clientvtxo.Descriptor
}

// Handle processes incoming-transfer outbox messages and returns
// follow-ups.
func (h *inProcessReceiveOutbox) Handle(_ context.Context,
	_ clientoor.SessionID,
	outbox clientoor.OutboxEvent) ([]clientoor.Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *clientoor.IncomingTransferNotification:
		return nil, nil

	case *clientoor.QueryIncomingMetadataRequest:
		matches := make(
			[]clientoor.IncomingMetadataMatch, 0,
			len(msg.Recipients),
		)
		for i := range msg.Recipients {
			m := clientoor.IncomingMetadataMatch{
				OutputIndex: msg.Recipients[i].OutputIndex,
				Metadata: clientoor.IncomingVTXOMetadata{
					RoundID: "oor-e2e",
				},
			}
			matches = append(matches, m)
		}

		return []clientoor.Event{
			&clientoor.IncomingMetadataResolvedEvent{
				Matches: matches,
			},
		}, nil

	case *clientoor.MaterializeIncomingVTXOsRequest:
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
				RoundID:        "oor-e2e",
				CommitmentTxID: arkTxid,
				Status:         clientvtxo.VTXOStatusLive,
			}
			h.materialized = append(
				h.materialized, desc,
			)
		}

		return []clientoor.Event{
			&clientoor.IncomingHandledEvent{},
		}, nil

	case *clientoor.SendIncomingAckRequest:
		// Client split-4 requires an explicit ack event to
		// transition from ReceiveAwaitingAck to ReceiveCompleted.
		return []clientoor.Event{
			&clientoor.IncomingAckSentEvent{},
		}, nil

	default:
		return nil, nil
	}
}

var _ clientoor.OutboxHandler = (*inProcessReceiveOutbox)(nil)

// driveOutboxToFSM drives an outbox list by invoking the handler and
// feeding any follow-up events back into the FSM until no more outbox
// events are emitted.
func driveOutboxToFSM(ctx context.Context, t *testing.T,
	sessionID clientoor.SessionID, fsm *clientoor.StateMachine,
	handler clientoor.OutboxHandler,
	outbox []clientoor.OutboxEvent) error {

	t.Helper()

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
			err = driveOutboxToFSM(
				ctx, t, sessionID, fsm,
				handler, next,
			)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// pausedFinalizeAdaptor is an outbox handler that drives submit and
// checkpoint signing, but records finalize requests so the test can
// simulate a server restart before finalization.
type pausedFinalizeAdaptor struct {
	t *testing.T

	server *serveroor.Actor

	pauseFinalize bool

	sessionID clientoor.SessionID

	arkPSBT *psbt.Packet

	finalCheckpointPSBTs []*psbt.Packet

	signDescs    []serveroor.VTXOSigningDescriptor
	clientSigner input.Signer
}

// Handle processes the outbox request and returns follow-up events.
func (h *pausedFinalizeAdaptor) Handle(ctx context.Context,
	sessionID clientoor.SessionID,
	outbox clientoor.OutboxEvent) ([]clientoor.Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *clientoor.RequestArkSignatures:
		// See inProcessClientToServerOutbox.Handle for
		// rationale — pass the Ark PSBT through unchanged.
		return []clientoor.Event{
			&clientoor.ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *clientoor.SendSubmitPackageRequest:
		attachArkLeafScriptsForTest(
			h.t, msg.ArkPSBT, msg.TransferInputs,
			msg.CheckpointPSBTs,
		)

		resp := h.server.Receive(
			ctx, &serveroor.SubmitOORRequest{
				ArkPSBT:                msg.ArkPSBT,
				CheckpointPSBTs:        msg.CheckpointPSBTs,
				VTXOSigningDescriptors: h.signDescs,
			},
		)
		require.True(h.t, resp.IsOk())

		unwrapped := resp.UnwrapOr(nil)
		serverMsg, ok := unwrapped.(*serveroor.SubmitOORResponse)
		require.True(h.t, ok)

		h.sessionID = clientoor.SessionID(
			serverMsg.SessionID,
		)

		coSigned := clonePSBTSliceForTest(
			h.t, serverMsg.CoSignedCheckpointPSBTs,
		)

		return []clientoor.Event{
			&clientoor.SubmitAcceptedEvent{
				SessionID:               h.sessionID,
				ArkPSBT:                 msg.ArkPSBT,
				CoSignedCheckpointPSBTs: coSigned,
			},
		}, nil

	case *clientoor.RequestCheckpointSignatures:
		finalCheckpointPSBTs := finalizeCheckpointPSBTsForTest(
			h.t, h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)

		return []clientoor.Event{
			&clientoor.CheckpointsSignedEvent{
				FinalCheckpointPSBTs: finalCheckpointPSBTs,
			},
		}, nil

	case *clientoor.SendFinalizePackageRequest:
		if msg.ArkPSBT == nil || msg.ArkPSBT.UnsignedTx == nil {
			return nil, fmt.Errorf(
				"ark psbt must be provided",
			)
		}

		h.arkPSBT = msg.ArkPSBT
		h.finalCheckpointPSBTs = msg.FinalCheckpointPSBTs

		if h.pauseFinalize {
			// Do not acknowledge finalize yet. This simulates
			// an async RPC send where the response arrives
			// later (after restart).
			return nil, nil
		}

		resp := h.server.Receive(
			ctx, &serveroor.FinalizeOORRequest{
				SessionID: serveroor.SessionID(
					sessionID,
				),
				FinalCheckpointPSBTs: msg.
					FinalCheckpointPSBTs,
			},
		)
		require.True(h.t, resp.IsOk(), resp.Err())

		_, ok := resp.UnwrapOr(nil).(*serveroor.FinalizeOORResponse)
		require.True(h.t, ok)

		return []clientoor.Event{
			&clientoor.FinalizeAcceptedEvent{},
		}, nil

	case *clientoor.MarkInputsSpentRequest:
		_ = msg
		return []clientoor.Event{
			&clientoor.InputsMarkedSpentEvent{},
		}, nil

	default:
		return nil, nil
	}
}

var _ clientoor.OutboxHandler = (*pausedFinalizeAdaptor)(nil)

// TestOORClientServerE2E asserts the client outgoing transfer FSM can
// drive the server coordinator FSM to finalized, using only in-process
// test adaptors.
func TestOORClientServerE2E(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	sqlStore := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	locker := db.NewVTXOLockerDB(dbStore, btclog.Disabled)
	store := dbStore.NewVTXORecordStore()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	driver := serveroor.NewDriver(serveroor.DriverCfg{
		Locker:         locker,
		Store:          store,
		OperatorSigner: operatorSigner,
		OperatorKey: keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		},
	})

	deliveryStore, err := db.NewActorDeliveryStoreFromDB(
		sqlStore, clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	server := startE2EServerActor(t, serveroor.ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
		DeliveryStore:    deliveryStore,
	})

	inputValue := btcutil.Amount(10000)

	exitDelay := uint32(10)

	senderKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{senderKey}, nil,
	)

	senderInputOutpoint := wire.OutPoint{
		Hash:  [32]byte{0x01},
		Index: 0,
	}

	inputs := []clientoor.TransferInput{
		newClientTransferInput(
			t, senderKey, policy.OperatorKey, exitDelay,
			senderInputOutpoint, inputValue,
		),
	}

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientTapKey, err := arkscript.VTXOTapKey(
		recipientKey.PubKey(), policy.OperatorKey, exitDelay,
	)
	require.NoError(t, err)

	recipientPkScript, err := txscript.PayToTaprootScript(
		recipientTapKey,
	)
	require.NoError(t, err)

	recipients := []oortx.RecipientOutput{
		{
			PkScript: recipientPkScript,
			Value:    inputValue,
		},
	}

	err = store.Create(ctx, &vtxo.Record{
		Outpoint: senderInputOutpoint,
		Value:    int64(inputValue),
		PkScript: inputs[0].VTXO.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	bridge := newInProcessServerConnBridge(t, server)
	// The client uses its own database for the delivery store since
	// in production the client and server have separate databases.
	clientSQLStore := db.NewTestDB(t)
	clientDeliveryStore, err := db.NewActorDeliveryStoreFromDB(
		clientSQLStore, clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	client := clientoor.NewOORClientActor(clientoor.ClientActorCfg{
		OutboxHandler: &localClientOutboxHandler{
			t:            t,
			clientSigner: clientSigner,
		},
		ServerConn:    bridge,
		DeliveryStore: clientDeliveryStore,
	})
	defer client.Stop()
	bridge.setClient(client)

	startResp := client.Receive(ctx, &clientoor.StartTransferRequest{
		Policy: arkscript.CheckpointPolicy{
			OperatorKey: policy.OperatorKey,
			CSVDelay:    policy.CSVDelay,
		},
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk(), startResp.Err())

	startUnwrapped := startResp.UnwrapOr(nil)
	startMsg, ok := startUnwrapped.(*clientoor.StartTransferResponse)
	require.True(t, ok)

	// Now simulate the recipient client processing an incoming
	// transfer.
	//
	// In production, this would arrive via RPC push or polling. For
	// now, we drive the incoming-transfer FSM directly.
	var finalizeArkPSBT *psbt.Packet
	require.Eventually(t, func() bool {
		require.NoError(t, bridge.asyncError())

		stateResp := client.Receive(ctx, &clientoor.GetStateRequest{
			SessionID: startMsg.SessionID,
		})
		if stateResp.IsErr() {
			return false
		}

		stateMsg, ok := stateResp.UnwrapOr(
			nil,
		).(*clientoor.GetStateResponse)
		if !ok {
			return false
		}

		finalizeArkPSBT, _ = bridge.lastFinalizePackage()

		_, completed := stateMsg.State.(*clientoor.Completed)

		return completed && finalizeArkPSBT != nil
	}, 10*time.Second, 20*time.Millisecond)

	receiveSess, receiveOutbox, err := clientoor.DriveIncomingTransfer(
		ctx, startMsg.SessionID, finalizeArkPSBT,
	)
	require.NoError(t, err)

	receiveHandler := &inProcessReceiveOutbox{
		t: t,
		recipientKey: keychain.KeyDescriptor{
			PubKey: recipientKey.PubKey(),
		},
		operatorKey: policy.OperatorKey,
		exitDelay:   exitDelay,
	}
	err = driveOutboxToFSM(
		ctx, t, startMsg.SessionID,
		receiveSess.FSM, receiveHandler, receiveOutbox,
	)
	require.NoError(t, err)

	recvState, err := receiveSess.FSM.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &clientoor.ReceiveCompleted{}, recvState)

	require.Len(t, receiveHandler.materialized, 1)
	materialized := receiveHandler.materialized[0]
	require.Equal(t, recipientPkScript, materialized.PkScript)

	// Assert VTXO lifecycle transitions are applied by the server
	// outbox driver:
	//   - input is spent
	//   - recipient output is materialized as live
	inRec, err := store.Get(ctx, senderInputOutpoint)
	require.NoError(t, err)
	require.NotNil(t, inRec)
	require.Equal(t, vtxo.StatusSpent, inRec.Status)

	require.NotNil(t, finalizeArkPSBT)
	arkTxid := finalizeArkPSBT.UnsignedTx.TxHash()
	outRec, err := store.Get(ctx, wire.OutPoint{
		Hash:  arkTxid,
		Index: 0,
	})
	require.NoError(t, err)
	require.NotNil(t, outRec)
	require.Equal(t, vtxo.StatusLive, outRec.Status)
}

// TestOORClientServerRestartBeforeFinalize asserts a server can restart
// after reaching point-of-no-return and still accept finalize once the
// client resumes.
func TestOORClientServerRestartBeforeFinalize(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	db1 := db.NewTestDB(t)
	sessionStore1 := serveroor.NewDBSessionStore(
		db1, clock.NewDefaultClock(), btclog.Disabled,
	)
	deliveryStore1, err := db.NewActorDeliveryStoreFromDB(
		db1, clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	driver1 := serveroor.NewDriver(serveroor.DriverCfg{
		SessionStore:   sessionStore1,
		OperatorSigner: operatorSigner,
		OperatorKey: keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		},
	})
	server1 := serveroor.NewActor(serveroor.ActorCfg{
		OutboxHandler:    driver1,
		CheckpointPolicy: policy,
		DeliveryStore:    deliveryStore1,
		SessionStore:     sessionStore1,
	})

	err = server1.Start(ctx)
	require.NoError(t, err)

	inputValue := btcutil.Amount(10000)
	exitDelay := uint32(10)

	senderKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{senderKey}, nil,
	)

	senderInputOutpoint := wire.OutPoint{
		Hash:  [32]byte{0x02},
		Index: 0,
	}

	inputs := []clientoor.TransferInput{
		newClientTransferInput(
			t, senderKey, policy.OperatorKey, exitDelay,
			senderInputOutpoint, inputValue,
		),
	}

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientTapKey, err := arkscript.VTXOTapKey(
		recipientKey.PubKey(), policy.OperatorKey, exitDelay,
	)
	require.NoError(t, err)

	recipientPkScript, err := txscript.PayToTaprootScript(
		recipientTapKey,
	)
	require.NoError(t, err)

	recipients := []oortx.RecipientOutput{
		{
			PkScript: recipientPkScript,
			Value:    inputValue,
		},
	}

	bridge := newInProcessServerConnBridge(t, server1)
	bridge.setPauseFinalize(true)
	// The client uses its own database for the delivery store since
	// in production the client and server have separate databases.
	clientDB := db.NewTestDB(t)
	clientDeliveryStore, err := db.NewActorDeliveryStoreFromDB(
		clientDB, clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	client := clientoor.NewOORClientActor(clientoor.ClientActorCfg{
		OutboxHandler: &localClientOutboxHandler{
			t:            t,
			clientSigner: clientSigner,
		},
		ServerConn:    bridge,
		DeliveryStore: clientDeliveryStore,
	})
	defer client.Stop()
	bridge.setClient(client)

	startResp := client.Receive(
		ctx, &clientoor.StartTransferRequest{
			Policy: arkscript.CheckpointPolicy{
				OperatorKey: policy.OperatorKey,
				CSVDelay:    policy.CSVDelay,
			},
			Inputs:     inputs,
			Recipients: recipients,
		},
	)
	require.True(t, startResp.IsOk(), startResp.Err())

	startUnwrapped := startResp.UnwrapOr(nil)
	startMsg, ok := startUnwrapped.(*clientoor.StartTransferResponse)
	require.True(t, ok)
	require.Eventually(t, func() bool {
		require.NoError(t, bridge.asyncError())

		stateResp := client.Receive(ctx, &clientoor.GetStateRequest{
			SessionID: startMsg.SessionID,
		})
		if stateResp.IsErr() {
			return false
		}

		stateMsg, ok := stateResp.UnwrapOr(
			nil,
		).(*clientoor.GetStateResponse)
		if !ok {
			return false
		}

		_, awaitingFinalize := stateMsg.State.(*clientoor.
			AwaitingFinalizeAccepted)
		_, finalCheckpointPSBTs := bridge.lastFinalizePackage()

		return awaitingFinalize && len(finalCheckpointPSBTs) > 0
	}, 10*time.Second, 20*time.Millisecond)

	// At this point the server should be at point-of-no-return and
	// the session snapshot should be durable.
	serverState, err := server1.CurrentState(
		ctx, serveroor.SessionID(startMsg.SessionID),
	)
	require.NoError(t, err)
	require.IsType(t, &serveroor.CoSignedState{}, serverState)

	// Simulate server restart by creating a new actor instance that
	// rehydrates state from the database, rather than in-memory
	// session state.
	server1.Stop()

	sessionStore2 := serveroor.NewDBSessionStore(
		db1, clock.NewDefaultClock(), btclog.Disabled,
	)
	deliveryStore2, err := db.NewActorDeliveryStoreFromDB(
		db1, clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	driver2 := serveroor.NewDriver(serveroor.DriverCfg{
		SessionStore:   sessionStore2,
		OperatorSigner: operatorSigner,
		OperatorKey: keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		},
	})
	server2 := serveroor.NewActor(serveroor.ActorCfg{
		OutboxHandler:    driver2,
		CheckpointPolicy: policy,
		DeliveryStore:    deliveryStore2,
		SessionStore:     sessionStore2,
	})

	err = server2.Start(ctx)
	require.NoError(t, err)
	defer server2.Stop()

	// The paused adaptor captured the finalize package that must
	// survive restart and be replayable.
	finalizeArkPSBT, finalCheckpointPSBTs := bridge.lastFinalizePackage()
	require.NotNil(t, finalizeArkPSBT)
	require.NotEmpty(t, finalCheckpointPSBTs)

	// Wait for the restarted server to restore the co-signed session
	// from durable state before replaying finalize.
	require.Eventually(t, func() bool {
		state, stateErr := server2.CurrentState(
			ctx, serveroor.SessionID(startMsg.SessionID),
		)
		if stateErr != nil {
			return false
		}

		_, ok := state.(*serveroor.CoSignedState)

		return ok
	}, 10*time.Second, 20*time.Millisecond)

	bridge.setServer(server2)
	bridge.setPauseFinalize(false)

	finalizeResp := server2.Ref().Ask(
		ctx, &serveroor.FinalizeOORRequest{
			SessionID: serveroor.SessionID(
				startMsg.SessionID,
			),
			FinalCheckpointPSBTs: finalCheckpointPSBTs,
		},
	).Await(ctx)
	require.True(t, finalizeResp.IsOk(), finalizeResp.Err())

	// Drive the local client completion path after finalize
	// acceptance.
	driveResp := client.Receive(ctx, &clientoor.DriveEventRequest{
		SessionID: startMsg.SessionID,
		Event:     &clientoor.FinalizeAcceptedEvent{},
	})
	require.True(t, driveResp.IsOk(), driveResp.Err())

	stateResp := client.Receive(ctx, &clientoor.GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(
		nil,
	).(*clientoor.GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &clientoor.Completed{}, stateMsg.State)
}

// TestOORServerRejectsTamperedFinalizeSignature ensures finalize fails
// if the client tampers with checkpoint signature material after
// co-signing.
func TestOORServerRejectsTamperedFinalizeSignature(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	driver := serveroor.NewDriver(serveroor.DriverCfg{
		OperatorSigner: operatorSigner,
		OperatorKey: keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		},
	})

	// Construct the actor without starting the durable runtime. This
	// test calls Receive directly (bypassing the durable mailbox), so
	// starting the runtime would race: the restart message wipes the
	// sessions map asynchronously, which can erase the session
	// created by the submit call before finalize runs.
	server := serveroor.NewActor(serveroor.ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	inputValue := btcutil.Amount(10000)
	exitDelay := uint32(10)

	senderKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{senderKey}, nil,
	)

	senderOutpoint := wire.OutPoint{
		Hash:  [32]byte{0x03},
		Index: 0,
	}

	inputs := []clientoor.TransferInput{
		newClientTransferInput(
			t, senderKey, policy.OperatorKey, exitDelay,
			senderOutpoint, inputValue,
		),
	}

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientTapKey, err := arkscript.VTXOTapKey(
		recipientKey.PubKey(), policy.OperatorKey, exitDelay,
	)
	require.NoError(t, err)

	recipientPkScript, err := txscript.PayToTaprootScript(
		recipientTapKey,
	)
	require.NoError(t, err)

	checkpointRes, err := oortx.BuildCheckpointPSBT(
		arkscript.CheckpointPolicy{
			OperatorKey: policy.OperatorKey,
			CSVDelay:    policy.CSVDelay,
		}, oortx.CheckpointInput{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: senderOutpoint,
				Output: &wire.TxOut{
					Value:    int64(inputValue),
					PkScript: inputs[0].VTXO.PkScript,
				},
			},
			OwnerLeafScript: inputs[0].OwnerLeafScript,
			OwnerLeafPolicy: inputs[0].OwnerLeafPolicy,
		},
	)
	require.NoError(t, err)

	arkPSBT, err := oortx.BuildArkPSBT(
		[]oortx.CheckpointOutput{{
			Txid: checkpointRes.PSBT.
				UnsignedTx.TxHash(),
			Output: checkpointRes.PSBT.
				UnsignedTx.TxOut[0],
			TapTreeEncoded:  checkpointRes.TapTreeEncoded,
			OwnerLeafPolicy: checkpointRes.OwnerLeafPolicy,
		}}, []oortx.RecipientOutput{{
			PkScript: recipientPkScript,
			Value:    inputValue,
		}},
	)
	require.NoError(t, err)

	leaf, err := oortx.BuildTaprootTapLeafScript(
		checkpointRes.TapTreeEncoded,
		inputs[0].OwnerLeafScript,
	)
	require.NoError(t, err)
	arkPSBT.Inputs[0].TaprootLeafScript =
		[]*psbt.TaprootTapLeafScript{leaf}

	senderPolicyTemplate := mustStandardVTXOPolicyTemplate(
		t, senderKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)

	submitResp := server.Receive(
		ctx, &serveroor.SubmitOORRequest{
			ArkPSBT: arkPSBT,
			CheckpointPSBTs: []*psbt.Packet{
				checkpointRes.PSBT,
			},
			VTXOSigningDescriptors: []serveroor.
				VTXOSigningDescriptor{{
				Outpoint:           senderOutpoint,
				VTXOPolicyTemplate: senderPolicyTemplate,
				SpendPath: mustStandardCollabSpendPath(
					t, senderKey.PubKey(),
					operatorKey.PubKey(), exitDelay,
				),
				OwnerLeafPolicy: inputs[0].OwnerLeafPolicy,
			}},
		},
	)
	require.True(t, submitResp.IsOk(), submitResp.Err())

	submitMsg, ok := submitResp.UnwrapOr(
		nil,
	).(*serveroor.SubmitOORResponse)
	require.True(t, ok)

	finalized := finalizeCheckpointPSBTsForTest(
		t, clientSigner, inputs, clonePSBTSliceForTest(
			t, submitMsg.CoSignedCheckpointPSBTs,
		),
	)
	tampered := clonePSBTSliceForTest(t, finalized)
	require.NotEmpty(t, tampered)
	require.NotEmpty(t, tampered[0].Inputs)
	require.NotEmpty(
		t, tampered[0].Inputs[0].TaprootScriptSpendSig,
	)

	operatorXOnly := schnorr.SerializePubKey(
		policy.OperatorKey,
	)

	var tamperedOwner bool
	for i := range tampered[0].Inputs[0].TaprootScriptSpendSig {
		sigRec := tampered[0].Inputs[0].
			TaprootScriptSpendSig[i]

		if sigRec == nil {
			continue
		}

		// Tamper the non-operator signature while preserving
		// operator signature bytes as sent in
		// submit-accepted.
		if bytes.Equal(sigRec.XOnlyPubKey, operatorXOnly) {
			continue
		}

		require.NotEmpty(t, sigRec.Signature)
		sigRec.Signature[0] ^= 0x01
		tamperedOwner = true

		break
	}
	require.True(t, tamperedOwner)

	finalizeResp := server.Receive(
		ctx, &serveroor.FinalizeOORRequest{
			SessionID:            submitMsg.SessionID,
			FinalCheckpointPSBTs: tampered,
		},
	)
	require.True(t, finalizeResp.IsErr())
	require.ErrorContains(
		t, finalizeResp.Err(), "owner signature invalid",
	)
}
