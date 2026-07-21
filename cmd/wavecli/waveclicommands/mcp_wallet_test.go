package waveclicommands

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type recordingWalletServiceClient struct {
	wavewalletrpc.WalletServiceClient

	prepareReqs []*wavewalletrpc.PrepareSendRequest
	sendReqs    []*wavewalletrpc.SendRequest
}

func (c *recordingWalletServiceClient) PrepareSend(_ context.Context,
	req *wavewalletrpc.PrepareSendRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.PrepareSendResponse, error) {

	c.prepareReqs = append(c.prepareReqs, req)

	return &wavewalletrpc.PrepareSendResponse{
		SendIntentId:   "intent-123",
		AmountSat:      50_000,
		ExpectedFeeSat: 123,
		FeeKnown:       true,
	}, nil
}

func (c *recordingWalletServiceClient) Send(_ context.Context,
	req *wavewalletrpc.SendRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.SendResponse, error) {

	c.sendReqs = append(c.sendReqs, req)

	return &wavewalletrpc.SendResponse{}, nil
}

func TestPrepareMCPWalletSendDoesNotDispatch(t *testing.T) {
	t.Parallel()

	client := &recordingWalletServiceClient{}
	result, err := prepareMCPWalletSend(
		t.Context(), client, mcpSendPrepareArgs{
			Destination: "lnbcrt100u1pwlqxyz",
			MaxFeeSat:   250,
			Note:        "coffee",
		},
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, client.prepareReqs, 1)
	require.Empty(t, client.sendReqs)
	require.Equal(
		t, "lnbcrt100u1pwlqxyz", client.prepareReqs[0].GetInvoice(),
	)
	require.Equal(t, uint64(250), client.prepareReqs[0].GetMaxFeeSat())

	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	var preview map[string]any
	require.NoError(t, json.Unmarshal([]byte(text.Text), &preview))
	require.Equal(t, "intent-123", preview["send_intent_id"])
}

func TestDispatchMCPWalletSendConsumesExactIntent(t *testing.T) {
	t.Parallel()

	client := &recordingWalletServiceClient{}
	result, err := dispatchMCPWalletSend(
		t.Context(), client, mcpSendArgs{
			SendIntentID: "intent-123",
		},
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, client.prepareReqs)
	require.Len(t, client.sendReqs, 1)
	require.Equal(
		t, "intent-123", client.sendReqs[0].GetSendIntentId(),
	)
}

func TestDispatchMCPWalletSendRequiresIntent(t *testing.T) {
	t.Parallel()

	client := &recordingWalletServiceClient{}
	result, err := dispatchMCPWalletSend(
		t.Context(), client, mcpSendArgs{},
	)
	require.Nil(t, result)
	require.ErrorContains(t, err, "send.prepare")
	require.Empty(t, client.prepareReqs)
	require.Empty(t, client.sendReqs)
}

func TestMCPWalletSendToolsExposeTwoPhaseSchemas(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	server := buildMCPServer(nil, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()

	client := mcp.NewClient(
		&mcp.Implementation{
			Name:    "test",
			Version: "0",
		},
		nil,
	)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer clientSession.Close()

	tools, err := clientSession.ListTools(ctx, nil)
	require.NoError(t, err)

	var prepareSchema, sendSchema []byte
	for _, tool := range tools.Tools {
		schema, err := json.Marshal(tool.InputSchema)
		require.NoError(t, err)

		switch tool.Name {
		case "send.prepare":
			prepareSchema = schema

		case "send":
			sendSchema = schema
		}
	}

	require.Contains(t, string(prepareSchema), `"destination"`)
	require.NotContains(t, string(prepareSchema), `"send_intent_id"`)
	require.Contains(t, string(sendSchema), `"send_intent_id"`)
	require.NotContains(t, string(sendSchema), `"destination"`)
}

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
		t, wavewalletrpc.ListView_LIST_VIEW_ACTIVITY, req.GetView(),
	)
	require.True(t, req.GetPendingOnly())
	require.Len(t, req.GetKinds(), 2)
	require.Equal(t, uint32(50), req.GetLimit())
	require.Equal(t, "cursor-token", req.GetCursor())
}

// TestCheckMCPRefreshConsent pins the MCP refresh fee-consent
// contract: a dry-run preview and an acknowledged refresh pass, a
// bare real refresh is refused with an actionable error that names
// both the preview and the acknowledgement path — matching the
// consent the schema registry promises for this method.
func TestCheckMCPRefreshConsent(t *testing.T) {
	t.Parallel()

	require.NoError(t, checkMCPRefreshConsent(true, false))
	require.NoError(t, checkMCPRefreshConsent(false, true))
	require.NoError(t, checkMCPRefreshConsent(true, true))

	err := checkMCPRefreshConsent(false, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "dry_run:true")
	require.Contains(t, err.Error(), "yes:true")
	require.Contains(t, err.Error(), "operator fee")
}
