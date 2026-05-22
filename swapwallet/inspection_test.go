//go:build walletrpc && swapruntime

package swapwallet

import (
	"encoding/hex"
	"testing"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newInspectionFixture wires an inspection service with fake swap and daemon
// backends.
func newInspectionFixture(t *testing.T) (*InspectionService, *fakeSwapService,
	*fakeRPCServer) {

	t.Helper()

	swap := &fakeSwapService{}
	rpc := &fakeRPCServer{}
	deps := &Deps{
		SwapService: swap,
		RPCServer:   rpc,
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	return newInspectionService(deps, runtime), swap, rpc
}

// TestInspectActivityShowsPayFundingTrace verifies a pay swap inspection links
// the friendly send row to its funding input and change output.
func TestInspectActivityShowsPayFundingTrace(t *testing.T) {
	t.Parallel()

	inspection, swap, rpc := newInspectionFixture(t)

	sessionID := testBytes(32, 0x21)
	sessionHex := hex.EncodeToString(sessionID)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "payment-hash",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				Pending:          true,
				AmountSat:        1_234,
				VhtlcOutpoint:    "vhtlc-txid:0",
				VhtlcAmountSat:   1_234,
				UpdatedAtUnix:    200,
				FundingSessionId: "funding-session",
			},
		},
	}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:           "oor",
				Subtype:        ledger.EventVTXOSent,
				AmountSat:      999_745,
				DebitAccount:   ledger.AccountTransfersOut,
				CreditAccount:  ledger.AccountVTXOBalance,
				SessionId:      sessionID,
				EntryId:        13,
				CreatedAtUnixS: 100,
			},
			{
				Type:          "round",
				Subtype:       ledger.EventVTXOReceived,
				AmountSat:     998_511,
				DebitAccount:  ledger.AccountVTXOBalance,
				CreditAccount: ledger.AccountTransfersIn,
				Description: "VTXO received via oor: " +
					sessionHex + ":1",
				EntryId:        14,
				CreatedAtUnixS: 101,
			},
		},
	}

	resp, err := inspection.InspectActivity(
		t.Context(), &walletrpc.InspectActivityRequest{
			Id: "payment-hash",
		},
	)
	require.NoError(t, err)

	require.Equal(t, "payment-hash", resp.GetEntry().GetId())
	require.Equal(t, "payment-hash", resp.GetSwap().GetPaymentHash())
	require.Len(t, resp.GetLedgerRows(), 2)
	require.Len(t, resp.GetVtxos(), 3)

	ledgerByID := map[int64]*walletrpc.ActivityLedgerTrace{}
	for _, row := range resp.GetLedgerRows() {
		ledgerByID[row.GetEntryId()] = row
	}

	require.True(t, ledgerByID[13].GetHiddenFromActivity())
	require.Equal(t, "spent_input", ledgerByID[13].GetRole())
	require.False(t, ledgerByID[14].GetHiddenFromActivity())
	require.Equal(t, "change_output", ledgerByID[14].GetRole())

	vtxoByRole := map[string]*walletrpc.ActivityVTXOTrace{}
	for _, row := range resp.GetVtxos() {
		vtxoByRole[row.GetRole()] = row
	}

	require.Equal(
		t, int64(1_234), vtxoByRole["vhtlc_output"].GetAmountSat(),
	)
	require.False(t, vtxoByRole["vhtlc_output"].GetOurs())
	require.Equal(
		t, int64(999_745), vtxoByRole["spent_input"].GetAmountSat(),
	)
	require.Equal(
		t, int64(998_511), vtxoByRole["change_output"].GetAmountSat(),
	)
	require.True(t, vtxoByRole["change_output"].GetOurs())
}

// TestInspectActivityNotFound verifies missing ids return a NotFound status.
func TestInspectActivityNotFound(t *testing.T) {
	t.Parallel()

	inspection, swap, rpc := newInspectionFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{}

	_, err := inspection.InspectActivity(
		t.Context(), &walletrpc.InspectActivityRequest{
			Id: "missing",
		},
	)
	require.Error(t, err)
	require.Equal(t, codes.NotFound, status.Code(err))
}
