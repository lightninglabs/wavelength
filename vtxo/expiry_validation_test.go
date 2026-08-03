package vtxo

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHasUsableBatchExpiry asserts which descriptors carry an expiry that an
// expiry decision may be based on. Every rejected shape would otherwise be
// read as a deadline that has already passed, because all expiry arithmetic
// is `BatchExpiry - currentHeight`.
func TestHasUsableBatchExpiry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		batchExpiry   int32
		createdHeight int32
		nilDescriptor bool
		expectUsable  bool
	}{
		{
			name:          "usable expiry",
			batchExpiry:   1000,
			createdHeight: 100,
			expectUsable:  true,
		},
		{
			name:          "usable without created height",
			batchExpiry:   1000,
			createdHeight: 0,
			expectUsable:  true,
		},
		{
			name:          "zero expiry",
			batchExpiry:   0,
			createdHeight: 100,
			expectUsable:  false,
		},
		{
			name:          "negative expiry",
			batchExpiry:   -1,
			createdHeight: 100,
			expectUsable:  false,
		},
		{
			name:          "expires before it was created",
			batchExpiry:   50,
			createdHeight: 100,
			expectUsable:  false,
		},
		{
			name:          "expires exactly at creation",
			batchExpiry:   100,
			createdHeight: 100,
			expectUsable:  true,
		},
		{
			name:          "nil descriptor",
			nilDescriptor: true,
			expectUsable:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var desc *Descriptor
			if !tc.nilDescriptor {
				desc = &Descriptor{
					BatchExpiry:   tc.batchExpiry,
					CreatedHeight: tc.createdHeight,
				}
			}

			require.Equal(
				t, tc.expectUsable, HasUsableBatchExpiry(desc),
			)
		})
	}
}

// TestCheckExpiryUnusableBatchExpiry asserts that an untrustworthy expiry
// yields ExpiryStatusUnknown rather than ExpiryStatusExpired. Reporting
// "expired" here is the dangerous direction: it would surrender live funds to
// the expiry path on the strength of a corrupt field.
func TestCheckExpiryUnusableBatchExpiry(t *testing.T) {
	t.Parallel()

	cfg := DefaultExpiryConfig()

	t.Run("zero expiry is unknown not expired", func(t *testing.T) {
		t.Parallel()

		desc := &Descriptor{BatchExpiry: 0, CreatedHeight: 100}

		status := cfg.CheckExpiry(desc, 200)
		require.Equal(t, ExpiryStatusUnknown, status)
		require.NotEqual(t, ExpiryStatusExpired, status)
	})

	t.Run("expiry before creation is unknown", func(t *testing.T) {
		t.Parallel()

		desc := &Descriptor{BatchExpiry: 50, CreatedHeight: 100}

		require.Equal(
			t, ExpiryStatusUnknown, cfg.CheckExpiry(desc, 200),
		)
	})

	t.Run("real expiry still reports expired", func(t *testing.T) {
		t.Parallel()

		desc := &Descriptor{BatchExpiry: 150, CreatedHeight: 100}

		// The control: a trustworthy expiry that has genuinely passed
		// must still classify as expired, so the guard above is not
		// swallowing real deadlines.
		require.Equal(
			t, ExpiryStatusExpired, cfg.CheckExpiry(desc, 200),
		)
	})
}

// TestExpiryStatusUnknownString asserts the new status renders, so log lines
// and the daemon RPC mapping do not surface a bare integer.
func TestExpiryStatusUnknownString(t *testing.T) {
	t.Parallel()

	require.Equal(t, "unknown", ExpiryStatusUnknown.String())
}

// TestShouldWaitForFreeRefreshWindow verifies cohorting preserves a safe,
// configured future waiver while disabled or too-late windows do not delay
// maintenance.
func TestShouldWaitForFreeRefreshWindow(t *testing.T) {
	t.Parallel()

	vtxo := &Descriptor{
		BatchExpiry:    1_000,
		CreatedHeight:  100,
		RelativeExpiry: 10,
	}

	tests := []struct {
		name     string
		window   uint32
		height   int32
		wantWait bool
	}{
		{
			name:     "safe future window",
			window:   120,
			height:   800,
			wantWait: true,
		},
		{
			name:     "inside window",
			window:   120,
			height:   880,
			wantWait: false,
		},
		{
			name:     "disabled window",
			window:   0,
			height:   800,
			wantWait: false,
		},
		{
			name:     "unsafe late window",
			window:   40,
			height:   800,
			wantWait: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := &ExpiryConfig{
				CriticalThresholdBlocks: 30,
				MinRefreshBuffer:        50,
				TreeDepthMultiplier:     1,
				FreeRefreshWindow: func() uint32 {
					return test.window
				},
			}

			require.Equal(
				t, test.wantWait,
				cfg.ShouldWaitForFreeRefreshWindow(
					vtxo, test.height,
				),
			)
		})
	}
}

// TestCriticalThresholdIncludesOORHops asserts that the critical threshold
// budgets time for the OOR checkpoint chain, not just the commitment tree.
//
// The critical threshold exists so the client never has to race the operator's
// sweep. An exit must confirm the deepest tree path AND one recovery
// transaction per OOR hop before its final CSV even starts, so omitting
// ChainDepth under-sizes exactly the deep OOR chains that need the most room.
func TestCriticalThresholdIncludesOORHops(t *testing.T) {
	t.Parallel()

	const (
		treeDepthMultiplier = int32(6)
		relativeExpiry      = uint32(144)
		chainDepth          = 4
	)

	cfg := &ExpiryConfig{
		// Floor kept low so the dynamic term is what is measured.
		CriticalThresholdBlocks: 1,
		RefreshThresholdBlocks:  1,
		MinRefreshBuffer:        1,
		TreeDepthMultiplier:     treeDepthMultiplier,
	}

	// Identical VTXOs except for the OOR hop count. Both carry a
	// single-fragment ancestry so MaxTreeDepth is equal.
	ancestry := []Ancestry{{TreeDepth: 3}}

	roundBorn := &Descriptor{
		Ancestry:       ancestry,
		RelativeExpiry: relativeExpiry,
		ChainDepth:     0,
	}
	oorDerived := &Descriptor{
		Ancestry:       ancestry,
		RelativeExpiry: relativeExpiry,
		ChainDepth:     chainDepth,
	}

	roundThreshold := cfg.CalculateCriticalThreshold(roundBorn)
	oorThreshold := cfg.CalculateCriticalThreshold(oorDerived)

	require.Greater(
		t, oorThreshold, roundThreshold, "an OOR-derived VTXO "+
			"needs a larger exit budget than an otherwise "+
			"identical round-born one",
	)
	require.Equal(
		t, roundThreshold+chainDepth*treeDepthMultiplier, oorThreshold,
		"each OOR hop must cost one recovery transaction of budget",
	)
}

// TestCriticalThresholdIgnoresNegativeChainDepth asserts that a corrupt hop
// count cannot shorten the exit budget below the round-born baseline.
func TestCriticalThresholdIgnoresNegativeChainDepth(t *testing.T) {
	t.Parallel()

	cfg := &ExpiryConfig{
		CriticalThresholdBlocks: 1,
		TreeDepthMultiplier:     6,
	}

	desc := &Descriptor{
		Ancestry: []Ancestry{
			{
				TreeDepth: 3,
			},
		},
		RelativeExpiry: 144,
		ChainDepth:     -5,
	}
	baseline := &Descriptor{
		Ancestry: []Ancestry{
			{
				TreeDepth: 3,
			},
		},
		RelativeExpiry: 144,
		ChainDepth:     0,
	}

	require.Equal(
		t, cfg.CalculateCriticalThreshold(baseline),
		cfg.CalculateCriticalThreshold(desc),
	)
}

// TestLiveStateBlockEpochUnusableExpiry asserts that a VTXO whose expiry
// cannot be trusted is held in LiveState: it is neither surrendered to the
// expiry path nor allowed to fail the transition, which would wedge the actor
// on every block.
func TestLiveStateBlockEpochUnusableExpiry(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 0
	vtxo.CreatedHeight = 100

	h.withState(&LiveState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	// A height far beyond any plausible expiry: with the old arithmetic
	// this VTXO would have been classified expired and retired.
	evt := h.newBlockEpochEvent(900_000)
	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	assertState[*LiveState](h)
	require.Empty(
		t, h.outboxMessages,
		"an untrustworthy expiry must not drive any state change",
	)
}

// TestLiveStateBlockEpochExpiredPersists asserts that reaching batch expiry
// persists the new status and lands in the non-terminal ExpiredState.
//
// The old behaviour returned a terminal FailedState with no outbox at all, so
// the row stayed Live: the VTXO kept counting toward spendable balance, kept
// being offered to coin selection, and was recovered as Live on every restart
// only to re-fail on the next block.
func TestLiveStateBlockEpochExpiredPersists(t *testing.T) {
	t.Parallel()

	const expiryHeight = int32(1_000)

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = expiryHeight
	vtxo.CreatedHeight = 100

	h.withState(&LiveState{
		VTXO:              vtxo,
		LastCheckedHeight: 900,
	})

	_, err := h.sendEvent(h.newBlockEpochEvent(expiryHeight))
	require.NoError(t, err)

	state := assertState[*ExpiredState](h)
	require.Equal(t, expiryHeight, state.ObservedHeight)

	// Expiry must be durable, and the actor must not be reaped: the value
	// is still recoverable by forfeiting this VTXO in an ordinary round.
	require.False(
		t, state.IsTerminal(),
		"expired VTXOs stay recoverable, so the actor must live on",
	)

	var sawStatusUpdate bool
	for _, msg := range h.outboxMessages {
		switch typed := msg.(type) {
		case *VTXOStatusUpdate:
			require.Equal(t, VTXOStatusExpired, typed.NewStatus)
			sawStatusUpdate = true

		case *VTXOTerminatedNotification:
			t.Fatal("expired VTXO must not be reaped")
		}
	}
	require.True(
		t, sawStatusUpdate,
		"expiry must be persisted, not held in memory only",
	)
}

// TestExpiredStateRefusesUnilateralExit asserts that an expired VTXO declines
// to start an exit. Completing one means confirming the whole ancestry and
// then waiting out the exit CSV while racing an operator whose sweep is
// already spendable, so it burns fees on an exit that cannot land.
func TestExpiredStateRefusesUnilateralExit(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 1_000

	h.withState(&ExpiredState{VTXO: vtxo, ObservedHeight: 1_000})

	_, err := h.sendEvent(&ForceUnrollEvent{Reason: "manual"})
	require.NoError(t, err)

	assertState[*ExpiredState](h)
	require.Empty(
		t, h.outboxMessages,
		"an expired VTXO must not hand itself to the chain resolver",
	)
}

// TestExpiredStateAcceptsForfeitForReclaim asserts that an expired VTXO can
// still be committed to a round, which is the whole recovery path: a reclaim
// is an ordinary refresh whose input happens to be expired.
func TestExpiredStateAcceptsForfeitForReclaim(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 1_000

	h.withState(&ExpiredState{VTXO: vtxo, ObservedHeight: 1_000})

	_, err := h.sendEvent(&PendingForfeitEvent{})
	require.NoError(t, err)

	assertState[*PendingForfeitState](h)

	var sawStatusUpdate bool
	for _, msg := range h.outboxMessages {
		if typed, ok := msg.(*VTXOStatusUpdate); ok {
			require.Equal(
				t, VTXOStatusPendingForfeit, typed.NewStatus,
			)
			sawStatusUpdate = true
		}
	}
	require.True(t, sawStatusUpdate)
}

// TestExpiredStateBlockEpochQueuesRefresh asserts that an expired VTXO
// automatically enters the ordinary refresh flow as soon as the wallet has a
// synchronized chain height. No operator sweep observation or redeemability
// RPC is involved.
func TestExpiredStateBlockEpochQueuesRefresh(t *testing.T) {
	t.Parallel()

	const currentHeight = int32(1_005)

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 1_000

	h.withState(&ExpiredState{VTXO: vtxo, ObservedHeight: 1_000})

	_, err := h.sendEvent(h.newBlockEpochEvent(currentHeight))
	require.NoError(t, err)

	state := assertState[*PendingForfeitState](h)
	require.Equal(t, currentHeight, state.RequestedAtHeight)
	requireStatusUpdate(t, h, VTXOStatusPendingForfeit)

	var refresh *ForfeitRequest
	for _, msg := range h.outboxMessages {
		request, ok := msg.(*ForfeitRequest)
		if ok {
			refresh = request
		}
	}
	require.NotNil(t, refresh)
	require.Equal(t, vtxo.Outpoint, refresh.VTXOOutpoint)
	require.Equal(t, currentHeight, refresh.LastCheckedHeight)
}

// TestExpiredStateRefusesOrdinarySpend asserts an expired VTXO cannot be
// claimed for an out-of-round spend. There is nothing left to spend
// cooperatively until it has been reissued.
func TestExpiredStateRefusesOrdinarySpend(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 1_000

	h.withState(&ExpiredState{VTXO: vtxo, ObservedHeight: 1_000})

	_, err := h.sendEvent(&SpendReserveEvent{})
	require.NoError(t, err)

	assertState[*ExpiredState](h)
	require.Empty(t, h.outboxMessages)
}

// TestForfeitReleaseRestoresExpired asserts that releasing a reclaim's forfeit
// reservation rolls the VTXO back to Expired rather than Live.
//
// Restoring Live would put value the operator may already have swept back into
// spendable balance and coin selection. A payment funded from it would build
// on a dead lineage and the recipient would receive nothing. The
// in_flight-preserving sweep query only protects the success path; this covers
// the failure path.
func TestForfeitReleaseRestoresExpired(t *testing.T) {
	t.Parallel()

	const (
		batchExpiry = int32(1_000)
		pastExpiry  = batchExpiry + 5
	)

	t.Run("pending forfeit release", func(t *testing.T) {
		t.Parallel()

		h := newVTXOTestHarness(t)
		vtxo := h.newTestDescriptor()
		vtxo.BatchExpiry = batchExpiry

		h.withState(&PendingForfeitState{
			VTXO:              vtxo,
			RequestedAtHeight: pastExpiry,
		})

		_, err := h.sendEvent(&ForfeitReleasedEvent{})
		require.NoError(t, err)

		assertState[*ExpiredState](h)
		requireStatusUpdate(t, h, VTXOStatusExpired)
	})

	t.Run("forfeiting release", func(t *testing.T) {
		t.Parallel()

		h := newVTXOTestHarness(t)
		vtxo := h.newTestDescriptor()
		vtxo.BatchExpiry = batchExpiry

		h.withState(&ForfeitingState{
			VTXO:              vtxo,
			LastCheckedHeight: pastExpiry,
		})

		_, err := h.sendEvent(&ForfeitReleasedEvent{})
		require.NoError(t, err)

		assertState[*ExpiredState](h)
		requireStatusUpdate(t, h, VTXOStatusExpired)
	})

	t.Run("ordinary refresh still returns to live", func(t *testing.T) {
		t.Parallel()

		// The control: a released refresh of a VTXO that is nowhere
		// near expiry must not be quarantined.
		h := newVTXOTestHarness(t)
		vtxo := h.newTestDescriptor()
		vtxo.BatchExpiry = batchExpiry

		h.withState(&PendingForfeitState{
			VTXO:              vtxo,
			RequestedAtHeight: 100,
		})

		_, err := h.sendEvent(&ForfeitReleasedEvent{})
		require.NoError(t, err)

		assertState[*LiveState](h)
		requireStatusUpdate(t, h, VTXOStatusLive)
	})

	t.Run("unknown height returns to live", func(t *testing.T) {
		t.Parallel()

		// Without a height there is no evidence of expiry, so the
		// VTXO must not be quarantined. This is the restart path,
		// where the first block epoch reclassifies almost immediately.
		h := newVTXOTestHarness(t)
		vtxo := h.newTestDescriptor()
		vtxo.BatchExpiry = batchExpiry

		h.withState(&PendingForfeitState{
			VTXO:              vtxo,
			RequestedAtHeight: 0,
		})

		_, err := h.sendEvent(&ForfeitReleasedEvent{})
		require.NoError(t, err)

		assertState[*LiveState](h)
	})
}

// TestExpiredStateReturnsToLiveWhenNotExpired asserts ExpiredState is
// self-correcting. A VTXO can reach it without the chain having been
// consulted, so a block epoch proving the deadline has not passed must return
// it to the spendable set rather than stranding it.
func TestExpiredStateReturnsToLiveWhenNotExpired(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 100_000
	vtxo.CreatedHeight = 100

	h.withState(&ExpiredState{VTXO: vtxo, ObservedHeight: 0})

	_, err := h.sendEvent(h.newBlockEpochEvent(200))
	require.NoError(t, err)

	assertState[*LiveState](h)
	requireStatusUpdate(t, h, VTXOStatusLive)
}

// requireStatusUpdate asserts the outbox persisted exactly the given status.
func requireStatusUpdate(t *testing.T, h *vtxoTestHarness, want VTXOStatus) {
	t.Helper()

	for _, msg := range h.outboxMessages {
		if update, ok := msg.(*VTXOStatusUpdate); ok {
			require.Equal(t, want, update.NewStatus)

			return
		}
	}

	t.Fatalf("no VTXOStatusUpdate emitted, wanted %v", want)
}

// TestExpiredStateRejectsUnexpectedEvent asserts that a genuinely unexpected
// event surfaces as an error rather than being silently absorbed. A blanket
// default would let a real routing or lifecycle bug sit invisible for the
// whole life of an expired VTXO, which is long.
func TestExpiredStateRejectsUnexpectedEvent(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 1_000

	h.withState(&ExpiredState{VTXO: vtxo, ObservedHeight: 1_000})

	_, err := h.sendEvent(&ForfeitConfirmedEvent{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unexpected event")

	// The actor must survive the error rather than transitioning.
	assertState[*ExpiredState](h)
}

// TestExpiredStateAbsorbsStaleEvents asserts the enumerated stale events are
// still absorbed. They can legitimately arrive long after expiry from a path
// that ran before it.
func TestExpiredStateAbsorbsStaleEvents(t *testing.T) {
	t.Parallel()

	events := []VTXOEvent{
		&SpendReleasedEvent{},
		&SpendCompletedEvent{},
		&ForfeitReleasedEvent{},
		&ExitFailedEvent{},
		&ExitConfirmedEvent{},
	}

	for _, event := range events {
		t.Run(fmt.Sprintf("%T", event), func(t *testing.T) {
			t.Parallel()

			h := newVTXOTestHarness(t)
			vtxo := h.newTestDescriptor()
			vtxo.BatchExpiry = 1_000

			h.withState(&ExpiredState{
				VTXO:           vtxo,
				ObservedHeight: 1_000,
			})

			_, err := h.sendEvent(event)
			require.NoError(t, err)
			assertState[*ExpiredState](h)
		})
	}
}

// TestSpendingStateExpiredTerminates asserts that an outgoing spend whose
// batch expires mid-flight still terminates, and terminates correctly.
//
// SpendingState no longer escalates to unilateral exit on expiry, which is a
// broader change than reclaim: it affects every OOR send. The justification is
// that an exit started past the deadline cannot complete, so escalating would
// abandon a spend that may still settle. That is only sound if the spend has
// a terminating path either way, which is what this covers.
func TestSpendingStateExpiredTerminates(t *testing.T) {
	t.Parallel()

	const (
		batchExpiry = int32(1_000)
		pastExpiry  = batchExpiry + 5
	)

	newSpending := func(t *testing.T) *vtxoTestHarness {
		t.Helper()

		h := newVTXOTestHarness(t)
		vtxo := h.newTestDescriptor()
		vtxo.BatchExpiry = batchExpiry

		h.withState(&SpendingState{
			VTXO:              vtxo,
			LastCheckedHeight: pastExpiry,
		})

		return h
	}

	t.Run("block epoch does not escalate", func(t *testing.T) {
		t.Parallel()

		h := newSpending(t)

		_, err := h.sendEvent(h.newBlockEpochEvent(pastExpiry))
		require.NoError(t, err)

		// The in-flight OOR must be left alone to settle.
		assertState[*SpendingState](h)
		require.Empty(t, h.outboxMessages)
	})

	t.Run("completion still retires the VTXO", func(t *testing.T) {
		t.Parallel()

		h := newSpending(t)

		_, err := h.sendEvent(&SpendCompletedEvent{})
		require.NoError(t, err)

		assertState[*SpentState](h)
	})

	t.Run("release returns to expired not live", func(t *testing.T) {
		t.Parallel()

		h := newSpending(t)

		_, err := h.sendEvent(&SpendReleasedEvent{})
		require.NoError(t, err)

		// Releasing to Live would put value the operator may already
		// have swept back into coin selection.
		assertState[*ExpiredState](h)

		var released bool
		for _, msg := range h.outboxMessages {
			update, ok := msg.(*VTXOStatusUpdate)
			if !ok {
				continue
			}

			require.Equal(t, VTXOStatusExpired, update.NewStatus)

			// The spend reservation must still be cleared, or the
			// durable index would keep a stale row.
			require.True(t, update.ReleaseSpendReservation)
			released = true
		}
		require.True(t, released, "expected a status update")
	})
}
