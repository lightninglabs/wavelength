//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"testing"

	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
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
	sessionHex := testSessionString(t, sessionID)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "payment-hash",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				Pending:   true,
				AmountSat: 1_234,
				SettlementType: swapclientrpc.
					SwapSettlementType_SWAP_SETTLEMENT_TYPE_IN_ARK,
				SenderPubkey:     "sender-pubkey",
				Preimage:         "deadbeef",
				VhtlcOutpoint:    "vhtlc-txid:0",
				VhtlcAmountSat:   1_234,
				UpdatedAtUnix:    200,
				FundingSessionId: sessionHex,
			},
		},
	}
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{
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
				Type:           "oor",
				Subtype:        ledger.EventVTXOReceived,
				AmountSat:      998_511,
				DebitAccount:   ledger.AccountVTXOBalance,
				CreditAccount:  ledger.AccountTransfersIn,
				Txid:           sessionHex,
				OutputIndex:    1,
				EntryId:        14,
				CreatedAtUnixS: 101,
			},
		},
	}

	resp, err := inspection.InspectActivity(
		t.Context(), &wavewalletrpc.InspectActivityRequest{
			Id: "payment-hash",
		},
	)
	require.NoError(t, err)

	require.Equal(t, "payment-hash", resp.GetEntry().GetId())
	require.Equal(t, "payment-hash", resp.GetSwap().GetPaymentHash())
	require.Equal(
		t, "SWAP_SETTLEMENT_TYPE_IN_ARK",
		resp.GetSwap().GetSettlementType(),
	)
	require.Equal(t, "sender-pubkey", resp.GetSwap().GetSenderPubkey())
	require.Equal(t, "deadbeef", resp.GetSwap().GetPreimage())
	require.Len(t, resp.GetLedgerRows(), 2)
	require.Len(t, resp.GetVtxos(), 3)

	ledgerByID := map[int64]*wavewalletrpc.ActivityLedgerTrace{}
	for _, row := range resp.GetLedgerRows() {
		ledgerByID[row.GetEntryId()] = row
	}

	require.True(t, ledgerByID[13].GetHiddenFromActivity())
	require.Equal(t, "spent_input", ledgerByID[13].GetRole())
	require.True(t, ledgerByID[14].GetHiddenFromActivity())
	require.Equal(t, "change_output", ledgerByID[14].GetRole())
	require.Equal(t, int32(1), ledgerByID[14].GetOutputIndex())

	vtxoByRole := map[string]*wavewalletrpc.ActivityVTXOTrace{}
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
	require.Equal(
		t, uint32(1), vtxoByRole["change_output"].GetOutputIndex(),
	)
	require.Len(t, resp.GetNotes(), 1)
}

// TestInspectActivityOmitsIrrelevantNotes verifies ordinary deposits do not
// carry OOR caveats in the inspection response.
func TestInspectActivityOmitsIrrelevantNotes(t *testing.T) {
	t.Parallel()

	inspection, swap, rpc := newInspectionFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{
			{
				Type:               "boarding",
				Subtype:            ledger.EventWalletUTXOCreated,
				AmountSat:          1_000_000,
				ConfirmationStatus: "confirmed",
				Txid:               "deposit-txid",
				OutputIndex:        1,
				EntryId:            1,
				CreatedAtUnixS:     100,
			},
		},
	}

	resp, err := inspection.InspectActivity(
		t.Context(), &wavewalletrpc.InspectActivityRequest{
			Id: "deposit-txid:1",
		},
	)
	require.NoError(t, err)
	require.Empty(t, resp.GetNotes())
	require.Equal(t, int32(1), resp.GetLedgerRows()[0].GetOutputIndex())
}

// TestInspectActivityNotFound verifies missing ids return a NotFound status.
func TestInspectActivityNotFound(t *testing.T) {
	t.Parallel()

	inspection, swap, rpc := newInspectionFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &waverpc.ListTransactionsResponse{}

	_, err := inspection.InspectActivity(
		t.Context(), &wavewalletrpc.InspectActivityRequest{
			Id: "missing",
		},
	)
	require.Error(t, err)
	require.Equal(t, codes.NotFound, status.Code(err))
}
