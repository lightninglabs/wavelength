package wavewalletdk

import (
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/sdk/bolt11"
	"github.com/lightningnetwork/lnd/zpay32"
)

// InvoiceDetails describes a decoded BOLT-11, before any quote or wallet call.
// AmountSat is absent when the payer must choose the amount. A present value
// rounds the signed millisatoshi amount up for display. Hosts must prepare a
// fixed invoice with AmountSat zero so its exact signed amount remains in
// force.
type InvoiceDetails struct {
	AmountSat     *uint64 `json:"amount_sat"`
	Description   string  `json:"description"`
	ExpiresAtUnix int64   `json:"expires_at_unix"`
}

// DecodeInvoice validates the full invoice against the selected Bitcoin
// network. It does not start a wallet or contact a service. Amounts remain
// signed invoice data; a missing amount is distinct from a malformed or expired
// invoice.
func DecodeInvoice(invoice, network string) (*InvoiceDetails, error) {
	var params *chaincfg.Params
	switch network {
	case "mainnet":
		params = &chaincfg.MainNetParams

	case "testnet", "testnet3":
		params = &chaincfg.TestNet3Params

	case "testnet4":
		params = &chaincfg.TestNet4Params

	case "signet":
		params = &chaincfg.SigNetParams

	case "regtest":
		params = &chaincfg.RegressionNetParams

	default:
		return nil, fmt.Errorf("unknown bitcoin network")
	}
	decoded, err := zpay32.Decode(invoice, params)
	if err != nil {
		return nil, fmt.Errorf("invalid invoice: %w", err)
	}
	expiry := decoded.Timestamp.Add(decoded.Expiry())
	if !expiry.After(time.Now()) {
		return nil, fmt.Errorf("invoice has expired")
	}
	result := &InvoiceDetails{ExpiresAtUnix: expiry.Unix()}
	if decoded.MilliSat != nil {
		msat, err := bolt11.AmountMSat(decoded, 0)
		if err != nil {
			return nil, err
		}
		sat := (msat + 999) / 1000
		result.AmountSat = &sat
	}
	if decoded.Description != nil {
		result.Description = *decoded.Description
	}

	return result, nil
}
