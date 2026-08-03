package db

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/wallet"
)

// PendingIntentStore groups the SQL methods needed to maintain the generic
// pending-intents outbox: a kind-agnostic header, per-kind detail tables, and
// the shared anchor table. One logical store spans the header, both detail
// tables, and the anchor table; splitting it would fragment the transaction
// boundary across the same table family.
//
//nolint:interfacebloat
type PendingIntentStore interface {
	UpsertPendingIntentHeader(ctx context.Context,
		arg sqlc.UpsertPendingIntentHeaderParams) error

	UpsertPendingBoardIntent(ctx context.Context,
		arg sqlc.UpsertPendingBoardIntentParams) error

	UpsertPendingSendIntent(ctx context.Context,
		arg sqlc.UpsertPendingSendIntentParams) error

	UpsertPendingIntentAnchor(ctx context.Context,
		arg sqlc.UpsertPendingIntentAnchorParams) error

	ListPendingBoardIntents(ctx context.Context) (
		[]sqlc.ListPendingBoardIntentsRow, error)

	ListPendingSendIntents(ctx context.Context) (
		[]sqlc.ListPendingSendIntentsRow, error)

	ListPendingIntentAnchorsByKind(ctx context.Context,
		kind string) ([]sqlc.PendingIntentAnchor, error)

	ClearPendingIntentAnchorByOutpoint(ctx context.Context,
		arg sqlc.ClearPendingIntentAnchorByOutpointParams) error

	DeleteOrphanedPendingBoardIntents(ctx context.Context) error

	DeleteOrphanedPendingSendIntents(ctx context.Context) error

	DeleteOrphanedPendingIntents(ctx context.Context) error

	DeletePendingIntentAnchorsByIntentID(ctx context.Context,
		intentID []byte) error

	DeletePendingBoardIntentByID(ctx context.Context, intentID []byte) error

	DeletePendingSendIntentByID(ctx context.Context, intentID []byte) error

	DeletePendingIntentByID(ctx context.Context, intentID []byte) error

	DeletePendingIntentAnchorsByKind(ctx context.Context, kind string) error

	DeletePendingBoardIntentsAll(ctx context.Context) error

	DeletePendingSendIntentsAll(ctx context.Context) error

	DeletePendingIntentsByKind(ctx context.Context, kind string) error
}

// BatchedPendingIntentStore combines the query surface with batched
// transaction execution.
type BatchedPendingIntentStore interface {
	PendingIntentStore
	BatchedTx[PendingIntentStore]
}

// PendingIntentPersistenceStore implements wallet.PendingIntentStore: the
// persistence half of the restart-safe intent outbox. Intents are written
// before the wallet publishes them to the round actor, anchors are cleared
// by the round-state checkpoint on adoption (see
// RoundPersistenceStore.CommitState), and the wallet's startup replay hook
// lists rows per kind to re-issue lost intents.
type PendingIntentPersistenceStore struct {
	db BatchedPendingIntentStore
}

// NewPendingIntentPersistenceStore creates a pending-intent store using the
// transaction executor pattern.
func NewPendingIntentPersistenceStore(
	db BatchedPendingIntentStore) *PendingIntentPersistenceStore {

	return &PendingIntentPersistenceStore{
		db: db,
	}
}

// UpsertPendingIntent atomically writes the intent header, its kind-specific
// detail row, and all of its anchor rows. Anchors already bound to another
// intent are rebound to this one (newest intent wins); any intent left
// anchor-less by the rebind is swept (detail + header) in the same
// transaction so stale parents never accumulate.
func (s *PendingIntentPersistenceStore) UpsertPendingIntent(ctx context.Context,
	intent wallet.PendingIntent) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q PendingIntentStore) error {
		err := q.UpsertPendingIntentHeader(
			ctx, sqlc.UpsertPendingIntentHeaderParams{
				IntentID:        intent.ID[:],
				Kind:            string(intent.Kind()),
				RequestedAtUnix: intent.RequestedAt,
			},
		)
		if err != nil {
			return fmt.Errorf("upsert pending intent header: %w",
				err)
		}

		if err := upsertPendingIntentDetail(
			ctx, q, intent,
		); err != nil {
			return err
		}

		for _, anchor := range intent.Anchors {
			err := q.UpsertPendingIntentAnchor(
				ctx, sqlc.UpsertPendingIntentAnchorParams{
					OutpointHash:  anchor.Hash[:],
					OutpointIndex: int32(anchor.Index),
					IntentID:      intent.ID[:],
				},
			)
			if err != nil {
				return fmt.Errorf("upsert pending intent "+
					"anchor: %w", err)
			}
		}

		// An anchor rebind above may have stolen the last anchor of an
		// older intent; sweep any detail + header row left without
		// anchors so the startup replay never sees an intent that can
		// no longer be adopted.
		return sweepOrphanedPendingIntents(ctx, q)
	})
}

// upsertPendingIntentDetail writes the kind-specific detail row for the
// intent by type-switching on the concrete payload.
func upsertPendingIntentDetail(ctx context.Context, q PendingIntentStore,
	intent wallet.PendingIntent) error {

	switch p := intent.Payload.(type) {
	case *wallet.BoardIntentPayload:
		// Store nil rather than a zero-length slice when there is no
		// custom policy so the columns round-trip as NULL on Postgres
		// (the x'' BYTEA pitfall) and the pk_script length CHECK holds.
		var policyTemplate, pkScript []byte
		if len(p.PolicyTemplate) > 0 {
			policyTemplate = p.PolicyTemplate
		}
		if len(p.PkScript) > 0 {
			pkScript = p.PkScript
		}

		err := q.UpsertPendingBoardIntent(
			ctx, sqlc.UpsertPendingBoardIntentParams{
				IntentID:           intent.ID[:],
				TargetVtxoCount:    int32(p.TargetVTXOCount),
				VtxoPolicyTemplate: policyTemplate,
				PkScript:           pkScript,
			},
		)
		if err != nil {
			return fmt.Errorf("upsert board intent detail: %w", err)
		}

		return nil

	case *wallet.SendOnChainIntentPayload:
		var sweepAll int32
		if p.SweepAll {
			sweepAll = 1
		}

		// Store nil rather than a zero-length slice when there is no
		// operator key so the column round-trips as NULL on Postgres
		// (the x'' BYTEA pitfall) and the CHECK on length holds.
		var operatorKey []byte
		if p.OperatorKey != nil {
			operatorKey = p.OperatorKey.SerializeCompressed()
		}

		err := q.UpsertPendingSendIntent(
			ctx, sqlc.UpsertPendingSendIntentParams{
				IntentID:        intent.ID[:],
				DestPkscript:    p.DestinationPkScript,
				TargetAmountSat: int64(p.TargetAmountSat),
				SweepAll:        sweepAll,
				OperatorKey:     operatorKey,
				VtxoExitDelay:   int32(p.VTXOExitDelay),
				DustLimitSat:    int64(p.DustLimit),
			},
		)
		if err != nil {
			return fmt.Errorf("upsert send intent detail: %w", err)
		}

		return nil

	default:
		return fmt.Errorf("unknown pending intent payload type %T",
			intent.Payload)
	}
}

// sweepOrphanedPendingIntents deletes detail rows and then header rows that
// no longer have any anchor, in the one transaction. Detail rows must go
// before the header because the detail tables foreign-key the header.
func sweepOrphanedPendingIntents(ctx context.Context,
	q PendingIntentStore) error {

	if err := q.DeleteOrphanedPendingBoardIntents(ctx); err != nil {
		return fmt.Errorf("sweep orphaned board intents: %w", err)
	}

	if err := q.DeleteOrphanedPendingSendIntents(ctx); err != nil {
		return fmt.Errorf("sweep orphaned send intents: %w", err)
	}

	if err := q.DeleteOrphanedPendingIntents(ctx); err != nil {
		return fmt.Errorf("sweep orphaned pending intents: %w", err)
	}

	return nil
}

// ListPendingIntents returns every persisted intent of the given kind with
// its surviving anchors, ordered by requested_at_unix ascending.
func (s *PendingIntentPersistenceStore) ListPendingIntents(ctx context.Context,
	kind wallet.PendingIntentKind) ([]wallet.PendingIntent, error) {

	readTxOpts := ReadTxOption()

	var result []wallet.PendingIntent

	err := s.db.ExecTx(ctx, readTxOpts, func(q PendingIntentStore) error {
		anchors, err := loadPendingIntentAnchors(ctx, q, kind)
		if err != nil {
			return err
		}

		switch kind {
		case wallet.PendingIntentKindBoard:
			result, err = listPendingBoardIntents(ctx, q, anchors)

		case wallet.PendingIntentKindSendOnChain:
			result, err = listPendingSendIntents(ctx, q, anchors)

		default:
			return fmt.Errorf("unknown pending intent kind %q",
				kind)
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// loadPendingIntentAnchors groups the anchors of the given kind by intent ID.
func loadPendingIntentAnchors(ctx context.Context, q PendingIntentStore,
	kind wallet.PendingIntentKind) (
	map[wallet.PendingIntentID][]wire.OutPoint, error) {

	anchorRows, err := q.ListPendingIntentAnchorsByKind(ctx, string(kind))
	if err != nil {
		return nil, fmt.Errorf("list pending intent anchors: %w", err)
	}

	anchors := make(map[wallet.PendingIntentID][]wire.OutPoint)
	for _, row := range anchorRows {
		id, err := pendingIntentID(row.IntentID)
		if err != nil {
			return nil, err
		}

		// NewHash validates the exact 32-byte length, so a short or
		// corrupt blob surfaces as an error rather than a silently
		// zero-padded outpoint.
		hash, err := chainhash.NewHash(row.OutpointHash)
		if err != nil {
			return nil, err
		}

		anchors[id] = append(anchors[id], wire.OutPoint{
			Hash:  *hash,
			Index: uint32(row.OutpointIndex),
		})
	}

	return anchors, nil
}

// listPendingBoardIntents builds the board-kind pending intents from the
// board detail table joined with the supplied anchor set.
func listPendingBoardIntents(ctx context.Context, q PendingIntentStore,
	anchors map[wallet.PendingIntentID][]wire.OutPoint) (
	[]wallet.PendingIntent, error) {

	rows, err := q.ListPendingBoardIntents(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pending board intents: %w", err)
	}

	intents := make([]wallet.PendingIntent, 0, len(rows))
	for _, row := range rows {
		id, err := pendingIntentID(row.IntentID)
		if err != nil {
			return nil, err
		}

		intents = append(intents, wallet.PendingIntent{
			ID: id,
			Payload: &wallet.BoardIntentPayload{
				TargetVTXOCount: uint32(row.TargetVtxoCount),
				PolicyTemplate:  row.VtxoPolicyTemplate,
				PkScript:        row.PkScript,
			},
			RequestedAt: row.RequestedAtUnix,
			Anchors:     anchors[id],
		})
	}

	return intents, nil
}

// listPendingSendIntents builds the send-kind pending intents from the send
// detail table joined with the supplied anchor set.
func listPendingSendIntents(ctx context.Context, q PendingIntentStore,
	anchors map[wallet.PendingIntentID][]wire.OutPoint) (
	[]wallet.PendingIntent, error) {

	rows, err := q.ListPendingSendIntents(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pending send intents: %w", err)
	}

	intents := make([]wallet.PendingIntent, 0, len(rows))
	for _, row := range rows {
		id, err := pendingIntentID(row.IntentID)
		if err != nil {
			return nil, err
		}

		var operatorKey *btcec.PublicKey
		if len(row.OperatorKey) > 0 {
			operatorKey, err = btcec.ParsePubKey(row.OperatorKey)
			if err != nil {
				return nil, fmt.Errorf("parse persisted "+
					"operator key: %w", err)
			}
		}

		intents = append(intents, wallet.PendingIntent{
			ID: id,
			Payload: &wallet.SendOnChainIntentPayload{
				DestinationPkScript: row.DestPkscript,
				TargetAmountSat: btcutil.Amount(
					row.TargetAmountSat,
				),
				SweepAll:      row.SweepAll == 1,
				OperatorKey:   operatorKey,
				VTXOExitDelay: uint32(row.VtxoExitDelay),
				DustLimit: btcutil.Amount(
					row.DustLimitSat,
				),
			},
			RequestedAt: row.RequestedAtUnix,
			Anchors:     anchors[id],
		})
	}

	return intents, nil
}

// pendingIntentID converts a stored 32-byte id blob into a PendingIntentID,
// rejecting a wrong length rather than silently truncating.
func pendingIntentID(b []byte) (wallet.PendingIntentID, error) {
	var id wallet.PendingIntentID
	if len(b) != len(id) {
		return id, fmt.Errorf("invalid intent id length %d", len(b))
	}
	copy(id[:], b)

	return id, nil
}

// DeletePendingIntent removes one intent (anchors, both detail tables, then
// the header) atomically. The detail delete for the non-matching kind is a
// harmless no-op since intent IDs are unique across kinds.
func (s *PendingIntentPersistenceStore) DeletePendingIntent(ctx context.Context,
	id wallet.PendingIntentID) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q PendingIntentStore) error {
		err := q.DeletePendingIntentAnchorsByIntentID(ctx, id[:])
		if err != nil {
			return fmt.Errorf("delete pending intent anchors: %w",
				err)
		}

		if err := q.DeletePendingBoardIntentByID(
			ctx, id[:],
		); err != nil {
			return fmt.Errorf("delete board intent detail: %w", err)
		}

		if err := q.DeletePendingSendIntentByID(
			ctx, id[:],
		); err != nil {
			return fmt.Errorf("delete send intent detail: %w", err)
		}

		if err := q.DeletePendingIntentByID(ctx, id[:]); err != nil {
			return fmt.Errorf("delete pending intent: %w", err)
		}

		return nil
	})
}

// ClearPendingIntentsByKind removes every persisted intent of the given kind
// (anchors, detail rows, header rows), atomically.
func (s *PendingIntentPersistenceStore) ClearPendingIntentsByKind(
	ctx context.Context, kind wallet.PendingIntentKind) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q PendingIntentStore) error {
		err := q.DeletePendingIntentAnchorsByKind(ctx, string(kind))
		if err != nil {
			return fmt.Errorf("delete pending intent anchors by "+
				"kind: %w", err)
		}

		if err := clearPendingIntentDetail(ctx, q, kind); err != nil {
			return err
		}

		err = q.DeletePendingIntentsByKind(ctx, string(kind))
		if err != nil {
			return fmt.Errorf("delete pending intents by kind: %w",
				err)
		}

		return nil
	})
}

// clearPendingIntentDetail deletes all detail rows for the given kind. The
// detail tables hold only rows of their own kind, so an all-rows delete on
// the matching table is the kind-scoped clear.
func clearPendingIntentDetail(ctx context.Context, q PendingIntentStore,
	kind wallet.PendingIntentKind) error {

	switch kind {
	case wallet.PendingIntentKindBoard:
		if err := q.DeletePendingBoardIntentsAll(ctx); err != nil {
			return fmt.Errorf("delete board intent details: %w",
				err)
		}

		return nil

	case wallet.PendingIntentKindSendOnChain:
		if err := q.DeletePendingSendIntentsAll(ctx); err != nil {
			return fmt.Errorf("delete send intent details: %w", err)
		}

		return nil

	default:
		return fmt.Errorf("unknown pending intent kind %q", kind)
	}
}

// Compile-time check that the persistence store satisfies the wallet's
// pending-intent outbox interface.
var _ wallet.PendingIntentStore = (*PendingIntentPersistenceStore)(nil)
