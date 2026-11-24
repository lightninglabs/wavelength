//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// TestArkChannelBindingDispatch exercises the JSON ABI against an embedded
// server, recording the decoded request and returning a controlled RPC reply.
// No peer, invoice signing, or payment settlement is simulated here.
func TestArkChannelBindingDispatch(t *testing.T) {
	cfg := wavewalletdk.Config{
		DataDir:             t.TempDir(),
		Network:             "regtest",
		WalletType:          waved.WalletTypeLwwallet,
		WalletEsploraURL:    "http://127.0.0.1:1",
		ServerAddress:       "http://127.0.0.1:1",
		ServerTransport:     wavewalletdk.TransportREST,
		ServerInsecure:      true,
		SwapServerAddress:   "http://127.0.0.1:1",
		SwapServerTransport: wavewalletdk.TransportREST,
		SwapServerInsecure:  true,
		ArkChannelsEnabled:  true,
		LogWriter:           io.Discard,
		DaemonConfig:        waved.DefaultConfig(),
	}
	type observation struct {
		method string
		req    proto.Message
	}
	observed := make(chan observation, 8)
	var reject atomic.Bool
	channelID := sha256.Sum256([]byte("channel"))
	preimage := sha256.Sum256([]byte("preimage"))
	hash := sha256.Sum256(preimage[:])
	cfg.DaemonConfig.UnaryServerInterceptors = []grpc.UnaryServerInterceptor{
		func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
			next grpc.UnaryHandler) (any, error) {

			var resp proto.Message
			switch req := req.(type) {
			case *arkchannelrpc.PromoteVTXORequest:
				id := channelID[:]
				if req.GetIdempotencyKey() == "short-id" {
					id = id[:1]
				}
				resp = &arkchannelrpc.PromoteVTXOResponse{
					Channel: &arkchannelrpc.ArkChannel{
						ChannelId:   id,
						Phase:       "Active",
						CapacitySat: 12345,
					},
				}

			case *arkchannelrpc.PayLightningInvoiceRequest:
				pay := &arkchannelrpc.PayLightningInvoiceResponse{
					ChannelId: channelID[:], PaymentHash: hash[:],
					Preimage: preimage[:], PrivateAmountSat: 101, FeeSat: 1,
				}
				switch req.GetPaymentRequest() {
				case "short-id":
					pay.ChannelId = pay.ChannelId[:1]

				case "short-preimage":
					pay.Preimage = pay.Preimage[:1]
					shortHash := sha256.Sum256(pay.Preimage)
					pay.PaymentHash = shortHash[:]

				case "short-hash":
					pay.PaymentHash = pay.PaymentHash[:1]

				case "wrong-hash":
					pay.PaymentHash = channelID[:]

				case "empty-reply":
					pay = &arkchannelrpc.PayLightningInvoiceResponse{}
				}
				resp = pay

			case *wavewalletrpc.RecvRequest:
				resp = &wavewalletrpc.RecvResponse{
					Invoice: "ln-result",
				}

			default:
				return next(ctx, req)
			}
			observed <- observation{
				info.FullMethod,
				req.(proto.Message),
			}
			if reject.Load() {
				return nil, status.Error(
					codes.FailedPrecondition,
					"no liquidity",
				)
			}

			return resp, nil
		},
	}
	err := startEmbedded(
		func(start, _ context.Context) (*wavewalletdk.Client, error) {
			return wavewalletdk.Start(start, cfg)
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, Stop()) })

	for _, tc := range []struct {
		verb   func([]byte) ([]byte, error)
		method string
		body   string
		req    proto.Message
		want   map[string]any
	}{
		{PromoteVTXO, arkchannelrpc.ArkChannelService_PromoteVTXO_FullMethodName,
			`{"AmountSat":12345,"IdempotencyKey":"promotion-1"}`,
			&arkchannelrpc.PromoteVTXORequest{
				AmountSat:      12345,
				IdempotencyKey: "promotion-1",
			},
			map[string]any{
				"ChannelID":   hex.EncodeToString(channelID[:]),
				"Phase":       "Active",
				"CapacitySat": 12345.0,
			}},
		{PayOnActiveChannel, arkchannelrpc.ArkChannelService_PayLightningInvoice_FullMethodName,
			`{"Invoice":"ln-pay","MaxFeeSat":4}`,
			&arkchannelrpc.PayLightningInvoiceRequest{
				PaymentRequest: "ln-pay",
				MaxFeeSat:      4,
			},
			map[string]any{"ChannelID": hex.EncodeToString(
				channelID[:],
			),
				"PaymentHash": hex.EncodeToString(hash[:]), "Preimage": hex.EncodeToString(
					preimage[:],
				),
				"PrivateAmountSat": 101.0, "FeeSat": 1.0}},
		{ReceiveOnActiveChannel, wavewalletrpc.WalletService_RecvOnActiveChannel_FullMethodName,
			`{"AmountSat":500,"Memo":"inbound","TimeoutSeconds":4}`,
			&wavewalletrpc.RecvRequest{
				AmtSat: 500,
				Memo:   "inbound",
			},
			map[string]any{
				"Invoice": "ln-result",
			}},
		{Receive, wavewalletrpc.WalletService_Recv_FullMethodName,
			`{"AmountSat":700,"Memo":"ordinary"}`,
			&wavewalletrpc.RecvRequest{
				AmtSat: 700,
				Memo:   "ordinary",
			},
			map[string]any{
				"Invoice": "ln-result",
			}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			for _, fail := range []bool{false, true} {
				reject.Store(fail)
				out, err := tc.verb([]byte(tc.body))
				if fail {
					require.Nil(t, out)
					require.Equal(
						t, codes.FailedPrecondition,
						status.Code(err),
					)
					require.ErrorContains(
						t, err, "no liquidity",
					)
				} else {
					require.NoError(t, err)
					var got map[string]any
					require.NoError(
						t, json.Unmarshal(out, &got),
					)
					require.Subset(t, got, tc.want)
				}
				require.Len(
					t, observed, 1, "one RPC, no fallback",
				)
				call := <-observed
				require.Equal(t, tc.method, call.method)
				require.True(
					t, proto.Equal(tc.req, call.req),
					"%v", call.req,
				)
			}
		})
	}
	t.Run("malformed replies", func(t *testing.T) {
		reject.Store(false)
		out, err := PromoteVTXO(
			[]byte(
				`{"AmountSat":12345,"IdempotencyKey":"short-id"}`,
			),
		)
		require.Nil(t, out)
		require.ErrorContains(t, err, "invalid channel ID")
		for invoice, wantErr := range map[string]string{
			"empty-reply":    "invalid channel ID",
			"short-id":       "invalid channel ID",
			"short-preimage": "invalid settlement proof",
			"short-hash":     "invalid settlement proof",
			"wrong-hash":     "invalid settlement proof",
		} {
			req, err := json.Marshal(
				wavewalletdk.PayOnActiveChannelRequest{
					Invoice: invoice,
				},
			)
			require.NoError(t, err)
			out, err := PayOnActiveChannel(req)
			require.Nil(t, out)
			require.ErrorContains(t, err, wantErr)
		}
	})
}
