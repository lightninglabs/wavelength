//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
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
}

// newRouter constructs a router bound to the shared wallet runtime.
func newRouter(deps *Deps, runtime *Runtime) *router {
	return &router{deps: deps, runtime: runtime}
}

// Send dispatches a SendRequest to the right backend and returns the
// initial WalletEntry that callers can poll or subscribe to for status
// transitions.
func (r *router) Send(ctx context.Context, req *walletdkrpc.SendRequest) (
	*walletdkrpc.SendResponse, error) {

	if r == nil || r.deps == nil {
		return nil, ErrSwapBackendUnavailable
	}

	switch dest := req.GetDestination().(type) {
	case *walletdkrpc.SendRequest_Invoice:
		return r.sendInvoice(ctx, dest.Invoice, req)

	case *walletdkrpc.SendRequest_OnchainAddress:
		return r.sendOnchain(ctx, dest.OnchainAddress, req)

	default:
		return nil, ErrInvalidDestination
	}
}

// sendInvoice routes a BOLT-11 invoice through the daemon-owned swap
// subserver. PR #339 lets the swap server transparently settle same-Ark
// p2p when both parties are co-located, so the caller never sees that
// distinction at the wallet layer.
func (r *router) sendInvoice(ctx context.Context, invoice string,
	req *walletdkrpc.SendRequest) (*walletdkrpc.SendResponse, error) {

	invoice = strings.TrimSpace(invoice)
	if invoice == "" {
		return nil, ErrInvalidDestination
	}
	if r.deps.SwapService == nil {
		return nil, ErrSwapBackendUnavailable
	}

	startResp, err := r.deps.SwapService.StartPay(
		ctx, &swapclientrpc.StartPayRequest{
			Invoice:   invoice,
			MaxFeeSat: req.GetMaxFeeSat(),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("start pay: %w", err)
	}

	entry := swapEntryFromSummary(
		startResp.GetSwap(), req.GetNote(), invoice,
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

// sendOnchain routes an onchain destination through the existing
// LeaveVTXOs cooperative-exit RPC. The router selects VTXOs covering the
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
// Sweep semantics are gated on the explicit SendRequest.sweep_all flag:
// amt_sat = 0 with sweep_all = false is rejected (the most common typo)
// and amt_sat > 0 with sweep_all = true is rejected (contradictory).
// This keeps "drain the wallet" structurally distinct from a defaulted
// zero amount.
func (r *router) sendOnchain(ctx context.Context, addr string,
	req *walletdkrpc.SendRequest) (*walletdkrpc.SendResponse, error) {

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

	leaveReq := &daemonrpc.LeaveVTXOsRequest{
		DefaultDestination: &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_Address{
				Address: addr,
			},
		},
	}

	var actualSat int64
	switch {
	case sweepAll:
		// Sweep every live VTXO. LeaveVTXOs rejects non-empty
		// per-outpoint overrides under selection=all, so the
		// caller's address is applied uniformly.
		leaveReq.Selection = &daemonrpc.LeaveVTXOsRequest_All{
			All: true,
		}

		// Total in flight is the sum of every live VTXO so the
		// caller's UI can echo it back before the user treats the
		// send as confirmed.
		total, err := r.totalLiveVTXOAmount(ctx)
		if err != nil {
			return nil, err
		}
		actualSat = total

	default:
		// Caller-bounded send: select live VTXOs whose total covers
		// the requested amount, then sweep them. The selected set
		// is the input to LeaveVTXOs; per-outpoint destinations are
		// omitted so DefaultDestination applies to every selected
		// outpoint.
		selected, selectedSum, err := r.selectVTXOsForAmount(
			ctx, int64(amtSat),
		)
		if err != nil {
			return nil, err
		}
		leaveReq.Selection = &daemonrpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &daemonrpc.OutpointSelection{
				Outpoints: selected,
			},
		}
		actualSat = selectedSum
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
	entryAmt := int64(amtSat)
	if sweepAll {
		entryAmt = actualSat
	}
	entry := leaveEntryStub(
		resp.GetQueuedOutpoints(), addr, entryAmt, req.GetNote(),
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

// totalLiveVTXOAmount sums every live VTXO so a sweep-all caller can be
// told how much will actually leave the wallet before the daemon hands
// the operation off to LeaveVTXOs.
func (r *router) totalLiveVTXOAmount(ctx context.Context) (int64, error) {
	vtxos, err := r.listLiveVTXOsForLeave(ctx)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, v := range vtxos {
		if v.GetAmountSat() <= 0 {
			continue
		}
		total += v.GetAmountSat()
	}

	return total, nil
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
