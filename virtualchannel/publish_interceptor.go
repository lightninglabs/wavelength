package virtualchannel

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/lnwallet"
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
	ChannelStatusLookup

	MarkVirtualChannelFundingVerified(context.Context, ID) (bool, error)

	// MarkVirtualChannelMaterializing records that the backing parent is
	// being published for a conflict or force-close path.
	MarkVirtualChannelMaterializing(ctx context.Context,
		id ID) (bool, error)
}

// BackingMaterializer materializes the VTXO ancestry and final cooperative
// backing spend needed before lnd publishes a child transaction.
type BackingMaterializer interface {
	// MaterializeVirtualChannelBacking blocks until the channel's
	// cooperative backing transaction is publishable by lnd's child spend.
	MaterializeVirtualChannelBacking(ctx context.Context,
		channel *Channel) error
}

// MaterializingPublishInterceptor publishes virtual channel backing parents
// before allowing lnd to publish a dependent child spend.
type MaterializingPublishInterceptor struct {
	store        MaterializationStore
	materializer BackingMaterializer
	ctx          func() context.Context
}

// MaterializingPublishInterceptorConfig configures a publish interceptor.
type MaterializingPublishInterceptorConfig struct {
	Store        MaterializationStore
	Materializer BackingMaterializer
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
	if cfg.Materializer == nil {
		return nil, fmt.Errorf("virtual channel materializer is " +
			"required")
	}
	if cfg.Context == nil {
		cfg.Context = context.Background
	}

	return &MaterializingPublishInterceptor{
		store:        cfg.Store,
		materializer: cfg.Materializer,
		ctx:          cfg.Context,
	}, nil
}

// PublishTransaction implements lnwallet.PublishInterceptor.
func (i *MaterializingPublishInterceptor) PublishTransaction(tx *wire.MsgTx,
	_ string, publish func() error) error {

	ctx := i.ctx()
	suppress, err := i.suppressVirtualFundingPublish(ctx, tx)
	if err != nil {
		return err
	}
	if suppress {
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

		err = i.materializer.MaterializeVirtualChannelBacking(
			ctx, channel,
		)
		if err != nil {
			return fmt.Errorf("materialize virtual channel %x "+
				"backing: %w", channel.ID, err)
		}
	}

	return nil
}

// shouldMaterialize reports whether a dependent lnd publish should materialize
// the virtual funding parent.
func shouldMaterialize(status Status) bool {
	switch status {
	case StatusBackingArmed, StatusRoundConfirmed, StatusActive,
		StatusClosing, StatusMaterializing:
		return true

	default:
		return false
	}
}

// shouldSuppressFundingPublish reports whether lnd's direct funding
// transaction publish should stay virtual.
func shouldSuppressFundingPublish(status Status) bool {
	switch status {
	case StatusNegotiating, StatusFundingVerified, StatusBackingArmed,
		StatusRoundConfirmed, StatusActive:
		return true

	default:
		return false
	}
}

// Compile-time check that the interceptor satisfies lnd's hook interface.
var _ lnwallet.PublishInterceptor = (*MaterializingPublishInterceptor)(nil)
