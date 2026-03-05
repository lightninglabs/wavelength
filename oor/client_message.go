package oor

import (
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/darepo/clientconn"
	"google.golang.org/protobuf/proto"
)

// OORRecipientNotification implements clientconn.ClientMessage for incoming
// OOR transfer notifications pushed to recipient clients. The bridge routes
// the message to the correct per-client DurableActor based on the clientID.
//
// This follows the same pattern as indexer/events.go (indexerEventMessage)
// and rounds/outbox_messages.go (ClientSuccessResp, etc.).
type OORRecipientNotification struct {
	// clientID identifies the target client for bridge routing. This is
	// the recipient's mailbox principal resolved via RecipientResolver.
	clientID clientconn.ClientID

	// SessionID is the OOR session identifier (Ark txid hash).
	SessionID SessionID

	// ArkPSBT is the canonical Ark tx PSBT for the transfer.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs are the fully signed checkpoint PSBTs so
	// the recipient can drive its incoming transfer FSM without a
	// follow-up query.
	FinalCheckpointPSBTs []*psbt.Packet
}

// ClientID returns the target client identifier for bridge routing.
func (m *OORRecipientNotification) ClientID() clientconn.ClientID {
	return m.clientID
}

// ToProto returns the proto event payload for envelope body construction.
// The per-client DurableActor wraps this in anypb.Any before sending.
//
// TODO(roasbeef): return a rich IncomingOORTransferEvent proto once the
// production mailbox delivery path is wired.
func (m *OORRecipientNotification) ToProto() proto.Message {
	return nil
}

// NewOORRecipientNotification constructs an OORRecipientNotification for the
// given client.
func NewOORRecipientNotification(clientID clientconn.ClientID,
	sessionID SessionID, arkPSBT *psbt.Packet,
	finalCheckpointPSBTs []*psbt.Packet) *OORRecipientNotification {

	return &OORRecipientNotification{
		clientID:             clientID,
		SessionID:            sessionID,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: finalCheckpointPSBTs,
	}
}

// Compile-time interface check.
var _ clientconn.ClientMessage = (*OORRecipientNotification)(nil)
