package waveclicommands

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"sync"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type retryArkChannelClient struct {
	arkchannelrpc.ArkChannelServiceClient

	mu       sync.Mutex
	requests []*arkchannelrpc.PromoteVTXORequest
	errors   []error
}

// PromoteVTXO records retries and returns configured failures before success.
func (c *retryArkChannelClient) PromoteVTXO(_ context.Context,
	req *arkchannelrpc.PromoteVTXORequest, _ ...grpc.CallOption) (
	*arkchannelrpc.PromoteVTXOResponse, error) {

	c.mu.Lock()
	defer c.mu.Unlock()

	c.requests = append(c.requests, req)
	if len(c.errors) > 0 {
		err := c.errors[0]
		c.errors = c.errors[1:]

		return nil, err
	}

	return &arkchannelrpc.PromoteVTXOResponse{}, nil
}

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
			name: "refresh",
			cmd:  newChannelRefreshCmd(),
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
	refresh, _, err := root.Find([]string{"refresh"})
	require.NoError(t, err)
	require.Equal(t, "refresh", refresh.Name())
	_, _, err = root.Find([]string{"close"})
	require.ErrorContains(t, err, "unknown command")

	send := newChannelSendCmd()
	receive := newChannelReceiveCmd()
	require.NotEqual(t, send.Short, receive.Short)

	pay := newChannelPayCmd()
	maxFee, err := pay.Flags().GetUint64("max-fee-sat")
	require.NoError(t, err)
	require.Equal(t, uint64(1_000), maxFee)
}

// TestNewChannelCreateKey verifies the default CLI path always supplies a
// compact non-empty daemon idempotency key.
func TestNewChannelCreateKey(t *testing.T) {
	t.Parallel()

	first, err := newChannelCreateKey()
	require.NoError(t, err)
	second, err := newChannelCreateKey()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(first, channelCreateKeyPrefix))
	require.LessOrEqual(t, len(first), 128)
	require.NotEqual(t, first, second)
}

// TestPromoteVTXOWithRetryReusesRequest verifies a timed-out funding call is
// resumed with the exact same logical intent instead of minting another
// channel.
func TestPromoteVTXOWithRetryReusesRequest(t *testing.T) {
	t.Parallel()

	client := &retryArkChannelClient{errors: []error{
		status.Error(codes.DeadlineExceeded, "still activating"),
	}}
	request := &arkchannelrpc.PromoteVTXORequest{
		AmountSat: 100_000, IdempotencyKey: "create-42",
	}
	response, err := promoteVTXOWithRetry(
		&cobra.Command{}, client, request,
	)
	require.NoError(t, err)
	require.NotNil(t, response)
	require.Len(t, client.requests, 2)
	require.Same(t, client.requests[0], client.requests[1])
}

// TestPromoteVTXOWithRetryHonorsCancellation verifies an interrupted command
// returns its caller's cancellation instead of starting another RPC attempt.
func TestPromoteVTXOWithRetryHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	client := &retryArkChannelClient{errors: []error{
		status.Error(codes.Unavailable, "operator offline"),
	}}
	_, err := promoteVTXOWithRetry(
		cmd, client, &arkchannelrpc.PromoteVTXORequest{
			AmountSat: 100_000, IdempotencyKey: "create-43",
		},
	)
	require.ErrorIs(t, err, context.Canceled)
}
