package swapclientserver

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/sdk/swaps"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightningnetwork/lnd/lntypes"
)

// arkChannelPaymentBridge adapts the swap SDK to waved's process-owned native
// lnd channel controller without introducing an in-process gRPC loop.
type arkChannelPaymentBridge struct {
	rpc *waved.RPCServer
}

// IncomingBackingFee returns the daemon policy used by both route padding and
// channel OOR construction.
func (b *arkChannelPaymentBridge) IncomingBackingFee() btcutil.Amount {
	return b.rpc.ArkChannelIncomingBackingFee()
}

// PrepareIncomingPayment installs the known-preimage native invoice.
func (b *arkChannelPaymentBridge) PrepareIncomingPayment(ctx context.Context,
	preimage lntypes.Preimage, amount btcutil.Amount) error {

	return b.rpc.PrepareArkChannelIncomingPayment(ctx, preimage, amount)
}

// RegisterIncomingPayment binds the public future SCID at the hub.
func (b *arkChannelPaymentBridge) RegisterIncomingPayment(ctx context.Context,
	hash lntypes.Hash, amount btcutil.Amount, reservedSCID uint64) error {

	return b.rpc.RegisterArkChannelIncomingPayment(
		ctx, hash, amount, reservedSCID,
	)
}

// WaitIncomingPayment waits for private lnd settlement.
func (b *arkChannelPaymentBridge) WaitIncomingPayment(ctx context.Context,
	hash lntypes.Hash) error {

	return b.rpc.WaitArkChannelIncomingPayment(ctx, hash)
}

// PromoteIncomingVHTLC asks the durable channel FSM to negotiate backing
// before committing the preimage-path OOR claim.
func (b *arkChannelPaymentBridge) PromoteIncomingVHTLC(ctx context.Context,
	request swaps.ArkChannelReceivePromotion) (
	swaps.ArkChannelPromotionResult, error) {

	if request.Input.AmountSat <= 0 ||
		btcutil.Amount(request.Input.AmountSat) != request.Capacity+
			request.BackingFee {
		return swaps.ArkChannelPromotionResult{}, fmt.Errorf(
			"incoming vHTLC amount does not match channel request")
	}
	record, err := b.rpc.PromoteArkChannelIncomingVHTLC(
		ctx, request.PaymentHash, request.ReservedSCID,
		request.Capacity, waved.ArkChannelClaimSource{
			Outpoint: request.Input.Outpoint,
			Amount:   btcutil.Amount(request.Input.AmountSat),
			PolicyTemplate: append(
				[]byte(nil),
				request.Input.VTXOPolicyTemplate...,
			),
			SpendPath: append(
				[]byte(nil), request.Input.SpendPath...,
			),
			PkScript: append(
				[]byte(nil), request.Input.PkScript...,
			),
		},
	)
	if err != nil {
		return swaps.ArkChannelPromotionResult{}, err
	}
	if record.Snapshot.Source == nil {
		return swaps.ArkChannelPromotionResult{}, fmt.Errorf(
			"promoted Ark channel source is missing")
	}

	return swaps.ArkChannelPromotionResult{
		ChannelID:    record.Snapshot.Terms.ID,
		OORSessionID: record.Snapshot.Source.OORSessionID,
	}, nil
}

var _ swaps.ArkChannelPaymentBridge = (*arkChannelPaymentBridge)(nil)
