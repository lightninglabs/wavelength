package restclient

import (
	"context"

	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/lightninglabs/darepo-client/swaprpc"
	"google.golang.org/grpc"
)

type (
	swapClientService = swapclientrpc.SwapClientServiceClient

	watchRoundsStream interface {
		grpc.ServerStreamingClient[daemonrpc.WatchRoundsResponse]
	}
	watchRoundsREST = StreamClient[daemonrpc.WatchRoundsResponse]

	subscribeSwapsStream interface {
		grpc.ServerStreamingClient[swapclientrpc.SubscribeSwapsResponse]
	}
	subscribeSwapsREST = StreamClient[swapclientrpc.SubscribeSwapsResponse]

	walletEntryStream interface {
		grpc.ServerStreamingClient[walletrpc.WalletEntry]
	}
	walletEntryREST = StreamClient[walletrpc.WalletEntry]
)

var (
	_ arkrpc.ArkServiceClient        = (*ArkServiceClient)(nil)
	_ daemonrpc.DaemonServiceClient  = (*DaemonServiceClient)(nil)
	_ mailboxpb.MailboxServiceClient = (*MailboxServiceClient)(nil)
	_ swapClientService              = (*SwapClientServiceClient)(nil)
	_ swaprpc.SwapServiceClient      = (*SwapServiceClient)(nil)
	_ walletrpc.WalletServiceClient  = (*WalletServiceClient)(nil)
	_ watchRoundsStream              = (*watchRoundsREST)(nil)
	_ subscribeSwapsStream           = (*subscribeSwapsREST)(nil)
	_ walletEntryStream              = (*walletEntryREST)(nil)
)

// NewArkServiceClient creates an ArkService REST client.
func NewArkServiceClient(addr string, opts ...Option) arkrpc.ArkServiceClient {
	return NewArkServiceClientFromClient(New(addr, opts...))
}

// NewArkServiceClientFromClient creates an ArkService REST client from an
// existing shared REST transport.
func NewArkServiceClientFromClient(c *Client) arkrpc.ArkServiceClient {
	return &ArkServiceClient{client: c}
}

// ArkServiceClient implements arkrpc.ArkServiceClient over grpc-gateway.
type ArkServiceClient struct {
	client *Client
}

// GetInfo returns basic information about the Ark server.
func (c *ArkServiceClient) GetInfo(ctx context.Context,
	in *arkrpc.GetInfoRequest, _ ...grpc.CallOption) (
	*arkrpc.GetInfoResponse, error) {

	out := new(arkrpc.GetInfoResponse)
	err := c.client.Post(ctx, "/v1/ark/get-info", in, out)

	return out, err
}

// EstimateFee returns a fee breakdown for a given VTXO amount.
func (c *ArkServiceClient) EstimateFee(ctx context.Context,
	in *arkrpc.EstimateFeeRequest, _ ...grpc.CallOption) (
	*arkrpc.EstimateFeeResponse, error) {

	out := new(arkrpc.EstimateFeeResponse)
	err := c.client.Post(ctx, "/v1/ark/estimate-fee", in, out)

	return out, err
}

// NewMailboxServiceClient creates a MailboxService REST client.
func NewMailboxServiceClient(addr string,
	opts ...Option) mailboxpb.MailboxServiceClient {

	return NewMailboxServiceClientFromClient(New(addr, opts...))
}

// NewMailboxServiceClientFromClient creates a MailboxService REST client from
// an existing shared REST transport.
func NewMailboxServiceClientFromClient(
	c *Client) mailboxpb.MailboxServiceClient {

	return &MailboxServiceClient{client: c}
}

// MailboxServiceClient implements mailboxpb.MailboxServiceClient over
// grpc-gateway.
type MailboxServiceClient struct {
	client *Client
}

// Send submits one mailbox envelope.
func (c *MailboxServiceClient) Send(ctx context.Context,
	in *mailboxpb.SendRequest, _ ...grpc.CallOption) (
	*mailboxpb.SendResponse, error) {

	out := new(mailboxpb.SendResponse)
	err := c.client.Post(ctx, "/v1/mailbox/send", in, out)

	return out, err
}

// Pull fetches mailbox envelopes for one mailbox id.
func (c *MailboxServiceClient) Pull(ctx context.Context,
	in *mailboxpb.PullRequest, _ ...grpc.CallOption) (
	*mailboxpb.PullResponse, error) {

	out := new(mailboxpb.PullResponse)
	err := c.client.Post(ctx, "/v1/mailbox/pull", in, out)

	return out, err
}

// AckUpTo acknowledges mailbox envelopes up to one cursor.
func (c *MailboxServiceClient) AckUpTo(ctx context.Context,
	in *mailboxpb.AckUpToRequest, _ ...grpc.CallOption) (
	*mailboxpb.AckUpToResponse, error) {

	out := new(mailboxpb.AckUpToResponse)
	err := c.client.Post(ctx, "/v1/mailbox/ack-up-to", in, out)

	return out, err
}

// NewSwapServiceClient creates a SwapService REST client.
func NewSwapServiceClient(addr string,
	opts ...Option) swaprpc.SwapServiceClient {

	return NewSwapServiceClientFromClient(New(addr, opts...))
}

// NewSwapServiceClientFromClient creates a SwapService REST client from an
// existing shared REST transport.
func NewSwapServiceClientFromClient(c *Client) swaprpc.SwapServiceClient {
	return &SwapServiceClient{client: c}
}

// SwapServiceClient implements swaprpc.SwapServiceClient over grpc-gateway.
type SwapServiceClient struct {
	client *Client
}

// RequestChannelId asks the swap server to allocate a channel id.
func (c *SwapServiceClient) RequestChannelId(ctx context.Context,
	in *swaprpc.RequestChannelIdRequest, _ ...grpc.CallOption) (
	*swaprpc.RequestChannelIdResponse, error) {

	out := new(swaprpc.RequestChannelIdResponse)
	err := c.client.Post(ctx, "/v1/swap/request-channel-id", in, out)

	return out, err
}

// CreateInSwap starts one Ark-to-Lightning swap with the swap server.
func (c *SwapServiceClient) CreateInSwap(ctx context.Context,
	in *swaprpc.CreateInSwapRequest, _ ...grpc.CallOption) (
	*swaprpc.CreateInSwapResponse, error) {

	out := new(swaprpc.CreateInSwapResponse)
	err := c.client.Post(ctx, "/v1/swap/create-in-swap", in, out)

	return out, err
}

// NewDaemonServiceClient creates a DaemonService REST client.
func NewDaemonServiceClient(addr string,
	opts ...Option) daemonrpc.DaemonServiceClient {

	return NewDaemonServiceClientFromClient(New(addr, opts...))
}

// NewDaemonServiceClientFromClient creates a DaemonService REST client from an
// existing shared REST transport.
func NewDaemonServiceClientFromClient(c *Client) daemonrpc.DaemonServiceClient {
	return &DaemonServiceClient{client: c}
}

// DaemonServiceClient implements daemonrpc.DaemonServiceClient over
// grpc-gateway.
type DaemonServiceClient struct {
	client *Client
}

// GetInfo returns basic daemon status.
func (c *DaemonServiceClient) GetInfo(ctx context.Context,
	in *daemonrpc.GetInfoRequest, _ ...grpc.CallOption) (
	*daemonrpc.GetInfoResponse, error) {

	out := new(daemonrpc.GetInfoResponse)
	err := c.client.Post(ctx, "/v1/daemon/get-info", in, out)

	return out, err
}

// GenSeed generates a new wallet seed.
func (c *DaemonServiceClient) GenSeed(ctx context.Context,
	in *daemonrpc.GenSeedRequest, _ ...grpc.CallOption) (
	*daemonrpc.GenSeedResponse, error) {

	out := new(daemonrpc.GenSeedResponse)
	err := c.client.Post(ctx, "/v1/daemon/gen-seed", in, out)

	return out, err
}

// InitWallet initializes the daemon wallet.
func (c *DaemonServiceClient) InitWallet(ctx context.Context,
	in *daemonrpc.InitWalletRequest, _ ...grpc.CallOption) (
	*daemonrpc.InitWalletResponse, error) {

	out := new(daemonrpc.InitWalletResponse)
	err := c.client.Post(ctx, "/v1/daemon/init-wallet", in, out)

	return out, err
}

// UnlockWallet unlocks the daemon wallet.
func (c *DaemonServiceClient) UnlockWallet(ctx context.Context,
	in *daemonrpc.UnlockWalletRequest, _ ...grpc.CallOption) (
	*daemonrpc.UnlockWalletResponse, error) {

	out := new(daemonrpc.UnlockWalletResponse)
	err := c.client.Post(ctx, "/v1/daemon/unlock-wallet", in, out)

	return out, err
}

// GetBalance returns the daemon wallet balance.
func (c *DaemonServiceClient) GetBalance(ctx context.Context,
	in *daemonrpc.GetBalanceRequest, _ ...grpc.CallOption) (
	*daemonrpc.GetBalanceResponse, error) {

	out := new(daemonrpc.GetBalanceResponse)
	err := c.client.Post(ctx, "/v1/daemon/get-balance", in, out)

	return out, err
}

// ListVTXOs lists daemon wallet VTXOs.
func (c *DaemonServiceClient) ListVTXOs(ctx context.Context,
	in *daemonrpc.ListVTXOsRequest, _ ...grpc.CallOption) (
	*daemonrpc.ListVTXOsResponse, error) {

	out := new(daemonrpc.ListVTXOsResponse)
	err := c.client.Post(ctx, "/v1/daemon/list-vtxos", in, out)

	return out, err
}

// NewAddress returns a new boarding address.
func (c *DaemonServiceClient) NewAddress(ctx context.Context,
	in *daemonrpc.NewAddressRequest, _ ...grpc.CallOption) (
	*daemonrpc.NewAddressResponse, error) {

	out := new(daemonrpc.NewAddressResponse)
	err := c.client.Post(ctx, "/v1/daemon/new-address", in, out)

	return out, err
}

// NewReceiveScript returns a new Ark receive script.
func (c *DaemonServiceClient) NewReceiveScript(ctx context.Context,
	in *daemonrpc.NewReceiveScriptRequest, _ ...grpc.CallOption) (
	*daemonrpc.NewReceiveScriptResponse, error) {

	out := new(daemonrpc.NewReceiveScriptResponse)
	err := c.client.Post(ctx, "/v1/daemon/new-receive-script", in, out)

	return out, err
}

// ReceiveAuthKey returns the receive auth public key for one payment hash.
func (c *DaemonServiceClient) ReceiveAuthKey(ctx context.Context,
	in *daemonrpc.ReceiveAuthKeyRequest, _ ...grpc.CallOption) (
	*daemonrpc.ReceiveAuthKeyResponse, error) {

	out := new(daemonrpc.ReceiveAuthKeyResponse)
	err := c.client.Post(ctx, "/v1/daemon/receive-auth-key", in, out)

	return out, err
}

// SignReceiveAuthMessage signs a receive auth message.
func (c *DaemonServiceClient) SignReceiveAuthMessage(ctx context.Context,
	in *daemonrpc.SignReceiveAuthMessageRequest, _ ...grpc.CallOption) (
	*daemonrpc.SignReceiveAuthMessageResponse, error) {

	out := new(daemonrpc.SignReceiveAuthMessageResponse)
	err := c.client.Post(
		ctx, "/v1/daemon/sign-receive-auth-message", in, out,
	)

	return out, err
}

// SignReceiveAuthMessageCompact signs a compact receive auth message.
func (c *DaemonServiceClient) SignReceiveAuthMessageCompact(ctx context.Context,
	in *daemonrpc.SignReceiveAuthMessageCompactRequest,
	_ ...grpc.CallOption) (*daemonrpc.SignReceiveAuthMessageCompactResponse,
	error) {

	out := new(daemonrpc.SignReceiveAuthMessageCompactResponse)
	err := c.client.Post(
		ctx, "/v1/daemon/sign-receive-auth-message-compact", in, out,
	)

	return out, err
}

// ReceiveAuthECDH derives a receive auth ECDH secret.
func (c *DaemonServiceClient) ReceiveAuthECDH(ctx context.Context,
	in *daemonrpc.ReceiveAuthECDHRequest, _ ...grpc.CallOption) (
	*daemonrpc.ReceiveAuthECDHResponse, error) {

	out := new(daemonrpc.ReceiveAuthECDHResponse)
	err := c.client.Post(ctx, "/v1/daemon/receive-auth-ecdh", in, out)

	return out, err
}

// GetIndexedVTXOByPkScript looks up one indexed VTXO by pkScript.
func (c *DaemonServiceClient) GetIndexedVTXOByPkScript(ctx context.Context,
	in *daemonrpc.GetIndexedVTXOByPkScriptRequest, _ ...grpc.CallOption) (
	*daemonrpc.GetIndexedVTXOByPkScriptResponse, error) {

	out := new(daemonrpc.GetIndexedVTXOByPkScriptResponse)
	err := c.client.Post(
		ctx, "/v1/daemon/get-indexed-vtxo-by-pk-script", in, out,
	)

	return out, err
}

// GetIndexedOORSessionByTxid looks up one indexed OOR session by txid.
func (c *DaemonServiceClient) GetIndexedOORSessionByTxid(ctx context.Context,
	in *daemonrpc.GetIndexedOORSessionByTxidRequest, _ ...grpc.CallOption) (
	*daemonrpc.GetIndexedOORSessionByTxidResponse, error) {

	out := new(daemonrpc.GetIndexedOORSessionByTxidResponse)
	err := c.client.Post(
		ctx, "/v1/daemon/get-indexed-oor-session-by-txid", in, out,
	)

	return out, err
}

// SendVTXO initiates an in-round VTXO transfer.
func (c *DaemonServiceClient) SendVTXO(ctx context.Context,
	in *daemonrpc.SendVTXORequest, _ ...grpc.CallOption) (
	*daemonrpc.SendVTXOResponse, error) {

	out := new(daemonrpc.SendVTXOResponse)
	err := c.client.Post(ctx, "/v1/daemon/send-vtxo", in, out)

	return out, err
}

// SendOOR initiates an out-of-round VTXO transfer.
func (c *DaemonServiceClient) SendOOR(ctx context.Context,
	in *daemonrpc.SendOORRequest, _ ...grpc.CallOption) (
	*daemonrpc.SendOORResponse, error) {

	out := new(daemonrpc.SendOORResponse)
	err := c.client.Post(ctx, "/v1/daemon/send-oor", in, out)

	return out, err
}

// RefreshVTXOs queues VTXOs for refresh.
func (c *DaemonServiceClient) RefreshVTXOs(ctx context.Context,
	in *daemonrpc.RefreshVTXOsRequest, _ ...grpc.CallOption) (
	*daemonrpc.RefreshVTXOsResponse, error) {

	out := new(daemonrpc.RefreshVTXOsResponse)
	err := c.client.Post(ctx, "/v1/daemon/refresh-vtxos", in, out)

	return out, err
}

// LeaveVTXOs queues VTXOs for cooperative exit.
func (c *DaemonServiceClient) LeaveVTXOs(ctx context.Context,
	in *daemonrpc.LeaveVTXOsRequest, _ ...grpc.CallOption) (
	*daemonrpc.LeaveVTXOsResponse, error) {

	out := new(daemonrpc.LeaveVTXOsResponse)
	err := c.client.Post(ctx, "/v1/daemon/leave-vtxos", in, out)

	return out, err
}

// Board queues confirmed boarding UTXOs for the next round.
func (c *DaemonServiceClient) Board(ctx context.Context,
	in *daemonrpc.BoardRequest, _ ...grpc.CallOption) (
	*daemonrpc.BoardResponse, error) {

	out := new(daemonrpc.BoardResponse)
	err := c.client.Post(ctx, "/v1/daemon/board", in, out)

	return out, err
}

// JoinNextRound commits queued round intents and joins the next round.
func (c *DaemonServiceClient) JoinNextRound(ctx context.Context,
	in *daemonrpc.JoinNextRoundRequest, _ ...grpc.CallOption) (
	*daemonrpc.JoinNextRoundResponse, error) {

	out := new(daemonrpc.JoinNextRoundResponse)
	err := c.client.Post(
		ctx, "/v1/daemon/join-next-round", in, out,
	)

	return out, err
}

// SweepBoardingUTXOs sweeps mature boarding UTXOs.
func (c *DaemonServiceClient) SweepBoardingUTXOs(ctx context.Context,
	in *daemonrpc.SweepBoardingUTXOsRequest, _ ...grpc.CallOption) (
	*daemonrpc.SweepBoardingUTXOsResponse, error) {

	out := new(daemonrpc.SweepBoardingUTXOsResponse)
	err := c.client.Post(ctx, "/v1/daemon/sweep-boarding-utxos", in, out)

	return out, err
}

// ListBoardingSweeps lists boarding sweep jobs.
func (c *DaemonServiceClient) ListBoardingSweeps(ctx context.Context,
	in *daemonrpc.ListBoardingSweepsRequest, _ ...grpc.CallOption) (
	*daemonrpc.ListBoardingSweepsResponse, error) {

	out := new(daemonrpc.ListBoardingSweepsResponse)
	err := c.client.Post(ctx, "/v1/daemon/list-boarding-sweeps", in, out)

	return out, err
}

// ListRounds lists client-visible rounds.
func (c *DaemonServiceClient) ListRounds(ctx context.Context,
	in *daemonrpc.ListRoundsRequest, _ ...grpc.CallOption) (
	*daemonrpc.ListRoundsResponse, error) {

	out := new(daemonrpc.ListRoundsResponse)
	err := c.client.Post(ctx, "/v1/daemon/list-rounds", in, out)

	return out, err
}

// GetRound returns one client-visible round.
func (c *DaemonServiceClient) GetRound(ctx context.Context,
	in *daemonrpc.GetRoundRequest, _ ...grpc.CallOption) (
	*daemonrpc.GetRoundResponse, error) {

	out := new(daemonrpc.GetRoundResponse)
	err := c.client.Post(ctx, "/v1/daemon/get-round", in, out)

	return out, err
}

// WatchRounds streams round updates from the daemon.
func (c *DaemonServiceClient) WatchRounds(ctx context.Context,
	in *daemonrpc.WatchRoundsRequest, _ ...grpc.CallOption) (
	grpc.ServerStreamingClient[daemonrpc.WatchRoundsResponse], error) {

	resp, err := c.client.Stream( //nolint:bodyclose // Stream owns body.
		ctx, "/v1/daemon/watch-rounds", in,
	)
	if err != nil {
		return nil, err
	}

	return NewStreamClient[daemonrpc.WatchRoundsResponse](
		resp, "WatchRounds", func() *daemonrpc.WatchRoundsResponse {
			return new(daemonrpc.WatchRoundsResponse)
		},
	), nil
}

// ListOORSessions lists known OOR sessions.
func (c *DaemonServiceClient) ListOORSessions(ctx context.Context,
	in *daemonrpc.ListOORSessionsRequest, _ ...grpc.CallOption) (
	*daemonrpc.ListOORSessionsResponse, error) {

	out := new(daemonrpc.ListOORSessionsResponse)
	err := c.client.Post(ctx, "/v1/daemon/list-oor-sessions", in, out)

	return out, err
}

// GetOORSession returns one OOR session.
func (c *DaemonServiceClient) GetOORSession(ctx context.Context,
	in *daemonrpc.GetOORSessionRequest, _ ...grpc.CallOption) (
	*daemonrpc.GetOORSessionResponse, error) {

	out := new(daemonrpc.GetOORSessionResponse)
	err := c.client.Post(ctx, "/v1/daemon/get-oor-session", in, out)

	return out, err
}

// EstimateFee estimates an on-chain fee.
func (c *DaemonServiceClient) EstimateFee(ctx context.Context,
	in *daemonrpc.EstimateFeeRequest, _ ...grpc.CallOption) (
	*daemonrpc.EstimateFeeResponse, error) {

	out := new(daemonrpc.EstimateFeeResponse)
	err := c.client.Post(ctx, "/v1/daemon/estimate-fee", in, out)

	return out, err
}

// GetFeeHistory returns recent fee estimates.
func (c *DaemonServiceClient) GetFeeHistory(ctx context.Context,
	in *daemonrpc.GetFeeHistoryRequest, _ ...grpc.CallOption) (
	*daemonrpc.GetFeeHistoryResponse, error) {

	out := new(daemonrpc.GetFeeHistoryResponse)
	err := c.client.Post(ctx, "/v1/daemon/get-fee-history", in, out)

	return out, err
}

// ListTransactions lists daemon wallet transactions.
func (c *DaemonServiceClient) ListTransactions(ctx context.Context,
	in *daemonrpc.ListTransactionsRequest, _ ...grpc.CallOption) (
	*daemonrpc.ListTransactionsResponse, error) {

	out := new(daemonrpc.ListTransactionsResponse)
	err := c.client.Post(ctx, "/v1/daemon/list-transactions", in, out)

	return out, err
}

// Unroll starts a unilateral exit job.
func (c *DaemonServiceClient) Unroll(ctx context.Context,
	in *daemonrpc.UnrollRequest, _ ...grpc.CallOption) (
	*daemonrpc.UnrollResponse, error) {

	out := new(daemonrpc.UnrollResponse)
	err := c.client.Post(ctx, "/v1/daemon/unroll", in, out)

	return out, err
}

// GetUnrollStatus returns one unilateral exit job status.
func (c *DaemonServiceClient) GetUnrollStatus(ctx context.Context,
	in *daemonrpc.GetUnrollStatusRequest, _ ...grpc.CallOption) (
	*daemonrpc.GetUnrollStatusResponse, error) {

	out := new(daemonrpc.GetUnrollStatusResponse)
	err := c.client.Post(ctx, "/v1/daemon/get-unroll-status", in, out)

	return out, err
}

// NewSwapClientServiceClient creates a SwapClientService REST client.
func NewSwapClientServiceClient(addr string,
	opts ...Option) swapclientrpc.SwapClientServiceClient {

	return NewSwapClientServiceClientFromClient(New(addr, opts...))
}

// NewSwapClientServiceClientFromClient creates a SwapClientService REST client
// from an existing shared REST transport.
func NewSwapClientServiceClientFromClient(
	c *Client) swapclientrpc.SwapClientServiceClient {

	return &SwapClientServiceClient{client: c}
}

// SwapClientServiceClient implements swapclientrpc.SwapClientServiceClient over
// grpc-gateway.
type SwapClientServiceClient struct {
	client *Client
}

// StartPay starts a daemon-owned pay swap.
func (c *SwapClientServiceClient) StartPay(ctx context.Context,
	in *swapclientrpc.StartPayRequest, _ ...grpc.CallOption) (
	*swapclientrpc.StartPayResponse, error) {

	out := new(swapclientrpc.StartPayResponse)
	err := c.client.Post(ctx, "/v1/swapclient/start-pay", in, out)

	return out, err
}

// StartReceive starts a daemon-owned receive swap.
func (c *SwapClientServiceClient) StartReceive(ctx context.Context,
	in *swapclientrpc.StartReceiveRequest, _ ...grpc.CallOption) (
	*swapclientrpc.StartReceiveResponse, error) {

	out := new(swapclientrpc.StartReceiveResponse)
	err := c.client.Post(ctx, "/v1/swapclient/start-receive", in, out)

	return out, err
}

// ResumeSwap resumes one persisted swap.
func (c *SwapClientServiceClient) ResumeSwap(ctx context.Context,
	in *swapclientrpc.ResumeSwapRequest, _ ...grpc.CallOption) (
	*swapclientrpc.ResumeSwapResponse, error) {

	out := new(swapclientrpc.ResumeSwapResponse)
	err := c.client.Post(ctx, "/v1/swapclient/resume-swap", in, out)

	return out, err
}

// ListSwaps lists persisted swaps.
func (c *SwapClientServiceClient) ListSwaps(ctx context.Context,
	in *swapclientrpc.ListSwapsRequest, _ ...grpc.CallOption) (
	*swapclientrpc.ListSwapsResponse, error) {

	out := new(swapclientrpc.ListSwapsResponse)
	err := c.client.Post(ctx, "/v1/swapclient/list-swaps", in, out)

	return out, err
}

// GetSwap returns one persisted swap.
func (c *SwapClientServiceClient) GetSwap(ctx context.Context,
	in *swapclientrpc.GetSwapRequest, _ ...grpc.CallOption) (
	*swapclientrpc.GetSwapResponse, error) {

	out := new(swapclientrpc.GetSwapResponse)
	err := c.client.Post(ctx, "/v1/swapclient/get-swap", in, out)

	return out, err
}

// SubscribeSwaps streams daemon-owned swap updates.
func (c *SwapClientServiceClient) SubscribeSwaps(ctx context.Context,
	in *swapclientrpc.SubscribeSwapsRequest, _ ...grpc.CallOption) (
	grpc.ServerStreamingClient[swapclientrpc.SubscribeSwapsResponse],
	error) {

	resp, err := c.client.Stream( //nolint:bodyclose // Stream owns body.
		ctx, "/v1/swapclient/subscribe-swaps", in,
	)
	if err != nil {
		return nil, err
	}

	return NewStreamClient[swapclientrpc.SubscribeSwapsResponse](
		resp, "SubscribeSwaps",
		func() *swapclientrpc.SubscribeSwapsResponse {
			return new(swapclientrpc.SubscribeSwapsResponse)
		},
	), nil
}

// NewWalletServiceClient creates a WalletService REST client.
func NewWalletServiceClient(addr string,
	opts ...Option) walletrpc.WalletServiceClient {

	return NewWalletServiceClientFromClient(New(addr, opts...))
}

// NewWalletServiceClientFromClient creates a WalletService REST client from an
// existing shared REST transport.
func NewWalletServiceClientFromClient(c *Client) walletrpc.WalletServiceClient {
	return &WalletServiceClient{client: c}
}

// WalletServiceClient implements walletrpc.WalletServiceClient over
// grpc-gateway.
type WalletServiceClient struct {
	client *Client
}

// Create initializes a wallet through WalletService.
func (c *WalletServiceClient) Create(ctx context.Context,
	in *walletrpc.CreateRequest, _ ...grpc.CallOption) (
	*walletrpc.CreateResponse, error) {

	out := new(walletrpc.CreateResponse)
	err := c.client.Post(ctx, "/v1/wallet/create", in, out)

	return out, err
}

// Unlock unlocks a wallet through WalletService.
func (c *WalletServiceClient) Unlock(ctx context.Context,
	in *walletrpc.UnlockRequest, _ ...grpc.CallOption) (
	*walletrpc.UnlockResponse, error) {

	out := new(walletrpc.UnlockResponse)
	err := c.client.Post(ctx, "/v1/wallet/unlock", in, out)

	return out, err
}

// Send dispatches a wallet send.
func (c *WalletServiceClient) Send(ctx context.Context,
	in *walletrpc.SendRequest, _ ...grpc.CallOption) (
	*walletrpc.SendResponse, error) {

	out := new(walletrpc.SendResponse)
	err := c.client.Post(ctx, "/v1/wallet/send", in, out)

	return out, err
}

// Recv creates a wallet receive invoice.
func (c *WalletServiceClient) Recv(ctx context.Context,
	in *walletrpc.RecvRequest, _ ...grpc.CallOption) (
	*walletrpc.RecvResponse, error) {

	out := new(walletrpc.RecvResponse)
	err := c.client.Post(ctx, "/v1/wallet/recv", in, out)

	return out, err
}

// List returns a wallet list view.
func (c *WalletServiceClient) List(ctx context.Context,
	in *walletrpc.ListRequest, _ ...grpc.CallOption) (
	*walletrpc.ListResponse, error) {

	out := new(walletrpc.ListResponse)
	err := c.client.Post(ctx, "/v1/wallet/list", in, out)

	return out, err
}

// Deposit returns a wallet deposit address.
func (c *WalletServiceClient) Deposit(ctx context.Context,
	in *walletrpc.DepositRequest, _ ...grpc.CallOption) (
	*walletrpc.DepositResponse, error) {

	out := new(walletrpc.DepositResponse)
	err := c.client.Post(ctx, "/v1/wallet/deposit", in, out)

	return out, err
}

// Balance returns the wallet balance summary.
func (c *WalletServiceClient) Balance(ctx context.Context,
	in *walletrpc.BalanceRequest, _ ...grpc.CallOption) (
	*walletrpc.BalanceResponse, error) {

	out := new(walletrpc.BalanceResponse)
	err := c.client.Post(ctx, "/v1/wallet/balance", in, out)

	return out, err
}

// Status returns the wallet readiness summary.
func (c *WalletServiceClient) Status(ctx context.Context,
	in *walletrpc.StatusRequest, _ ...grpc.CallOption) (
	*walletrpc.StatusResponse, error) {

	out := new(walletrpc.StatusResponse)
	err := c.client.Post(ctx, "/v1/wallet/status", in, out)

	return out, err
}

// Exit starts a wallet unilateral exit.
func (c *WalletServiceClient) Exit(ctx context.Context,
	in *walletrpc.ExitRequest, _ ...grpc.CallOption) (
	*walletrpc.ExitResponse, error) {

	out := new(walletrpc.ExitResponse)
	err := c.client.Post(ctx, "/v1/wallet/exit", in, out)

	return out, err
}

// ExitStatus returns one wallet unilateral exit status.
func (c *WalletServiceClient) ExitStatus(ctx context.Context,
	in *walletrpc.ExitStatusRequest, _ ...grpc.CallOption) (
	*walletrpc.ExitStatusResponse, error) {

	out := new(walletrpc.ExitStatusResponse)
	err := c.client.Post(ctx, "/v1/wallet/exit-status", in, out)

	return out, err
}

// SubscribeWallet streams wallet updates.
func (c *WalletServiceClient) SubscribeWallet(ctx context.Context,
	in *walletrpc.SubscribeWalletRequest, _ ...grpc.CallOption) (
	grpc.ServerStreamingClient[walletrpc.WalletEntry], error) {

	resp, err := c.client.Stream( //nolint:bodyclose // Stream owns body.
		ctx, "/v1/wallet/subscribe", in,
	)
	if err != nil {
		return nil, err
	}

	return NewStreamClient[walletrpc.WalletEntry](
		resp, "SubscribeWallet", func() *walletrpc.WalletEntry {
			return new(walletrpc.WalletEntry)
		},
	), nil
}
