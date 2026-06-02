package virtualchannel

import (
	lnd "github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/chanacceptor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// BuildAuxComponents builds the lnd auxiliary components needed by virtual
// channels when waved runs lnd in the same process.
func BuildAuxComponents(cfg MaterializingPublishInterceptorConfig) (
	*lnd.AuxComponents, error) {

	publishInterceptor, err := NewMaterializingPublishInterceptor(cfg)
	if err != nil {
		return nil, err
	}

	channelAcceptor, err := NewRegisteredChannelAcceptor(
		RegisteredChannelAcceptorConfig{
			Store:   cfg.Store,
			Context: cfg.Context,
		},
	)
	if err != nil {
		return nil, err
	}

	hopHintProvider, err := NewInvoiceHopHintProvider(cfg.Store)
	if err != nil {
		return nil, err
	}

	return &lnd.AuxComponents{
		PublishInterceptor: fn.Some[lnwallet.PublishInterceptor](
			publishInterceptor,
		),
		ChannelAcceptor: fn.Some[chanacceptor.ChannelAcceptor](
			channelAcceptor,
		),
		InvoiceHopHintProvider: fn.Some[invoicesrpc.HopHintProvider](
			hopHintProvider,
		),
	}, nil
}
