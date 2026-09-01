package chainbackends

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

// BackendChainNotifier adapts Wavelength's process-owned chain backend to the
// native lnd notifier interface. It does not start or stop the backend because
// the embedding wallet owns that lifecycle.
type BackendChainNotifier struct {
	backend chainsource.ChainBackend
	started atomic.Bool
}

// NewBackendChainNotifier constructs a notifier over an already-running chain
// backend.
func NewBackendChainNotifier(backend chainsource.ChainBackend) (
	*BackendChainNotifier, error) {

	if backend == nil {
		return nil, fmt.Errorf("chain backend is required")
	}

	notifier := &BackendChainNotifier{backend: backend}
	notifier.started.Store(true)

	return notifier, nil
}

// RegisterConfirmationsNtfn forwards one confirmation lifecycle into lnd's
// notifier event shape.
func (n *BackendChainNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent,
	error) {

	notifierOpts := chainntnfs.DefaultNotifierOptions()
	for _, opt := range opts {
		opt(notifierOpts)
	}

	ctx, cancel := context.WithCancel(context.Background())
	registration, err := n.backend.RegisterConf(
		ctx, txid, pkScript, numConfs, heightHint,
		notifierOpts.IncludeBlock,
	)
	if err != nil {
		cancel()

		return nil, err
	}

	event := chainntnfs.NewConfirmationEvent(numConfs, func() {
		cancel()
		registration.Cancel()
	})
	go forwardBackendConfirmations(ctx, registration, event)

	return event, nil
}

// RegisterSpendNtfn forwards one spend lifecycle into lnd's notifier event
// shape.
func (n *BackendChainNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	ctx, cancel := context.WithCancel(context.Background())
	registration, err := n.backend.RegisterSpend(
		ctx, outpoint, pkScript, heightHint,
	)
	if err != nil {
		cancel()

		return nil, err
	}

	event := chainntnfs.NewSpendEvent(func() {
		cancel()
		registration.Cancel()
	})
	go forwardBackendSpends(ctx, registration, event)

	return event, nil
}

// RegisterBlockEpochNtfn seeds the current tip before forwarding new blocks
// from the process chain backend. Lnd consumers use that first epoch as the
// registration barrier before starting their event loops.
func (n *BackendChainNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	ctx, cancel := context.WithCancel(context.Background())
	registration, err := n.backend.RegisterBlocks(ctx)
	if err != nil {
		cancel()

		return nil, err
	}
	height, hash, err := n.backend.BestBlock(ctx)
	if err != nil {
		cancel()
		registration.Cancel()

		return nil, fmt.Errorf("read block epoch registration tip: %w",
			err)
	}

	epochs := make(chan *chainntnfs.BlockEpoch, 10)
	go func() {
		defer close(epochs)

		lastHeight := height
		lastHash := hash
		seedTip := bestBlock == nil || bestBlock.Hash == nil ||
			bestBlock.Height != height || *bestBlock.Hash != hash
		if seedTip {
			select {
			case epochs <- &chainntnfs.BlockEpoch{
				Hash: &hash, Height: height,
			}:
			case <-ctx.Done():
				return
			}
		}

		for {
			select {
			case epoch, ok := <-registration.Epochs:
				if !ok {
					return
				}
				if epoch.Height == lastHeight &&
					epoch.Hash == lastHash {

					continue
				}
				lastHeight = epoch.Height
				lastHash = epoch.Hash
				hash := epoch.Hash
				select {
				case epochs <- &chainntnfs.BlockEpoch{
					Hash: &hash, Height: epoch.Height,
				}:
				case <-ctx.Done():
					return
				}

			case <-ctx.Done():
				return
			}
		}
	}()

	return &chainntnfs.BlockEpochEvent{
		Epochs: epochs,
		Cancel: func() {
			cancel()
			registration.Cancel()
		},
	}, nil
}

// Start records notifier availability. The chain backend remains owned by the
// embedding Wavelength wallet.
func (n *BackendChainNotifier) Start() error {
	n.started.Store(true)

	return nil
}

// Started reports whether the adapter accepts registrations.
func (n *BackendChainNotifier) Started() bool {
	return n.started.Load()
}

// Stop marks the adapter stopped without stopping the shared chain backend.
func (n *BackendChainNotifier) Stop() error {
	n.started.Store(false)

	return nil
}

// forwardBackendConfirmations preserves each backend lifecycle on the lnd
// event returned to native channel components.
func forwardBackendConfirmations(ctx context.Context,
	registration *chainsource.ConfRegistration,
	event *chainntnfs.ConfirmationEvent) {

	for {
		select {
		case confirmation, ok := <-registration.Confirmed:
			if !ok {
				return
			}
			select {
			case event.Confirmed <- &chainntnfs.TxConfirmation{
				BlockHash:   confirmation.BlockHash,
				BlockHeight: confirmation.BlockHeight,
				TxIndex:     confirmation.TxIndex,
				Tx:          confirmation.Tx,
				Block:       confirmation.Block,
			}:
			case <-ctx.Done():
				return
			}

		case _, ok := <-registration.Reorged:
			if !ok {
				return
			}
			select {
			case event.NegativeConf <- 0:
			case <-ctx.Done():
				return
			}

		case _, ok := <-registration.Done:
			if !ok {
				return
			}
			select {
			case event.Done <- struct{}{}:
			case <-ctx.Done():
			}

			return

		case <-ctx.Done():
			return
		}
	}
}

// forwardBackendSpends preserves each backend spend lifecycle on the lnd
// event returned to native channel components.
func forwardBackendSpends(ctx context.Context,
	registration *chainsource.SpendRegistration,
	event *chainntnfs.SpendEvent) {

	for {
		select {
		case spend, ok := <-registration.Spend:
			if !ok {
				return
			}
			select {
			case event.Spend <- &chainntnfs.SpendDetail{
				SpentOutPoint:     spend.SpentOutPoint,
				SpenderTxHash:     spend.SpenderTxHash,
				SpendingTx:        spend.SpendingTx,
				SpenderInputIndex: spend.SpenderInputIndex,
				SpendingHeight:    spend.SpendingHeight,
			}:
			case <-ctx.Done():
				return
			}

		case _, ok := <-registration.Reorged:
			if !ok {
				return
			}
			select {
			case event.Reorg <- struct{}{}:
			case <-ctx.Done():
				return
			}

		case _, ok := <-registration.Done:
			if !ok {
				return
			}
			select {
			case event.Done <- struct{}{}:
			case <-ctx.Done():
			}

			return

		case <-ctx.Done():
			return
		}
	}
}

var _ chainntnfs.ChainNotifier = (*BackendChainNotifier)(nil)
