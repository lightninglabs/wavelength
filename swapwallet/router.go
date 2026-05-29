//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"strings"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/lightningnetwork/lnd/zpay32"
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

	intent := &preparedSendIntent{
		kind:      preparedSendInvoice,
		invoice:   invoice,
		amountSat: amountSat,
		note:      req.GetNote(),
		maxFeeSat: req.GetMaxFeeSat(),
	}

	if _, err := r.intents.put(intent); err != nil {
		return nil, err
	}

	return prepareResponseFromIntent(
		intent, prepareSendPreview{
			rail: walletdkrpc.SendRail_SEND_RAIL_OFFCHAIN_UNKNOWN,
			quoteStatus: walletdkrpc.
				SendQuoteStatus_SEND_QUOTE_STATUS_LOCAL_ONLY,
			amountSat:          int64(amountSat),
			feeKnown:           false,
			totalOutflowKnown:  false,
			destinationSummary: truncate(invoice, 32),
			invoiceDescription: description,
			paymentHash:        paymentHash,
			warning: "swapserver quote support is not available " +
				"yet",
		},
	), nil
}

// sendInvoiceIntent routes a prepared BOLT-11 invoice through the
// daemon-owned swap subserver.
func (r *router) sendInvoiceIntent(ctx context.Context,
	intent *preparedSendIntent) (*walletdkrpc.SendResponse, error) {

	if r.deps.SwapService == nil {
		return nil, ErrSwapBackendUnavailable
	}

	startResp, err := r.deps.SwapService.StartPay(
		ctx, &swapclientrpc.StartPayRequest{
			Invoice:   intent.invoice,
			MaxFeeSat: intent.maxFeeSat,
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
	return &walletdkrpc.SendResponse{
		Entry:           entry,
		ActualAmountSat: startResp.GetSwap().GetAmountSat(),
	}, nil
}

// prepareOnchain validates an onchain destination through local VTXO
// selection. The router selects VTXOs covering the
// requested amount using the existing wallet listing surface — no new
// coin-selection primitive is introduced here.
//
// v1 semantics: LeaveVTXOs sweeps WHOLE VTXOs to the destination, so the
// caller's onchain wallet may receive more than amt_sat (the sum of the
// selected VTXOs is what actually leaves the wallet). The router returns
// that sum on SendResponse.actual_amount_sat so the CLI / UI can echo it
// before the user treats the send as confirmed. A future enhancement can
// pre-split a VTXO via OOR for exact amounts; that work is intentionally
// out of scope here.
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

	var (
		actualSat int64
		selected  []string
	)
	switch {
	case sweepAll:
		vtxos, err := r.listLiveVTXOsForLeave(ctx)
		if err != nil {
			return nil, err
		}
		selected, actualSat = outpointsAndSum(vtxos)
		if actualSat == 0 {
			return nil, fmt.Errorf("%w: no live VTXOs to sweep",
				ErrAmountRequired)
		}

	default:
		// Caller-bounded send: select live VTXOs whose total covers
		// the requested amount, then sweep them. The selected set
		// is the input to LeaveVTXOs; per-outpoint destinations are
		// omitted so DefaultDestination applies to every selected
		// outpoint.
		selectedSet, selectedSum, err := r.selectVTXOsForAmount(
			ctx, int64(amtSat),
		)
		if err != nil {
			return nil, err
		}
		selected = selectedSet
		actualSat = selectedSum
	}

	intent := &preparedSendIntent{
		kind:              preparedSendOnchain,
		onchainAddress:    addr,
		amountSat:         amtSat,
		note:              req.GetNote(),
		maxFeeSat:         req.GetMaxFeeSat(),
		sweepAll:          sweepAll,
		selectedOutpoints: append([]string(nil), selected...),
		actualAmountSat:   actualSat,
	}

	if _, err := r.intents.put(intent); err != nil {
		return nil, err
	}

	warning := "operator cooperative-leave quote support is not " +
		"available yet"

	previewAmount := int64(amtSat)
	if sweepAll {
		previewAmount = actualSat
	}

	return prepareResponseFromIntent(
		intent, prepareSendPreview{
			rail: walletdkrpc.SendRail_SEND_RAIL_ONCHAIN,
			quoteStatus: walletdkrpc.
				SendQuoteStatus_SEND_QUOTE_STATUS_LOCAL_ONLY,
			amountSat:               previewAmount,
			feeKnown:                false,
			expectedTotalOutflowSat: actualSat,
			totalOutflowKnown:       true,
			destinationSummary:      addr,
			warning:                 warning,
		},
	), nil
}

// sendOnchainIntent routes a prepared onchain destination through the existing
// LeaveVTXOs cooperative-exit RPC.
func (r *router) sendOnchainIntent(ctx context.Context,
	intent *preparedSendIntent) (*walletdkrpc.SendResponse, error) {

	if r.deps.RPCServer == nil {
		return nil, ErrSwapBackendUnavailable
	}
	if len(intent.selectedOutpoints) == 0 {
		return nil, ErrInvalidSendIntent
	}

	leaveReq := &daemonrpc.LeaveVTXOsRequest{
		Selection: &daemonrpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &daemonrpc.OutpointSelection{
				Outpoints: append(
					[]string(nil),
					intent.selectedOutpoints...,
				),
			},
		},
		DefaultDestination: &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_Address{
				Address: intent.onchainAddress,
			},
		},
	}

	resp, err := r.deps.RPCServer.LeaveVTXOs(ctx, leaveReq)
	if err != nil {
		return nil, fmt.Errorf("leave vtxos: %w", err)
	}

	// LeaveVTXOs only QUEUES the leave intent in the round actor's
	// PendingAssembly state; the round does not actually seal until
	// the daemon receives a JoinNextRound trigger. The everyday
	// wallet verb is documented as a one-shot ("send onchain"), so
	// we commit the intent here on the caller's behalf. The ark.*
	// raw `vtxos refresh` / `vtxos leave` CLI exposes the
	// queue-only mode via --no_join for batching use cases; that
	// path is not reachable through walletdkrpc.Send and should not
	// be — the higher-level Send verb has no batching contract.
	//
	// A join failure here leaves the leave intent queued in the
	// round actor: LeaveVTXOs has already returned successfully and
	// the intent persists in PendingAssembly. We surface the error
	// (rather than swallowing it) so the caller is not silently
	// stranded — but the recovery is a one-liner, and the wrapped
	// message embeds the exact `ark rounds join` command the user
	// needs to commit the queued intent.
	if _, err := r.deps.RPCServer.JoinNextRound(
		ctx, &daemonrpc.JoinNextRoundRequest{},
	); err != nil {
		return nil, fmt.Errorf("auto-join next round after leave: the "+
			"leave intent is queued and can be committed manually "+
			"with `ark rounds join`: %w", err)
	}

	// For sweep-all the caller's amt_sat is required to be zero, so
	// recording int64(amtSat) on the pending entry would make the row
	// show as a zero-sat exit in List/SubscribeWallet. The real outflow
	// is the actualSat sum of selected VTXOs computed above.
	entryAmt := int64(intent.amountSat)
	if intent.sweepAll {
		entryAmt = intent.actualAmountSat
	}
	entry := leaveEntryStub(
		resp.GetQueuedOutpoints(), intent.onchainAddress, entryAmt,
		intent.note,
	)

	// v1 SCOPE: we track the pending row for the deadline watcher
	// but do NOT register an intent index. EXIT/DEPOSIT canonical-id
	// correlation between the pending row and the eventual sweep
	// ledger row is deferred to v2 — see swapwallet/doc.go for the
	// limitation note.
	r.runtime.trackPending(
		entry.GetId(), entry.GetKind(),
		unixToTime(
			entry.GetCreatedAtUnix(),
		),
	)

	return &walletdkrpc.SendResponse{
		Entry:           entry,
		ActualAmountSat: intent.actualAmountSat,
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

// selectVTXOsForAmount returns the smallest-sufficient set of live VTXO
// outpoints whose summed amount covers target, plus the actual sum of
// the selection. The selection is greedy by VTXO order returned from the
// daemon; v1 does not optimize for change minimization because
// LeaveVTXOs already sweeps WHOLE VTXOs, so any remainder above target
// lands at the destination. Callers surface that sum back to the user
// via SendResponse.actual_amount_sat.
//
// Returns ErrAmountInvalid when target is non-positive, and
// ErrAmountRequired when no combination of live VTXOs covers target.
func (r *router) selectVTXOsForAmount(ctx context.Context, target int64) (
	[]string, int64, error) {

	if target <= 0 {
		return nil, 0, ErrAmountInvalid
	}

	vtxos, err := r.listLiveVTXOsForLeave(ctx)
	if err != nil {
		return nil, 0, err
	}

	var (
		selected []string
		covered  int64
	)
	for _, v := range vtxos {
		if v.GetAmountSat() <= 0 {
			continue
		}
		selected = append(selected, v.GetOutpoint())
		covered += v.GetAmountSat()
		if covered >= target {
			return selected, covered, nil
		}
	}

	return nil, 0, fmt.Errorf("%w: insufficient live VTXOs cover %d sat "+
		"(covered=%d)", ErrAmountRequired, target, covered)
}

func outpointsAndSum(vtxos []*daemonrpc.VTXO) ([]string, int64) {
	var (
		outpoints []string
		total     int64
	)
	for _, v := range vtxos {
		if v.GetAmountSat() <= 0 {
			continue
		}
		outpoints = append(outpoints, v.GetOutpoint())
		total += v.GetAmountSat()
	}

	return outpoints, total
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
	if amountMSat%1000 != 0 {
		return 0, fmt.Errorf("%w: invoice amount must be whole "+
			"satoshis", ErrAmountInvalid)
	}

	return amountMSat / 1000, nil
}
