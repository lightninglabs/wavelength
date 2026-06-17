package swaps

import (
	"context"
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

// QuotePayViaLightning previews a pay-side in-swap without creating durable
// swap state or starting the pay FSM.
func (c *SwapClient) QuotePayViaLightning(ctx context.Context,
	invoice string, maxFeeSat uint64) (*InSwapQuote, error) {

	if c == nil || c.server == nil {
		return nil, fmt.Errorf("swap server is required")
	}

	quote, err := c.server.QuoteInSwap(ctx, invoice, maxFeeSat)
	if err != nil {
		return nil, err
	}

	if err := validateInSwapPreview(
		invoice, quote, c.chainParams,
	); err != nil {
		return nil, err
	}

	return quote, nil
}

// validateInSwapPreview verifies that a server quote is bound to the caller's
// invoice before the wallet renders it as a user-visible preview.
func validateInSwapPreview(invoice string, quote *InSwapQuote,
	chainParams *chaincfg.Params) error {

	if quote == nil {
		return fmt.Errorf("in-swap quote is required")
	}
	if quote.InvoiceAmountSat == 0 {
		return fmt.Errorf("in-swap quote invoice amount must be " +
			"positive")
	}
	if quote.AmountSat == 0 {
		return fmt.Errorf("in-swap quote amount must be positive")
	}
	if quote.SettlementType == "" {
		return fmt.Errorf("in-swap quote settlement type is required")
	}

	decoded, err := decodePayInvoice(invoice, chainParams)
	if err != nil {
		return err
	}

	if decoded.PaymentHash == nil {
		return fmt.Errorf("invoice payment hash is required")
	}

	invoiceHash := lntypes.Hash(*decoded.PaymentHash)
	if invoiceHash != quote.PaymentHash {
		return fmt.Errorf("in-swap quote payment hash does not match " +
			"invoice")
	}

	amountSat, err := extractInvoiceAmountSat(decoded.MilliSat)
	if err != nil {
		return err
	}

	if amountSat != quote.InvoiceAmountSat {
		return fmt.Errorf("in-swap quote invoice amount %d does not "+
			"match invoice amount %d", quote.InvoiceAmountSat,
			amountSat)
	}

	if quote.FeeSat > maxInt64Uint {
		return fmt.Errorf("in-swap quote fee overflows int64 range")
	}

	if amountSat > maxInt64Uint-quote.FeeSat {
		return fmt.Errorf("in-swap quote amount overflows int64 range")
	}

	expectedAmountSat := amountSat + quote.FeeSat
	if quote.AmountSat != expectedAmountSat {
		return fmt.Errorf("in-swap quote amount %d does not equal "+
			"invoice amount %d plus fee %d", quote.AmountSat,
			amountSat, quote.FeeSat)
	}

	return nil
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
