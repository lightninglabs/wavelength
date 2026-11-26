package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/tap-sdk/embedded"
	"github.com/lightninglabs/wavelength/db/sqlc"
)

// ErrAssetJournalEntryNotFound reports a journal key with no stored value.
var ErrAssetJournalEntryNotFound = errors.New("asset journal entry not found")

// AssetEngineStore persists the keys, proofs, transfers, addresses and
// receives of the embedded asset engine in the daemon's database.
type AssetEngineStore struct {
	*TransactionExecutor[*sqlc.Queries]
}

// A compile-time check that AssetEngineStore is an engine store and an
// address store.
var (
	_ embedded.Store        = (*AssetEngineStore)(nil)
	_ embedded.AddressStore = (*AssetEngineStore)(nil)
)

// NewAssetEngineStore creates an AssetEngineStore from a Store.
func NewAssetEngineStore(store *Store) *AssetEngineStore {
	return &AssetEngineStore{
		TransactionExecutor: NewTransactionExecutor(
			store.BaseDB(),
			func(tx *sql.Tx) *sqlc.Queries {
				return store.Queries().WithTx(tx)
			},
			store.log,
		),
	}
}

// InsertInternalKey records a derived key. A known key is left as it is.
func (s *AssetEngineStore) InsertInternalKey(ctx context.Context,
	key embedded.KeyRecord) error {

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.InsertAssetEngineKey(
			ctx, sqlc.InsertAssetEngineKeyParams{
				Pubkey:    key.PubKey[:],
				KeyFamily: int64(key.Family),
				KeyIndex:  int64(key.Index),
			},
		)
	})
}

// FetchInternalKey returns the record of a derived key.
func (s *AssetEngineStore) FetchInternalKey(ctx context.Context,
	pubKey [33]byte) (*embedded.KeyRecord, error) {

	// An unknown key is an answer, not a failed transaction.
	var record *embedded.KeyRecord
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		record = nil

		row, err := q.GetAssetEngineKey(ctx, pubKey[:])
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}

		record, err = assetEngineKey(
			row.Pubkey, row.KeyFamily, row.KeyIndex,
		)

		return err
	})
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, embedded.ErrNotFound
	}

	return record, nil
}

// InsertScriptKey records a script key, replacing a record with the same
// tweaked key.
func (s *AssetEngineStore) InsertScriptKey(ctx context.Context,
	key embedded.ScriptKeyRecord) error {

	params := sqlc.UpsertAssetEngineScriptKeyParams{
		TweakedKey: key.TweakedKey[:],
		RawKey:     key.RawKey.PubKey[:],
		KeyFamily:  int64(key.RawKey.Family),
		KeyIndex:   int64(key.RawKey.Index),
		KeyType:    int32(key.Type),
	}
	if len(key.Tweak) != 0 {
		params.Tweak = key.Tweak
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.UpsertAssetEngineScriptKey(ctx, params)
	})
}

// FetchScriptKey returns the record of a tweaked script key.
func (s *AssetEngineStore) FetchScriptKey(ctx context.Context,
	tweakedKey [33]byte) (*embedded.ScriptKeyRecord, error) {

	var record *embedded.ScriptKeyRecord
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		record = nil

		row, err := q.GetAssetEngineScriptKey(ctx, tweakedKey[:])
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}

		rawKey, err := assetEngineKey(
			row.RawKey, row.KeyFamily, row.KeyIndex,
		)
		if err != nil {
			return err
		}
		if row.KeyType < 0 || row.KeyType > math.MaxUint8 {
			return fmt.Errorf("stored script key type %d out "+
				"of range", row.KeyType)
		}

		record = &embedded.ScriptKeyRecord{
			RawKey: *rawKey,
			Tweak:  row.Tweak,
			Type:   uint8(row.KeyType),
		}
		copy(record.TweakedKey[:], row.TweakedKey)

		return nil
	})
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, embedded.ErrNotFound
	}

	return record, nil
}

// InsertProof stores a proof file. A known output keeps its spent flag.
func (s *AssetEngineStore) InsertProof(ctx context.Context,
	proof embedded.AssetProof) error {

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return upsertAssetEngineProof(ctx, q, proof)
	})
}

// upsertAssetEngineProof stores a proof file within a transaction.
func upsertAssetEngineProof(ctx context.Context, q *sqlc.Queries,
	proof embedded.AssetProof) error {

	if len(proof.ProofFile) == 0 {
		return fmt.Errorf("asset proof file is empty")
	}

	return q.UpsertAssetEngineProof(ctx, sqlc.UpsertAssetEngineProofParams{
		AssetID:       proof.AssetID[:],
		ScriptKey:     proof.ScriptKey[:],
		OutpointHash:  proof.Outpoint.Hash[:],
		OutpointIndex: int64(proof.Outpoint.Index),
		ProofFile:     proof.ProofFile,
		Owned:         proof.Owned,
	})
}

// FetchProofs returns the proofs of an asset and script key. A nil outpoint
// matches every anchor output.
func (s *AssetEngineStore) FetchProofs(ctx context.Context, assetID [32]byte,
	scriptKey [33]byte, outpoint *wire.OutPoint) ([]embedded.AssetProof,
	error) {

	var proofs []embedded.AssetProof
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		proofs = nil

		if outpoint != nil {
			row, err := q.GetAssetEngineProof(
				ctx, sqlc.GetAssetEngineProofParams{
					AssetID:       assetID[:],
					ScriptKey:     scriptKey[:],
					OutpointHash:  outpoint.Hash[:],
					OutpointIndex: int64(outpoint.Index),
				},
			)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}

			proof, err := assetEngineProof(row)
			if err != nil {
				return err
			}
			proofs = append(proofs, *proof)

			return nil
		}

		rows, err := q.ListAssetEngineProofsByKey(
			ctx, sqlc.ListAssetEngineProofsByKeyParams{
				AssetID:   assetID[:],
				ScriptKey: scriptKey[:],
			},
		)
		if err != nil {
			return err
		}
		for _, row := range rows {
			proof, err := assetEngineProof(row)
			if err != nil {
				return err
			}
			proofs = append(proofs, *proof)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return proofs, nil
}

// ListOwnedProofs returns the asset outputs the wallet holds.
func (s *AssetEngineStore) ListOwnedProofs(ctx context.Context,
	includeSpent bool) ([]embedded.AssetProof, error) {

	var proofs []embedded.AssetProof
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		proofs = nil

		list := q.ListUnspentAssetEngineProofs
		if includeSpent {
			list = q.ListOwnedAssetEngineProofs
		}
		rows, err := list(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			proof, err := assetEngineProof(row)
			if err != nil {
				return err
			}
			proofs = append(proofs, *proof)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return proofs, nil
}

// InsertTransfer records a transfer and marks the outputs it spends.
func (s *AssetEngineStore) InsertTransfer(ctx context.Context,
	transfer embedded.Transfer, spends []embedded.ProofLocator) error {

	// A recorded transfer is an answer, not a failed transaction.
	var exists bool
	err := s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		exists = false

		rows, err := q.InsertAssetEngineTransfer(
			ctx, sqlc.InsertAssetEngineTransferParams{
				AnchorTxid: transfer.AnchorTxid[:],
				State:      int32(transfer.State),
				HeightHint: int64(transfer.HeightHint),
				BlockHash: assetEngineBlockHash(
					transfer.BlockHash,
				),
				BlockHeight: int64(transfer.BlockHeight),
				CreatedAt:   transfer.CreatedAt.UnixNano(),
				Payload:     transfer.Payload,
			},
		)
		if err != nil {
			return err
		}
		if rows == 0 {
			exists = true

			return nil
		}

		for _, locator := range spends {
			rows, err := q.MarkAssetEngineProofSpent(
				ctx, sqlc.MarkAssetEngineProofSpentParams{
					AssetID:      locator.AssetID[:],
					ScriptKey:    locator.ScriptKey[:],
					OutpointHash: locator.Outpoint.Hash[:],
					OutpointIndex: int64(
						locator.Outpoint.Index,
					),
				},
			)
			if err != nil {
				return err
			}
			if rows == 0 {
				return fmt.Errorf("spent asset output %v: %w",
					locator.Outpoint, embedded.ErrNotFound)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}
	if exists {
		return embedded.ErrTransferExists
	}

	return nil
}

// UpdateTransfer replaces the state, block and payload of a recorded
// transfer and applies its proof changes in the same transaction.
func (s *AssetEngineStore) UpdateTransfer(ctx context.Context,
	transfer embedded.Transfer, putProofs []embedded.AssetProof,
	deleteProofs []embedded.ProofLocator) error {

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.UpdateAssetEngineTransfer(
			ctx, sqlc.UpdateAssetEngineTransferParams{
				AnchorTxid: transfer.AnchorTxid[:],
				State:      int32(transfer.State),
				BlockHash: assetEngineBlockHash(
					transfer.BlockHash,
				),
				BlockHeight: int64(transfer.BlockHeight),
				Payload:     transfer.Payload,
			},
		)
		if err != nil {
			return err
		}
		if rows == 0 {
			return embedded.ErrNotFound
		}

		for _, proof := range putProofs {
			err := upsertAssetEngineProof(ctx, q, proof)
			if err != nil {
				return err
			}
		}
		for _, locator := range deleteProofs {
			err := q.DeleteAssetEngineProof(
				ctx, sqlc.DeleteAssetEngineProofParams{
					AssetID:      locator.AssetID[:],
					ScriptKey:    locator.ScriptKey[:],
					OutpointHash: locator.Outpoint.Hash[:],
					OutpointIndex: int64(
						locator.Outpoint.Index,
					),
				},
			)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// ListTransfers returns the recorded transfers, oldest first. A nil anchor
// transaction ID matches every transfer.
func (s *AssetEngineStore) ListTransfers(ctx context.Context,
	anchorTxid *chainhash.Hash) ([]embedded.Transfer, error) {

	var transfers []embedded.Transfer
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		transfers = nil

		if anchorTxid != nil {
			row, err := q.GetAssetEngineTransfer(
				ctx, anchorTxid[:],
			)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}

			transfer, err := assetEngineTransfer(row)
			if err != nil {
				return err
			}
			transfers = append(transfers, *transfer)

			return nil
		}

		rows, err := q.ListAssetEngineTransfers(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			transfer, err := assetEngineTransfer(row)
			if err != nil {
				return err
			}
			transfers = append(transfers, *transfer)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return transfers, nil
}

// InsertAddress records an address the engine created.
func (s *AssetEngineStore) InsertAddress(ctx context.Context,
	addr embedded.AddressRecord) error {

	// A recorded address is an answer, not a failed transaction.
	var exists bool
	err := s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.InsertAssetEngineAddress(
			ctx, sqlc.InsertAssetEngineAddressParams{
				ScriptKey:      addr.ScriptKey[:],
				Encoded:        addr.Encoded,
				InternalKey:    addr.InternalKey[:],
				CreationHeight: int64(addr.CreationHeight),
				CreatedAt:      addr.CreatedAt.UnixNano(),
			},
		)
		if err != nil {
			return err
		}
		exists = rows == 0

		return nil
	})
	if err != nil {
		return err
	}
	if exists {
		return embedded.ErrAddressExists
	}

	return nil
}

// ListAddresses returns the recorded addresses, oldest first. Addresses
// created at the same time keep the order of their insertion.
func (s *AssetEngineStore) ListAddresses(ctx context.Context) (
	[]embedded.AddressRecord, error) {

	var addrs []embedded.AddressRecord
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		addrs = nil

		rows, err := q.ListAssetEngineAddresses(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			addr, err := assetEngineAddress(row)
			if err != nil {
				return err
			}
			addrs = append(addrs, *addr)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return addrs, nil
}

// InsertReceive records an announced transfer and returns it. A recorded
// announcement of the same output and address is returned as it is stored.
func (s *AssetEngineStore) InsertReceive(ctx context.Context,
	receive embedded.ReceiveRecord) (*embedded.ReceiveRecord, error) {

	var record *embedded.ReceiveRecord
	err := s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		record = nil

		rows, err := q.InsertAssetEngineReceive(
			ctx, sqlc.InsertAssetEngineReceiveParams{
				OutpointHash: receive.Outpoint.Hash[:],
				OutpointIndex: int64(
					receive.Outpoint.Index,
				),
				AddressScriptKey: receive.Address[:],
				Announcement:     receive.Announcement[:],
				State:            int32(receive.State),
				BlockHash: assetEngineBlockHash(
					receive.BlockHash,
				),
				BlockHeight: int64(receive.BlockHeight),
				CreatedAt:   receive.CreatedAt.UnixNano(),
				Payload:     receive.Payload,
			},
		)
		if err != nil {
			return err
		}
		if rows != 0 {
			record = &receive

			return nil
		}

		row, err := q.GetAssetEngineReceive(
			ctx, sqlc.GetAssetEngineReceiveParams{
				OutpointHash: receive.Outpoint.Hash[:],
				OutpointIndex: int64(
					receive.Outpoint.Index,
				),
				AddressScriptKey: receive.Address[:],
				Announcement:     receive.Announcement[:],
			},
		)
		if err != nil {
			return err
		}
		record, err = assetEngineReceive(row)

		return err
	})
	if err != nil {
		return nil, err
	}

	return record, nil
}

// UpdateReceive replaces the state, block and payload of a recorded receive
// and stores its proofs in the same transaction, so the wallet holds the
// assets of a receive exactly when the receive reads confirmed.
func (s *AssetEngineStore) UpdateReceive(ctx context.Context,
	receive embedded.ReceiveRecord, putProofs []embedded.AssetProof) error {

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.UpdateAssetEngineReceive(
			ctx, sqlc.UpdateAssetEngineReceiveParams{
				OutpointHash: receive.Outpoint.Hash[:],
				OutpointIndex: int64(
					receive.Outpoint.Index,
				),
				AddressScriptKey: receive.Address[:],
				Announcement:     receive.Announcement[:],
				State:            int32(receive.State),
				BlockHash: assetEngineBlockHash(
					receive.BlockHash,
				),
				BlockHeight: int64(receive.BlockHeight),
				Payload:     receive.Payload,
			},
		)
		if err != nil {
			return err
		}
		if rows == 0 {
			return embedded.ErrNotFound
		}

		for _, proof := range putProofs {
			err := upsertAssetEngineProof(ctx, q, proof)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// ListReceives returns the recorded receives that match the filter, oldest
// first.
func (s *AssetEngineStore) ListReceives(ctx context.Context,
	filter embedded.ReceiveFilter) ([]embedded.ReceiveRecord, error) {

	var receives []embedded.ReceiveRecord
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		receives = nil

		var (
			rows []sqlc.AssetEngineReceive
			err  error
		)
		if filter.Outpoint != nil {
			rows, err = q.ListAssetEngineReceivesByOutpoint(
				ctx,
				sqlc.ListAssetEngineReceivesByOutpointParams{
					OutpointHash: filter.Outpoint.Hash[:],
					OutpointIndex: int64(
						filter.Outpoint.Index,
					),
				},
			)
		} else {
			rows, err = q.ListAssetEngineReceives(ctx)
		}
		if err != nil {
			return err
		}

		for _, row := range rows {
			receive, err := assetEngineReceive(row)
			if err != nil {
				return err
			}
			if filter.Address != nil &&
				receive.Address != *filter.Address {

				continue
			}
			if filter.State != nil &&
				receive.State != *filter.State {

				continue
			}
			receives = append(receives, *receive)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return receives, nil
}

// LoadJournalEntry returns the journal value stored under key, or
// ErrAssetJournalEntryNotFound.
func (s *AssetEngineStore) LoadJournalEntry(ctx context.Context, key string) (
	[]byte, error) {

	// An absent entry is an answer, not a failed transaction.
	var (
		value []byte
		found bool
	)
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		value, found = nil, false

		stored, err := q.GetAssetJournalEntry(ctx, key)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		value, found = stored, true

		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrAssetJournalEntryNotFound
	}

	return value, nil
}

// StoreJournalEntry stores a journal value under key, replacing a stored
// one.
func (s *AssetEngineStore) StoreJournalEntry(ctx context.Context, key string,
	value []byte) error {

	if len(value) == 0 {
		return fmt.Errorf("asset journal value is empty")
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.UpsertAssetJournalEntry(
			ctx, sqlc.UpsertAssetJournalEntryParams{
				JournalKey: key,
				Value:      value,
			},
		)
	})
}

// assetEngineBlockHash returns a confirming block hash as a column value,
// NULL for the zero hash of a record without a confirmation.
func assetEngineBlockHash(blockHash chainhash.Hash) []byte {
	if blockHash == (chainhash.Hash{}) {
		return nil
	}

	return blockHash[:]
}

// assetEngineKey converts stored key columns to a key record.
func assetEngineKey(pubKey []byte, family,
	index int64) (*embedded.KeyRecord, error) {

	if len(pubKey) != 33 || family < 0 || family > math.MaxUint32 ||
		index < 0 || index > math.MaxUint32 {
		return nil, fmt.Errorf("stored asset key is malformed")
	}

	record := &embedded.KeyRecord{
		Family: uint32(family),
		Index:  uint32(index),
	}
	copy(record.PubKey[:], pubKey)

	return record, nil
}

// assetEngineProof converts a stored row to an asset proof.
func assetEngineProof(row sqlc.AssetEngineProof) (*embedded.AssetProof, error) {
	if len(row.AssetID) != 32 || len(row.ScriptKey) != 33 ||
		len(row.OutpointHash) != chainhash.HashSize ||
		row.OutpointIndex < 0 || row.OutpointIndex > math.MaxUint32 {
		return nil, fmt.Errorf("stored asset proof is malformed")
	}

	proof := &embedded.AssetProof{
		ProofFile: row.ProofFile,
		Owned:     row.Owned,
		Spent:     row.Spent,
	}
	copy(proof.AssetID[:], row.AssetID)
	copy(proof.ScriptKey[:], row.ScriptKey)
	copy(proof.Outpoint.Hash[:], row.OutpointHash)
	proof.Outpoint.Index = uint32(row.OutpointIndex)

	return proof, nil
}

// assetEngineTransfer converts a stored row to a transfer.
func assetEngineTransfer(row sqlc.AssetEngineTransfer) (*embedded.Transfer,
	error) {

	if len(row.AnchorTxid) != chainhash.HashSize ||
		row.State < 0 || row.State > int32(embedded.TransferFinal) ||
		row.HeightHint < 0 || row.HeightHint > math.MaxUint32 ||
		row.BlockHeight < 0 || row.BlockHeight > math.MaxUint32 ||
		(row.BlockHash != nil &&
			len(row.BlockHash) != chainhash.HashSize) {
		return nil, fmt.Errorf("stored asset transfer is malformed")
	}

	transfer := &embedded.Transfer{
		State:       embedded.TransferState(row.State),
		HeightHint:  uint32(row.HeightHint),
		BlockHeight: uint32(row.BlockHeight),
		CreatedAt:   time.Unix(0, row.CreatedAt),
		Payload:     row.Payload,
	}
	copy(transfer.AnchorTxid[:], row.AnchorTxid)
	copy(transfer.BlockHash[:], row.BlockHash)

	return transfer, nil
}

// assetEngineAddress converts a stored row to an address record.
func assetEngineAddress(row sqlc.AssetEngineAddress) (*embedded.AddressRecord,
	error) {

	if len(row.ScriptKey) != 33 || len(row.InternalKey) != 33 ||
		row.CreationHeight < 0 || row.CreationHeight > math.MaxUint32 {
		return nil, fmt.Errorf("stored asset address is malformed")
	}

	addr := &embedded.AddressRecord{
		Encoded:        row.Encoded,
		CreationHeight: uint32(row.CreationHeight),
		CreatedAt:      time.Unix(0, row.CreatedAt),
	}
	copy(addr.ScriptKey[:], row.ScriptKey)
	copy(addr.InternalKey[:], row.InternalKey)

	return addr, nil
}

// assetEngineReceive converts a stored row to a receive record.
func assetEngineReceive(row sqlc.AssetEngineReceive) (*embedded.ReceiveRecord,
	error) {

	if len(row.OutpointHash) != chainhash.HashSize ||
		row.OutpointIndex < 0 || row.OutpointIndex > math.MaxUint32 ||
		len(row.AddressScriptKey) != 33 ||
		len(row.Announcement) != 32 ||
		row.State < 0 || row.State > math.MaxUint8 ||
		row.BlockHeight < 0 || row.BlockHeight > math.MaxUint32 ||
		(row.BlockHash != nil &&
			len(row.BlockHash) != chainhash.HashSize) {
		return nil, fmt.Errorf("stored asset receive is malformed")
	}

	receive := &embedded.ReceiveRecord{
		State:       embedded.ReceiveState(row.State),
		BlockHeight: uint32(row.BlockHeight),
		CreatedAt:   time.Unix(0, row.CreatedAt),
		Payload:     row.Payload,
	}
	copy(receive.Outpoint.Hash[:], row.OutpointHash)
	receive.Outpoint.Index = uint32(row.OutpointIndex)
	copy(receive.Address[:], row.AddressScriptKey)
	copy(receive.Announcement[:], row.Announcement)
	copy(receive.BlockHash[:], row.BlockHash)

	return receive, nil
}
