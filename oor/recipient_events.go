package oor

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	clientoor "github.com/lightninglabs/darepo-client/oor"
)

// RecipientEvent represents a durable incoming transfer notification that can
// be polled by a client.
type RecipientEvent struct {
	// EventID is a per-recipient monotonic cursor value.
	EventID int64

	// SessionID identifies the OOR transfer session for this event.
	SessionID SessionID

	// OutputIndex is the output index in the finalized Ark transaction.
	OutputIndex uint32

	// Value is the output amount in satoshis.
	Value btcutil.Amount

	// ArkPSBT is the finalized Ark package PSBT.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs is the finalized checkpoint set for the session.
	FinalCheckpointPSBTs []*psbt.Packet

	// CreatedAt is the event creation timestamp.
	CreatedAt time.Time

	// RecipientPkScript is the target recipient script used for polling.
	RecipientPkScript []byte
}

// RecipientEventStore persists and queries recipient notifications emitted
// after OOR finalize.
type RecipientEventStore interface {
	// AppendRecipientEvents records per-recipient events for the finalized
	// Ark transaction.
	AppendRecipientEvents(ctx context.Context, sessionID SessionID,
		arkPSBT *psbt.Packet,
		recipients []clientoor.ArkRecipientOutput) error

	// ListRecipientEvents lists events after the cursor.
	ListRecipientEvents(ctx context.Context, recipientPkScript []byte,
		afterEventID int64, limit int32) ([]*RecipientEvent, error)
}
