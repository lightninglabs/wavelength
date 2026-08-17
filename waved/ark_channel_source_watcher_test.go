package waved

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/fraud"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

type sourceWatcherRegistration struct {
	spend  chan *chainsource.SpendDetail
	done   chan struct{}
	once   sync.Once
	cancel bool
}

type sourceWatcherBackend struct {
	chainsource.ChainBackend

	mu            sync.Mutex
	registrations map[wire.OutPoint]*sourceWatcherRegistration
}

type sourceWatcherSink struct {
	mu        sync.Mutex
	failures  int
	calls     int
	channelID arkchannel.ID
	event     *arkchannel.SourceSpent
}

// RegisterSpend records one passive spend registration.
func (b *sourceWatcherBackend) RegisterSpend(_ context.Context,
	outpoint *wire.OutPoint, _ []byte, _ uint32) (
	*chainsource.SpendRegistration, error) {

	b.mu.Lock()
	defer b.mu.Unlock()
	registration := &sourceWatcherRegistration{
		spend: make(chan *chainsource.SpendDetail, 1),
		done:  make(chan struct{}),
	}
	b.registrations[*outpoint] = registration

	return &chainsource.SpendRegistration{
		Spend:   registration.spend,
		Reorged: make(chan uint64),
		Done:    registration.done,
		Cancel: func() {
			registration.once.Do(func() {
				b.mu.Lock()
				registration.cancel = true
				b.mu.Unlock()
				close(registration.done)
			})
		},
	}, nil
}

// Apply records one watcher event and injects transient failures when asked.
func (s *sourceWatcherSink) Apply(_ context.Context, id arkchannel.ID,
	event arkchannel.Event) (arkchannel.Record, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.channelID = id
	sourceSpent, ok := event.(*arkchannel.SourceSpent)
	if !ok {
		return arkchannel.Record{}, fmt.Errorf("unexpected source "+
			"watcher event %T", event)
	}
	s.event = sourceSpent
	if s.failures > 0 {
		s.failures--

		return arkchannel.Record{}, context.DeadlineExceeded
	}

	return arkchannel.Record{}, nil
}

// TestArkChannelSourceWatcherCoversAllAncestry verifies every path in a
// multi-input OOR proof is armed and the first spend is retried durably.
func TestArkChannelSourceWatcherCoversAllAncestry(t *testing.T) {
	t.Parallel()

	treeOne, sourceOne := sourceWatcherTree(t, 10)
	treeTwo, sourceTwo := sourceWatcherTree(t, 20)
	desc := &vtxo.Descriptor{
		Outpoint: sourceWatcherOutpoint(30),
		Ancestry: []vtxo.Ancestry{
			{
				TreePath:       treeOne,
				CommitmentTxID: treeOne.Root.Input.Hash,
				InputIndices: []uint32{
					0,
				},
				TreeDepth: 1,
			},
			{
				TreePath:       treeTwo,
				CommitmentTxID: treeTwo.Root.Input.Hash,
				InputIndices: []uint32{
					1,
				},
				TreeDepth: 1,
			},
		},
		CreatedHeight: 7,
	}
	plan, err := fraud.BuildWatchPlan(desc)
	require.NoError(t, err)

	backend := &sourceWatcherBackend{
		registrations: make(
			map[wire.OutPoint]*sourceWatcherRegistration,
		),
	}
	sink := &sourceWatcherSink{failures: 1}
	watcher := newArkChannelSourceWatcher(backend, btclog.Disabled)
	watcher.retryDelay = 5 * time.Millisecond
	require.NoError(t, watcher.BindChannelEventSink(sink))
	t.Cleanup(watcher.Stop)

	id := arkchannel.ID{1, 2, 3}
	require.NoError(t, watcher.Track(t.Context(), id, desc))
	backend.mu.Lock()
	require.Len(t, backend.registrations, len(plan.Watches))
	require.Contains(t, backend.registrations, treeOne.Root.Input)
	require.Contains(t, backend.registrations, sourceOne)
	require.Contains(t, backend.registrations, treeTwo.Root.Input)
	require.Contains(t, backend.registrations, sourceTwo)
	registration := backend.registrations[treeTwo.Root.Input]
	backend.mu.Unlock()

	spendingTx := wire.NewMsgTx(2)
	spendingTx.AddTxIn(wire.NewTxIn(
		&treeTwo.Root.Input, nil, nil,
	))
	spendingTx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
	spendingTxID := spendingTx.TxHash()
	registration.spend <- &chainsource.SpendDetail{
		SpentOutPoint:     &treeTwo.Root.Input,
		SpenderTxHash:     &spendingTxID,
		SpendingTx:        spendingTx,
		SpenderInputIndex: 0,
		SpendingHeight:    8,
	}

	require.Eventually(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()

		return sink.calls == 2
	}, time.Second, time.Millisecond)
	sink.mu.Lock()
	require.Equal(t, id, sink.channelID)
	require.Equal(t, treeTwo.Root.Input, sink.event.OutPoint)
	require.Equal(t, spendingTxID, sink.event.SpendingTxID)
	sink.mu.Unlock()

	require.Eventually(t, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		for _, item := range backend.registrations {
			if !item.cancel {
				return false
			}
		}

		return true
	}, time.Second, time.Millisecond)
}

// sourceWatcherTree creates a one-node ancestry tree and its VTXO leaf.
func sourceWatcherTree(t *testing.T, seed byte) (*tree.Tree, wire.OutPoint) {
	t.Helper()
	root := &tree.Node{
		Input: sourceWatcherOutpoint(seed),
		Outputs: []*wire.TxOut{
			{
				Value: 1_000,
				PkScript: []byte{
					0x51,
					seed,
				},
			},
		},
		Children: make(map[uint32]*tree.Node),
	}
	source, err := root.GetNonAnchorOutpoint()
	require.NoError(t, err)

	return &tree.Tree{
		Root: root,
		BatchOutput: &wire.TxOut{
			Value: 1_000,
			PkScript: []byte{
				0x51,
				seed,
				0,
			},
		},
	}, *source
}

// sourceWatcherOutpoint returns one deterministic test outpoint.
func sourceWatcherOutpoint(seed byte) wire.OutPoint {
	var hash chainhash.Hash
	hash[0] = seed

	return wire.OutPoint{Hash: hash}
}
