package wavewalletdk

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// maxTestDaemon supplies a balance containing live Bitcoin, locked Bitcoin and
// an asset. Only the live Bitcoin input may fund the preview.
type maxTestDaemon struct{ waverpc.DaemonServiceClient }

// GetInfo returns the configured network and an authenticated change floor.
func (*maxTestDaemon) GetInfo(context.Context, *waverpc.GetInfoRequest,
	...grpc.CallOption) (*waverpc.GetInfoResponse, error) {

	return &waverpc.GetInfoResponse{
		Network: "regtest", ServerInfo: &waverpc.ServerInfo{
			MinVtxoAmountSat: 1000,
		},
	}, nil
}

// ListVTXOs exercises the Max-side status and asset filters.
func (*maxTestDaemon) ListVTXOs(_ context.Context,
	req *waverpc.ListVTXOsRequest, _ ...grpc.CallOption) (
	*waverpc.ListVTXOsResponse, error) {

	if req.GetStatusFilter() != waverpc.VTXOStatus_VTXO_STATUS_LIVE {
		panic("unbounded VTXO read")
	}

	return &waverpc.ListVTXOsResponse{Vtxos: []*waverpc.VTXO{
		{
			Status:    waverpc.VTXOStatus_VTXO_STATUS_LIVE,
			AmountSat: 1500,
		},
		{
			Status:    waverpc.VTXOStatus_VTXO_STATUS_SPENDING,
			AmountSat: 10000,
		},
		{
			Status:    waverpc.VTXOStatus_VTXO_STATUS_LIVE,
			AmountSat: 10000,
			AssetRef:  "asset",
		},
	}}, nil
}

// maxTestSwaps exposes only the non-funding quote method. Any attempt to pay
// during the search calls the embedded nil interface and fails the test.
type maxTestSwaps struct {
	swapclientrpc.SwapClientServiceClient
}

// QuotePay quotes the original invoice without changing its bytes.
func (*maxTestSwaps) QuotePay(_ context.Context,
	req *swapclientrpc.QuotePayRequest, _ ...grpc.CallOption) (
	*swapclientrpc.QuotePayResponse, error) {

	return &swapclientrpc.QuotePayResponse{
		InvoiceAmountSat: req.AmountSat,
		AmountSat:        req.AmountSat + 6,
		FeeSat:           6,
	}, nil
}

// maxTestWallet records the one prepared intent and can change its final fee.
type maxTestWallet struct {
	wavewalletrpc.WalletServiceClient
	calls   int
	fee     int64
	request *wavewalletrpc.PrepareSendRequest
}

// PrepareSend creates the sole preview; it does not expose Send at all.
func (w *maxTestWallet) PrepareSend(_ context.Context,
	req *wavewalletrpc.PrepareSendRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.PrepareSendResponse, error) {

	w.calls++
	w.request = req

	return &wavewalletrpc.PrepareSendResponse{
		SendIntentId: "one-intent", AmountSat: int64(req.AmtSat),
		ExpectedFeeSat: w.fee, ExpectedTotalOutflowSat: int64(
			req.AmtSat,
		) + w.fee,
		FeeKnown: true, TotalOutflowKnown: true,
	}, nil
}

// maxTestInvoice creates a new single-use amountless invoice for local tests.
func maxTestInvoice(t *testing.T) string {
	t.Helper()
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	inv, err := zpay32.NewInvoice(
		&chaincfg.RegressionNetParams, [32]byte{42}, time.Now(),
		zpay32.Description("Max test"),
	)
	require.NoError(t, err)
	invoice, err := inv.Encode(zpay32.MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			digest := sha256.Sum256(msg)

			return ecdsa.SignCompact(key, digest[:], true), nil
		},
	})
	require.NoError(t, err)

	return invoice
}

// TestPrepareMaxSendCreatesOneIntent proves the SDK keeps the invoice, note,
// and credit permissions intact, excludes unusable funds and never sends.
func TestPrepareMaxSendCreatesOneIntent(t *testing.T) {
	for _, fee := range []int64{6, 7} {
		wallet := &maxTestWallet{fee: fee}
		client := newExitTestClient(wallet)
		client.daemon = &maxTestDaemon{}
		client.swaps = &maxTestSwaps{}
		invoice := maxTestInvoice(t)
		result, err := client.PrepareSend(
			t.Context(), PrepareSendRequest{
				Invoice:           invoice,
				Note:              "Coffee",
				MaxAmount:         true,
				MaxCreditSat:      100,
				MaxCreditTopupSat: 1000,
			},
		)
		if fee == 7 {
			require.ErrorIs(t, err, ErrMaxQuoteChanged)
			require.Nil(t, result)
		} else {
			require.NoError(t, err)
			require.Equal(t, int64(1494), result.AmountSat)
			require.Equal(
				t, int64(1500), result.ExpectedTotalOutflowSat,
			)
		}
		require.Equal(t, 1, wallet.calls)
		require.Equal(t, invoice, wallet.request.GetInvoice())
		require.Equal(t, "Coffee", wallet.request.GetNote())
		require.Equal(t, uint64(100), wallet.request.GetMaxCreditSat())
		require.Equal(
			t, uint64(1000), wallet.request.GetMaxCreditTopupSat(),
		)
	}
}
