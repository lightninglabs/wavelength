package wavewalletdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
)

// PromoteVTXORequest selects wallet liquidity to move into an Ark channel.
// Persist the idempotency key before dispatch and reuse it for the same intent.
type PromoteVTXORequest struct {
	AmountSat      int64
	IdempotencyKey string
}

// PromoteVTXOResult identifies the channel and its durable lifecycle phase.
type PromoteVTXOResult struct {
	ChannelID   string
	Phase       string
	CapacitySat int64
}

// PayOnActiveChannelRequest pays a BOLT-11 invoice from channel liquidity.
type PayOnActiveChannelRequest struct {
	Invoice   string
	MaxFeeSat uint64
}

// PayOnActiveChannelResult reports the settled payment. IDs and the proof of
// payment are hex encoded.
type PayOnActiveChannelResult struct {
	PaymentHash      string
	Preimage         string
	ChannelID        string
	PrivateAmountSat int64
	FeeSat           int64
}

// channelRPC selects the authenticated daemon connection, including embedded
// bufconn. The daemon has no HTTP/JSON gateway for ArkChannelService.
func (c *Client) channelRPC() (arkchannelrpc.ArkChannelServiceClient, error) {
	if c == nil || c.conn == nil {
		return nil, errors.New("channel control requires embedded or " +
			"gRPC transport")
	}

	return arkchannelrpc.NewArkChannelServiceClient(c.conn), nil
}

// PromoteVTXO moves existing wallet liquidity into an Ark-backed channel.
func (c *Client) PromoteVTXO(ctx context.Context, req PromoteVTXORequest) (
	*PromoteVTXOResult, error) {

	rpc, err := c.channelRPC()
	if err != nil {
		return nil, err
	}
	resp, err := rpc.PromoteVTXO(ctx, &arkchannelrpc.PromoteVTXORequest{
		AmountSat:      req.AmountSat,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return nil, fmt.Errorf("promote wallet VTXO: %w", err)
	}
	ch := resp.GetChannel()
	if len(ch.GetChannelId()) != len(arkchannel.ID{}) {
		return nil, errors.New("promotion returned an invalid " +
			"channel ID")
	}

	return &PromoteVTXOResult{
		ChannelID:   hex.EncodeToString(ch.GetChannelId()),
		Phase:       ch.GetPhase(),
		CapacitySat: ch.GetCapacitySat(),
	}, nil
}

// PayOnActiveChannel spends channel liquidity without changing the rail used
// by PrepareSend and SendPrepared. It never falls back to a wallet swap.
func (c *Client) PayOnActiveChannel(ctx context.Context,
	req PayOnActiveChannelRequest) (*PayOnActiveChannelResult, error) {

	rpc, err := c.channelRPC()
	if err != nil {
		return nil, err
	}
	resp, err := rpc.PayLightningInvoice(
		ctx, &arkchannelrpc.PayLightningInvoiceRequest{
			PaymentRequest: req.Invoice,
			MaxFeeSat:      req.MaxFeeSat,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("pay on active channel: %w", err)
	}

	if len(resp.GetChannelId()) != len(arkchannel.ID{}) {
		return nil, errors.New("channel payment returned an invalid " +
			"channel ID")
	}
	preimage := resp.GetPreimage()
	hash := sha256.Sum256(preimage)
	if len(preimage) != sha256.Size ||
		!bytes.Equal(hash[:], resp.GetPaymentHash()) {
		return nil, errors.New("channel payment returned an invalid " +
			"settlement proof")
	}

	return &PayOnActiveChannelResult{
		PaymentHash:      hex.EncodeToString(resp.GetPaymentHash()),
		Preimage:         hex.EncodeToString(resp.GetPreimage()),
		ChannelID:        hex.EncodeToString(resp.GetChannelId()),
		PrivateAmountSat: resp.GetPrivateAmountSat(),
		FeeSat:           resp.GetFeeSat(),
	}, nil
}
