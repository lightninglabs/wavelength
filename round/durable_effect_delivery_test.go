package round

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// effectFailureRef fails one enqueue while retaining delivery order for
// assertions.
type effectFailureRef[M actor.Message] struct {
	messages []M
	failAt   int
	err      error
}

// ID identifies the receiver used by the effect test.
func (*effectFailureRef[M]) ID() string { return "effect-target" }

// Tell fails the selected enqueue attempt before accepting later retries.
func (r *effectFailureRef[M]) Tell(_ context.Context, msg M) error {
	r.messages = append(r.messages, msg)
	if len(r.messages) == r.failAt {
		return r.err
	}

	return nil
}

// TryTell preserves the same deterministic failure behavior.
func (r *effectFailureRef[M]) TryTell(ctx context.Context, msg M) error {
	return r.Tell(ctx, msg)
}

// TestDurableLedgerDeliveryRetries retains identities after partial delivery.
func TestDurableLedgerDeliveryRetries(t *testing.T) {
	failed := errors.New("ledger mailbox unavailable")
	sink := &effectFailureRef[ledger.LedgerMsg]{failAt: 2, err: failed}
	owner := &RoundClientActor{
		cfg: &RoundClientConfig{
			LedgerSink: fn.Some[ledger.Sink](sink),
		},
		env: &ClientEnvironment{}, log: btclog.Disabled,
	}
	effect, err := newDurableClientEffect(&VTXOCreatedNotification{
		RoundID: RoundID{1}.String(),
		VTXOs: []*ClientVTXO{{
			Outpoint: wire.OutPoint{Index: 7}, Amount: 123,
			Origin: types.VTXOOriginRoundRefresh,
		}},
		OperatorFeeSat: 5,
	}, time.Now())
	require.NoError(t, err)
	require.ErrorIs(
		t,
		owner.deliverDurableEffect(
			t.Context(), effect,
		),
		failed,
	)
	require.Len(t, sink.messages, 2)
	require.NoError(t, owner.deliverDurableEffect(t.Context(), effect))
	require.Len(t, sink.messages, 5)
	require.Equal(t, sink.messages[0], sink.messages[2])
	require.Equal(t, sink.messages[1], sink.messages[3])
	require.IsType(t, &ledger.FeePaidMsg{}, sink.messages[4])
}

// TestDurableReservationReleaseFailure remains visible to the durable runtime.
func TestDurableReservationReleaseFailure(t *testing.T) {
	failed := errors.New("manager unavailable")
	sink := &effectFailureRef[VTXOManagerMsg]{
		failAt: 1,
		err:    failed,
	}
	owner := &RoundClientActor{
		cfg: &RoundClientConfig{
			VTXOManager: sink,
		},
		env: &ClientEnvironment{}, log: btclog.Disabled,
	}
	effect, err := newDurableClientEffect(&ReleaseForfeitReservation{
		Outpoints: []wire.OutPoint{{Index: 7}},
	}, time.Now())
	require.NoError(t, err)
	require.ErrorIs(
		t,
		owner.deliverDurableEffect(
			t.Context(), effect,
		),
		failed,
	)
	require.NoError(t, owner.deliverDurableEffect(t.Context(), effect))
	require.Len(t, sink.messages, 2)
	require.Equal(t, sink.messages[0], sink.messages[1])
}
