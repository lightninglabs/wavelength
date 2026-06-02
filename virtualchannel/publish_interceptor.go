package virtualchannel

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"
)

const (
	// MaterializedBackingLabel is the wallet label used when the
	// interceptor publishes the virtual funding parent before an lnd child
	// spend.
	MaterializedBackingLabel = "darepo virtual channel backing"
)

// ChannelPointLookup resolves lnd funding outpoints to persisted virtual
// channel registrations.
type ChannelPointLookup interface {
	// FindVirtualChannelByChannelPoint returns a virtual channel by lnd
	// funding outpoint and reports whether one exists.
	FindVirtualChannelByChannelPoint(ctx context.Context,
		channelPoint wire.OutPoint) (*Channel, bool, error)

	// ListVirtualChannelsByFundingTxID returns virtual channels whose lnd
	// channel point is an output of the given funding transaction id.
	ListVirtualChannelsByFundingTxID(ctx context.Context,
		txid chainhash.Hash) ([]*Channel, error)
}

// MaterializationStore persists virtual channel materialization state.
type MaterializationStore interface {
	ActiveChannelStore
	ChannelPointLookup
	PendingChannelLookup

	// MarkVirtualChannelMaterializing records that the backing parent is
	// being published for a conflict or force-close path.
	MarkVirtualChannelMaterializing(ctx context.Context,
		id ID) (bool, error)
}

// TxBroadcaster publishes Bitcoin transactions to the network.
type TxBroadcaster interface {
	// PublishTransaction publishes tx with the given wallet label.
	PublishTransaction(ctx context.Context, tx *wire.MsgTx,
		label string) error
}

// BackingMaterializer materializes the VTXO ancestry and final cooperative
// backing spend needed before lnd publishes a child transaction.
type BackingMaterializer interface {
	// MaterializeVirtualChannelBacking blocks until the channel's
	// cooperative backing transaction is publishable by lnd's child spend.
	MaterializeVirtualChannelBacking(ctx context.Context,
		channel *Channel) error
}

// CooperativeCloseSettler settles a virtual channel close without publishing
// the backing parent when both sides have agreed to a cooperative close.
type CooperativeCloseSettler interface {
	// SettleCooperativeClose attempts to settle closeTx virtually. It
	// returns true if the close was handled and lnd's default publish path
	// should be suppressed.
	SettleCooperativeClose(ctx context.Context, channel *Channel,
		closeTx *wire.MsgTx, label string) (bool, error)
}

// MaterializingPublishInterceptor publishes virtual channel backing parents
// before allowing lnd to publish a dependent child spend.
type MaterializingPublishInterceptor struct {
	store        MaterializationStore
	broadcaster  TxBroadcaster
	materializer BackingMaterializer
	closeSettler CooperativeCloseSettler
	ctx          func() context.Context
}

// MaterializingPublishInterceptorConfig configures a publish interceptor.
type MaterializingPublishInterceptorConfig struct {
	Store        MaterializationStore
	Broadcaster  TxBroadcaster
	Materializer BackingMaterializer
	CloseSettler CooperativeCloseSettler
	Context      func() context.Context
}

// NewMaterializingPublishInterceptor creates a new lnd publish interceptor
// backed by durable virtual channel state.
func NewMaterializingPublishInterceptor(
	cfg MaterializingPublishInterceptorConfig) (
	*MaterializingPublishInterceptor, error) {

	if cfg.Store == nil {
		return nil, fmt.Errorf("virtual channel store is nil")
	}
	if cfg.Broadcaster == nil && cfg.Materializer == nil {
		return nil, fmt.Errorf("virtual channel materializer or " +
			"broadcaster is required")
	}
	if cfg.Context == nil {
		cfg.Context = context.Background
	}

	return &MaterializingPublishInterceptor{
		store:        cfg.Store,
		broadcaster:  cfg.Broadcaster,
		materializer: cfg.Materializer,
		closeSettler: cfg.CloseSettler,
		ctx:          cfg.Context,
	}, nil
}

// PublishTransaction implements lnwallet.PublishInterceptor.
func (i *MaterializingPublishInterceptor) PublishTransaction(tx *wire.MsgTx,
	label string, publish func() error) error {

	ctx := i.ctx()
	suppress, err := i.suppressVirtualFundingPublish(ctx, tx)
	if err != nil {
		return err
	}
	if suppress {
		return nil
	}

	settled, err := i.settleCooperativeClose(ctx, tx, label)
	if err != nil {
		return err
	}
	if settled {
		return nil
	}

	if err := i.materializeParents(ctx, tx); err != nil {
		return err
	}

	return publish()
}

// suppressVirtualFundingPublish prevents lnd's zero-conf funding rebroadcast
// path from unrolling a registered virtual channel during the happy path.
func (i *MaterializingPublishInterceptor) suppressVirtualFundingPublish(
	ctx context.Context, tx *wire.MsgTx) (bool, error) {

	channels, err := i.store.ListVirtualChannelsByFundingTxID(
		ctx, tx.TxHash(),
	)
	if err != nil {
		return false, fmt.Errorf("lookup virtual funding tx %v: %w",
			tx.TxHash(), err)
	}

	for _, channel := range channels {
		if shouldSuppressFundingPublish(channel.Status) {
			return true, nil
		}
	}

	return false, nil
}

// settleCooperativeClose gives darepo a chance to settle an lnd cooperative
// close virtually before conflict materialization publishes the backing parent.
func (i *MaterializingPublishInterceptor) settleCooperativeClose(
	ctx context.Context, tx *wire.MsgTx, label string) (bool, error) {

	if i.closeSettler == nil {
		return false, nil
	}

	seen := make(map[ID]struct{})
	for _, txIn := range tx.TxIn {
		channel, ok, err := i.store.FindVirtualChannelByChannelPoint(
			ctx, txIn.PreviousOutPoint,
		)
		if err != nil {
			return false, fmt.Errorf("lookup virtual channel "+
				"%v: %w", txIn.PreviousOutPoint, err)
		}
		if !ok || !shouldSettleCooperativeClose(channel.Status) {
			continue
		}

		if _, ok := seen[channel.ID]; ok {
			continue
		}
		seen[channel.ID] = struct{}{}

		settled, err := i.closeSettler.SettleCooperativeClose(
			ctx, channel, tx, label,
		)
		if err != nil {
			return false, fmt.Errorf("settle virtual channel %x "+
				"cooperative close: %w", channel.ID, err)
		}
		if settled {
			return true, nil
		}
	}

	return false, nil
}

// materializeParents publishes every virtual funding parent that the lnd
// transaction spends before the original lnd publish callback runs.
func (i *MaterializingPublishInterceptor) materializeParents(
	ctx context.Context, tx *wire.MsgTx) error {

	seen := make(map[ID]struct{})
	for _, txIn := range tx.TxIn {
		channel, ok, err := i.store.FindVirtualChannelByChannelPoint(
			ctx, txIn.PreviousOutPoint,
		)
		if err != nil {
			return fmt.Errorf("lookup virtual channel %v: %w",
				txIn.PreviousOutPoint, err)
		}
		if !ok || !shouldMaterialize(channel.Status) {
			continue
		}

		if _, ok := seen[channel.ID]; ok {
			continue
		}
		seen[channel.ID] = struct{}{}

		_, err = i.store.MarkVirtualChannelMaterializing(
			ctx, channel.ID,
		)
		if err != nil {
			return fmt.Errorf("mark virtual channel %x "+
				"materializing: %w", channel.ID, err)
		}

		if i.materializer != nil {
			err = i.materializer.MaterializeVirtualChannelBacking(
				ctx, channel,
			)
			if err != nil {
				return fmt.Errorf("materialize virtual "+
					"channel %x backing: %w", channel.ID,
					err)
			}

			continue
		}

		if i.broadcaster == nil {
			return fmt.Errorf("virtual channel materializer is nil")
		}

		err = i.broadcaster.PublishTransaction(
			ctx, channel.BackingTx, MaterializedBackingLabel,
		)
		if err != nil {
			return fmt.Errorf("publish virtual channel %x "+
				"backing tx: %w", channel.ID, err)
		}
	}

	return nil
}

// shouldMaterialize reports whether a dependent lnd publish should materialize
// the virtual funding parent.
func shouldMaterialize(status Status) bool {
	switch status {
	case StatusActive, StatusClosing, StatusMaterializing:
		return true

	default:
		return false
	}
}

// shouldSettleCooperativeClose reports whether a publish attempt may represent
// a virtual cooperative close that should stay off chain.
func shouldSettleCooperativeClose(status Status) bool {
	switch status {
	case StatusActive, StatusClosing:
		return true

	default:
		return false
	}
}

// shouldSuppressFundingPublish reports whether lnd's direct funding
// transaction publish should stay virtual.
func shouldSuppressFundingPublish(status Status) bool {
	switch status {
	case StatusNegotiating, StatusActive, StatusClosing:
		return true

	default:
		return false
	}
}

// Compile-time check that the interceptor satisfies lnd's hook interface.
var _ lnwallet.PublishInterceptor = (*MaterializingPublishInterceptor)(nil)
