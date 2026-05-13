package swaps

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
)

// maxInt64Uint is the largest positive int64 value represented as uint64 for
// checked amount conversions.
const maxInt64Uint = uint64(1<<63 - 1)

// decodePayInvoice decodes one pay-side BOLT-11 invoice against the client's
// configured Bitcoin network.
func decodePayInvoice(invoice string,
	chainParams *chaincfg.Params) (*zpay32.Invoice, error) {

	if chainParams == nil {
		return nil, fmt.Errorf("pay invoice chain params are required")
	}

	decoded, err := zpay32.Decode(invoice, chainParams)
	if err != nil {
		return nil, fmt.Errorf("decode invoice: %w", err)
	}

	return decoded, nil
}

// extractInvoiceAmountSat extracts a positive whole-satoshi invoice amount.
func extractInvoiceAmountSat(msat *lnwire.MilliSatoshi) (uint64, error) {
	if msat == nil {
		return 0, fmt.Errorf("invoice amount is required")
	}

	amountMSat := uint64(*msat)
	if amountMSat == 0 {
		return 0, fmt.Errorf("invoice amount must be positive")
	}
	if amountMSat%1000 != 0 {
		return 0, fmt.Errorf("invoice amount must be whole satoshis")
	}

	return amountMSat / 1000, nil
}

// validateInSwapQuote verifies that the server quote is bound to the caller's
// invoice and fee limit before the client funds the quoted vHTLC.
func validateInSwapQuote(invoice string, maxFeeSat uint64, cfg *InSwapConfig,
	chainParams *chaincfg.Params) error {

	if cfg == nil {
		return fmt.Errorf("in-swap config is required")
	}
	if cfg.AmountSat <= 0 {
		return fmt.Errorf("in-swap amount must be positive")
	}

	decoded, err := decodePayInvoice(invoice, chainParams)
	if err != nil {
		return err
	}

	if decoded.PaymentHash == nil {
		return fmt.Errorf("invoice payment hash is required")
	}

	invoiceHash := lntypes.Hash(*decoded.PaymentHash)
	if invoiceHash != cfg.PaymentHash {
		return fmt.Errorf("in-swap payment hash does not match invoice")
	}

	amountSat, err := extractInvoiceAmountSat(decoded.MilliSat)
	if err != nil {
		return err
	}

	if cfg.FeeSat > maxFeeSat {
		return fmt.Errorf("in-swap fee %d exceeds max fee %d",
			cfg.FeeSat, maxFeeSat)
	}

	if cfg.FeeSat > maxInt64Uint {
		return fmt.Errorf("in-swap fee overflows int64 range")
	}

	if amountSat > maxInt64Uint-cfg.FeeSat {
		return fmt.Errorf("in-swap amount overflows int64 range")
	}

	expectedAmountSat := amountSat + cfg.FeeSat
	if uint64(cfg.AmountSat) != expectedAmountSat {
		return fmt.Errorf("in-swap amount %d does not equal invoice "+
			"amount %d plus fee %d", cfg.AmountSat, amountSat,
			cfg.FeeSat)
	}

	return nil
}
