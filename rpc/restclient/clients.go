package restclient

import (
	"context"

	"github.com/lightninglabs/wavelength/arkrpc"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc"
)

type (
	swapClientService = swapclientrpc.SwapClientServiceClient

	// subscribeWalletResponse aliases the generated response type so the
	// stream adapter declarations below stay under the line-length
	// limit with the longer wavewalletrpc package name.
	subscribeWalletResponse = wavewalletrpc.SubscribeWalletResponse

	watchRoundsStream interface {
		grpc.ServerStreamingClient[waverpc.WatchRoundsResponse]
	}
	watchRoundsREST = StreamClient[waverpc.WatchRoundsResponse]

	subscribeSwapsStream interface {
		grpc.ServerStreamingClient[swapclientrpc.SubscribeSwapsResponse]
	}
	subscribeSwapsREST = StreamClient[swapclientrpc.SubscribeSwapsResponse]

	subscribeWalletStream interface {
		grpc.ServerStreamingClient[subscribeWalletResponse]
	}
	subscribeWalletREST = StreamClient[subscribeWalletResponse]
)

var (
	_ arkrpc.ArkServiceClient           = (*ArkServiceClient)(nil)
	_ waverpc.DaemonServiceClient       = (*DaemonServiceClient)(nil)
	_ mailboxpb.MailboxServiceClient    = (*MailboxServiceClient)(nil)
	_ swapClientService                 = (*SwapClientServiceClient)(nil)
	_ swaprpc.SwapServiceClient         = (*SwapServiceClient)(nil)
	_ wavewalletrpc.WalletServiceClient = (*WalletServiceClient)(nil)
	_ watchRoundsStream                 = (*watchRoundsREST)(nil)
	_ subscribeSwapsStream              = (*subscribeSwapsREST)(nil)
	_ subscribeWalletStream             = (*subscribeWalletREST)(nil)
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

// RegisterTaprootAssetVTXO admits a confirmed direct-on-chain asset VTXO.
func (c *ArkServiceClient) RegisterTaprootAssetVTXO(ctx context.Context,
	in *arkrpc.RegisterTaprootAssetVTXORequest, _ ...grpc.CallOption) (
	*arkrpc.RegisterTaprootAssetVTXOResponse, error) {

	out := new(arkrpc.RegisterTaprootAssetVTXOResponse)
	err := c.client.Post(
		ctx, "/v1/ark/register-taproot-asset-vtxo", in, out,
	)

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

// QuoteInSwap previews one Ark-to-Lightning swap with the swap server.
func (c *SwapServiceClient) QuoteInSwap(ctx context.Context,
	in *swaprpc.QuoteInSwapRequest, _ ...grpc.CallOption) (
	*swaprpc.QuoteInSwapResponse, error) {

	out := new(swaprpc.QuoteInSwapResponse)
	err := c.client.Post(ctx, "/v1/swap/quote-in-swap", in, out)

	return out, err
}

// CreateCredit creates a server-owned credit funding operation.
func (c *SwapServiceClient) CreateCredit(ctx context.Context,
	in *swaprpc.CreateCreditRequest, _ ...grpc.CallOption) (
	*swaprpc.CreateCreditResponse, error) {

	out := new(swaprpc.CreateCreditResponse)
	err := c.client.Post(ctx, "/v1/swap/create-credit", in, out)

	return out, err
}

// RedeemCredit sends available credits back to an Ark recipient.
func (c *SwapServiceClient) RedeemCredit(ctx context.Context,
	in *swaprpc.RedeemCreditRequest, _ ...grpc.CallOption) (
	*swaprpc.RedeemCreditResponse, error) {

	out := new(swaprpc.RedeemCreditResponse)
	err := c.client.Post(ctx, "/v1/swap/redeem-credit", in, out)

	return out, err
}

// ListCredits lists server-side credit account state.
func (c *SwapServiceClient) ListCredits(ctx context.Context,
	in *swaprpc.ListCreditsRequest, _ ...grpc.CallOption) (
	*swaprpc.ListCreditsResponse, error) {

	out := new(swaprpc.ListCreditsResponse)
	err := c.client.Post(ctx, "/v1/swap/list-credits", in, out)

	return out, err
}

// AuthorizeInSwapRefund asks the swap server to sign a failed in-swap refund.
func (c *SwapServiceClient) AuthorizeInSwapRefund(ctx context.Context,
	in *swaprpc.AuthorizeInSwapRefundRequest, _ ...grpc.CallOption) (
	*swaprpc.AuthorizeInSwapRefundResponse, error) {

	out := new(swaprpc.AuthorizeInSwapRefundResponse)
	err := c.client.Post(
		ctx, "/v1/swap/authorize-in-swap-refund", in, out,
	)

	return out, err
}

// AcknowledgeOutSwapHtlc tells the swap server an out-swap receiver durably
// accepted the HTLC event.
func (c *SwapServiceClient) AcknowledgeOutSwapHtlc(ctx context.Context,
	in *swaprpc.AcknowledgeOutSwapHtlcRequest, _ ...grpc.CallOption) (
	*swaprpc.AcknowledgeOutSwapHtlcResponse, error) {

	out := new(swaprpc.AcknowledgeOutSwapHtlcResponse)
	err := c.client.Post(
		ctx, "/v1/swap/acknowledge-out-swap-htlc", in, out,
	)

	return out, err
}

// SignInSwapForfeit asks the swap server to sign an in-swap refresh forfeit.
func (c *SwapServiceClient) SignInSwapForfeit(ctx context.Context,
	in *swaprpc.SignInSwapForfeitRequest, _ ...grpc.CallOption) (
	*swaprpc.SignInSwapForfeitResponse, error) {

	out := new(swaprpc.SignInSwapForfeitResponse)
	err := c.client.Post(ctx, "/v1/swap/sign-in-swap-forfeit", in, out)

	return out, err
}

// SubmitOutSwapForfeitSignature submits an out-swap refresh participant
// signature to the swap server.
func (c *SwapServiceClient) SubmitOutSwapForfeitSignature(ctx context.Context,
	in *swaprpc.SubmitOutSwapForfeitSignatureRequest,
	_ ...grpc.CallOption) (*swaprpc.SubmitOutSwapForfeitSignatureResponse,
	error) {

	out := new(swaprpc.SubmitOutSwapForfeitSignatureResponse)
	err := c.client.Post(
		ctx, "/v1/swap/submit-out-swap-forfeit-signature", in, out,
	)

	return out, err
}

// NewDaemonServiceClient creates a DaemonService REST client.
func NewDaemonServiceClient(addr string,
	opts ...Option) waverpc.DaemonServiceClient {

	return NewDaemonServiceClientFromClient(New(addr, opts...))
}

// NewDaemonServiceClientFromClient creates a DaemonService REST client from an
// existing shared REST transport.
func NewDaemonServiceClientFromClient(c *Client) waverpc.DaemonServiceClient {
	return &DaemonServiceClient{client: c}
}

// DaemonServiceClient implements waverpc.DaemonServiceClient over
// grpc-gateway.
type DaemonServiceClient struct {
	client *Client
}

// GetInfo returns basic daemon status.
func (c *DaemonServiceClient) GetInfo(ctx context.Context,
	in *waverpc.GetInfoRequest, _ ...grpc.CallOption) (
	*waverpc.GetInfoResponse, error) {

	out := new(waverpc.GetInfoResponse)
	err := c.client.Post(ctx, "/v1/daemon/get-info", in, out)

	return out, err
}

// GenSeed generates a new wallet seed.
func (c *DaemonServiceClient) GenSeed(ctx context.Context,
	in *waverpc.GenSeedRequest, _ ...grpc.CallOption) (
	*waverpc.GenSeedResponse, error) {

	out := new(waverpc.GenSeedResponse)
	err := c.client.Post(ctx, "/v1/daemon/gen-seed", in, out)

	return out, err
}

// InitWallet initializes the daemon wallet.
func (c *DaemonServiceClient) InitWallet(ctx context.Context,
	in *waverpc.InitWalletRequest, _ ...grpc.CallOption) (
	*waverpc.InitWalletResponse, error) {

	out := new(waverpc.InitWalletResponse)
	err := c.client.Post(ctx, "/v1/daemon/init-wallet", in, out)

	return out, err
}

// UnlockWallet unlocks the daemon wallet.
func (c *DaemonServiceClient) UnlockWallet(ctx context.Context,
	in *waverpc.UnlockWalletRequest, _ ...grpc.CallOption) (
	*waverpc.UnlockWalletResponse, error) {

	out := new(waverpc.UnlockWalletResponse)
	err := c.client.Post(ctx, "/v1/daemon/unlock-wallet", in, out)

	return out, err
}

// GetBalance returns the daemon wallet balance.
func (c *DaemonServiceClient) GetBalance(ctx context.Context,
	in *waverpc.GetBalanceRequest, _ ...grpc.CallOption) (
	*waverpc.GetBalanceResponse, error) {

	out := new(waverpc.GetBalanceResponse)
	err := c.client.Post(ctx, "/v1/daemon/get-balance", in, out)

	return out, err
}

// ListVTXOs lists daemon wallet VTXOs.
func (c *DaemonServiceClient) ListVTXOs(ctx context.Context,
	in *waverpc.ListVTXOsRequest, _ ...grpc.CallOption) (
	*waverpc.ListVTXOsResponse, error) {

	out := new(waverpc.ListVTXOsResponse)
	err := c.client.Post(ctx, "/v1/daemon/list-vtxos", in, out)

	return out, err
}

// NewAddress returns a new boarding address.
func (c *DaemonServiceClient) NewAddress(ctx context.Context,
	in *waverpc.NewAddressRequest, _ ...grpc.CallOption) (
	*waverpc.NewAddressResponse, error) {

	out := new(waverpc.NewAddressResponse)
	err := c.client.Post(ctx, "/v1/daemon/new-address", in, out)

	return out, err
}

// NewReceiveScript returns a new Ark receive script.
func (c *DaemonServiceClient) NewReceiveScript(ctx context.Context,
	in *waverpc.NewReceiveScriptRequest, _ ...grpc.CallOption) (
	*waverpc.NewReceiveScriptResponse, error) {

	out := new(waverpc.NewReceiveScriptResponse)
	err := c.client.Post(ctx, "/v1/daemon/new-receive-script", in, out)

	return out, err
}

// ReceiveAuthKey returns the receive auth public key for one payment hash.
func (c *DaemonServiceClient) ReceiveAuthKey(ctx context.Context,
	in *waverpc.ReceiveAuthKeyRequest, _ ...grpc.CallOption) (
	*waverpc.ReceiveAuthKeyResponse, error) {

	out := new(waverpc.ReceiveAuthKeyResponse)
	err := c.client.Post(ctx, "/v1/daemon/receive-auth-key", in, out)

	return out, err
}

// SignReceiveAuthMessage signs a receive auth message.
func (c *DaemonServiceClient) SignReceiveAuthMessage(ctx context.Context,
	in *waverpc.SignReceiveAuthMessageRequest, _ ...grpc.CallOption) (
	*waverpc.SignReceiveAuthMessageResponse, error) {

	out := new(waverpc.SignReceiveAuthMessageResponse)
	err := c.client.Post(
		ctx, "/v1/daemon/sign-receive-auth-message", in, out,
	)

	return out, err
}

// SignReceiveAuthMessageCompact signs a compact receive auth message.
func (c *DaemonServiceClient) SignReceiveAuthMessageCompact(ctx context.Context,
	in *waverpc.SignReceiveAuthMessageCompactRequest,
	_ ...grpc.CallOption) (*waverpc.SignReceiveAuthMessageCompactResponse,
	error) {

	out := new(waverpc.SignReceiveAuthMessageCompactResponse)
	err := c.client.Post(
		ctx, "/v1/daemon/sign-receive-auth-message-compact", in, out,
	)

	return out, err
}

// ReceiveAuthECDH derives a receive auth ECDH secret.
func (c *DaemonServiceClient) ReceiveAuthECDH(ctx context.Context,
	in *waverpc.ReceiveAuthECDHRequest, _ ...grpc.CallOption) (
	*waverpc.ReceiveAuthECDHResponse, error) {

	out := new(waverpc.ReceiveAuthECDHResponse)
	err := c.client.Post(ctx, "/v1/daemon/receive-auth-ecdh", in, out)

	return out, err
}

// GetIndexedVTXOByPkScript looks up one indexed VTXO by pkScript.
func (c *DaemonServiceClient) GetIndexedVTXOByPkScript(ctx context.Context,
	in *waverpc.GetIndexedVTXOByPkScriptRequest, _ ...grpc.CallOption) (
	*waverpc.GetIndexedVTXOByPkScriptResponse, error) {

	out := new(waverpc.GetIndexedVTXOByPkScriptResponse)
	err := c.client.Post(
		ctx, "/v1/daemon/get-indexed-vtxo-by-pk-script", in, out,
	)

	return out, err
}

// GetVTXOExpiryInfo classifies one VTXO's expiry posture.
func (c *DaemonServiceClient) GetVTXOExpiryInfo(ctx context.Context,
	in *waverpc.GetVTXOExpiryInfoRequest, _ ...grpc.CallOption) (
	*waverpc.GetVTXOExpiryInfoResponse, error) {

	out := new(waverpc.GetVTXOExpiryInfoResponse)
	err := c.client.Post(
		ctx, "/v1/daemon/get-vtxo-expiry-info", in, out,
	)

	return out, err
}

// GetIndexedOORSessionByTxid looks up one indexed OOR session by txid.
func (c *DaemonServiceClient) GetIndexedOORSessionByTxid(ctx context.Context,
	in *waverpc.GetIndexedOORSessionByTxidRequest, _ ...grpc.CallOption) (
	*waverpc.GetIndexedOORSessionByTxidResponse, error) {

	out := new(waverpc.GetIndexedOORSessionByTxidResponse)
	err := c.client.Post(
		ctx, "/v1/daemon/get-indexed-oor-session-by-txid", in, out,
	)

	return out, err
}

// SendVTXO initiates an in-round VTXO transfer.
func (c *DaemonServiceClient) SendVTXO(ctx context.Context,
	in *waverpc.SendVTXORequest, _ ...grpc.CallOption) (
	*waverpc.SendVTXOResponse, error) {

	out := new(waverpc.SendVTXOResponse)
	err := c.client.Post(ctx, "/v1/daemon/send-vtxo", in, out)

	return out, err
}

// SendOOR initiates an out-of-round VTXO transfer.
func (c *DaemonServiceClient) SendOOR(ctx context.Context,
	in *waverpc.SendOORRequest, _ ...grpc.CallOption) (
	*waverpc.SendOORResponse, error) {

	out := new(waverpc.SendOORResponse)
	err := c.client.Post(ctx, "/v1/daemon/send-oor", in, out)

	return out, err
}

// OnboardTaprootAsset moves one isolated asset into a Wavelength VTXO.
func (c *DaemonServiceClient) OnboardTaprootAsset(ctx context.Context,
	in *waverpc.OnboardTaprootAssetRequest, _ ...grpc.CallOption) (
	*waverpc.OnboardTaprootAssetResponse, error) {

	out := new(waverpc.OnboardTaprootAssetResponse)
	err := c.client.Post(
		ctx, "/v1/daemon/onboard-taproot-asset", in, out,
	)

	return out, err
}

// PrepareOOR builds an out-of-round transfer package without submitting it.
func (c *DaemonServiceClient) PrepareOOR(ctx context.Context,
	in *waverpc.PrepareOORRequest, _ ...grpc.CallOption) (
	*waverpc.PrepareOORResponse, error) {

	out := new(waverpc.PrepareOORResponse)
	err := c.client.Post(ctx, "/v1/daemon/prepare-oor", in, out)

	return out, err
}

// SignOORCustomInput signs one prepared custom OOR input.
func (c *DaemonServiceClient) SignOORCustomInput(ctx context.Context,
	in *waverpc.SignOORCustomInputRequest, _ ...grpc.CallOption) (
	*waverpc.SignOORCustomInputResponse, error) {

	out := new(waverpc.SignOORCustomInputResponse)
	err := c.client.Post(ctx, "/v1/daemon/sign-oor-custom-input", in, out)

	return out, err
}

// SignVTXOForfeit signs one exact round forfeit transaction input.
func (c *DaemonServiceClient) SignVTXOForfeit(ctx context.Context,
	in *waverpc.SignVTXOForfeitRequest, _ ...grpc.CallOption) (
	*waverpc.SignVTXOForfeitResponse, error) {

	out := new(waverpc.SignVTXOForfeitResponse)
	err := c.client.Post(ctx, "/v1/daemon/sign-vtxo-forfeit", in, out)

	return out, err
}

// RefreshVTXOs queues VTXOs for refresh.
func (c *DaemonServiceClient) RefreshVTXOs(ctx context.Context,
	in *waverpc.RefreshVTXOsRequest, _ ...grpc.CallOption) (
	*waverpc.RefreshVTXOsResponse, error) {

	out := new(waverpc.RefreshVTXOsResponse)
	err := c.client.Post(ctx, "/v1/daemon/refresh-vtxos", in, out)

	return out, err
}

// RefreshCustomVTXOs queues caller-supplied custom-policy VTXOs for refresh.
func (c *DaemonServiceClient) RefreshCustomVTXOs(ctx context.Context,
	in *waverpc.RefreshCustomVTXOsRequest, _ ...grpc.CallOption) (
	*waverpc.RefreshCustomVTXOsResponse, error) {

	out := new(waverpc.RefreshCustomVTXOsResponse)
	err := c.client.Post(ctx, "/v1/daemon/refresh-custom-vtxos", in, out)

	return out, err
}

// ListPendingForfeitParticipantSignatureRequests lists pending custom-refresh
// participant signature requests.
func (c *DaemonServiceClient) ListPendingForfeitParticipantSignatureRequests(
	ctx context.Context,
	in *waverpc.ListPendingForfeitParticipantSignatureRequestsRequest,
	_ ...grpc.CallOption) (
	*waverpc.ListPendingForfeitParticipantSignatureRequestsResponse,
	error) {

	out := new(
		waverpc.
			ListPendingForfeitParticipantSignatureRequestsResponse,
	)
	path := "/v1/daemon/list-pending-forfeit-participant-" +
		"signature-requests"
	err := c.client.Post(
		ctx, path, in, out,
	)

	return out, err
}

// SubmitForfeitParticipantSignatures supplies participant signatures for one
// pending custom-refresh forfeit signature request.
func (c *DaemonServiceClient) SubmitForfeitParticipantSignatures(
	ctx context.Context,
	in *waverpc.SubmitForfeitParticipantSignaturesRequest,
	_ ...grpc.CallOption) (
	*waverpc.SubmitForfeitParticipantSignaturesResponse, error) {

	out := new(waverpc.SubmitForfeitParticipantSignaturesResponse)
	err := c.client.Post(
		ctx, "/v1/daemon/submit-forfeit-participant-signatures", in,
		out,
	)

	return out, err
}

// LeaveVTXOs queues VTXOs for cooperative exit.
func (c *DaemonServiceClient) LeaveVTXOs(ctx context.Context,
	in *waverpc.LeaveVTXOsRequest, _ ...grpc.CallOption) (
	*waverpc.LeaveVTXOsResponse, error) {

	out := new(waverpc.LeaveVTXOsResponse)
	err := c.client.Post(ctx, "/v1/daemon/leave-vtxos", in, out)

	return out, err
}

// SendOnChain plans and submits an atomic onchain payment from VTXOs.
func (c *DaemonServiceClient) SendOnChain(ctx context.Context,
	in *waverpc.SendOnChainRequest, _ ...grpc.CallOption) (
	*waverpc.SendOnChainResponse, error) {

	out := new(waverpc.SendOnChainResponse)
	err := c.client.Post(ctx, "/v1/daemon/send-onchain", in, out)

	return out, err
}

// Board queues confirmed boarding UTXOs for the next round.
func (c *DaemonServiceClient) Board(ctx context.Context,
	in *waverpc.BoardRequest, _ ...grpc.CallOption) (*waverpc.BoardResponse,
	error) {

	out := new(waverpc.BoardResponse)
	err := c.client.Post(ctx, "/v1/daemon/board", in, out)

	return out, err
}

// JoinNextRound commits queued round intents and joins the next round.
func (c *DaemonServiceClient) JoinNextRound(ctx context.Context,
	in *waverpc.JoinNextRoundRequest, _ ...grpc.CallOption) (
	*waverpc.JoinNextRoundResponse, error) {

	out := new(waverpc.JoinNextRoundResponse)
	err := c.client.Post(
		ctx, "/v1/daemon/join-next-round", in, out,
	)

	return out, err
}

// SweepBoardingUTXOs sweeps mature boarding UTXOs.
func (c *DaemonServiceClient) SweepBoardingUTXOs(ctx context.Context,
	in *waverpc.SweepBoardingUTXOsRequest, _ ...grpc.CallOption) (
	*waverpc.SweepBoardingUTXOsResponse, error) {

	out := new(waverpc.SweepBoardingUTXOsResponse)
	err := c.client.Post(ctx, "/v1/daemon/sweep-boarding-utxos", in, out)

	return out, err
}

// ListBoardingSweeps lists boarding sweep jobs.
func (c *DaemonServiceClient) ListBoardingSweeps(ctx context.Context,
	in *waverpc.ListBoardingSweepsRequest, _ ...grpc.CallOption) (
	*waverpc.ListBoardingSweepsResponse, error) {

	out := new(waverpc.ListBoardingSweepsResponse)
	err := c.client.Post(ctx, "/v1/daemon/list-boarding-sweeps", in, out)

	return out, err
}

// ListRounds lists client-visible rounds.
func (c *DaemonServiceClient) ListRounds(ctx context.Context,
	in *waverpc.ListRoundsRequest, _ ...grpc.CallOption) (
	*waverpc.ListRoundsResponse, error) {

	out := new(waverpc.ListRoundsResponse)
	err := c.client.Post(ctx, "/v1/daemon/list-rounds", in, out)

	return out, err
}

// GetRound returns one client-visible round.
func (c *DaemonServiceClient) GetRound(ctx context.Context,
	in *waverpc.GetRoundRequest, _ ...grpc.CallOption) (
	*waverpc.GetRoundResponse, error) {

	out := new(waverpc.GetRoundResponse)
	err := c.client.Post(ctx, "/v1/daemon/get-round", in, out)

	return out, err
}

// WatchRounds streams round updates from the daemon.
func (c *DaemonServiceClient) WatchRounds(ctx context.Context,
	in *waverpc.WatchRoundsRequest, _ ...grpc.CallOption) (
	grpc.ServerStreamingClient[waverpc.WatchRoundsResponse], error) {

	resp, err := c.client.Stream( //nolint:bodyclose // Stream owns body.
		ctx, "/v1/daemon/watch-rounds", in,
	)
	if err != nil {
		return nil, err
	}

	return NewStreamClient[waverpc.WatchRoundsResponse](
		resp, "WatchRounds", func() *waverpc.WatchRoundsResponse {
			return new(waverpc.WatchRoundsResponse)
		},
	), nil
}

// ListOORSessions lists known OOR sessions.
func (c *DaemonServiceClient) ListOORSessions(ctx context.Context,
	in *waverpc.ListOORSessionsRequest, _ ...grpc.CallOption) (
	*waverpc.ListOORSessionsResponse, error) {

	out := new(waverpc.ListOORSessionsResponse)
	err := c.client.Post(ctx, "/v1/daemon/list-oor-sessions", in, out)

	return out, err
}

// GetOORSession returns one OOR session.
func (c *DaemonServiceClient) GetOORSession(ctx context.Context,
	in *waverpc.GetOORSessionRequest, _ ...grpc.CallOption) (
	*waverpc.GetOORSessionResponse, error) {

	out := new(waverpc.GetOORSessionResponse)
	err := c.client.Post(ctx, "/v1/daemon/get-oor-session", in, out)

	return out, err
}

// EstimateFee estimates an on-chain fee.
func (c *DaemonServiceClient) EstimateFee(ctx context.Context,
	in *waverpc.EstimateFeeRequest, _ ...grpc.CallOption) (
	*waverpc.EstimateFeeResponse, error) {

	out := new(waverpc.EstimateFeeResponse)
	err := c.client.Post(ctx, "/v1/daemon/estimate-fee", in, out)

	return out, err
}

// GetFeeHistory returns recent fee estimates.
func (c *DaemonServiceClient) GetFeeHistory(ctx context.Context,
	in *waverpc.GetFeeHistoryRequest, _ ...grpc.CallOption) (
	*waverpc.GetFeeHistoryResponse, error) {

	out := new(waverpc.GetFeeHistoryResponse)
	err := c.client.Post(ctx, "/v1/daemon/get-fee-history", in, out)

	return out, err
}

// ListTransactions lists daemon wallet transactions.
func (c *DaemonServiceClient) ListTransactions(ctx context.Context,
	in *waverpc.ListTransactionsRequest, _ ...grpc.CallOption) (
	*waverpc.ListTransactionsResponse, error) {

	out := new(waverpc.ListTransactionsResponse)
	err := c.client.Post(ctx, "/v1/daemon/list-transactions", in, out)

	return out, err
}

// Unroll starts a unilateral exit job.
func (c *DaemonServiceClient) Unroll(ctx context.Context,
	in *waverpc.UnrollRequest, _ ...grpc.CallOption) (
	*waverpc.UnrollResponse, error) {

	out := new(waverpc.UnrollResponse)
	err := c.client.Post(ctx, "/v1/daemon/unroll", in, out)

	return out, err
}

// GetUnrollStatus returns one unilateral exit job status.
func (c *DaemonServiceClient) GetUnrollStatus(ctx context.Context,
	in *waverpc.GetUnrollStatusRequest, _ ...grpc.CallOption) (
	*waverpc.GetUnrollStatusResponse, error) {

	out := new(waverpc.GetUnrollStatusResponse)
	err := c.client.Post(ctx, "/v1/daemon/get-unroll-status", in, out)

	return out, err
}

// ArmVHTLCRecovery persists a dormant vHTLC recovery job.
func (c *DaemonServiceClient) ArmVHTLCRecovery(ctx context.Context,
	in *waverpc.ArmVHTLCRecoveryRequest, _ ...grpc.CallOption) (
	*waverpc.ArmVHTLCRecoveryResponse, error) {

	out := new(waverpc.ArmVHTLCRecoveryResponse)
	err := c.client.Post(ctx, "/v1/daemon/arm-vhtlc-recovery", in, out)

	return out, err
}

// EscalateVHTLCRecovery starts an armed vHTLC recovery job.
func (c *DaemonServiceClient) EscalateVHTLCRecovery(ctx context.Context,
	in *waverpc.EscalateVHTLCRecoveryRequest, _ ...grpc.CallOption) (
	*waverpc.EscalateVHTLCRecoveryResponse, error) {

	out := new(waverpc.EscalateVHTLCRecoveryResponse)
	err := c.client.Post(ctx, "/v1/daemon/escalate-vhtlc-recovery", in, out)

	return out, err
}

// CancelVHTLCRecovery records that a vHTLC recovery job is no longer needed.
func (c *DaemonServiceClient) CancelVHTLCRecovery(ctx context.Context,
	in *waverpc.CancelVHTLCRecoveryRequest, _ ...grpc.CallOption) (
	*waverpc.CancelVHTLCRecoveryResponse, error) {

	out := new(waverpc.CancelVHTLCRecoveryResponse)
	err := c.client.Post(ctx, "/v1/daemon/cancel-vhtlc-recovery", in, out)

	return out, err
}

// GetVHTLCRecoveryStatus returns one vHTLC recovery job status.
func (c *DaemonServiceClient) GetVHTLCRecoveryStatus(ctx context.Context,
	in *waverpc.GetVHTLCRecoveryStatusRequest, _ ...grpc.CallOption) (
	*waverpc.GetVHTLCRecoveryStatusResponse, error) {

	out := new(waverpc.GetVHTLCRecoveryStatusResponse)
	err := c.client.Post(
		ctx, "/v1/daemon/get-vhtlc-recovery-status", in, out,
	)

	return out, err
}

// ListVHTLCRecoveries returns vHTLC recovery jobs for operator inspection.
func (c *DaemonServiceClient) ListVHTLCRecoveries(ctx context.Context,
	in *waverpc.ListVHTLCRecoveriesRequest, _ ...grpc.CallOption) (
	*waverpc.ListVHTLCRecoveriesResponse, error) {

	out := new(waverpc.ListVHTLCRecoveriesResponse)
	err := c.client.Post(ctx, "/v1/daemon/list-vhtlc-recoveries", in, out)

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

// QuotePay previews a daemon-owned pay swap.
func (c *SwapClientServiceClient) QuotePay(ctx context.Context,
	in *swapclientrpc.QuotePayRequest, _ ...grpc.CallOption) (
	*swapclientrpc.QuotePayResponse, error) {

	out := new(swapclientrpc.QuotePayResponse)
	err := c.client.Post(ctx, "/v1/swapclient/quote-pay", in, out)

	return out, err
}

// StartPay starts a daemon-owned pay swap.
func (c *SwapClientServiceClient) StartPay(ctx context.Context,
	in *swapclientrpc.StartPayRequest, _ ...grpc.CallOption) (
	*swapclientrpc.StartPayResponse, error) {

	out := new(swapclientrpc.StartPayResponse)
	err := c.client.Post(ctx, "/v1/swapclient/start-pay", in, out)

	return out, err
}

// CreateCredit creates a daemon-owned credit funding operation.
func (c *SwapClientServiceClient) CreateCredit(ctx context.Context,
	in *swapclientrpc.CreateCreditRequest, _ ...grpc.CallOption) (
	*swapclientrpc.CreateCreditResponse, error) {

	out := new(swapclientrpc.CreateCreditResponse)
	err := c.client.Post(ctx, "/v1/swapclient/create-credit", in, out)

	return out, err
}

// RedeemCredit redeems daemon wallet credits into an Ark output.
func (c *SwapClientServiceClient) RedeemCredit(ctx context.Context,
	in *swapclientrpc.RedeemCreditRequest, _ ...grpc.CallOption) (
	*swapclientrpc.RedeemCreditResponse, error) {

	out := new(swapclientrpc.RedeemCreditResponse)
	err := c.client.Post(ctx, "/v1/swapclient/redeem-credit", in, out)

	return out, err
}

// ListCredits lists daemon wallet credit account state.
func (c *SwapClientServiceClient) ListCredits(ctx context.Context,
	in *swapclientrpc.ListCreditsRequest, _ ...grpc.CallOption) (
	*swapclientrpc.ListCreditsResponse, error) {

	out := new(swapclientrpc.ListCreditsResponse)
	err := c.client.Post(ctx, "/v1/swapclient/list-credits", in, out)

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
	opts ...Option) wavewalletrpc.WalletServiceClient {

	return NewWalletServiceClientFromClient(New(addr, opts...))
}

// NewWalletServiceClientFromClient creates a WalletService REST client from an
// existing shared REST transport.
func NewWalletServiceClientFromClient(
	c *Client) wavewalletrpc.WalletServiceClient {

	return &WalletServiceClient{client: c}
}

// WalletServiceClient implements wavewalletrpc.WalletServiceClient over
// grpc-gateway.
type WalletServiceClient struct {
	client *Client
}

// Create initializes a wallet through WalletService.
func (c *WalletServiceClient) Create(ctx context.Context,
	in *wavewalletrpc.CreateRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.CreateResponse, error) {

	out := new(wavewalletrpc.CreateResponse)
	err := c.client.Post(ctx, "/v1/wallet/create", in, out)

	return out, err
}

// Unlock unlocks a wallet through WalletService.
func (c *WalletServiceClient) Unlock(ctx context.Context,
	in *wavewalletrpc.UnlockRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.UnlockResponse, error) {

	out := new(wavewalletrpc.UnlockResponse)
	err := c.client.Post(ctx, "/v1/wallet/unlock", in, out)

	return out, err
}

// PrepareSend validates and previews a wallet send.
func (c *WalletServiceClient) PrepareSend(ctx context.Context,
	in *wavewalletrpc.PrepareSendRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.PrepareSendResponse, error) {

	out := new(wavewalletrpc.PrepareSendResponse)
	err := c.client.Post(ctx, "/v1/wallet/prepare-send", in, out)

	return out, err
}

// Send dispatches a wallet send.
func (c *WalletServiceClient) Send(ctx context.Context,
	in *wavewalletrpc.SendRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.SendResponse, error) {

	out := new(wavewalletrpc.SendResponse)
	err := c.client.Post(ctx, "/v1/wallet/send", in, out)

	return out, err
}

// Recv creates a wallet receive invoice.
func (c *WalletServiceClient) Recv(ctx context.Context,
	in *wavewalletrpc.RecvRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.RecvResponse, error) {

	out := new(wavewalletrpc.RecvResponse)
	err := c.client.Post(ctx, "/v1/wallet/recv", in, out)

	return out, err
}

// List returns a wallet list view.
func (c *WalletServiceClient) List(ctx context.Context,
	in *wavewalletrpc.ListRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.ListResponse, error) {

	out := new(wavewalletrpc.ListResponse)
	err := c.client.Post(ctx, "/v1/wallet/list", in, out)

	return out, err
}

// Deposit returns a wallet deposit address.
func (c *WalletServiceClient) Deposit(ctx context.Context,
	in *wavewalletrpc.DepositRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.DepositResponse, error) {

	out := new(wavewalletrpc.DepositResponse)
	err := c.client.Post(ctx, "/v1/wallet/deposit", in, out)

	return out, err
}

// Balance returns the wallet balance summary.
func (c *WalletServiceClient) Balance(ctx context.Context,
	in *wavewalletrpc.BalanceRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.BalanceResponse, error) {

	out := new(wavewalletrpc.BalanceResponse)
	err := c.client.Post(ctx, "/v1/wallet/balance", in, out)

	return out, err
}

// Status returns the wallet readiness summary.
func (c *WalletServiceClient) Status(ctx context.Context,
	in *wavewalletrpc.StatusRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.StatusResponse, error) {

	out := new(wavewalletrpc.StatusResponse)
	err := c.client.Post(ctx, "/v1/wallet/status", in, out)

	return out, err
}

// GetExitPlan previews wallet unilateral-exit funding readiness.
func (c *WalletServiceClient) GetExitPlan(ctx context.Context,
	in *wavewalletrpc.GetExitPlanRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.GetExitPlanResponse, error) {

	out := new(wavewalletrpc.GetExitPlanResponse)
	err := c.client.Post(ctx, "/v1/wallet/exit-plan", in, out)

	return out, err
}

// SweepWallet previews or broadcasts a backing-wallet sweep.
func (c *WalletServiceClient) SweepWallet(ctx context.Context,
	in *wavewalletrpc.SweepWalletRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.SweepWalletResponse, error) {

	out := new(wavewalletrpc.SweepWalletResponse)
	err := c.client.Post(ctx, "/v1/wallet/sweep-wallet", in, out)

	return out, err
}

// Exit starts a wallet unilateral exit.
func (c *WalletServiceClient) Exit(ctx context.Context,
	in *wavewalletrpc.ExitRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.ExitResponse, error) {

	out := new(wavewalletrpc.ExitResponse)
	err := c.client.Post(ctx, "/v1/wallet/exit", in, out)

	return out, err
}

// ExitStatus returns one wallet unilateral exit status.
func (c *WalletServiceClient) ExitStatus(ctx context.Context,
	in *wavewalletrpc.ExitStatusRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.ExitStatusResponse, error) {

	out := new(wavewalletrpc.ExitStatusResponse)
	err := c.client.Post(ctx, "/v1/wallet/exit-status", in, out)

	return out, err
}

// ExitSummary returns the wallet-wide portfolio of in-progress exits.
func (c *WalletServiceClient) ExitSummary(ctx context.Context,
	in *wavewalletrpc.ExitSummaryRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.ExitSummaryResponse, error) {

	out := new(wavewalletrpc.ExitSummaryResponse)
	err := c.client.Post(ctx, "/v1/wallet/exit-summary", in, out)

	return out, err
}

// SubscribeWallet streams wallet updates.
func (c *WalletServiceClient) SubscribeWallet(ctx context.Context,
	in *wavewalletrpc.SubscribeWalletRequest, _ ...grpc.CallOption) (
	grpc.ServerStreamingClient[wavewalletrpc.SubscribeWalletResponse],
	error) {

	resp, err := c.client.Stream( //nolint:bodyclose // Stream owns body.
		ctx, "/v1/wallet/subscribe", in,
	)
	if err != nil {
		return nil, err
	}

	return NewStreamClient[wavewalletrpc.SubscribeWalletResponse](
		resp, "SubscribeWallet",
		func() *wavewalletrpc.SubscribeWalletResponse {
			return new(wavewalletrpc.SubscribeWalletResponse)
		},
	), nil
}
