//go:build systest

package systest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo/clientconn"
	serveroor "github.com/lightninglabs/darepo/oor"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

const (
	// defaultOORResponseTimeout is the maximum time to wait for an
	// OOR response to arrive through the bridge.
	defaultOORResponseTimeout = 30 * time.Second
)

// bridgeOOROutbox is a test-only adaptor that routes client OOR FSM outbox
// messages through the BridgeClientConn infrastructure instead of using
// direct Ask calls. This exercises the full pushClientResponse ->
// ClientsConn -> BridgeClientConn path in tests.
//
// For server-bound requests (Submit, Finalize), the adaptor subscribes to
// the bridge BEFORE sending the Tell to avoid a race where the response
// arrives before the subscription is established. It then waits for the
// async response via WaitForOORResponse.
//
// For local operations (signing), the adaptor performs the work in-process
// just like the original oorClientToServerOutbox.
type bridgeOOROutbox struct {
	t *testing.T

	// clientID identifies this client for response routing through the
	// bridge. Must match the ClientID set on server requests so
	// pushClientResponse routes the response to this client.
	clientID clientconn.ClientID

	// server is the OOR coordinator actor to send requests to.
	server *serveroor.Actor

	// bridge is the BridgeClientConn that receives server responses
	// via the ClientsConn -> Tell path.
	bridge *BridgeClientConn

	// responseTimeout is the maximum time to wait for a bridge
	// response. Defaults to defaultOORResponseTimeout.
	responseTimeout time.Duration

	// senderSigner signs checkpoint transactions on behalf of the
	// sending client.
	senderSigner input.Signer

	// serverSignDescs carry VTXO signing descriptors needed by the
	// server to co-sign checkpoint transactions.
	serverSignDescs []serveroor.VTXOSigningDescriptor

	// signingInputs hold per-input signing metadata for checkpoint
	// signing.
	signingInputs []checkpointSignInput

	// finalCheckpointPSBTs captures the fully signed checkpoint PSBTs
	// after the client signs them, so the test can broadcast.
	finalCheckpointPSBTs []*psbt.Packet

	// coSignedBeforeClientSign captures the server co-signed
	// checkpoint bytes before the client adds its signature.
	coSignedBeforeClientSign [][]byte
}

// Handle processes a client outbox request and returns follow-up events.
// Server-bound requests (Submit, Finalize) are routed through the bridge
// infrastructure instead of direct Ask calls.
func (h *bridgeOOROutbox) Handle(ctx context.Context,
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
		return h.handleSubmitViaBridge(ctx, msg)

	case *clientoor.RequestCheckpointSignatures:
		return h.handleCheckpointSign(msg)

	case *clientoor.SendFinalizePackageRequest:
		return h.handleFinalizeViaBridge(ctx, sessionID, msg)

	case *clientoor.MarkInputsSpentRequest:
		return nil, fmt.Errorf(
			"unexpected MarkInputsSpentRequest in " +
				"transport adaptor (missing persistence " +
				"handler)",
		)

	default:
		return nil, nil
	}
}

// handleSubmitViaBridge sends a SubmitOORRequest through the bridge
// infrastructure and waits for the async SubmitOORResponse.
//
// The subscription is established BEFORE the Tell to prevent a race
// where the server responds before we are listening.
func (h *bridgeOOROutbox) handleSubmitViaBridge(ctx context.Context,
	msg *clientoor.SendSubmitPackageRequest) ([]clientoor.Event, error) {

	timeout := h.responseTimeout
	if timeout == 0 {
		timeout = defaultOORResponseTimeout
	}

	// Subscribe BEFORE Tell to avoid missing the response.
	sub, err := h.bridge.Subscribe(h.clientID)
	if err != nil {
		return nil, fmt.Errorf("subscribe for submit: %w", err)
	}
	defer sub.Cancel()

	// Fire-and-forget Tell with the authenticated client ID so
	// pushClientResponse routes the response back through the
	// bridge to this client.
	err = h.server.Ref().Tell(ctx, &serveroor.SubmitOORRequest{
		ClientID:               h.clientID,
		ArkPSBT:                msg.ArkPSBT,
		CheckpointPSBTs:        msg.CheckpointPSBTs,
		VTXOSigningDescriptors: h.serverSignDescs,
	})
	if err != nil {
		return nil, fmt.Errorf("tell submit request: %w", err)
	}

	// Wait for the async response through the bridge.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case update := <-sub.Updates():
			resp, ok := update.(*serveroor.SubmitOORResponse)
			if !ok {
				continue
			}

			return []clientoor.Event{
				&clientoor.SubmitAcceptedEvent{
					SessionID: clientoor.SessionID(
						resp.SessionID,
					),
					ArkPSBT: msg.ArkPSBT,
					CoSignedCheckpointPSBTs: resp.
						CoSignedCheckpointPSBTs,
				},
			}, nil

		case <-sub.Quit():
			return nil, fmt.Errorf(
				"subscription closed waiting for " +
					"submit response",
			)

		case <-timer.C:
			return nil, fmt.Errorf(
				"timeout waiting for submit response",
			)

		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// handleCheckpointSign signs checkpoint PSBTs locally and returns the
// signed result. This is identical to the original oorClientToServerOutbox
// implementation since signing is a local operation.
func (h *bridgeOOROutbox) handleCheckpointSign(
	msg *clientoor.RequestCheckpointSignatures) ([]clientoor.Event,
	error) {

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
			FinalCheckpointPSBTs: msg.
				CoSignedCheckpointPSBTs,
		},
	}, nil
}

// handleFinalizeViaBridge sends a FinalizeOORRequest through the bridge
// infrastructure and waits for the async FinalizeOORResponse.
func (h *bridgeOOROutbox) handleFinalizeViaBridge(ctx context.Context,
	sessionID clientoor.SessionID,
	msg *clientoor.SendFinalizePackageRequest) ([]clientoor.Event,
	error) {

	if msg.ArkPSBT == nil || msg.ArkPSBT.UnsignedTx == nil {
		return nil, fmt.Errorf("ark psbt must be provided")
	}

	h.finalCheckpointPSBTs = msg.FinalCheckpointPSBTs

	timeout := h.responseTimeout
	if timeout == 0 {
		timeout = defaultOORResponseTimeout
	}

	// Subscribe BEFORE Tell to avoid missing the response.
	sub, err := h.bridge.Subscribe(h.clientID)
	if err != nil {
		return nil, fmt.Errorf(
			"subscribe for finalize: %w", err,
		)
	}
	defer sub.Cancel()

	// Fire-and-forget Tell with the authenticated client ID.
	err = h.server.Ref().Tell(ctx, &serveroor.FinalizeOORRequest{
		ClientID: h.clientID,
		SessionID: serveroor.SessionID(
			sessionID,
		),
		FinalCheckpointPSBTs: msg.FinalCheckpointPSBTs,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"tell finalize request: %w", err,
		)
	}

	// Wait for the async response through the bridge.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case update := <-sub.Updates():
			_, ok := update.(*serveroor.FinalizeOORResponse)
			if !ok {
				continue
			}

			return []clientoor.Event{
				&clientoor.FinalizeAcceptedEvent{},
			}, nil

		case <-sub.Quit():
			return nil, fmt.Errorf(
				"subscription closed waiting for " +
					"finalize response",
			)

		case <-timer.C:
			return nil, fmt.Errorf(
				"timeout waiting for finalize response",
			)

		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Compile-time interface check.
var _ clientoor.OutboxHandler = (*bridgeOOROutbox)(nil)

// dropSubmitAcceptedBridgeOutbox is a bridge-based variant of
// dropSubmitAcceptedOutbox that routes through the bridge infrastructure
// but intentionally drops the SubmitOORResponse to simulate a crash after
// the server has co-signed but before the client receives the response.
type dropSubmitAcceptedBridgeOutbox struct {
	t *testing.T

	// clientID identifies this client for response routing.
	clientID clientconn.ClientID

	// server is the OOR coordinator actor.
	server *serveroor.Actor

	// bridge is the BridgeClientConn for async response delivery.
	bridge *BridgeClientConn

	// serverSignDescs carry VTXO signing descriptors.
	serverSignDescs []serveroor.VTXOSigningDescriptor

	// coSignedCheckpointBytes captures the co-signed checkpoint
	// bytes before the response is dropped.
	coSignedCheckpointBytes [][]byte
}

// Handle processes only submit requests via the bridge and drops the
// response to simulate a crash/lost delivery.
func (h *dropSubmitAcceptedBridgeOutbox) Handle(ctx context.Context,
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
		// Subscribe BEFORE Tell.
		sub, err := h.bridge.Subscribe(h.clientID)
		if err != nil {
			return nil, fmt.Errorf(
				"subscribe for submit: %w", err,
			)
		}
		defer sub.Cancel()

		// Fire-and-forget Tell with client ID.
		err = h.server.Ref().Tell(
			ctx, &serveroor.SubmitOORRequest{
				ClientID:        h.clientID,
				ArkPSBT:         msg.ArkPSBT,
				CheckpointPSBTs: msg.CheckpointPSBTs,
				VTXOSigningDescriptors: h.
					serverSignDescs,
			},
		)
		if err != nil {
			return nil, fmt.Errorf(
				"tell submit request: %w", err,
			)
		}

		// Wait for the response so we can capture the
		// co-signed bytes, then drop the result.
		timer := time.NewTimer(defaultOORResponseTimeout)
		defer timer.Stop()

		for {
			select {
			case update := <-sub.Updates():
				resp, ok := update.(*serveroor.SubmitOORResponse) //nolint:ll
				if !ok {
					continue
				}

				// Capture the co-signed checkpoint
				// bytes for test assertions.
				raw := make(
					[][]byte, 0,
					len(resp.CoSignedCheckpointPSBTs),
				)
				for i := range resp.CoSignedCheckpointPSBTs {
					b, err := oorSerializePSBT(
						resp.CoSignedCheckpointPSBTs[i],
					)
					require.NoError(h.t, err)

					raw = append(raw, b)
				}
				h.coSignedCheckpointBytes = raw

				// Drop the response to simulate a
				// crash/lost delivery.
				return nil, nil

			case <-sub.Quit():
				return nil, fmt.Errorf(
					"subscription closed",
				)

			case <-timer.C:
				return nil, fmt.Errorf(
					"timeout waiting for submit",
				)

			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

	default:
		return nil, fmt.Errorf(
			"unexpected outbox type: %T", msg,
		)
	}
}

// Compile-time interface check.
var _ clientoor.OutboxHandler = (*dropSubmitAcceptedBridgeOutbox)(nil)
