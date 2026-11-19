//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// receiver drives wallet-layer Recv flows. It is a thin composition over
// the existing swap subserver's StartReceive RPC; the swap subsystem owns
// the invoice signing (with a daemon-managed payment-scoped auth key via
// PR #337), persistence, lifecycle, and receive expiry. swapwallet only
// normalizes the response into a flat WalletEntry.
type receiver struct {
	deps    *Deps
	runtime *Runtime

	// requests serializes keyed receives and lookups per request ID. See
	// Recv for why a lookup must wait for an in-flight create.
	requests requestLocks
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

	return r.receive(ctx, req, false)
}

// receive applies wallet amount validation before selecting the requested rail.
func (r *receiver) receive(ctx context.Context, req *wavewalletrpc.RecvRequest,
	activeChannelOnly bool) (*wavewalletrpc.RecvResponse, error) {

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

	// A channel receive credits existing liquidity, not a new wallet VTXO.
	// The channel backend enforces inbound capacity; never fall back to
	// credit, even when the invoice is below the operator's VTXO floor.
	if activeChannelOnly {
		if req.GetRequestId() != "" {
			return nil, status.Error(
				codes.Unimplemented,
				"keyed active-channel receives are unsupported",
			)
		}

		return r.startReceive(ctx, req, true)
	}

	// A keyed receive first reads what the key already owns. Routing
	// depends on live credit balance and operator terms, so a retry could
	// otherwise land on the other backend and create a second invoice.
	// The per-ID lock is held until the backend has answered, so an
	// overlapping retry or LookupRecv waits for this create to finish
	// instead of reading "not found" while it is still in flight.
	requestID := req.GetRequestId()
	if requestID != "" {
		unlock, err := r.requests.lock(ctx, requestID)
		if err != nil {
			return nil, err
		}
		defer unlock()

		existing, err := r.lookup(ctx, requestID)
		switch {
		case err == nil:
			if existing.amt != amt ||
				existing.memo != req.GetMemo() {
				return nil, ErrReceiveRequestMismatch
			}

			return existing.resp, nil

		case !errors.Is(err, ErrReceiveNotFound):
			return nil, err
		}
	}

	plannedVHTLCSat := amt
	vtxoFloor, err := r.receiveVTXOFloor(ctx)
	if err != nil {
		r.deps.resolveLog().WarnS(ctx, "Skipping receive VTXO floor "+
			"planning: operator terms unavailable", err)
		vtxoFloor = 0
	}
	if vtxoFloor > 0 && amt < vtxoFloor {
		availableCreditSat, err := r.availableCreditSat(ctx)
		switch {
		case err != nil:
			// Credit balance lookup failed; still route to credit
			// (a fresh wallet has no credits anyway), but log the
			// decision so the sub-floor path is not silent.
			r.deps.resolveLog().InfoS(ctx, "Routing sub-floor "+
				"receive to credit subsystem",
				slog.Uint64("amt_sat", amt),
				slog.Uint64("vtxo_floor_sat", vtxoFloor),
				slog.String("credit_balance", "unavailable"))

			return r.recvCredit(ctx, req, amt, vtxoFloor)

		case amt > ^uint64(0)-availableCreditSat ||
			amt+availableCreditSat < vtxoFloor:

			r.deps.resolveLog().InfoS(ctx, "Routing sub-floor "+
				"receive to credit subsystem",
				slog.Uint64("amt_sat", amt),
				slog.Uint64("vtxo_floor_sat", vtxoFloor),
				slog.Uint64(
					"available_credit_sat",
					availableCreditSat,
				))

			return r.recvCredit(ctx, req, amt, vtxoFloor)

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

	return r.startReceive(ctx, req, false)
}

// startReceive projects the invoice returned by the requested backend.
func (r *receiver) startReceive(ctx context.Context,
	req *wavewalletrpc.RecvRequest, activeChannelOnly bool) (
	*wavewalletrpc.RecvResponse, error) {

	startResp, err := r.deps.SwapService.StartReceive(
		ctx, &swapclientrpc.StartReceiveRequest{
			AmountSat:         int64(req.GetAmtSat()),
			Memo:              req.GetMemo(),
			ActiveChannelOnly: activeChannelOnly,
			IdempotencyKey:    req.GetRequestId(),
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

// receiveVTXOFloor returns the effective floor from the daemon's latest
// operator snapshot. Receive routing is advisory and falls open when the
// snapshot is unavailable; the swap server enforces its live floor at the
// credit boundary.
func (r *receiver) receiveVTXOFloor(ctx context.Context) (uint64, error) {
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

	server := info.GetServerInfo()

	return max(server.GetDustLimit(), server.GetMinVtxoAmountSat()), nil
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

// recvCredit hands a sub-floor receive to the durable credit subsystem. The
// registry creates the server-owned receive invoice synchronously and returns
// it, while durably tracking the operation so the pending row survives a
// restart. A keyed receive uses recv:<request_id> as its op key, so the
// registry's op-key dedup binds the key to this one operation. An unkeyed
// receive gets a fresh random key.
func (r *receiver) recvCredit(ctx context.Context,
	req *wavewalletrpc.RecvRequest, amt, vtxoFloor uint64) (
	*wavewalletrpc.RecvResponse, error) {

	if r.deps.CreditRegistry == nil {
		return nil, fmt.Errorf("%w: credit subsystem unavailable",
			ErrSwapBackendUnavailable)
	}

	keyID := req.GetRequestId()
	if keyID == "" {
		var err error
		keyID, err = newSendIntentID()
		if err != nil {
			return nil, fmt.Errorf("create credit receive id: %w",
				err)
		}
	}

	// Wait for the registry's answer even if the caller gives up: once the
	// request is queued the registry admits it regardless, and the per-ID
	// lock in Recv must cover that admission. The registry bounds the
	// admission with its own timeout.
	resp, err := r.deps.CreditRegistry.Ask(
		ctx, &credit.StartCreditReceiveRequest{
			OpKey:     "recv:" + keyID,
			AmountSat: amt,
			Memo:      req.GetMemo(),
		},
	).Await(context.WithoutCancel(ctx)).Unpack()
	if err != nil {
		// Preserve caller-owned cancellation and local shutdown. A
		// canceled RPC, or a credit registry actor that is itself
		// terminating during daemon shutdown, says nothing about the
		// operator's credit subsystem: the operator never actually
		// rejected the receive. Neither must leave a durable FAILED row
		// for the caller, so return the underlying error unchanged
		// instead of mapping it to the operator-blaming sentinel below.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, actor.ErrActorTerminated) {
			return nil, err
		}

		// A sub-floor receive that the swap server never completes must
		// not vanish silently (wavelength#1041): log it, record a
		// FAILED activity row, and return a typed, actionable error
		// instead of the opaque underlying deadline/RPC error.
		r.deps.resolveLog().WarnS(
			ctx,
			"Sub-floor credit receive failed",
			err,
			slog.Uint64("amt_sat", amt),
			slog.Uint64("vtxo_floor_sat", vtxoFloor),
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
			"sub-floor receives; receive at least %d sat instead",
			ErrCreditReceiveUnavailable, amt, vtxoFloor)
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
		req.GetMemo(), start.OpID, start.Invoice, paymentHashHex, amt,
		nowUnix(),
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
// sub-floor credit receive whose admission never completed. Without it a failed
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

func creditReceiveEntry(memo, opID, invoice, paymentHashHex string, amt uint64,
	now int64) *wavewalletrpc.WalletEntry {

	return &wavewalletrpc.WalletEntry{
		Id:            opID,
		Kind:          wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     int64(amt),
		Counterparty:  "credit",
		CreatedAtUnix: now,
		UpdatedAtUnix: now,
		Note:          memo,
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

// maxReceiveRequestIDLen bounds caller-chosen receive keys. A UUID fits with
// room to spare.
const maxReceiveRequestIDLen = 64

// keyedReceive is a receive found by its request ID, with the parameters it
// was created with so a repeated Recv can be checked against them.
type keyedReceive struct {
	resp *wavewalletrpc.RecvResponse
	amt  uint64
	memo string
}

// LookupRecv returns the receive admitted under req.request_id, built the way
// Recv built it. It only reads: neither backend is asked to create, resume,
// or route anything.
func (r *receiver) LookupRecv(ctx context.Context,
	req *wavewalletrpc.LookupRecvRequest) (*wavewalletrpc.RecvResponse,
	error) {

	if r == nil || r.deps == nil || r.deps.SwapService == nil {
		return nil, ErrSwapBackendUnavailable
	}
	if req.GetRequestId() == "" {
		return nil, status.Error(
			codes.InvalidArgument, "request_id is required",
		)
	}

	// Wait out any keyed Recv still creating under this ID, so NotFound
	// means no receive exists and none is being created.
	unlock, err := r.requests.lock(ctx, req.GetRequestId())
	if err != nil {
		return nil, err
	}
	defer unlock()

	found, err := r.lookup(ctx, req.GetRequestId())
	if err != nil {
		return nil, err
	}

	return found.resp, nil
}

// requestLocks is a per-key mutex whose waits honor the caller's context.
type requestLocks struct {
	mu   sync.Mutex
	held map[string]chan struct{}
}

// lock waits until no other holder owns key, then takes it. The returned
// function releases it.
func (l *requestLocks) lock(ctx context.Context, key string) (func(), error) {
	for {
		l.mu.Lock()
		busy, ok := l.held[key]
		if !ok {
			if l.held == nil {
				l.held = make(map[string]chan struct{})
			}
			done := make(chan struct{})
			l.held[key] = done
			l.mu.Unlock()

			return func() {
				l.mu.Lock()
				delete(l.held, key)
				l.mu.Unlock()
				close(done)
			}, nil
		}
		l.mu.Unlock()

		select {
		case <-busy:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// lookup reads the receive owned by requestID from whichever backend admitted
// it: the swap store, then the credit registry. It returns ErrReceiveNotFound
// only when both report no receive; any other failure is returned as is so a
// caller never mistakes an outage for "safe to create".
func (r *receiver) lookup(ctx context.Context, requestID string) (*keyedReceive,
	error) {

	if len(requestID) > maxReceiveRequestIDLen {
		return nil, status.Errorf(codes.InvalidArgument, "request_id "+
			"exceeds %d bytes", maxReceiveRequestIDLen)
	}

	swapResp, err := r.deps.SwapService.GetSwap(
		ctx, &swapclientrpc.GetSwapRequest{
			ReceiveIdempotencyKey: requestID,
		},
	)
	switch {
	case err == nil:
		summary := swapResp.GetSwap()
		entry := swapEntryFromSummary(
			summary, summary.GetMemo(), "",
			wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
		)

		return &keyedReceive{
			resp: &wavewalletrpc.RecvResponse{
				Invoice: summary.GetInvoice(),
				Entry:   entry,
			},
			amt:  uint64(summary.GetAmountSat()),
			memo: summary.GetMemo(),
		}, nil

	case status.Code(err) != codes.NotFound:
		return nil, fmt.Errorf("look up receive: %w", err)
	}

	if r.deps.CreditRegistry == nil {
		return nil, ErrReceiveNotFound
	}

	resp, err := r.deps.CreditRegistry.Ask(
		ctx, &credit.LookupCreditReceiveRequest{
			OpKey: "recv:" + requestID,
		},
	).Await(ctx).Unpack()
	if errors.Is(err, db.ErrCreditOperationNotFound) {
		return nil, ErrReceiveNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("look up credit receive: %w", err)
	}

	found, ok := resp.(*credit.CreditReceiveLookupResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected credit lookup response %T",
			resp)
	}

	paymentHashHex := hex.EncodeToString(found.PaymentHash)
	entry := creditReceiveEntry(
		found.Memo, found.OpID, found.Invoice, paymentHashHex,
		found.AmountSat, found.CreatedAt.Unix(),
	)

	return &keyedReceive{
		resp: &wavewalletrpc.RecvResponse{
			Invoice: found.Invoice,
			Entry:   entry,
			CreditReceive: &wavewalletrpc.CreditReceive{
				OperationId: found.OpID,
				AmountSat:   found.AmountSat,
				PaymentHash: paymentHashHex,
			},
		},
		amt:  found.AmountSat,
		memo: found.Memo,
	}, nil
}
