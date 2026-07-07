package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	types "github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// Direction codes persisted in oor_packages.direction.
	oorPackageDirectionIncomingCode int32 = 0
	oorPackageDirectionOutgoingCode int32 = 1

	// Link-kind codes persisted in oor_vtxo_bindings.link_kind.
	oorPackageLinkKindCreatedOutputCode int32 = 0
	oorPackageLinkKindConsumedInputCode int32 = 1
)

// OORPackageDirection is the typed package direction enum.
type OORPackageDirection = types.OORPackageDirection

const (
	// OORPackageDirectionIncoming marks packages received by this client.
	OORPackageDirectionIncoming = types.OORPackageDirectionIncoming

	// OORPackageDirectionOutgoing marks packages sent by this client.
	OORPackageDirectionOutgoing = types.OORPackageDirectionOutgoing
)

// OORPackageLinkKind is the typed outpoint-binding relation enum.
type OORPackageLinkKind = types.OORPackageLinkKind

const (
	// OORPackageLinkKindCreatedOutput identifies bindings where the local
	// outpoint was created by the Ark transaction.
	OORPackageLinkKindCreatedOutput = types.OORPackageLinkKindCreatedOutput

	// OORPackageLinkKindConsumedInput identifies bindings where the local
	// outpoint was consumed as an OOR input.
	OORPackageLinkKindConsumedInput = types.OORPackageLinkKindConsumedInput
)

// OwnedReceiveScriptSource is the script-registration source enum.
type OwnedReceiveScriptSource int32

const (
	// OwnedReceiveScriptSourceWallet marks scripts discovered from
	// wallet state.
	OwnedReceiveScriptSourceWallet OwnedReceiveScriptSource = 0

	// OwnedReceiveScriptSourceRPC marks scripts registered from RPC/API.
	OwnedReceiveScriptSourceRPC OwnedReceiveScriptSource = 1

	// OwnedReceiveScriptSourceSync marks scripts restored from
	// sync/recovery.
	OwnedReceiveScriptSourceSync OwnedReceiveScriptSource = 2
)

func (s OwnedReceiveScriptSource) String() string {
	switch s {
	case OwnedReceiveScriptSourceWallet:
		return "wallet"

	case OwnedReceiveScriptSourceRPC:
		return "rpc"

	case OwnedReceiveScriptSourceSync:
		return "sync"

	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// OORArtifactStore groups SQL methods needed by OOR artifact persistence.
//
//nolint:interfacebloat
type OORArtifactStore interface {
	// InternalKeyQuerier lets the store register and hydrate the owned
	// receive-script client key via the shared internal_keys registry
	// within its own transaction.
	InternalKeyQuerier

	UpsertOORPackage(ctx context.Context,
		arg sqlc.UpsertOORPackageParams) (int64, error)

	DeleteOORPackageCheckpoints(ctx context.Context, sessionID []byte) error

	InsertOORPackageCheckpoint(ctx context.Context,
		arg sqlc.InsertOORPackageCheckpointParams) error

	GetOORPackage(ctx context.Context,
		sessionID []byte) (sqlc.OorPackage, error)

	ListOORPackageCheckpoints(ctx context.Context,
		sessionID []byte) ([]sqlc.OorPackageCheckpoint, error)

	ListOORPackages(ctx context.Context) ([]sqlc.OorPackage, error)

	ListOORPackagesByDirection(ctx context.Context,
		direction int32) ([]sqlc.OorPackage, error)

	UpsertOORVTXOBinding(ctx context.Context,
		arg sqlc.UpsertOORVTXOBindingParams) (int64, error)

	GetOORVTXOBindingByOutpoint(ctx context.Context,
		arg sqlc.GetOORVTXOBindingByOutpointParams) (
		sqlc.OorVtxoBinding, error)

	GetOORVTXOBindingByOutpointAndKind(ctx context.Context,
		arg sqlc.GetOORVTXOBindingByOutpointAndKindParams) (
		sqlc.OorVtxoBinding, error)

	GetVTXO(ctx context.Context, arg sqlc.GetVTXOParams) (sqlc.Vtxo, error)

	ListOORVTXOBindingsBySession(ctx context.Context,
		sessionID []byte) (
		[]sqlc.ListOORVTXOBindingsBySessionRow,
		error,
	)

	GetOORPackageByOutpoint(ctx context.Context,
		arg sqlc.GetOORPackageByOutpointParams) (
		sqlc.GetOORPackageByOutpointRow, error)

	GetOORPackageByOutpointAndKind(ctx context.Context,
		arg sqlc.GetOORPackageByOutpointAndKindParams) (
		sqlc.GetOORPackageByOutpointAndKindRow, error)

	UpsertOORRecipientCursor(ctx context.Context,
		arg sqlc.UpsertOORRecipientCursorParams) error

	GetOORRecipientCursor(ctx context.Context,
		recipientPkScript []byte) (sqlc.OorRecipientCursor, error)

	ListOORRecipientCursors(ctx context.Context) (
		[]sqlc.OorRecipientCursor,
		error,
	)

	UpsertOwnedReceiveScript(ctx context.Context,
		arg sqlc.UpsertOwnedReceiveScriptParams) error

	GetOwnedReceiveScript(ctx context.Context,
		pkScript []byte) (sqlc.OwnedReceiveScript, error)

	ListOwnedReceiveScripts(ctx context.Context) (
		[]sqlc.OwnedReceiveScript,
		error,
	)
}

// BatchedOORArtifactStore combines OOR artifact queries with batched
// transaction execution.
type BatchedOORArtifactStore interface {
	OORArtifactStore
	BatchedTx[OORArtifactStore]
}

// OORPackageBinding is a local outpoint link to a stored OOR package.
type OORPackageBinding struct {
	// Outpoint is the local VTXO outpoint linked to the package.
	Outpoint wire.OutPoint

	// SessionID is the package session identifier (Ark txid).
	SessionID chainhash.Hash

	// OutputIndex is the package output index (or enumerated input index
	// for consumed-input bindings).
	OutputIndex uint32

	// LinkKind identifies relation to the package
	// (created_output/consumed_input).
	LinkKind OORPackageLinkKind

	// RecipientPkScript is populated for created-output bindings.
	RecipientPkScript fn.Option[[]byte]

	// ValueSat is populated for created-output bindings when known.
	ValueSat fn.Option[int64]

	// CreatedAt is the binding creation timestamp.
	CreatedAt time.Time

	// UpdatedAt is the last binding update timestamp.
	UpdatedAt time.Time
}

// OORPackageBundle is a fully materialized OOR package view.
type OORPackageBundle struct {
	// SessionID is the stable session identifier.
	SessionID chainhash.Hash

	// Direction is incoming or outgoing from the local client perspective.
	Direction OORPackageDirection

	// ArkPSBT is the persisted Ark package.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs is the persisted finalized checkpoint package.
	FinalCheckpointPSBTs []*psbt.Packet

	// Bindings are all known local outpoint links for this session.
	Bindings []OORPackageBinding

	// CreatedAt is the package creation timestamp.
	CreatedAt time.Time

	// UpdatedAt is the package update timestamp.
	UpdatedAt time.Time

	// MatchedOutpointBinding is set on GetPackageForOutpoint responses.
	MatchedOutpointBinding fn.Option[OORPackageBinding]
}

// OwnedReceiveScriptRecord is a local script ownership registration row.
type OwnedReceiveScriptRecord struct {
	// PkScript is the tracked receive script.
	PkScript []byte

	// ClientKey is the local wallet key descriptor for this script.
	ClientKey keychain.KeyDescriptor

	// OperatorPubKey is the operator pubkey for this script.
	OperatorPubKey *btcec.PublicKey

	// ExitDelay is the relative CSV delay.
	ExitDelay int64

	// Source labels where this script registration came from.
	Source OwnedReceiveScriptSource

	// CreatedAt is the creation timestamp.
	CreatedAt time.Time

	// LastUsedAt tracks the last usage timestamp when available.
	LastUsedAt fn.Option[time.Time]
}

// OORArtifactPersistenceStore persists OOR artifacts and query surfaces needed
// for unroll package retrieval.
type OORArtifactPersistenceStore struct {
	db    BatchedOORArtifactStore
	clock clock.Clock

	// maxUnrollDepth bounds local ancestry traversal during unroll package
	// resolution.
	maxUnrollDepth int
}

// NewOORArtifactPersistenceStore constructs an OOR artifact store backed by
// batched SQL transactions.
//
// The returned store is safe to use across independent requests because each
// public method executes in its own transaction scope. If no clock is
// provided, a default wall-clock implementation is used.
func NewOORArtifactPersistenceStore(db BatchedOORArtifactStore,
	c clock.Clock) *OORArtifactPersistenceStore {

	if c == nil {
		c = clock.NewDefaultClock()
	}

	return &OORArtifactPersistenceStore{
		db:             db,
		clock:          c,
		maxUnrollDepth: defaultMaxUnrollDepth,
	}
}

// UpsertPackage writes or replaces one finalized OOR package identified by
// session ID.
//
// The method persists the Ark PSBT and then rewrites the checkpoint set in
// contiguous index order. Existing checkpoint rows for the session are removed
// first so retries always converge to the latest canonical package.
func (s *OORArtifactPersistenceStore) UpsertPackage(ctx context.Context,
	direction OORPackageDirection, sessionID chainhash.Hash,
	ark *psbt.Packet, checkpoints []*psbt.Packet) error {

	if s == nil || s.db == nil {
		return fmt.Errorf("store must be provided")
	}

	if err := validatePackageDirection(direction); err != nil {
		return err
	}
	directionCode, err := packageDirectionCode(direction)
	if err != nil {
		return err
	}

	if ark == nil {
		return fmt.Errorf("ark psbt must be provided")
	}

	if len(checkpoints) == 0 {
		return fmt.Errorf("checkpoint psbts must be provided")
	}

	arkRaw, err := psbtutil.Serialize(ark)
	if err != nil {
		return err
	}

	rawCheckpoints := make([][]byte, 0, len(checkpoints))
	for i := range checkpoints {
		raw, err := psbtutil.Serialize(checkpoints[i])
		if err != nil {
			return fmt.Errorf("serialize checkpoint %d: %w", i, err)
		}

		rawCheckpoints = append(rawCheckpoints, raw)
	}

	now := s.clock.Now().Unix()
	id := sessionID[:]

	writeTx := WriteTxOption()

	return s.db.ExecTx(ctx, writeTx, func(q OORArtifactStore) error {
		existing, err := q.GetOORPackage(ctx, id)
		switch {
		case err == nil:
			existingDirection, err := packageDirectionFromCode(
				existing.Direction,
			)
			if err != nil {
				return err
			}

			if existingDirection != direction {
				return packageDirectionConflictErr(
					id, existingDirection, direction,
				)
			}

			samePayload, err := sameOORPackagePayload(
				ctx, q, existing, arkRaw, rawCheckpoints,
			)
			if err != nil {
				return err
			}
			if !samePayload {
				return fmt.Errorf("oor package %x already "+
					"exists with different payload", id)
			}

			return nil

		case errors.Is(err, sql.ErrNoRows):
			// New package insert path.

		default:
			return err
		}

		rowsAffected, err := q.UpsertOORPackage(
			ctx, sqlc.UpsertOORPackageParams{
				SessionID: id,
				Direction: directionCode,
				ArkPsbt:   arkRaw,
				CreatedAt: now,
				UpdatedAt: now,
			},
		)
		if err != nil {
			return err
		}
		if rowsAffected == 0 {
			existing, err := q.GetOORPackage(ctx, id)
			if err != nil {
				return err
			}

			existingDirection, err := packageDirectionFromCode(
				existing.Direction,
			)
			if err != nil {
				return err
			}

			if existingDirection != direction {
				return packageDirectionConflictErr(
					id, existingDirection, direction,
				)
			}
		}

		err = q.DeleteOORPackageCheckpoints(ctx, id)
		if err != nil {
			return err
		}

		for i := range rawCheckpoints {
			err := q.InsertOORPackageCheckpoint(ctx,
				sqlc.InsertOORPackageCheckpointParams{
					SessionID:       id,
					CheckpointIndex: int32(i),
					CheckpointPsbt:  rawCheckpoints[i],
					CreatedAt:       now,
				},
			)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// UpsertBinding writes or updates one local outpoint-to-session binding.
//
// Bindings are used as the primary lookup surface for unroll preparation.
// created_output bindings map newly received outputs, while consumed_input
// bindings map local outpoints spent by an outgoing transfer.
func (s *OORArtifactPersistenceStore) UpsertBinding(ctx context.Context,
	outpoint wire.OutPoint, sessionID chainhash.Hash, outputIndex uint32,
	linkKind OORPackageLinkKind) error {

	if s == nil || s.db == nil {
		return fmt.Errorf("store must be provided")
	}

	if err := validateBindingKind(linkKind); err != nil {
		return err
	}
	linkKindCode, err := bindingKindCode(linkKind)
	if err != nil {
		return err
	}

	now := s.clock.Now().Unix()

	writeTx := WriteTxOption()

	return s.db.ExecTx(ctx, writeTx, func(q OORArtifactStore) error {
		params := sqlc.UpsertOORVTXOBindingParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
			SessionID:     sessionID[:],
			OutputIndex:   int32(outputIndex),
			LinkKind:      linkKindCode,
			CreatedAt:     now,
			UpdatedAt:     now,
		}

		_, err := q.GetVTXO(ctx, sqlc.GetVTXOParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
		})
		switch {
		case err == nil:
			// Continue.

		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %v",
				types.ErrOORBindingOutpointNotFound, outpoint)

		default:
			return err
		}

		existing, err := q.GetOORVTXOBindingByOutpointAndKind(
			ctx, sqlc.GetOORVTXOBindingByOutpointAndKindParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
				LinkKind:      linkKindCode,
			},
		)
		switch {
		case err == nil:
			if !bytes.Equal(existing.SessionID, sessionID[:]) {
				return fmt.Errorf("binding conflict for "+
					"outpoint %v (%s): already bound to "+
					"session %x, requested %x", outpoint,
					linkKind, existing.SessionID,
					sessionID[:])
			}
			if existing.OutputIndex != int32(outputIndex) {
				return fmt.Errorf("binding output index "+
					"conflict for outpoint %v (%s): "+
					"existing=%d requested=%d", outpoint,
					linkKind, existing.OutputIndex,
					outputIndex)
			}

		case errors.Is(err, sql.ErrNoRows):
			// New binding insert path.

		default:
			return err
		}

		rowsAffected, err := q.UpsertOORVTXOBinding(ctx, params)
		if err != nil {
			return err
		}
		if rowsAffected == 0 {
			lookupParams :=
				sqlc.GetOORVTXOBindingByOutpointAndKindParams{
					OutpointHash: outpoint.Hash[:],
					OutpointIndex: int32(
						outpoint.Index,
					),
					LinkKind: linkKindCode,
				}
			existing, err := q.GetOORVTXOBindingByOutpointAndKind(
				ctx, lookupParams,
			)
			if err != nil {
				return err
			}
			if !bytes.Equal(existing.SessionID, sessionID[:]) {
				return fmt.Errorf("binding conflict for "+
					"outpoint %v (%s): already bound to "+
					"session %x, requested %x", outpoint,
					linkKind, existing.SessionID,
					sessionID[:])
			}
			if existing.OutputIndex != int32(outputIndex) {
				return fmt.Errorf("binding output index "+
					"conflict for outpoint %v (%s): "+
					"existing=%d requested=%d", outpoint,
					linkKind, existing.OutputIndex,
					outputIndex)
			}
		}

		return nil
	})
}

// GetPackageForOutpoint returns the fully materialized package linked to one
// local outpoint.
//
// The response includes all persisted checkpoints, all known bindings for the
// session, and the specific binding row that matched the requested outpoint.
func (s *OORArtifactPersistenceStore) GetPackageForOutpoint(ctx context.Context,
	outpoint wire.OutPoint) (*OORPackageBundle, error) {

	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store must be provided")
	}

	readTx := ReadTxOption()

	var result *OORPackageBundle

	err := s.db.ExecTx(ctx, readTx, func(q OORArtifactStore) error {
		row, err := q.GetOORPackageByOutpoint(ctx,
			sqlc.GetOORPackageByOutpointParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
			},
		)
		if err != nil {
			return err
		}

		pkg, err := materializePackageBundle(ctx, q, sqlc.OorPackage{
			SessionID: row.SessionID,
			Direction: row.Direction,
			ArkPsbt:   row.ArkPsbt,
			CreatedAt: row.PackageCreatedAt,
			UpdatedAt: row.PackageUpdatedAt,
		})
		if err != nil {
			return err
		}

		matched, err := bindingFromOutpointJoinRow(row)
		if err != nil {
			return err
		}

		pkg.MatchedOutpointBinding = fn.Some(*matched)
		result = pkg

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// GetPackage returns the fully materialized package for one OOR session id.
func (s *OORArtifactPersistenceStore) GetPackage(ctx context.Context,
	sessionID chainhash.Hash) (*OORPackageBundle, error) {

	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store must be provided")
	}

	readTx := ReadTxOption()

	var result *OORPackageBundle
	err := s.db.ExecTx(ctx, readTx, func(q OORArtifactStore) error {
		row, err := q.GetOORPackage(ctx, sessionID[:])
		if err != nil {
			return err
		}

		pkg, err := materializePackageBundle(ctx, q, row)
		if err != nil {
			return err
		}

		result = pkg

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// ListPackages returns all persisted packages, optionally filtered by
// direction.
//
// Each returned entry is fully materialized with checkpoints and binding rows.
// Callers can therefore consume the result directly without additional per-row
// lookups.
func (s *OORArtifactPersistenceStore) ListPackages(ctx context.Context,
	direction *OORPackageDirection) ([]*OORPackageBundle, error) {

	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store must be provided")
	}

	readTx := ReadTxOption()

	var results []*OORPackageBundle

	err := s.db.ExecTx(ctx, readTx, func(q OORArtifactStore) error {
		var (
			rows []sqlc.OorPackage
			err  error
		)

		if direction == nil {
			rows, err = q.ListOORPackages(ctx)
		} else {
			if err = validatePackageDirection(
				*direction,
			); err != nil {
				return err
			}

			directionCode, mapErr := packageDirectionCode(
				*direction,
			)
			if mapErr != nil {
				return mapErr
			}

			rows, err = q.ListOORPackagesByDirection(
				ctx, directionCode,
			)
		}
		if err != nil {
			return err
		}

		out := make([]*OORPackageBundle, 0, len(rows))
		for i := range rows {
			pkg, err := materializePackageBundle(
				ctx, q, rows[i],
			)
			if err != nil {
				return err
			}

			out = append(out, pkg)
		}

		results = out

		return nil
	})
	if err != nil {
		return nil, err
	}

	return results, nil
}

// ListReceivedPackages lists fully materialized incoming OOR packages.
//
// This is a convenience wrapper around ListPackages with the incoming
// direction filter preselected.
func (s *OORArtifactPersistenceStore) ListReceivedPackages(
	ctx context.Context) ([]*OORPackageBundle, error) {

	direction := OORPackageDirectionIncoming

	return s.ListPackages(ctx, &direction)
}

// ListSentPackages lists fully materialized outgoing OOR packages.
//
// This is a convenience wrapper around ListPackages with the outgoing
// direction filter preselected.
func (s *OORArtifactPersistenceStore) ListSentPackages(ctx context.Context) (
	[]*OORPackageBundle, error) {

	direction := OORPackageDirectionOutgoing

	return s.ListPackages(ctx, &direction)
}

// UpsertRecipientCursor writes recipient polling progress for one script.
//
// The cursor enables at-least-once recipient event ingestion. Reprocessing the
// same or older events is safe because artifact writes are idempotent.
//
// NOTE: This tracks receiver-side polling state for server recipient events
// that can be expanded to finalized Ark/checkpoint package artifacts.
// TODO(oor-receive-polling): Wire runtime receiver polling to this cursor.
func (s *OORArtifactPersistenceStore) UpsertRecipientCursor(ctx context.Context,
	recipientPkScript []byte, lastEventID int64,
	lastSessionID *chainhash.Hash) error {

	if s == nil || s.db == nil {
		return fmt.Errorf("store must be provided")
	}

	if len(recipientPkScript) == 0 {
		return fmt.Errorf("recipient pk script must be provided")
	}

	var sessionID []byte
	if lastSessionID != nil {
		sessionID = lastSessionID[:]
	}

	writeTx := WriteTxOption()
	now := s.clock.Now().Unix()

	return s.db.ExecTx(ctx, writeTx, func(q OORArtifactStore) error {
		return q.UpsertOORRecipientCursor(ctx,
			sqlc.UpsertOORRecipientCursorParams{
				RecipientPkScript: recipientPkScript,
				LastEventID:       lastEventID,
				UpdatedAt:         now,
				LastSessionID:     sessionID,
			},
		)
	})
}

// GetRecipientCursor loads the persisted recipient cursor for one script.
//
// Callers can use this to resume polling from the last acknowledged event ID.
func (s *OORArtifactPersistenceStore) GetRecipientCursor(ctx context.Context,
	recipientPkScript []byte) (*sqlc.OorRecipientCursor, error) {

	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store must be provided")
	}

	readTx := ReadTxOption()

	var row sqlc.OorRecipientCursor

	err := s.db.ExecTx(ctx, readTx, func(q OORArtifactStore) error {
		var err error
		row, err = q.GetOORRecipientCursor(ctx, recipientPkScript)

		return err
	})
	if err != nil {
		return nil, err
	}

	return &row, nil
}

// ListRecipientCursors returns all recipient cursor rows ordered by update
// time.
//
// This is primarily used by orchestration code that needs to inspect or resume
// polling across all tracked scripts.
func (s *OORArtifactPersistenceStore) ListRecipientCursors(
	ctx context.Context) ([]sqlc.OorRecipientCursor, error) {

	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store must be provided")
	}

	readTx := ReadTxOption()

	var rows []sqlc.OorRecipientCursor

	err := s.db.ExecTx(ctx, readTx, func(q OORArtifactStore) error {
		var err error
		rows, err = q.ListOORRecipientCursors(ctx)

		return err
	})
	if err != nil {
		return nil, err
	}

	return rows, nil
}

// UpsertOwnedReceiveScript writes metadata describing one locally owned
// recipient script.
//
// The metadata links a script to key-derivation context and checkpoint policy,
// which allows incoming event ingestion to map recipient outputs directly to
// local wallet ownership.
//
// NOTE: This is receiver-side indexing metadata for polling/attribution, not
// sender-side transfer state.
func (s *OORArtifactPersistenceStore) UpsertOwnedReceiveScript(
	ctx context.Context, rec OwnedReceiveScriptRecord) error {

	if s == nil || s.db == nil {
		return fmt.Errorf("store must be provided")
	}

	if len(rec.PkScript) == 0 {
		return fmt.Errorf("pk script must be provided")
	}

	if rec.ClientKey.PubKey == nil {
		return fmt.Errorf("client key pubkey must be provided")
	}

	if rec.OperatorPubKey == nil {
		return fmt.Errorf("operator pubkey must be provided")
	}

	sourceCode, err := ownedReceiveScriptSourceCode(rec.Source)
	if err != nil {
		return err
	}

	writeTx := WriteTxOption()
	now := s.clock.Now().Unix()

	return s.db.ExecTx(ctx, writeTx, func(q OORArtifactStore) error {
		createdAtUnix := rec.CreatedAt.Unix()
		if rec.CreatedAt.IsZero() {
			createdAtUnix = now
		}

		lastUsedAt := nullUnixFromOptionTime(rec.LastUsedAt)
		operatorPubkey := rec.OperatorPubKey.
			SerializeCompressed()

		// Register the client wallet key in the shared internal_keys
		// registry and reference it by id.
		clientKeyID, err := RegisterInternalKeyTx(
			ctx, q, now, rec.ClientKey,
		)
		if err != nil {
			return fmt.Errorf("register client key: %w", err)
		}

		return q.UpsertOwnedReceiveScript(ctx,
			sqlc.UpsertOwnedReceiveScriptParams{
				PkScript: rec.PkScript,
				ClientKeyID: sql.NullInt64{
					Int64: clientKeyID,
					Valid: true,
				},
				OperatorPubkey: operatorPubkey,
				ExitDelay:      rec.ExitDelay,
				Source:         sourceCode,
				CreatedAt:      createdAtUnix,
				LastUsedAt:     lastUsedAt,
			},
		)
	})
}

// LookupOwnedReceiveScript loads one owned receive script metadata row by
// pkScript.
//
// This is a direct lookup path used by receive-side attribution logic.
func (s *OORArtifactPersistenceStore) LookupOwnedReceiveScript(
	ctx context.Context, pkScript []byte) (*OwnedReceiveScriptRecord,
	error) {

	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store must be provided")
	}

	readTx := ReadTxOption()

	var result *OwnedReceiveScriptRecord

	err := s.db.ExecTx(ctx, readTx, func(q OORArtifactStore) error {
		row, err := q.GetOwnedReceiveScript(ctx, pkScript)
		if err != nil {
			return err
		}

		rec, err := ownedReceiveScriptRowToRecord(ctx, q, row)
		if err != nil {
			return err
		}

		result = rec

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// ownedReceiveScriptRowToRecord converts a stored owned receive-script row to
// its domain record, hydrating the client key descriptor from the
// internal_keys registry within the caller's query context.
func ownedReceiveScriptRowToRecord(ctx context.Context, q OORArtifactStore,
	row sqlc.OwnedReceiveScript) (*OwnedReceiveScriptRecord, error) {

	clientKey, err := unmarshalClientKey(ctx, q, row)
	if err != nil {
		return nil, err
	}

	operatorPubKey, err := btcec.ParsePubKey(row.OperatorPubkey)
	if err != nil {
		return nil, fmt.Errorf("parse operator pubkey: %w", err)
	}

	source, err := ownedReceiveScriptSourceFromCode(row.Source)
	if err != nil {
		return nil, err
	}

	return &OwnedReceiveScriptRecord{
		PkScript:       row.PkScript,
		ClientKey:      clientKey,
		OperatorPubKey: operatorPubKey,
		ExitDelay:      row.ExitDelay,
		Source:         source,
		CreatedAt:      unixTimeUTC(row.CreatedAt),
		LastUsedAt:     optionTimeFromNullUnix(row.LastUsedAt),
	}, nil
}

// ListOwnedReceiveScripts returns all owned receive script registrations.
//
// The result is ordered by creation time descending and is intended for worker
// bootstrap and operator inspection.
func (s *OORArtifactPersistenceStore) ListOwnedReceiveScripts(
	ctx context.Context) ([]OwnedReceiveScriptRecord, error) {

	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store must be provided")
	}

	readTx := ReadTxOption()

	var records []OwnedReceiveScriptRecord

	err := s.db.ExecTx(ctx, readTx, func(q OORArtifactStore) error {
		rows, err := q.ListOwnedReceiveScripts(ctx)
		if err != nil {
			return err
		}

		records = make([]OwnedReceiveScriptRecord, 0, len(rows))
		for i := range rows {
			rec, err := ownedReceiveScriptRowToRecord(
				ctx, q, rows[i],
			)
			if err != nil {
				return err
			}

			records = append(records, *rec)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return records, nil
}

// materializePackageBundle assembles one fully populated package bundle from a
// package row, loading checkpoint and binding rows by session ID.
func materializePackageBundle(ctx context.Context, q OORArtifactStore,
	pkgRow sqlc.OorPackage) (*OORPackageBundle, error) {

	sessionHash, err := parseHash32(pkgRow.SessionID)
	if err != nil {
		return nil, err
	}

	ark, err := psbtutil.Parse(pkgRow.ArkPsbt)
	if err != nil {
		return nil, err
	}

	checkpointRows, err := q.ListOORPackageCheckpoints(
		ctx, pkgRow.SessionID,
	)
	if err != nil {
		return nil, err
	}

	checkpoints := make([]*psbt.Packet, 0, len(checkpointRows))
	for i := range checkpointRows {
		pkt, err := psbtutil.Parse(checkpointRows[i].CheckpointPsbt)
		if err != nil {
			return nil, fmt.Errorf("parse checkpoint %d: %w", i,
				err)
		}

		checkpoints = append(checkpoints, pkt)
	}

	bindingRows, err := q.ListOORVTXOBindingsBySession(
		ctx, pkgRow.SessionID,
	)
	if err != nil {
		return nil, err
	}

	bindings := make([]OORPackageBinding, 0, len(bindingRows))
	for i := range bindingRows {
		binding, err := bindingFromRow(bindingRows[i])
		if err != nil {
			return nil, err
		}

		bindings = append(bindings, *binding)
	}

	direction, err := packageDirectionFromCode(pkgRow.Direction)
	if err != nil {
		return nil, err
	}

	return &OORPackageBundle{
		SessionID:              sessionHash,
		Direction:              direction,
		ArkPSBT:                ark,
		FinalCheckpointPSBTs:   checkpoints,
		Bindings:               bindings,
		CreatedAt:              unixTimeUTC(pkgRow.CreatedAt),
		UpdatedAt:              unixTimeUTC(pkgRow.UpdatedAt),
		MatchedOutpointBinding: fn.None[OORPackageBinding](),
	}, nil
}

// bindingFromRow converts a raw binding row into the API binding shape.
func bindingFromRow(row sqlc.ListOORVTXOBindingsBySessionRow) (
	*OORPackageBinding, error) {

	sessionID, err := parseHash32(row.SessionID)
	if err != nil {
		return nil, err
	}

	outpointHash, err := parseHash32(row.OutpointHash)
	if err != nil {
		return nil, err
	}

	linkKind, err := bindingKindFromCode(row.LinkKind)
	if err != nil {
		return nil, err
	}

	recipientPkScript := deriveBindingRecipientPkScript(
		row.LinkKind, row.RecipientPkScript,
	)
	value := deriveBindingValueSat(row.LinkKind, row.ValueSat)

	return &OORPackageBinding{
		Outpoint: wire.OutPoint{
			Hash:  outpointHash,
			Index: uint32(row.OutpointIndex),
		},
		SessionID:         sessionID,
		OutputIndex:       uint32(row.OutputIndex),
		LinkKind:          linkKind,
		RecipientPkScript: recipientPkScript,
		ValueSat:          value,
		CreatedAt:         unixTimeUTC(row.CreatedAt),
		UpdatedAt:         unixTimeUTC(row.UpdatedAt),
	}, nil
}

// bindingFromOutpointJoinRow converts an outpoint join row into a binding.
func bindingFromOutpointJoinRow(row sqlc.GetOORPackageByOutpointRow) (
	*OORPackageBinding, error) {

	sessionID, err := parseHash32(row.SessionID)
	if err != nil {
		return nil, err
	}

	outpointHash, err := parseHash32(row.OutpointHash)
	if err != nil {
		return nil, err
	}

	linkKind, err := bindingKindFromCode(row.LinkKind)
	if err != nil {
		return nil, err
	}

	recipientPkScript := deriveBindingRecipientPkScript(
		row.LinkKind, row.RecipientPkScript,
	)
	value := deriveBindingValueSat(row.LinkKind, row.ValueSat)

	return &OORPackageBinding{
		Outpoint: wire.OutPoint{
			Hash:  outpointHash,
			Index: uint32(row.OutpointIndex),
		},
		SessionID:         sessionID,
		OutputIndex:       uint32(row.OutputIndex),
		LinkKind:          linkKind,
		RecipientPkScript: recipientPkScript,
		ValueSat:          value,
		CreatedAt:         unixTimeUTC(row.BindingCreatedAt),
		UpdatedAt:         unixTimeUTC(row.BindingUpdatedAt),
	}, nil
}

// bindingFromOutpointAndKindJoinRow converts an outpoint+kind join row into
// a binding.
func bindingFromOutpointAndKindJoinRow(
	row sqlc.GetOORPackageByOutpointAndKindRow) (*OORPackageBinding,
	error) {

	sessionID, err := parseHash32(row.SessionID)
	if err != nil {
		return nil, err
	}

	outpointHash, err := parseHash32(row.OutpointHash)
	if err != nil {
		return nil, err
	}

	linkKind, err := bindingKindFromCode(row.LinkKind)
	if err != nil {
		return nil, err
	}

	recipientPkScript := deriveBindingRecipientPkScript(
		row.LinkKind, row.RecipientPkScript,
	)
	value := deriveBindingValueSat(row.LinkKind, row.ValueSat)

	return &OORPackageBinding{
		Outpoint: wire.OutPoint{
			Hash:  outpointHash,
			Index: uint32(row.OutpointIndex),
		},
		SessionID:         sessionID,
		OutputIndex:       uint32(row.OutputIndex),
		LinkKind:          linkKind,
		RecipientPkScript: recipientPkScript,
		ValueSat:          value,
		CreatedAt:         unixTimeUTC(row.BindingCreatedAt),
		UpdatedAt:         unixTimeUTC(row.BindingUpdatedAt),
	}, nil
}

func deriveBindingRecipientPkScript(linkKindCode int32,
	pkScript []byte) fn.Option[[]byte] {

	if linkKindCode != oorPackageLinkKindCreatedOutputCode {
		return fn.None[[]byte]()
	}

	return fn.Some(append([]byte(nil), pkScript...))
}

func deriveBindingValueSat(linkKindCode int32,
	valueSat int64) fn.Option[int64] {

	if linkKindCode != oorPackageLinkKindCreatedOutputCode {
		return fn.None[int64]()
	}

	return fn.Some(valueSat)
}

func unixTimeUTC(unixSeconds int64) time.Time {
	return time.Unix(unixSeconds, 0).UTC()
}

func nullUnixFromOptionTime(t fn.Option[time.Time]) sql.NullInt64 {
	var out sql.NullInt64
	t.WhenSome(func(v time.Time) {
		out = sql.NullInt64{
			Int64: v.Unix(),
			Valid: true,
		}
	})

	return out
}

func optionTimeFromNullUnix(v sql.NullInt64) fn.Option[time.Time] {
	if !v.Valid {
		return fn.None[time.Time]()
	}

	return fn.Some(unixTimeUTC(v.Int64))
}

// unmarshalClientKey hydrates the owned receive-script client key descriptor
// from the internal_keys registry via the row's client_key_id FK.
func unmarshalClientKey(ctx context.Context, q OORArtifactStore,
	row sqlc.OwnedReceiveScript) (keychain.KeyDescriptor, error) {

	if !row.ClientKeyID.Valid {
		return keychain.KeyDescriptor{}, fmt.Errorf("owned receive "+
			"script %x missing client key id", row.PkScript)
	}

	return InternalKeyDescByIDTx(ctx, q, row.ClientKeyID.Int64)
}

// parseHash32 converts a 32-byte hash payload to chainhash.Hash.
func parseHash32(raw []byte) (chainhash.Hash, error) {
	h, err := chainhash.NewHash(raw)
	if err != nil {
		return chainhash.Hash{}, err
	}

	return *h, nil
}

// validatePackageDirection validates accepted package direction values.
func validatePackageDirection(direction OORPackageDirection) error {
	switch direction {
	case OORPackageDirectionIncoming, OORPackageDirectionOutgoing:
		return nil

	default:
		return fmt.Errorf("unsupported package direction: %d",
			direction)
	}
}

// sameOORPackagePayload reports whether an existing package row already holds
// the exact serialized payload being upserted.
func sameOORPackagePayload(ctx context.Context, q OORArtifactStore,
	existing sqlc.OorPackage, arkRaw []byte,
	rawCheckpoints [][]byte) (bool, error) {

	if !bytes.Equal(existing.ArkPsbt, arkRaw) {
		return false, nil
	}

	existingCheckpoints, err := q.ListOORPackageCheckpoints(
		ctx, existing.SessionID,
	)
	if err != nil {
		return false, err
	}

	if len(existingCheckpoints) != len(rawCheckpoints) {
		return false, nil
	}

	for i := range rawCheckpoints {
		if !bytes.Equal(
			existingCheckpoints[i].CheckpointPsbt,
			rawCheckpoints[i],
		) {
			return false, nil
		}
	}

	return true, nil
}

func packageDirectionCode(direction OORPackageDirection) (int32, error) {
	if err := validatePackageDirection(direction); err != nil {
		return 0, err
	}

	return int32(direction), nil
}

func packageDirectionFromCode(directionCode int32) (OORPackageDirection,
	error) {

	switch directionCode {
	case oorPackageDirectionIncomingCode:
		return OORPackageDirectionIncoming, nil

	case oorPackageDirectionOutgoingCode:
		return OORPackageDirectionOutgoing, nil

	default:
		return 0, fmt.Errorf("unsupported package direction code: %d",
			directionCode)
	}
}

// validateBindingKind validates accepted outpoint binding relation kinds.
func validateBindingKind(kind OORPackageLinkKind) error {
	switch kind {
	case OORPackageLinkKindCreatedOutput, OORPackageLinkKindConsumedInput:
		return nil

	default:
		return fmt.Errorf("unsupported binding kind: %d", kind)
	}
}

func bindingKindCode(kind OORPackageLinkKind) (int32, error) {
	if err := validateBindingKind(kind); err != nil {
		return 0, err
	}

	return int32(kind), nil
}

func bindingKindFromCode(kindCode int32) (OORPackageLinkKind, error) {
	switch kindCode {
	case oorPackageLinkKindCreatedOutputCode:
		return OORPackageLinkKindCreatedOutput, nil

	case oorPackageLinkKindConsumedInputCode:
		return OORPackageLinkKindConsumedInput, nil

	default:
		return 0, fmt.Errorf("unsupported binding kind code: %d",
			kindCode)
	}
}

func validateOwnedReceiveScriptSource(source OwnedReceiveScriptSource) error {
	switch source {
	case OwnedReceiveScriptSourceWallet,
		OwnedReceiveScriptSourceRPC,
		OwnedReceiveScriptSourceSync:
		return nil

	default:
		return fmt.Errorf("unsupported owned receive script source: %d",
			source)
	}
}

func ownedReceiveScriptSourceCode(source OwnedReceiveScriptSource) (int32,
	error) {

	if err := validateOwnedReceiveScriptSource(source); err != nil {
		return 0, err
	}

	return int32(source), nil
}

func ownedReceiveScriptSourceFromCode(sourceCode int32) (
	OwnedReceiveScriptSource, error) {

	switch sourceCode {
	case int32(OwnedReceiveScriptSourceWallet):
		return OwnedReceiveScriptSourceWallet, nil

	case int32(OwnedReceiveScriptSourceRPC):
		return OwnedReceiveScriptSourceRPC, nil

	case int32(OwnedReceiveScriptSourceSync):
		return OwnedReceiveScriptSourceSync, nil

	default:
		return 0, fmt.Errorf("unsupported owned receive script source "+
			"code: %d", sourceCode)
	}
}

func packageDirectionConflictErr(
	sessionID []byte,
	existing OORPackageDirection,
	requested OORPackageDirection,
) error {

	return fmt.Errorf("%w: %x existing=%s requested=%s",
		types.ErrOORPackageDirectionConflict, sessionID, existing,
		requested)
}
