package swapclientserver

import (
	"context"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/sdk/swaps"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightningnetwork/lnd/lntypes"
)

// arkChannelPaymentBridge adapts the swap SDK to waved's process-owned native
// lnd channel controller without introducing an in-process gRPC loop.
type arkChannelPaymentBridge struct {
	rpc *waved.RPCServer
}

// PrepareIncomingPayment installs the known-preimage native invoice.
func (b *arkChannelPaymentBridge) PrepareIncomingPayment(ctx context.Context,
	preimage lntypes.Preimage, amount btcutil.Amount) error {

	return b.rpc.PrepareArkChannelIncomingPayment(ctx, preimage, amount)
}

// RegisterIncomingPayment binds the public future SCID at the hub and returns
// the minimum CLTV delta in blocks required by its private channel payment.
func (b *arkChannelPaymentBridge) RegisterIncomingPayment(ctx context.Context,
	hash lntypes.Hash, amount btcutil.Amount, reservedSCID uint64) (uint32,
	error) {

	return b.rpc.RegisterArkChannelIncomingPayment(
		ctx, hash, amount, reservedSCID,
	)
}

// WaitIncomingPayment waits for private lnd acceptance and channel activation.
func (b *arkChannelPaymentBridge) WaitIncomingPayment(ctx context.Context,
	hash lntypes.Hash) (arkchannel.ID, bool, error) {

	return b.rpc.WaitArkChannelIncomingPayment(ctx, hash)
}

// SettleIncomingPayment releases the selected private hold invoice.
func (b *arkChannelPaymentBridge) SettleIncomingPayment(ctx context.Context,
	preimage lntypes.Preimage) error {

	return b.rpc.SettleArkChannelIncomingPayment(ctx, preimage)
}

// CancelIncomingPayment fails the private hold invoice and pending intent.
func (b *arkChannelPaymentBridge) CancelIncomingPayment(ctx context.Context,
	hash lntypes.Hash, reason string) error {

	return b.rpc.CancelArkChannelIncomingPayment(ctx, hash, reason)
}

var _ swaps.ArkChannelPaymentBridge = (*arkChannelPaymentBridge)(nil)
