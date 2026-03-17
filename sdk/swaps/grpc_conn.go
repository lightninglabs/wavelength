package swaps

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const (
	// rawCodecName is the name for our custom pass-through codec.
	rawCodecName = "swaps-raw-proto"

	// methodRequestChannelID is the full gRPC method name for the
	// RequestChannelId RPC.
	methodRequestChannelID = "/swaprpc.OutSwapsService/" +
		"RequestChannelId"

	// methodCreateInSwap is the full gRPC method name for the
	// CreateInSwap RPC.
	methodCreateInSwap = "/swaprpc.OutSwapsService/" +
		"CreateInSwap"
)

func init() {
	encoding.RegisterCodec(rawCodec{})
}

// rawCodec is a gRPC codec that passes rawMsg values through
// without additional encoding. This lets us manually encode proto
// wire bytes without needing generated message types.
type rawCodec struct{}

// Marshal implements encoding.Codec.
func (rawCodec) Marshal(v any) ([]byte, error) {
	switch msg := v.(type) {
	case *rawMsg:
		return msg.data, nil

	case proto.Message:
		return proto.Marshal(msg)

	default:
		return nil, fmt.Errorf(
			"rawCodec: unsupported type %T", v,
		)
	}
}

// Unmarshal implements encoding.Codec.
func (rawCodec) Unmarshal(data []byte, v any) error {
	switch msg := v.(type) {
	case *rawMsg:
		msg.data = make([]byte, len(data))
		copy(msg.data, data)

		return nil

	case proto.Message:
		return proto.Unmarshal(data, msg)

	default:
		return fmt.Errorf(
			"rawCodec: unsupported type %T", v,
		)
	}
}

// Name implements encoding.Codec.
func (rawCodec) Name() string {
	return rawCodecName
}

// rawMsg wraps a raw protobuf-encoded byte slice for use with
// rawCodec.
type rawMsg struct {
	data []byte
}

// forceRaw returns a grpc.CallOption that forces the raw codec for
// a single RPC call.
func forceRaw() grpc.CallOption {
	return grpc.ForceCodec(rawCodec{})
}

// GRPCSwapServerConn implements SwapServerConn by calling the swap
// server's gRPC methods using raw protobuf encoding. This avoids
// importing the server module's generated types.
type GRPCSwapServerConn struct {
	conn grpc.ClientConnInterface
}

// NewGRPCSwapServerConn creates a new gRPC-backed SwapServerConn.
// The caller must supply a connected grpc.ClientConnInterface
// pointing at the swap server.
func NewGRPCSwapServerConn(
	conn grpc.ClientConnInterface) *GRPCSwapServerConn {

	return &GRPCSwapServerConn{conn: conn}
}

// RequestChannelID asks the server for a route hint and the
// locked-in vHTLC configuration. It encodes the request manually
// using protowire and decodes the response the same way.
//
// NOTE: This is part of the SwapServerConn interface.
func (g *GRPCSwapServerConn) RequestChannelID(ctx context.Context,
	vhtlcPubkey *btcec.PublicKey,
	expirySeconds uint32) (*RouteHint, *VHTLCConfig, error) {

	req := encodeRequestChannelID(
		vhtlcPubkey.SerializeCompressed(), expirySeconds,
	)

	resp := &rawMsg{}
	err := g.conn.Invoke(
		ctx, methodRequestChannelID, req, resp, forceRaw(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"RequestChannelID: %w", err,
		)
	}

	return decodeRequestChannelIDResponse(resp.data)
}

// CreateInSwap initiates an Ark->LN swap on the server and returns
// the negotiated swap configuration.
//
// NOTE: This is part of the SwapServerConn interface.
func (g *GRPCSwapServerConn) CreateInSwap(ctx context.Context,
	invoice string, maxFeeSat uint64,
	clientVhtlcPubkey *btcec.PublicKey) (*InSwapConfig, error) {

	req := encodeCreateInSwapRequest(
		invoice, maxFeeSat,
		clientVhtlcPubkey.SerializeCompressed(),
	)

	resp := &rawMsg{}
	err := g.conn.Invoke(
		ctx, methodCreateInSwap, req, resp, forceRaw(),
	)
	if err != nil {
		return nil, fmt.Errorf("CreateInSwap: %w", err)
	}

	return decodeCreateInSwapResponse(resp.data)
}

// Close is a no-op because the caller owns the underlying
// grpc.ClientConn and is responsible for closing it.
//
// NOTE: This is part of the SwapServerConn interface.
func (g *GRPCSwapServerConn) Close() error {
	return nil
}

// --- Proto wire encoding helpers ---

// encodeRequestChannelID manually builds the protobuf wire bytes
// for a RequestChannelIdRequest.
//
// Wire layout:
//
//	field 1 (varint): expiry_seconds
//	field 2 (bytes):  client_vhtlc_pubkey
func encodeRequestChannelID(pubkey []byte,
	expiry uint32) *rawMsg {

	var buf []byte
	buf = protowire.AppendTag(
		buf, 1, protowire.VarintType,
	)
	buf = protowire.AppendVarint(buf, uint64(expiry))
	buf = protowire.AppendTag(
		buf, 2, protowire.BytesType,
	)
	buf = protowire.AppendBytes(buf, pubkey)

	return &rawMsg{data: buf}
}

// decodeRequestChannelIDResponse parses a
// RequestChannelIdResponse from raw proto bytes. The response
// contains a RouteHint at field 1 and a VHTLCConfig at field 2.
func decodeRequestChannelIDResponse(
	data []byte) (*RouteHint, *VHTLCConfig, error) {

	var (
		hintBytes []byte
		cfgBytes  []byte
	)

	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, nil, fmt.Errorf("invalid tag")
		}
		data = data[n:]

		switch typ {
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return nil, nil, fmt.Errorf(
					"invalid bytes field",
				)
			}
			data = data[n:]

			switch num {
			case 1:
				hintBytes = v
			case 2:
				cfgBytes = v
			}

		case protowire.VarintType:
			_, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return nil, nil, fmt.Errorf(
					"invalid varint field",
				)
			}
			data = data[n:]

		default:
			return nil, nil, fmt.Errorf(
				"unsupported wire type %d", typ,
			)
		}
	}

	if hintBytes == nil {
		return nil, nil, fmt.Errorf(
			"missing route_hint field",
		)
	}

	hint, err := decodeRouteHint(hintBytes)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"decode route hint: %w", err,
		)
	}

	if cfgBytes == nil {
		return nil, nil, fmt.Errorf(
			"missing vhtlc_config field",
		)
	}

	cfg, err := decodeVHTLCConfig(cfgBytes)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"decode vhtlc config: %w", err,
		)
	}

	return hint, cfg, nil
}

// decodeRouteHint parses a RouteHint proto message from raw bytes.
func decodeRouteHint(data []byte) (*RouteHint, error) {
	hint := &RouteHint{}

	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, fmt.Errorf("invalid tag")
		}
		data = data[n:]

		switch typ {
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return nil, fmt.Errorf(
					"invalid bytes",
				)
			}
			data = data[n:]

			if num == 1 {
				hint.NodeID = v
			}

		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return nil, fmt.Errorf(
					"invalid varint",
				)
			}
			data = data[n:]

			switch num {
			case 2:
				hint.ChannelID = v
			case 3:
				hint.FeeBaseMsat = v
			case 4:
				hint.FeePropPpm = v
			case 5:
				hint.CltvExpiryDelta = uint32(v)
			}

		default:
			return nil, fmt.Errorf(
				"unsupported wire type %d", typ,
			)
		}
	}

	return hint, nil
}

// encodeCreateInSwapRequest builds CreateInSwapRequest proto bytes.
//
// Wire layout:
//
//	field 1 (bytes):  invoice
//	field 2 (varint): max_fee_sat
//	field 3 (bytes):  client_vhtlc_pubkey
func encodeCreateInSwapRequest(invoice string,
	maxFeeSat uint64, pubkey []byte) *rawMsg {

	var buf []byte
	buf = protowire.AppendTag(
		buf, 1, protowire.BytesType,
	)
	buf = protowire.AppendString(buf, invoice)
	buf = protowire.AppendTag(
		buf, 2, protowire.VarintType,
	)
	buf = protowire.AppendVarint(buf, maxFeeSat)
	buf = protowire.AppendTag(
		buf, 3, protowire.BytesType,
	)
	buf = protowire.AppendBytes(buf, pubkey)

	return &rawMsg{data: buf}
}

// decodeCreateInSwapResponse parses a CreateInSwapResponse.
func decodeCreateInSwapResponse(
	data []byte) (*InSwapConfig, error) {

	var (
		paymentHash []byte
		amountSat   uint64
		feeSat      uint64
		serverPub   []byte
		cfgBytes    []byte
		expirySec   int64
		expiryNano  int32
	)

	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, fmt.Errorf("invalid tag")
		}
		data = data[n:]

		switch typ {
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return nil, fmt.Errorf(
					"invalid bytes",
				)
			}
			data = data[n:]

			switch num {
			case 1:
				paymentHash = v
			case 4:
				serverPub = v
			case 5:
				cfgBytes = v
			case 6:
				// Timestamp sub-message.
				expirySec, expiryNano =
					decodeTimestamp(v)
			}

		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return nil, fmt.Errorf(
					"invalid varint",
				)
			}
			data = data[n:]

			switch num {
			case 2:
				amountSat = v
			case 3:
				feeSat = v
			}

		default:
			return nil, fmt.Errorf(
				"unsupported wire type %d", typ,
			)
		}
	}

	// Parse the payment hash.
	var hash lntypes.Hash
	if len(paymentHash) == lntypes.HashSize {
		copy(hash[:], paymentHash)
	}

	// Parse the server pubkey.
	serverKey, err := btcec.ParsePubKey(serverPub)
	if err != nil {
		return nil, fmt.Errorf(
			"parse server pubkey: %w", err,
		)
	}

	// Parse the vHTLC config sub-message.
	vhtlcCfg, err := decodeVHTLCConfig(cfgBytes)
	if err != nil {
		return nil, fmt.Errorf(
			"decode vhtlc config: %w", err,
		)
	}

	expiry := time.Unix(expirySec, int64(expiryNano))

	return &InSwapConfig{
		PaymentHash:  hash,
		AmountSat:    int64(amountSat),
		FeeSat:       feeSat,
		ServerPubkey: serverKey,
		VHTLCConfig:  *vhtlcCfg,
		Expiry:       expiry,
	}, nil
}

// decodeTimestamp parses a google.protobuf.Timestamp sub-message
// and returns seconds and nanos.
func decodeTimestamp(data []byte) (int64, int32) {
	var sec int64
	var nanos int32

	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]

		if typ != protowire.VarintType {
			break
		}

		v, n := protowire.ConsumeVarint(data)
		if n < 0 {
			break
		}
		data = data[n:]

		switch num {
		case 1:
			sec = int64(v)
		case 2:
			nanos = int32(v)
		}
	}

	return sec, nanos
}

// decodeVHTLCConfig parses a VHTLCConfig proto sub-message.
func decodeVHTLCConfig(data []byte) (*VHTLCConfig, error) {
	cfg := &VHTLCConfig{}

	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, fmt.Errorf("invalid tag")
		}
		data = data[n:]

		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return nil, fmt.Errorf(
					"invalid varint",
				)
			}
			data = data[n:]

			switch num {
			case 1:
				cfg.RefundLocktime = uint32(v)
			case 2:
				cfg.UnilateralClaimDelay = uint32(v)
			case 3:
				cfg.UnilateralRefundDelay = uint32(v)
			case 4:
				cfg.UnilateralRefundWithoutReceiverDelay =
					uint32(v)
			}

		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return nil, fmt.Errorf(
					"invalid bytes",
				)
			}
			data = data[n:]

			if num == 5 {
				cfg.SwapServerPubkey = v
			}

		default:
			return nil, fmt.Errorf(
				"unsupported wire type %d", typ,
			)
		}
	}

	return cfg, nil
}

// Compile-time assertion that GRPCSwapServerConn implements
// SwapServerConn.
var _ SwapServerConn = (*GRPCSwapServerConn)(nil)
