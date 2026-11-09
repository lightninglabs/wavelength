package lnruntime

import (
	"sync/atomic"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

// runtimeNotifier supplies independent block streams to native lnd
// components.
type runtimeNotifier struct {
	height             int32
	started            atomic.Bool
	epochRegistrations atomic.Int32
}

// newRuntimeNotifier creates a started notifier at a fixed height.
func newRuntimeNotifier(height int32) *runtimeNotifier {
	notifier := &runtimeNotifier{height: height}
	notifier.started.Store(true)

	return notifier
}

// RegisterConfirmationsNtfn returns an idle confirmation subscription.
func (*runtimeNotifier) RegisterConfirmationsNtfn(*chainhash.Hash, []byte,
	uint32, uint32, ...chainntnfs.NotifierOption) (
	*chainntnfs.ConfirmationEvent, error) {

	return chainntnfs.NewConfirmationEvent(1, func() {}), nil
}

// RegisterSpendNtfn returns an idle spend subscription.
func (*runtimeNotifier) RegisterSpendNtfn(*wire.OutPoint, []byte, uint32) (
	*chainntnfs.SpendEvent, error) {

	return chainntnfs.NewSpendEvent(func() {}), nil
}

// RegisterBlockEpochNtfn returns a private stream seeded with the current tip.
func (n *runtimeNotifier) RegisterBlockEpochNtfn(*chainntnfs.BlockEpoch) (
	*chainntnfs.BlockEpochEvent, error) {

	n.epochRegistrations.Add(1)
	epochs := make(chan *chainntnfs.BlockEpoch, 1)
	epochs <- &chainntnfs.BlockEpoch{
		Height: n.height,
		Hash:   &chainhash.Hash{},
	}

	return &chainntnfs.BlockEpochEvent{
		Epochs: epochs,
		Cancel: func() {},
	}, nil
}

// Start marks the notifier active.
func (n *runtimeNotifier) Start() error {
	n.started.Store(true)

	return nil
}

// Started reports notifier state.
func (n *runtimeNotifier) Started() bool {
	return n.started.Load()
}

// Stop marks the notifier inactive.
func (n *runtimeNotifier) Stop() error {
	n.started.Store(false)

	return nil
}
