package waved

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// sourceExpiryBackend allows tip jumps, dropped subscriptions, and registration
// failures without changing the persisted channel lifecycle.
type sourceExpiryBackend struct {
	chainsource.ChainBackend
	mu            sync.Mutex
	height        int32
	registrations []chan *chainsource.BlockEpoch
}

// offlineEpochBackend supplies an authoritative tip but no notification feed.
type offlineEpochBackend struct{ *sourceExpiryBackend }

// RegisterBlocks simulates a disconnected subscription independently of RPC.
func (offlineEpochBackend) RegisterBlocks(context.Context) (
	*chainsource.BlockRegistration, error) {

	return nil, fmt.Errorf("stream offline")
}

// TestExpiryTipWithoutStream ensures a failed subscription cannot suppress
// catch-up when the backend can still answer an authoritative tip query.
func TestExpiryTipWithoutStream(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	backend := offlineEpochBackend{&sourceExpiryBackend{height: 701}}
	var tip int32
	watchSourceExpiryTip(
		ctx, backend, clock.NewDefaultClock(), btclog.Disabled,
		func(height int32) {
			tip = height
			cancel()
		},
	)
	require.EqualValues(t, 701, tip)
}

// TestHubRecoverySharedExpiry verifies that endpoint recovery views have one
// height subscription and still observe a missed-height jump. Idle endpoint
// counts are diagnostic; the subscription count is a correctness assertion.
func TestHubRecoverySharedExpiry(t *testing.T) {
	raw, _ := testArkChannelResumeStore(t, t.TempDir())
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	backend := &sourceExpiryBackend{height: 100}
	hub, err := NewArkChannelRecoveryRuntime(
		t.Context(), ArkChannelRecoveryRuntimeConfig{
			Store: raw,
			ChannelStore: raw.NewArkChannelStore(
				clock.NewDefaultClock(),
			),
			Wallet:       struct{ ArkChannelUnrollWallet }{},
			ChainBackend: backend,
			Log:          btclog.Disabled,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, hub.Stop()) })
	require.Eventually(t, func() bool {
		height, _ := hub.SourceExpiryTip()

		return height == 100
	}, time.Second, time.Millisecond)
	_, firstWake := hub.SourceExpiryTip()
	for n := 1; n <= 100; n++ {
		view, _, err := hub.NewEndpoint()
		require.NoError(t, err)
		t.Cleanup(view.Stop)
		_, next := hub.SourceExpiryTip()
		require.Equal(t, firstWake, next)
		if n == 1 || n == 100 {
			t.Logf(
				"%d idle recovery views: %d process "+
					"goroutines, %d open SQL connections",
				n, runtime.NumGoroutine(),
				raw.DB().Stats().OpenConnections,
			)
		}
	}
	backend.mu.Lock()
	count := len(backend.registrations)
	backend.height = 120
	stream := backend.registrations[0]
	backend.mu.Unlock()
	require.Equal(t, 1, count)
	stream <- &chainsource.BlockEpoch{Height: 120}
	select {
	case <-firstWake:
	case <-time.After(time.Second):
		t.Fatal("shared tip did not wake recovery views")
	}
	height, _ := hub.SourceExpiryTip()
	require.EqualValues(t, 120, height)
}

// BestBlock returns the independently observed test tip.
func (b *sourceExpiryBackend) BestBlock(context.Context) (int32, chainhash.Hash,
	error) {

	b.mu.Lock()
	defer b.mu.Unlock()

	return b.height, chainhash.Hash{}, nil
}

// RegisterBlocks records a process-owned subscription.
func (b *sourceExpiryBackend) RegisterBlocks(context.Context) (
	*chainsource.BlockRegistration, error) {

	b.mu.Lock()
	defer b.mu.Unlock()
	stream := make(chan *chainsource.BlockEpoch, 1)
	b.registrations = append(b.registrations, stream)

	return &chainsource.BlockRegistration{
		Epochs: stream, Cancel: func() {},
	}, nil
}

// sourceExpiryRecovery isolates the deadline query from materialization. The
// archive's descriptor validation is exercised in its recovery tests.
type sourceExpiryRecovery struct {
	ArkChannelRecoveryController
	expiry int32
}

// Stop leaves the test-owned deadline fixture intact.
func (r sourceExpiryRecovery) Stop() {}

// SourceExpiring uses the production deadline arithmetic over persisted data.
func (r sourceExpiryRecovery) SourceExpiring(_ context.Context,
	terms arkchannel.Terms, _ arkchannel.VTXOBinding, height int32) (bool,
	error) {

	deadline, err := arkchannel.SourceMaterializationHeight(
		r.expiry, terms.VTXO.FunderDelay,
	)

	return height >= deadline, err
}

// TestSourceExpiryArchiveSurvivesRestart derives the guard from the exact
// recovery-only descriptor after reopening the production SQL store.
func TestSourceExpiryArchiveSurvivesRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	raw, coordinator := testArkChannelResumeStore(t, dir)
	initial := testArkChannelResumeRecord(t, coordinator, false)
	terms := initial.Snapshot.Terms
	source := *initial.Snapshot.Source
	desc, err := arkChannelRecoveryDescriptor(
		terms, source, arkchannel.RecoveryDescriptor{
			RoundID: "expiry-round", BatchExpiry: 1_000,
			CreatedHeight:  100,
			CommitmentTxID: chainhash.Hash{1},
		},
	)
	require.NoError(t, err)
	require.NoError(
		t,
		raw.NewVTXOStore(
			clock.NewDefaultClock()).
			SaveRecoveryOnlyVTXO(t.Context(),
				desc,
			),
	)
	require.NoError(t, raw.Close())
	raw, err = db.NewStoreFromConfig(db.DefaultConfig(dir), btclog.Disabled)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	archive := &arkChannelRecoveryArchive{
		vtxos: raw.NewVTXOStore(clock.NewDefaultClock()),
	}
	deadline, err := arkchannel.SourceMaterializationHeight(
		1_000, terms.VTXO.FunderDelay,
	)
	require.NoError(t, err)
	for _, height := range []int32{deadline - 1, deadline, 1_001} {
		expiring, err := archive.SourceExpiring(
			t.Context(), terms, source, height,
		)
		require.NoError(t, err)
		require.Equal(t, height >= deadline, expiring)
	}
	_, err = archive.SourceExpiring(t.Context(), terms, source, -1)
	require.Error(t, err)
}

// TestSourceExpiryGuardCatchesMissedBlocks proves a lost stream is replaced,
// its queued heights are not trusted, and recovery starts without a new block
// after reconnect. The SQL transition precedes a blocking materializer.
func TestSourceExpiryGuardCatchesMissedBlocks(t *testing.T) {
	t.Parallel()

	raw, coordinator := testArkChannelResumeStore(t, t.TempDir())
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	initial := testArkChannelResumeRecord(t, coordinator, false)
	deadline, err := arkchannel.SourceMaterializationHeight(
		1_000, initial.Snapshot.Terms.VTXO.FunderDelay,
	)
	require.NoError(t, err)
	backend := &sourceExpiryBackend{height: deadline - 1}
	started := make(chan struct{}, 1)
	service, err := arkchannel.NewService(
		arkchannel.PartyHub, coordinator,
		arkChannelResumeExecutor(func(ctx context.Context,
			_ arkchannel.ID, action arkchannel.Action) error {

			if _, ok := action.(*arkchannel.PublishChannel); !ok {
				return fmt.Errorf("unexpected expiry action %T",
					action)
			}
			started <- struct{}{}
			<-ctx.Done()

			return ctx.Err()
		}),
	)
	require.NoError(t, err)
	controller := testArkChannelResumeController(t)
	controller.coordinator = coordinator
	controller.cfg.ChainBackend = backend
	controller.cfg.Recovery = sourceExpiryRecovery{expiry: 1_000}
	testClock := clock.NewTestClock(time.Now())
	controller.cfg.Clock = testClock
	controller.startLifecycleWorker(func(ctx context.Context) {
		controller.runSourceExpiryGuard(ctx, service, nil)
	})
	require.Eventually(t, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()

		return len(backend.registrations) == 1
	}, time.Second, time.Millisecond)
	record, err := coordinator.Get(t.Context(), initial.Snapshot.Terms.ID)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseActive, record.Snapshot.Phase)
	select {
	case <-started:
		t.Fatal("source materialized before its deadline")

	default:
	}
	backend.mu.Lock()
	backend.height = deadline + 5
	close(backend.registrations[0])
	backend.mu.Unlock()
	require.Eventually(t, func() bool {
		testClock.SetTime(testClock.Now().Add(time.Second))
		backend.mu.Lock()
		defer backend.mu.Unlock()

		return len(backend.registrations) >= 2
	}, time.Second, time.Millisecond)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("source deadline was lost with the block stream")
	}
	record, err = coordinator.Get(t.Context(), initial.Snapshot.Terms.ID)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseMaterializing, record.Snapshot.Phase)
	require.Nil(t, record.Snapshot.SourceConflict)
	_, err = arkchannel.PendingAction(record.Snapshot)
	require.NoError(t, err)
	controller.Quiesce()
}

// TestSourceExpiryGuardStartupCatchup proves startup does not wait for the
// next block when the persisted source deadline passed while offline.
func TestSourceExpiryGuardStartupCatchup(t *testing.T) {
	t.Parallel()

	raw, coordinator := testArkChannelResumeStore(t, t.TempDir())
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	initial := testArkChannelResumeRecord(t, coordinator, false)
	started := make(chan struct{}, 1)
	service, err := arkchannel.NewService(
		arkchannel.PartyHub, coordinator,
		arkChannelResumeExecutor(func(ctx context.Context,
			_ arkchannel.ID, _ arkchannel.Action) error {

			started <- struct{}{}
			<-ctx.Done()

			return ctx.Err()
		}),
	)
	require.NoError(t, err)
	controller := testArkChannelResumeController(t)
	controller.coordinator = coordinator
	controller.cfg.ChainBackend = &sourceExpiryBackend{height: 500}
	controller.cfg.Recovery = sourceExpiryRecovery{expiry: 1_000}
	controller.startLifecycleWorker(func(ctx context.Context) {
		controller.runSourceExpiryGuard(ctx, service, nil)
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal(
			"startup waited for a future block to protect the " +
				"source",
		)
	}
	record, err := coordinator.Get(t.Context(), initial.Snapshot.Terms.ID)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseMaterializing, record.Snapshot.Phase)
	controller.Quiesce()
}
