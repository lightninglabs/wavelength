package waveclicommands

import (
	"bytes"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestExitRejectsAddressWithForceAck asserts the flag conflict is refused, and
// that the refusal points at dropping the force acknowledgement rather than
// the destination.
//
// This is the check that stopped wavelength#845 from being hit for real: the
// user supplied both flags, and clearing --onchain-address (the reading the
// old message invited) would have started a unilateral exit on a VTXO whose
// forfeit the operator already held.
func TestExitRejectsAddressWithForceAck(t *testing.T) {
	t.Parallel()

	const addr = "tb1qvvkd2gf5temvmx4ghq0nq6xkzlx5j55kwmdk44"

	cmd := newExitCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--outpoint", "90199df4b977569efd84936ca97e3f8756422f427bf" +
			"d3ad2392b764c7c2a48a1:0",
		"--onchain-address", addr,
		"--force-unroll-ack", forceUnrollAck,
	})

	// Validation runs before the daemon is dialed, so this fails without
	// any wallet wiring.
	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "--force-unroll-ack")

	// The recovery hint must name the cooperative direction and echo the
	// destination the caller already chose.
	require.Contains(t, err.Error(), "Drop --force-unroll-ack")
	require.Contains(t, err.Error(), addr)
}

// TestPrintExitModeNotice asserts the stderr notice states which exit ran. A
// cooperative leave must say so in as many words, since the verb is named
// `exit` and a user expecting a unilateral one would otherwise read the
// accepted response as the command having ignored them.
func TestPrintExitModeNotice(t *testing.T) {
	t.Parallel()

	const (
		outpoint = "90199df4b977569efd84936ca97e3f8756422f427bfd3ad2" +
			"392b764c7c2a48a1:0"
		addr = "tb1qvvkd2gf5temvmx4ghq0nq6xkzlx5j55kwmdk44"
	)

	t.Run("cooperative", func(t *testing.T) {
		t.Parallel()

		var stderr bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetErr(&stderr)

		printExitModeNotice(cmd, &wavewalletrpc.ExitResponse{
			Mode:           wavewalletrpc.ExitMode_EXIT_MODE_COOPERATIVE, //nolint:ll
			OnchainAddress: addr,
		}, outpoint)

		out := stderr.String()
		require.Contains(t, out, "cooperative leave")
		require.Contains(t, out, "not a unilateral exit")
		require.Contains(t, out, outpoint)
		require.Contains(t, out, addr)
	})

	t.Run("unilateral", func(t *testing.T) {
		t.Parallel()

		var stderr bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetErr(&stderr)

		printExitModeNotice(cmd, &wavewalletrpc.ExitResponse{
			Mode: wavewalletrpc.ExitMode_EXIT_MODE_UNILATERAL,
		}, outpoint)

		out := stderr.String()
		require.Contains(t, out, "unilateral exit")
		require.Contains(t, out, "exit status")
	})
}
