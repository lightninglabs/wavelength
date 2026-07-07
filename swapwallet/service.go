//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
)

// Service implements the daemon-side WalletService gRPC handler. It is a
// thin facade: every method translates the proto request into typed internal
// calls (router, recv, history, runtime) and returns a normalized response.
// No business logic lives here.
type Service struct {
	walletdkrpc.UnimplementedWalletServiceServer

	deps    *Deps
	runtime *Runtime
	router  *router
	recv    *receiver
	history *history
}

// newService builds the Service handle given its composed dependencies and
// runtime owner. The internal router, receiver, and history hold the
// dispatch logic for Send, Recv, and List respectively; service.go remains
// pure wiring.
func newService(deps *Deps, runtime *Runtime) *Service {
	return &Service{
		deps:    deps,
		runtime: runtime,
		router:  newRouter(deps, runtime),
		recv:    newReceiver(deps, runtime),
		history: newHistory(deps, runtime),
	}
}

// earmarkedCreditSat reports the credit balance reserved by live prepared
// credit-backed sends. The daemon wires it into the credit auto-redeem
// interlock so the sweep never redeems credits a prepared-but-unsent send is
// about to spend. It satisfies credit.EarmarkFunc.
func (s *Service) earmarkedCreditSat(context.Context) (uint64, error) {
	return s.router.intents.earmarkedCreditSat(), nil
}

// Create initializes a new wallet from a freshly generated aezeed
// mnemonic. The handler is admin-shape — it runs BEFORE the swap runtime
// is live — so it does not depend on Runtime, router, recv, or history.
func (s *Service) Create(ctx context.Context, req *walletdkrpc.CreateRequest) (
	*walletdkrpc.CreateResponse, error) {

	return s.create(ctx, req)
}

// Unlock decrypts the on-disk wallet seed and starts the wallet
// subsystem. Admin-shape handler; does not depend on Runtime.
func (s *Service) Unlock(ctx context.Context, req *walletdkrpc.UnlockRequest) (
	*walletdkrpc.UnlockResponse, error) {

	return s.unlock(ctx, req)
}

// PrepareSend validates and previews an outbound payment, returning a
// short-lived intent that Send can consume.
func (s *Service) PrepareSend(ctx context.Context,
	req *walletdkrpc.PrepareSendRequest) (*walletdkrpc.PrepareSendResponse,
	error) {

	if err := s.requireWalletReady(ctx); err != nil {
		return nil, err
	}

	return s.router.PrepareSend(ctx, req)
}

// Send dispatches a previously prepared outbound payment. Invoice intents
// route through the daemon-owned swap subserver; onchain intents route through
// the existing LeaveVTXOs cooperative-exit RPC.
func (s *Service) Send(ctx context.Context, req *walletdkrpc.SendRequest) (
	*walletdkrpc.SendResponse, error) {

	if err := s.requireWalletReady(ctx); err != nil {
		return nil, err
	}

	return s.router.Send(ctx, req)
}

// Exit queues cooperative leave by default, or starts forced unroll when the
// request carries the exact acknowledgement string.
func (s *Service) Exit(ctx context.Context, req *walletdkrpc.ExitRequest) (
	*walletdkrpc.ExitResponse, error) {

	return s.exit(ctx, req)
}

// GetExitPlan previews whether the backing wallet has enough confirmed fee
// inputs for unilateral exit and returns a backing-wallet funding address when
// more fee inputs are needed.
func (s *Service) GetExitPlan(ctx context.Context,
	req *walletdkrpc.GetExitPlanRequest) (*walletdkrpc.GetExitPlanResponse,
	error) {

	return s.getExitPlan(ctx, req)
}

// SweepWallet previews or broadcasts a normal backing-wallet sweep. Boarding
// UTXOs remain owned by the dedicated boarding-sweep flow.
func (s *Service) SweepWallet(ctx context.Context,
	req *walletdkrpc.SweepWalletRequest) (*walletdkrpc.SweepWalletResponse,
	error) {

	return s.sweepWallet(ctx, req)
}

// ExitStatus reports the current phase of an exit (unroll) job for the
// specified VTXO outpoint by proxying daemonrpc.GetUnrollStatus.
func (s *Service) ExitStatus(ctx context.Context,
	req *walletdkrpc.ExitStatusRequest) (*walletdkrpc.ExitStatusResponse,
	error) {

	return s.exitStatus(ctx, req)
}

// Recv opens an out-swap via the daemon-owned swap subserver and returns the
// daemon-signed BOLT-11 invoice plus the initial WalletEntry. The invoice
// is signed with a payment-scoped daemon-managed auth key (PR #337);
// nothing in the wallet layer generates or holds raw private keys.
func (s *Service) Recv(ctx context.Context, req *walletdkrpc.RecvRequest) (
	*walletdkrpc.RecvResponse, error) {

	if err := s.requireWalletReady(ctx); err != nil {
		return nil, err
	}

	return s.recv.Recv(ctx, req)
}

// List returns the unified, normalized wallet history merged across the
// swap subserver and the daemon's ledger/sweep stores.
func (s *Service) List(ctx context.Context, req *walletdkrpc.ListRequest) (
	*walletdkrpc.ListResponse, error) {

	return s.history.List(ctx, req)
}

// Deposit returns a fresh boarding onchain address by delegating to the
// daemon's existing NewAddress RPC. The wallet layer never derives keys or
// constructs scripts itself.
func (s *Service) Deposit(ctx context.Context,
	req *walletdkrpc.DepositRequest) (*walletdkrpc.DepositResponse, error) {

	if err := s.requireWalletReady(ctx); err != nil {
		return nil, err
	}

	addrResp, err := s.deps.RPCServer.NewAddress(
		ctx, &daemonrpc.NewAddressRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("new boarding address: %w", err)
	}

	// The returned entry is keyed by the address-scoped id the confirmed
	// deposit will later carry (deposit-<address>), so a caller can
	// correlate this response with the eventual activity row. It is
	// deliberately NOT projected into the store: merely allocating an
	// address is not a pending deposit (no funds are in flight), so
	// persisting it would strand a PENDING row for every unfunded address a
	// user ever requests. A deposit becomes an activity row only once the
	// daemon records the incoming UTXO (at confirmation); before that,
	// unconfirmed boarding funds surface via Balance.
	createdAt := nowUnix()
	entry := &walletdkrpc.WalletEntry{
		Id:            fmt.Sprintf("deposit-%s", addrResp.GetAddress()),
		Kind:          walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     int64(req.GetAmtSatHint()),
		Counterparty:  "boarding",
		CreatedAtUnix: createdAt,
		UpdatedAtUnix: createdAt,
		Request:       requestFromOnchainAddress(addrResp.GetAddress()),
		Progress: &walletdkrpc.WalletEntryProgress{
			Phase:      walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_REQUEST_CREATED,
			PhaseLabel: "address_issued",
		},
	}

	return &walletdkrpc.DepositResponse{
		OnchainAddress: addrResp.GetAddress(),
		Entry:          entry,
	}, nil
}

// requireWalletReady rejects wallet verbs that need unlocked key material
// before they can reach swap or address-generation code paths.
func (s *Service) requireWalletReady(ctx context.Context) error {
	if s == nil || s.deps == nil || s.deps.RPCServer == nil {
		return statusSwapBackendUnavailable()
	}

	info, err := s.deps.RPCServer.GetInfo(
		ctx, &daemonrpc.GetInfoRequest{},
	)
	if err != nil {
		return fmt.Errorf("get info: %w", err)
	}

	switch info.GetWalletState() {
	case daemonrpc.WalletState_WALLET_STATE_READY:
		return nil

	case daemonrpc.WalletState_WALLET_STATE_NONE:
		return daemonrpc.WalletNotReadyStateError(
			"wallet is not ready",
			daemonrpc.WalletNotReadyStateNone,
		)

	case daemonrpc.WalletState_WALLET_STATE_LOCKED:
		return daemonrpc.WalletNotReadyStateError(
			"wallet is not ready",
			daemonrpc.WalletNotReadyStateLocked,
		)

	case daemonrpc.WalletState_WALLET_STATE_SYNCING:
		return daemonrpc.WalletNotReadyStateError(
			"wallet is not ready",
			daemonrpc.WalletNotReadyStateSyncing,
		)

	default:
		return daemonrpc.WalletNotReadyStateError(
			"wallet is not ready",
			daemonrpc.WalletNotReadyStateUnknown,
		)
	}
}

// Balance composes the unified balance summary by reading the daemon's
// existing GetBalance RPC and projecting its fields onto the flat
// confirmed/pending shape exposed by the wallet layer.
func (s *Service) Balance(ctx context.Context,
	req *walletdkrpc.BalanceRequest) (*walletdkrpc.BalanceResponse, error) {

	return s.fetchBalance(ctx)
}

// Status returns wallet readiness, network, balance summary, and pending
// count in one shot by composing over the daemon's existing GetInfo,
// GetBalance, and the swap subserver's ListSwaps (pending_only).
func (s *Service) Status(ctx context.Context, req *walletdkrpc.StatusRequest) (
	*walletdkrpc.StatusResponse, error) {

	if s.deps.RPCServer == nil {
		return nil, statusSwapBackendUnavailable()
	}

	info, err := s.deps.RPCServer.GetInfo(
		ctx, &daemonrpc.GetInfoRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("get info: %w", err)
	}

	bal, err := s.fetchBalance(ctx)
	if err != nil {
		return nil, err
	}

	pendingCount, err := s.countPendingEntries(ctx)
	if err != nil {
		return nil, err
	}

	state := info.GetWalletState()

	return &walletdkrpc.StatusResponse{
		// Ready collapses to "wallet fully usable for signing".
		Ready: state == daemonrpc.WalletState_WALLET_STATE_READY,

		// Unlocked retains the legacy wallet-exists signal exposed
		// by this field. Use Ready to determine whether wallet RPCs
		// are currently usable.
		Unlocked: state ==
			daemonrpc.WalletState_WALLET_STATE_LOCKED ||
			state == daemonrpc.WalletState_WALLET_STATE_SYNCING ||
			state == daemonrpc.WalletState_WALLET_STATE_READY,

		Network:      info.GetNetwork(),
		Balance:      bal,
		PendingCount: pendingCount,
	}, nil
}

// SubscribeWallet streams WalletEntry updates. v1 optionally emits the
// current snapshot before live updates and then forwards updates from the
// runtime event bus until the caller disconnects or the daemon shuts down.
func (s *Service) SubscribeWallet(req *walletdkrpc.SubscribeWalletRequest,
	stream walletdkrpc.WalletService_SubscribeWalletServer) error {

	ctx := stream.Context()

	// Subscribe BEFORE taking the snapshot so the runtime starts
	// buffering updates immediately. Otherwise any update that fires
	// between snapshot fetch and subscribe registration is lost to this
	// stream. Clients dedupe by WalletEntry.id, so the snapshot+stream
	// overlap window is harmless.
	updates := s.runtime.subscribe()
	defer s.runtime.unsubscribe(updates)

	if req.GetIncludeExisting() {
		// Use the configured hard cap rather than the default page
		// size so wallets with more than defaultListLimit entries
		// receive a complete initial snapshot. A truncated snapshot
		// would let the subscriber observe live updates that
		// reference rows it never saw.
		snapshot, err := s.history.List(ctx, &walletdkrpc.ListRequest{
			View:  walletdkrpc.ListView_LIST_VIEW_ACTIVITY,
			Kinds: req.GetKinds(),
			Limit: s.deps.resolveMaxListLimit(),
		})
		if err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}

		for _, e := range snapshot.GetActivity().GetEntries() {
			if err := stream.Send(e); err != nil {
				return err
			}
		}
	}

	kindFilter, err := buildKindFilter(req.GetKinds())
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case e, ok := <-updates:
			if !ok {
				return nil
			}
			if len(kindFilter) > 0 {
				if _, in := kindFilter[e.GetKind()]; !in {
					continue
				}
			}
			if err := stream.Send(e); err != nil {
				return err
			}
		}
	}
}

// fetchBalance is the shared helper that pulls the daemon's GetBalance and
// projects its richer breakdown onto the flat wallet shape.
func (s *Service) fetchBalance(ctx context.Context) (
	*walletdkrpc.BalanceResponse, error) {

	if s.deps.RPCServer == nil {
		return nil, statusSwapBackendUnavailable()
	}

	bal, err := s.deps.RPCServer.GetBalance(
		ctx, &daemonrpc.GetBalanceRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("get balance: %w", err)
	}

	// confirmed_sat is the spendable VTXO balance only — funds the
	// user can actually send right now. Boarding-confirmed funds
	// are NOT spendable as VTXOs until `ark board` registers them
	// into a round, so they belong in pending_in_sat alongside
	// boarding-unconfirmed. Once a round checkpoint adopts the boarding
	// UTXO, the funding output disappears from ListUnspent before the
	// resulting VTXO becomes live; keep that adopted amount in
	// pending_in_sat so the user does not see balance drop to zero
	// during commitment confirmation. The daemon's GetBalance shape
	// returns total_confirmed_sat = vtxo_balance_sat +
	// boarding_confirmed_sat; mapping that conflated total onto
	// confirmed_sat would tell the user they have spendable balance
	// immediately after a faucet deposit, before any round commit, which
	// the proto contract (and the `send` verb's runtime check)
	// explicitly disallow.
	resp := &walletdkrpc.BalanceResponse{
		ConfirmedSat: bal.GetVtxoBalanceSat(),
		PendingInSat: bal.GetBoardingConfirmedSat() +
			bal.GetBoardingUnconfirmedSat() +
			bal.GetBoardingAdoptedSat(),
		PendingOutSat: bal.GetBoardingPendingSweepSat(),
	}

	if s.deps.SwapService == nil {
		return resp, nil
	}

	credits, err := s.deps.SwapService.ListCredits(
		ctx, &swapclientrpc.ListCreditsRequest{
			Limit: 1,
		},
	)
	if err != nil {
		return resp, nil
	}

	resp.CreditAvailableSat = credits.GetAvailableSat()
	resp.CreditReservedSat = credits.GetReservedSat()

	return resp, nil
}

// countPendingEntries returns the wallet-level count of in-flight entries. It
// delegates to the history merger's full-feed pending count rather than a
// List page total, which is capped at one page under cursor pagination.
func (s *Service) countPendingEntries(ctx context.Context) (uint32, error) {
	count, err := s.history.countPending(ctx)
	if err != nil {
		return 0, fmt.Errorf("count pending: %w", err)
	}

	return count, nil
}
