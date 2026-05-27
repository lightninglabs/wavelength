//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"
	"sync"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
)

// fakeRPCServer is a hand-written implementation of swapwallet.RPCServer
// (the narrow interface defined in deps.go) used by router/recv/history/
// service tests. Each method returns whatever the test sets on the
// corresponding response field, or the corresponding err field.
type fakeRPCServer struct {
	mu sync.Mutex

	listVTXOsResp  *daemonrpc.ListVTXOsResponse
	listVTXOsErr   error
	listVTXOsCalls int
	leaveResp      *daemonrpc.LeaveVTXOsResponse
	leaveErr       error
	leaveCalls     int
	leaveLastReq   *daemonrpc.LeaveVTXOsRequest
	listTxResp     *daemonrpc.ListTransactionsResponse
	listTxErr      error
	listTxCalls    int
	listTxLastReq  *daemonrpc.ListTransactionsRequest
	getInfoResp    *daemonrpc.GetInfoResponse
	getInfoErr     error
	getBalanceResp *daemonrpc.GetBalanceResponse
	getBalanceErr  error
	newAddressResp *daemonrpc.NewAddressResponse
	newAddressErr  error

	genSeedResp     *daemonrpc.GenSeedResponse
	genSeedErr      error
	genSeedCalls    int
	genSeedLastReq  *daemonrpc.GenSeedRequest
	initWalletResp  *daemonrpc.InitWalletResponse
	initWalletErr   error
	initWalletCalls int
	initWalletLast  *daemonrpc.InitWalletRequest

	unlockWalletResp  *daemonrpc.UnlockWalletResponse
	unlockWalletErr   error
	unlockWalletCalls int
	unlockWalletLast  *daemonrpc.UnlockWalletRequest

	unrollResp  *daemonrpc.UnrollResponse
	unrollErr   error
	unrollCalls int
	unrollLast  *daemonrpc.UnrollRequest

	unrollStatusResp  *daemonrpc.GetUnrollStatusResponse
	unrollStatusErr   error
	unrollStatusCalls int
	unrollStatusLast  *daemonrpc.GetUnrollStatusRequest

	joinNextRoundResp  *daemonrpc.JoinNextRoundResponse
	joinNextRoundErr   error
	joinNextRoundCalls int
}

func (f *fakeRPCServer) LeaveVTXOs(_ context.Context,
	req *daemonrpc.LeaveVTXOsRequest) (*daemonrpc.LeaveVTXOsResponse,
	error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.leaveCalls++
	f.leaveLastReq = req

	return f.leaveResp, f.leaveErr
}

func (f *fakeRPCServer) ListVTXOs(_ context.Context,
	_ *daemonrpc.ListVTXOsRequest) (*daemonrpc.ListVTXOsResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.listVTXOsCalls++

	return f.listVTXOsResp, f.listVTXOsErr
}

func (f *fakeRPCServer) ListTransactions(_ context.Context,
	req *daemonrpc.ListTransactionsRequest) (
	*daemonrpc.ListTransactionsResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.listTxCalls++
	f.listTxLastReq = req

	return f.listTxResp, f.listTxErr
}

func (f *fakeRPCServer) GetInfo(_ context.Context,
	_ *daemonrpc.GetInfoRequest) (*daemonrpc.GetInfoResponse, error) {

	return f.getInfoResp, f.getInfoErr
}

func (f *fakeRPCServer) GetBalance(_ context.Context,
	_ *daemonrpc.GetBalanceRequest) (*daemonrpc.GetBalanceResponse, error) {

	return f.getBalanceResp, f.getBalanceErr
}

func (f *fakeRPCServer) NewAddress(_ context.Context,
	_ *daemonrpc.NewAddressRequest) (*daemonrpc.NewAddressResponse, error) {

	return f.newAddressResp, f.newAddressErr
}

func (f *fakeRPCServer) GenSeed(_ context.Context,
	req *daemonrpc.GenSeedRequest) (*daemonrpc.GenSeedResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.genSeedCalls++
	f.genSeedLastReq = req

	return f.genSeedResp, f.genSeedErr
}

func (f *fakeRPCServer) InitWallet(_ context.Context,
	req *daemonrpc.InitWalletRequest) (*daemonrpc.InitWalletResponse,
	error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.initWalletCalls++
	f.initWalletLast = req

	return f.initWalletResp, f.initWalletErr
}

func (f *fakeRPCServer) UnlockWallet(_ context.Context,
	req *daemonrpc.UnlockWalletRequest) (*daemonrpc.UnlockWalletResponse,
	error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.unlockWalletCalls++
	f.unlockWalletLast = req

	return f.unlockWalletResp, f.unlockWalletErr
}

func (f *fakeRPCServer) Unroll(_ context.Context,
	req *daemonrpc.UnrollRequest) (*daemonrpc.UnrollResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.unrollCalls++
	f.unrollLast = req

	return f.unrollResp, f.unrollErr
}

func (f *fakeRPCServer) GetUnrollStatus(_ context.Context,
	req *daemonrpc.GetUnrollStatusRequest) (
	*daemonrpc.GetUnrollStatusResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.unrollStatusCalls++
	f.unrollStatusLast = req

	return f.unrollStatusResp, f.unrollStatusErr
}

func (f *fakeRPCServer) JoinNextRound(_ context.Context,
	_ *daemonrpc.JoinNextRoundRequest) (*daemonrpc.JoinNextRoundResponse,
	error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.joinNextRoundCalls++

	return f.joinNextRoundResp, f.joinNextRoundErr
}

// fakeSwapService is a minimal swapclientrpc.SwapClientServiceServer used
// by router/recv/history/service tests. Each method returns canned
// responses or errors set by the test. Streaming methods are not invoked
// by the unit tests; the monitor loop has its own dedicated test that
// uses a different stream-aware fake.
type fakeSwapService struct {
	swapclientrpc.UnimplementedSwapClientServiceServer

	startPayResp    *swapclientrpc.StartPayResponse
	startPayErr     error
	startPayCalls   int
	startPayLastReq *swapclientrpc.StartPayRequest

	startReceiveResp  *swapclientrpc.StartReceiveResponse
	startReceiveErr   error
	startReceiveCalls int
	startReceiveLast  *swapclientrpc.StartReceiveRequest

	listSwapsResp  *swapclientrpc.ListSwapsResponse
	listSwapsErr   error
	listSwapsCalls int
	listSwapsLast  *swapclientrpc.ListSwapsRequest
}

func (f *fakeSwapService) StartPay(_ context.Context,
	req *swapclientrpc.StartPayRequest) (*swapclientrpc.StartPayResponse,
	error) {

	f.startPayCalls++
	f.startPayLastReq = req

	return f.startPayResp, f.startPayErr
}

func (f *fakeSwapService) StartReceive(_ context.Context,
	req *swapclientrpc.StartReceiveRequest) (
	*swapclientrpc.StartReceiveResponse, error) {

	f.startReceiveCalls++
	f.startReceiveLast = req

	return f.startReceiveResp, f.startReceiveErr
}

func (f *fakeSwapService) ListSwaps(_ context.Context,
	req *swapclientrpc.ListSwapsRequest) (*swapclientrpc.ListSwapsResponse,
	error) {

	f.listSwapsCalls++
	f.listSwapsLast = req

	if !req.GetPendingOnly() || f.listSwapsResp == nil {
		return f.listSwapsResp, f.listSwapsErr
	}

	filtered := &swapclientrpc.ListSwapsResponse{
		Swaps: make(
			[]*swapclientrpc.SwapSummary, 0,
			len(
				f.listSwapsResp.GetSwaps(),
			),
		),
	}
	for _, swap := range f.listSwapsResp.GetSwaps() {
		if swap.GetPending() {
			filtered.Swaps = append(filtered.Swaps, swap)
		}
	}

	return filtered, f.listSwapsErr
}

// errFakeStreamClosed is the canonical error returned by streaming-aware
// fakes when the test scope is exiting.
var errFakeStreamClosed = errors.New("fake stream closed")
