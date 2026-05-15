//go:build walletrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service implements the daemon-side WalletService gRPC handler. It is a
// thin facade: every method translates the proto request into typed internal
// calls (router, recv, history, runtime) and returns a normalized response.
// No business logic lives here.
type Service struct {
	walletrpc.UnimplementedWalletServiceServer

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

// Send dispatches an outbound payment. Invoice destinations route through
// the daemon-owned swap subserver (which transparently picks same-Ark p2p
// vHTLC vs real Lightning per PR #339); onchain destinations route through
// the existing LeaveVTXOs cooperative-exit RPC.
func (s *Service) Send(ctx context.Context, req *walletrpc.SendRequest) (
	*walletrpc.SendResponse, error) {

	return s.router.Send(ctx, req)
}

// Recv opens a swap-in via the daemon-owned swap subserver and returns the
// daemon-signed BOLT-11 invoice plus the initial WalletEntry. The invoice
// is signed with a payment-scoped daemon-managed auth key (PR #337);
// nothing in the wallet layer generates or holds raw private keys.
func (s *Service) Recv(ctx context.Context, req *walletrpc.RecvRequest) (
	*walletrpc.RecvResponse, error) {

	return s.recv.Recv(ctx, req)
}

// List returns the unified, normalized wallet history merged across the
// swap subserver and the daemon's ledger/sweep stores.
func (s *Service) List(ctx context.Context, req *walletrpc.ListRequest) (
	*walletrpc.ListResponse, error) {

	return s.history.List(ctx, req)
}

// Deposit returns a fresh boarding onchain address by delegating to the
// daemon's existing NewAddress RPC. The wallet layer never derives keys or
// constructs scripts itself.
func (s *Service) Deposit(ctx context.Context, req *walletrpc.DepositRequest) (
	*walletrpc.DepositResponse, error) {

	if s.deps.RPCServer == nil {
		return nil, status.Error(
			codes.Unavailable, ErrSwapBackendUnavailable.Error(),
		)
	}

	addrResp, err := s.deps.RPCServer.NewAddress(
		ctx, &daemonrpc.NewAddressRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("new boarding address: %w", err)
	}

	createdAt := nowUnix()
	canonicalID := fmt.Sprintf("deposit-%s", addrResp.GetAddress())
	entry := &walletrpc.WalletEntry{
		Id:            canonicalID,
		Kind:          walletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		Status:        walletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     int64(req.GetAmtSatHint()),
		Counterparty:  "boarding",
		CreatedAtUnix: createdAt,
		UpdatedAtUnix: createdAt,
	}

	s.runtime.registerDepositIntent(canonicalID, addrResp.GetAddress())

	return &walletrpc.DepositResponse{
		OnchainAddress: addrResp.GetAddress(),
		Entry:          entry,
	}, nil
}

// Balance composes the unified balance summary by reading the daemon's
// existing GetBalance RPC and projecting its fields onto the flat
// confirmed/pending shape exposed by the wallet layer.
func (s *Service) Balance(ctx context.Context, req *walletrpc.BalanceRequest) (
	*walletrpc.BalanceResponse, error) {

	return s.fetchBalance(ctx)
}

// Status returns wallet readiness, network, balance summary, and pending
// count in one shot by composing over the daemon's existing GetInfo,
// GetBalance, and the swap subserver's ListSwaps (pending_only).
func (s *Service) Status(ctx context.Context, req *walletrpc.StatusRequest) (
	*walletrpc.StatusResponse, error) {

	if s.deps.RPCServer == nil {
		return nil, status.Error(
			codes.Unavailable, ErrSwapBackendUnavailable.Error(),
		)
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

	return &walletrpc.StatusResponse{
		Ready:        info.GetWalletReady(),
		Unlocked:     info.GetWalletReady(),
		Network:      info.GetNetwork(),
		Balance:      bal,
		PendingCount: pendingCount,
	}, nil
}

// SubscribeWallet streams WalletEntry updates. v1 optionally emits the
// current snapshot before live updates and then forwards updates from the
// runtime event bus until the caller disconnects or the daemon shuts down.
func (s *Service) SubscribeWallet(req *walletrpc.SubscribeWalletRequest,
	stream walletrpc.WalletService_SubscribeWalletServer) error {

	ctx := stream.Context()

	if req.GetIncludeExisting() {
		snapshot, err := s.history.List(ctx, &walletrpc.ListRequest{
			Kinds: req.GetKinds(),
		})
		if err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}

		for _, e := range snapshot.GetEntries() {
			if err := stream.Send(e); err != nil {
				return err
			}
		}
	}

	updates := s.runtime.subscribe()
	defer s.runtime.unsubscribe(updates)

	kindFilter := buildKindFilter(req.GetKinds())

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
func (s *Service) fetchBalance(ctx context.Context) (*walletrpc.BalanceResponse,
	error) {

	if s.deps.RPCServer == nil {
		return nil, status.Error(
			codes.Unavailable, ErrSwapBackendUnavailable.Error(),
		)
	}

	bal, err := s.deps.RPCServer.GetBalance(
		ctx, &daemonrpc.GetBalanceRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("get balance: %w", err)
	}

	return &walletrpc.BalanceResponse{
		ConfirmedSat:  bal.GetTotalConfirmedSat(),
		PendingInSat:  bal.GetBoardingUnconfirmedSat(),
		PendingOutSat: bal.GetBoardingPendingSweepSat(),
	}, nil
}

// countPendingEntries asks the history merger for a pending-only page and
// returns the size as the wallet-level pending count.
func (s *Service) countPendingEntries(ctx context.Context) (uint32, error) {
	resp, err := s.history.List(ctx, &walletrpc.ListRequest{
		PendingOnly: true,
		Limit:       s.deps.resolveMaxListLimit(),
	})
	if err != nil {
		return 0, fmt.Errorf("count pending: %w", err)
	}

	return resp.GetTotal(), nil
}
