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

	return c.QuotePayViaLightningWithOptions(
		ctx, invoice, InSwapOptions{
			MaxFeeSat: maxFeeSat,
		},
	)
}

// QuotePayViaLightningWithCredits previews a pay-side in-swap with optional
// credit use.
func (c *SwapClient) QuotePayViaLightningWithCredits(ctx context.Context,
	invoice string, maxFeeSat uint64, maxCreditSat uint64) (*InSwapQuote,
	error) {

	return c.QuotePayViaLightningWithOptions(ctx, invoice, InSwapOptions{
		MaxFeeSat:    maxFeeSat,
		MaxCreditSat: maxCreditSat,
	})
}

// QuotePayViaLightningWithOptions previews a pay-side in-swap with explicit
// fee and credit limits.
func (c *SwapClient) QuotePayViaLightningWithOptions(ctx context.Context,
	invoice string, options InSwapOptions) (*InSwapQuote, error) {

	if c == nil || c.server == nil {
		return nil, fmt.Errorf("swap server is required")
	}
	if err := validateInSwapOptions(options); err != nil {
		return nil, err
	}

	var (
		quote          *InSwapQuote
		legacyFeeTerms bool
	)
	if server, ok := c.server.(interface {
		QuoteInSwapWithOptions(context.Context, string, InSwapOptions,
			[]byte) (*InSwapQuote, error)
	}); ok {

		accountKey, err := c.inSwapQuoteAccountKey(ctx)
		if err != nil {
			return nil, err
		}

		quoted, err := server.QuoteInSwapWithOptions(
			ctx, invoice, options, accountKey,
		)
		if err != nil {
			return nil, err
		}
		quote = quoted
	} else if server, ok := c.server.(interface {
		QuoteInSwapWithCredits(context.Context, string, uint64, []byte,
			uint64) (*InSwapQuote, error)
	}); ok {

		legacyFeeTerms = true
		accountKey, err := c.inSwapQuoteAccountKey(ctx)
		if err != nil {
			return nil, err
		}

		quoted, err := server.QuoteInSwapWithCredits(
			ctx, invoice, options.MaxFeeSat, accountKey,
			options.MaxCreditSat,
		)
		if err != nil {
			return nil, err
		}
		quote = quoted
	} else {
		legacyFeeTerms = true
		var err error
		quote, err = c.server.QuoteInSwap(
			ctx, invoice, options.MaxFeeSat,
		)
		if err != nil {
			return nil, err
		}
	}

	if legacyFeeTerms {
		normalizeInSwapQuoteFeeTerms(quote)
	}
	if err := validateInSwapPreview(
		invoice, quote, c.chainParams,
	); err != nil {
		return nil, err
	}

	return quote, nil
}

// validateInSwapOptions rejects values that cannot be represented by durable
// SQLite and PostgreSQL BIGINT columns.
func validateInSwapOptions(options InSwapOptions) error {
	switch {
	case options.MaxFeeSat > maxInt64Uint:
		return fmt.Errorf("max fee %d sat exceeds int64",
			options.MaxFeeSat)

	case options.MaxCreditSat > maxInt64Uint &&
		options.MaxCreditSat != AllAvailableCredit:
		return fmt.Errorf("max credit %d sat exceeds int64",
			options.MaxCreditSat)

	default:
		return nil
	}
}

// inSwapQuoteAccountKey returns the wallet account used to resolve any credit
// requirements attached to a quote.
func (c *SwapClient) inSwapQuoteAccountKey(ctx context.Context) ([]byte,
	error) {

	if c == nil || c.daemon == nil {
		return nil, fmt.Errorf("daemon is not configured")
	}

	accountKey, err := c.daemon.IdentityPubKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("get credit account pubkey: %w", err)
	}

	return accountKey.SerializeCompressed(), nil
}

// normalizeInSwapQuoteFeeTerms supplies the conservative legacy split used
// when an older server returns no decomposition for a non-zero total fee.
func normalizeInSwapQuoteFeeTerms(quote *InSwapQuote) {
	if quote == nil {
		return
	}

	quote.ServerFeeSat, quote.RoutingFeeBudgetSat =
		normalizeInSwapFeeTerms(
			quote.FeeSat, quote.ServerFeeSat,
			quote.RoutingFeeBudgetSat,
		)
}

// normalizeInSwapConfigFeeTerms supplies the conservative legacy split used
// when an older server returns no decomposition for a non-zero total fee.
func normalizeInSwapConfigFeeTerms(cfg *InSwapConfig) {
	if cfg == nil {
		return
	}

	cfg.ServerFeeSat, cfg.RoutingFeeBudgetSat = normalizeInSwapFeeTerms(
		cfg.FeeSat, cfg.ServerFeeSat, cfg.RoutingFeeBudgetSat,
	)
}

// normalizeInSwapFeeTerms treats an absent legacy decomposition as entirely
// routing-funded so the reported components still reconcile to the total fee.
func normalizeInSwapFeeTerms(feeSat, serverFeeSat,
	routingFeeBudgetSat uint64) (uint64, uint64) {

	if feeSat > 0 && serverFeeSat == 0 && routingFeeBudgetSat == 0 {
		return 0, feeSat
	}

	return serverFeeSat, routingFeeBudgetSat
}

// validateInSwapFeeTerms verifies the server's fee decomposition is durable
// and reconciles exactly to the accepted total fee.
func validateInSwapFeeTerms(feeSat, serverFeeSat, estimatedRoutingFeeSat,
	routingFeeBudgetSat uint64) error {

	switch {
	case feeSat > maxInt64Uint:
		return fmt.Errorf("in-swap fee overflows int64 range")

	case serverFeeSat > maxInt64Uint:
		return fmt.Errorf("in-swap server fee overflows int64 range")

	case estimatedRoutingFeeSat > maxInt64Uint:
		return fmt.Errorf("in-swap estimated routing fee overflows " +
			"int64 range")

	case routingFeeBudgetSat > maxInt64Uint:
		return fmt.Errorf("in-swap routing fee budget overflows " +
			"int64 range")

	case serverFeeSat > feeSat ||
		routingFeeBudgetSat != feeSat-serverFeeSat:
		return fmt.Errorf("in-swap fee split does not reconcile: "+
			"service fee %d + routing fee budget %d != "+
			"total fee %d", serverFeeSat, routingFeeBudgetSat,
			feeSat)

	default:
		return nil
	}
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

	err = validateInSwapFeeTerms(
		quote.FeeSat, quote.ServerFeeSat, quote.EstimatedRoutingFeeSat,
		quote.RoutingFeeBudgetSat,
	)
	if err != nil {
		return fmt.Errorf("invalid in-swap quote: %w", err)
	}

	if expectedInvoiceSat > maxInt64Uint-quote.FeeSat {
		return fmt.Errorf("in-swap quote amount overflows int64 range")
	}

	expectedAmountSat := expectedInvoiceSat + quote.FeeSat
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
// invoice and fee limit before the client funds the quoted vHTLC.
func validateInSwapQuote(invoice string, maxFeeSat uint64, cfg *InSwapConfig,
	chainParams *chaincfg.Params) error {

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

	err = validateInSwapFeeTerms(
		cfg.FeeSat, cfg.ServerFeeSat, 0, cfg.RoutingFeeBudgetSat,
	)
	if err != nil {
		return fmt.Errorf("invalid in-swap quote: %w", err)
	}

	if amountSat > maxInt64Uint-cfg.FeeSat {
		return fmt.Errorf("in-swap amount overflows int64 range")
	}

	expectedAmountSat := amountSat + cfg.FeeSat
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
