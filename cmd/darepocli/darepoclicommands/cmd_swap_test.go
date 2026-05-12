//go:build swapdirect && !swapruntime

package darepoclicommands

import (
	"encoding/hex"
	"testing"

	"github.com/lightninglabs/darepo-client/sdk/swaps"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestPaymentHashFromArgsOrFlagAcceptsPositionalHash verifies that resume can
// be driven by the shorter `swap resume <payment_hash>` form.
func TestPaymentHashFromArgsOrFlagAcceptsPositionalHash(t *testing.T) {
	t.Parallel()

	hash := testPaymentHash(1)
	cmd := newSwapResumeCmd()

	parsed, err := paymentHashFromArgsOrFlag(
		cmd, []string{
			hex.EncodeToString(hash[:]),
		},
	)
	require.NoError(t, err)
	require.Equal(t, hash, parsed)
}

// TestPaymentHashFromArgsOrFlagRejectsAmbiguousInput verifies that users do
// not accidentally resume a different swap than the positional argument names.
func TestPaymentHashFromArgsOrFlagRejectsAmbiguousInput(t *testing.T) {
	t.Parallel()

	hash := testPaymentHash(2)
	cmd := newSwapResumeCmd()
	require.NoError(
		t,
		cmd.Flags().Set(
			"payment_hash",
			hex.EncodeToString(hash[:]),
		),
	)

	_, err := paymentHashFromArgsOrFlag(
		cmd, []string{
			hex.EncodeToString(hash[:]),
		},
	)
	require.ErrorContains(t, err, "set as argument and flag")
}

// TestInferResumeDirectionReturnsUniquePendingDirection verifies that the CLI
// can resume by payment hash alone when exactly one pending row matches.
func TestInferResumeDirectionReturnsUniquePendingDirection(t *testing.T) {
	t.Parallel()

	hash := testPaymentHash(3)
	direction, err := inferResumeDirection(hash, []swaps.SwapSummary{
		{
			Direction:   swaps.SwapDirectionPay,
			PaymentHash: testPaymentHash(4),
			Pending:     true,
		},
		{
			Direction:   swaps.SwapDirectionReceive,
			PaymentHash: hash,
			Pending:     true,
		},
	})
	require.NoError(t, err)
	require.Equal(t, swaps.SwapDirectionReceive, direction)
}

// TestInferResumeDirectionIgnoresTerminalRows verifies that resume-by-hash only
// considers sessions that can still make progress.
func TestInferResumeDirectionIgnoresTerminalRows(t *testing.T) {
	t.Parallel()

	hash := testPaymentHash(5)
	_, err := inferResumeDirection(hash, []swaps.SwapSummary{
		{
			Direction:   swaps.SwapDirectionPay,
			PaymentHash: hash,
			Pending:     false,
		},
	})
	require.ErrorContains(t, err, "no pending swap")
}

// TestInferResumeDirectionRejectsAmbiguousPendingRows verifies that the user
// must specify --direction if corrupted or hand-edited state contains both
// directions for one hash.
func TestInferResumeDirectionRejectsAmbiguousPendingRows(t *testing.T) {
	t.Parallel()

	hash := testPaymentHash(6)
	_, err := inferResumeDirection(hash, []swaps.SwapSummary{
		{
			Direction:   swaps.SwapDirectionPay,
			PaymentHash: hash,
			Pending:     true,
		},
		{
			Direction:   swaps.SwapDirectionReceive,
			PaymentHash: hash,
			Pending:     true,
		},
	})
	require.ErrorContains(t, err, "multiple pending")
}

// TestIsLocalSwapServerAddr verifies that the CLI keeps the plaintext default
// limited to local development addresses.
func TestIsLocalSwapServerAddr(t *testing.T) {
	t.Parallel()

	require.True(t, isLocalSwapServerAddr("localhost:10030"))
	require.True(t, isLocalSwapServerAddr("127.0.0.1:10030"))
	require.True(t, isLocalSwapServerAddr("[::1]:10030"))
	require.True(t, isLocalSwapServerAddr("unix:///tmp/swap.sock"))
	require.False(t, isLocalSwapServerAddr("example.com:10030"))
}

// testPaymentHash returns a deterministic payment hash for CLI unit tests.
func testPaymentHash(seed byte) lntypes.Hash {
	var hash lntypes.Hash
	for i := range hash {
		hash[i] = seed
	}

	return hash
}
