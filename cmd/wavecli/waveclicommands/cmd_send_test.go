package waveclicommands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestConfirmSendIfNeededRefusesNonTTY(t *testing.T) {
	prev := stdinIsTTY
	stdinIsTTY = func(*cobra.Command) bool { return false }
	t.Cleanup(func() {
		stdinIsTTY = prev
	})

	cmd := newSendCmd()
	err := confirmSendIfNeeded(cmd, &walletdkrpc.PrepareSendResponse{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "non-interactive stdin")
}

func TestConfirmSendIfNeededForceSkipsPrompt(t *testing.T) {
	prev := stdinIsTTY
	stdinIsTTY = func(*cobra.Command) bool { return false }
	t.Cleanup(func() {
		stdinIsTTY = prev
	})

	cmd := newSendCmd()
	require.NoError(t, cmd.Flags().Set("force", "true"))

	err := confirmSendIfNeeded(cmd, &walletdkrpc.PrepareSendResponse{})
	require.NoError(t, err)
}

func TestPromptSendConfirmationAcceptsYes(t *testing.T) {
	cmd := newSendCmd()
	cmd.SetIn(strings.NewReader("yes\n"))

	var stderr bytes.Buffer
	cmd.SetErr(&stderr)

	err := promptSendConfirmation(cmd, &walletdkrpc.PrepareSendResponse{
		AmountSat:               50_000,
		ExpectedFeeSat:          123,
		FeeKnown:                true,
		ExpectedTotalOutflowSat: 50_123,
		TotalOutflowKnown:       true,
		Rail: walletdkrpc.
			SendRail_SEND_RAIL_LIGHTNING,
		DestinationSummary: "lnbc...",
		PaymentHash:        "abcd",
	})
	require.NoError(t, err)
	require.Contains(t, stderr.String(), "Send 50000 sats")
	require.Contains(t, stderr.String(), "Proceed? [y/N]:")
}

func TestPromptSendConfirmationDisplaysQuoteDetails(t *testing.T) {
	cmd := newSendCmd()
	cmd.SetIn(strings.NewReader("yes\n"))

	var stderr bytes.Buffer
	cmd.SetErr(&stderr)

	err := promptSendConfirmation(cmd, &walletdkrpc.PrepareSendResponse{
		AmountSat:               50_000,
		ExpectedFeeSat:          123,
		FeeKnown:                true,
		ExpectedTotalOutflowSat: 50_123,
		TotalOutflowKnown:       true,
		Rail: walletdkrpc.
			SendRail_SEND_RAIL_LIGHTNING,
		DestinationSummary: "lnbcrt...",
		InvoiceDescription: "coffee",
		PaymentHash:        "abcd",
		Warning:            "quoted fee exceeds max_fee_sat",
	})
	require.NoError(t, err)

	output := stderr.String()
	require.Contains(t, output, "Send 50000 sats")
	require.Contains(t, output, "Rail: Lightning")
	require.Contains(t, output, "Expected fee: 123 sats")
	require.Contains(t, output, "Expected total outflow: 50123 sats")
	require.Contains(t, output, "Destination: lnbcrt...")
	require.Contains(t, output, "Invoice: coffee")
	require.Contains(t, output, "Payment hash: abcd")
	require.Contains(t, output, "Warning: quoted fee exceeds max_fee_sat")
}

func TestPromptSendConfirmationUsesSweepOutflow(t *testing.T) {
	cmd := newSendCmd()
	cmd.SetIn(strings.NewReader("yes\n"))

	var stderr bytes.Buffer
	cmd.SetErr(&stderr)

	err := promptSendConfirmation(cmd, &walletdkrpc.PrepareSendResponse{
		AmountSat:               0,
		ExpectedTotalOutflowSat: 12_000,
		TotalOutflowKnown:       true,
		Rail: walletdkrpc.
			SendRail_SEND_RAIL_ONCHAIN,
	})
	require.NoError(t, err)
	require.Contains(t, stderr.String(), "Send 12000 sats")
	require.NotContains(t, stderr.String(), "Send 0 sats")
}

func TestPromptSendConfirmationDefaultsNo(t *testing.T) {
	cmd := newSendCmd()
	cmd.SetIn(strings.NewReader("\n"))

	err := promptSendConfirmation(cmd, &walletdkrpc.PrepareSendResponse{})
	require.ErrorContains(t, err, "aborted by user")
}

// TestSendWaitFlagDefaults verifies the wait family is wired with the expected
// defaults: `send` blocks on settlement by default and --no-wait opts out, so a
// caller gets the preimage without further tuning.
func TestSendWaitFlagDefaults(t *testing.T) {
	cmd := newSendCmd()

	noWait, err := cmd.Flags().GetBool("no-wait")
	require.NoError(t, err)
	require.False(t, noWait)

	timeout, err := cmd.Flags().GetDuration("wait-timeout")
	require.NoError(t, err)
	require.Equal(t, defaultSendWaitTimeout, timeout)

	interval, err := cmd.Flags().GetDuration("wait-poll-interval")
	require.NoError(t, err)
	require.Equal(t, defaultSendWaitPollInterval, interval)
}

// TestEmptyNonEmpty verifies the terse failure-reason fallback used by the
// wait renderer.
func TestEmptyNonEmpty(t *testing.T) {
	require.Equal(t, "fallback", emptyNonEmpty("   ", "fallback"))
	require.Equal(t, "reason", emptyNonEmpty("reason", "fallback"))
}
