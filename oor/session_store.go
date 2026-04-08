package oor

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/vtxo"
)

// SessionStore provides durable persistence for OOR transfer sessions.
//
// The primary goal is restart-safe processing after the point-of-no-return
// (CoSigned). The DB is authoritative for session state so that active
// sessions can be loaded directly on restart without relying on actor
// checkpoint blobs.
type SessionStore interface {
	// UpsertCoSigned persists the CoSigned point-of-no-return snapshot for
	// a session, including its lock set.
	UpsertCoSigned(ctx context.Context, sessionID SessionID,
		inputs []wire.OutPoint, arkPSBT *psbt.Packet,
		coSignedCheckpointPSBTs []*psbt.Packet,
		expiresAt time.Time) error

	// ApplyFinalize transitions the session from cosigned to
	// awaiting_notify, persists finalized checkpoint artifacts, and sets
	// finalized_at. Idempotency is payload-equality based: a repeat call
	// with the same artifacts succeeds, a call with different artifacts
	// returns an error.
	ApplyFinalize(ctx context.Context, sessionID SessionID,
		finalCheckpointPSBTs []*psbt.Packet) error

	// MarkNotified transitions the session from awaiting_notify to
	// finalized. If already finalized, returns success (idempotent).
	MarkNotified(ctx context.Context, sessionID SessionID) error

	// GetSessionState returns the persisted lifecycle state for sessionID.
	//
	// found=false indicates the session does not exist.
	GetSessionState(ctx context.Context, sessionID SessionID) (
		sessionState, bool, error,
	)

	// LoadActiveSessions returns durable snapshots for sessions that
	// require processing after restart (state = cosigned or
	// awaiting_notify). This is the DB-authoritative restart source.
	LoadActiveSessions(ctx context.Context) ([]*ActiveSession, error)

	// LoadFinalizedPackage returns the canonical finalized package for a
	// session.
	LoadFinalizedPackage(ctx context.Context, sessionID SessionID) (
		*FinalizedPackage, error,
	)

	// LoadCheckpointTxByInput returns the broadcastable finalized
	// checkpoint transaction that spends input, if one exists.
	LoadCheckpointTxByInput(ctx context.Context, input wire.OutPoint) (
		*wire.MsgTx, bool, error,
	)
}

// CoSignedAtomicStore is an optional extension for stores that can persist the
// CoSigned snapshot and mark session inputs in-flight in one transaction.
//
// This closes the crash window where one side-effect could succeed while the
// other does not.
type CoSignedAtomicStore interface {
	// UpsertCoSignedAndMarkInFlight persists the CoSigned snapshot and
	// marks all inputs in-flight for owner atomically.
	UpsertCoSignedAndMarkInFlight(ctx context.Context,
		sessionID SessionID, inputs []wire.OutPoint,
		arkPSBT *psbt.Packet,
		coSignedCheckpointPSBTs []*psbt.Packet, expiresAt time.Time,
		owner vtxo.LockOwner) error
}

// FinalizeAtomicStore is an optional extension for stores that can apply the
// finalized session transition and VTXO set mutations in one transaction.
//
// This closes the crash window where inputs can be marked spent before
// recipient outputs are materialized or the session advances to
// awaiting_notify.
type FinalizeAtomicStore interface {
	// ApplyFinalizeAndMaterialize persists the finalized checkpoint set,
	// marks the consumed inputs spent, and materializes recipient
	// outputs atomically.
	ApplyFinalizeAndMaterialize(ctx context.Context,
		sessionID SessionID, inputs []wire.OutPoint,
		finalCheckpointPSBTs []*psbt.Packet,
		outputRecords []*vtxo.Record) error
}

// sessionState is a typed lifecycle state for OOR sessions persisted in the
// database. Using a dedicated type prevents accidental comparison against
// arbitrary strings.
type sessionState string

const (
	// oorStateCoSigned indicates the session has reached the
	// point-of-no-return with operator co-signatures applied.
	oorStateCoSigned sessionState = "cosigned"

	// oorStateAwaitingNotify indicates finalized checkpoints have been
	// persisted and the session is waiting for recipient notification.
	oorStateAwaitingNotify sessionState = "awaiting_notify"

	// oorStateFinalized indicates all recipients have been notified and
	// the session lifecycle is complete.
	oorStateFinalized sessionState = "finalized"
)

// ActiveSession is the durable data for an active OOR session loaded from
// the DB on restart. It includes the session state so the coordinator can
// reconstruct the appropriate FSM state.
type ActiveSession struct {
	// SessionID is the deterministic OOR session identifier.
	SessionID SessionID

	// State is the persisted session lifecycle state (cosigned or
	// awaiting_notify).
	State sessionState

	// Inputs is the persisted input lock set.
	Inputs []wire.OutPoint

	// ArkPSBT is the submitted Ark package.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs is the checkpoint PSBT set (co-signed for cosigned
	// state, finalized for awaiting_notify state).
	CheckpointPSBTs []*psbt.Packet
}

// CoSignedSnapshot is the minimal durable data required to resume an OOR
// session after the point-of-no-return.
type CoSignedSnapshot struct {
	// SessionID is the deterministic OOR session identifier.
	SessionID SessionID

	// Inputs is the persisted input lock set.
	Inputs []wire.OutPoint

	// ArkPSBT is the submitted Ark package.
	ArkPSBT *psbt.Packet

	// CoSignedCheckpointPSBTs is the operator co-signed checkpoint set.
	CoSignedCheckpointPSBTs []*psbt.Packet
}

// FinalizedPackage is the canonical finalized package persisted by the server.
type FinalizedPackage struct {
	// ArkPSBT is the finalized Ark package PSBT.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs is the finalized checkpoint set.
	FinalCheckpointPSBTs []*psbt.Packet
}

// DefaultSessionExpiry is the default session expiry used when persisting
// point-of-no-return snapshots.
const DefaultSessionExpiry = 24 * time.Hour

// serializePSBT encodes a PSBT packet to raw bytes.
func serializePSBT(pkt *psbt.Packet) ([]byte, error) {
	if pkt == nil {
		return nil, fmt.Errorf("psbt must be provided")
	}

	var b bytes.Buffer
	err := pkt.Serialize(&b)
	if err != nil {
		return nil, fmt.Errorf("serialize psbt: %w", err)
	}

	return b.Bytes(), nil
}

// deserializePSBT decodes a PSBT packet from raw bytes. The size cap
// bounds heap allocation during decoding so a single oversized blob
// submitted on the wire cannot force the server to materialize an
// arbitrarily large psbt.Packet.
func deserializePSBT(b []byte) (*psbt.Packet, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("psbt bytes must be provided")
	}

	if len(b) > MaxPSBTBytesPerRequest {
		return nil, fmt.Errorf(
			"psbt blob size %d exceeds max %d",
			len(b), MaxPSBTBytesPerRequest,
		)
	}

	pkt, err := psbt.NewFromRawBytes(bytes.NewReader(b), false)
	if err != nil {
		return nil, fmt.Errorf("parse psbt: %w", err)
	}

	return pkt, nil
}

// sessionIDBytes returns the session identifier as 32 bytes.
func sessionIDBytes(id SessionID) []byte {
	h := chainhash.Hash(id)
	return h[:]
}
