package chainbackends

import (
	"context"
	"fmt"
	"sync"

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

	mu            sync.Mutex
	started       bool
	stopped       bool
	nextID        uint64
	registrations map[uint64]*backendNotifierRegistration
	wg            sync.WaitGroup
}

// backendNotifierRegistration owns both sides of one adapted registration.
type backendNotifierRegistration struct {
	mu sync.Mutex

	canceled      bool
	cancelContext context.CancelFunc
	cancelBackend func()
}

// NewBackendChainNotifier constructs a notifier over an already-running chain
// backend.
func NewBackendChainNotifier(backend chainsource.ChainBackend) (
	*BackendChainNotifier, error) {

	if backend == nil {
		return nil, fmt.Errorf("chain backend is required")
	}

	notifier := &BackendChainNotifier{
		backend: backend,
		started: true,
		registrations: make(
			map[uint64]*backendNotifierRegistration,
		),
	}

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

	ctx, id, owned, err := n.beginRegistration()
	if err != nil {
		return nil, err
	}
	heightHint, err = n.clampHeightHint(ctx, heightHint)
	if err != nil {
		n.finishRegistration(id, owned)

		return nil, err
	}
	registration, err := n.backend.RegisterConf(
		ctx, txid, pkScript, numConfs, heightHint,
		notifierOpts.IncludeBlock,
	)
	if err != nil {
		n.finishRegistration(id, owned)

		return nil, err
	}
	owned.setBackendCancel(registration.Cancel)
	if err := ctx.Err(); err != nil {
		n.finishRegistration(id, owned)

		return nil, fmt.Errorf("backend chain notifier stopped: %w",
			err)
	}

	event := chainntnfs.NewConfirmationEvent(numConfs, func() {
		owned.cancel()
	})
	go func() {
		defer n.finishRegistration(id, owned)

		forwardBackendConfirmations(ctx, registration, event)
	}()

	return event, nil
}

// RegisterSpendNtfn forwards one spend lifecycle into lnd's notifier event
// shape.
func (n *BackendChainNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	ctx, id, owned, err := n.beginRegistration()
	if err != nil {
		return nil, err
	}
	heightHint, err = n.clampHeightHint(ctx, heightHint)
	if err != nil {
		n.finishRegistration(id, owned)

		return nil, err
	}
	registration, err := n.backend.RegisterSpend(
		ctx, outpoint, pkScript, heightHint,
	)
	if err != nil {
		n.finishRegistration(id, owned)

		return nil, err
	}
	owned.setBackendCancel(registration.Cancel)
	if err := ctx.Err(); err != nil {
		n.finishRegistration(id, owned)

		return nil, fmt.Errorf("backend chain notifier stopped: %w",
			err)
	}

	event := chainntnfs.NewSpendEvent(func() {
		owned.cancel()
	})
	var lastSeq uint64
	select {
	case spend, ok := <-registration.Spend:
		if ok && applyBackendSequence(spend.Seq, &lastSeq) {
			event.Spend <- lndSpendDetail(spend)
		}

	default:
	}
	go func() {
		defer n.finishRegistration(id, owned)

		forwardBackendSpends(ctx, registration, event, lastSeq)
	}()

	return event, nil
}

// RegisterBlockEpochNtfn seeds the current tip before forwarding new blocks
// from the process chain backend. Lnd consumers use that first epoch as the
// registration barrier before starting their event loops.
func (n *BackendChainNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	ctx, id, owned, err := n.beginRegistration()
	if err != nil {
		return nil, err
	}
	registration, err := n.backend.RegisterBlocks(ctx)
	if err != nil {
		n.finishRegistration(id, owned)

		return nil, err
	}
	owned.setBackendCancel(registration.Cancel)
	height, hash, err := n.backend.BestBlock(ctx)
	if err != nil {
		n.finishRegistration(id, owned)

		return nil, fmt.Errorf("read block epoch registration tip: %w",
			err)
	}
	if err := ctx.Err(); err != nil {
		n.finishRegistration(id, owned)

		return nil, fmt.Errorf("backend chain notifier stopped: %w",
			err)
	}

	epochs := make(chan *chainntnfs.BlockEpoch, 10)
	go func() {
		defer n.finishRegistration(id, owned)
		defer close(epochs)

		lastHeight := height
		lastHash := hash
		registrationHeight := height
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
				if epoch.Height < registrationHeight {
					continue
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
			owned.cancel()
		},
	}, nil
}

// Start records notifier availability. The chain backend remains owned by the
// embedding Wavelength wallet.
func (n *BackendChainNotifier) Start() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.stopped {
		return fmt.Errorf("backend chain notifier already stopped")
	}
	n.started = true

	return nil
}

// Started reports whether the adapter accepts registrations.
func (n *BackendChainNotifier) Started() bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.started
}

// Stop cancels adapter-owned registrations without stopping the shared backend.
func (n *BackendChainNotifier) Stop() error {
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()

		return nil
	}
	n.started = false
	n.stopped = true
	registrations := make(
		[]*backendNotifierRegistration, 0, len(n.registrations),
	)
	for _, registration := range n.registrations {
		registrations = append(registrations, registration)
	}
	n.mu.Unlock()

	for _, registration := range registrations {
		registration.cancel()
	}
	n.wg.Wait()

	return nil
}

// beginRegistration reserves lifecycle ownership before calling the backend.
func (n *BackendChainNotifier) beginRegistration() (context.Context, uint64,
	*backendNotifierRegistration, error) {

	n.mu.Lock()
	defer n.mu.Unlock()

	if !n.started || n.stopped {
		return nil, 0, nil, fmt.Errorf("backend chain notifier is " +
			"stopped")
	}
	ctx, cancel := context.WithCancel(context.Background())
	n.nextID++
	id := n.nextID
	registration := &backendNotifierRegistration{
		cancelContext: cancel,
	}
	n.registrations[id] = registration
	n.wg.Add(1)

	return ctx, id, registration, nil
}

// finishRegistration releases one lifecycle reservation exactly once.
func (n *BackendChainNotifier) finishRegistration(id uint64,
	registration *backendNotifierRegistration) {

	registration.cancel()
	n.mu.Lock()
	if n.registrations[id] == registration {
		delete(n.registrations, id)
		n.wg.Done()
	}
	n.mu.Unlock()
}

// clampHeightHint prevents virtual SCID heights above tip from suppressing a
// backend's historical confirmation or spend scan.
func (n *BackendChainNotifier) clampHeightHint(ctx context.Context,
	heightHint uint32) (uint32, error) {

	if heightHint == 0 {
		return 0, nil
	}
	height, _, err := n.backend.BestBlock(ctx)
	if err != nil {
		return 0, fmt.Errorf("read notifier height-hint tip: %w", err)
	}
	if height < 0 {
		return 0, fmt.Errorf("invalid notifier tip height %d", height)
	}
	if heightHint > uint32(height) {
		return uint32(height), nil
	}

	return heightHint, nil
}

// setBackendCancel installs the backend half of a registration cancellation.
func (r *backendNotifierRegistration) setBackendCancel(cancel func()) {
	r.mu.Lock()
	if !r.canceled {
		r.cancelBackend = cancel
		r.mu.Unlock()

		return
	}
	r.mu.Unlock()

	cancel()
}

// cancel releases both the adapter context and backend registration once.
func (r *backendNotifierRegistration) cancel() {
	r.mu.Lock()
	if r.canceled {
		r.mu.Unlock()

		return
	}
	r.canceled = true
	cancelContext := r.cancelContext
	cancelBackend := r.cancelBackend
	r.mu.Unlock()

	cancelContext()
	if cancelBackend != nil {
		cancelBackend()
	}
}

// forwardBackendConfirmations preserves each backend lifecycle on the lnd
// event returned to native channel components.
func forwardBackendConfirmations(ctx context.Context,
	registration *chainsource.ConfRegistration,
	event *chainntnfs.ConfirmationEvent) {

	var lastSeq uint64
	for {
		select {
		case confirmation, ok := <-registration.Confirmed:
			if !ok {
				return
			}
			if !applyBackendSequence(confirmation.Seq, &lastSeq) {
				continue
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

		case seq, ok := <-registration.Reorged:
			if !ok {
				return
			}
			if !applyBackendSequence(seq, &lastSeq) {
				continue
			}
			select {
			case event.NegativeConf <- 1:
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
	event *chainntnfs.SpendEvent, lastSeq uint64) {

	for {
		select {
		case spend, ok := <-registration.Spend:
			if !ok {
				return
			}
			if !applyBackendSequence(spend.Seq, &lastSeq) {
				continue
			}
			select {
			case event.Spend <- lndSpendDetail(spend):
			case <-ctx.Done():
				return
			}

		case seq, ok := <-registration.Reorged:
			if !ok {
				return
			}
			if !applyBackendSequence(seq, &lastSeq) {
				continue
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

// lndSpendDetail converts a backend spend into lnd's notifier event shape.
func lndSpendDetail(spend *chainsource.SpendDetail) *chainntnfs.SpendDetail {
	return &chainntnfs.SpendDetail{
		SpentOutPoint:     spend.SpentOutPoint,
		SpenderTxHash:     spend.SpenderTxHash,
		SpendingTx:        spend.SpendingTx,
		SpenderInputIndex: spend.SpenderInputIndex,
		SpendingHeight:    spend.SpendingHeight,
	}
}

// applyBackendSequence rejects an event that lost ordering across two ready
// backend channels. Sequence zero denotes a backend without reorg ordering.
func applyBackendSequence(seq uint64, lastSeq *uint64) bool {
	if seq == 0 {
		return true
	}
	if seq <= *lastSeq {
		return false
	}
	*lastSeq = seq

	return true
}

var _ chainntnfs.ChainNotifier = (*BackendChainNotifier)(nil)
