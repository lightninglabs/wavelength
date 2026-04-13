package swaps

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/swaprpc"
	"google.golang.org/grpc"
)

// GRPCSwapServerConn implements SwapServerConn using the shared generated
// swaprpc client stubs.
type GRPCSwapServerConn struct {
	client swaprpc.SwapServiceClient
}

// NewGRPCSwapServerConn creates a gRPC-backed SwapServerConn from one connected
// gRPC client connection.
func NewGRPCSwapServerConn(
	conn grpc.ClientConnInterface) *GRPCSwapServerConn {

	return &GRPCSwapServerConn{
		client: swaprpc.NewSwapServiceClient(conn),
	}
}

// RequestChannelID asks the swap server for one route hint and the negotiated
// vHTLC configuration for a Lightning-to-Ark receive flow.
func (g *GRPCSwapServerConn) RequestChannelID(ctx context.Context,
	vhtlcPubkey *btcec.PublicKey,
	expirySeconds uint32) (*RouteHint, *VHTLCConfig, error) {

	if vhtlcPubkey == nil {
		return nil, nil, fmt.Errorf(
			"vHTLC pubkey must be provided",
		)
	}

	resp, err := g.client.RequestChannelId(
		ctx, &swaprpc.RequestChannelIdRequest{
			ExpirySeconds:     expirySeconds,
			ClientVhtlcPubkey: vhtlcPubkey.SerializeCompressed(),
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"RequestChannelId RPC: %w", err,
		)
	}

	hint, err := routeHintFromProto(resp.GetRouteHint())
	if err != nil {
		return nil, nil, err
	}

	cfg, err := vhtlcConfigFromProto(resp.GetVhtlcConfig())
	if err != nil {
		return nil, nil, err
	}

	return hint, cfg, nil
}

// CreateInSwap initiates one Ark-to-Lightning swap on the swap server.
func (g *GRPCSwapServerConn) CreateInSwap(ctx context.Context,
	invoice string, maxFeeSat uint64,
	clientVhtlcPubkey *btcec.PublicKey) (*InSwapConfig, error) {

	if clientVhtlcPubkey == nil {
		return nil, fmt.Errorf(
			"client vHTLC pubkey must be provided",
		)
	}

	resp, err := g.client.CreateInSwap(
		ctx, &swaprpc.CreateInSwapRequest{
			Invoice:   invoice,
			MaxFeeSat: maxFeeSat,
			ClientVhtlcPubkey: clientVhtlcPubkey.
				SerializeCompressed(),
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"CreateInSwap RPC: %w", err,
		)
	}

	return inSwapConfigFromProto(resp)
}

// Close is a no-op because the caller owns the underlying grpc.ClientConn.
func (g *GRPCSwapServerConn) Close() error {
	return nil
}

// routeHintFromProto converts one generated swaprpc route hint into the local
// SDK shape.
func routeHintFromProto(hint *swaprpc.RouteHint) (*RouteHint, error) {
	if hint == nil {
		return nil, fmt.Errorf("route hint must be provided")
	}

	return &RouteHint{
		NodeID:          append([]byte(nil), hint.GetNodeId()...),
		ChannelID:       hint.GetChannelId(),
		FeeBaseMsat:     hint.GetFeeBaseMsat(),
		FeePropPpm:      hint.GetFeeProportionalPpm(),
		CltvExpiryDelta: hint.GetCltvExpiryDelta(),
	}, nil
}

// vhtlcConfigFromProto converts one generated swaprpc vHTLC config into the
// local SDK shape.
func vhtlcConfigFromProto(cfg *swaprpc.VHTLCConfig) (*VHTLCConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("vHTLC config must be provided")
	}

	return &VHTLCConfig{
		RefundLocktime:        cfg.GetRefundLocktime(),
		UnilateralClaimDelay:  cfg.GetUnilateralClaimDelay(),
		UnilateralRefundDelay: cfg.GetUnilateralRefundDelay(),
		UnilateralRefundWithoutReceiverDelay: cfg.
			GetUnilateralRefundWithoutReceiverDelay(),
		SwapServerPubkey: append(
			[]byte(nil), cfg.GetSwapserverPubkey()...,
		),
	}, nil
}

// inSwapConfigFromProto converts one generated in-swap response into the local
// SDK shape.
func inSwapConfigFromProto(
	resp *swaprpc.CreateInSwapResponse) (*InSwapConfig, error) {

	if resp == nil {
		return nil, fmt.Errorf(
			"in-swap response must be provided",
		)
	}

	if len(resp.GetPaymentHash()) == 0 {
		return nil, fmt.Errorf(
			"in-swap response missing payment hash",
		)
	}

	if len(resp.GetServerPubkey()) == 0 {
		return nil, fmt.Errorf(
			"in-swap response missing server pubkey",
		)
	}

	var paymentHash [32]byte
	copy(paymentHash[:], resp.GetPaymentHash())

	serverPubkey, err := btcec.ParsePubKey(resp.GetServerPubkey())
	if err != nil {
		return nil, fmt.Errorf(
			"parse server pubkey: %w", err,
		)
	}

	cfg, err := vhtlcConfigFromProto(resp.GetVhtlcConfig())
	if err != nil {
		return nil, err
	}

	var expiryTime time.Time
	if resp.GetExpiry() != nil {
		expiryTime = resp.GetExpiry().AsTime()
	}

	return &InSwapConfig{
		PaymentHash:  paymentHash,
		AmountSat:    int64(resp.GetAmountSat()),
		FeeSat:       resp.GetFeeSat(),
		ServerPubkey: serverPubkey,
		VHTLCConfig:  *cfg,
		Expiry:       expiryTime,
	}, nil
}

// Ensure GRPCSwapServerConn satisfies the SwapServerConn interface.
var _ SwapServerConn = (*GRPCSwapServerConn)(nil)
