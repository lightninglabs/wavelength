package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/virtualchannel"
)

// VirtualChannelStoreDB persists virtual Lightning channel registrations and
// their backing VTXO material.
type VirtualChannelStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]

	clock func() time.Time
}

// NewVirtualChannelStoreDB creates a virtual channel store bound to the
// underlying Store's connection pool.
func NewVirtualChannelStoreDB(store *Store) *VirtualChannelStoreDB {
	txExec := NewTransactionExecutor(
		store.BaseDB(),
		func(tx *sql.Tx) *sqlc.Queries {
			return store.Queries().WithTx(tx)
		}, store.log,
	)

	return &VirtualChannelStoreDB{
		TransactionExecutor: txExec,
		clock:               time.Now,
	}
}

// InsertVirtualChannelPendingOpen persists the pre-lnd-open intent that the
// inbound channel acceptor uses before the funding parent is known.
func (s *VirtualChannelStoreDB) InsertVirtualChannelPendingOpen(
	ctx context.Context, pending virtualchannel.PendingOpen) error {

	if len(pending.BackingVTXOs) == 0 {
		return fmt.Errorf("virtual channel intent has no backing VTXOs")
	}

	now := s.clock().UTC().UnixNano()

	return s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		err := qtx.InsertVirtualChannelIntent(
			ctx, sqlc.InsertVirtualChannelIntentParams{
				PendingChannelID: pending.PendingChannelID[:],
				RemoteNodePubkey: pending.RemoteNodePubKey[:],
				Role:             string(pending.Role),
				Status:           string(pending.Status),
				CapacitySat:      int64(pending.Capacity),
				LocalBalanceSat:  int64(pending.LocalBalance),
				RemoteBalanceSat: int64(pending.RemoteBalance),
				CreatedAt:        now,
				UpdatedAt:        now,
			},
		)
		if err != nil {
			return err
		}

		for _, backing := range pending.BackingVTXOs {
			backingHash := backing.OutPoint.Hash[:]
			err := qtx.InsertVirtualChannelIntentVTXO(
				ctx, sqlc.InsertVirtualChannelIntentVTXOParams{
					PendingChannelID: pending.
						PendingChannelID[:],
					OutpointHash: backingHash,
					OutpointIndex: int32(
						backing.OutPoint.Index,
					),
					AmountSat: int64(backing.Amount),
				},
			)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// InsertVirtualChannel persists a virtual channel and all backing VTXOs in one
// transaction. Negotiating channels can store the unsigned txid-stable backing
// parent; active channels must update that artifact with all conflict-safe
// witnesses before the publish hook can use it after a crash.
func (s *VirtualChannelStoreDB) InsertVirtualChannel(ctx context.Context,
	reg virtualchannel.Registration) error {

	if reg.BackingTx == nil {
		return fmt.Errorf("virtual channel backing tx is nil")
	}

	if len(reg.BackingVTXOs) == 0 {
		return fmt.Errorf("virtual channel has no backing VTXOs")
	}

	backingTx, err := encodeMsgTx(reg.BackingTx)
	if err != nil {
		return err
	}

	now := s.clock().UTC().UnixNano()

	return s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		err := qtx.InsertVirtualChannel(
			ctx, sqlc.InsertVirtualChannelParams{
				VirtualChannelID: reg.ID[:],
				PendingChannelID: reg.PendingChannelID[:],
				ChannelPointHash: reg.ChannelPoint.Hash[:],
				ChannelPointIndex: int32(
					reg.ChannelPoint.Index,
				),
				RemoteNodePubkey: reg.RemoteNodePubKey[:],
				Role:             string(reg.Role),
				Status:           string(reg.Status),
				CapacitySat:      int64(reg.Capacity),
				LocalBalanceSat:  int64(reg.LocalBalance),
				RemoteBalanceSat: int64(reg.RemoteBalance),
				BackingTx:        backingTx,
				FundingPsbt:      reg.FundingPsbt,
				CreatedAt:        now,
				UpdatedAt:        now,
			},
		)
		if err != nil {
			return err
		}

		_, err = qtx.DeleteVirtualChannelIntent(
			ctx, reg.PendingChannelID[:],
		)
		if err != nil {
			return err
		}

		for _, backing := range reg.BackingVTXOs {
			backingHash := backing.OutPoint.Hash[:]
			err := qtx.InsertVirtualChannelVTXO(
				ctx, sqlc.InsertVirtualChannelVTXOParams{
					VirtualChannelID: reg.ID[:],
					OutpointHash:     backingHash,
					OutpointIndex: int32(
						backing.OutPoint.Index,
					),
					AmountSat: int64(backing.Amount),
				},
			)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// GetVirtualChannel loads a virtual channel by its stable darepo id.
func (s *VirtualChannelStoreDB) GetVirtualChannel(ctx context.Context,
	id virtualchannel.ID) (*virtualchannel.Channel, error) {

	var channel *virtualchannel.Channel
	err := s.ExecTx(ctx, ReadTxOption(), func(qtx *sqlc.Queries) error {
		row, err := qtx.GetVirtualChannel(ctx, id[:])
		if err != nil {
			return err
		}

		channel, err = s.loadChannel(ctx, qtx, row)

		return err
	})

	return channel, err
}

// GetVirtualChannelByChannelPoint loads the virtual channel backing the given
// lnd funding outpoint.
func (s *VirtualChannelStoreDB) GetVirtualChannelByChannelPoint(
	ctx context.Context, channelPoint wire.OutPoint) (
	*virtualchannel.Channel, error) {

	var channel *virtualchannel.Channel
	err := s.ExecTx(ctx, ReadTxOption(), func(qtx *sqlc.Queries) error {
		row, err := qtx.GetVirtualChannelByChannelPoint(
			ctx, sqlc.GetVirtualChannelByChannelPointParams{
				ChannelPointHash: channelPoint.Hash[:],
				ChannelPointIndex: int32(
					channelPoint.Index,
				),
			},
		)
		if err != nil {
			return err
		}

		channel, err = s.loadChannel(ctx, qtx, row)

		return err
	})

	return channel, err
}

// FindVirtualChannelByChannelPoint loads the virtual channel backing the given
// lnd funding outpoint and reports whether a row exists.
func (s *VirtualChannelStoreDB) FindVirtualChannelByChannelPoint(
	ctx context.Context, channelPoint wire.OutPoint) (
	*virtualchannel.Channel, bool, error) {

	channel, err := s.GetVirtualChannelByChannelPoint(ctx, channelPoint)
	if err == nil {
		return channel, true, nil
	}

	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}

	return nil, false, err
}

// GetVirtualChannelByPendingChannelID loads the virtual channel negotiation
// that uses the given lnd pending channel id.
func (s *VirtualChannelStoreDB) GetVirtualChannelByPendingChannelID(
	ctx context.Context, pendingID virtualchannel.PendingChannelID) (
	*virtualchannel.Channel, error) {

	var channel *virtualchannel.Channel
	err := s.ExecTx(ctx, ReadTxOption(), func(qtx *sqlc.Queries) error {
		row, err := qtx.GetVirtualChannelByPendingID(
			ctx, pendingID[:],
		)
		if err != nil {
			return err
		}

		channel, err = s.loadChannel(ctx, qtx, row)

		return err
	})

	return channel, err
}

// FindVirtualChannelByPendingChannelID loads the virtual channel negotiation
// that uses the given lnd pending channel id and reports whether a row exists.
func (s *VirtualChannelStoreDB) FindVirtualChannelByPendingChannelID(
	ctx context.Context, pendingID virtualchannel.PendingChannelID) (
	*virtualchannel.Channel, bool, error) {

	channel, err := s.GetVirtualChannelByPendingChannelID(ctx, pendingID)
	if err == nil {
		return channel, true, nil
	}

	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}

	return nil, false, err
}

// FindVirtualChannelPendingOpen loads either a full registration or a pending
// intent for the given lnd pending channel id.
func (s *VirtualChannelStoreDB) FindVirtualChannelPendingOpen(
	ctx context.Context, pendingID virtualchannel.PendingChannelID) (
	*virtualchannel.PendingOpen, bool, error) {

	channel, ok, err := s.FindVirtualChannelByPendingChannelID(
		ctx, pendingID,
	)
	if err != nil {
		return nil, false, err
	}
	if ok {
		return pendingOpenFromRegistration(channel.Registration),
			true, nil
	}

	var pending *virtualchannel.PendingOpen
	err = s.ExecTx(ctx, ReadTxOption(), func(qtx *sqlc.Queries) error {
		row, err := qtx.GetVirtualChannelIntentByPendingID(
			ctx, pendingID[:],
		)
		if err != nil {
			return err
		}

		pending, err = s.loadPendingOpen(ctx, qtx, row)

		return err
	})
	if err == nil {
		return pending, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}

	return nil, false, err
}

// ListVirtualChannelsByFundingTxID loads virtual channels whose channel point
// is an output of the given funding transaction id.
func (s *VirtualChannelStoreDB) ListVirtualChannelsByFundingTxID(
	ctx context.Context, txid chainhash.Hash) ([]*virtualchannel.Channel,
	error) {

	var channels []*virtualchannel.Channel
	err := s.ExecTx(ctx, ReadTxOption(), func(qtx *sqlc.Queries) error {
		rows, err := qtx.ListVirtualChannelsByChannelPointHash(
			ctx, txid[:],
		)
		if err != nil {
			return err
		}

		channels = make([]*virtualchannel.Channel, 0, len(rows))
		for _, row := range rows {
			channel, err := s.loadChannel(ctx, qtx, row)
			if err != nil {
				return err
			}

			channels = append(channels, channel)
		}

		return nil
	})

	return channels, err
}

// ListVirtualChannelsByStatus loads virtual channels with the given lifecycle
// status.
func (s *VirtualChannelStoreDB) ListVirtualChannelsByStatus(ctx context.Context,
	status virtualchannel.Status) ([]*virtualchannel.Channel, error) {

	var channels []*virtualchannel.Channel
	err := s.ExecTx(ctx, ReadTxOption(), func(qtx *sqlc.Queries) error {
		rows, err := qtx.ListVirtualChannelsByStatus(
			ctx, string(status),
		)
		if err != nil {
			return err
		}

		channels = make([]*virtualchannel.Channel, 0, len(rows))
		for _, row := range rows {
			channel, err := s.loadChannel(ctx, qtx, row)
			if err != nil {
				return err
			}

			channels = append(channels, channel)
		}

		return nil
	})

	return channels, err
}

// MarkVirtualChannelActive replaces the unsigned negotiating parent with the
// publishable backing parent and marks the virtual channel active. The backing
// transaction must carry witnesses for every input; adding witness data leaves
// the txid stable, and the SQL update also requires that txid to match the
// registered lnd channel point hash.
func (s *VirtualChannelStoreDB) MarkVirtualChannelActive(ctx context.Context,
	id virtualchannel.ID, backingTx *wire.MsgTx) (bool, error) {

	if err := validateActiveBackingTx(backingTx); err != nil {
		return false, err
	}

	backingTxBytes, err := encodeMsgTx(backingTx)
	if err != nil {
		return false, err
	}

	txid := backingTx.TxHash()
	now := s.clock().UTC().UnixNano()
	var rows int64
	err = s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		var err error
		rows, err = qtx.MarkVirtualChannelActive(
			ctx, sqlc.MarkVirtualChannelActiveParams{
				VirtualChannelID: id[:],
				BackingTx:        backingTxBytes,
				UpdatedAt:        now,
				ChannelPointHash: txid[:],
			},
		)

		return err
	})

	return rows > 0, err
}

// MarkVirtualChannelMaterializing records that the backing parent is being
// published for a conflict or force-close path.
func (s *VirtualChannelStoreDB) MarkVirtualChannelMaterializing(
	ctx context.Context, id virtualchannel.ID) (bool, error) {

	now := s.clock().UTC().UnixNano()
	var rows int64
	err := s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		var err error
		rows, err = qtx.MarkVirtualChannelMaterializing(
			ctx, sqlc.MarkVirtualChannelMaterializingParams{
				VirtualChannelID: id[:],
				UpdatedAt:        now,
				MaterializedAt: sql.NullInt64{
					Int64: now,
					Valid: true,
				},
			},
		)

		return err
	})

	return rows > 0, err
}

// MarkVirtualChannelClosing records that close resolution has started.
func (s *VirtualChannelStoreDB) MarkVirtualChannelClosing(ctx context.Context,
	id virtualchannel.ID) (bool, error) {

	now := s.clock().UTC().UnixNano()
	var rows int64
	err := s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		var err error
		rows, err = qtx.MarkVirtualChannelClosing(
			ctx, sqlc.MarkVirtualChannelClosingParams{
				VirtualChannelID: id[:],
				UpdatedAt:        now,
			},
		)

		return err
	})

	return rows > 0, err
}

// MarkVirtualChannelClosed records that close resolution reached a terminal
// state.
func (s *VirtualChannelStoreDB) MarkVirtualChannelClosed(ctx context.Context,
	id virtualchannel.ID) (bool, error) {

	now := s.clock().UTC().UnixNano()
	var rows int64
	err := s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		var err error
		rows, err = qtx.MarkVirtualChannelClosed(
			ctx, sqlc.MarkVirtualChannelClosedParams{
				VirtualChannelID: id[:],
				UpdatedAt:        now,
				ClosedAt: sql.NullInt64{
					Int64: now,
					Valid: true,
				},
			},
		)

		return err
	})

	return rows > 0, err
}

// MarkVirtualChannelCoopClosed records a cooperative close that was settled
// virtually instead of publishing the backing parent. The close transaction
// must spend the registered lnd channel point.
func (s *VirtualChannelStoreDB) MarkVirtualChannelCoopClosed(
	ctx context.Context, id virtualchannel.ID, closeTx *wire.MsgTx,
	localBalance, remoteBalance btcutil.Amount) (bool, error) {

	if localBalance < 0 {
		return false, fmt.Errorf("local close balance is negative")
	}
	if remoteBalance < 0 {
		return false, fmt.Errorf("remote close balance is negative")
	}
	if closeTx == nil {
		return false, fmt.Errorf("virtual channel close tx is nil")
	}

	closeTxBytes, err := encodeMsgTx(closeTx)
	if err != nil {
		return false, err
	}

	now := s.clock().UTC().UnixNano()
	var rows int64
	err = s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		row, err := qtx.GetVirtualChannel(ctx, id[:])
		if err != nil {
			return err
		}

		channelPoint, err := outPoint(
			row.ChannelPointHash, row.ChannelPointIndex,
		)
		if err != nil {
			return err
		}
		if !txSpendsOutPoint(closeTx, channelPoint) {
			return fmt.Errorf("virtual channel close tx does not "+
				"spend channel point %v", channelPoint)
		}
		if virtualchannel.Status(row.Status) ==
			virtualchannel.StatusClosed {

			if !bytes.Equal(row.CloseTx, closeTxBytes) {
				return fmt.Errorf("virtual channel %x is "+
					"already closed with a different "+
					"close tx", id)
			}
			if row.LocalBalanceSat != int64(localBalance) ||
				row.RemoteBalanceSat != int64(remoteBalance) {
				return fmt.Errorf("virtual channel %x is "+
					"already closed with different "+
					"balances", id)
			}

			rows = 1

			return nil
		}

		rows, err = qtx.MarkVirtualChannelCoopClosed(
			ctx, sqlc.MarkVirtualChannelCoopClosedParams{
				VirtualChannelID: id[:],
				LocalBalanceSat:  int64(localBalance),
				RemoteBalanceSat: int64(remoteBalance),
				CloseTx:          closeTxBytes,
				UpdatedAt:        now,
				ClosedAt: sql.NullInt64{
					Int64: now,
					Valid: true,
				},
			},
		)

		return err
	})

	return rows > 0, err
}

// MarkVirtualChannelFailed records that negotiation failed before the virtual
// channel became active.
func (s *VirtualChannelStoreDB) MarkVirtualChannelFailed(ctx context.Context,
	id virtualchannel.ID) (bool, error) {

	now := s.clock().UTC().UnixNano()
	var rows int64
	err := s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		var err error
		rows, err = qtx.MarkVirtualChannelFailed(
			ctx, sqlc.MarkVirtualChannelFailedParams{
				VirtualChannelID: id[:],
				UpdatedAt:        now,
			},
		)

		return err
	})

	return rows > 0, err
}

func validateActiveBackingTx(tx *wire.MsgTx) error {
	if tx == nil {
		return fmt.Errorf("virtual channel backing tx is nil")
	}
	if len(tx.TxIn) == 0 {
		return fmt.Errorf("virtual channel backing tx has no inputs")
	}
	if len(tx.TxOut) == 0 {
		return fmt.Errorf("virtual channel backing tx has no outputs")
	}

	for idx, txIn := range tx.TxIn {
		if len(txIn.Witness) == 0 {
			return fmt.Errorf("virtual channel backing tx input "+
				"%d has no witness", idx)
		}
	}

	return nil
}

// loadChannel converts a sqlc virtual channel row and its VTXO children into
// the domain type used by activation and publish hooks.
func (s *VirtualChannelStoreDB) loadChannel(ctx context.Context,
	qtx *sqlc.Queries, row sqlc.VirtualChannel) (*virtualchannel.Channel,
	error) {

	vtxoRows, err := qtx.ListVirtualChannelVTXOs(
		ctx, row.VirtualChannelID,
	)
	if err != nil {
		return nil, err
	}

	backingTx, err := decodeMsgTx(row.BackingTx)
	if err != nil {
		return nil, err
	}

	id, err := virtualChannelID(row.VirtualChannelID)
	if err != nil {
		return nil, err
	}

	pendingID, err := pendingChannelID(row.PendingChannelID)
	if err != nil {
		return nil, err
	}

	channelPoint, err := outPoint(
		row.ChannelPointHash, row.ChannelPointIndex,
	)
	if err != nil {
		return nil, err
	}

	nodePubKey, err := nodePubKey(row.RemoteNodePubkey)
	if err != nil {
		return nil, err
	}

	backingVTXOs := make(
		[]virtualchannel.BackingVTXO, 0, len(vtxoRows),
	)
	for _, vtxoRow := range vtxoRows {
		op, err := outPoint(
			vtxoRow.OutpointHash, vtxoRow.OutpointIndex,
		)
		if err != nil {
			return nil, err
		}

		backingVTXOs = append(
			backingVTXOs, virtualchannel.BackingVTXO{
				OutPoint: op,
				Amount:   btcutil.Amount(vtxoRow.AmountSat),
			},
		)
	}

	var closeTx *wire.MsgTx
	if len(row.CloseTx) > 0 {
		closeTx, err = decodeMsgTx(row.CloseTx)
		if err != nil {
			return nil, err
		}
	}

	return &virtualchannel.Channel{
		Registration: virtualchannel.Registration{
			ID:               id,
			PendingChannelID: pendingID,
			ChannelPoint:     channelPoint,
			RemoteNodePubKey: nodePubKey,
			Role:             virtualchannel.Role(row.Role),
			Status:           virtualchannel.Status(row.Status),
			Capacity: btcutil.Amount(
				row.CapacitySat,
			),
			LocalBalance: btcutil.Amount(
				row.LocalBalanceSat,
			),
			RemoteBalance: btcutil.Amount(
				row.RemoteBalanceSat,
			),
			BackingTx:    backingTx,
			FundingPsbt:  row.FundingPsbt,
			BackingVTXOs: backingVTXOs,
		},
		CreatedAt:      time.Unix(0, row.CreatedAt).UTC(),
		UpdatedAt:      time.Unix(0, row.UpdatedAt).UTC(),
		MaterializedAt: nullUnixNano(row.MaterializedAt),
		ClosedAt:       nullUnixNano(row.ClosedAt),
		CloseTx:        closeTx,
	}, nil
}

func (s *VirtualChannelStoreDB) loadPendingOpen(ctx context.Context,
	qtx *sqlc.Queries, row sqlc.VirtualChannelIntent) (
	*virtualchannel.PendingOpen, error) {

	vtxoRows, err := qtx.ListVirtualChannelIntentVTXOs(
		ctx, row.PendingChannelID,
	)
	if err != nil {
		return nil, err
	}

	pendingID, err := pendingChannelID(row.PendingChannelID)
	if err != nil {
		return nil, err
	}

	nodePubKey, err := nodePubKey(row.RemoteNodePubkey)
	if err != nil {
		return nil, err
	}

	backingVTXOs := make(
		[]virtualchannel.BackingVTXO, 0, len(vtxoRows),
	)
	for _, vtxoRow := range vtxoRows {
		op, err := outPoint(
			vtxoRow.OutpointHash, vtxoRow.OutpointIndex,
		)
		if err != nil {
			return nil, err
		}

		backingVTXOs = append(
			backingVTXOs, virtualchannel.BackingVTXO{
				OutPoint: op,
				Amount:   btcutil.Amount(vtxoRow.AmountSat),
			},
		)
	}

	return &virtualchannel.PendingOpen{
		PendingChannelID: pendingID,
		RemoteNodePubKey: nodePubKey,
		Role:             virtualchannel.Role(row.Role),
		Status:           virtualchannel.Status(row.Status),
		Capacity:         btcutil.Amount(row.CapacitySat),
		LocalBalance:     btcutil.Amount(row.LocalBalanceSat),
		RemoteBalance:    btcutil.Amount(row.RemoteBalanceSat),
		BackingVTXOs:     backingVTXOs,
	}, nil
}

func pendingOpenFromRegistration(
	reg virtualchannel.Registration) *virtualchannel.PendingOpen {

	return &virtualchannel.PendingOpen{
		PendingChannelID: reg.PendingChannelID,
		RemoteNodePubKey: reg.RemoteNodePubKey,
		Role:             reg.Role,
		Status:           reg.Status,
		Capacity:         reg.Capacity,
		LocalBalance:     reg.LocalBalance,
		RemoteBalance:    reg.RemoteBalance,
		BackingVTXOs:     reg.BackingVTXOs,
	}
}

// encodeMsgTx serializes a Bitcoin transaction using wire encoding.
func encodeMsgTx(tx *wire.MsgTx) ([]byte, error) {
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// decodeMsgTx deserializes a Bitcoin transaction using wire encoding.
func decodeMsgTx(raw []byte) (*wire.MsgTx, error) {
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}

	return tx, nil
}

// txSpendsOutPoint reports whether tx has an input spending outpoint.
func txSpendsOutPoint(tx *wire.MsgTx, outpoint wire.OutPoint) bool {
	for _, txIn := range tx.TxIn {
		if txIn.PreviousOutPoint == outpoint {
			return true
		}
	}

	return false
}

// virtualChannelID copies a sql row value into a fixed-size channel id.
func virtualChannelID(raw []byte) (virtualchannel.ID, error) {
	var id virtualchannel.ID
	if len(raw) != len(id) {
		return id, fmt.Errorf("invalid virtual channel id length: %d",
			len(raw))
	}

	copy(id[:], raw)

	return id, nil
}

// pendingChannelID copies a sql row value into a fixed-size lnd pending id.
func pendingChannelID(raw []byte) (virtualchannel.PendingChannelID, error) {
	var id virtualchannel.PendingChannelID
	if len(raw) != len(id) {
		return id, fmt.Errorf("invalid pending channel id length: %d",
			len(raw))
	}

	copy(id[:], raw)

	return id, nil
}

// nodePubKey copies a sql row value into a fixed-size compressed node key.
func nodePubKey(raw []byte) (virtualchannel.NodePubKey, error) {
	var key virtualchannel.NodePubKey
	if len(raw) != len(key) {
		return key, fmt.Errorf("invalid node pubkey length: %d",
			len(raw))
	}

	copy(key[:], raw)

	return key, nil
}

// outPoint converts sql row fields into a Bitcoin outpoint.
func outPoint(hashBytes []byte, index int32) (wire.OutPoint, error) {
	hash, err := chainhash.NewHash(hashBytes)
	if err != nil {
		return wire.OutPoint{}, err
	}

	return wire.OutPoint{
		Hash:  *hash,
		Index: uint32(index),
	}, nil
}

// nullUnixNano converts a nullable unix-nano timestamp to time.Time.
func nullUnixNano(ts sql.NullInt64) time.Time {
	if !ts.Valid {
		return time.Time{}
	}

	return time.Unix(0, ts.Int64).UTC()
}
