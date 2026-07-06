//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/darepo-client/coinselect"
	"github.com/lightninglabs/darepo-client/credit"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// router dispatches outbound Send requests to the appropriate daemon
// subsystem based on the destination oneof. It owns no business logic of
// its own: invoice sends route through swapclientserver.StartPay (where
// the swap server transparently picks same-Ark p2p vHTLC vs real
// Lightning per PR #339), and onchain sends route through
// RPCServer.LeaveVTXOs (the existing cooperative-exit RPC, daemon.proto:91).
type router struct {
	deps    *Deps
	runtime *Runtime
	intents *preparedSendStore
}

// newRouter constructs a router bound to the shared wallet runtime.
func newRouter(deps *Deps, runtime *Runtime) *router {
	return &router{
		deps:    deps,
		runtime: runtime,
		intents: newPreparedSendStore(),
	}
}

// PrepareSend validates an outbound payment and records the exact intent that
// a later Send call may consume.
func (r *router) PrepareSend(ctx context.Context,
	req *walletdkrpc.PrepareSendRequest) (*walletdkrpc.PrepareSendResponse,
	error) {

	if r == nil || r.deps == nil {
		return nil, ErrSwapBackendUnavailable
	}
	if req == nil {
		return nil, ErrInvalidDestination
	}

	switch dest := req.GetDestination().(type) {
	case *walletdkrpc.PrepareSendRequest_Invoice:
		return r.prepareInvoice(ctx, dest.Invoice, req)

	case *walletdkrpc.PrepareSendRequest_OnchainAddress:
		return r.prepareOnchain(ctx, dest.OnchainAddress, req)

	default:
		return nil, ErrInvalidDestination
	}
}

// Send consumes a prepared send intent and dispatches it to the right backend.
func (r *router) Send(ctx context.Context, req *walletdkrpc.SendRequest) (
	*walletdkrpc.SendResponse, error) {

	if r == nil || r.deps == nil {
		return nil, ErrSwapBackendUnavailable
	}
	if req == nil {
		return nil, ErrInvalidSendIntent
	}

	intent, err := r.intents.consume(
		strings.TrimSpace(
			req.GetSendIntentId(),
		),
	)
	if err != nil {
		return nil, err
	}

	switch intent.kind {
	case preparedSendInvoice:
		return r.sendInvoiceIntent(ctx, intent)

	case preparedSendOnchain:
		return r.sendOnchainIntent(ctx, intent)

	default:
		return nil, ErrInvalidSendIntent
	}
}

func (r *router) prepareInvoice(ctx context.Context, invoice string,
	req *walletdkrpc.PrepareSendRequest) (*walletdkrpc.PrepareSendResponse,
	error) {

	invoice = strings.TrimSpace(invoice)
	if invoice == "" {
		return nil, ErrInvalidDestination
	}

	decoded, err := decodePreparedInvoice(invoice, r.deps.ChainParams)
	if err != nil {
		return nil, err
	}

	amountSat, err := extractPreparedInvoiceAmountSat(decoded)
	if err != nil {
		return nil, err
	}
	if amountSat > math.MaxInt64 {
		return nil, fmt.Errorf("%w: invoice amount exceeds int64 range",
			ErrAmountInvalid)
	}

	paymentHash := ""
	if decoded.PaymentHash != nil {
		paymentHash = hex.EncodeToString(decoded.PaymentHash[:])
	}

	description := ""
	if decoded.Description != nil {
		description = strings.TrimSpace(*decoded.Description)
	}

	if r.deps.SwapService == nil {
		return nil, ErrSwapBackendUnavailable
	}

	quote, err := r.deps.SwapService.QuotePay(
		ctx, &swapclientrpc.QuotePayRequest{
			Invoice:      invoice,
			MaxFeeSat:    req.GetMaxFeeSat(),
			MaxCreditSat: ^uint64(0),
		},
	)
	if err != nil {
		if !quotePayUnavailable(err) {
			return nil, fmt.Errorf("quote pay: %w", err)
		}
	}

	intent := &preparedSendIntent{
		kind:      preparedSendInvoice,
		invoice:   invoice,
		amountSat: amountSat,
		note:      req.GetNote(),
		maxFeeSat: req.GetMaxFeeSat(),
	}

	preview, err := prepareInvoicePreview(
		invoice, description, paymentHash, amountSat, quote, err,
	)
	if err != nil {
		return nil, err
	}
	if quote != nil && quote.GetCreditQuote() != nil {
		creditQuote := quote.GetCreditQuote()
		intent.creditPreview = preview.creditPreview
		intent.maxCreditSat = saturatingAddSat(
			creditQuote.GetCreditAppliedSat(),
			creditQuote.GetCreditShortfallSat(),
		)
		if creditQuote.GetMustUseCredit() {
			if uint64(amountSat) > intent.maxCreditSat {
				intent.maxCreditSat = uint64(amountSat)
			}
		}
	}

	if _, err := r.intents.put(intent); err != nil {
		return nil, err
	}

	return prepareResponseFromIntent(intent, preview), nil
}

// sendInvoiceIntent routes a prepared BOLT-11 invoice. A credit-backed pay
// (credit-only or mixed rail) is handed off to the durable credit subsystem,
// which performs any Ark top-up and the pay crash-safely under a stable
// idempotency key. A pure-Lightning pay goes straight to the swap subserver.
func (r *router) sendInvoiceIntent(ctx context.Context,
	intent *preparedSendIntent) (*walletdkrpc.SendResponse, error) {

	if r.deps.SwapService == nil {
		return nil, ErrSwapBackendUnavailable
	}

	if intentUsesCredit(intent) {
		return r.sendCreditInvoiceIntent(ctx, intent)
	}

	startResp, err := r.deps.SwapService.StartPay(
		ctx, &swapclientrpc.StartPayRequest{
			Invoice:      intent.invoice,
			MaxFeeSat:    intent.maxFeeSat,
			MaxCreditSat: intent.maxCreditSat,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("start pay: %w", err)
	}

	entry := swapEntryFromSummary(
		startResp.GetSwap(), intent.note, intent.invoice,
		walletdkrpc.EntryKind_ENTRY_KIND_SEND,
	)

	// For invoice sends actual_amount_sat is the swap's negotiated
	// principal: that's what is being paid to the BOLT-11 destination
	// (routing fees are tracked separately via fee_sat on the entry).
	actualAmountSat := startResp.GetSwap().GetAmountSat()
	if actualAmountSat == 0 {
		actualAmountSat = int64(intent.amountSat)
	}

	return &walletdkrpc.SendResponse{
		Entry:           entry,
		ActualAmountSat: actualAmountSat,
	}, nil
}

// sendCreditInvoiceIntent hands a credit-backed pay to the durable credit
// registry under a payment-hash-derived idempotency key, then returns a pending
// wallet entry. The registry's per-operation actor performs the Ark top-up
// (idempotently) and the pay on the daemon root context, so a client disconnect
// or restart never strands or double-funds the credits.
func (r *router) sendCreditInvoiceIntent(ctx context.Context,
	intent *preparedSendIntent) (*walletdkrpc.SendResponse, error) {

	if r.deps.CreditRegistry == nil {
		return nil, fmt.Errorf("%w: credit subsystem unavailable",
			ErrSwapBackendUnavailable)
	}

	decoded, err := decodePreparedInvoice(
		intent.invoice, r.deps.ChainParams,
	)
	if err != nil {
		return nil, err
	}
	if decoded.PaymentHash == nil {
		return nil, fmt.Errorf("%w: invoice payment hash is required",
			ErrInvalidDestination)
	}
	paymentHash := *decoded.PaymentHash

	var (
		topupSat   uint64
		creditOnly bool
	)
	if cp := intent.creditPreview; cp != nil {
		topupSat = cp.GetCreditTopupSat()

		// A pay is credit-only when it carries no Lightning swap leg:
		// either the server pins it to credit (sub-dust), or the
		// applied credits plus the planned top-up already cover the
		// full principal. A mixed pay leaves this false so the swap
		// monitor stays the single terminal authority for the shared
		// payment-hash row. The cover sum is computed overflow-safe so
		// a wrapped server quote cannot flip the routing decision.
		creditOnly = cp.GetMustUseCredit() || creditCoversSat(
			cp.GetCreditAppliedSat(), cp.GetCreditTopupSat(),
			intent.amountSat,
		)
	}

	opKey := "pay:" + hex.EncodeToString(paymentHash[:])
	resp, err := r.deps.CreditRegistry.Ask(ctx, &credit.StartCreditPayRequest{
		OpKey:        opKey,
		Invoice:      intent.invoice,
		PaymentHash:  paymentHash,
		AmountSat:    intent.amountSat,
		TopupSat:     topupSat,
		MaxCreditSat: intent.maxCreditSat,
		MaxFeeSat:    intent.maxFeeSat,
		CreditOnly:   creditOnly,
	}).Await(ctx).Unpack()
	if err != nil {
		return nil, fmt.Errorf("start credit pay: %w", err)
	}
	if _, ok := resp.(*credit.StartCreditResponse); !ok {
		return nil, fmt.Errorf("unexpected credit pay response %T",
			resp)
	}

	// Emit a pending entry keyed by the payment hash. A mixed pay's swap
	// session updates this same row through the monitor loop; a credit-only
	// pay completes server-side and the row is reconciled from credit
	// state. Project off the RPC context so a CLI disconnect cannot cancel
	// the write of an already-accepted pay.
	entry := creditPayEntry(intent, paymentHash)
	r.runtime.trackPendingEntryWithoutTimeout(entry)
	r.runtime.projectAndEmit(context.WithoutCancel(ctx), entry)

	return &walletdkrpc.SendResponse{
		Entry:           entry,
		ActualAmountSat: int64(intent.amountSat),
	}, nil
}

// intentUsesCredit reports whether a prepared invoice intent's quote actually
// reserves or requires credits, in which case the send must route through the
// durable credit subsystem rather than the direct pay path.
func intentUsesCredit(intent *preparedSendIntent) bool {
	cp := intent.creditPreview
	if cp == nil {
		return false
	}

	return cp.GetMustUseCredit() || cp.GetCreditAppliedSat() > 0 ||
		cp.GetCreditShortfallSat() > 0
}

// creditPayEntry builds the pending wallet entry for a credit-backed pay,
// keyed by the payment hash so later swap-session or credit-state updates
// reconcile the same row.
func creditPayEntry(intent *preparedSendIntent,
	paymentHash [32]byte) *walletdkrpc.WalletEntry {

	now := nowUnix()
	paymentHashHex := hex.EncodeToString(paymentHash[:])

	return &walletdkrpc.WalletEntry{
		Id:            paymentHashHex,
		Kind:          walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     int64(intent.amountSat),
		Counterparty:  "credit",
		CreatedAtUnix: now,
		UpdatedAtUnix: now,
		Note:          intent.note,
		Request: &walletdkrpc.WalletEntryRequest{
			Request: &walletdkrpc.WalletEntryRequest_LightningInvoice{
				LightningInvoice: &walletdkrpc.
					LightningInvoiceRequest{
					Invoice:     intent.invoice,
					PaymentHash: paymentHashHex,
				},
			},
		},
		Progress: &walletdkrpc.WalletEntryProgress{
			Phase: walletdkrpc.
				WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING,
			PhaseLabel:  "settling",
			PaymentHash: paymentHashHex,
		},
	}
}

// prepareOnchain validates an onchain destination and builds a fee-aware
// preview. The router selects live VTXOs through the shared coinselect
// package (the same largest-first algorithm the daemon runs), so it does
// not grow a parallel coin-selection primitive.
//
// A bounded send delivers exactly amt_sat to the destination and pays the
// fee on top; the residual returns as a change VTXO under the #270
// seal-time handshake, so the net outflow reported is amt_sat plus the
// estimated fee, not the gross sum of the selected VTXOs. Selection uses
// the same amount + operator-fee + dust headroom the daemon applies, and
// the preview is rejected if the selected funds cannot cover amt_sat plus
// the fee, so a confirmed preview is always fundable.
//
// Sweep semantics are gated on the explicit PrepareSendRequest.sweep_all flag:
// amt_sat = 0 with sweep_all = false is rejected (the most common typo)
// and amt_sat > 0 with sweep_all = true is rejected (contradictory).
// This keeps "drain the wallet" structurally distinct from a defaulted
// zero amount. PrepareSend snapshots sweep_all as an explicit outpoint list;
// VTXOs arriving after prepare are not included in the subsequent Send.
func (r *router) prepareOnchain(ctx context.Context, addr string,
	req *walletdkrpc.PrepareSendRequest) (*walletdkrpc.PrepareSendResponse,
	error) {

	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, ErrInvalidDestination
	}
	if r.deps.RPCServer == nil {
		return nil, ErrSwapBackendUnavailable
	}

	amtSat := req.GetAmtSat()
	sweepAll := req.GetSweepAll()

	switch {
	case sweepAll && amtSat != 0:
		return nil, fmt.Errorf("%w: sweep_all requires amt_sat=0",
			ErrAmountInvalid)

	case !sweepAll && amtSat == 0:
		return nil, fmt.Errorf("%w: amt_sat is required for onchain "+
			"sends (set sweep_all to drain the wallet)",
			ErrAmountRequired)

	case amtSat > math.MaxInt64:
		return nil, fmt.Errorf("%w: amt_sat exceeds int64 range",
			ErrAmountInvalid)
	}

	// Fetch the operator terms once: they size the coin-selection
	// headroom here and the local fee floor in the quote below.
	liveVTXOs, err := r.listLiveVTXOsForLeave(ctx)
	if err != nil {
		return nil, err
	}
	terms := r.fetchOnchainTerms(ctx)

	// Select live VTXOs through the shared selector so the preview
	// mirrors the largest-first selection the daemon will run for the
	// real send, rather than a parallel greedy walk. For a bounded send
	// we select against the same amount + operator-fee + dust headroom
	// the daemon uses (wallet.handleSendOnChain), so the preview does not
	// under-select relative to what the real send needs. Selection stays
	// local (over the daemon's live-VTXO view) so a preview is still
	// produced when the operator is offline; only the fee quote below
	// reaches out to the operator.
	selectTarget := btcutil.Amount(amtSat)
	if !sweepAll {
		selectTarget += terms.minOperatorFee + terms.dustLimit
	}

	vtxoAmount := func(v *daemonrpc.VTXO) btcutil.Amount {
		return btcutil.Amount(v.GetAmountSat())
	}
	res, err := coinselect.LargestFirst(
		liveVTXOs, vtxoAmount, coinselect.Request{
			Target:   selectTarget,
			SweepAll: sweepAll,
		},
	)
	switch {
	case errors.Is(err, coinselect.ErrNoCandidates):
		if sweepAll {
			return nil, fmt.Errorf("%w: no live VTXOs to sweep",
				ErrAmountRequired)
		}

		return nil, fmt.Errorf("%w: no live VTXOs to cover %d sat",
			ErrAmountRequired, amtSat)

	case errors.Is(err, coinselect.ErrSelectionShortfall):
		return nil, fmt.Errorf("%w: live VTXOs cover %d of %d sat",
			ErrAmountRequired, int64(res.Total),
			int64(selectTarget))

	case err != nil:
		return nil, err
	}

	selectedTotal := int64(res.Total)
	selected := make([]string, 0, len(res.Selected))
	for _, v := range res.Selected {
		selected = append(selected, v.GetOutpoint())
	}

	// previewAmount is the amount delivered to the destination: the
	// requested amount for a bounded send, or the entire swept balance
	// for a sweep.
	previewAmount := int64(amtSat)
	if sweepAll {
		previewAmount = selectedTotal
	}

	feeQuote := r.estimateOnchainFee(
		ctx, previewAmount, len(res.Selected), sweepAll, terms,
	)

	// Guard the preview's coherence: a bounded send must not report an
	// outflow its own selected funds cannot cover. The daemon re-selects
	// for the real send, but the preview should reject an unaffordable
	// amount + fee here rather than confirm a stale, unfundable quote
	// that the SendOnChain path will later fail. A sweep moves the whole
	// balance, so it is affordable by construction.
	if !sweepAll {
		needed := int64(amtSat) + feeQuote.feeSat
		if selectedTotal < needed {
			return nil, fmt.Errorf("%w: live VTXOs cover %d sat, "+
				"need %d sat (amount plus fee)",
				ErrAmountRequired, selectedTotal, needed)
		}
	}

	intent := &preparedSendIntent{
		kind:              preparedSendOnchain,
		onchainAddress:    addr,
		amountSat:         amtSat,
		note:              req.GetNote(),
		maxFeeSat:         req.GetMaxFeeSat(),
		sweepAll:          sweepAll,
		selectedOutpoints: append([]string(nil), selected...),
		actualAmountSat:   selectedTotal,
	}

	if _, err := r.intents.put(intent); err != nil {
		return nil, err
	}

	// Net outflow is what actually leaves the wallet. A bounded send
	// delivers exactly amtSat and pays the fee on top; the residual
	// returns as a change VTXO, so the gross selected total is NOT the
	// outflow. A sweep moves the entire selected balance (the fee is
	// absorbed out of the single leave output).
	expectedOutflow := selectedTotal
	if !sweepAll {
		expectedOutflow = int64(amtSat) + feeQuote.feeSat
	}

	return prepareResponseFromIntent(
		intent, prepareSendPreview{
			rail:                    walletdkrpc.SendRail_SEND_RAIL_ONCHAIN,
			quoteStatus:             feeQuote.quoteStatus,
			amountSat:               previewAmount,
			expectedFeeSat:          feeQuote.feeSat,
			feeKnown:                feeQuote.feeKnown,
			expectedTotalOutflowSat: expectedOutflow,
			totalOutflowKnown:       true,
			destinationSummary:      addr,
			warning:                 feeQuote.warning,
		},
	), nil
}

// sendOnchainIntent routes a prepared onchain destination through the
// daemon's SendOnChain RPC. The daemon owns VTXO selection, intent
// composition (forfeits + one fixed leave output + one change VTXO in
// bounded mode; forfeits + one fee-absorbing leave output in sweep-all
// mode), and atomic round registration — the wallet layer is a thin
// translator from the prepared intent to daemonrpc.SendOnChainRequest
// plus the WalletEntry stub the deadline watcher needs.
//
// Exact-amount semantics: a bounded send (--amt N) lands exactly N
// sats at the destination; any residual returns to the caller as a
// change VTXO under the #270 seal-time fee handshake. The earlier
// "whole-VTXO sweep semantics" overpay (issue #634) is gone.
func (r *router) sendOnchainIntent(ctx context.Context,
	intent *preparedSendIntent) (*walletdkrpc.SendResponse, error) {

	if r.deps.RPCServer == nil {
		return nil, ErrSwapBackendUnavailable
	}

	sendReq := &daemonrpc.SendOnChainRequest{
		Destination: &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_Address{
				Address: intent.onchainAddress,
			},
		},
	}
	if intent.sweepAll {
		sendReq.Amount = &daemonrpc.SendOnChainRequest_SweepAll{
			SweepAll: true,
		}
	} else {
		sendReq.Amount = &daemonrpc.SendOnChainRequest_AmountSat{
			AmountSat: int64(intent.amountSat),
		}
	}

	sendResp, err := r.deps.RPCServer.SendOnChain(ctx, sendReq)
	if err != nil {
		return nil, fmt.Errorf("send on-chain: %w", err)
	}

	// SendOnChain registers the intent atomically (TriggerRegistration
	// is set inside the daemon handler), so there is no explicit
	// JoinNextRound call to make here — that was an artifact of the
	// previous LeaveVTXOs-based implementation that queued without
	// committing.

	// In sweep-all mode the caller's amount_sat is zero, so the
	// pending entry must carry the daemon-reported actual amount
	// rather than logging a zero-sat exit row in
	// List/SubscribeWallet. In bounded mode actual_amount_sat equals
	// the requested --amt exactly under the new exact-amount path.
	actualSat := sendResp.GetActualAmountSat()
	entryAmt := int64(intent.amountSat)
	if intent.sweepAll {
		entryAmt = actualSat
	}
	entry := leaveEntryStub(
		sendResp.GetSendJobId(), sendResp.GetSelectedOutpoints(),
		intent.onchainAddress, entryAmt, intent.note,
	)

	// The row is keyed by the daemon's stable leave-job id (id above).
	// The forfeit-driven completion still correlates via the retained
	// consumed outpoint (Progress.VtxoOutpoint); the live, cross-restart
	// terminal reconciliation under this id is C2.
	r.runtime.trackPendingEntryWithoutTimeout(entry)

	// Project off the RPC context so a CLI disconnect cannot cancel the
	// write of an already-accepted leave intent; the daemon owns this row.
	r.runtime.projectAndEmit(context.WithoutCancel(ctx), entry)

	return &walletdkrpc.SendResponse{
		Entry:           entry,
		ActualAmountSat: actualSat,
	}, nil
}

// listLiveVTXOsForLeave returns the daemon's view of VTXOs that are
// safe to feed into a fresh LeaveVTXOs call. The default ListVTXOs
// response also includes VTXOs in PendingForfeit / Forfeiting /
// Spending — those are "not yet terminal" but already committed to
// another in-flight operation, and reselecting them races into the
// VTXO manager's reservation gate (issue darepo-client#577: a second
// onchain send while the first leave round is still unconfirmed
// fails with "forfeiting: bad event: *round.PendingForfeitEvent").
// Filtering to VTXO_STATUS_LIVE at the source closes that race for
// every caller — both `--sweep-all` totalling and bounded-amount
// coin selection.
func (r *router) listLiveVTXOsForLeave(ctx context.Context) ([]*daemonrpc.VTXO,
	error) {

	listResp, err := r.deps.RPCServer.ListVTXOs(
		ctx, &daemonrpc.ListVTXOsRequest{
			StatusFilter: daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list vtxos: %w", err)
	}

	return listResp.GetVtxos(), nil
}

func decodePreparedInvoice(invoice string,
	chainParams *chaincfg.Params) (*zpay32.Invoice, error) {

	if chainParams == nil {
		return nil, fmt.Errorf("%w: invoice chain params are required",
			ErrSwapBackendUnavailable)
	}

	decoded, err := zpay32.Decode(invoice, chainParams)
	if err != nil {
		return nil, fmt.Errorf("%w: decode invoice: %v",
			ErrInvalidDestination, err)
	}

	return decoded, nil
}

func extractPreparedInvoiceAmountSat(decoded *zpay32.Invoice) (uint64, error) {
	if decoded == nil || decoded.MilliSat == nil {
		return 0, fmt.Errorf("%w: invoice amount is required",
			ErrAmountRequired)
	}

	amountMSat := uint64(*decoded.MilliSat)
	if amountMSat == 0 {
		return 0, fmt.Errorf("%w: invoice amount must be positive",
			ErrAmountInvalid)
	}

	return ceilMsatToSat(amountMSat), nil
}

// quotePayUnavailable reports whether a quote failure means the configured
// daemon or swap server is older than the quote RPC.
func quotePayUnavailable(err error) bool {
	switch status.Code(err) {
	case codes.Unimplemented, codes.NotFound:
		return true

	default:
		return false
	}
}

// prepareInvoicePreview builds a remote quote preview when available and keeps
// mixed-version deployments usable with a local-only fallback otherwise.
func prepareInvoicePreview(invoice, description, paymentHash string,
	amountSat uint64, quote *swapclientrpc.QuotePayResponse,
	quoteErr error) (prepareSendPreview, error) {

	if quoteErr != nil {
		return prepareInvoiceLocalPreview(
			invoice, description, paymentHash, amountSat,
		), nil
	}

	return prepareInvoicePreviewFromQuote(
		invoice, description, paymentHash, quote,
	)
}

// prepareInvoiceLocalPreview records only invoice facts the wallet can verify
// locally. The fee and exact off-chain rail remain unknown until Send starts.
func prepareInvoiceLocalPreview(invoice, description, paymentHash string,
	amountSat uint64) prepareSendPreview {

	return prepareSendPreview{
		rail:                    walletdkrpc.SendRail_SEND_RAIL_OFFCHAIN_UNKNOWN,
		quoteStatus:             walletdkrpc.SendQuoteStatus_SEND_QUOTE_STATUS_LOCAL_ONLY,
		amountSat:               int64(amountSat),
		expectedFeeSat:          0,
		feeKnown:                false,
		expectedTotalOutflowSat: int64(amountSat),
		totalOutflowKnown:       false,
		destinationSummary:      truncate(invoice, 32),
		invoiceDescription:      description,
		paymentHash:             paymentHash,
		warning: "server quote unavailable; fee and rail will be " +
			"resolved when Send starts",
	}
}

func prepareInvoicePreviewFromQuote(invoice, description, paymentHash string,
	quote *swapclientrpc.QuotePayResponse) (prepareSendPreview, error) {

	if quote == nil {
		return prepareSendPreview{}, fmt.Errorf("quote pay response " +
			"is required")
	}

	if quote.GetInvoiceAmountSat() > math.MaxInt64 ||
		quote.GetAmountSat() > math.MaxInt64 ||
		quote.GetFeeSat() > math.MaxInt64 {
		return prepareSendPreview{}, fmt.Errorf("%w: quote amount "+
			"exceeds int64 range", ErrAmountInvalid)
	}

	if paymentHash != "" && quote.GetPaymentHash() != paymentHash {
		return prepareSendPreview{}, fmt.Errorf("%w: quote payment "+
			"hash does not match invoice", ErrInvalidDestination)
	}

	rail, err := sendRailFromQuoteSettlement(quote.GetSettlementType())
	if err != nil {
		return prepareSendPreview{}, err
	}

	warning := ""
	if quote.GetExceedsMaxFee() {
		warning = "quoted fee exceeds max_fee_sat; Send will fail " +
			"unless prepared with a higher fee cap"
	}
	creditPreview := creditPreviewFromQuote(quote.GetCreditQuote())
	if creditPreview != nil && creditPreview.GetCreditShortfallSat() > 0 {
		warning = fmt.Sprintf("credit shortfall requires %d sat top-up",
			creditPreview.GetCreditTopupSat())
	}

	return prepareSendPreview{
		rail: rail,
		quoteStatus: walletdkrpc.
			SendQuoteStatus_SEND_QUOTE_STATUS_COMPLETE,
		amountSat:               int64(quote.GetInvoiceAmountSat()),
		expectedFeeSat:          int64(quote.GetFeeSat()),
		feeKnown:                true,
		expectedTotalOutflowSat: int64(quote.GetAmountSat()),
		totalOutflowKnown:       true,
		destinationSummary:      truncate(invoice, 32),
		invoiceDescription:      description,
		paymentHash:             quote.GetPaymentHash(),
		warning:                 warning,
		creditPreview:           creditPreview,
	}, nil
}

func creditPreviewFromQuote(
	quote *swapclientrpc.CreditQuote) *walletdkrpc.CreditPreview {

	if quote == nil {
		return nil
	}

	return &walletdkrpc.CreditPreview{
		MustUseCredit:      quote.GetMustUseCredit(),
		CreditAppliedSat:   quote.GetCreditAppliedSat(),
		CreditShortfallSat: quote.GetCreditShortfallSat(),
		CreditTopupSat:     quote.GetCreditTopupSat(),
		ArkFundingSat:      quote.GetArkFundingSat(),
	}
}

func sendRailFromQuoteSettlement(
	settlementType swapclientrpc.SwapSettlementType) (walletdkrpc.SendRail,
	error) {

	switch settlementType {
	case swapclientrpc.
		SwapSettlementType_SWAP_SETTLEMENT_TYPE_LIGHTNING:
		return walletdkrpc.SendRail_SEND_RAIL_LIGHTNING, nil

	case swapclientrpc.SwapSettlementType_SWAP_SETTLEMENT_TYPE_IN_ARK:
		return walletdkrpc.SendRail_SEND_RAIL_IN_ARK, nil

	case swapclientrpc.SwapSettlementType_SWAP_SETTLEMENT_TYPE_CREDIT:
		return walletdkrpc.SendRail_SEND_RAIL_CREDIT, nil

	case swapclientrpc.SwapSettlementType_SWAP_SETTLEMENT_TYPE_MIXED:
		return walletdkrpc.SendRail_SEND_RAIL_MIXED, nil

	default:
		return walletdkrpc.SendRail_SEND_RAIL_OFFCHAIN_UNKNOWN, nil
	}
}
