package wallet

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// PendingIntentKind discriminates the intent types persisted in the generic
// pending-intents outbox. Each kind owns a concrete payload type and
// registers a replayer with the wallet actor that knows how to reconcile
// persisted rows against live state and re-issue the original command after a
// daemon restart.
type PendingIntentKind string

const (
	// PendingIntentKindBoard is a user-issued Board RPC: sweep all
	// confirmed boarding UTXOs into the next round. Anchored to the
	// confirmed boarding outpoints the call admitted.
	PendingIntentKindBoard PendingIntentKind = "board"

	// PendingIntentKindSendOnChain is a user-issued SendOnChain RPC: an
	// exact-amount (or sweep-all) onchain payment funded by forfeiting
	// VTXOs in the next round. Anchored to the reserved forfeit VTXO
	// outpoints.
	PendingIntentKindSendOnChain PendingIntentKind = "send_onchain"
)

// PendingIntentID is the 32-byte identifier of one persisted pending intent.
// It is derived deterministically from the payload's kind, the sorted
// anchors, and the payload's canonical field encoding (see
// NewPendingIntentID), so re-persisting the same logical intent is a natural
// upsert rather than a duplicate row.
type PendingIntentID [32]byte

// PendingIntentPayload is the kind-specific parameter set of a pending
// intent. Each kind has a concrete implementation (BoardIntentPayload,
// SendOnChainIntentPayload) whose fields the persistence layer stores as
// first-class typed columns, so no opaque blob crosses the wallet/db
// boundary. The interface is sealed: only the payload types in this package
// satisfy it.
type PendingIntentPayload interface {
	// Kind reports which intent kind (and which detail table) this
	// payload belongs to.
	Kind() PendingIntentKind

	// writeIDDigest writes the canonical, deterministic encoding of the
	// payload's fields into w. It feeds the intent-ID hash only and is
	// never persisted; storage uses typed columns. The encoding must be
	// stable across releases or persisted intent IDs would shift.
	writeIDDigest(w io.Writer)

	// sealPendingIntentPayload seals the interface to this package.
	sealPendingIntentPayload()
}

// BoardIntentPayload carries the Board replay parameters: everything beyond
// the anchor outpoints needed to re-issue the original BoardRequest after a
// restart.
type BoardIntentPayload struct {
	// TargetVTXOCount mirrors BoardRequest.TargetVTXOCount: zero means
	// "collapse the confirmed boarding balance into one VTXO", non-zero
	// fans the balance into that many VTXOs.
	TargetVTXOCount uint32

	// PolicyTemplate mirrors BoardRequest.PolicyTemplate: the serialized
	// arkscript policy the boarded outputs adopt, or nil for the standard
	// collaborative policy. Persisted so restart replay recreates the same
	// custom output rather than silently re-boarding into the standard
	// shape.
	PolicyTemplate []byte

	// PkScript mirrors BoardRequest.PkScript: the pinned taproot output
	// script, or nil to derive it from PolicyTemplate.
	PkScript []byte
}

// Kind reports the board intent kind.
func (p *BoardIntentPayload) Kind() PendingIntentKind {
	return PendingIntentKindBoard
}

// writeIDDigest writes the canonical field encoding for ID derivation. Byte
// slices are length-prefixed so two boards differing only by policy (or by an
// empty vs a set script that shares a prefix) can never collide on the same
// intent ID.
func (p *BoardIntentPayload) writeIDDigest(w io.Writer) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], p.TargetVTXOCount)
	_, _ = w.Write(b[:])

	writeLenPrefixed(w, p.PolicyTemplate)
	writeLenPrefixed(w, p.PkScript)
}

func (p *BoardIntentPayload) sealPendingIntentPayload() {}

// SendOnChainIntentPayload carries the SendOnChain replay parameters. Every
// field the replay rebuild needs beyond the anchor (forfeit) outpoints lives
// here; the anchors themselves are the reserved forfeit set and are not
// duplicated into the payload. The forfeited VTXO amounts are intentionally
// absent: replay re-reads the canonical amounts from the VTXO store after
// re-reserving the exact anchors.
type SendOnChainIntentPayload struct {
	// DestinationPkScript is the on-chain destination script of the
	// leave output.
	DestinationPkScript []byte

	// TargetAmountSat is the exact amount to land at the destination.
	// Zero in sweep-all mode.
	TargetAmountSat btcutil.Amount

	// SweepAll marks the sweep-all mode where the single leave output
	// absorbs the seal-time residual instead of a fixed amount plus a
	// change VTXO.
	SweepAll bool

	// OperatorKey is the operator pubkey for the change-VTXO policy
	// template. Nil in sweep-all mode (no change VTXO is built).
	OperatorKey *btcec.PublicKey

	// VTXOExitDelay is the CSV delay of the change VTXO's exit path.
	// Unused in sweep-all mode.
	VTXOExitDelay uint32

	// DustLimit is the change-VTXO dust floor used for the defensive
	// re-validation on replay. Unused in sweep-all mode.
	DustLimit btcutil.Amount
}

// Kind reports the send-onchain intent kind.
func (p *SendOnChainIntentPayload) Kind() PendingIntentKind {
	return PendingIntentKindSendOnChain
}

// writeIDDigest writes the canonical field encoding for ID derivation. Byte
// slices are length-prefixed so distinct field boundaries can never collide
// (e.g. an empty operator key vs a destination script that happens to share
// a prefix).
func (p *SendOnChainIntentPayload) writeIDDigest(w io.Writer) {
	writeLenPrefixed(w, p.DestinationPkScript)

	var amt [8]byte
	binary.BigEndian.PutUint64(amt[:], uint64(p.TargetAmountSat))
	_, _ = w.Write(amt[:])

	var sweep [1]byte
	if p.SweepAll {
		sweep[0] = 1
	}
	_, _ = w.Write(sweep[:])

	var opKey []byte
	if p.OperatorKey != nil {
		opKey = p.OperatorKey.SerializeCompressed()
	}
	writeLenPrefixed(w, opKey)

	var delay [4]byte
	binary.BigEndian.PutUint32(delay[:], p.VTXOExitDelay)
	_, _ = w.Write(delay[:])

	var dust [8]byte
	binary.BigEndian.PutUint64(dust[:], uint64(p.DustLimit))
	_, _ = w.Write(dust[:])
}

func (p *SendOnChainIntentPayload) sealPendingIntentPayload() {}

// writeLenPrefixed writes a 4-byte big-endian length followed by b, so a
// hash over a sequence of fields is unambiguous regardless of field values.
func writeLenPrefixed(w io.Writer, b []byte) {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(b)))
	_, _ = w.Write(l[:])
	_, _ = w.Write(b)
}

// PendingIntent is one restart-safe user intent persisted in the outbox. A
// row exists from the moment the daemon accepts the request until a round
// durably adopts it: the round-state checkpoint clears the intent's anchors
// (and then the orphaned detail + header rows) inside the same SQL
// transaction that records adoption, so replay-after-adoption is
// structurally impossible.
type PendingIntent struct {
	// ID uniquely identifies the intent. See NewPendingIntentID.
	ID PendingIntentID

	// Payload is the concrete, kind-specific parameter set. Its Kind()
	// selects the detail table and the replayer.
	Payload PendingIntentPayload

	// RequestedAt is the unix timestamp when the user issued the intent.
	RequestedAt int64

	// Anchors is the set of outpoints the round consumes when it adopts
	// the intent: confirmed boarding outpoints for Board, reserved
	// forfeit VTXO outpoints for SendOnChain. An anchor that is no
	// longer live in the anchoring domain marks the intent stale.
	Anchors []wire.OutPoint
}

// Kind returns the intent's kind, delegating to its payload.
func (i PendingIntent) Kind() PendingIntentKind {
	return i.Payload.Kind()
}

// PendingIntentStore is the persistence surface of the generic
// pending-intents outbox. The wallet writes intents before publishing them
// to the round actor (persist-before-publish), the round-state checkpoint
// clears anchors on adoption, and the wallet's startup replay hook lists
// rows per kind to re-issue lost intents.
type PendingIntentStore interface {
	// UpsertPendingIntent atomically writes the intent header, its
	// kind-specific detail row, and all of its anchor rows. An anchor
	// already bound to another intent is rebound to this one (newest
	// intent wins), and any intent left with zero anchors by the rebind
	// is deleted in the same transaction.
	UpsertPendingIntent(ctx context.Context, intent PendingIntent) error

	// ListPendingIntents returns every persisted intent of the given
	// kind together with its surviving anchors, ordered by
	// requested_at_unix ascending. An empty slice is returned when
	// nothing is pending.
	ListPendingIntents(ctx context.Context,
		kind PendingIntentKind) ([]PendingIntent, error)

	// DeletePendingIntent removes one intent (header, detail, anchors).
	// Used by replayers to discard a stale intent whose anchors are no
	// longer live.
	DeletePendingIntent(ctx context.Context, id PendingIntentID) error

	// ClearPendingIntentsByKind removes every persisted intent of the
	// given kind. Used by the startup sweep when no anchor of any
	// persisted intent is still live, so the next start is a no-op.
	ClearPendingIntentsByKind(ctx context.Context,
		kind PendingIntentKind) error
}

// PendingIntentReplayer reconciles the persisted intents of one kind
// against live state after a daemon restart and re-issues the original
// command (via a self-Tell into the wallet's own mailbox) for every intent
// that is still live. Stale intents — those whose anchors no longer resolve
// in the anchoring domain — are cleared so the next start is a no-op.
type PendingIntentReplayer interface {
	// Kind returns the intent kind this replayer owns.
	Kind() PendingIntentKind

	// Replay reconciles and re-issues the given persisted intents. It
	// returns true when at least one intent was re-issued, false when
	// everything was stale (and has been cleared).
	Replay(ctx context.Context, intents []PendingIntent) (bool, error)
}

// NewPendingIntentID derives the deterministic intent identifier by hashing
// the payload's kind, the sorted anchor outpoints, and the payload's
// canonical field encoding. Sorting makes the ID independent of anchor
// enumeration order, so the same logical intent always maps to the same row
// regardless of how the caller assembled it.
func NewPendingIntentID(payload PendingIntentPayload,
	anchors []wire.OutPoint) PendingIntentID {

	sorted := slices.Clone(anchors)
	slices.SortFunc(sorted, func(a, b wire.OutPoint) int {
		if c := bytes.Compare(a.Hash[:], b.Hash[:]); c != 0 {
			return c
		}

		return cmp.Compare(a.Index, b.Index)
	})

	h := sha256.New()
	h.Write([]byte(payload.Kind()))
	for _, op := range sorted {
		h.Write(op.Hash[:])

		var idx [4]byte
		binary.BigEndian.PutUint32(idx[:], op.Index)
		h.Write(idx[:])
	}
	payload.writeIDDigest(h)

	var id PendingIntentID
	copy(id[:], h.Sum(nil))

	return id
}
