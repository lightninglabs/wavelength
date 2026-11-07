//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// receiptCreditRPC reads the real source SQL store behind the optional seam.
type receiptCreditRPC struct {
	*fakeRPCServer
	store *db.CreditOperationStoreDB
}

// ReadCreditOperationForReceipt forwards one identity to its durable store.
func (r *receiptCreditRPC) ReadCreditOperationForReceipt(ctx context.Context,
	id string) (*db.CreditOperationRecord, error) {

	return r.store.GetOperation(ctx, id)
}

// TestLightningReceiptUsesPrincipal checks the typed receive and real canonical
// sink. Attached credits must not inflate an invoice's presented amount.
func TestLightningReceiptUsesPrincipal(t *testing.T) {
	runtime, store, _ := newReconcileFixture(t)
	runtime.deps.ChainParams = &chaincfg.RegressionNetParams
	invoice, hash := testPreparedInvoice(t, 100_000, "receipt")
	swap := &fakeSwapService{}
	runtime.deps.SwapService = swap
	swap.getSwapResp = &swapclientrpc.GetSwapResponse{
		Swap: &swapclientrpc.SwapSummary{
			PaymentHash: hash, Invoice: invoice,
			Direction: swapclientrpc.
				SwapDirection_SWAP_DIRECTION_RECEIVE,
			State:   swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
			Pending: true, AmountSat: 100_000,
			RequestedAmountSat: 100_000, AttachedCreditSat: 7_000,
			CreatedAtUnix: 1001, UpdatedAtUnix: 1002,
		},
	}
	ref := waved.LightningReceiptReference{
		Source: waved.LightningReceiptSwap, ID: hash,
	}
	pending, err := runtime.lookupLightningReceipt(t.Context(), ref, 1000)
	require.NoError(t, err)
	require.Equal(t, "pending", pending.State)
	require.EqualValues(t, 100_000, pending.AmountSat)
	swap.getSwapResp.Swap.Pending = false
	complete, err := runtime.lookupLightningReceipt(t.Context(), ref, 1000)
	require.NoError(t, err)
	require.Equal(t, "complete", complete.State)
	require.EqualValues(t, 100_000, complete.AmountSat)
	require.EqualValues(t, 1001, complete.CreatedAtUnix)
	row, err := store.GetEntry(t.Context(), hash)
	require.NoError(t, err)
	require.EqualValues(t, 100_000, row.AmountSat)
	before, err := store.EventHighWater(t.Context())
	require.NoError(t, err)
	_, err = runtime.lookupLightningReceipt(t.Context(), ref, 1000)
	require.NoError(t, err)
	after, err := store.EventHighWater(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	// Original admission at the exact opt-in second is intentionally
	// excluded.
	old, err := runtime.lookupLightningReceipt(t.Context(), ref, 1001)
	require.NoError(t, err)
	require.Nil(t, old)
	// Outgoing invoice activity cannot acquire receive authority by ID
	// reuse.
	swap.getSwapResp.Swap.Direction = swapclientrpc.
		SwapDirection_SWAP_DIRECTION_PAY
	outgoing, err := runtime.lookupLightningReceipt(t.Context(), ref, 1000)
	require.NoError(t, err)
	require.Nil(t, outgoing)
}

// TestCreditReceiptRetainsOriginalAdmission covers the missing-pending-row
// crash shape: late projection must not turn historical credit into new funds.
func TestCreditReceiptRetainsOriginalAdmission(t *testing.T) {
	database := db.NewTestDB(t)
	clk := clock.NewTestClock(time.Unix(1001, 0))
	base := db.NewStore(
		database.DB, database.Queries, database.Backend(),
		btclog.Disabled,
	)
	credits := db.NewCreditOperationStore(base, clk)
	activity := base.NewActivityStore(clk)
	runtime := newRuntime(t.Context(), &Deps{
		ActivityStore: activity,
		ChainParams:   &chaincfg.RegressionNetParams,
		RPCServer: &receiptCreditRPC{
			fakeRPCServer: &fakeRPCServer{}, store: credits,
		},
	})
	t.Cleanup(runtime.stop)
	invoice, hash := testPreparedInvoice(t, 21, "credit receipt")
	hashBytes, err := hex.DecodeString(hash)
	require.NoError(t, err)
	record := db.CreditOperationRecord{
		OpID: "receive", OpKey: "recv:admission",
		Kind:       db.CreditOpKindReceive,
		State:      string(credit.StateCompleted),
		Status:     db.CreditOpStatusCompleted,
		ServerOpID: "server-receive", PaymentHash: hashBytes,
		Invoice: invoice, AmountSat: 21, SnapshotData: []byte{
			1,
		},
		SnapshotVersion: 1,
	}
	require.NoError(t, credits.UpsertOperation(t.Context(), record))
	clk.SetTime(time.Unix(5000, 0))
	// Later lifecycle writes do not replace source creation time.
	require.NoError(t, credits.UpsertOperation(t.Context(), record))
	ref := waved.LightningReceiptReference{
		Source: waved.LightningReceiptCredit, ID: record.OpID,
	}
	old, err := runtime.lookupLightningReceipt(t.Context(), ref, 2000)
	require.NoError(t, err)
	require.Nil(t, old)
	water, err := activity.EventHighWater(t.Context())
	require.NoError(t, err)
	require.Zero(t, water)
	complete, err := runtime.lookupLightningReceipt(t.Context(), ref, 1000)
	require.NoError(t, err)
	require.Equal(t, "complete", complete.State)
	require.EqualValues(t, 21, complete.AmountSat)
	require.EqualValues(t, 1001, complete.CreatedAtUnix)
	row, err := activity.GetEntry(t.Context(), record.OpID)
	require.NoError(t, err)
	require.EqualValues(t, 1001, row.CreatedAtUnix)
	// A pay top-up or redemption is not a receive, even with a positive
	// value.
	record.OpID = "pay"
	record.OpKey = "pay:" + hash
	record.Kind = db.CreditOpKindPay
	ref.ID = record.OpID
	require.NoError(t, credits.UpsertOperation(t.Context(), record))
	other, err := runtime.lookupLightningReceipt(t.Context(), ref, 1000)
	require.NoError(t, err)
	require.Nil(t, other)
}

// TestLightningReceiptRejectsUnboundInvoice verifies the network, payment hash
// and exact principal before any notification can use a source amount.
func TestLightningReceiptRejectsUnboundInvoice(t *testing.T) {
	runtime, _, _ := newReconcileFixture(t)
	runtime.deps.ChainParams = &chaincfg.RegressionNetParams
	invoice, hash := testPreparedInvoice(t, 100_000, "receipt")
	summary := &swapclientrpc.SwapSummary{
		PaymentHash: hash, Invoice: invoice,
		Direction: swapclientrpc.
			SwapDirection_SWAP_DIRECTION_RECEIVE,
		State:     swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
		AmountSat: 100_001, CreatedAtUnix: 1001,
	}
	swap := &fakeSwapService{
		getSwapResp: &swapclientrpc.GetSwapResponse{
			Swap: summary,
		},
	}
	runtime.deps.SwapService = swap
	ref := waved.LightningReceiptReference{
		Source: waved.LightningReceiptSwap,
		ID:     hash}
	_, err := runtime.lookupLightningReceipt(t.Context(), ref, 1000)
	require.ErrorContains(t, err, "disagrees")
	summary.AmountSat = 100_000
	runtime.deps.ChainParams = &chaincfg.MainNetParams
	_, err = runtime.lookupLightningReceipt(t.Context(), ref, 1000)
	require.ErrorContains(t, err, "decode receipt invoice")
	runtime.deps.ChainParams = &chaincfg.RegressionNetParams
	summary.PaymentHash = "00" + hash[2:]
	ref.ID = summary.PaymentHash
	_, err = runtime.lookupLightningReceipt(t.Context(), ref, 1000)
	require.ErrorContains(t, err, "disagrees")
}
