package swaps

import (
	"context"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/rpc/restclient"
	"github.com/lightninglabs/darepo-client/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc"
)

// GRPCSwapServerConn implements SwapServerConn using the shared generated
// swaprpc client stubs.
type GRPCSwapServerConn struct {
	client swaprpc.SwapServiceClient
}

// NewGRPCSwapServerConn creates a gRPC-backed SwapServerConn from one connected
// gRPC client connection.
func NewGRPCSwapServerConn(conn grpc.ClientConnInterface) *GRPCSwapServerConn {
	return &GRPCSwapServerConn{
		client: swaprpc.NewSwapServiceClient(conn),
	}
}

// NewRESTSwapServerConn creates a REST-backed SwapServerConn for one
// grpc-gateway base address.
func NewRESTSwapServerConn(addr string,
	opts ...restclient.Option) *GRPCSwapServerConn {

	return &GRPCSwapServerConn{
		client: restclient.NewSwapServiceClient(addr, opts...),
	}
}

// RequestChannelID asks the swap server for one route hint for a
// Lightning-to-Ark receive flow.
func (g *GRPCSwapServerConn) RequestChannelID(ctx context.Context,
	vhtlcPubkey *btcec.PublicKey, paymentHash lntypes.Hash,
	amountSat btcutil.Amount, expirySeconds uint32) (*OutSwapQuote, error) {

	if vhtlcPubkey == nil {
		return nil, fmt.Errorf("vHTLC pubkey must be provided")
	}
	if amountSat <= 0 {
		return nil, fmt.Errorf("receive amount must be positive")
	}

	resp, err := g.client.RequestChannelId(
		ctx, &swaprpc.RequestChannelIdRequest{
			ExpirySeconds: expirySeconds,
			ClientVhtlcPubkey: vhtlcPubkey.
				SerializeCompressed(),
			PaymentHash: append(
				[]byte(nil), paymentHash[:]...,
			),
			AmountMsat: uint64(amountSat) * 1000,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("RequestChannelId RPC: %w", err)
	}

	hint, err := routeHintFromProto(resp.GetRouteHint())
	if err != nil {
		return nil, err
	}

	return &OutSwapQuote{
		RouteHint: hint,
		// The server binds the route to the requested amount but does
		// not echo it, so keep the caller's amount in the quote.
		ReceiveAmountSat: amountSat,
		PayerFeeMsat:     resp.GetPayerFeeMsat(),
	}, nil
}

// CreateInSwap initiates one Ark-to-Lightning swap on the swap server.
func (g *GRPCSwapServerConn) CreateInSwap(ctx context.Context, invoice string,
	maxFeeSat uint64, clientVhtlcPubkey *btcec.PublicKey) (*InSwapConfig,
	error) {

	if clientVhtlcPubkey == nil {
		return nil, fmt.Errorf("client vHTLC pubkey must be provided")
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
		return nil, fmt.Errorf("CreateInSwap RPC: %w", err)
	}

	return inSwapConfigFromProto(resp)
}

// AuthorizeInSwapRefund asks the swap server to sign one prepared cooperative
// in-swap refund.
func (g *GRPCSwapServerConn) AuthorizeInSwapRefund(ctx context.Context,
	paymentHash lntypes.Hash, vhtlcOutpoint string, vhtlcAmountSat int64,
	vhtlcPolicyTemplate, refundSpendPath, checkpointPSBT []byte) (
	*InSwapRefundAuthorization, error) {

	if vhtlcOutpoint == "" {
		return nil, fmt.Errorf("vHTLC outpoint must be provided")
	}
	if vhtlcAmountSat <= 0 {
		return nil, fmt.Errorf("vHTLC amount must be positive")
	}

	resp, err := g.client.AuthorizeInSwapRefund(
		ctx, &swaprpc.AuthorizeInSwapRefundRequest{
			PaymentHash:    append([]byte(nil), paymentHash[:]...),
			VhtlcOutpoint:  vhtlcOutpoint,
			VhtlcAmountSat: uint64(vhtlcAmountSat),
			VhtlcPolicyTemplate: append(
				[]byte(nil), vhtlcPolicyTemplate...,
			),
			RefundSpendPath: append(
				[]byte(nil), refundSpendPath...,
			),
			CheckpointPsbt: append([]byte(nil), checkpointPSBT...),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("AuthorizeInSwapRefund RPC: %w", err)
	}

	sig := resp.GetSignature()
	if sig == nil {
		return nil, fmt.Errorf("AuthorizeInSwapRefund RPC returned " +
			"empty signature")
	}

	return &InSwapRefundAuthorization{
		Signature: TaprootScriptSignature{
			PubKey: append([]byte(nil), sig.GetPubkey()...),
			WitnessScript: append(
				[]byte(nil), sig.GetWitnessScript()...,
			),
			Signature: append([]byte(nil), sig.GetSignature()...),
			SigHash:   sig.GetSighash(),
		},
		FailureReason: resp.GetFailureReason(),
	}, nil
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

	routeHint := &RouteHint{
		NodeID:          append([]byte(nil), hint.GetNodeId()...),
		ChannelID:       hint.GetChannelId(),
		FeeBaseMsat:     hint.GetFeeBaseMsat(),
		FeePropPpm:      hint.GetFeeProportionalPpm(),
		CltvExpiryDelta: hint.GetCltvExpiryDelta(),
	}

	if err := validateRouteHint(routeHint); err != nil {
		return nil, err
	}

	return routeHint, nil
}

// vhtlcConfigFromProto converts one generated swaprpc vHTLC config into the
// local SDK shape.
func vhtlcConfigFromProto(cfg *swaprpc.VHTLCConfig) (*VHTLCConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("vHTLC config must be provided")
	}

	if cfg.GetRefundLocktime() == 0 {
		return nil, fmt.Errorf("vHTLC refund locktime must be set")
	}

	if cfg.GetUnilateralClaimDelay() == 0 {
		return nil, fmt.Errorf("vHTLC unilateral claim delay must be " +
			"set")
	}

	if cfg.GetUnilateralRefundDelay() == 0 {
		return nil, fmt.Errorf("vHTLC unilateral refund delay must " +
			"be set")
	}

	if cfg.GetUnilateralRefundWithoutReceiverDelay() == 0 {
		return nil, fmt.Errorf("vHTLC unilateral " +
			"refund-without-receiver delay must be set")
	}

	if len(cfg.GetSwapserverPubkey()) == 0 {
		return nil, fmt.Errorf("vHTLC swap server pubkey must be set")
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

// outSwapHtlcEventFromProto converts one generated funded-HTLC event into the
// local SDK shape.
func outSwapHtlcEventFromProto(event *swaprpc.OutSwapHtlcEvent) (
	*OutSwapHtlcEvent, error) {

	if event == nil {
		return nil, fmt.Errorf("out-swap HTLC event must be provided")
	}
	if len(event.GetPaymentHash()) != lntypes.HashSize {
		return nil, fmt.Errorf("out-swap event payment hash must be "+
			"%d bytes", lntypes.HashSize)
	}
	if event.GetAmountSat() > math.MaxInt64 {
		return nil, fmt.Errorf("out-swap event amount exceeds int64")
	}

	var paymentHash lntypes.Hash
	copy(paymentHash[:], event.GetPaymentHash())

	cfg, err := vhtlcConfigFromProto(event.GetVhtlcConfig())
	if err != nil {
		return nil, err
	}

	return &OutSwapHtlcEvent{
		PaymentHash: paymentHash,
		AmountSat:   int64(event.GetAmountSat()),
		OnionBlob: append(
			[]byte(nil), event.GetOnionBlob()...,
		),
		VHTLCConfig: *cfg,
	}, nil
}

// inArkHtlcEventFromProto converts one generated p2p event into the local SDK
// shape.
func inArkHtlcEventFromProto(event *swaprpc.InArkHtlcEvent) (*InArkHtlcEvent,
	error) {

	if event == nil {
		return nil, fmt.Errorf("in-ark HTLC event must be provided")
	}
	if len(event.GetPaymentHash()) != lntypes.HashSize {
		return nil, fmt.Errorf("in-ark event payment hash must be "+
			"%d bytes", lntypes.HashSize)
	}
	if event.GetAmountSat() > math.MaxInt64 ||
		event.GetVhtlcAmountSat() > math.MaxInt64 {
		return nil, fmt.Errorf("in-ark event amount exceeds int64")
	}

	var paymentHash lntypes.Hash
	copy(paymentHash[:], event.GetPaymentHash())

	senderKey, err := btcec.ParsePubKey(event.GetSenderPubkey())
	if err != nil {
		return nil, fmt.Errorf("parse in-ark sender pubkey: %w", err)
	}

	cfg, err := vhtlcConfigFromProto(event.GetVhtlcConfig())
	if err != nil {
		return nil, err
	}

	return &InArkHtlcEvent{
		PaymentHash:    paymentHash,
		AmountSat:      int64(event.GetAmountSat()),
		SenderPubkey:   senderKey,
		VHTLCConfig:    *cfg,
		VHTLCOutpoint:  event.GetVhtlcOutpoint(),
		VHTLCAmountSat: int64(event.GetVhtlcAmountSat()),
	}, nil
}

// inSwapConfigFromProto converts one generated in-swap response into the local
// SDK shape.
func inSwapConfigFromProto(resp *swaprpc.CreateInSwapResponse) (*InSwapConfig,
	error) {

	if resp == nil {
		return nil, fmt.Errorf("in-swap response must be provided")
	}

	if len(resp.GetPaymentHash()) != lntypes.HashSize {
		return nil, fmt.Errorf("in-swap response payment hash must be "+
			"%d bytes", lntypes.HashSize)
	}

	if resp.GetAmountSat() == 0 {
		return nil, fmt.Errorf("in-swap response amount must be " +
			"positive")
	}

	if resp.GetAmountSat() > math.MaxInt64 {
		return nil, fmt.Errorf("in-swap response amount exceeds " +
			"int64 range")
	}

	if len(resp.GetServerPubkey()) == 0 {
		return nil, fmt.Errorf("in-swap response missing server pubkey")
	}

	var paymentHash lntypes.Hash
	copy(paymentHash[:], resp.GetPaymentHash())

	serverPubkey, err := btcec.ParsePubKey(resp.GetServerPubkey())
	if err != nil {
		return nil, fmt.Errorf("parse server pubkey: %w", err)
	}

	cfg, err := vhtlcConfigFromProto(resp.GetVhtlcConfig())
	if err != nil {
		return nil, err
	}

	if resp.GetExpiry() == nil {
		return nil, fmt.Errorf("in-swap response missing expiry")
	}

	expiryTime := resp.GetExpiry().AsTime()
	if expiryTime.IsZero() {
		return nil, fmt.Errorf("in-swap response expiry must be " +
			"non-zero")
	}

	settlementType, err := settlementTypeFromProto(
		resp.GetSettlementType(),
	)
	if err != nil {
		return nil, err
	}

	return &InSwapConfig{
		PaymentHash:    paymentHash,
		AmountSat:      int64(resp.GetAmountSat()),
		FeeSat:         resp.GetFeeSat(),
		ServerPubkey:   serverPubkey,
		VHTLCConfig:    *cfg,
		Expiry:         expiryTime,
		SettlementType: settlementType,
	}, nil
}

// settlementTypeFromProto converts the wire settlement enum, treating
// unspecified as Lightning for compatibility with older test fixtures.
func settlementTypeFromProto(wireType swaprpc.SettlementType) (SettlementType,
	error) {

	switch wireType {
	case swaprpc.SettlementType_SETTLEMENT_TYPE_UNSPECIFIED,
		swaprpc.SettlementType_SETTLEMENT_TYPE_LIGHTNING:
		return SettlementTypeLightning, nil

	case swaprpc.SettlementType_SETTLEMENT_TYPE_IN_ARK:
		return SettlementTypeInArk, nil

	default:
		return "", fmt.Errorf("unknown settlement type %v", wireType)
	}
}

// Ensure GRPCSwapServerConn satisfies the SwapServerConn interface.
var _ SwapServerConn = (*GRPCSwapServerConn)(nil)
