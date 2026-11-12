package swaps

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/v2"
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

func extractInvoiceAmountMsat(msat *lnwire.MilliSatoshi) (uint64, error) {
	if msat == nil {
		return 0, fmt.Errorf("invoice amount is required")
	}

	amountMSat := uint64(*msat)
	if amountMSat == 0 {
		return 0, fmt.Errorf("invoice amount must be positive")
	}

	return amountMSat, nil
}

func ceilMsatToSat(msat uint64) uint64 {
	if msat == 0 {
		return 0
	}

	return (msat + 999) / 1000
}

// QuotePayViaLightning previews a pay-side in-swap without creating durable
// swap state or starting the pay FSM.
func (c *SwapClient) QuotePayViaLightning(ctx context.Context, invoice string,
	maxFeeSat uint64) (*InSwapQuote, error) {

	return c.QuotePayViaLightningWithCredits(ctx, invoice, maxFeeSat, 0)
}

// QuotePayViaLightningWithCredits previews a pay-side in-swap with optional
// credit use.
func (c *SwapClient) QuotePayViaLightningWithCredits(ctx context.Context,
	invoice string, maxFeeSat uint64, maxCreditSat uint64) (*InSwapQuote,
	error) {

	if c == nil || c.server == nil {
		return nil, fmt.Errorf("swap server is required")
	}

	var quote *InSwapQuote
	if server, ok := c.server.(interface {
		QuoteInSwapWithCredits(context.Context, string, uint64, []byte,
			uint64) (*InSwapQuote, error)
	}); ok {

		accountKey, err := c.daemon.IdentityPubKey(ctx)
		if err != nil {
			return nil, fmt.Errorf("get credit account pubkey: %w",
				err)
		}

		quote, err = server.QuoteInSwapWithCredits(
			ctx, invoice, maxFeeSat,
			accountKey.SerializeCompressed(), maxCreditSat,
		)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		quote, err = c.server.QuoteInSwap(ctx, invoice, maxFeeSat)
		if err != nil {
			return nil, err
		}
	}

	if err := validateInSwapPreview(
		invoice, quote, maxCreditSat, c.chainParams,
	); err != nil {
		return nil, err
	}

	return quote, nil
}

// validateInSwapPreview verifies that a server quote is bound to the caller's
// invoice and credit cap before the wallet renders a user-visible preview.
func validateInSwapPreview(invoice string, quote *InSwapQuote,
	maxCreditSat uint64, chainParams *chaincfg.Params) error {

	if quote == nil {
		return fmt.Errorf("in-swap quote is required")
	}
	if quote.InvoiceAmountSat == 0 {
		return fmt.Errorf("in-swap quote invoice amount must be " +
			"positive")
	}
	if quote.AmountSat == 0 &&
		quote.SettlementType != SettlementTypeCredit {
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

	amountMsat, err := extractInvoiceAmountMsat(decoded.MilliSat)
	if err != nil {
		return err
	}

	expectedInvoiceSat := ceilMsatToSat(amountMsat)
	if expectedInvoiceSat != quote.InvoiceAmountSat {
		return fmt.Errorf("in-swap quote invoice amount %d does not "+
			"match invoice amount %d", quote.InvoiceAmountSat,
			expectedInvoiceSat)
	}

	if quote.FeeSat > maxInt64Uint {
		return fmt.Errorf("in-swap quote fee overflows int64 range")
	}

	if expectedInvoiceSat > maxInt64Uint-quote.FeeSat {
		return fmt.Errorf("in-swap quote amount overflows int64 range")
	}

	expectedAmountSat := expectedInvoiceSat + quote.FeeSat
	if err := validatePayCredit(
		quote.SettlementType, quote.CreditQuote, maxCreditSat,
		expectedAmountSat, false,
	); err != nil {
		return err
	}
	switch quote.SettlementType {
	case "", SettlementTypeLightning, SettlementTypeInArk:
	case SettlementTypeCredit:
		expectedAmountSat = 0

	case SettlementTypeMixed:
		if quote.CreditQuote == nil {
			return fmt.Errorf("mixed in-swap quote missing " +
				"credit quote")
		}
		expectedAmountSat = quote.CreditQuote.ArkFundingSat
	}

	if quote.AmountSat != expectedAmountSat {
		return fmt.Errorf("in-swap quote amount %d does not equal "+
			"invoice amount %d plus fee %d", quote.AmountSat,
			expectedInvoiceSat, quote.FeeSat)
	}

	return nil
}

// validateInSwapQuote verifies that the server quote is bound to the caller's
// invoice, fee limit and credit cap before accepting completion or funding.
func validateInSwapQuote(invoice string, maxFeeSat, maxCreditSat uint64,
	cfg *InSwapConfig, chainParams *chaincfg.Params) error {

	if cfg == nil {
		return fmt.Errorf("in-swap config is required")
	}
	if cfg.AmountSat <= 0 && cfg.SettlementType != SettlementTypeCredit {
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

	amountMsat, err := extractInvoiceAmountMsat(decoded.MilliSat)
	if err != nil {
		return err
	}
	amountSat := ceilMsatToSat(amountMsat)

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
	if err := validatePayCredit(
		cfg.SettlementType, cfg.CreditQuote, maxCreditSat,
		expectedAmountSat, true,
	); err != nil {
		return err
	}
	switch cfg.SettlementType {
	case "", SettlementTypeLightning, SettlementTypeInArk:
	case SettlementTypeCredit:
		if cfg.Preimage == nil {
			return fmt.Errorf("credit in-swap config missing " +
				"preimage")
		}
		if cfg.Preimage.Hash() != cfg.PaymentHash {
			return fmt.Errorf("credit in-swap preimage does not " +
				"match payment hash")
		}
		expectedAmountSat = 0

	case SettlementTypeMixed:
		if cfg.CreditQuote == nil {
			return fmt.Errorf("mixed in-swap config missing " +
				"credit quote")
		}
		expectedAmountSat = cfg.CreditQuote.ArkFundingSat
	}

	if uint64(cfg.AmountSat) != expectedAmountSat {
		return fmt.Errorf("in-swap amount %d does not equal invoice "+
			"amount %d plus fee %d", cfg.AmountSat, amountSat,
			cfg.FeeSat)
	}

	return nil
}

// validatePayCredit binds the server's credit leg to local authorization and
// the invoice plus fee. Preview may include a shortfall; creation must return
// a fully reserved plan. A completed credit pay can report a larger original
// reservation than its final invoice plus charged fee after unused routing
// budget is released. Mandatory credit never expands caller authority.
func validatePayCredit(settlement SettlementType, quote *CreditQuote,
	maxCreditSat, totalSat uint64, completed bool) error {

	switch settlement {
	case "", SettlementTypeLightning, SettlementTypeInArk,
		SettlementTypeCredit, SettlementTypeMixed:
	default:
		return fmt.Errorf("unknown in-swap settlement type %q",
			settlement)
	}

	if quote == nil {
		if settlement == SettlementTypeCredit ||
			settlement == SettlementTypeMixed {
			return fmt.Errorf("credit settlement missing credit " +
				"quote")
		}

		return nil
	}
	applied, shortfall := quote.CreditAppliedSat, quote.CreditShortfallSat
	if applied > maxCreditSat || shortfall > maxCreditSat-applied {
		return fmt.Errorf("credit requirement exceeds "+
			"max_credit_sat %d", maxCreditSat)
	}
	if completed && (shortfall != 0 || quote.CreditTopupSat != 0) {
		return fmt.Errorf("completed credit reservation contains a " +
			"shortfall or top-up")
	}
	required := applied + shortfall
	switch settlement {
	case SettlementTypeCredit:
		if maxCreditSat == 0 || required < totalSat ||
			(!completed && required != totalSat) ||
			quote.ArkFundingSat != 0 {
			return fmt.Errorf("credit settlement does not cover " +
				"invoice plus fee within authorization")
		}

	case SettlementTypeMixed:
		if applied == 0 || shortfall != 0 || applied >= totalSat ||
			quote.ArkFundingSat != totalSat-applied {
			return fmt.Errorf("mixed credit and Ark funding do " +
				"not equal invoice plus fee")
		}

	case "", SettlementTypeLightning, SettlementTypeInArk:
		if required != 0 || quote.MustUseCredit ||
			quote.CreditTopupSat != 0 {
			return fmt.Errorf("non-credit settlement contains " +
				"credit reservation")
		}

	}

	return nil
}
