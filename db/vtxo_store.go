package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
)

// VTXOPersistenceStore implements the vtxo.VTXOStore interface using the
// BatchedTx pattern for transaction-safe VTXO lifecycle operations.
type VTXOPersistenceStore struct {
	// db provides the underlying batched transaction executor.
	db BatchedRoundStore

	// clock provides time for timestamps.
	clock clock.Clock
}

// NewVTXOPersistenceStore creates a new VTXO persistence store using the
// transaction executor pattern.
func NewVTXOPersistenceStore(
	db BatchedRoundStore, c clock.Clock,
) *VTXOPersistenceStore {

	return &VTXOPersistenceStore{
		db:    db,
		clock: c,
	}
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

		params, err := s.descriptorToInsertParams(desc)
		if err != nil {
			return fmt.Errorf("convert descriptor: %w", err)
		}

		return q.InsertVTXO(ctx, params)
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
	}

	return q.InsertRound(ctx, params)
}

// GetVTXO retrieves a VTXO by its outpoint. Used for actor recovery on startup.
// Returns error if not found.
func (s *VTXOPersistenceStore) GetVTXO(
	ctx context.Context, outpoint wire.OutPoint,
) (*vtxo.Descriptor, error) {

	readTxOpts := ReadTxOption()

	var result *vtxo.Descriptor

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		params := sqlc.GetVTXOParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
		}

		row, err := q.GetVTXO(ctx, params)
		if err != nil {
			return fmt.Errorf("get VTXO: %w", err)
		}

		desc, err := s.rowToDescriptor(row)
		if err != nil {
			return fmt.Errorf("convert VTXO: %w", err)
		}

		result = desc

		return nil
	})

	return result, err
}

// ListLiveVTXOs returns all VTXOs not in a terminal state. Used during startup
// to recover active VTXO actors after restart.
func (s *VTXOPersistenceStore) ListLiveVTXOs(
	ctx context.Context,
) ([]*vtxo.Descriptor, error) {

	readTxOpts := ReadTxOption()

	var result []*vtxo.Descriptor

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		rows, err := q.ListLiveVTXOs(ctx)
		if err != nil {
			return fmt.Errorf("list live VTXOs: %w", err)
		}

		descs := make([]*vtxo.Descriptor, 0, len(rows))
		for _, row := range rows {
			desc, err := s.rowToDescriptor(row)
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
// directly from the database instead of filtering in memory.
func (s *VTXOPersistenceStore) ListVTXOsByStatus(
	ctx context.Context, status vtxo.VTXOStatus,
) ([]*vtxo.Descriptor, error) {

	readTxOpts := ReadTxOption()

	var result []*vtxo.Descriptor

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		rows, err := q.ListVTXOsByStatus(ctx, int32(status))
		if err != nil {
			return fmt.Errorf("list VTXOs by status: %w", err)
		}

		descs := make([]*vtxo.Descriptor, 0, len(rows))
		for _, row := range rows {
			desc, err := s.rowToDescriptor(row)
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
				return fmt.Errorf(
					"serialize forfeit tx: %w", err,
				)
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
func (s *VTXOPersistenceStore) GetForfeitTx(
	ctx context.Context, outpoint wire.OutPoint,
) (*wire.MsgTx, error) {

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
	ctx context.Context, outpoint wire.OutPoint, forfeitTxID chainhash.Hash,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		params := sqlc.MarkVTXOForfeitedParams{
			OutpointHash:   outpoint.Hash[:],
			OutpointIndex:  int32(outpoint.Index),
			ForfeitTxid:    forfeitTxID[:],
			ReplacedByHash: nil, // Set separately if needed.
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
// parameters.
func (s *VTXOPersistenceStore) descriptorToInsertParams(
	desc *vtxo.Descriptor,
) (InsertVTXOParams, error) {

	// Serialize tree path. Use empty blob if no path is available
	// (e.g., incoming VTXOs from round notifications).
	treePathBytes := []byte{}
	if desc.TreePath != nil {
		data, err := SerializeTree(desc.TreePath)
		if err != nil {
			return InsertVTXOParams{}, fmt.Errorf(
				"serialize tree path: %w", err,
			)
		}

		treePathBytes = data
	}

	var operatorPubkey []byte
	if desc.OperatorKey != nil {
		operatorPubkey = desc.OperatorKey.SerializeCompressed()
	}

	var clientPubkey []byte
	if desc.ClientKey.PubKey != nil {
		clientPubkey = desc.ClientKey.PubKey.SerializeCompressed()
	}

	nowUnix := s.clock.Now().Unix()

	return InsertVTXOParams{
		OutpointHash:    desc.Outpoint.Hash[:],
		OutpointIndex:   int32(desc.Outpoint.Index),
		RoundID:         desc.RoundID,
		Amount:          int64(desc.Amount),
		PkScript:        desc.PkScript,
		Expiry:          int32(desc.RelativeExpiry),
		PolicyTemplate:  bytes.Clone(desc.PolicyTemplate),
		ClientKeyFamily: int32(desc.ClientKey.Family),
		ClientKeyIndex:  int32(desc.ClientKey.Index),
		ClientPubkey:    clientPubkey,
		OperatorPubkey:  operatorPubkey,
		TreePath:        treePathBytes,
		BatchExpiry:     desc.BatchExpiry,
		TreeDepth:       int32(desc.TreeDepth),
		ChainDepth:      int32(desc.ChainDepth),
		CreatedHeight:   desc.CreatedHeight,
		CommitmentTxid:  desc.CommitmentTxID[:],
		Spent:           false,
		CreationTime:    nowUnix,
		LastUpdateTime:  nowUnix,
	}, nil
}

// rowToDescriptor converts a database VTXO row to a vtxo.Descriptor.
func (s *VTXOPersistenceStore) rowToDescriptor(
	row VTXORow,
) (*vtxo.Descriptor, error) {

	var outpointHash chainhash.Hash
	copy(outpointHash[:], row.OutpointHash)

	outpoint := wire.OutPoint{
		Hash:  outpointHash,
		Index: uint32(row.OutpointIndex),
	}

	// Parse client public key.
	var clientPubkey *btcec.PublicKey
	if len(row.ClientPubkey) > 0 {
		key, err := btcec.ParsePubKey(row.ClientPubkey)
		if err != nil {
			return nil, fmt.Errorf("parse client pubkey: %w", err)
		}

		clientPubkey = key
	}

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

	// Deserialize tree path.
	var treePath *tree.Tree
	if len(row.TreePath) > 0 {
		t, err := DeserializeTree(row.TreePath)
		if err != nil {
			return nil, fmt.Errorf("deserialize tree path: %w", err)
		}

		treePath = t
	}

	keyFamily := keychain.KeyFamily(row.ClientKeyFamily)

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
			return nil, fmt.Errorf(
				"decode VTXO policy template: %w", err,
			)
		}

		if params, err := arkscript.DecodeStandardVTXOParams(
			template,
		); err == nil {
			if operatorPubkey == nil {
				operatorPubkey = params.OperatorKey
			}
			relativeExpiry = params.ExitDelay

			if clientPubkey == nil {
				clientPubkey = params.OwnerKey
			}
		}
	}

	// Parse commitment txid.
	var commitmentTxID chainhash.Hash
	if len(row.CommitmentTxid) == chainhash.HashSize {
		copy(commitmentTxID[:], row.CommitmentTxid)
	}

	return &vtxo.Descriptor{
		Outpoint:       outpoint,
		Amount:         btcutil.Amount(row.Amount),
		PolicyTemplate: policyTemplate,
		PkScript:       row.PkScript,
		ClientKey: keychain.KeyDescriptor{
			PubKey: clientPubkey,
			KeyLocator: keychain.KeyLocator{
				Family: keyFamily,
				Index:  uint32(row.ClientKeyIndex),
			},
		},
		OperatorKey:    operatorPubkey,
		TapScript:      tapscript,
		TreePath:       treePath,
		RoundID:        row.RoundID,
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    row.BatchExpiry,
		RelativeExpiry: relativeExpiry,
		TreeDepth:      int(row.TreeDepth),
		ChainDepth:     int(row.ChainDepth),
		CreatedHeight:  row.CreatedHeight,
		Status:         vtxo.VTXOStatus(row.Status),
	}, nil
}

// Compile-time check that VTXOPersistenceStore implements vtxo.VTXOStore.
var _ vtxo.VTXOStore = (*VTXOPersistenceStore)(nil)
