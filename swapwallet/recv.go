//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
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

// Recv opens an out-swap session via the daemon-owned swap subserver and
// returns the daemon-signed BOLT-11 invoice plus the initial WalletEntry.
// Background lifecycle (waiting for the LN payer, claiming the vHTLC,
// terminal transitions) is owned by the swap subsystem; the wallet layer
// observes those transitions through the monitor loop and projects them
// onto the WalletEntry shape.
func (r *receiver) Recv(ctx context.Context, req *wavewalletrpc.RecvRequest) (
	*wavewalletrpc.RecvResponse, error) {

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
		switch {
		case err != nil:
			// Credit balance lookup failed; still route to credit
			// (a fresh wallet has no credits anyway), but log the
			// decision so the sub-dust path is not silent.
			r.deps.resolveLog().InfoS(ctx, "Routing sub-dust "+
				"receive to credit subsystem",
				slog.Uint64("amt_sat", amt),
				slog.Uint64("dust_limit_sat", dustLimit),
				slog.String("credit_balance", "unavailable"))

			return r.recvCredit(ctx, req, amt, dustLimit)

		case amt > ^uint64(0)-availableCreditSat ||
			amt+availableCreditSat < dustLimit:

			r.deps.resolveLog().InfoS(ctx, "Routing sub-dust "+
				"receive to credit subsystem",
				slog.Uint64("amt_sat", amt),
				slog.Uint64("dust_limit_sat", dustLimit),
				slog.Uint64(
					"available_credit_sat",
					availableCreditSat,
				))

			return r.recvCredit(ctx, req, amt, dustLimit)

		default:
			plannedVHTLCSat = amt + availableCreditSat
		}
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
		wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
	)

	// Project the pending row on dispatch so it is visible immediately,
	// not only once the monitor's async projection lands. project (not
	// projectAndEmit): the monitor owns the swap-backed row's live emit.
	r.runtime.project(context.WithoutCancel(ctx), entry)

	return &wavewalletrpc.RecvResponse{
		Invoice: startResp.GetInvoice(),
		Entry:   entry,
	}, nil
}

func (r *receiver) receiveDustLimit(ctx context.Context) (uint64, error) {
	if r.deps.RPCServer == nil {
		return 0, nil
	}

	info, err := r.deps.RPCServer.GetInfo(ctx, &waverpc.GetInfoRequest{})
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
func (r *receiver) recvCredit(ctx context.Context,
	req *wavewalletrpc.RecvRequest, amt, dustLimit uint64) (
	*wavewalletrpc.RecvResponse, error) {

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
		// A sub-dust receive that the swap server never completes must
		// not vanish silently (wavelength#1041): log it, record a
		// FAILED activity row, and return a typed, actionable error
		// instead of the opaque underlying deadline/RPC error.
		r.deps.resolveLog().WarnS(ctx, "Sub-dust credit receive failed",
			err,
			slog.Uint64("amt_sat", amt),
			slog.Uint64("dust_limit_sat", dustLimit),
		)

		r.projectFailedCreditReceive(ctx, req, "recv:"+keyID, amt)

		// Return only the typed sentinel + actionable guidance. The
		// underlying swap-server error is deliberately NOT wrapped into
		// the returned error: it is a gRPC status (often
		// DeadlineExceeded from the credit AdmitTimeout), and wrapping
		// it would let the error-mapping interceptor pass that raw code
		// through instead of the typed Unavailable. The underlying
		// cause is logged above.
		return nil, fmt.Errorf("%w: the operator did not complete the "+
			"credit request for %d sat and may not support "+
			"sub-dust receives; receive at least %d sat instead",
			ErrCreditReceiveUnavailable, amt, dustLimit)
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
	r.runtime.trackPendingEntryWithoutTimeout(entry)
	r.runtime.projectAndEmit(context.WithoutCancel(ctx), entry)

	return &wavewalletrpc.RecvResponse{
		Invoice: start.Invoice,
		Entry:   entry,
		CreditReceive: &wavewalletrpc.CreditReceive{
			OperationId: start.OpID,
			AmountSat:   amt,
			PaymentHash: paymentHashHex,
		},
	}, nil
}

// projectFailedCreditReceive records a terminal FAILED activity row for a
// sub-dust credit receive whose admission never completed. Without it a failed
// attempt leaves no trace at all (the success path is the only place that
// projects a row), so the user has no evidence the receive was attempted
// (wavelength#1041).
func (r *receiver) projectFailedCreditReceive(ctx context.Context,
	req *wavewalletrpc.RecvRequest, id string, amt uint64) {

	now := nowUnix()
	entry := &wavewalletrpc.WalletEntry{
		Id:            id,
		Kind:          wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_FAILED,
		AmountSat:     int64(amt),
		Counterparty:  "credit",
		CreatedAtUnix: now,
		UpdatedAtUnix: now,
		Note:          req.GetMemo(),
		FailureReason: "credit receive unavailable",
		// Classify the row with a stable, machine-readable code so
		// clients can branch on it without parsing the free-text
		// reason. This mirrors how the credit projector tags a terminal
		// credit-op failure (ENTRY_FAILURE_CODE_FAILED): the receive
		// reached a terminal, non-actionable failure.
		FailureCode: failureCodePtr(
			wavewalletrpc.EntryFailureCode_ENTRY_FAILURE_CODE_FAILED,
		),
		Progress: &wavewalletrpc.WalletEntryProgress{
			Phase: wavewalletrpc.
				WalletEntryPhase_WALLET_ENTRY_PHASE_FAILED,
			PhaseLabel: "failed",
		},
	}

	r.runtime.projectAndEmit(context.WithoutCancel(ctx), entry)
}

func creditReceiveEntry(req *wavewalletrpc.RecvRequest, opID, invoice,
	paymentHashHex string, amt uint64) *wavewalletrpc.WalletEntry {

	now := nowUnix()

	return &wavewalletrpc.WalletEntry{
		Id:            opID,
		Kind:          wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     int64(amt),
		Counterparty:  "credit",
		CreatedAtUnix: now,
		UpdatedAtUnix: now,
		Note:          req.GetMemo(),
		Request: &wavewalletrpc.WalletEntryRequest{
			Request: &wavewalletrpc.WalletEntryRequest_LightningInvoice{
				LightningInvoice: &wavewalletrpc.
					LightningInvoiceRequest{
					Invoice:     invoice,
					PaymentHash: paymentHashHex,
				},
			},
		},
		Progress: &wavewalletrpc.WalletEntryProgress{
			Phase: wavewalletrpc.
				WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_PAYMENT,
			PhaseLabel:  "waiting_for_payment",
			PaymentHash: paymentHashHex,
		},
	}
}
