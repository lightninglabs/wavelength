//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"
	"sync"

	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
)

// fakeRPCServer is a hand-written implementation of swapwallet.RPCServer
// (the narrow interface defined in deps.go) used by router/recv/history/
// service tests. Each method returns whatever the test sets on the
// corresponding response field, or the corresponding err field.
type fakeRPCServer struct {
	mu sync.Mutex

	listVTXOsResp    *waverpc.ListVTXOsResponse
	listVTXOsErr     error
	listVTXOsCalls   int
	listVTXOsLastReq *waverpc.ListVTXOsRequest
	leaveResp        *waverpc.LeaveVTXOsResponse
	leaveErr         error
	leaveCalls       int
	leaveLastReq     *waverpc.LeaveVTXOsRequest
	listTxResp       *waverpc.ListTransactionsResponse
	listTxErr        error
	listTxCalls      int
	listTxLastReq    *waverpc.ListTransactionsRequest
	getInfoResp      *waverpc.GetInfoResponse
	getInfoErr       error
	getBalanceResp   *waverpc.GetBalanceResponse
	getBalanceErr    error
	newAddressResp   *waverpc.NewAddressResponse
	newAddressErr    error

	listVTXOsByStatus map[waverpc.VTXOStatus]*waverpc.
				ListVTXOsResponse

	newWalletAddressResp string
	newWalletAddressErr  error
	listWalletUnspent    []*wallet.Utxo
	listWalletUnspentErr error
	activeBoardingAddrs  []string
	activeBoardingErr    error

	genSeedResp     *waverpc.GenSeedResponse
	genSeedErr      error
	genSeedCalls    int
	genSeedLastReq  *waverpc.GenSeedRequest
	initWalletResp  *waverpc.InitWalletResponse
	initWalletErr   error
	initWalletCalls int
	initWalletLast  *waverpc.InitWalletRequest

	unlockWalletResp  *waverpc.UnlockWalletResponse
	unlockWalletErr   error
	unlockWalletCalls int
	unlockWalletLast  *waverpc.UnlockWalletRequest

	unrollResp  *waverpc.UnrollResponse
	unrollErr   error
	unrollCalls int
	unrollLast  *waverpc.UnrollRequest

	unrollStatusResp  *waverpc.GetUnrollStatusResponse
	unrollStatusErr   error
	unrollStatusCalls int
	unrollStatusLast  *waverpc.GetUnrollStatusRequest

	exitSummaryResp  *waved.ExitSummaryResult
	exitSummaryErr   error
	exitSummaryCalls int

	exitPlanResp  *waved.ExitPlanResponse
	exitPlanErr   error
	exitPlanCalls int
	exitPlanLast  *waved.ExitPlanRequest

	sweepWalletResp  *waved.SweepWalletResponse
	sweepWalletErr   error
	sweepWalletCalls int
	sweepWalletLast  *waved.SweepWalletRequest

	joinNextRoundResp  *waverpc.JoinNextRoundResponse
	joinNextRoundErr   error
	joinNextRoundCalls int

	sendOnChainResp    *waverpc.SendOnChainResponse
	sendOnChainErr     error
	sendOnChainCalls   int
	sendOnChainLastReq *waverpc.SendOnChainRequest

	sendOORResp    *waverpc.SendOORResponse
	sendOORErr     error
	sendOORCalls   int
	sendOORLastReq *waverpc.SendOORRequest

	estimateFeeResp    *waverpc.EstimateFeeResponse
	estimateFeeErr     error
	estimateFeeCalls   int
	estimateFeeLastReq *waverpc.EstimateFeeRequest
}

func (f *fakeRPCServer) LeaveVTXOs(_ context.Context,
	req *waverpc.LeaveVTXOsRequest) (*waverpc.LeaveVTXOsResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.leaveCalls++
	f.leaveLastReq = req

	return f.leaveResp, f.leaveErr
}

func (f *fakeRPCServer) ListVTXOs(_ context.Context,
	req *waverpc.ListVTXOsRequest) (*waverpc.ListVTXOsResponse, error) {

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
	req *waverpc.ListTransactionsRequest) (
	*waverpc.ListTransactionsResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.listTxCalls++
	f.listTxLastReq = req

	return f.listTxResp, f.listTxErr
}

func (f *fakeRPCServer) GetInfo(_ context.Context, _ *waverpc.GetInfoRequest) (
	*waverpc.GetInfoResponse, error) {

	if f.getInfoResp == nil && f.getInfoErr == nil {
		return &waverpc.GetInfoResponse{
			WalletState: waverpc.WalletState_WALLET_STATE_READY,
		}, nil
	}

	return f.getInfoResp, f.getInfoErr
}

func (f *fakeRPCServer) EstimateFee(_ context.Context,
	req *waverpc.EstimateFeeRequest) (*waverpc.EstimateFeeResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.estimateFeeCalls++
	f.estimateFeeLastReq = req

	return f.estimateFeeResp, f.estimateFeeErr
}

func (f *fakeRPCServer) GetBalance(_ context.Context,
	_ *waverpc.GetBalanceRequest) (*waverpc.GetBalanceResponse, error) {

	return f.getBalanceResp, f.getBalanceErr
}

func (f *fakeRPCServer) NewAddress(_ context.Context,
	_ *waverpc.NewAddressRequest) (*waverpc.NewAddressResponse, error) {

	return f.newAddressResp, f.newAddressErr
}

func (f *fakeRPCServer) NewWalletAddress(_ context.Context) (string, error) {
	return f.newWalletAddressResp, f.newWalletAddressErr
}

func (f *fakeRPCServer) ListWalletUnspent(_ context.Context, _ int32, _ int32) (
	[]*wallet.Utxo, error) {

	return f.listWalletUnspent, f.listWalletUnspentErr
}

func (f *fakeRPCServer) ListActiveBoardingAddresses(_ context.Context) (
	[]string, error) {

	return f.activeBoardingAddrs, f.activeBoardingErr
}

func (f *fakeRPCServer) ListUnconfirmedBoardingUTXOs(_ context.Context) (
	[]*wallet.Utxo, error) {

	return f.listWalletUnspent, f.listWalletUnspentErr
}

func (f *fakeRPCServer) GenSeed(_ context.Context,
	req *waverpc.GenSeedRequest) (*waverpc.GenSeedResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.genSeedCalls++
	f.genSeedLastReq = req

	return f.genSeedResp, f.genSeedErr
}

func (f *fakeRPCServer) InitWallet(_ context.Context,
	req *waverpc.InitWalletRequest) (*waverpc.InitWalletResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.initWalletCalls++
	f.initWalletLast = req

	return f.initWalletResp, f.initWalletErr
}

func (f *fakeRPCServer) UnlockWallet(_ context.Context,
	req *waverpc.UnlockWalletRequest) (*waverpc.UnlockWalletResponse,
	error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.unlockWalletCalls++
	f.unlockWalletLast = req

	return f.unlockWalletResp, f.unlockWalletErr
}

func (f *fakeRPCServer) Unroll(_ context.Context, req *waverpc.UnrollRequest) (
	*waverpc.UnrollResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.unrollCalls++
	f.unrollLast = req

	return f.unrollResp, f.unrollErr
}

func (f *fakeRPCServer) GetUnrollStatus(_ context.Context,
	req *waverpc.GetUnrollStatusRequest) (*waverpc.GetUnrollStatusResponse,
	error) {

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
	*waved.ExitSummaryResult, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.exitSummaryCalls++

	return f.exitSummaryResp, f.exitSummaryErr
}

func (f *fakeRPCServer) GetExitPlan(_ context.Context,
	req *waved.ExitPlanRequest) (*waved.ExitPlanResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.exitPlanCalls++
	f.exitPlanLast = req

	return f.exitPlanResp, f.exitPlanErr
}

func (f *fakeRPCServer) SweepWallet(_ context.Context,
	req *waved.SweepWalletRequest) (*waved.SweepWalletResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.sweepWalletCalls++
	f.sweepWalletLast = req

	return f.sweepWalletResp, f.sweepWalletErr
}

func (f *fakeRPCServer) JoinNextRound(_ context.Context,
	_ *waverpc.JoinNextRoundRequest) (*waverpc.JoinNextRoundResponse,
	error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.joinNextRoundCalls++

	return f.joinNextRoundResp, f.joinNextRoundErr
}

func (f *fakeRPCServer) SendOnChain(_ context.Context,
	req *waverpc.SendOnChainRequest) (*waverpc.SendOnChainResponse, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.sendOnChainCalls++
	f.sendOnChainLastReq = req

	return f.sendOnChainResp, f.sendOnChainErr
}

func (f *fakeRPCServer) SendOOR(_ context.Context,
	req *waverpc.SendOORRequest) (*waverpc.SendOORResponse, error) {

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
