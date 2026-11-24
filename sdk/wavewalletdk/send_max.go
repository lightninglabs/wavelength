package wavewalletdk

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/coinselect"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxSendQuote is a repeatable quote lookup. Searching must not create
// prepared intents: those can earmark credits even before dispatch.
type maxSendQuote func(context.Context, uint64) (
	*swapclientrpc.QuotePayResponse, error)

// prepareMaxSend resolves Max through existing read RPCs, then creates one
// normal intent. Offline estimates cannot establish an affordable maximum.
// Funds can still change after prepare, just as with other prepared payments.
func (c *Client) prepareMaxSend(ctx context.Context, req PrepareSendRequest) (
	*PrepareSendResult, error) {

	if strings.TrimSpace(req.Invoice) == "" ||
		strings.TrimSpace(req.OnchainAddress) != "" ||
		req.AmountSat != 0 || req.SweepAll {
		return nil, fmt.Errorf("Max requires an amountless invoice " +
			"without an entered amount")
	}
	req.Invoice = strings.TrimSpace(req.Invoice)
	info, err := c.daemon.GetInfo(ctx, &waverpc.GetInfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("read Max payment policy: %w", err)
	}
	decoded, err := DecodeInvoice(req.Invoice, info.GetNetwork())
	if err != nil {
		return nil, err
	}
	if decoded.AmountSat != nil {
		return nil, fmt.Errorf("Max requires an invoice without an " +
			"amount")
	}
	terms := info.GetServerInfo()
	if terms == nil {
		return nil, fmt.Errorf("reconnect to calculate the available " +
			"balance")
	}
	floor := max(terms.GetDustLimit(), terms.GetMinVtxoAmountSat())
	if floor > math.MaxInt64 {
		return nil, fmt.Errorf("invalid minimum change policy")
	}
	listed, err := c.daemon.ListVTXOs(ctx, &waverpc.ListVTXOsRequest{
		StatusFilter:           waverpc.VTXOStatus_VTXO_STATUS_LIVE,
		ExcludeCheckpointPsbts: true,
	})
	if err != nil {
		return nil, fmt.Errorf("read available VTXOs: %w", err)
	}
	var coins []btcutil.Amount
	for _, v := range listed.GetVtxos() {
		if v.GetStatus() == waverpc.VTXOStatus_VTXO_STATUS_LIVE &&
			v.GetAssetRef() == "" && v.GetAmountSat() > 0 {

			coins = append(coins, btcutil.Amount(v.GetAmountSat()))
		}
	}
	lookup := func(ctx context.Context, amount uint64) (
		*swapclientrpc.QuotePayResponse, error) {

		return c.swaps.QuotePay(ctx, &swapclientrpc.QuotePayRequest{
			Invoice: req.Invoice, AmountSat: amount,
			MaxFeeSat:    req.MaxFeeSat,
			MaxCreditSat: req.MaxCreditSat,
		})
	}
	amount, quoted, err := maximumInvoiceAmount(
		ctx, coins, btcutil.Amount(floor), req, lookup,
	)
	if err != nil {
		return nil, err
	}
	req.MaxAmount = false
	req.AmountSat = amount
	prepared, err := c.PrepareSend(ctx, req)
	if err != nil {
		return nil, err
	}
	// The final prepare quotes again. Never present the search result with
	// a different fee/debit or an offline fallback. No Send has run.
	// An abandoned intent expires normally. Its temporary earmark applies
	// to automatic credit redemption only, so quoting again remains safe.
	cost := quoted.GetAmountSat() +
		quoted.GetCreditQuote().GetCreditTopupSat()
	if !prepared.FeeKnown || !prepared.TotalOutflowKnown ||
		prepared.AmountSat != int64(amount) ||
		prepared.ExpectedFeeSat != int64(quoted.GetFeeSat()) ||
		prepared.ExpectedTotalOutflowSat != int64(cost) ||
		!maxCreditPlanMatches(
			prepared.CreditPreview, quoted.GetCreditQuote(),
		) {
		return nil, ErrMaxQuoteChanged
	}

	return prepared, nil
}

// maxCreditPlanMatches keeps the independently funded legs bound to the plan
// checked during search, even if a second quote has the same total debit.
func maxCreditPlanMatches(prepared *CreditPreview,
	quoted *swapclientrpc.CreditQuote) bool {

	if prepared == nil {
		return quoted == nil || quoted.GetCreditAppliedSat() == 0 &&
			quoted.GetCreditShortfallSat() == 0 &&
			quoted.GetCreditTopupSat() == 0
	}

	return prepared.MustUseCredit == quoted.GetMustUseCredit() &&
		prepared.CreditAppliedSat == quoted.GetCreditAppliedSat() &&
		prepared.CreditShortfallSat == quoted.GetCreditShortfallSat() &&
		prepared.CreditTopupSat == quoted.GetCreditTopupSat() &&
		prepared.ArkFundingSat == quoted.GetArkFundingSat()
}

// maximumInvoiceAmount solves amount + quoted fee = input budget + applied
// credits. Each iteration requotes the resulting amount instead of probing
// arbitrary sub-floor amounts. No solution is guessed: the final quote must
// pass the actual funding selector. Rounded fees can skip an exact drain, so
// retry the exact-input prefixes and the minimum-change budget in that case.
func maximumInvoiceAmount(ctx context.Context, coins []btcutil.Amount,
	floor btcutil.Amount, req PrepareSendRequest, lookup maxSendQuote) (
	uint64, *swapclientrpc.QuotePayResponse, error) {

	coins = append([]btcutil.Amount(nil), coins...)
	sort.Slice(coins, func(i, j int) bool { return coins[i] > coins[j] })
	var total uint64
	for _, coin := range coins {
		if coin <= 0 || uint64(coin) > btcutil.MaxSatoshi-total {
			return 0, nil, fmt.Errorf("invalid available balance")
		}
		total += uint64(coin)
	}
	if req.MaxCreditSat > btcutil.MaxSatoshi-total {
		return 0, nil, fmt.Errorf("invalid credit limit")
	}
	calculator := maxSendCalculator{coins: coins, floor: floor, req: req,
		lookup: lookup, cache: make(
			map[uint64]*swapclientrpc.QuotePayResponse,
		)}
	amount, q, err := calculator.solve(ctx, total)
	if err != nil {
		return 0, nil, err
	}
	if amount > 0 {
		return amount, q, nil
	}
	var safeBudget uint64
	if total >= uint64(floor) {
		safeBudget = total - uint64(floor)
	}
	budgets := []uint64{safeBudget}
	var prefix uint64
	for _, coin := range coins {
		prefix += uint64(coin)
		if prefix > safeBudget && prefix < total {
			budgets = append(budgets, prefix)
		}
	}
	var best uint64
	var bestQuote *swapclientrpc.QuotePayResponse
	for _, budget := range budgets {
		candidate, cq, err := calculator.solve(ctx, budget)
		if err != nil {
			return 0, nil, err
		}
		if candidate > best {
			best, bestQuote = candidate, cq
		}
	}
	if best == 0 {
		return 0, nil, ErrMaxInsufficientBalance
	}

	return best, bestQuote, nil
}

// maxSendCalculator keeps quote caching local to one Max request. No intent
// or reservation is created until the final amount has been verified.
type maxSendCalculator struct {
	coins  []btcutil.Amount
	floor  btcutil.Amount
	req    PrepareSendRequest
	lookup maxSendQuote
	cache  map[uint64]*swapclientrpc.QuotePayResponse
}

// quote reads and validates a repeatable quote, caching only successful reads.
func (c *maxSendCalculator) quote(ctx context.Context, amount uint64) (
	*swapclientrpc.QuotePayResponse, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q, ok := c.cache[amount]; ok {
		return q, nil
	}
	q, err := c.lookup(ctx, amount)
	if err != nil {
		return nil, err
	}
	if q == nil || q.GetInvoiceAmountSat() != amount ||
		q.GetFeeSat() > btcutil.MaxSatoshi-amount ||
		q.GetAmountSat() > btcutil.MaxSatoshi ||
		q.GetCreditQuote().GetCreditAppliedSat() > amount+
			q.GetFeeSat() ||
		q.GetCreditQuote().GetCreditTopupSat() >
			btcutil.MaxSatoshi-q.GetAmountSat() {
		return nil, fmt.Errorf("invalid Max payment quote")
	}
	c.cache[amount] = q

	return q, nil
}

// solve requotes the fee-adjusted principal until it fits or rounding cycles.
// Only amounts that pass the actual funding selector can be returned.
func (c *maxSendCalculator) solve(ctx context.Context, budget uint64) (uint64,
	*swapclientrpc.QuotePayResponse, error) {

	// A sub-floor funding leg cannot be created. Existing credit
	// may still pay an invoice, without consuming these Bitcoin
	// inputs.
	if budget < uint64(c.floor) {
		budget = 0
	}
	upper := budget + c.req.MaxCreditSat
	amount := upper
	seen := make(map[uint64]bool)
	var best uint64
	var bestQuote *swapclientrpc.QuotePayResponse
	for step := 0; step < 8 && amount > 0 && !seen[amount]; step++ {
		seen[amount] = true
		q, err := c.quote(ctx, amount)
		// At a credit-only upper bound, principal plus its fee
		// can exceed the caller cap. A one-sat read obtains the
		// fee without authorizing more credit, then retries the
		// bounded amount.
		if err != nil && step == 0 && budget == 0 &&
			c.req.MaxCreditSat > 0 && maxAmountPolicyRejection(
			err,
		) {

			probe, probeErr := c.quote(ctx, 1)
			if probeErr != nil {
				if maxAmountPolicyRejection(probeErr) {
					return 0, nil, nil
				}

				return 0, nil, probeErr
			}
			if probe.GetFeeSat() >= upper {
				break
			}
			amount = upper - probe.GetFeeSat()

			continue
		}
		if err != nil {
			if maxAmountPolicyRejection(err) {
				if budget == 0 && c.req.MaxCreditSat > 0 {
					return c.creditOnlyMaximum(
						ctx, amount-1,
					)
				}

				break
			}

			return 0, nil, fmt.Errorf("calculate Max payment: %w",
				err)
		}
		cost := q.GetAmountSat() + q.
			GetCreditQuote().
			GetCreditTopupSat()
		if cost <= budget && amount > best &&
			maxQuoteFundable(c.coins, c.floor, c.req, q) {

			best, bestQuote = amount, q
		}
		available := budget + q.
			GetCreditQuote().
			GetCreditAppliedSat()
		if q.GetFeeSat() >= available {
			break
		}
		next := available - q.GetFeeSat()
		if next == amount {
			return best, bestQuote, nil
		}
		if next > upper {
			break
		}
		amount = next
	}

	return best, bestQuote, nil
}

// creditOnlyMaximum handles an amount-dependent fee that exceeds the cap
// after the cheap one-sat fee probe. Credit-cap rejections are upper bounds;
// service/transport failures are not. Only existing-credit quotes with no
// Bitcoin debit qualify, and the original cap is never raised.
func (c *maxSendCalculator) creditOnlyMaximum(ctx context.Context,
	upper uint64) (uint64, *swapclientrpc.QuotePayResponse, error) {

	var best uint64
	var bestQuote *swapclientrpc.QuotePayResponse
	for low, high := uint64(1), upper; low <= high; {
		amount := low + (high-low)/2
		q, err := c.quote(ctx, amount)
		if err != nil {
			if !maxAmountPolicyRejection(err) {
				return 0, nil, err
			}
			high = amount - 1

			continue
		}
		cost := q.GetAmountSat() +
			q.GetCreditQuote().GetCreditTopupSat()
		if cost == 0 {
			best, bestQuote = amount, q
			low = amount + 1
		} else {
			high = amount - 1
		}
	}
	if !maxQuoteFundable(c.coins, c.floor, c.req, bestQuote) {
		return 0, nil, nil
	}

	return best, bestQuote, nil
}

// maxAmountPolicyRejection identifies read-only quote boundaries that cannot
// fund this candidate. Connectivity and unknown failures always propagate.
func maxAmountPolicyRejection(err error) bool {
	message := err.Error()

	switch status.Code(err) {
	case codes.Unknown:
		return strings.Contains(message, swapclientrpc.PayCreditCapHint)

	case codes.InvalidArgument:
		return strings.Contains(
			message, swapclientrpc.PayBelowMinimumHint,
		)

	default:
		return false
	}
}

// maxQuoteFundable verifies both independently funded legs in dispatch order.
// A top-up's change becomes available before the remaining Ark pay leg starts.
func maxQuoteFundable(coins []btcutil.Amount, floor btcutil.Amount,
	req PrepareSendRequest, q *swapclientrpc.QuotePayResponse) bool {

	if q == nil || q.GetExceedsMaxFee() {
		return false
	}
	cp := q.GetCreditQuote()
	if cp.GetCreditTopupSat() > req.MaxCreditTopupSat ||
		cp.GetCreditAppliedSat() > req.MaxCreditSat ||
		cp.GetCreditShortfallSat() >
			req.MaxCreditSat-cp.GetCreditAppliedSat() {
		return false
	}
	return coinselect.SelectSequential(
		coins, floor,
		btcutil.Amount(cp.GetCreditTopupSat()),
		btcutil.Amount(q.GetAmountSat()),
	) == nil
}
