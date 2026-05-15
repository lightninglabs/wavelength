//go:build walletrpc && swapruntime

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
	getInfoResp    *daemonrpc.GetInfoResponse
	getInfoErr     error
	getBalanceResp *daemonrpc.GetBalanceResponse
	getBalanceErr  error
	newAddressResp *daemonrpc.NewAddressResponse
	newAddressErr  error
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
	_ *daemonrpc.ListTransactionsRequest) (
	*daemonrpc.ListTransactionsResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.listTxCalls++

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

	listSwapsResp  *swapclientrpc.ListSwapsResponse
	listSwapsErr   error
	listSwapsCalls int
}

func (f *fakeSwapService) StartPay(_ context.Context,
	req *swapclientrpc.StartPayRequest) (*swapclientrpc.StartPayResponse,
	error) {

	f.startPayCalls++
	f.startPayLastReq = req

	return f.startPayResp, f.startPayErr
}

func (f *fakeSwapService) StartReceive(_ context.Context,
	_ *swapclientrpc.StartReceiveRequest) (
	*swapclientrpc.StartReceiveResponse, error) {

	f.startReceiveCalls++

	return f.startReceiveResp, f.startReceiveErr
}

func (f *fakeSwapService) ListSwaps(_ context.Context,
	_ *swapclientrpc.ListSwapsRequest) (*swapclientrpc.ListSwapsResponse,
	error) {

	f.listSwapsCalls++

	return f.listSwapsResp, f.listSwapsErr
}

// errFakeStreamClosed is the canonical error returned by streaming-aware
// fakes when the test scope is exiting.
var errFakeStreamClosed = errors.New("fake stream closed")
