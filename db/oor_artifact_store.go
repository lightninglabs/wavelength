package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/clock"
)

const (
	// OORPackageDirectionIncoming marks packages received by this client.
	OORPackageDirectionIncoming = "incoming"

	// OORPackageDirectionOutgoing marks packages sent by this client.
	OORPackageDirectionOutgoing = "outgoing"

	// OORPackageLinkKindCreatedOutput identifies bindings where the local
	// outpoint was created by the Ark transaction.
	OORPackageLinkKindCreatedOutput = "created_output"

	// OORPackageLinkKindConsumedInput identifies bindings where the local
	// outpoint was consumed as an OOR input.
	OORPackageLinkKindConsumedInput = "consumed_input"
)

// OORArtifactStore groups SQL methods needed by OOR artifact persistence.
//
//nolint:interfacebloat
type OORArtifactStore interface {
	UpsertOORPackage(ctx context.Context,
		arg sqlc.UpsertOORPackageParams) error

	DeleteOORPackageCheckpoints(ctx context.Context, sessionID []byte) error

	InsertOORPackageCheckpoint(ctx context.Context,
		arg sqlc.InsertOORPackageCheckpointParams) error

	GetOORPackage(ctx context.Context, sessionID []byte) (sqlc.OorPackage,
		error)

	ListOORPackageCheckpoints(ctx context.Context,
		sessionID []byte) ([]sqlc.OorPackageCheckpoint, error)

	ListOORPackages(ctx context.Context) ([]sqlc.OorPackage, error)

	ListOORPackagesByDirection(ctx context.Context,
		direction string) ([]sqlc.OorPackage, error)

	UpsertOORVTXOBinding(ctx context.Context,
		arg sqlc.UpsertOORVTXOBindingParams) error

	GetOORVTXOBindingByOutpoint(ctx context.Context,
		arg sqlc.GetOORVTXOBindingByOutpointParams) (
		sqlc.OorVtxoBinding, error,
	)

	ListOORVTXOBindingsBySession(ctx context.Context,
		sessionID []byte) ([]sqlc.OorVtxoBinding, error)

	GetOORPackageByOutpoint(ctx context.Context,
		arg sqlc.GetOORPackageByOutpointParams) (
		sqlc.GetOORPackageByOutpointRow, error)

	UpsertOORRecipientCursor(ctx context.Context,
		arg sqlc.UpsertOORRecipientCursorParams) error

	GetOORRecipientCursor(ctx context.Context,
		recipientPkScript []byte) (sqlc.OorRecipientCursor, error)

	ListOORRecipientCursors(ctx context.Context) (
		[]sqlc.OorRecipientCursor, error)

	UpsertOwnedReceiveScript(ctx context.Context,
		arg sqlc.UpsertOwnedReceiveScriptParams) error

	GetOwnedReceiveScript(ctx context.Context,
		pkScript []byte) (sqlc.OwnedReceiveScript, error)

	ListOwnedReceiveScripts(ctx context.Context) (
		[]sqlc.OwnedReceiveScript, error)
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
	LinkKind string

	// RecipientPkScript is populated for created-output bindings.
	RecipientPkScript []byte

	// ValueSat is populated for created-output bindings when known.
	ValueSat *int64

	// CreatedAt is the binding creation timestamp.
	CreatedAt int64

	// UpdatedAt is the last binding update timestamp.
	UpdatedAt int64
}

// OORPackageBundle is a fully materialized OOR package view.
type OORPackageBundle struct {
	// SessionID is the stable session identifier.
	SessionID chainhash.Hash

	// Direction is incoming or outgoing from the local client perspective.
	Direction string

	// ArkPSBT is the persisted Ark package.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs is the persisted finalized checkpoint package.
	FinalCheckpointPSBTs []*psbt.Packet

	// Bindings are all known local outpoint links for this session.
	Bindings []OORPackageBinding

	// CreatedAt is the package creation timestamp.
	CreatedAt int64

	// UpdatedAt is the package update timestamp.
	UpdatedAt int64

	// MatchedOutpointBinding is set on GetPackageForOutpoint responses.
	MatchedOutpointBinding *OORPackageBinding
}

// OwnedReceiveScriptRecord is a local script ownership registration row.
type OwnedReceiveScriptRecord struct {
	// PkScript is the tracked receive script.
	PkScript []byte

	// ClientKeyFamily is the local key family.
	ClientKeyFamily int64

	// ClientKeyIndex is the local key index.
	ClientKeyIndex int64

	// ClientPubKey is the client pubkey for this script.
	ClientPubKey []byte

	// OperatorPubKey is the operator pubkey for this script.
	OperatorPubKey []byte

	// ExitDelay is the relative CSV delay.
	ExitDelay int64

	// Source labels where this script registration came from.
	Source string

	// CreatedAt is the creation timestamp.
	CreatedAt int64

	// LastUsedAt tracks the last usage timestamp when available.
	LastUsedAt sql.NullInt64
}

// OORArtifactPersistenceStore persists OOR artifacts and query surfaces needed
// for unroll package retrieval.
type OORArtifactPersistenceStore struct {
	db    BatchedOORArtifactStore
	clock clock.Clock
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
		db:    db,
		clock: c,
	}
}

// UpsertPackage writes or replaces one finalized OOR package identified by
// session ID.
//
// The method persists the Ark PSBT and then rewrites the checkpoint set in
// contiguous index order. Existing checkpoint rows for the session are removed
// first so retries always converge to the latest canonical package.
func (s *OORArtifactPersistenceStore) UpsertPackage(ctx context.Context,
	direction string, sessionID chainhash.Hash, ark *psbt.Packet,
	checkpoints []*psbt.Packet) error {

	if s == nil || s.db == nil {
		return fmt.Errorf("store must be provided")
	}

	if err := validatePackageDirection(direction); err != nil {
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
		err := q.UpsertOORPackage(ctx, sqlc.UpsertOORPackageParams{
			SessionID: id,
			Direction: direction,
			ArkPsbt:   arkRaw,
			CreatedAt: now,
			UpdatedAt: now,
		})
		if err != nil {
			return err
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
	linkKind string, recipientPkScript []byte, valueSat *int64) error {

	if s == nil || s.db == nil {
		return fmt.Errorf("store must be provided")
	}

	if err := validateBindingKind(linkKind); err != nil {
		return err
	}

	now := s.clock.Now().Unix()

	writeTx := WriteTxOption()

	return s.db.ExecTx(ctx, writeTx, func(q OORArtifactStore) error {
		var value sql.NullInt64
		if valueSat != nil {
			value = sql.NullInt64{
				Int64: *valueSat,
				Valid: true,
			}
		}

		params := sqlc.UpsertOORVTXOBindingParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
			SessionID:     sessionID[:],
			OutputIndex:   int32(outputIndex),
			LinkKind:      linkKind,
			ValueSat:      value,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		params.RecipientPkScript = recipientPkScript

		return q.UpsertOORVTXOBinding(ctx, params)
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

		pkg, err := loadPackageBundleBySession(ctx, q, row.SessionID)
		if err != nil {
			return err
		}

		matched, err := bindingFromOutpointJoinRow(row)
		if err != nil {
			return err
		}

		pkg.MatchedOutpointBinding = matched
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
	direction *string) ([]*OORPackageBundle, error) {

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

			rows, err = q.ListOORPackagesByDirection(
				ctx, *direction,
			)
		}
		if err != nil {
			return err
		}

		out := make([]*OORPackageBundle, 0, len(rows))
		for i := range rows {
			pkg, err := loadPackageBundleBySession(
				ctx, q, rows[i].SessionID,
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
func (s *OORArtifactPersistenceStore) ListSentPackages(
	ctx context.Context) ([]*OORPackageBundle, error) {

	direction := OORPackageDirectionOutgoing
	return s.ListPackages(ctx, &direction)
}

// UpsertRecipientCursor writes recipient polling progress for one script.
//
// The cursor enables at-least-once recipient event ingestion. Reprocessing the
// same or older events is safe because artifact writes are idempotent.
func (s *OORArtifactPersistenceStore) UpsertRecipientCursor(
	ctx context.Context, recipientPkScript []byte, lastEventID int64,
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
func (s *OORArtifactPersistenceStore) UpsertOwnedReceiveScript(
	ctx context.Context, rec OwnedReceiveScriptRecord) error {

	if s == nil || s.db == nil {
		return fmt.Errorf("store must be provided")
	}

	if len(rec.PkScript) == 0 {
		return fmt.Errorf("pk script must be provided")
	}

	if len(rec.ClientPubKey) == 0 {
		return fmt.Errorf("client pubkey must be provided")
	}

	if len(rec.OperatorPubKey) == 0 {
		return fmt.Errorf("operator pubkey must be provided")
	}

	if rec.Source == "" {
		return fmt.Errorf("source must be provided")
	}

	writeTx := WriteTxOption()
	now := s.clock.Now().Unix()

	return s.db.ExecTx(ctx, writeTx, func(q OORArtifactStore) error {
		createdAt := rec.CreatedAt
		if createdAt == 0 {
			createdAt = now
		}

		return q.UpsertOwnedReceiveScript(ctx,
			sqlc.UpsertOwnedReceiveScriptParams{
				PkScript:        rec.PkScript,
				ClientKeyFamily: rec.ClientKeyFamily,
				ClientKeyIndex:  rec.ClientKeyIndex,
				ClientPubkey:    rec.ClientPubKey,
				OperatorPubkey:  rec.OperatorPubKey,
				ExitDelay:       rec.ExitDelay,
				Source:          rec.Source,
				CreatedAt:       createdAt,
				LastUsedAt:      rec.LastUsedAt,
			},
		)
	})
}

// LookupOwnedReceiveScript loads one owned receive script metadata row by
// pkScript.
//
// This is a direct lookup path used by receive-side attribution logic.
func (s *OORArtifactPersistenceStore) LookupOwnedReceiveScript(
	ctx context.Context,
	pkScript []byte) (*OwnedReceiveScriptRecord, error) {

	if s == nil || s.db == nil {
		return nil, fmt.Errorf("store must be provided")
	}

	readTx := ReadTxOption()

	var row sqlc.OwnedReceiveScript

	err := s.db.ExecTx(ctx, readTx, func(q OORArtifactStore) error {
		var err error
		row, err = q.GetOwnedReceiveScript(ctx, pkScript)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &OwnedReceiveScriptRecord{
		PkScript:        row.PkScript,
		ClientKeyFamily: row.ClientKeyFamily,
		ClientKeyIndex:  row.ClientKeyIndex,
		ClientPubKey:    row.ClientPubkey,
		OperatorPubKey:  row.OperatorPubkey,
		ExitDelay:       row.ExitDelay,
		Source:          row.Source,
		CreatedAt:       row.CreatedAt,
		LastUsedAt:      row.LastUsedAt,
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

	var rows []sqlc.OwnedReceiveScript

	err := s.db.ExecTx(ctx, readTx, func(q OORArtifactStore) error {
		var err error
		rows, err = q.ListOwnedReceiveScripts(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}

	records := make([]OwnedReceiveScriptRecord, 0, len(rows))
	for i := range rows {
		records = append(records, OwnedReceiveScriptRecord{
			PkScript:        rows[i].PkScript,
			ClientKeyFamily: rows[i].ClientKeyFamily,
			ClientKeyIndex:  rows[i].ClientKeyIndex,
			ClientPubKey:    rows[i].ClientPubkey,
			OperatorPubKey:  rows[i].OperatorPubkey,
			ExitDelay:       rows[i].ExitDelay,
			Source:          rows[i].Source,
			CreatedAt:       rows[i].CreatedAt,
			LastUsedAt:      rows[i].LastUsedAt,
		})
	}

	return records, nil
}

// loadPackageBundleBySession materializes a full package bundle by session ID.
func loadPackageBundleBySession(ctx context.Context, q OORArtifactStore,
	sessionID []byte) (*OORPackageBundle, error) {

	pkgRow, err := q.GetOORPackage(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	sessionHash, err := parseSessionID(sessionID)
	if err != nil {
		return nil, err
	}

	ark, err := psbtutil.Parse(pkgRow.ArkPsbt)
	if err != nil {
		return nil, err
	}

	checkpointRows, err := q.ListOORPackageCheckpoints(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	checkpoints := make([]*psbt.Packet, 0, len(checkpointRows))
	for i := range checkpointRows {
		pkt, err := psbtutil.Parse(checkpointRows[i].CheckpointPsbt)
		if err != nil {
			return nil, fmt.Errorf(
				"parse checkpoint %d: %w", i, err,
			)
		}

		checkpoints = append(checkpoints, pkt)
	}

	bindingRows, err := q.ListOORVTXOBindingsBySession(ctx, sessionID)
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

	return &OORPackageBundle{
		SessionID:            sessionHash,
		Direction:            pkgRow.Direction,
		ArkPSBT:              ark,
		FinalCheckpointPSBTs: checkpoints,
		Bindings:             bindings,
		CreatedAt:            pkgRow.CreatedAt,
		UpdatedAt:            pkgRow.UpdatedAt,
	}, nil
}

// bindingFromRow converts a raw binding row into the API binding shape.
func bindingFromRow(row sqlc.OorVtxoBinding) (*OORPackageBinding, error) {
	sessionID, err := parseSessionID(row.SessionID)
	if err != nil {
		return nil, err
	}

	outpointHash, err := parseSessionID(row.OutpointHash)
	if err != nil {
		return nil, err
	}

	var value *int64
	if row.ValueSat.Valid {
		v := row.ValueSat.Int64
		value = &v
	}

	return &OORPackageBinding{
		Outpoint: wire.OutPoint{
			Hash:  outpointHash,
			Index: uint32(row.OutpointIndex),
		},
		SessionID:         sessionID,
		OutputIndex:       uint32(row.OutputIndex),
		LinkKind:          row.LinkKind,
		RecipientPkScript: row.RecipientPkScript,
		ValueSat:          value,
		CreatedAt:         row.CreatedAt,
		UpdatedAt:         row.UpdatedAt,
	}, nil
}

// bindingFromOutpointJoinRow converts an outpoint join row into a binding.
func bindingFromOutpointJoinRow(
	row sqlc.GetOORPackageByOutpointRow) (*OORPackageBinding, error) {

	sessionID, err := parseSessionID(row.SessionID)
	if err != nil {
		return nil, err
	}

	outpointHash, err := parseSessionID(row.OutpointHash)
	if err != nil {
		return nil, err
	}

	var value *int64
	if row.ValueSat.Valid {
		v := row.ValueSat.Int64
		value = &v
	}

	return &OORPackageBinding{
		Outpoint: wire.OutPoint{
			Hash:  outpointHash,
			Index: uint32(row.OutpointIndex),
		},
		SessionID:         sessionID,
		OutputIndex:       uint32(row.OutputIndex),
		LinkKind:          row.LinkKind,
		RecipientPkScript: row.RecipientPkScript,
		ValueSat:          value,
		CreatedAt:         row.BindingCreatedAt,
		UpdatedAt:         row.BindingUpdatedAt,
	}, nil
}

// parseSessionID converts a 32-byte hash payload to chainhash.Hash.
func parseSessionID(raw []byte) (chainhash.Hash, error) {
	h, err := chainhash.NewHash(raw)
	if err != nil {
		return chainhash.Hash{}, err
	}

	return *h, nil
}

// validatePackageDirection validates accepted package direction values.
func validatePackageDirection(direction string) error {
	switch direction {
	case OORPackageDirectionIncoming, OORPackageDirectionOutgoing:
		return nil

	default:
		return fmt.Errorf(
			"unsupported package direction: %s", direction,
		)
	}
}

// validateBindingKind validates accepted outpoint binding relation kinds.
func validateBindingKind(kind string) error {
	switch kind {
	case OORPackageLinkKindCreatedOutput, OORPackageLinkKindConsumedInput:
		return nil

	default:
		return fmt.Errorf("unsupported binding kind: %s", kind)
	}
}
