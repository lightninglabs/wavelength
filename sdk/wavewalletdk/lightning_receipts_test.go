//go:build wavewalletrpc && swapruntime

package wavewalletdk

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// receiptWalletStub returns a real wallet RPC response through Receive's
// production conversion path, including the credit response discriminator.
type receiptWalletStub struct {
	wavewalletrpc.WalletServiceClient
	response *wavewalletrpc.RecvResponse
}

// Recv returns the admitted invoice and its typed source response.
func (s *receiptWalletStub) Recv(context.Context, *wavewalletrpc.RecvRequest,
	...grpc.CallOption) (*wavewalletrpc.RecvResponse, error) {

	return s.response, nil
}

// TestReceiveReturnsTypedReceiptReference proves the host need not infer a
// source from display strings or canonical-ID syntax when enrolling an invoice.
func TestReceiveReturnsTypedReceiptReference(t *testing.T) {
	for _, source := range []LightningReceiptSource{
		LightningReceiptSwap, LightningReceiptCredit,
	} {
		t.Run(string(source), func(t *testing.T) {
			response := &wavewalletrpc.RecvResponse{
				Invoice: "invoice",
				Entry: &wavewalletrpc.WalletEntry{
					Id: "entry-id", AmountSat: 100_000,
				},
			}
			if source == LightningReceiptCredit {
				creditReceive := &wavewalletrpc.CreditReceive{
					OperationId: "source-id",
					AmountSat:   100_000,
				}
				response.CreditReceive = creditReceive
			}
			client := &Client{
				canWallet: true,
				wallet: &receiptWalletStub{
					response: response,
				},
			}
			result, err := client.Receive(
				t.Context(), ReceiveRequest{
					AmountSat: 100_000,
				},
			)
			require.NoError(t, err)
			expectedID := "entry-id"
			if source == LightningReceiptCredit {
				expectedID = "source-id"
			}
			require.Equal(t, &LightningReceiptReference{
				Source: source, ID: expectedID,
			}, result.ReceiptReference)
		})
	}
}

// TestLightningReceiptFacadeJSON checks the actual encoding/json contract used
// by gomobile, including numeric satoshis and original source timestamps.
func TestLightningReceiptFacadeJSON(t *testing.T) {
	ref := LightningReceiptReference{
		Source: LightningReceiptSwap,
		ID:     "hash",
	}
	client := &Client{readLightningReceipts: func(ctx context.Context,
		request LightningReceiptRequest) ([]LightningReceipt, error) {

		require.NoError(t, ctx.Err())
		require.EqualValues(t, 1000, request.CreatedAfterUnix)
		require.Equal(
			t, []LightningReceiptReference{ref}, request.References,
		)

		return []LightningReceipt{{
			Reference: ref, CanonicalID: "hash", State: "complete",
			AmountSat: 100_000, CreatedAtUnix: 1001,
		}}, nil
	}}
	result, err := client.ReadLightningReceipts(
		t.Context(), LightningReceiptRequest{
			References:       []LightningReceiptReference{ref},
			CreatedAfterUnix: 1000,
		},
	)
	require.NoError(t, err)
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	require.JSONEq(t, `[{"reference":{"source":"lightning_swap",`+
		`"id":"hash"},"canonical_id":"hash","state":"complete",`+
		`"amount_sat":100000,"created_at_unix":1001}]`, string(raw))
	remote := &Client{}
	_, err = remote.ReadLightningReceipts(
		t.Context(), LightningReceiptRequest{},
	)
	require.ErrorIs(t, err, ErrLightningReceiptsUnavailable)
}
