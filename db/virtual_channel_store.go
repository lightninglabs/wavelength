package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
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

	kind := normalizedVirtualChannelKind(pending.Kind)
	preBinding := pending.Status == virtualchannel.StatusRequested ||
		pending.Status == virtualchannel.StatusRoundRequested
	if preBinding && len(pending.BackingVTXOs) != 0 {
		return fmt.Errorf("unbound virtual channel intent cannot " +
			"have VTXOs")
	}
	if !preBinding && len(pending.BackingVTXOs) != 1 {
		return fmt.Errorf("bound virtual channel intent requires " +
			"exactly one VTXO")
	}
	if kind == virtualchannel.KindReceiveChannel && !preBinding &&
		pending.RoundID == "" {
		return fmt.Errorf("receive channel requires round id")
	}
	if kind == virtualchannel.KindReceiveChannel && preBinding &&
		pending.RequestKey == "" {
		return fmt.Errorf("receive channel request requires " +
			"idempotency key")
	}
	if err := virtualchannel.ValidateInitialBalances(
		kind, pending.Role, pending.Capacity, pending.LocalBalance,
		pending.RemoteBalance,
	); err != nil {
		return err
	}
	pending.Kind = kind
	if pending.StateVersion == 0 {
		pending.StateVersion = 1
	}
	existing, ok, err := s.FindVirtualChannelPendingOpen(
		ctx, pending.PendingChannelID,
	)
	if err != nil {
		return err
	}
	if ok {
		if sameVirtualChannelPendingBinding(*existing, pending) {
			return nil
		}

		return fmt.Errorf("pending channel id is already bound to a " +
			"different virtual channel")
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
				Kind:             string(kind),
				RoundID: nullableString(
					pending.RoundID,
				),
				RequestKey: nullableString(
					pending.RequestKey,
				),
				StateVersion: int64(pending.StateVersion),
			},
		)
		if err != nil {
			return err
		}

		for _, backing := range pending.BackingVTXOs {
			if err := ensureVirtualChannelBackingAvailable(
				ctx, qtx, backing.OutPoint,
			); err != nil {
				return err
			}

			backingHash := backing.OutPoint.Hash[:]
			err := qtx.InsertVirtualChannelIntentVTXO(
				ctx, sqlc.InsertVirtualChannelIntentVTXOParams{
					PendingChannelID: pending.
						PendingChannelID[:],
					OutpointHash: backingHash,
					OutpointIndex: int32(
						backing.OutPoint.Index,
					),
					AmountSat:      int64(backing.Amount),
					PkScript:       backing.PkScript,
					PolicyTemplate: backing.PolicyTemplate,
				},
			)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// MarkVirtualChannelRoundRequested records that a durable receive request is
// ready for round-FSM handoff. The write happens before dispatch so a crash or
// dispatch failure remains replayable.
func (s *VirtualChannelStoreDB) MarkVirtualChannelRoundRequested(
	ctx context.Context, pendingID virtualchannel.PendingChannelID) (bool,
	error) {

	return s.transitionVirtualChannelPendingOpen(
		ctx, pendingID, virtualchannel.StatusRoundRequested,
	)
}

// MarkVirtualChannelLNDNegotiating records that both daemons have armed their
// channel acceptors and lnd may start the funding handshake.
func (s *VirtualChannelStoreDB) MarkVirtualChannelLNDNegotiating(
	ctx context.Context, pendingID virtualchannel.PendingChannelID) (bool,
	error) {

	return s.transitionVirtualChannelPendingOpen(
		ctx, pendingID, virtualchannel.StatusLNDNegotiating,
	)
}

// MarkVirtualChannelPendingFailed terminates a negotiation before its full
// channel registration has replaced the pending intent.
func (s *VirtualChannelStoreDB) MarkVirtualChannelPendingFailed(
	ctx context.Context, pendingID virtualchannel.PendingChannelID) (bool,
	error) {

	return s.transitionVirtualChannelPendingOpen(
		ctx, pendingID, virtualchannel.StatusFailed,
	)
}

func (s *VirtualChannelStoreDB) transitionVirtualChannelPendingOpen(
	ctx context.Context, pendingID virtualchannel.PendingChannelID,
	next virtualchannel.Status) (bool, error) {

	pending, ok, err := s.FindVirtualChannelPendingOpen(ctx, pendingID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, sql.ErrNoRows
	}
	if pending.Status == next {
		return false, nil
	}
	if err := virtualchannel.ValidateTransition(
		pending.Kind, pending.Status, next,
	); err != nil {
		return false, err
	}

	now := s.clock().UTC().UnixNano()
	var rows int64
	err = s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		var err error
		rows, err = qtx.TransitionVirtualChannelIntent(
			ctx, sqlc.TransitionVirtualChannelIntentParams{
				PendingChannelID: pendingID[:],
				Status:           string(pending.Status),
				StateVersion:     int64(pending.StateVersion),
				Status_2:         string(next),
				UpdatedAt:        now,
			},
		)

		if err != nil || rows != 1 ||
			next != virtualchannel.StatusFailed {
			return err
		}

		return qtx.DeleteVirtualChannelIntentVTXOs(ctx, pendingID[:])
	})

	if err != nil || rows > 0 {
		return rows > 0, err
	}

	current, ok, err := s.FindVirtualChannelPendingOpen(ctx, pendingID)
	if err != nil {
		return false, err
	}
	if ok && current.Status == next {
		return false, nil
	}
	if !ok {
		return false, fmt.Errorf("virtual channel intent %x "+
			"disappeared while transitioning to %s", pendingID,
			next)
	}

	return false, fmt.Errorf("virtual channel intent %x changed to %s "+
		"while transitioning to %s", pendingID, current.Status, next)
}

// BindVirtualChannelPendingOpen attaches the exact round-created VTXO to a
// previously requested receive channel in one transaction.
func (s *VirtualChannelStoreDB) BindVirtualChannelPendingOpen(
	ctx context.Context, bound virtualchannel.PendingOpen) (bool, error) {

	if bound.Kind != virtualchannel.KindReceiveChannel {
		return false, fmt.Errorf("only receive channel intents bind " +
			"to rounds")
	}
	if bound.Status != virtualchannel.StatusFundingBound {
		return false, fmt.Errorf("bound receive channel must be " +
			"funding_bound")
	}
	if bound.RoundID == "" {
		return false, fmt.Errorf("receive channel requires round id")
	}
	if len(bound.BackingVTXOs) != 1 {
		return false, fmt.Errorf("receive channel requires exactly " +
			"one VTXO")
	}

	existing, ok, err := s.FindVirtualChannelPendingOpen(
		ctx, bound.PendingChannelID,
	)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, sql.ErrNoRows
	}
	if existing.Status == virtualchannel.StatusFundingBound ||
		existing.Status == virtualchannel.StatusLNDNegotiating {

		if sameVirtualChannelPendingBinding(*existing, bound) {
			return false, nil
		}

		return false, fmt.Errorf("receive channel is bound to " +
			"another round VTXO")
	}
	if err := virtualchannel.ValidateTransition(
		existing.Kind, existing.Status,
		virtualchannel.StatusFundingBound,
	); err != nil {
		return false, err
	}
	if existing.Kind != bound.Kind ||
		existing.RequestKey != bound.RequestKey ||
		existing.PendingChannelID != bound.PendingChannelID ||
		existing.RemoteNodePubKey != bound.RemoteNodePubKey ||
		existing.Role != bound.Role ||
		existing.Capacity != bound.Capacity ||
		existing.LocalBalance != bound.LocalBalance ||
		existing.RemoteBalance != bound.RemoteBalance {
		return false, fmt.Errorf("round VTXO does not match receive " +
			"channel request")
	}

	now := s.clock().UTC().UnixNano()
	var rows int64
	err = s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		var err error
		rows, err = qtx.BindVirtualChannelIntent(
			ctx, sqlc.BindVirtualChannelIntentParams{
				PendingChannelID: bound.PendingChannelID[:],
				StateVersion:     int64(existing.StateVersion),
				Kind:             string(bound.Kind),
				RoundID:          nullableString(bound.RoundID),
				UpdatedAt:        now,
			},
		)
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("receive channel intent changed " +
				"while binding")
		}

		backing := bound.BackingVTXOs[0]
		if err := ensureVirtualChannelBackingAvailable(
			ctx, qtx, backing.OutPoint,
		); err != nil {
			return err
		}

		return qtx.InsertVirtualChannelIntentVTXO(
			ctx, sqlc.InsertVirtualChannelIntentVTXOParams{
				PendingChannelID: bound.PendingChannelID[:],
				OutpointHash:     backing.OutPoint.Hash[:],
				OutpointIndex:    int32(backing.OutPoint.Index),
				AmountSat:        int64(backing.Amount),
				PkScript:         backing.PkScript,
				PolicyTemplate:   backing.PolicyTemplate,
			},
		)
	})

	return rows > 0, err
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

	if len(reg.BackingVTXOs) != 1 {
		return fmt.Errorf("virtual channel requires exactly one " +
			"backing VTXO")
	}
	kind := normalizedVirtualChannelKind(reg.Kind)
	if kind == virtualchannel.KindReceiveChannel && reg.RoundID == "" {
		return fmt.Errorf("receive channel requires round id")
	}
	if err := virtualchannel.ValidateInitialBalances(
		kind, reg.Role, reg.Capacity, reg.LocalBalance,
		reg.RemoteBalance,
	); err != nil {
		return err
	}
	reg.Kind = kind
	existing, err := s.GetVirtualChannel(ctx, reg.ID)
	if err == nil {
		if sameVirtualChannelRegistrationBinding(
			existing.Registration, reg,
		) {
			return nil
		}

		return fmt.Errorf("virtual channel id is already bound to a " +
			"different registration")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	stateVersion := reg.StateVersion
	pending, pendingFound, err := s.FindVirtualChannelPendingOpen(
		ctx, reg.PendingChannelID,
	)
	if err != nil {
		return err
	}
	if pendingFound {
		if !registrationMatchesPending(*pending, reg) {
			return fmt.Errorf("virtual channel registration does " +
				"not match its durable intent")
		}
		if pending.Status == reg.Status {
			stateVersion = pending.StateVersion
		} else {
			if err := virtualchannel.ValidateTransition(
				pending.Kind, pending.Status, reg.Status,
			); err != nil {
				return err
			}
			stateVersion = pending.StateVersion + 1
		}
	} else if stateVersion == 0 {
		stateVersion = 1
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
				Kind:             string(kind),
				RoundID:          nullableString(reg.RoundID),
				StateVersion:     int64(stateVersion),
			},
		)
		if err != nil {
			return err
		}

		if pendingFound {
			pendingChannelID := reg.PendingChannelID[:]
			pendingStatus := string(pending.Status)
			rows, err := qtx.DeleteVirtualChannelIntentCAS(
				ctx, sqlc.DeleteVirtualChannelIntentCASParams{
					PendingChannelID: pendingChannelID,
					Status:           pendingStatus,
					StateVersion: int64(
						pending.StateVersion,
					),
				},
			)
			if err != nil {
				return err
			}
			if rows != 1 {
				return fmt.Errorf("virtual channel intent " +
					"changed while registering")
			}
		}

		for _, backing := range reg.BackingVTXOs {
			if err := ensureVirtualChannelBackingAvailable(
				ctx, qtx, backing.OutPoint,
			); err != nil {
				return err
			}

			backingHash := backing.OutPoint.Hash[:]
			err := qtx.InsertVirtualChannelVTXO(
				ctx, sqlc.InsertVirtualChannelVTXOParams{
					VirtualChannelID: reg.ID[:],
					OutpointHash:     backingHash,
					OutpointIndex: int32(
						backing.OutPoint.Index,
					),
					AmountSat:      int64(backing.Amount),
					PkScript:       backing.PkScript,
					PolicyTemplate: backing.PolicyTemplate,
				},
			)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// GetVirtualChannel loads a virtual channel by its stable Wavelength id.
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

// FindVirtualChannelByBackingVTXO loads the channel that owns the given VTXO
// as its exact funding parent input.
func (s *VirtualChannelStoreDB) FindVirtualChannelByBackingVTXO(
	ctx context.Context, outpoint wire.OutPoint) (*virtualchannel.Channel,
	bool, error) {

	var channel *virtualchannel.Channel
	err := s.ExecTx(ctx, ReadTxOption(), func(qtx *sqlc.Queries) error {
		row, err := qtx.GetVirtualChannelByBackingVTXO(
			ctx, sqlc.GetVirtualChannelByBackingVTXOParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
			},
		)
		if err != nil {
			return err
		}

		channel, err = s.loadChannel(ctx, qtx, row)

		return err
	})
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

// ListVirtualChannelPendingOpensByStatus loads durable pre-channel requests in
// update order for startup recovery.
func (s *VirtualChannelStoreDB) ListVirtualChannelPendingOpensByStatus(
	ctx context.Context, status virtualchannel.Status) (
	[]*virtualchannel.PendingOpen, error) {

	var pending []*virtualchannel.PendingOpen
	err := s.ExecTx(ctx, ReadTxOption(), func(qtx *sqlc.Queries) error {
		rows, err := qtx.ListVirtualChannelIntentsByStatus(
			ctx, string(status),
		)
		if err != nil {
			return err
		}

		pending = make([]*virtualchannel.PendingOpen, 0, len(rows))
		for _, row := range rows {
			intent, err := s.loadPendingOpen(ctx, qtx, row)
			if err != nil {
				return err
			}
			pending = append(pending, intent)
		}

		return nil
	})

	return pending, err
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

// ArmVirtualChannelBacking persists a fully verified VTXO-to-channel parent.
// Receive rounds gate final signatures on this state, not on channel activity.
func (s *VirtualChannelStoreDB) ArmVirtualChannelBacking(ctx context.Context,
	id virtualchannel.ID, backingTx *wire.MsgTx) (bool, error) {

	channel, err := s.GetVirtualChannel(ctx, id)
	if err != nil {
		return false, err
	}
	if virtualChannelHasArmedBacking(channel.Status) {
		if err := exactVirtualChannelBacking(
			channel, backingTx,
		); err != nil {
			return false, err
		}

		return false, nil
	}
	if err := virtualchannel.ValidateTransition(
		channel.Kind, channel.Status, virtualchannel.StatusBackingArmed,
	); err != nil {
		return false, err
	}
	if err := virtualchannel.ValidateBackingProof(
		channel.Registration, backingTx,
	); err != nil {
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
		rows, err = qtx.ArmVirtualChannelBacking(
			ctx, sqlc.ArmVirtualChannelBackingParams{
				VirtualChannelID: id[:],
				StateVersion:     int64(channel.StateVersion),
				ChannelPointHash: txid[:],
				BackingTx:        backingTxBytes,
				UpdatedAt:        now,
			},
		)

		return err
	})

	if err != nil || rows > 0 {
		return rows > 0, err
	}

	// Another worker may have armed the intent after our read. Accept only
	// an exact replay of the durable signed transaction.
	channel, err = s.GetVirtualChannel(ctx, id)
	if err != nil {
		return false, err
	}
	if !virtualChannelHasArmedBacking(channel.Status) {
		return false, fmt.Errorf("virtual channel %x changed to %s "+
			"while arming", id, channel.Status)
	}

	return false, exactVirtualChannelBacking(channel, backingTx)
}

// ActivateVirtualChannel advances an armed promoted VTXO, or a confirmed
// receive channel, to the routable state.
func (s *VirtualChannelStoreDB) ActivateVirtualChannel(ctx context.Context,
	id virtualchannel.ID) (bool, error) {

	return s.transitionVirtualChannel(
		ctx, id, virtualchannel.StatusActive,
	)
}

// MarkVirtualChannelFundingVerified records that lnd's activation hook observed
// the exact channel point in its durable pending-channel state.
func (s *VirtualChannelStoreDB) MarkVirtualChannelFundingVerified(
	ctx context.Context, id virtualchannel.ID) (bool, error) {

	return s.transitionVirtualChannel(
		ctx, id, virtualchannel.StatusFundingVerified,
	)
}

// transitionVirtualChannel applies a validated status-only FSM edge with a
// compare-and-swap on both the current status and state version.
func (s *VirtualChannelStoreDB) transitionVirtualChannel(ctx context.Context,
	id virtualchannel.ID, next virtualchannel.Status) (bool, error) {

	channel, err := s.GetVirtualChannel(ctx, id)
	if err != nil {
		return false, err
	}
	if channel.Status == next {
		return false, nil
	}
	if err := virtualchannel.ValidateTransition(
		channel.Kind, channel.Status, next,
	); err != nil {
		return false, err
	}

	now := s.clock().UTC().UnixNano()
	var rows int64
	err = s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		var err error
		rows, err = qtx.TransitionVirtualChannel(
			ctx, sqlc.TransitionVirtualChannelParams{
				VirtualChannelID: id[:],
				Status:           string(channel.Status),
				StateVersion:     int64(channel.StateVersion),
				Status_2:         string(next),
				UpdatedAt:        now,
			},
		)

		if err != nil || rows != 1 ||
			next != virtualchannel.StatusFailed ||
			virtualChannelHasArmedBacking(channel.Status) {
			return err
		}

		return qtx.DeleteVirtualChannelVTXOs(ctx, id[:])
	})

	if err != nil || rows > 0 {
		return rows > 0, err
	}

	current, err := s.GetVirtualChannel(ctx, id)
	if err != nil {
		return false, err
	}
	if current.Status == next {
		return false, nil
	}

	return false, fmt.Errorf("virtual channel %x changed to %s while "+
		"transitioning to %s", id, current.Status, next)
}

// MarkRoundVirtualChannelsConfirmed records the chain-confirmation gate without
// making the channels routable yet.
func (s *VirtualChannelStoreDB) MarkRoundVirtualChannelsConfirmed(
	ctx context.Context, roundID string) (int64, error) {

	if roundID == "" {
		return 0, fmt.Errorf("round id is required")
	}

	now := s.clock().UTC().UnixNano()
	var changed int64
	err := s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		var err error
		changed, err = qtx.ConfirmRoundVirtualChannels(
			ctx, sqlc.ConfirmRoundVirtualChannelsParams{
				RoundID:   nullableString(roundID),
				UpdatedAt: now,
			},
		)

		return err
	})

	return changed, err
}

// ActivateConfirmedRoundVirtualChannels exposes the confirmed channels for a
// single round. It is safe to replay after a crash.
func (s *VirtualChannelStoreDB) ActivateConfirmedRoundVirtualChannels(
	ctx context.Context, roundID string) (int64, error) {

	if roundID == "" {
		return 0, fmt.Errorf("round id is required")
	}

	now := s.clock().UTC().UnixNano()
	var changed int64
	err := s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		var err error
		changed, err = qtx.ActivateConfirmedRoundVirtualChannels(
			ctx, sqlc.ActivateConfirmedRoundVirtualChannelsParams{
				RoundID:   nullableString(roundID),
				UpdatedAt: now,
			},
		)

		return err
	})

	return changed, err
}

// RecoverConfirmedVirtualChannels completes confirmation transitions that
// were interrupted between their two durable writes.
func (s *VirtualChannelStoreDB) RecoverConfirmedVirtualChannels(
	ctx context.Context) (int64, error) {

	now := s.clock().UTC().UnixNano()
	var changed int64
	err := s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		var err error
		changed, err = qtx.ActivateAllConfirmedVirtualChannels(ctx, now)

		return err
	})

	return changed, err
}

// FailRoundVirtualChannels terminates every pre-activation receive channel
// bound to a round that can no longer create its backing VTXO.
func (s *VirtualChannelStoreDB) FailRoundVirtualChannels(ctx context.Context,
	roundID string) (int64, error) {

	if roundID == "" {
		return 0, fmt.Errorf("round id is required")
	}

	now := s.clock().UTC().UnixNano()
	var changed int64
	err := s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		channels, err := qtx.FailRoundVirtualChannels(
			ctx, sqlc.FailRoundVirtualChannelsParams{
				RoundID:   nullableString(roundID),
				UpdatedAt: now,
			},
		)
		if err != nil {
			return err
		}
		armed, err := qtx.CloseFailedRoundArmedVirtualChannels(
			ctx, sqlc.CloseFailedRoundArmedVirtualChannelsParams{
				RoundID:   nullableString(roundID),
				UpdatedAt: now,
			},
		)
		if err != nil {
			return err
		}
		intents, err := qtx.FailRoundVirtualChannelIntents(
			ctx, sqlc.FailRoundVirtualChannelIntentsParams{
				RoundID:   nullableString(roundID),
				UpdatedAt: now,
			},
		)
		if err != nil {
			return err
		}
		changed = channels + armed + intents
		round := nullableString(roundID)
		if err := qtx.ReleaseFailedRoundVirtualChannelVTXOs(
			ctx, round,
		); err != nil {
			return err
		}

		return qtx.ReleaseFailedRoundVirtualChannelIntentVTXOs(
			ctx, round,
		)
	})

	return changed, err
}

// RecoverRoundVirtualChannels reconciles pre-activation receive channels
// against the durable client round table. A missing round is terminal because
// the client persists the round before releasing its final signatures.
func (s *VirtualChannelStoreDB) RecoverRoundVirtualChannels(
	ctx context.Context) (int64, error) {

	var changed int64
	seen := make(map[string]struct{})
	for _, status := range []virtualchannel.Status{
		virtualchannel.StatusFundingBound,
		virtualchannel.StatusLNDNegotiating,
	} {
		intents, err := s.ListVirtualChannelPendingOpensByStatus(
			ctx, status,
		)
		if err != nil {
			return changed, err
		}
		for _, intent := range intents {
			if intent.Kind != virtualchannel.KindReceiveChannel ||
				intent.RoundID == "" {

				continue
			}
			if _, ok := seen[intent.RoundID]; ok {
				continue
			}
			seen[intent.RoundID] = struct{}{}

			state, err := s.virtualChannelRoundState(
				ctx, intent.RoundID,
			)
			if err != nil {
				return changed, err
			}
			switch state {
			case virtualChannelRoundPending:
				continue

			case virtualChannelRoundConfirmed:
				return changed, fmt.Errorf("confirmed round "+
					"%s has pending virtual channel %x in "+
					"unsafe state %s", intent.RoundID,
					intent.PendingChannelID, intent.Status)

			case virtualChannelRoundFailed:
				count, err := s.FailRoundVirtualChannels(
					ctx, intent.RoundID,
				)
				if err != nil {
					return changed, err
				}
				changed += count
			}
		}
	}

	var candidates []*virtualchannel.Channel
	for _, status := range []virtualchannel.Status{
		virtualchannel.StatusFundingBound,
		virtualchannel.StatusLNDNegotiating,
		virtualchannel.StatusFundingVerified,
		virtualchannel.StatusBackingArmed,
		virtualchannel.StatusRoundConfirmed,
	} {
		channels, err := s.ListVirtualChannelsByStatus(ctx, status)
		if err != nil {
			return 0, err
		}
		candidates = append(candidates, channels...)
	}

	for _, channel := range candidates {
		if channel.Kind != virtualchannel.KindReceiveChannel ||
			channel.RoundID == "" {

			continue
		}
		if _, ok := seen[channel.RoundID]; ok {
			continue
		}
		seen[channel.RoundID] = struct{}{}

		state, err := s.virtualChannelRoundState(ctx, channel.RoundID)
		if err != nil {
			return changed, err
		}
		switch state {
		case virtualChannelRoundPending:
			if channel.Status ==
				virtualchannel.StatusRoundConfirmed {
				return changed, fmt.Errorf("virtual channel "+
					"%x is round_confirmed while round %s "+
					"is pending", channel.ID,
					channel.RoundID)
			}

			continue

		case virtualChannelRoundConfirmed:
			backingArmed := channel.Status ==
				virtualchannel.StatusBackingArmed
			if !backingArmed &&
				channel.Status !=
					virtualchannel.StatusRoundConfirmed {
				return changed, fmt.Errorf("confirmed round "+
					"%s has virtual channel %x in unsafe "+
					"state %s", channel.RoundID, channel.ID,
					channel.Status)
			}
			count, err := s.ConfirmRoundVirtualChannels(
				ctx, channel.RoundID,
			)
			if err != nil {
				return changed, err
			}
			changed += count

		case virtualChannelRoundFailed:
			count, err := s.FailRoundVirtualChannels(
				ctx, channel.RoundID,
			)
			if err != nil {
				return changed, err
			}
			changed += count
		}
	}

	return changed, nil
}

type virtualChannelRoundState uint8

const (
	virtualChannelRoundPending virtualChannelRoundState = iota
	virtualChannelRoundConfirmed
	virtualChannelRoundFailed
)

func (s *VirtualChannelStoreDB) virtualChannelRoundState(ctx context.Context,
	roundID string) (virtualChannelRoundState, error) {

	var state virtualChannelRoundState
	err := s.ExecTx(ctx, ReadTxOption(), func(qtx *sqlc.Queries) error {
		row, err := qtx.GetRound(ctx, roundID)
		if errors.Is(err, sql.ErrNoRows) {
			state = virtualChannelRoundFailed

			return nil
		}
		if err != nil {
			return err
		}
		switch row.Status {
		case "input_sig_sent":
			state = virtualChannelRoundPending

		case "confirmed":
			state = virtualChannelRoundConfirmed

		case "failed", "archived":
			state = virtualChannelRoundFailed

		default:
			return fmt.Errorf("unknown round %s status %q", roundID,
				row.Status)
		}

		return nil
	})

	return state, err
}

// ConfirmRoundVirtualChannels advances every armed receive channel for a
// confirmed round and then exposes it as active using two replayable writes.
func (s *VirtualChannelStoreDB) ConfirmRoundVirtualChannels(ctx context.Context,
	roundID string) (int64, error) {

	confirmed, err := s.MarkRoundVirtualChannelsConfirmed(ctx, roundID)
	if err != nil {
		return 0, err
	}
	active, err := s.ActivateConfirmedRoundVirtualChannels(ctx, roundID)

	return confirmed + active, err
}

// MarkVirtualChannelMaterializing records that the backing parent is being
// published for a conflict or force-close path.
func (s *VirtualChannelStoreDB) MarkVirtualChannelMaterializing(
	ctx context.Context, id virtualchannel.ID) (bool, error) {

	channel, err := s.GetVirtualChannel(ctx, id)
	if err != nil {
		return false, err
	}
	if channel.Status == virtualchannel.StatusMaterializing {
		return false, nil
	}
	if err := virtualchannel.ValidateTransition(
		channel.Kind, channel.Status,
		virtualchannel.StatusMaterializing,
	); err != nil {
		return false, err
	}

	now := s.clock().UTC().UnixNano()
	var rows int64
	err = s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		var err error
		rows, err = qtx.MarkVirtualChannelMaterializing(
			ctx, sqlc.MarkVirtualChannelMaterializingParams{
				VirtualChannelID: id[:],
				Status:           string(channel.Status),
				StateVersion:     int64(channel.StateVersion),
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

	return s.transitionVirtualChannel(
		ctx, id, virtualchannel.StatusClosing,
	)
}

// MarkVirtualChannelClosed records that close resolution reached a terminal
// state.
func (s *VirtualChannelStoreDB) MarkVirtualChannelClosed(ctx context.Context,
	id virtualchannel.ID) (bool, error) {

	channel, err := s.GetVirtualChannel(ctx, id)
	if err != nil {
		return false, err
	}
	if channel.Status == virtualchannel.StatusClosed {
		return false, nil
	}
	if err := virtualchannel.ValidateTransition(
		channel.Kind, channel.Status, virtualchannel.StatusClosed,
	); err != nil {
		return false, err
	}

	now := s.clock().UTC().UnixNano()
	var rows int64
	err = s.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		var err error
		rows, err = qtx.MarkVirtualChannelClosed(
			ctx, sqlc.MarkVirtualChannelClosedParams{
				VirtualChannelID: id[:],
				Status:           string(channel.Status),
				StateVersion:     int64(channel.StateVersion),
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

	return s.transitionVirtualChannel(
		ctx, id, virtualchannel.StatusFailed,
	)
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
				PkScript: bytes.Clone(
					vtxoRow.PkScript,
				),
				PolicyTemplate: bytes.Clone(
					vtxoRow.PolicyTemplate,
				),
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
			ID: id,
			Kind: normalizedVirtualChannelKind(
				virtualchannel.IntentKind(row.Kind),
			),
			RoundID:          row.RoundID.String,
			StateVersion:     uint64(row.StateVersion),
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
				PkScript: bytes.Clone(
					vtxoRow.PkScript,
				),
				PolicyTemplate: bytes.Clone(
					vtxoRow.PolicyTemplate,
				),
			},
		)
	}

	return &virtualchannel.PendingOpen{
		Kind: normalizedVirtualChannelKind(
			virtualchannel.IntentKind(row.Kind),
		),
		RequestKey:       row.RequestKey.String,
		RoundID:          row.RoundID.String,
		StateVersion:     uint64(row.StateVersion),
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
		Kind:             normalizedVirtualChannelKind(reg.Kind),
		RoundID:          reg.RoundID,
		StateVersion:     reg.StateVersion,
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

func normalizedVirtualChannelKind(
	kind virtualchannel.IntentKind) virtualchannel.IntentKind {

	if kind == "" {
		return virtualchannel.KindPromoteVTXO
	}

	return kind
}

func nullableString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func sameVirtualChannelPendingBinding(a, b virtualchannel.PendingOpen) bool {
	return normalizedVirtualChannelKind(a.Kind) ==
		normalizedVirtualChannelKind(b.Kind) &&
		a.RequestKey == b.RequestKey &&
		a.RoundID == b.RoundID &&
		a.PendingChannelID == b.PendingChannelID &&
		a.RemoteNodePubKey == b.RemoteNodePubKey &&
		a.Role == b.Role && a.Capacity == b.Capacity &&
		a.LocalBalance == b.LocalBalance &&
		a.RemoteBalance == b.RemoteBalance &&
		sameVirtualChannelBackingVTXOs(a.BackingVTXOs, b.BackingVTXOs)
}

func sameVirtualChannelRegistrationBinding(
	a, b virtualchannel.Registration) bool {

	return a.ID == b.ID &&
		sameVirtualChannelPendingBinding(
			*pendingOpenFromRegistration(a),
			*pendingOpenFromRegistration(b),
		) &&
		a.ChannelPoint == b.ChannelPoint &&
		a.BackingTx != nil && b.BackingTx != nil &&
		a.BackingTx.TxHash() == b.BackingTx.TxHash() &&
		bytes.Equal(a.FundingPsbt, b.FundingPsbt)
}

func registrationMatchesPending(pending virtualchannel.PendingOpen,
	reg virtualchannel.Registration) bool {

	return normalizedVirtualChannelKind(pending.Kind) ==
		normalizedVirtualChannelKind(reg.Kind) &&
		pending.RoundID == reg.RoundID &&
		pending.PendingChannelID == reg.PendingChannelID &&
		pending.RemoteNodePubKey == reg.RemoteNodePubKey &&
		pending.Role == reg.Role && pending.Capacity == reg.Capacity &&
		pending.LocalBalance == reg.LocalBalance &&
		pending.RemoteBalance == reg.RemoteBalance &&
		sameVirtualChannelBackingVTXOs(
			pending.BackingVTXOs, reg.BackingVTXOs,
		)
}

func sameVirtualChannelBackingVTXOs(a, b []virtualchannel.BackingVTXO) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].OutPoint != b[i].OutPoint ||
			a[i].Amount != b[i].Amount ||
			!bytes.Equal(a[i].PkScript, b[i].PkScript) ||
			!bytes.Equal(a[i].PolicyTemplate, b[i].PolicyTemplate) {
			return false
		}
	}

	return true
}

func ensureVirtualChannelBackingAvailable(ctx context.Context,
	qtx *sqlc.Queries, outpoint wire.OutPoint) error {

	owners, err := qtx.CountVirtualChannelBackingOwners(
		ctx, sqlc.CountVirtualChannelBackingOwnersParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
		},
	)
	if err != nil {
		return err
	}
	if owners != 0 {
		return fmt.Errorf("VTXO %v already backs another "+
			"virtual channel", outpoint)
	}

	return nil
}

// encodeMsgTx serializes a Bitcoin transaction using wire encoding.
func encodeMsgTx(tx *wire.MsgTx) ([]byte, error) {
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func virtualChannelHasArmedBacking(status virtualchannel.Status) bool {
	return virtualchannel.HasArmedBacking(status)
}

func exactVirtualChannelBacking(channel *virtualchannel.Channel,
	backingTx *wire.MsgTx) error {

	if err := virtualchannel.ValidateBackingProof(
		channel.Registration, backingTx,
	); err != nil {
		return err
	}
	stored, err := encodeMsgTx(channel.BackingTx)
	if err != nil {
		return fmt.Errorf("encode stored backing transaction: %w", err)
	}
	replayed, err := encodeMsgTx(backingTx)
	if err != nil {
		return fmt.Errorf("encode replayed backing transaction: %w",
			err)
	}
	if !bytes.Equal(stored, replayed) {
		return fmt.Errorf("replayed backing transaction differs from " +
			"durable proof")
	}

	return nil
}

// decodeMsgTx deserializes a Bitcoin transaction using wire encoding.
func decodeMsgTx(raw []byte) (*wire.MsgTx, error) {
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}

	return tx, nil
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
