package waveclicommands

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestParseChannelIDAcceptsCLIEncodings verifies command chaining works with
// both the protobuf JSON response and the canonical hexadecimal identifier.
func TestParseChannelIDAcceptsCLIEncodings(t *testing.T) {
	t.Parallel()

	id := make([]byte, 32)
	for i := range id {
		id[i] = byte(i + 1)
	}

	for _, encoded := range []string{
		hex.EncodeToString(id), base64.StdEncoding.EncodeToString(id),
	} {
		decoded, err := parseChannelID(encoded)
		require.NoError(t, err)
		require.Equal(t, id, decoded)
	}
}

// TestParseChannelIDRejectsWrongLength verifies malformed identifiers fail
// before the CLI opens a daemon connection.
func TestParseChannelIDRejectsWrongLength(t *testing.T) {
	t.Parallel()

	_, err := parseChannelID(hex.EncodeToString(make([]byte, 31)))
	require.ErrorContains(t, err, "exactly 32 bytes")
}

// TestParsePositiveChannelAmount keeps channel creation's public input to one
// positive amount instead of exposing internal funding-policy switches.
func TestParsePositiveChannelAmount(t *testing.T) {
	t.Parallel()

	amount, err := parsePositiveChannelAmount("200000")
	require.NoError(t, err)
	require.EqualValues(t, 200_000, amount)

	for _, value := range []string{"0", "-1", "one"} {
		_, err := parsePositiveChannelAmount(value)
		require.ErrorContains(t, err, "positive integer")
	}
}

// TestChannelMoneyCommandsRequireConfirmation verifies every command that
// changes wallet or channel balances stops before dialing without approval.
func TestChannelMoneyCommandsRequireConfirmation(t *testing.T) {
	previous := stdinIsTTY
	stdinIsTTY = func(*cobra.Command) bool { return false }
	t.Cleanup(func() {
		stdinIsTTY = previous
	})

	id := hex.EncodeToString(make([]byte, 32))
	tests := []struct {
		name string
		cmd  *cobra.Command
		args []string
	}{
		{
			name: "create",
			cmd:  newChannelCreateCmd(),
			args: []string{
				"1000",
			},
		},
		{
			name: "send",
			cmd:  newChannelSendCmd(),
			args: []string{
				id,
				"1000",
			},
		},
		{
			name: "receive",
			cmd:  newChannelReceiveCmd(),
			args: []string{
				id,
				"1000",
			},
		},
		{
			name: "pay",
			cmd:  newChannelPayCmd(),
			args: []string{
				"lnbc1test",
			},
		},
		{
			name: "close",
			cmd:  newChannelCloseCmd(),
			args: []string{
				id,
			},
		},
		{
			name: "force close",
			cmd:  newChannelForceCloseCmd(),
			args: []string{
				id,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.cmd.SetIn(strings.NewReader(""))
			err := test.cmd.RunE(test.cmd, test.args)
			require.Error(t, err)
			require.Equal(
				t, ExitConfirmationRequired, ExitCodeFor(err),
			)
		})
	}
}

// TestChannelCommandRecoveryFlags verifies the create retry key, listing
// command, distinct payment descriptions, and conservative fee default.
func TestChannelCommandRecoveryFlags(t *testing.T) {
	t.Parallel()

	create := newChannelCreateCmd()
	require.NotNil(t, create.Flags().Lookup("idempotency-key"))
	require.NotNil(t, create.Flags().Lookup("yes"))

	root := newChannelCmd()
	list, _, err := root.Find([]string{"list"})
	require.NoError(t, err)
	require.Equal(t, "list", list.Name())

	send := newChannelSendCmd()
	receive := newChannelReceiveCmd()
	require.NotEqual(t, send.Short, receive.Short)

	pay := newChannelPayCmd()
	maxFee, err := pay.Flags().GetUint64("max-fee-sat")
	require.NoError(t, err)
	require.Equal(t, uint64(1_000), maxFee)
}
