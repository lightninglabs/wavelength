package wavewalletdk

import (
	"context"
	"errors"

	"github.com/lightninglabs/wavelength/waved"
)

// ErrLightningReceiptsUnavailable rejects targeted source reads on handles
// without the embedded wallet-layer adapter.
var ErrLightningReceiptsUnavailable = errors.New("lightning receipts require " +
	"an embedded wallet runtime")

// LightningReceiptSource identifies an ordinary or credit invoice receive.
type LightningReceiptSource = waved.LightningReceiptSource

const (
	// LightningReceiptSwap selects a persisted Bitcoin Lightning receive.
	LightningReceiptSwap = waved.LightningReceiptSwap

	// LightningReceiptCredit selects a persisted credit micropayment
	// receive.
	LightningReceiptCredit = waved.LightningReceiptCredit
)

// LightningReceiptReference names the typed source returned by Receive.
type LightningReceiptReference = waved.LightningReceiptReference

// LightningReceiptRequest checks at most 64 enrolled invoice sources.
type LightningReceiptRequest = waved.LightningReceiptRequest

// LightningReceipt contains original source admission and current canonical
// status. Only complete is eligible for amount presentation, after consent and
// durable host deduplication. It does not assert who supplied the payment.
type LightningReceipt = waved.LightningReceipt

// ReadLightningReceipts checks explicitly enrolled local invoice identities.
// It uses targeted durable source reads, validates their Bitcoin invoice and
// principal, and projects their committed local state through the normal
// canonical sink. Pending means remote/FSM progress may still be required.
// No remote mutation, broad historical baseline or safe suspension is claimed.
func (c *Client) ReadLightningReceipts(ctx context.Context,
	request LightningReceiptRequest) ([]LightningReceipt, error) {

	if c == nil || c.readLightningReceipts == nil {
		return nil, ErrLightningReceiptsUnavailable
	}
	callCtx, release, err := c.beginLocalCall(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	return c.readLightningReceipts(callCtx, request)
}
