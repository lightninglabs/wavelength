package waveclicommands

import (
	"testing"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestValidateDestinationRejectsAgentHallucinations confirms the
// destination validator catches the agent-hallucination patterns the
// CLI specifically guards against: embedded query params / fragments,
// whitespace, and empty strings.
func TestValidateDestinationRejectsAgentHallucinations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		dest   string
		errIs  string
		expect bool
	}{
		// Happy paths.
		{
			"lnbcrt100u1pwlqxyz...",
			"",
			true,
		},
		{
			"bcrt1q0123abc",
			"",
			true,
		},

		// Empty.
		{
			"",
			"required",
			false,
		},

		// Embedded query / fragment.
		{
			"lnbcrt100?fields=amt",
			"query/fragment",
			false,
		},
		{
			"bcrt1q0123#alias",
			"query/fragment",
			false,
		},

		// Whitespace.
		{
			"bcrt1q0123 ",
			"whitespace",
			false,
		},
		{
			"\tlnbcrt100",
			"whitespace",
			false,
		},
	}
	for _, tc := range cases {
		err := validateDestination(tc.dest)
		if tc.expect {
			require.NoError(t, err, "dest=%q", tc.dest)

			continue
		}
		require.Error(t, err, "dest=%q", tc.dest)
		require.ErrorContains(t, err, tc.errIs, "dest=%q", tc.dest)
	}
}

// TestValidateFreeTextRejectsControlCharacters confirms note/memo
// validation rejects control characters but accepts ordinary UTF-8.
func TestValidateFreeTextRejectsControlCharacters(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateFreeText("--note", ""))
	require.NoError(t, validateFreeText("--note", "hello world"))
	require.NoError(t, validateFreeText("--note", "café ☕"))

	// Tab is a control char.
	err := validateFreeText("--note", "tab\there")
	require.ErrorContains(t, err, "control character")

	// Bell.
	err = validateFreeText("--memo", "bell\x07here")
	require.ErrorContains(t, err, "control character")

	// DEL (0x7f) is a control char too.
	err = validateFreeText("--note", "del\x7fchar")
	require.ErrorContains(t, err, "control character")
}

// TestValidateOutpointEnforcesShape confirms the outpoint validator
// requires the canonical txid:vout form and rejects malformed input
// up front so the daemon never sees obvious garbage.
func TestValidateOutpointEnforcesShape(t *testing.T) {
	t.Parallel()

	good := "abcdef01" + "abcdef01abcdef01abcdef01abcdef01abcdef01" +
		"abcdef01abcdef01" + ":0"
	require.NoError(t, validateOutpoint(good))

	cases := []struct {
		op    string
		errIs string
	}{
		{
			"",
			"required",
		},
		{
			"abc",
			"txid:vout",
		},
		{
			"abc:0:1",
			"txid:vout",
		},
		{
			"shorttxid:0",
			"64 hex",
		},
		// Non-hex character in txid (length 64, 'g' at position 0).
		{
			"g" +
				"abcdef01abcdef01abcdef01abcdef01" +
				"abcdef01abcdef01abcdef01abcdef0" + ":0",
			"non-hex",
		},
		// Empty vout.
		{
			"abcdef01abcdef01abcdef01abcdef01abcdef01" +
				"abcdef01abcdef01abcdef01:",
			"vout is empty",
		},
	}
	for _, tc := range cases {
		err := validateOutpoint(tc.op)
		require.Error(t, err, "op=%q", tc.op)
		require.ErrorContains(t, err, tc.errIs, "op=%q", tc.op)
	}
}

// TestResolveOffchainFlagDefaultsToOffchain confirms absence of either
// flag implies offchain (the default for send/recv).
func TestResolveOffchainFlagDefaultsToOffchain(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.Flags().Bool("offchain", false, "")
	cmd.Flags().Bool("onchain", false, "")

	offchain, err := resolveOffchainFlag(cmd)
	require.NoError(t, err)
	require.True(t, offchain)
}

// TestResolveOffchainFlagOnchainOverridesDefault confirms --onchain
// flips the default.
func TestResolveOffchainFlagOnchainOverridesDefault(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.Flags().Bool("offchain", false, "")
	cmd.Flags().Bool("onchain", false, "")
	require.NoError(t, cmd.Flags().Set("onchain", "true"))

	offchain, err := resolveOffchainFlag(cmd)
	require.NoError(t, err)
	require.False(t, offchain)
}

// TestResolveOffchainFlagRejectsConflict confirms the validator
// rejects both flags at once.
func TestResolveOffchainFlagRejectsConflict(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.Flags().Bool("offchain", false, "")
	cmd.Flags().Bool("onchain", false, "")
	require.NoError(t, cmd.Flags().Set("offchain", "true"))
	require.NoError(t, cmd.Flags().Set("onchain", "true"))

	_, err := resolveOffchainFlag(cmd)
	require.ErrorIs(t, err, errOffchainOnchainConflict)
}

// TestParseEntryKindAcceptsCanonicalForms confirms the kind parser
// accepts the canonical names plus the obvious aliases ("receive" for
// "recv").
func TestParseEntryKindAcceptsCanonicalForms(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want wavewalletrpc.EntryKind
		bad  bool
	}{
		{
			"send",
			wavewalletrpc.EntryKind_ENTRY_KIND_SEND,
			false,
		},
		{
			"recv",
			wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
			false,
		},
		{
			"receive",
			wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
			false,
		},
		{
			"deposit",
			wavewalletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
			false,
		},
		{
			"exit",
			wavewalletrpc.EntryKind_ENTRY_KIND_EXIT,
			false,
		},
		{
			"",
			wavewalletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
			true,
		},
		{
			"junk",
			wavewalletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
			true,
		},
	}
	for _, tc := range cases {
		k, err := parseEntryKind(tc.in)
		if tc.bad {
			require.Error(t, err, "in=%q", tc.in)

			continue
		}
		require.NoError(t, err, "in=%q", tc.in)
		require.Equal(t, tc.want, k, "in=%q", tc.in)
	}
}

// TestWalletCommandsRejectUnexpectedArgs confirms wallet verbs do not silently
// ignore positional input. This is especially important for secret-bearing
// commands where an argv typo can leak sensitive material to shell history.
func TestWalletCommandsRejectUnexpectedArgs(t *testing.T) {
	t.Parallel()

	commands := []*cobra.Command{
		newBalanceCmd(),
		newCreateCmd(),
		newUnlockCmd(),
		newRecvCmd(),
		newActivityCmd(),
		newExitCmd(),
		newExitStatusCmd(),
		newExitSummaryCmd(),
	}

	for _, cmd := range commands {
		require.Error(t, cmd.Args(cmd, []string{"unexpected"}), cmd.Use)
	}
}
