package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

// VTXOPersistenceStore implements the vtxo.VTXOStore interface using the
// BatchedTx pattern for transaction-safe VTXO lifecycle operations.
type VTXOPersistenceStore struct {
	// db provides the underlying batched transaction executor.
	db BatchedRoundStore

	// clock provides time for timestamps.
	clock clock.Clock

	// ancestryCache aliases immutable decoded ancestry tree fragments
	// across repeated list and selection queries.
	ancestryCache *ancestryTreeCache

	// descriptorCache memoizes the immutable derived parts of VTXO rows
	// (parsed keys, reconstructed taproot script, policy-resolved expiry)
	// by outpoint, so repeated listings skip the per-row secp256k1 and
	// policy-decode work.
	descriptorCache *vtxoDescriptorCache

	// Log is an optional logger for this persistence store. If None,
	// the store falls back to extracting a logger from context via
	// build.LoggerFromContext, or uses btclog.Disabled if no logger
	// is found. Matches the fn.Option[btclog.Logger] pattern used by
	// other subsystems (indexer.SyncClient, oor.Actor, etc.).
	Log fn.Option[btclog.Logger]
}

// NewVTXOPersistenceStore creates a new VTXO persistence store using the
// transaction executor pattern. The logger is unset by default; to route
// rehydrate-path diagnostics (e.g. expiry drift warnings) through the
// daemon's subsystem logger, set Log to fn.Some(logger) after construction
// or use NewVTXOPersistenceStoreWithLogger.
func NewVTXOPersistenceStore(
	db BatchedRoundStore, c clock.Clock,
) *VTXOPersistenceStore {

	return &VTXOPersistenceStore{
		db:              db,
		clock:           c,
		ancestryCache:   newAncestryTreeCache(),
		descriptorCache: newVTXODescriptorCache(),
	}
}

// NewVTXOPersistenceStoreWithLogger constructs a VTXO persistence store
// with an explicit logger attached.
func NewVTXOPersistenceStoreWithLogger(db BatchedRoundStore, c clock.Clock,
	log fn.Option[btclog.Logger]) *VTXOPersistenceStore {

	return &VTXOPersistenceStore{
		db:              db,
		clock:           c,
		ancestryCache:   newAncestryTreeCache(),
		descriptorCache: newVTXODescriptorCache(),
		Log:             log,
	}
}

// logger returns the configured logger, falling back to extracting a
// logger from context. If neither is available, returns btclog.Disabled.
func (s *VTXOPersistenceStore) logger(ctx context.Context) btclog.Logger {
	return s.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// SaveVTXO persists a new VTXO to storage. Called when a VTXO actor is created.
// Returns error if a VTXO with the same outpoint already exists.
func (s *VTXOPersistenceStore) SaveVTXO(
	ctx context.Context, desc *vtxo.Descriptor,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		err := s.ensureRoundExists(ctx, q, desc)
		if err != nil {
			return fmt.Errorf("ensure round: %w", err)
		}

		params, err := s.descriptorToInsertParams(ctx, q, desc)
		if err != nil {
			return fmt.Errorf("convert descriptor: %w", err)
		}

		if err := q.InsertVTXO(ctx, params); err != nil {
			return fmt.Errorf("insert VTXO: %w", err)
		}

		return upsertAncestryPaths(
			ctx, q, desc.Outpoint.Hash[:],
			int32(desc.Outpoint.Index), desc.Ancestry,
		)
	})
}

// ensureRoundExists inserts a minimal confirmed round row when a VTXO refers
// to a remote round that this client has not otherwise persisted locally.
// If the round already exists (e.g. from the normal round flow), the existing
// row is left untouched to avoid overwriting richer state.
func (s *VTXOPersistenceStore) ensureRoundExists(ctx context.Context,
	q RoundStore, desc *vtxo.Descriptor) error {

	if desc == nil {
		return fmt.Errorf("descriptor must be provided")
	}

	if desc.RoundID == "" {
		return fmt.Errorf("round id must be provided")
	}

	// Check whether the round already exists before inserting, because
	// InsertRound uses ON CONFLICT DO UPDATE which would overwrite
	// fields like status on an existing row.
	_, err := q.GetRound(ctx, desc.RoundID)
	if err == nil {
		return nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	var confirmationHeight sql.NullInt32
	if desc.CreatedHeight > 0 {
		confirmationHeight = sql.NullInt32{
			Int32: desc.CreatedHeight,
			Valid: true,
		}
	}

	nowUnix := s.clock.Now().Unix()
	params := InsertRoundParams{
		RoundID:            desc.RoundID,
		StartHeight:        0,
		ConfirmationHeight: confirmationHeight,
		CommitmentTxid:     desc.CommitmentTxID[:],
		Status:             "confirmed",
		CreationTime:       nowUnix,
		LastUpdateTime:     nowUnix,

		// Stamp the flow version explicitly at creation rather than
		// relying on the column DEFAULT. Versions are zero-indexed, so
		// V1 is the zero value; stamping it explicitly keeps "stamped
		// at creation" true on this synthetic path.
		FlowVersion: int32(roundpb.FlowVersionV1),
	}

	return q.InsertRound(ctx, params)
}

// GetVTXO retrieves a VTXO by its outpoint. Used for actor recovery on startup.
// Returns vtxo.ErrVTXONotFound if the outpoint is not stored.
func (s *VTXOPersistenceStore) GetVTXO(ctx context.Context,
	outpoint wire.OutPoint) (*vtxo.Descriptor, error) {

	readTxOpts := ReadTxOption()

	var result *vtxo.Descriptor

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		params := sqlc.GetVTXOParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
		}

		row, err := q.GetVTXO(ctx, params)
		if err != nil {
			// Translate the persistence-layer miss into the domain
			// sentinel so callers match vtxo.ErrVTXONotFound rather
			// than sql.ErrNoRows. We keep sql.ErrNoRows in the
			// chain so existing call sites that still test for it
			// keep working while they migrate.
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("get VTXO: %w: %w",
					vtxo.ErrVTXONotFound, err)
			}

			return fmt.Errorf("get VTXO: %w", err)
		}

		desc, err := s.rowToDescriptor(ctx, q, row, nil)
		if err != nil {
			return fmt.Errorf("convert VTXO: %w", err)
		}

		result = desc

		return nil
	})

	return result, err
}

// ListLiveVTXOs returns all VTXOs not in a terminal state. Used during startup
// to recover active VTXO actors after restart. Issues exactly two queries —
// the parent VTXO list and a batched ancestry-paths fetch — so descriptor
// rehydration runs in O(2) round-trips rather than O(N).
func (s *VTXOPersistenceStore) ListLiveVTXOs(ctx context.Context) (
	[]*vtxo.Descriptor, error) {

	readTxOpts := ReadTxOption()

	var result []*vtxo.Descriptor

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		rows, err := q.ListLiveVTXOs(ctx)
		if err != nil {
			return fmt.Errorf("list live VTXOs: %w", err)
		}

		ancestryRows, err := q.ListLiveVTXOAncestryPaths(ctx)
		if err != nil {
			return fmt.Errorf("list live ancestry paths: %w", err)
		}

		ancestryByOutpoint, err := groupAncestryRowsWithCache(
			ancestryRows, s.ancestryCache,
		)
		if err != nil {
			return fmt.Errorf("group ancestry rows: %w", err)
		}

		descs := make([]*vtxo.Descriptor, 0, len(rows))
		for _, row := range rows {
			desc, err := s.rowToDescriptor(
				ctx, q, row, ancestryByOutpoint,
			)
			if err != nil {
				return fmt.Errorf("convert VTXO: %w", err)
			}

			descs = append(descs, desc)
		}

		result = descs

		return nil
	})

	return result, err
}

// ListVTXOsByStatus returns all VTXOs matching the given status. This
// enables the ListVTXOs RPC to query terminal states (spent, forfeited)
// directly from the database instead of filtering in memory. Like
// ListLiveVTXOs, the ancestry side table is loaded via a single batched
// query rather than per-row.
func (s *VTXOPersistenceStore) ListVTXOsByStatus(ctx context.Context,
	status vtxo.VTXOStatus) ([]*vtxo.Descriptor, error) {

	readTxOpts := ReadTxOption()

	var result []*vtxo.Descriptor

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		rows, err := q.ListVTXOsByStatus(ctx, int32(status))
		if err != nil {
			return fmt.Errorf("list VTXOs by status: %w", err)
		}

		ancestryRows, err := q.ListVTXOAncestryPathsByStatus(
			ctx, int32(status),
		)
		if err != nil {
			return fmt.Errorf("list ancestry paths by status: %w",
				err)
		}

		ancestryByOutpoint, err := groupAncestryRowsWithCache(
			ancestryRows, s.ancestryCache,
		)
		if err != nil {
			return fmt.Errorf("group ancestry rows: %w", err)
		}

		descs, err := s.byStatusRowsToDescriptors(
			ctx, q, rows, ancestryByOutpoint,
		)
		if err != nil {
			return err
		}

		result = descs

		return nil
	})

	return result, err
}

// ListLiveVTXOsLight returns the same descriptors as ListLiveVTXOs with a
// nil Ancestry on every entry. The ancestry side table's TLV tree fragments
// grow with OOR chain depth, and the batched join sorts those blobs through
// SQLite's external sorter on every call, so consumers that never walk the
// lineage (the ListVTXOs RPC response carries no ancestry) skip the side
// table entirely.
func (s *VTXOPersistenceStore) ListLiveVTXOsLight(ctx context.Context) (
	[]*vtxo.Descriptor, error) {

	readTxOpts := ReadTxOption()

	var result []*vtxo.Descriptor

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		rows, err := q.ListLiveVTXOs(ctx)
		if err != nil {
			return fmt.Errorf("list live VTXOs: %w", err)
		}

		descs, err := s.rowsToDescriptorsNoAncestry(ctx, q, rows)
		if err != nil {
			return err
		}

		result = descs

		return nil
	})

	return result, err
}

// ListVTXOsByStatusLight returns the same descriptors as ListVTXOsByStatus
// with a nil Ancestry on every entry. See ListLiveVTXOsLight for why
// listing-only consumers skip the ancestry side table.
func (s *VTXOPersistenceStore) ListVTXOsByStatusLight(ctx context.Context,
	status vtxo.VTXOStatus) ([]*vtxo.Descriptor, error) {

	readTxOpts := ReadTxOption()

	var result []*vtxo.Descriptor

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		rows, err := q.ListVTXOsByStatus(ctx, int32(status))
		if err != nil {
			return fmt.Errorf("list VTXOs by status: %w", err)
		}

		// A non-nil empty index keeps rowToDescriptor on the preloaded
		// (zero ancestry) path, matching rowsToDescriptorsNoAncestry.
		noAncestry := map[wire.OutPoint][]vtxo.Ancestry{}
		descs, err := s.byStatusRowsToDescriptors(
			ctx, q, rows, noAncestry,
		)
		if err != nil {
			return err
		}

		result = descs

		return nil
	})

	return result, err
}

// byStatusRowsToDescriptors converts the joined by-status rows to descriptors
// using the supplied (possibly empty) preloaded ancestry index, then stamps
// the settlement txid/height carried by the LEFT JOIN onto rounds. The join
// columns are NULL for every VTXO whose forfeit round is unknown, so a VTXO
// that is not a confirmed forfeit keeps the zero settlement values and the RPC
// surface degrades to today's behavior.
func (s *VTXOPersistenceStore) byStatusRowsToDescriptors(ctx context.Context,
	q RoundStore, rows []sqlc.ListVTXOsByStatusRow,
	preloaded map[wire.OutPoint][]vtxo.Ancestry) ([]*vtxo.Descriptor,
	error) {

	descs := make([]*vtxo.Descriptor, 0, len(rows))
	for _, row := range rows {
		desc, err := s.rowToDescriptor(ctx, q, row.Vtxo, preloaded)
		if err != nil {
			return nil, fmt.Errorf("convert VTXO: %w", err)
		}

		// settlement_txid is a 32-byte BLOB when the forfeit round row
		// exists; treat any other length (NULL join, short/legacy) as
		// unset so the descriptor's Settlement stays None. The txid,
		// height, and round-level operator fee are attached together,
		// as they all describe the same forfeit round.
		if len(row.SettlementTxid) == chainhash.HashSize {
			var settle vtxo.Settlement
			copy(settle.TxID[:], row.SettlementTxid)
			if row.SettlementHeight.Valid {
				settle.Height = row.SettlementHeight.Int32
			}
			settle.FeeSat = row.SettlementFeeSat
			desc.Settlement = fn.Some(settle)
		}

		descs = append(descs, desc)
	}

	return descs, nil
}

// rowsToDescriptorsNoAncestry converts VTXO rows to descriptors without
// touching the ancestry side table. The non-nil empty index keeps
// rowToDescriptor on the preloaded path (zero ancestry) instead of falling
// back to the per-row singleton ancestry query.
func (s *VTXOPersistenceStore) rowsToDescriptorsNoAncestry(ctx context.Context,
	q RoundStore, rows []VTXORow) ([]*vtxo.Descriptor, error) {

	noAncestry := map[wire.OutPoint][]vtxo.Ancestry{}

	descs := make([]*vtxo.Descriptor, 0, len(rows))
	for _, row := range rows {
		desc, err := s.rowToDescriptor(ctx, q, row, noAncestry)
		if err != nil {
			return nil, fmt.Errorf("convert VTXO: %w", err)
		}

		descs = append(descs, desc)
	}

	return descs, nil
}

// ListSelectionCandidatesByStatus returns the lightweight projection coin
// selection runs on: outpoint, amount, and pkScript per VTXO in the given
// status. Selection happens on every payment and needs only these fields, so
// this path skips the full descriptor decode (pubkey parsing, taproot script
// reconstruction, policy template decode) and the batched ancestry query
// entirely.
func (s *VTXOPersistenceStore) ListSelectionCandidatesByStatus(
	ctx context.Context, status vtxo.VTXOStatus) ([]vtxo.SelectedVTXO,
	error) {

	readTxOpts := ReadTxOption()

	var result []vtxo.SelectedVTXO

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		rows, err := q.ListVTXOSelectionCandidatesByStatus(
			ctx, int32(status),
		)
		if err != nil {
			return fmt.Errorf("list selection candidates: %w", err)
		}

		candidates := make([]vtxo.SelectedVTXO, 0, len(rows))
		for _, row := range rows {
			var outpointHash chainhash.Hash
			copy(outpointHash[:], row.OutpointHash)

			candidates = append(candidates, vtxo.SelectedVTXO{
				Outpoint: wire.OutPoint{
					Hash:  outpointHash,
					Index: uint32(row.OutpointIndex),
				},
				Amount:   btcutil.Amount(row.Amount),
				PkScript: row.PkScript,
			})
		}

		result = candidates

		return nil
	})

	return result, err
}

// UpdateVTXOStatus atomically updates a VTXO's status. This is the primary
// method for state transitions that don't require additional data.
func (s *VTXOPersistenceStore) UpdateVTXOStatus(
	ctx context.Context, outpoint wire.OutPoint, status vtxo.VTXOStatus,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		params := sqlc.UpdateVTXOStatusParams{
			OutpointHash:   outpoint.Hash[:],
			OutpointIndex:  int32(outpoint.Index),
			Status:         int32(status),
			LastUpdateTime: s.clock.Now().Unix(),
		}

		return q.UpdateVTXOStatus(ctx, params)
	})
}

// UpdateVTXOStatusReleasingReservation updates a VTXO's status and deletes its
// spending-reservation row in the same transaction. It is used for transitions
// that move a VTXO out of SpendingState so the durable index never retains a
// stale row that could mask a future orphan on the same outpoint. Deleting a
// non-existent row is a no-op, so this is safe on outpoints that were never
// reserved.
func (s *VTXOPersistenceStore) UpdateVTXOStatusReleasingReservation(
	ctx context.Context, outpoint wire.OutPoint, status vtxo.VTXOStatus,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		statusParams := sqlc.UpdateVTXOStatusParams{
			OutpointHash:   outpoint.Hash[:],
			OutpointIndex:  int32(outpoint.Index),
			Status:         int32(status),
			LastUpdateTime: s.clock.Now().Unix(),
		}
		if err := q.UpdateVTXOStatus(ctx, statusParams); err != nil {
			return err
		}

		return q.DeleteSpendingReservation(
			ctx, sqlc.DeleteSpendingReservationParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
			},
		)
	})
}

// MarkForfeiting transitions a VTXO to forfeiting state and persists the signed
// forfeit transaction for crash recovery. Called when entering the forfeit flow
// before the new round's commitment confirms.
func (s *VTXOPersistenceStore) MarkForfeiting(
	ctx context.Context, outpoint wire.OutPoint, roundID string,
	forfeitTx *wire.MsgTx,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		// Serialize the forfeit transaction.
		var forfeitTxBytes []byte
		if forfeitTx != nil {
			var buf bytes.Buffer
			if err := forfeitTx.Serialize(&buf); err != nil {
				return fmt.Errorf("serialize forfeit tx: %w",
					err)
			}

			forfeitTxBytes = buf.Bytes()
		}

		params := sqlc.MarkVTXOForfeitingParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
			ForfeitRoundID: sql.NullString{
				String: roundID,
				Valid:  roundID != "",
			},
			ForfeitTx:      forfeitTxBytes,
			LastUpdateTime: s.clock.Now().Unix(),
		}

		return q.MarkVTXOForfeiting(ctx, params)
	})
}

// GetForfeitTx retrieves the persisted forfeit transaction for a VTXO. Used
// during recovery to restore the ForfeitingState with its tx. Returns nil if
// no forfeit tx is stored for this outpoint.
func (s *VTXOPersistenceStore) GetForfeitTx(ctx context.Context,
	outpoint wire.OutPoint) (*wire.MsgTx, error) {

	readTxOpts := ReadTxOption()

	var result *wire.MsgTx

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		params := sqlc.GetVTXOForfeitTxParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
		}

		row, err := q.GetVTXOForfeitTx(ctx, params)
		if err != nil {
			return fmt.Errorf("get forfeit tx: %w", err)
		}

		if len(row.ForfeitTx) == 0 {

			// No forfeit tx stored.
			return nil
		}

		// Deserialize the forfeit transaction.
		tx := &wire.MsgTx{}
		reader := bytes.NewReader(row.ForfeitTx)
		if err := tx.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize forfeit tx: %w", err)
		}

		result = tx

		return nil
	})

	return result, err
}

// MarkForfeited marks a VTXO as forfeited and records the forfeit transaction
// ID. This is called when the new round's commitment transaction confirms.
func (s *VTXOPersistenceStore) MarkForfeited(
	ctx context.Context, outpoint wire.OutPoint,
	forfeitTxID, consumerBatchTxID chainhash.Hash,
) error {

	if consumerBatchTxID == (chainhash.Hash{}) {
		return fmt.Errorf("forfeit consumer batch txid is required")
	}

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		params := sqlc.MarkVTXOForfeitedParams{
			OutpointHash:        outpoint.Hash[:],
			OutpointIndex:       int32(outpoint.Index),
			ForfeitTxid:         forfeitTxID[:],
			ForfeitConsumerTxid: consumerBatchTxID[:],
			ReplacedByHash:      nil, // Set separately if needed.
			ReplacedByIndex: sql.NullInt32{
				Valid: false,
			},
			LastUpdateTime: s.clock.Now().Unix(),
		}

		return q.MarkVTXOForfeited(ctx, params)
	})
}

// DeleteVTXO removes a VTXO from storage. Used for cleanup after terminal
// states are reached and the VTXO is no longer needed.
func (s *VTXOPersistenceStore) DeleteVTXO(
	ctx context.Context, outpoint wire.OutPoint,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		params := sqlc.DeleteVTXOParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
		}

		return q.DeleteVTXO(ctx, params)
	})
}

// descriptorToInsertParams converts a vtxo.Descriptor to sqlc insert
// parameters. Ancestry paths are persisted separately in the
// vtxo_ancestry_paths side table by upsertAncestryPaths; this function
// only writes scalar columns that live on the vtxos row itself.
func (s *VTXOPersistenceStore) descriptorToInsertParams(ctx context.Context,
	q RoundStore, desc *vtxo.Descriptor) (InsertVTXOParams, error) {

	var operatorPubkey []byte
	if desc.OperatorKey != nil {
		operatorPubkey = desc.OperatorKey.SerializeCompressed()
	}

	nowUnix := s.clock.Now().Unix()

	// Register the local-ownership (client) key in the shared internal_keys
	// registry and reference it by id. The key is absent on the minimal row
	// the round store may create first; in that case the FK stays NULL and
	// is healed by a later full-descriptor insert.
	var clientKeyID sql.NullInt64
	if desc.ClientKey.PubKey != nil {
		id, err := RegisterInternalKeyTx(
			ctx, q, nowUnix, desc.ClientKey,
		)
		if err != nil {
			return InsertVTXOParams{}, fmt.Errorf("register "+
				"client key: %w", err)
		}

		clientKeyID = sql.NullInt64{Int64: id, Valid: true}
	}

	return InsertVTXOParams{
		OutpointHash:   desc.Outpoint.Hash[:],
		OutpointIndex:  int32(desc.Outpoint.Index),
		RoundID:        desc.RoundID,
		Amount:         int64(desc.Amount),
		PkScript:       desc.PkScript,
		Expiry:         int32(desc.RelativeExpiry),
		PolicyTemplate: bytes.Clone(desc.PolicyTemplate),
		ClientKeyID:    clientKeyID,
		OperatorPubkey: operatorPubkey,
		BatchExpiry:    desc.BatchExpiry,
		ChainDepth:     int32(desc.ChainDepth),
		CreatedHeight:  desc.CreatedHeight,
		CommitmentTxid: desc.CommitmentTxID[:],
		Spent:          false,
		CreationTime:   nowUnix,
		LastUpdateTime: nowUnix,

		// Stamp the VTXO's construction version. Versions are
		// zero-indexed, so an unstamped descriptor reads as V1 (the
		// zero value) with no normalization needed. The version is
		// immutable, so the InsertVTXO upsert never updates it on
		// conflict.
		ConstructionVersion: int32(desc.ConstructionVersion),
	}, nil
}

// rowToDescriptor converts a database VTXO row to a vtxo.Descriptor. The
// caller's context is threaded through so any diagnostics emitted during
// rehydrate (e.g. the expiry-drift warning) can pick up request-scoped
// logger metadata. The query handle is required to load ancestry rows
// from the vtxo_ancestry_paths side table introduced by migration 000009.
//
// preloaded is an optional per-outpoint ancestry index built by the
// caller (typically via groupAncestryRows over a batched ancestry
// query) so the list paths can avoid the per-row N+1
// ListVTXOAncestryPaths fetch. When preloaded is nil, rowToDescriptor
// falls back to the singleton query.
func (s *VTXOPersistenceStore) rowToDescriptor(ctx context.Context,
	q RoundStore, row VTXORow,
	preloaded map[wire.OutPoint][]vtxo.Ancestry) (*vtxo.Descriptor, error) {

	var outpointHash chainhash.Hash
	copy(outpointHash[:], row.OutpointHash)

	outpoint := wire.OutPoint{
		Hash:  outpointHash,
		Index: uint32(row.OutpointIndex),
	}

	// Hydrate the client key descriptor (pubkey + locator) from the
	// internal_keys registry via the client_key_id FK. The FK is NULL on a
	// minimal round-created row the VTXO manager has not yet healed; in
	// that case the descriptor key is left empty and the policy-template
	// fallback below may lift the owner pubkey instead.
	var clientKey keychain.KeyDescriptor
	if row.ClientKeyID.Valid {
		desc, err := InternalKeyDescByIDTx(
			ctx, q, row.ClientKeyID.Int64,
		)
		if err != nil {
			return nil, fmt.Errorf("hydrate client key: %w", err)
		}

		clientKey = desc
	}

	// Resolve the expensive derived parts (parsed keys, taproot script,
	// policy-resolved expiry) through the per-outpoint cache. The script
	// material is bound to the on-chain output, so an outpoint can never
	// map to different derived values; only the mutable row state below
	// is read fresh on every call.
	derived, ok := s.descriptorCache.get(outpoint)
	if !ok {
		var err error
		derived, err = s.deriveDescriptorParts(ctx, row, outpoint)
		if err != nil {
			return nil, err
		}

		if err := s.descriptorCache.put(outpoint, derived); err != nil {
			// A put failure only costs a future re-derivation.
			s.logger(ctx).WarnS(
				ctx,
				"Failed to cache VTXO descriptor parts",
				err,
				slog.String("outpoint", outpoint.String()),
			)
		}
	}

	// Load ancestry tree fragments from the side table. Round-direct
	// VTXOs and same-commitment OOR VTXOs surface a length-1 slice;
	// cross-commitment multi-input OOR VTXOs surface one entry per
	// distinct contributing commitment tx. List paths supply a
	// pre-grouped index so a batched list call runs in 2 queries
	// instead of N+1; the singleton path falls back to the per-row
	// query.
	var (
		ancestry []vtxo.Ancestry
		err      error
	)
	if preloaded != nil {
		ancestry = preloaded[outpoint]
	} else {
		ancestry, err = loadAncestryPathsWithCache(
			ctx, q, row.OutpointHash, row.OutpointIndex,
			s.ancestryCache,
		)
		if err != nil {
			return nil, fmt.Errorf("load ancestry paths: %w", err)
		}
	}

	// Parse commitment txid.
	var commitmentTxID chainhash.Hash
	if len(row.CommitmentTxid) == chainhash.HashSize {
		copy(commitmentTxID[:], row.CommitmentTxid)
	}

	if clientKey.PubKey == nil {
		clientKey.PubKey = derived.clientPubkey
	}

	forfeitConsumer, err := bytesToOptionHash(row.ForfeitConsumerTxid)
	if err != nil {
		return nil, fmt.Errorf("decode forfeit consumer txid: %w", err)
	}

	return &vtxo.Descriptor{
		Outpoint:             outpoint,
		Amount:               btcutil.Amount(row.Amount),
		PolicyTemplate:       derived.policyTemplate,
		PkScript:             row.PkScript,
		ClientKey:            clientKey,
		OperatorKey:          derived.operatorPubkey,
		TapScript:            derived.tapscript,
		Ancestry:             ancestry,
		RoundID:              row.RoundID,
		CommitmentTxID:       commitmentTxID,
		BatchExpiry:          row.BatchExpiry,
		RelativeExpiry:       derived.relativeExpiry,
		ChainDepth:           int(row.ChainDepth),
		CreatedHeight:        row.CreatedHeight,
		Status:               vtxo.VTXOStatus(row.Status),
		BusinessRevision:     uint64(row.BusinessRevision),
		ForfeitConsumerBatch: forfeitConsumer,
		ConstructionVersion: arkrpc.ConstructionVersion(
			row.ConstructionVersion,
		),
	}, nil
}

// deriveDescriptorParts computes the immutable derived parts of one VTXO row:
// the parsed public keys, the reconstructed taproot script, and the
// policy-resolved relative expiry. This is the expensive slice of descriptor
// decoding (secp256k1 point math, policy template decode), pulled out so
// rowToDescriptor can memoize it per outpoint.
func (s *VTXOPersistenceStore) deriveDescriptorParts(ctx context.Context,
	row VTXORow, outpoint wire.OutPoint) (*vtxoDescriptorCacheValue,
	error) {

	var clientPubkey *btcec.PublicKey
	policyTemplate := bytes.Clone(row.PolicyTemplate)

	// Parse operator public key.
	var operatorPubkey *btcec.PublicKey
	if len(row.OperatorPubkey) > 0 {
		key, err := btcec.ParsePubKey(row.OperatorPubkey)
		if err != nil {
			return nil, fmt.Errorf("parse operator pubkey: %w", err)
		}

		operatorPubkey = key
	}

	// Reconstruct the TapScript from the semantic policy when
	// the descriptor uses the standard Ark VTXO shape. Custom
	// policies keep TapScript nil and rely on explicit spend
	// paths instead.
	var tapscript *waddrmgr.Tapscript
	if len(policyTemplate) > 0 {
		desc := &vtxo.Descriptor{PolicyTemplate: policyTemplate}
		ts, err := desc.StandardTapScript()
		if err == nil {
			tapscript = ts
		}
	}

	relativeExpiry := uint32(row.Expiry)
	if len(policyTemplate) > 0 {
		template, err := arkscript.DecodePolicyTemplate(policyTemplate)
		if err != nil {
			return nil, fmt.Errorf("decode VTXO policy "+
				"template: %w", err)
		}

		// Prefer stored compressed pubkeys when available; only
		// lift from the policy template as a fallback. See
		// issue #252 for the encoding-wide discussion.
		if params, err := arkscript.DecodeStandardVTXOParams(
			template,
		); err == nil {

			if operatorPubkey == nil {
				operatorPubkey = params.OperatorKey
			}

			if clientPubkey == nil {
				clientPubkey = params.OwnerKey
			}

			// Warn on expiry drift between the stored column
			// and the policy-derived value: they should match
			// on a well-formed row. A mismatch points at a
			// corrupted row or a partially-applied migration,
			// and silently letting the policy win here would
			// mask the divergence.
			if relativeExpiry != 0 &&
				relativeExpiry != params.ExitDelay {

				s.logger(ctx).WarnS(
					ctx,
					"VTXO expiry drift between "+
						"stored column and "+
						"policy template",
					nil,
					slog.String(
						"outpoint", outpoint.String(),
					),
					slog.Uint64(
						"stored_expiry",
						uint64(relativeExpiry),
					),
					slog.Uint64(
						"policy_expiry",
						uint64(params.ExitDelay),
					),
				)
			}

			relativeExpiry = params.ExitDelay
		}
	}

	return &vtxoDescriptorCacheValue{
		clientPubkey:   clientPubkey,
		operatorPubkey: operatorPubkey,
		policyTemplate: policyTemplate,
		tapscript:      tapscript,
		relativeExpiry: relativeExpiry,
	}, nil
}

// Compile-time check that VTXOPersistenceStore implements vtxo.VTXOStore.
var _ vtxo.VTXOStore = (*VTXOPersistenceStore)(nil)
