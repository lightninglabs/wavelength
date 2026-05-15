//go:build walletrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"
	"strings"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
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

// newRouter constructs a router bound to the runtime that will track the
// resulting pending entries for the deadline watcher.
func newRouter(deps *Deps, runtime *Runtime) *router {
	return &router{deps: deps, runtime: runtime}
}

// Send dispatches a SendRequest to the right backend and returns the
// initial WalletEntry that callers can poll or subscribe to for status
// transitions.
func (r *router) Send(ctx context.Context, req *walletrpc.SendRequest) (
	*walletrpc.SendResponse, error) {

	if r == nil || r.deps == nil {
		return nil, ErrSwapBackendUnavailable
	}

	switch dest := req.GetDestination().(type) {
	case *walletrpc.SendRequest_Invoice:
		return r.sendInvoice(ctx, dest.Invoice, req)

	case *walletrpc.SendRequest_OnchainAddress:
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
	req *walletrpc.SendRequest) (*walletrpc.SendResponse, error) {

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
	)
	entry.Kind = walletrpc.EntryKind_ENTRY_KIND_SEND

	r.runtime.registerSendInvoiceIntent(entry.GetId())

	if entry.Status == walletrpc.EntryStatus_ENTRY_STATUS_PENDING {
		r.runtime.trackPending(
			entry.GetId(), entry.GetKind(),
			unixToTime(
				entry.GetCreatedAtUnix(),
			),
		)
	}

	return &walletrpc.SendResponse{Entry: entry}, nil
}

// sendOnchain routes an onchain destination through the existing
// LeaveVTXOs cooperative-exit RPC. The router selects VTXOs covering the
// requested amount using the existing wallet listing surface — no new
// coin-selection primitive is introduced here.
//
// v1 semantics: LeaveVTXOs sweeps WHOLE VTXOs to the destination, so the
// caller's onchain wallet may receive slightly more than amt_sat (any
// remainder above the selected VTXO sum). amt_sat=0 sweeps every live
// VTXO. A future enhancement can pre-split a VTXO via OOR for exact
// amounts; that work is intentionally out of scope here.
func (r *router) sendOnchain(ctx context.Context, addr string,
	req *walletrpc.SendRequest) (*walletrpc.SendResponse, error) {

	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, ErrInvalidDestination
	}
	if r.deps.RPCServer == nil {
		return nil, ErrSwapBackendUnavailable
	}

	amtSat := req.GetAmtSat()
	leaveReq := &daemonrpc.LeaveVTXOsRequest{
		DefaultDestination: &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_Address{
				Address: addr,
			},
		},
	}

	switch {
	case amtSat == 0:
		// Sweep every live VTXO. LeaveVTXOs rejects non-empty
		// per-outpoint overrides under selection=all, so the
		// caller's address is applied uniformly.
		leaveReq.Selection = &daemonrpc.LeaveVTXOsRequest_All{
			All: true,
		}

	default:
		// Caller-bounded send: select live VTXOs whose total covers
		// the requested amount, then sweep them. The selected set
		// is the input to LeaveVTXOs; per-outpoint destinations are
		// omitted so DefaultDestination applies to every selected
		// outpoint.
		selected, err := r.selectVTXOsForAmount(ctx, int64(amtSat))
		if err != nil {
			return nil, err
		}
		leaveReq.Selection = &daemonrpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &daemonrpc.OutpointSelection{
				Outpoints: selected,
			},
		}
	}

	resp, err := r.deps.RPCServer.LeaveVTXOs(ctx, leaveReq)
	if err != nil {
		return nil, fmt.Errorf("leave vtxos: %w", err)
	}

	entry := leaveEntryStub(
		resp.GetQueuedOutpoints(), addr, int64(amtSat), req.GetNote(),
	)

	r.runtime.registerExitIntent(
		entry.GetId(), resp.GetQueuedOutpoints(),
	)
	r.runtime.trackPending(
		entry.GetId(), entry.GetKind(),
		unixToTime(
			entry.GetCreatedAtUnix(),
		),
	)

	return &walletrpc.SendResponse{Entry: entry}, nil
}

// selectVTXOsForAmount returns the smallest-sufficient set of live VTXO
// outpoints whose summed amount covers target. The selection is greedy by
// VTXO order returned from the daemon; v1 does not optimize for change
// minimization because LeaveVTXOs already accepts the entire sweep as
// payment to the destination, so any remainder above target lands at the
// destination instead of being held as change.
//
// Returns ErrAmountInvalid when target is non-positive, and
// ErrAmountRequired when no combination of live VTXOs covers target.
func (r *router) selectVTXOsForAmount(ctx context.Context, target int64) (
	[]string, error) {

	if target <= 0 {
		return nil, ErrAmountInvalid
	}

	listResp, err := r.deps.RPCServer.ListVTXOs(
		ctx, &daemonrpc.ListVTXOsRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("list vtxos: %w", err)
	}

	var (
		selected []string
		covered  int64
	)
	for _, v := range listResp.GetVtxos() {
		if v.GetAmountSat() <= 0 {
			continue
		}
		selected = append(selected, v.GetOutpoint())
		covered += v.GetAmountSat()
		if covered >= target {
			return selected, nil
		}
	}

	return nil, fmt.Errorf("%w: insufficient live VTXOs cover %d sat "+
		"(covered=%d)", ErrAmountRequired, target, covered)
}
