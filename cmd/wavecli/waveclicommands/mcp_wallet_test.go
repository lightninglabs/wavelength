package waveclicommands

import (
	"testing"

	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/stretchr/testify/require"
)

// TestParseDirectionFieldDefaultsToOffchain confirms an agent that
// omits the direction string lands on the safe invoice path. The CLI
// flag layer enforces the same default via resolveOffchainFlag — the
// MCP layer must not drift from it.
func TestParseDirectionFieldDefaultsToOffchain(t *testing.T) {
	t.Parallel()

	offchain, err := parseDirectionField("")
	require.NoError(t, err)
	require.True(t, offchain)
}

// TestParseDirectionFieldRejectsUnknown rejects an unknown direction
// rather than coercing it to a default — silent coercion would let an
// agent dispatch an onchain leave with a typo'd "onchian".
func TestParseDirectionFieldRejectsUnknown(t *testing.T) {
	t.Parallel()

	_, err := parseDirectionField("onchian")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown direction")
}

// TestBuildWalletPrepareSendRequestHardensAgentInput exercises the shared
// builder used by both the CLI send verb and the MCP send tool. The
// MCP path can't drift past the same input-hardening checks as the
// CLI, so the rejections are exhaustive.
func TestBuildWalletPrepareSendRequestHardensAgentInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		dest     string
		offchain bool
		amt      uint64
		note     string
		sweepAll bool
		wantErr  string
	}{
		{
			name:     "empty destination",
			dest:     "",
			offchain: true,
			amt:      1000,
			wantErr:  "destination is required",
		},
		{
			name:     "embedded query param",
			dest:     "lnbcrt100?fields=amt",
			offchain: true,
			amt:      1000,
			wantErr:  "query/fragment",
		},
		{
			name:     "control char in note",
			dest:     "lnbcrt100",
			offchain: true,
			amt:      1000,
			note:     "hello\x01world",
			wantErr:  "control character",
		},
		{
			name:     "sweep_all on offchain",
			dest:     "lnbcrt100",
			offchain: true,
			sweepAll: true,
			wantErr:  "only valid with onchain",
		},
		{
			name:     "onchain without amt or sweep_all",
			dest:     "bcrt1q0123",
			offchain: false,
			amt:      0,
			wantErr:  "amt_sat is required for onchain",
		},
		{
			name:     "onchain with both amt and sweep_all",
			dest:     "bcrt1q0123",
			offchain: false,
			amt:      1000,
			sweepAll: true,
			wantErr:  "sweep_all requires amt_sat=0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildWalletPrepareSendRequest(
				tc.dest, tc.offchain, tc.amt, 0, tc.note,
				tc.sweepAll,
			)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestBuildWalletPrepareSendRequestHappyPath confirms a valid invoice send
// produces the expected oneof and scalar fields.
func TestBuildWalletPrepareSendRequestHappyPath(t *testing.T) {
	t.Parallel()

	req, err := buildWalletPrepareSendRequest(
		"lnbcrt100u1pwlqxyz", true, 0, 250, "coffee", false,
	)
	require.NoError(t, err)

	require.Equal(t, "lnbcrt100u1pwlqxyz", req.GetInvoice())
	require.Equal(t, uint64(250), req.GetMaxFeeSat())
	require.Equal(t, "coffee", req.GetNote())
}

// TestBuildWalletActivityRequestHappyPath confirms the MCP activity tool
// accepts filters and produces a populated request.
func TestBuildWalletActivityRequestHappyPath(t *testing.T) {
	t.Parallel()

	req, err := buildWalletActivityRequest(
		true, []string{"send", "recv"}, 50, "cursor-token",
	)
	require.NoError(t, err)
	require.Equal(
		t, walletdkrpc.ListView_LIST_VIEW_ACTIVITY, req.GetView(),
	)
	require.True(t, req.GetPendingOnly())
	require.Len(t, req.GetKinds(), 2)
	require.Equal(t, uint32(50), req.GetLimit())
	require.Equal(t, "cursor-token", req.GetCursor())
}
