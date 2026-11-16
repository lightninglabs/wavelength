// Package bolt11 validates payment amounts without changing signed invoices.
package bolt11

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/zpay32"
)

// AmountMSat resolves the signed amount or the payer's whole-satoshi amount.
// A caller may repeat a fixed amount, but cannot replace it. Bitcoin's supply
// bound keeps satoshi-to-millisatoshi conversions and downstream sums safe.
func AmountMSat(invoice *zpay32.Invoice, amountSat uint64) (uint64, error) {
	if invoice == nil {
		return 0, fmt.Errorf("invoice is required")
	}
	if amountSat > uint64(btcutil.MaxSatoshi) {
		return 0, fmt.Errorf("amount exceeds bitcoin supply")
	}
	if invoice.MilliSat == nil {
		if amountSat == 0 {
			return 0, fmt.Errorf("invoice amount is required; " +
				"enter an amount in sats")
		}

		return amountSat * 1000, nil
	}
	signed := uint64(*invoice.MilliSat)
	if signed == 0 || signed > uint64(btcutil.MaxSatoshi)*1000 {
		return 0, fmt.Errorf("invoice amount is invalid")
	}
	if amountSat != 0 && amountSat*1000 != signed {
		return 0, fmt.Errorf("amount differs from signed invoice")
	}

	return signed, nil
}
