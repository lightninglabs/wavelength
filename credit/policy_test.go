package credit

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// scriptedFloorDaemon records floor reads and can fail an initial prefix of
// calls before returning the configured floor.
type scriptedFloorDaemon struct {
	*fakeDaemon

	calls     atomic.Int32
	failCalls int32
	floor     uint64
}

// VTXOFloor returns the scripted transient failures and eventual floor.
func (d *scriptedFloorDaemon) VTXOFloor(context.Context) (uint64, error) {
	call := d.calls.Add(1)
	if call <= d.failCalls {
		return 0, context.DeadlineExceeded
	}

	return d.floor, nil
}

// recordingCreditRef captures policy messages without starting an actor.
type recordingCreditRef struct {
	messages chan CreditMsg
}

// ID returns the stable test reference id.
func (r *recordingCreditRef) ID() string {
	return "recording-credit-ref"
}

// Tell captures one policy message or returns caller cancellation.
func (r *recordingCreditRef) Tell(ctx context.Context, msg CreditMsg) error {
	select {
	case r.messages <- msg:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryTell captures without parking when the test channel is full.
func (r *recordingCreditRef) TryTell(ctx context.Context, msg CreditMsg) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case r.messages <- msg:
		return nil

	default:
		return actor.ErrMailboxFull
	}
}

// TestRedeemWatermarkCleared asserts the receive-driven auto-redeem watermark
// check: a settled receive triggers a redeem only when auto-redeem is enabled
// and the earmark-adjusted available balance reaches the threshold. The
// no-pending-pay/redeem interlock is applied separately by the registry.
func TestRedeemWatermarkCleared(t *testing.T) {
	t.Parallel()

	const threshold = 354

	earmarkOf := func(v uint64) *atomic.Pointer[EarmarkFunc] {
		var p atomic.Pointer[EarmarkFunc]
		var fn EarmarkFunc = func(context.Context) (uint64, error) {
			return v, nil
		}
		p.Store(&fn)

		return &p
	}
	earmarkErr := func() *atomic.Pointer[EarmarkFunc] {
		var p atomic.Pointer[EarmarkFunc]
		var fn EarmarkFunc = func(context.Context) (uint64, error) {
			return 0, context.DeadlineExceeded
		}
		p.Store(&fn)

		return &p
	}

	cases := []struct {
		name      string
		enabled   bool
		available uint64
		earmark   *atomic.Pointer[EarmarkFunc]
		wantAmt   uint64
		wantOK    bool
	}{
		{
			name:      "above threshold no earmark",
			enabled:   true,
			available: 1000,
			wantAmt:   1000,
			wantOK:    true,
		},
		{
			name:      "at threshold",
			enabled:   true,
			available: threshold,
			wantAmt:   threshold,
			wantOK:    true,
		},
		{
			name:      "below threshold",
			enabled:   true,
			available: 100,
			wantOK:    false,
		},
		{
			name:      "disabled never redeems",
			enabled:   false,
			available: 1000,
			wantOK:    false,
		},
		{
			name:      "earmark drops below threshold",
			enabled:   true,
			available: 1000,
			earmark:   earmarkOf(800),
			wantOK:    false,
		},
		{
			name:      "earmark leaves headroom",
			enabled:   true,
			available: 1000,
			earmark:   earmarkOf(200),
			wantAmt:   800,
			wantOK:    true,
		},
		{
			name:      "earmark error fails safe",
			enabled:   true,
			available: 1000,
			earmark:   earmarkErr(),
			wantOK:    false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := &opBehavior{
				cfg: OpActorConfig{
					OpID:              "op",
					AutoRedeemEnabled: tc.enabled,
					MinRedeemSat:      threshold,
					Daemon:            newFakeDaemon(),
					Earmark:           tc.earmark,
				},
				log: btclog.Disabled,
			}

			amt, ok := b.redeemWatermarkCleared(
				context.Background(), &CreditSnapshot{
					AvailableSat: tc.available,
				},
			)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.wantAmt, amt)
		})
	}
}

// TestRedeemWatermarkHonorsLiveOperatorFloor verifies a lower configured
// watermark cannot authorize a redemption below the current VTXO minimum.
func TestRedeemWatermarkHonorsLiveOperatorFloor(t *testing.T) {
	t.Parallel()

	b := &opBehavior{
		cfg: OpActorConfig{
			OpID:              "op",
			AutoRedeemEnabled: true,
			MinRedeemSat:      354,
			Daemon: &fakeDaemon{
				vtxoFloor: 1_000,
			},
		},
		log: btclog.Disabled,
	}

	amount, ok := b.redeemWatermarkCleared(
		t.Context(), &CreditSnapshot{
			AvailableSat: 999,
		},
	)
	require.False(t, ok)
	require.Zero(t, amount)

	amount, ok = b.redeemWatermarkCleared(
		t.Context(), &CreditSnapshot{
			AvailableSat: 1_000,
		},
	)
	require.True(t, ok)
	require.Equal(t, uint64(1_000), amount)
}

// TestRedeemWatermarkFailsClosedWithoutOperatorFloor verifies configured
// policy never substitutes for unavailable live operator terms.
func TestRedeemWatermarkFailsClosedWithoutOperatorFloor(t *testing.T) {
	t.Parallel()

	b := &opBehavior{
		cfg: OpActorConfig{
			OpID:              "op",
			AutoRedeemEnabled: true,
			MinRedeemSat:      354,
			Daemon: &fakeDaemon{
				vtxoFloorErr: context.DeadlineExceeded,
			},
		},
		log: btclog.Disabled,
	}

	amount, ok := b.redeemWatermarkCleared(
		t.Context(), &CreditSnapshot{
			AvailableSat: 10_000,
		},
	)
	require.False(t, ok)
	require.Zero(t, amount)
}

// TestRedeemWatermarkSkipsFloorBelowConfiguredMinimum verifies a cheap local
// watermark rejection does not put an operator round trip on the receive turn.
func TestRedeemWatermarkSkipsFloorBelowConfiguredMinimum(t *testing.T) {
	t.Parallel()

	daemon := &scriptedFloorDaemon{
		fakeDaemon: newFakeDaemon(),
		floor:      1_000,
	}
	b := &opBehavior{
		cfg: OpActorConfig{
			OpID:              "op",
			AutoRedeemEnabled: true,
			MinRedeemSat:      2_000,
			Daemon:            daemon,
		},
		log: btclog.Disabled,
	}

	amount, ok := b.redeemWatermarkCleared(
		t.Context(), &CreditSnapshot{
			AvailableSat: 1_999,
		},
	)
	require.False(t, ok)
	require.Zero(t, amount)
	require.Zero(t, daemon.calls.Load())
}

// TestRedeemWatermarkRejectsZeroFloor verifies the policy boundary remains
// fail-closed even when a future daemon implementation returns zero and nil.
func TestRedeemWatermarkRejectsZeroFloor(t *testing.T) {
	t.Parallel()

	daemon := &scriptedFloorDaemon{
		fakeDaemon: newFakeDaemon(),
	}
	b := &opBehavior{
		cfg: OpActorConfig{
			OpID:              "op",
			AutoRedeemEnabled: true,
			Daemon:            daemon,
		},
		log: btclog.Disabled,
	}

	amount, ok := b.redeemWatermarkCleared(
		t.Context(), &CreditSnapshot{
			AvailableSat: 1_000,
		},
	)
	require.False(t, ok)
	require.Zero(t, amount)
	require.Equal(t, int32(1), daemon.calls.Load())
}

// TestBootAutoRedeemRetriesTransientFloorFailure verifies startup keeps the one
// uncovered balance case live until a complete reconciliation succeeds.
func TestBootAutoRedeemRetriesTransientFloorFailure(t *testing.T) {
	t.Parallel()

	server := newFakeServer()
	server.available = 1_000
	daemon := &scriptedFloorDaemon{
		fakeDaemon: newFakeDaemon(),
		failCalls:  1,
		floor:      1_000,
	}
	registry := &recordingCreditRef{
		messages: make(chan CreditMsg, 1),
	}
	redeemer := &autoRedeemer{
		cfg: AutoRedeemConfig{
			Enabled: true,
		},
		server:   server,
		daemon:   daemon,
		registry: registry,
		log:      btclog.Disabled,
		retry:    5 * time.Millisecond,
	}

	redeemer.start(t.Context())
	t.Cleanup(redeemer.stop)

	select {
	case msg := <-registry.messages:
		request, ok := msg.(*ConsiderRedeemRequest)
		require.True(t, ok)
		require.Equal(t, uint64(1_000), request.AvailableSat)
		require.Equal(t, int32(2), daemon.calls.Load())

	case <-time.After(time.Second):
		t.Fatal(
			"boot auto-redeem did not recover from transient " +
				"floor error",
		)
	}
}

// TestRedeemOpKeyUnique asserts redeem op keys are prefixed and random.
func TestRedeemOpKeyUnique(t *testing.T) {
	t.Parallel()

	a, err := redeemOpKey()
	require.NoError(t, err)
	b, err := redeemOpKey()
	require.NoError(t, err)

	require.Contains(t, a, "redeem:")
	require.NotEqual(t, a, b)
}
