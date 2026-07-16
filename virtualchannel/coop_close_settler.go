package virtualchannel

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// CooperativeCloseStore persists virtual cooperative close settlement.
type CooperativeCloseStore interface {
	// MarkVirtualChannelCoopClosed records a cooperative close that was
	// settled virtually instead of publishing the backing parent.
	MarkVirtualChannelCoopClosed(ctx context.Context, id ID,
		closeTx *wire.MsgTx,
		localBalance, remoteBalance btcutil.Amount) (bool, error)
}

// CloseBalanceResolver derives final virtual balances from lnd's negotiated
// cooperative close transaction.
type CloseBalanceResolver interface {
	// ResolveCooperativeClose returns final balances and whether closeTx is
	// a virtual cooperative close this resolver can handle.
	ResolveCooperativeClose(ctx context.Context, channel *Channel,
		closeTx *wire.MsgTx) (btcutil.Amount, btcutil.Amount, bool,
		error)
}

// DurableCooperativeCloseSettlerConfig configures a durable close settler.
type DurableCooperativeCloseSettlerConfig struct {
	Store           CooperativeCloseStore
	BalanceResolver CloseBalanceResolver
}

// DurableCooperativeCloseSettler persists virtual cooperative closes.
type DurableCooperativeCloseSettler struct {
	store    CooperativeCloseStore
	resolver CloseBalanceResolver
}

// NewDurableCooperativeCloseSettler creates a close settler backed by durable
// virtual channel state.
func NewDurableCooperativeCloseSettler(
	cfg DurableCooperativeCloseSettlerConfig) (
	*DurableCooperativeCloseSettler, error) {

	if cfg.Store == nil {
		return nil, fmt.Errorf("virtual channel close store is nil")
	}
	if cfg.BalanceResolver == nil {
		return nil, fmt.Errorf("virtual channel close balance " +
			"resolver is nil")
	}

	return &DurableCooperativeCloseSettler{
		store:    cfg.Store,
		resolver: cfg.BalanceResolver,
	}, nil
}

// SettleCooperativeClose implements CooperativeCloseSettler.
func (s *DurableCooperativeCloseSettler) SettleCooperativeClose(
	ctx context.Context, channel *Channel, closeTx *wire.MsgTx, _ string) (
	bool, error) {

	localBalance, remoteBalance, handled, err :=
		s.resolver.ResolveCooperativeClose(ctx, channel, closeTx)
	if err != nil {
		return false, err
	}
	if !handled {
		return false, nil
	}

	changed, err := s.store.MarkVirtualChannelCoopClosed(
		ctx, channel.ID, closeTx, localBalance, remoteBalance,
	)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, fmt.Errorf("virtual channel %x cooperative "+
			"close was not persisted", channel.ID)
	}

	return true, nil
}

var _ CooperativeCloseSettler = (*DurableCooperativeCloseSettler)(nil)
