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
