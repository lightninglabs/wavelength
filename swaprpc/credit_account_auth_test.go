package swaprpc

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestCreditAccountRequestDigestCommitsToRequest verifies each supported RPC
// shape has a stable account binding and that changing its semantics changes
// the signed digest.
func TestCreditAccountRequestDigestCommitsToRequest(t *testing.T) {
	t.Parallel()

	accountKey := []byte{2, 3, 4}
	tests := []struct {
		name   string
		req    proto.Message
		mutate func(*testing.T, proto.Message)
	}{
		{
			name: "request channel id",
			req: &RequestChannelIdRequest{
				ClientVhtlcPubkey: accountKey,
				PaymentHash: []byte{
					1,
				},
				AmountSat: 2,
			},
			mutate: func(t *testing.T, req proto.Message) {
				typed, ok := req.(*RequestChannelIdRequest)
				require.True(t, ok)
				typed.AmountSat++
			},
		},
		{
			name: "create in swap",
			req: &CreateInSwapRequest{
				AccountPubkey: accountKey,
				Invoice:       "invoice-a",
			},
			mutate: func(t *testing.T, req proto.Message) {
				typed, ok := req.(*CreateInSwapRequest)
				require.True(t, ok)
				typed.Invoice = "invoice-b"
			},
		},
		{
			name: "create refresh swap",
			req: &CreateRefreshSwapRequest{
				ClientVhtlcPubkey: accountKey,
				PaymentHash: []byte{
					1,
				},
				AmountSat:        2,
				MaxVtxoAgeBlocks: 6,
			},
			mutate: func(t *testing.T, req proto.Message) {
				typed, ok := req.(*CreateRefreshSwapRequest)
				require.True(t, ok)
				typed.MaxVtxoAgeBlocks++
			},
		},
		{
			name: "quote in swap",
			req: &QuoteInSwapRequest{
				AccountPubkey: accountKey,
				MaxCreditSat:  3,
			},
			mutate: func(t *testing.T, req proto.Message) {
				typed, ok := req.(*QuoteInSwapRequest)
				require.True(t, ok)
				typed.MaxCreditSat++
			},
		},
		{
			name: "create credit",
			req: &CreateCreditRequest{
				AccountPubkey:  accountKey,
				IdempotencyKey: "create-a",
			},
			mutate: func(t *testing.T, req proto.Message) {
				typed, ok := req.(*CreateCreditRequest)
				require.True(t, ok)
				typed.IdempotencyKey = "create-b"
			},
		},
		{
			name: "redeem credit",
			req: &RedeemCreditRequest{
				AccountPubkey: accountKey,
				DestinationPubkey: []byte{
					5,
				},
			},
			mutate: func(t *testing.T, req proto.Message) {
				typed, ok := req.(*RedeemCreditRequest)
				require.True(t, ok)
				typed.DestinationPubkey = []byte{
					6,
				}
			},
		},
		{
			name: "list credits",
			req: &ListCreditsRequest{
				AccountPubkey: accountKey,
				Limit:         10,
			},
			mutate: func(t *testing.T, req proto.Message) {
				typed, ok := req.(*ListCreditsRequest)
				require.True(t, ok)
				typed.Limit++
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			originalDigest, gotAccount, err :=
				CreditAccountRequestDigest(test.req)
			require.NoError(t, err)
			require.Equal(t, accountKey, gotAccount)

			withAuth := proto.Clone(test.req)
			err = SetCreditAccountAuthorization(
				withAuth, &CreditAccountAuthorization{
					ExpiresAtUnix: 1,
					Nonce:         []byte{2},
					Signature:     []byte{3},
				},
			)
			require.NoError(t, err)
			authorizedDigest, _, err :=
				CreditAccountRequestDigest(withAuth)
			require.NoError(t, err)
			require.Equal(t, originalDigest, authorizedDigest)

			mutated := proto.Clone(test.req)
			test.mutate(t, mutated)
			mutatedDigest, _, err :=
				CreditAccountRequestDigest(mutated)
			require.NoError(t, err)
			require.NotEqual(t, originalDigest, mutatedDigest)
		})
	}
}

// TestCreditAccountRequestDigestRejectsUnknownRequest verifies callers cannot
// accidentally authorize a request outside the explicit account RPC set.
func TestCreditAccountRequestDigestRejectsUnknownRequest(t *testing.T) {
	t.Parallel()

	_, _, err := CreditAccountRequestDigest(&RouteHint{})
	require.ErrorContains(t, err, "unsupported credit account request")
}
