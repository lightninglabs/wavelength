package waved

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/internal/expiryfixture"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// TestHasUnknownCommitmentHeight verifies that only usable ancestry with at
// least one missing height enters the startup repair.
func TestHasUnknownCommitmentHeight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		desc *vtxo.Descriptor
		want bool
	}{
		{
			name: "nil descriptor",
		},
		{
			name: "empty ancestry",
			desc: &vtxo.Descriptor{},
		},
		{
			name: "all heights known",
			desc: &vtxo.Descriptor{
				Ancestry: []vtxo.Ancestry{{
					CommitmentHeight: 100,
				}},
			},
		},
		{
			name: "zero height",
			desc: &vtxo.Descriptor{
				Ancestry: []vtxo.Ancestry{
					{},
				},
			},
			want: true,
		},
		{
			name: "mixed heights",
			desc: &vtxo.Descriptor{
				Ancestry: []vtxo.Ancestry{
					{
						CommitmentHeight: 100,
					},
					{},
				},
			},
			want: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(
				t, test.want,
				hasUnknownCommitmentHeight(test.desc),
			)
		})
	}
}

// commitmentRepairRPC injects transport failures into a real indexer client.
// The ancestry fixtures and database writes still use production validators.
type commitmentRepairRPC struct {
	recoveryIndexerStub
	requestMu sync.Mutex
	keys      []string
	onSend    func(context.Context, int) error
}

// SendRPC records every attempt, including rejected requests.
func (r *commitmentRepairRPC) SendRPC(ctx context.Context,
	method mailboxrpc.ServiceMethod, req proto.Message,
	opts mailboxrpc.RPCOptions) (mailboxrpc.SendResult, error) {

	r.requestMu.Lock()
	r.keys = append(r.keys, opts.IdempotencyKey)
	attempt := len(r.keys)
	r.requestMu.Unlock()
	if r.onSend != nil {
		if err := r.onSend(ctx, attempt); err != nil {
			return mailboxrpc.SendResult{}, err
		}
	}

	return r.recoveryIndexerStub.SendRPC(ctx, method, req, opts)
}

// requestKeys returns a snapshot safe to inspect while the worker is running.
func (r *commitmentRepairRPC) requestKeys() []string {
	r.requestMu.Lock()
	defer r.requestMu.Unlock()

	return append([]string(nil), r.keys...)
}

// newCommitmentRepairServer persists missing heights on signed ancestry and
// returns a daemon wired to the actual store and indexer query path.
func newCommitmentRepairServer(t *testing.T, count int, clk clock.Clock) (
	*Server, *commitmentRepairRPC, []*vtxo.Descriptor) {

	t.Helper()
	sqliteDB := db.NewTestDB(t)
	rpc := &commitmentRepairRPC{
		recoveryIndexerStub: recoveryIndexerStub{
			byScript: make(map[string]*arkrpc.VTXO),
		},
	}
	s := &Server{
		clk:         clk,
		log:         btclog.Disabled,
		daemonReady: make(chan struct{}),
		chainBackend: &heightOnlyChainBackend{
			height: 500,
		},
		proofKeyBackend: &recoveryKeyBackend{},
		vtxoStore: db.NewStore(
			sqliteDB.DB, sqliteDB.Queries, sqliteDB.Backend(),
			btclog.Disabled,
		).NewVTXOStore(clk),
		indexer: indexer.New(
			rpc, nil, "test-server", "client:test",
			fn.None[btclog.Logger](),
		),
	}
	var descs []*vtxo.Descriptor
	for i := range count {
		tag := byte(i + 1)
		_, pub := btcec.PrivKeyFromBytes([]byte{tag, 1})
		_, operator := btcec.PrivKeyFromBytes([]byte{tag, 2})
		script, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootKeyNoScript(pub),
		)
		require.NoError(t, err)
		candidate, _ := expiryfixture.Round(
			t, 10000, script, 50, 321, tag,
		)
		ancestry, err := vtxo.IndexedAncestryFromRPC(candidate)
		require.NoError(t, err)
		for j := range ancestry {
			ancestry[j].CommitmentHeight = 0
		}
		desc := &vtxo.Descriptor{
			Amount: btcutil.Amount(candidate.ValueSat),
			Outpoint: wire.OutPoint{
				Index: candidate.Outpoint.Vout,
			},
			PkScript: script,
			ClientKey: keychain.KeyDescriptor{
				PubKey: pub,
				KeyLocator: keychain.KeyLocator{
					Index: uint32(i),
				},
			},
			OperatorKey:    operator,
			Ancestry:       ancestry,
			CommitmentTxID: ancestry[0].CommitmentTxID,
			RoundID:        candidate.RoundId,
			CreatedHeight:  candidate.CreatedHeight,
			RelativeExpiry: 50,
			BatchExpiry:    1000,
			Status:         vtxo.VTXOStatusLive,
		}
		copy(desc.Outpoint.Hash[:], candidate.Outpoint.Txid)
		if i == count-1 {
			desc.Status = vtxo.VTXOStatusUnilateralExit
		}
		require.NoError(t, s.vtxoStore.SaveVTXO(t.Context(), desc))
		require.NoError(
			t,
			s.vtxoStore.UpdateVTXOStatus(
				t.Context(), desc.Outpoint, desc.Status,
			),
		)
		rpc.byScript[string(script)] = candidate
		descs = append(descs, desc)
	}

	return s, rpc, descs
}

// fastCommitmentRepairPage retains the production attempt budget and retry
// implementation while bounding total wall-clock waits to a few milliseconds.
func fastCommitmentRepairPage(ctx context.Context,
	call func(mailboxrpc.RPCOptions) error) error {

	return mailboxrpc.Retry(
		ctx, mailboxrpc.RetryPolicy{
			MaxAttempts: recoveryIndexerRetryAttempts,
			BaseDelay:   time.Millisecond,
			MaxDelay:    time.Millisecond,
		},
		func(_ context.Context, opts mailboxrpc.RPCOptions) error {
			return call(opts)
		},
	)
}

// TestRepairLegacyCommitmentHeightsRetriesShedQueries proves an inventory
// larger than an operator burst completes without a restart. Both recoverable
// and exiting descriptors retain their proof and status across the backfill.
func TestRepairLegacyCommitmentHeightsRetriesShedQueries(t *testing.T) {
	t.Parallel()
	clk := clock.NewTestClock(time.Now())
	s, rpc, descs := newCommitmentRepairServer(t, 25, clk)
	var before []*vtxo.Descriptor
	for _, desc := range descs {
		reloaded, err := s.vtxoStore.GetVTXO(t.Context(), desc.Outpoint)
		require.NoError(t, err)
		before = append(before, reloaded)
	}
	rpc.onSend = func(_ context.Context, attempt int) error {
		if attempt == 21 || attempt == 22 {
			return status.Error(
				codes.ResourceExhausted, "rate limited",
			)
		}

		return nil
	}
	result, err := s.repairLegacyCommitmentHeights(
		t.Context(), fastCommitmentRepairPage,
	)
	require.NoError(t, err)
	require.Equal(t, result.candidates, result.completed)
	keys := rpc.requestKeys()
	require.Len(t, keys, 27)
	require.NotEmpty(t, keys[20])
	require.Equal(t, keys[20], keys[21])
	require.Equal(t, keys[21], keys[22])
	require.NotEqual(t, keys[19], keys[20])
	require.NotEqual(t, keys[22], keys[23])
	for i, desc := range descs {
		reloaded, err := s.vtxoStore.GetVTXO(t.Context(), desc.Outpoint)
		require.NoError(t, err)
		require.Equal(
			t, int32(321), reloaded.Ancestry[0].CommitmentHeight,
		)
		reloaded.Ancestry[0].CommitmentHeight = 0
		require.Equal(t, before[i].Ancestry, reloaded.Ancestry)
		require.Equal(t, desc.Status, reloaded.Status)
	}
	// Already repaired inventory must not consume the shared query budget.
	result, err = s.repairLegacyCommitmentHeights(
		t.Context(), fastCommitmentRepairPage,
	)
	require.NoError(t, err)
	require.Zero(t, result.candidates)
	require.Len(t, rpc.requestKeys(), len(keys))
}

// TestRepairLegacyCommitmentHeightsCancellationPreservesProgress proves a
// cancelled pass stops querying immediately and the next pass picks up only
// the unfinished inventory, using the heights already committed to storage.
func TestRepairLegacyCommitmentHeightsCancellationPreservesProgress(
	t *testing.T) {

	t.Parallel()
	clk := clock.NewTestClock(time.Now())
	s, rpc, descs := newCommitmentRepairServer(t, 6, clk)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rpc.onSend = func(_ context.Context, attempt int) error {
		if attempt == 4 {
			cancel()

			return ctx.Err()
		}

		return nil
	}
	result, err := s.repairLegacyCommitmentHeights(
		ctx, fastCommitmentRepairPage,
	)
	require.Equal(t, 6, result.candidates)
	require.Equal(t, 3, result.completed)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "3 of 6")
	require.Len(t, rpc.requestKeys(), 4)
	var repaired int
	for _, desc := range descs {
		reloaded, err := s.vtxoStore.GetVTXO(t.Context(), desc.Outpoint)
		require.NoError(t, err)
		if !hasUnknownCommitmentHeight(reloaded) {
			repaired++
		}
	}
	require.Equal(t, 3, repaired)
	result, err = s.repairLegacyCommitmentHeights(
		t.Context(), fastCommitmentRepairPage,
	)
	require.NoError(t, err)
	require.Equal(t, 3, result.completed)
	require.Len(t, rpc.requestKeys(), 7)
	for _, desc := range descs {
		reloaded, err := s.vtxoStore.GetVTXO(t.Context(), desc.Outpoint)
		require.NoError(t, err)
		require.False(t, hasUnknownCommitmentHeight(reloaded))
	}
}

// commitmentRepairLog delivers the maintenance summary to the test without
// racing a bytes.Buffer read against the background logger.
type commitmentRepairLog chan string

// Write forwards a log record to a buffered test channel.
func (l commitmentRepairLog) Write(p []byte) (int, error) {
	l <- string(p)

	return len(p), nil
}

// TestLegacyCommitmentHeightRepairResumesPartialPass proves a failed target is
// retried after the cooldown while completed targets are skipped. Full
// readiness is independent of repair completion and the worker exits on
// success.
func TestLegacyCommitmentHeightRepairResumesPartialPass(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		failures int
		err      error
	}{
		{
			"deadline",
			1,
			context.DeadlineExceeded,
		},
		{"rate limit retry exhaustion", recoveryIndexerRetryAttempts,
			status.Error(codes.ResourceExhausted, "rate limited")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ticks := make(chan time.Duration, 1)
			clk := clock.NewTestClockWithTickSignal(
				time.Now(), ticks,
			)
			s, rpc, descs := newCommitmentRepairServer(t, 3, clk)
			logs := make(commitmentRepairLog, 10)
			s.log = btclog.NewSLogger(
				btclog.NewDefaultHandler(logs),
			)
			rpc.onSend = func(_ context.Context, n int) error {
				if n <= test.failures {
					return test.err
				}

				return nil
			}
			stop := s.startLegacyCommitmentHeightRepair(
				t.Context(), fastCommitmentRepairPage,
			)
			t.Cleanup(stop)
			require.Empty(t, rpc.requestKeys())
			s.markDaemonReady()
			select {
			case delay := <-ticks:
				require.Equal(
					t, legacyCommitmentHeightRepairInterval,
					delay,
				)

			case <-time.After(5 * time.Second):
				t.Fatal("repair did not schedule the next pass")
			}
			keys := rpc.requestKeys()
			require.Len(t, keys, 2+test.failures)
			for _, key := range keys[:test.failures] {
				require.Equal(t, keys[0], key)
			}
			var repaired int
			for _, desc := range descs {
				reloaded, err := s.vtxoStore.GetVTXO(
					t.Context(), desc.Outpoint,
				)
				require.NoError(t, err)
				if !hasUnknownCommitmentHeight(reloaded) {
					repaired++
				}
			}
			require.Equal(t, 2, repaired)
			interval := legacyCommitmentHeightRepairInterval
			clk.SetTime(clk.Now().Add(interval))
			deadline := time.After(5 * time.Second)
			for {
				select {
				case record := <-logs:
					if !strings.Contains(
						record, "repair complete",
					) {

						continue
					}
					stop()
					require.Len(
						t, rpc.requestKeys(),
						3+test.failures,
					)
					assertHeightRepairComplete(t, s, descs)

					return

				case <-deadline:
					t.Fatal("repair never completed")
				}
			}
		})
	}
}

// TestLegacyCommitmentHeightRepairShutdown joins maintenance before resource
// teardown, whether shutdown arrives before readiness, in an RPC, or in
// backoff.
func TestLegacyCommitmentHeightRepairShutdown(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"startup", "query", "backoff"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			ticks := make(chan time.Duration, 1)
			clk := clock.NewTestClockWithTickSignal(
				time.Now(), ticks,
			)
			s, rpc, _ := newCommitmentRepairServer(t, 2, clk)
			entered := make(chan struct{})
			rpc.onSend = func(ctx context.Context, _ int) error {
				if phase == "backoff" {
					return status.Error(
						codes.Unavailable, "offline",
					)
				}
				close(entered)
				<-ctx.Done()

				return ctx.Err()
			}
			stop := s.startLegacyCommitmentHeightRepair(
				t.Context(), fastCommitmentRepairPage,
			)
			t.Cleanup(stop)
			if phase != "startup" {
				s.markDaemonReady()
				if phase == "backoff" {
					select {
					case <-ticks:
					case <-time.After(5 * time.Second):
						t.Fatal(
							"no repair cooldown",
						)
					}
				} else {
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("repair did not query")
					}
				}
			}
			stopped := make(chan struct{})
			go func() {
				stop()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("repair did not stop")
			}
			if phase == "startup" {
				require.Empty(t, rpc.requestKeys())
			}
			if phase == "query" {
				require.Len(t, rpc.requestKeys(), 1)
			}
		})
	}
}

// assertHeightRepairComplete checks the durable inventory after maintenance.
func assertHeightRepairComplete(t *testing.T, s *Server,
	descs []*vtxo.Descriptor) {

	t.Helper()
	for _, desc := range descs {
		reloaded, err := s.vtxoStore.GetVTXO(t.Context(), desc.Outpoint)
		require.NoError(t, err)
		require.False(t, hasUnknownCommitmentHeight(reloaded))
	}
}

// TestLegacyCommitmentHeightRepairBackoff bounds persistent polling while
// durable progress resets the cooldown and eventual recovery completes.
func TestLegacyCommitmentHeightRepairBackoff(t *testing.T) {
	t.Parallel()
	ticks := make(chan time.Duration, 1)
	clk := clock.NewTestClockWithTickSignal(time.Now(), ticks)
	s, rpc, descs := newCommitmentRepairServer(t, 3, clk)
	logs := make(commitmentRepairLog, 20)
	s.log = btclog.NewSLogger(btclog.NewDefaultHandler(logs))
	var mode atomic.Int32
	rpc.onSend = func(_ context.Context, _ int) error {
		if mode.Load() == 2 || mode.CompareAndSwap(1, 0) {
			return nil
		}

		return status.Error(codes.Unavailable, "offline")
	}
	stop := s.startLegacyCommitmentHeightRepair(
		t.Context(), fastCommitmentRepairPage,
	)
	t.Cleanup(stop)
	s.markDaemonReady()
	for i, minutes := range []int{1, 2, 4, 8, 16, 32, 60, 60} {
		delay := awaitHeightRepairTick(t, ticks)
		require.Equal(t, time.Duration(minutes)*time.Minute, delay)
		// The last capped delay is followed by one successful target.
		if i == 7 {
			mode.Store(1)
		}
		clk.SetTime(clk.Now().Add(delay))
	}
	delay := awaitHeightRepairTick(t, ticks)
	require.Equal(t, time.Minute, delay)
	mode.Store(2)
	clk.SetTime(clk.Now().Add(delay))
	awaitHeightRepairCompletion(t, logs)
	stop()
	assertHeightRepairComplete(t, s, descs)
}

// TestLegacyCommitmentHeightRepairSetupBackoff covers failures before target
// queries, which must follow the same growing cooldown as per-target failures.
func TestLegacyCommitmentHeightRepairSetupBackoff(t *testing.T) {
	t.Parallel()
	ticks := make(chan time.Duration, 1)
	clk := clock.NewTestClockWithTickSignal(time.Now(), ticks)
	s, rpc, _ := newCommitmentRepairServer(t, 1, clk)
	s.chainBackend = nil
	stop := s.startLegacyCommitmentHeightRepair(
		t.Context(), fastCommitmentRepairPage,
	)
	t.Cleanup(stop)
	s.markDaemonReady()
	delay := awaitHeightRepairTick(t, ticks)
	require.Equal(t, time.Minute, delay)
	clk.SetTime(clk.Now().Add(delay))
	require.Equal(t, 2*time.Minute, awaitHeightRepairTick(t, ticks))
	stop()
	require.Empty(t, rpc.requestKeys())
}

// TestLegacyCommitmentHeightRepairIdle skips the migration completion signal
// and indexer dependencies on fresh wallets and subsequent repaired boots.
func TestLegacyCommitmentHeightRepairIdle(t *testing.T) {
	t.Parallel()
	for _, count := range []int{0, 1} {
		s, rpc, descs := newCommitmentRepairServer(
			t, count,
			clock.NewTestClock(
				time.Now(),
			),
		)
		if count > 0 {
			result, err := s.repairLegacyCommitmentHeights(
				t.Context(), fastCommitmentRepairPage,
			)
			require.NoError(t, err)
			require.Equal(t, count, result.completed)
			assertHeightRepairComplete(t, s, descs)
		}
		before := len(rpc.requestKeys())
		logs := make(commitmentRepairLog, 10)
		s.log = btclog.NewSLogger(btclog.NewDefaultHandler(logs))
		s.chainBackend = nil
		s.indexer = nil
		s.markDaemonReady()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		s.runLegacyCommitmentHeightRepair(ctx, fastCommitmentRepairPage)
		require.NoError(t, ctx.Err())
		cancel()
		require.Len(t, rpc.requestKeys(), before)
		require.Empty(t, logs)
	}
}

// TestLegacyCommitmentHeightRepairConcurrentBackfill treats a zero-update
// atomic backfill as completion only because another writer repaired the row.
func TestLegacyCommitmentHeightRepairConcurrentBackfill(t *testing.T) {
	t.Parallel()
	s, rpc, descs := newCommitmentRepairServer(
		t, 1,
		clock.NewTestClock(
			time.Now(),
		),
	)
	logs := make(commitmentRepairLog, 10)
	s.log = btclog.NewSLogger(btclog.NewDefaultHandler(logs))
	desc := descs[0]
	rpc.onSend = func(ctx context.Context, _ int) error {
		indexed, err := vtxo.IndexedAncestryFromRPC(
			rpc.byScript[string(desc.PkScript)],
		)
		if err != nil {
			return err
		}
		n, err := s.vtxoStore.BackfillVTXOCommitmentHeights(
			ctx, desc.Outpoint, indexed, 500,
		)
		require.Equal(t, 1, n)

		return err
	}
	s.markDaemonReady()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	s.runLegacyCommitmentHeightRepair(ctx, fastCommitmentRepairPage)
	require.NoError(t, ctx.Err())
	require.Len(t, rpc.requestKeys(), 1)
	// An additional worker write would log progress before completion.
	// Inspect every record after the synchronous worker has returned.
	require.Len(t, logs, 1)
	require.Contains(t, <-logs, "repair complete")
	assertHeightRepairComplete(t, s, descs)
}

// awaitHeightRepairTick bounds waits for the next fake-clock cooldown.
func awaitHeightRepairTick(t *testing.T,
	ticks <-chan time.Duration) time.Duration {

	t.Helper()
	select {
	case delay := <-ticks:
		return delay

	case <-time.After(5 * time.Second):
		t.Fatal("repair did not schedule a cooldown")

		return 0
	}
}

// awaitHeightRepairCompletion waits for the migration completion signal.
func awaitHeightRepairCompletion(t *testing.T, logs <-chan string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case record := <-logs:
			if strings.Contains(record, "repair complete") {
				return
			}

		case <-deadline:
			t.Fatal("repair never completed")
		}
	}
}
