//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/darepo-client/credit"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
)

// receiver drives wallet-layer Recv flows. It is a thin composition over
// the existing swap subserver's StartReceive RPC; the swap subsystem owns
// the invoice signing (with a daemon-managed payment-scoped auth key via
// PR #337), persistence, lifecycle, and receive expiry. swapwallet only
// normalizes the response into a flat WalletEntry.
type receiver struct {
	deps    *Deps
	runtime *Runtime
}

// newReceiver constructs a receiver bound to the shared wallet runtime.
func newReceiver(deps *Deps, runtime *Runtime) *receiver {
	return &receiver{deps: deps, runtime: runtime}
}

// Recv opens a swap-in session via the daemon-owned swap subserver and
// returns the daemon-signed BOLT-11 invoice plus the initial WalletEntry.
// Background lifecycle (waiting for the LN payer, claiming the vHTLC,
// terminal transitions) is owned by the swap subsystem; the wallet layer
// observes those transitions through the monitor loop and projects them
// onto the WalletEntry shape.
func (r *receiver) Recv(ctx context.Context, req *walletdkrpc.RecvRequest) (
	*walletdkrpc.RecvResponse, error) {

	if r == nil || r.deps == nil || r.deps.SwapService == nil {
		return nil, ErrSwapBackendUnavailable
	}

	amt := req.GetAmtSat()
	if amt == 0 {
		return nil, ErrAmountRequired
	}
	// amount_sat in swapclientrpc is int64; reject values that overflow
	// the signed range so we never silently submit a wrapped amount.
	if amt > (1<<63)-1 {
		return nil, fmt.Errorf("%w: amt_sat exceeds int64 range",
			ErrAmountInvalid)
	}

	plannedVHTLCSat := amt
	dustLimit, err := r.receiveDustLimit(ctx)
	if err != nil {
		r.deps.resolveLog().WarnS(ctx, "Skipping receive dust "+
			"planning: operator terms unavailable", err)
		dustLimit = 0
	}
	if dustLimit > 0 && amt < dustLimit {
		availableCreditSat, err := r.availableCreditSat(ctx)
		if err != nil {
			return r.recvCredit(ctx, req, amt)
		}
		if amt > ^uint64(0)-availableCreditSat ||
			amt+availableCreditSat < dustLimit {
			return r.recvCredit(ctx, req, amt)
		}

		plannedVHTLCSat = amt + availableCreditSat
	}

	// Enforce the operator's per-VTXO and total-balance limits before a
	// swap session is created. For credit-assisted receives, check the
	// actual vHTLC amount the server will ask the client to accept.
	if r.deps.RPCServer != nil {
		err := checkReceiveLimits(
			ctx, r.deps.RPCServer, r.deps.resolveLog(),
			btcutil.Amount(plannedVHTLCSat),
		)
		if err != nil {
			return nil, err
		}
	}

	startResp, err := r.deps.SwapService.StartReceive(
		ctx, &swapclientrpc.StartReceiveRequest{
			AmountSat: int64(amt),
			Memo:      req.GetMemo(),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("start receive: %w", err)
	}

	if startResp.GetSwap() != nil &&
		startResp.GetSwap().GetInvoice() == "" {

		startResp.GetSwap().Invoice = startResp.GetInvoice()
	}

	// Pin the kind to RECV so the row is unambiguous even if the SDK's
	// summary direction is UNSPECIFIED on the initial response (the
	// publishHash update fills it in afterwards). Passing the kind in
	// also ensures the amount sign is computed from RECV rather than
	// the raw (possibly-zero) direction.
	entry := swapEntryFromSummary(
		startResp.GetSwap(), req.GetMemo(), "",
		walletdkrpc.EntryKind_ENTRY_KIND_RECV,
	)

	return &walletdkrpc.RecvResponse{
		Invoice: startResp.GetInvoice(),
		Entry:   entry,
	}, nil
}

func (r *receiver) receiveDustLimit(ctx context.Context) (uint64, error) {
	if r.deps.RPCServer == nil {
		return 0, nil
	}

	info, err := r.deps.RPCServer.GetInfo(ctx, &daemonrpc.GetInfoRequest{})
	if err != nil {
		return 0, fmt.Errorf("get server info: %w", err)
	}
	if info.GetServerInfo() == nil {
		return 0, nil
	}

	return info.GetServerInfo().GetDustLimit(), nil
}

func (r *receiver) availableCreditSat(ctx context.Context) (uint64, error) {
	credits, err := r.deps.SwapService.ListCredits(
		ctx, &swapclientrpc.ListCreditsRequest{
			Limit: 1,
		},
	)
	if err != nil {
		return 0, err
	}

	return credits.GetAvailableSat(), nil
}

// recvCredit hands a sub-dust receive to the durable credit subsystem. The
// registry creates the server-owned receive invoice synchronously and returns
// it, while durably tracking the operation so the pending row survives a
// restart. Each Recv is a distinct receive, so the op key is freshly random
// (inbound receives carry no double-spend risk that would need cross-call
// dedup).
func (r *receiver) recvCredit(ctx context.Context, req *walletdkrpc.RecvRequest,
	amt uint64) (*walletdkrpc.RecvResponse, error) {

	if r.deps.CreditRegistry == nil {
		return nil, fmt.Errorf("%w: credit subsystem unavailable",
			ErrSwapBackendUnavailable)
	}

	keyID, err := newSendIntentID()
	if err != nil {
		return nil, fmt.Errorf("create credit receive id: %w", err)
	}

	resp, err := r.deps.CreditRegistry.Ask(
		ctx, &credit.StartCreditReceiveRequest{
			OpKey:     "recv:" + keyID,
			AmountSat: amt,
			Memo:      req.GetMemo(),
		},
	).Await(ctx).Unpack()
	if err != nil {
		return nil, fmt.Errorf("start credit receive: %w", err)
	}

	start, ok := resp.(*credit.StartCreditResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected credit receive response %T",
			resp)
	}
	if start.Invoice == "" {
		return nil, fmt.Errorf("credit receive invoice missing")
	}

	paymentHashHex := hex.EncodeToString(start.PaymentHash)
	entry := creditReceiveEntry(
		req, start.OpID, start.Invoice, paymentHashHex, amt,
	)

	return &walletdkrpc.RecvResponse{
		Invoice: start.Invoice,
		Entry:   entry,
		CreditReceive: &walletdkrpc.CreditReceive{
			OperationId: start.OpID,
			AmountSat:   amt,
			PaymentHash: paymentHashHex,
		},
	}, nil
}

func creditReceiveEntry(req *walletdkrpc.RecvRequest, opID, invoice,
	paymentHashHex string, amt uint64) *walletdkrpc.WalletEntry {

	now := nowUnix()

	return &walletdkrpc.WalletEntry{
		Id:            opID,
		Kind:          walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     int64(amt),
		Counterparty:  "credit",
		CreatedAtUnix: now,
		UpdatedAtUnix: now,
		Note:          req.GetMemo(),
		Request: &walletdkrpc.WalletEntryRequest{
			Request: &walletdkrpc.WalletEntryRequest_LightningInvoice{
				LightningInvoice: &walletdkrpc.
					LightningInvoiceRequest{
					Invoice:     invoice,
					PaymentHash: paymentHashHex,
				},
			},
		},
		Progress: &walletdkrpc.WalletEntryProgress{
			Phase: walletdkrpc.
				WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_PAYMENT,
			PhaseLabel:  "waiting_for_payment",
			PaymentHash: paymentHashHex,
		},
	}
}
