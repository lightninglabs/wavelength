package vtxo

import (
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
