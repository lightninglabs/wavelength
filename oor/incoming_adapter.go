package oor

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
)

const incomingResolveCorrelationPrefix = "oor-incoming-resolve:"

// NewResolveIncomingTransferRequest converts a lightweight IncomingOOREvent
// notification into a durable actor request that can be persisted without
// blocking mailbox ingress on a follow-up indexer query.
func NewResolveIncomingTransferRequest(evt *arkrpc.IncomingOOREvent) (
	*ResolveIncomingTransferRequest, error) {

	if evt == nil {
		return nil, fmt.Errorf("nil IncomingOOREvent")
	}

	sessionID, err := chainhash.NewHash(evt.GetSessionId())
	if err != nil {
		return nil, fmt.Errorf("parse session id: %w", err)
	}

	return &ResolveIncomingTransferRequest{
		SessionID: SessionID(*sessionID),
		RecipientPkScript: append(
			[]byte(nil), evt.GetRecipientPkScript()...,
		),
		RecipientEventID: evt.GetRecipientEventId(),
	}, nil
}

// IncomingResolveCorrelationID returns the stable unary correlation ID used
// for durable incoming-transfer resolution queries for the given session and
// recipient event.
func IncomingResolveCorrelationID(sessionID SessionID,
	recipientEventID uint64) string {

	return incomingResolveCorrelationPrefix +
		chainhash.Hash(sessionID).String() + ":" +
		strconv.FormatUint(recipientEventID, 10)
}

// ParseIncomingResolveCorrelationID decodes a durable incoming-transfer
// resolution query correlation ID back into the OOR session ID and recipient
// event ID.
func ParseIncomingResolveCorrelationID(correlationID string) (
	SessionID, uint64, error) {

	if len(correlationID) <= len(incomingResolveCorrelationPrefix) ||
		correlationID[:len(incomingResolveCorrelationPrefix)] !=
			incomingResolveCorrelationPrefix {

		return SessionID{}, 0, fmt.Errorf(
			"unexpected incoming resolve "+
				"correlation id: %q", correlationID,
		)
	}

	suffix := correlationID[len(incomingResolveCorrelationPrefix):]
	parts := strings.SplitN(suffix, ":", 2)
	if len(parts) != 2 {
		return SessionID{}, 0, fmt.Errorf(
			"unexpected incoming resolve "+
				"correlation id payload: %q", suffix,
		)
	}

	hash, err := chainhash.NewHashFromStr(parts[0])
	if err != nil {
		return SessionID{}, 0, fmt.Errorf("parse incoming resolve "+
			"session id: %w", err)
	}

	recipientEventID, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return SessionID{}, 0, fmt.Errorf("parse incoming resolve "+
			"event id: %w", err)
	}

	return SessionID(*hash), recipientEventID, nil
}

// IncomingTransferEventFromResponse validates and converts one
// ListOORRecipientEventsByScriptResponse payload into the complete incoming
// transfer event expected by the receive FSM.
func IncomingTransferEventFromResponse(sessionID SessionID,
	recipientEventID uint64,
	resp *arkrpc.ListOORRecipientEventsByScriptResponse) (
	*IncomingTransferEvent, error) {

	if resp == nil {
		return nil, fmt.Errorf("incoming transfer response must be " +
			"provided")
	}

	if len(resp.GetEvents()) == 0 {
		return nil, fmt.Errorf("no events found for session %x", //nolint:ll
			sessionID[:])
	}

	recipientEvt := resp.Events[0]
	if recipientEvt == nil {
		return nil, fmt.Errorf(
			"incoming transfer event must be provided",
		)
	}

	if recipientEvt.GetEventId() != recipientEventID {
		return nil, fmt.Errorf(
			"unexpected recipient event id: "+
				"got %d, want %d",
			recipientEvt.GetEventId(), recipientEventID,
		)
	}

	eventSessionID, err := chainhash.NewHash(recipientEvt.GetSessionId())
	if err != nil {
		return nil, fmt.Errorf("parse event session id: %w", err)
	}
	if SessionID(*eventSessionID) != sessionID {
		return nil, fmt.Errorf("incoming transfer session mismatch")
	}

	arkPSBT, err := psbtutil.Parse(recipientEvt.GetArkPsbt())
	if err != nil {
		return nil, fmt.Errorf("parse ark psbt: %w", err)
	}

	// TODO(oor-receive): The maxCheckpointPSBTs limit is a pragmatic
	// upper bound for the OOR checkpoint chain depth. If the protocol
	// ever allows deeper chains this constant should be raised via a
	// tracked issue rather than silently increasing memory exposure.
	const maxCheckpointPSBTs = 64
	if len(recipientEvt.GetCheckpointPsbts()) > maxCheckpointPSBTs { //nolint:ll
		return nil, fmt.Errorf(
			"checkpoint count %d exceeds limit %d",
			len(recipientEvt.GetCheckpointPsbts()),
			maxCheckpointPSBTs,
		)
	}

	checkpoints := make([]*psbt.Packet, 0,
		len(recipientEvt.GetCheckpointPsbts()))
	for _, cpRaw := range recipientEvt.GetCheckpointPsbts() {
		cp, cpErr := psbtutil.Parse(cpRaw)
		if cpErr != nil {
			return nil, fmt.Errorf("parse checkpoint: %w", cpErr)
		}

		checkpoints = append(checkpoints, cp)
	}

	return &IncomingTransferEvent{
		SessionID:            sessionID,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: checkpoints,
	}, nil
}
