package oor

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo-client/oorwire"
	"github.com/lightningnetwork/lnd/input"
)

type oorMailboxClient = oorwire.OORMailboxServiceMailboxClient

// InputSpendMarker abstracts marking local input VTXOs spent after finalize is
// accepted by the server.
type InputSpendMarker interface {
	MarkVTXOSpent(ctx context.Context, outpoint wire.OutPoint) error
}

// MailboxOutboxHandler executes outgoing OOR FSM outbox side effects by
// calling server-side OOR methods over mailbox unary RPC.
//
// This is the production-path plumbing layer for:
// - submit package transport;
// - finalize package transport;
// - checkpoint signing; and
// - local input-spent persistence.
type MailboxOutboxHandler struct {
	// RPCClient is the mailbox unary RPC client (typically
	// serverconn.Runtime.Unary()).
	RPCClient mailboxrpc.RPCClient

	// Signer signs checkpoint inputs at RequestCheckpointSignatures.
	Signer input.Signer

	// SpendMarker marks local inputs spent after finalize is accepted.
	SpendMarker InputSpendMarker
}

// Handle executes one OOR outbox request and returns follow-up FSM events.
func (h *MailboxOutboxHandler) Handle(ctx context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		// v0 does not require extra local Ark signing beyond
		// deterministic package construction in this path.
		// Preserve the current behavior by forwarding the Ark
		// PSBT as signed.
		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *SendSubmitPackageRequest:
		return h.handleSubmit(ctx, msg)

	case *RequestCheckpointSignatures:
		return h.handleCheckpointSignatures(msg)

	case *SendFinalizePackageRequest:
		return h.handleFinalize(ctx, sessionID, msg)

	case *MarkInputsSpentRequest:
		return h.handleMarkInputsSpent(ctx, msg)

	case *ScheduleRetryRequest:
		// Retry scheduling policy is owned by the higher
		// layer running this handler. For now we emit
		// RetryDueEvent immediately.
		return []Event{
			&RetryDueEvent{},
		}, nil

	default:
		return nil, nil
	}
}

func (h *MailboxOutboxHandler) handleSubmit(ctx context.Context,
	msg *SendSubmitPackageRequest) ([]Event, error) {

	if h == nil || h.RPCClient == nil {
		return nil, fmt.Errorf("rpc client is required")
	}

	signDescs := make(
		[]oorwire.SigningDescriptor, 0, len(msg.TransferInputs),
	)
	for i := range msg.TransferInputs {
		in := msg.TransferInputs[i]
		if in.VTXO == nil {
			return nil, fmt.Errorf(
				"transfer input %d missing vtxo", i,
			)
		}
		if in.VTXO.ClientKey.PubKey == nil {
			return nil, fmt.Errorf(
				"transfer input %d missing client key", i,
			)
		}

		signDescs = append(signDescs, oorwire.SigningDescriptor{
			Outpoint:  in.VTXO.Outpoint,
			OwnerKey:  in.VTXO.ClientKey.PubKey,
			ExitDelay: in.VTXO.RelativeExpiry,
		})
	}

	req, err := oorwire.NewSubmitPackageRequest(
		msg.ArkPSBT, msg.CheckpointPSBTs, signDescs,
	)
	if err != nil {
		return nil, err
	}

	rpcClient := h.oorRPCClient()
	resp, err := rpcClient.SubmitPackage(ctx, req)
	if err != nil {
		return nil, err
	}

	sessionHash, checkpoints, err := oorwire.ParseSubmitPackageResponse(
		resp,
	)
	if err != nil {
		return nil, err
	}

	return []Event{
		&SubmitAcceptedEvent{
			SessionID:               SessionID(sessionHash),
			ArkPSBT:                 msg.ArkPSBT,
			CoSignedCheckpointPSBTs: checkpoints,
		},
	}, nil
}

func (h *MailboxOutboxHandler) handleCheckpointSignatures(
	msg *RequestCheckpointSignatures) ([]Event, error) {

	if h == nil || h.Signer == nil {
		return nil, fmt.Errorf("signer is required")
	}

	err := SignCheckpointPSBTs(
		h.Signer, msg.TransferInputs, msg.CoSignedCheckpointPSBTs,
	)
	if err != nil {
		return nil, err
	}

	return []Event{
		&CheckpointsSignedEvent{
			FinalCheckpointPSBTs: msg.CoSignedCheckpointPSBTs,
		},
	}, nil
}

func (h *MailboxOutboxHandler) handleFinalize(ctx context.Context,
	sessionID SessionID, msg *SendFinalizePackageRequest) ([]Event, error) {

	if h == nil || h.RPCClient == nil {
		return nil, fmt.Errorf("rpc client is required")
	}

	req, err := oorwire.NewFinalizePackageRequest(
		chainhash.Hash(sessionID), msg.FinalCheckpointPSBTs,
	)
	if err != nil {
		return nil, err
	}

	rpcClient := h.oorRPCClient()
	resp, err := rpcClient.FinalizePackage(ctx, req)
	if err != nil {
		return nil, err
	}

	serverSessionID, err := oorwire.ParseFinalizePackageResponse(resp)
	if err != nil {
		return nil, err
	}

	if SessionID(serverSessionID) != sessionID {
		return nil, fmt.Errorf("finalize response session mismatch")
	}

	return []Event{
		&FinalizeAcceptedEvent{},
	}, nil
}

func (h *MailboxOutboxHandler) handleMarkInputsSpent(ctx context.Context,
	msg *MarkInputsSpentRequest) ([]Event, error) {

	if h == nil || h.SpendMarker == nil {
		return nil, fmt.Errorf("spend marker is required")
	}

	for i := range msg.Outpoints {
		err := h.SpendMarker.MarkVTXOSpent(ctx, msg.Outpoints[i])
		if err != nil {
			return nil, err
		}
	}

	return []Event{
		&InputsMarkedSpentEvent{},
	}, nil
}

// oorRPCClient returns a typed mailbox client for OOR unary methods.
func (h *MailboxOutboxHandler) oorRPCClient() *oorMailboxClient {
	return oorwire.NewOORMailboxServiceMailboxClient(h.RPCClient)
}

var _ OutboxHandler = (*MailboxOutboxHandler)(nil)
