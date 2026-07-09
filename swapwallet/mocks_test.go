//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"
	"sync"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/wallet"
)

// fakeRPCServer is a hand-written implementation of swapwallet.RPCServer
// (the narrow interface defined in deps.go) used by router/recv/history/
// service tests. Each method returns whatever the test sets on the
// corresponding response field, or the corresponding err field.
type fakeRPCServer struct {
	mu sync.Mutex

	listVTXOsResp    *daemonrpc.ListVTXOsResponse
	listVTXOsErr     error
	listVTXOsCalls   int
	listVTXOsLastReq *daemonrpc.ListVTXOsRequest
	leaveResp        *daemonrpc.LeaveVTXOsResponse
	leaveErr         error
	leaveCalls       int
	leaveLastReq     *daemonrpc.LeaveVTXOsRequest
	listTxResp       *daemonrpc.ListTransactionsResponse
	listTxErr        error
	listTxCalls      int
	listTxLastReq    *daemonrpc.ListTransactionsRequest
	getInfoResp      *daemonrpc.GetInfoResponse
	getInfoErr       error
	getBalanceResp   *daemonrpc.GetBalanceResponse
	getBalanceErr    error
	newAddressResp   *daemonrpc.NewAddressResponse
	newAddressErr    error

	listVTXOsByStatus map[daemonrpc.VTXOStatus]*daemonrpc.
				ListVTXOsResponse

	newWalletAddressResp string
	newWalletAddressErr  error
	listWalletUnspent    []*wallet.Utxo
	listWalletUnspentErr error

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

	exitSummaryResp  *darepod.ExitSummaryResult
	exitSummaryErr   error
	exitSummaryCalls int

	exitPlanResp  *darepod.ExitPlanResponse
	exitPlanErr   error
	exitPlanCalls int
	exitPlanLast  *darepod.ExitPlanRequest

	sweepWalletResp  *darepod.SweepWalletResponse
	sweepWalletErr   error
	sweepWalletCalls int
	sweepWalletLast  *darepod.SweepWalletRequest

	joinNextRoundResp  *daemonrpc.JoinNextRoundResponse
	joinNextRoundErr   error
	joinNextRoundCalls int

	sendOnChainResp    *daemonrpc.SendOnChainResponse
	sendOnChainErr     error
	sendOnChainCalls   int
	sendOnChainLastReq *daemonrpc.SendOnChainRequest

	sendOORResp    *daemonrpc.SendOORResponse
	sendOORErr     error
	sendOORCalls   int
	sendOORLastReq *daemonrpc.SendOORRequest

	estimateFeeResp    *daemonrpc.EstimateFeeResponse
	estimateFeeErr     error
	estimateFeeCalls   int
	estimateFeeLastReq *daemonrpc.EstimateFeeRequest
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
	req *daemonrpc.ListVTXOsRequest) (*daemonrpc.ListVTXOsResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.listVTXOsCalls++
	f.listVTXOsLastReq = req

	if f.listVTXOsByStatus != nil {
		if resp, ok := f.listVTXOsByStatus[req.GetStatusFilter()]; ok {
			return resp, f.listVTXOsErr
		}
	}

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

	if f.getInfoResp == nil && f.getInfoErr == nil {
		return &daemonrpc.GetInfoResponse{
			WalletState: daemonrpc.WalletState_WALLET_STATE_READY,
		}, nil
	}

	return f.getInfoResp, f.getInfoErr
}

func (f *fakeRPCServer) EstimateFee(_ context.Context,
	req *daemonrpc.EstimateFeeRequest) (*daemonrpc.EstimateFeeResponse,
	error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.estimateFeeCalls++
	f.estimateFeeLastReq = req

	return f.estimateFeeResp, f.estimateFeeErr
}

func (f *fakeRPCServer) GetBalance(_ context.Context,
	_ *daemonrpc.GetBalanceRequest) (*daemonrpc.GetBalanceResponse, error) {

	return f.getBalanceResp, f.getBalanceErr
}

func (f *fakeRPCServer) NewAddress(_ context.Context,
	_ *daemonrpc.NewAddressRequest) (*daemonrpc.NewAddressResponse, error) {

	return f.newAddressResp, f.newAddressErr
}

func (f *fakeRPCServer) NewWalletAddress(_ context.Context) (string, error) {
	return f.newWalletAddressResp, f.newWalletAddressErr
}

func (f *fakeRPCServer) ListWalletUnspent(_ context.Context, _ int32, _ int32) (
	[]*wallet.Utxo, error) {

	return f.listWalletUnspent, f.listWalletUnspentErr
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

	// Mirror the real daemon: GetUnrollStatus parses its argument as a
	// txid:vout outpoint and rejects anything else. A caller that feeds it
	// a bare send_job_id hash must fail here, so a regression that queries
	// by the hash id is caught by a test rather than only at runtime.
	if !looksLikeOutpoint(req.GetOutpoint()) {
		return nil, errors.New("invalid outpoint: " + req.GetOutpoint())
	}

	return f.unrollStatusResp, f.unrollStatusErr
}

func (f *fakeRPCServer) ExitSummary(_ context.Context) (
	*darepod.ExitSummaryResult, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.exitSummaryCalls++

	return f.exitSummaryResp, f.exitSummaryErr
}

func (f *fakeRPCServer) GetExitPlan(_ context.Context,
	req *darepod.ExitPlanRequest) (*darepod.ExitPlanResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.exitPlanCalls++
	f.exitPlanLast = req

	return f.exitPlanResp, f.exitPlanErr
}

func (f *fakeRPCServer) SweepWallet(_ context.Context,
	req *darepod.SweepWalletRequest) (*darepod.SweepWalletResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.sweepWalletCalls++
	f.sweepWalletLast = req

	return f.sweepWalletResp, f.sweepWalletErr
}

func (f *fakeRPCServer) JoinNextRound(_ context.Context,
	_ *daemonrpc.JoinNextRoundRequest) (*daemonrpc.JoinNextRoundResponse,
	error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.joinNextRoundCalls++

	return f.joinNextRoundResp, f.joinNextRoundErr
}

func (f *fakeRPCServer) SendOnChain(_ context.Context,
	req *daemonrpc.SendOnChainRequest) (*daemonrpc.SendOnChainResponse,
	error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.sendOnChainCalls++
	f.sendOnChainLastReq = req

	return f.sendOnChainResp, f.sendOnChainErr
}

func (f *fakeRPCServer) SendOOR(_ context.Context,
	req *daemonrpc.SendOORRequest) (*daemonrpc.SendOORResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.sendOORCalls++
	f.sendOORLastReq = req

	return f.sendOORResp, f.sendOORErr
}

// fakeSwapService is a minimal swapclientrpc.SwapClientServiceServer used
// by router/recv/history/service tests. Each method returns canned
// responses or errors set by the test. Streaming methods are not invoked
// by the unit tests; the monitor loop has its own dedicated test that
// uses a different stream-aware fake.
type fakeSwapService struct {
	swapclientrpc.UnimplementedSwapClientServiceServer

	quotePayResp    *swapclientrpc.QuotePayResponse
	quotePayErr     error
	quotePayCalls   int
	quotePayLastReq *swapclientrpc.QuotePayRequest

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

	createCreditResp  *swapclientrpc.CreateCreditResponse
	createCreditErr   error
	createCreditCalls int
	createCreditLast  *swapclientrpc.CreateCreditRequest

	listCreditsResp  *swapclientrpc.ListCreditsResponse
	listCreditsErr   error
	listCreditsCalls int
	listCreditsLast  *swapclientrpc.ListCreditsRequest
}

func (f *fakeSwapService) QuotePay(_ context.Context,
	req *swapclientrpc.QuotePayRequest) (*swapclientrpc.QuotePayResponse,
	error) {

	f.quotePayCalls++
	f.quotePayLastReq = req

	return f.quotePayResp, f.quotePayErr
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

func (f *fakeSwapService) CreateCredit(_ context.Context,
	req *swapclientrpc.CreateCreditRequest) (
	*swapclientrpc.CreateCreditResponse, error) {

	f.createCreditCalls++
	f.createCreditLast = req

	return f.createCreditResp, f.createCreditErr
}

func (f *fakeSwapService) ListCredits(_ context.Context,
	req *swapclientrpc.ListCreditsRequest) (
	*swapclientrpc.ListCreditsResponse, error) {

	f.listCreditsCalls++
	f.listCreditsLast = req

	return f.listCreditsResp, f.listCreditsErr
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
