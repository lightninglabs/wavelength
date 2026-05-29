package darepoclicommands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
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
