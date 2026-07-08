package walletdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	btcwalletrpc "github.com/btcsuite/btcwallet/rpc/walletrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/restclient"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/lightninglabs/darepo-client/rpcauth"
	"google.golang.org/grpc"
)

const defaultCloseTimeout = 5 * time.Second

// Client is the wallet-facing SDK handle. It is safe for concurrent use.
type Client struct {
	conn   grpc.ClientConnInterface
	daemon daemonrpc.DaemonServiceClient
	swaps  swapclientrpc.SwapClientServiceClient
	wallet walletdkrpc.WalletServiceClient
	btcw   btcwalletrpc.WalletServiceClient
	btcwV  btcwalletrpc.VersionServiceClient

	canWallet bool
	waitCh    <-chan error

	closeFn   func(context.Context) error
	closeOnce sync.Once
	closeErr  error
}

// Connect returns a walletdk client connected to an external daemon.
func Connect(ctx context.Context, cfg ConnectConfig) (*Client, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("address is required")
	}

	switch cfg.Transport {
	case "", TransportGRPC:
		return connectGRPC(ctx, cfg)

	case TransportREST:
		return connectREST(ctx, cfg)

	default:
		return nil, fmt.Errorf("unknown walletdk transport %q",
			cfg.Transport)
	}
}

func connectREST(ctx context.Context, cfg ConnectConfig) (*Client, error) {
	opts, err := connectRESTOptions(cfg)
	if err != nil {
		return nil, err
	}

	transport := restclient.New(cfg.Address, opts...)
	daemon := restclient.NewDaemonServiceClientFromClient(transport)
	swaps := restclient.NewSwapClientServiceClientFromClient(transport)
	wallet := restclient.NewWalletServiceClientFromClient(transport)

	if _, err := daemon.GetInfo(
		ctx, &daemonrpc.GetInfoRequest{},
	); err != nil {
		return nil, fmt.Errorf("wait for wallet daemon readiness: %w",
			err)
	}

	closeFn := func(context.Context) error {
		return nil
	}

	return newClientWithRPC(
		nil, daemon, swaps, wallet, true, closedWaitChan(), closeFn,
	), nil
}

// connectRESTOptions returns HTTP and macaroon options for REST transports.
func connectRESTOptions(cfg ConnectConfig) ([]restclient.Option, error) {
	httpClient := cfg.HTTPClient
	if httpClient == nil && cfg.TLSCertPath != "" {
		var err error
		httpClient, err = rpcauth.HTTPClientForCert(cfg.TLSCertPath)
		if err != nil {
			return nil, err
		}
	}

	opts := []restclient.Option{
		restclient.WithHTTPClient(httpClient),
	}

	if cfg.MacaroonPath != "" {
		macHex, err := rpcauth.HexFromFile(cfg.MacaroonPath)
		if err != nil {
			return nil, err
		}

		opts = append(
			opts, restclient.WithHeader(
				rpcauth.MacaroonMetadataKey, macHex,
			),
		)
	}

	return opts, nil
}

// Stop shuts down the embedded daemon or releases the remote transport.
func (c *Client) Stop() error {
	return c.close()
}

// Close is an alias for Stop for callers that expect io-style cleanup.
func (c *Client) Close() error {
	return c.close()
}

// Wait returns a channel that yields the embedded daemon's terminal run error.
func (c *Client) Wait() <-chan error {
	if c == nil || c.waitCh == nil {
		return closedWaitChan()
	}

	return c.waitCh
}

// GRPCConn returns the private gRPC client connection used by walletdk.
func (c *Client) GRPCConn() grpc.ClientConnInterface {
	if c == nil {
		return nil
	}

	return c.conn
}

// WalletRPC returns the wallet RPC client for advanced callers.
func (c *Client) WalletRPC() walletdkrpc.WalletServiceClient {
	if c == nil {
		return nil
	}

	return c.wallet
}

// BtcwalletRPC returns btcsuite btcwallet's native WalletService client.
func (c *Client) BtcwalletRPC() btcwalletrpc.WalletServiceClient {
	if c == nil {
		return nil
	}

	return c.btcw
}

// BtcwalletVersionRPC returns btcsuite btcwallet's VersionService client.
func (c *Client) BtcwalletVersionRPC() btcwalletrpc.VersionServiceClient {
	if c == nil {
		return nil
	}

	return c.btcwV
}

// ArkRPC returns the raw daemon RPC client for advanced callers.
func (c *Client) ArkRPC() daemonrpc.DaemonServiceClient {
	if c == nil {
		return nil
	}

	return c.daemon
}

// SwapRPC returns the raw daemon-owned swap RPC client for advanced callers.
func (c *Client) SwapRPC() swapclientrpc.SwapClientServiceClient {
	if c == nil {
		return nil
	}

	return c.swaps
}

// GetInfo returns the current daemon readiness snapshot.
func (c *Client) GetInfo(ctx context.Context) (*Info, error) {
	resp, err := c.daemon.GetInfo(ctx, &daemonrpc.GetInfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("get daemon info: %w", err)
	}

	return &Info{
		Version:         resp.GetVersion(),
		Commit:          resp.GetCommit(),
		Network:         resp.GetNetwork(),
		BlockHeight:     resp.GetBlockHeight(),
		ServerConnected: resp.GetServerConnected(),
		WalletType:      resp.GetWalletType(),
		WalletState:     WalletState(resp.GetWalletState()),
		IdentityPubKey:  resp.GetIdentityPubkey(),
	}, nil
}

// CreateWallet creates or imports the embedded daemon wallet.
func (c *Client) CreateWallet(ctx context.Context, req CreateWalletRequest) (
	*CreateWalletResult, error) {

	mnemonic := append([]string(nil), req.Mnemonic...)
	var encipheredSeed []byte

	if len(mnemonic) == 0 {
		seed, err := c.daemon.GenSeed(ctx, &daemonrpc.GenSeedRequest{
			SeedPassphrase: bytes.Clone(req.SeedPassphrase),
		})
		if err != nil {
			return nil, fmt.Errorf("generate wallet seed: %w", err)
		}

		mnemonic = append([]string(nil), seed.GetMnemonic()...)
		encipheredSeed = bytes.Clone(seed.GetEncipheredSeed())
	}

	initResp, err := c.daemon.InitWallet(ctx,
		&daemonrpc.InitWalletRequest{
			Mnemonic:       mnemonic,
			SeedPassphrase: bytes.Clone(req.SeedPassphrase),
			WalletPassword: bytes.Clone(req.WalletPassword),
			RecoverState:   req.RecoverState,
			RecoveryWindow: req.RecoveryWindow,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("initialize wallet: %w", err)
	}

	return &CreateWalletResult{
		Mnemonic:       mnemonic,
		EncipheredSeed: encipheredSeed,
		IdentityPubKey: initResp.GetIdentityPubkey(),
		RecoveryRan:    initResp.GetRecoveryRan(),
		RecoveredBoardingAddresses: initResp.
			GetRecoveredBoardingAddresses(),
		RecoveredBoardingUTXOs: initResp.
			GetRecoveredBoardingUtxos(),
		RecoveredVTXOs: initResp.GetRecoveredVtxos(),
		RecoveredOORReceiveScripts: initResp.
			GetRecoveredOorReceiveScripts(),
		RecoveredOORRecipientEvents: initResp.GetRecoveredOorEvents(),
	}, nil
}

// initFromMnemonic runs the daemon InitWallet step for an already-derived
// mnemonic, returning the daemon identity pubkey. The passkey import path uses
// it; CreateWallet inlines its own InitWallet call.
func (c *Client) initFromMnemonic(ctx context.Context, mnemonic []string,
	seedPassphrase, walletPassword []byte) (string, error) {

	initResp, err := c.daemon.InitWallet(ctx,
		&daemonrpc.InitWalletRequest{
			Mnemonic:       mnemonic,
			SeedPassphrase: bytes.Clone(seedPassphrase),
			WalletPassword: bytes.Clone(walletPassword),
		},
	)
	if err != nil {
		return "", fmt.Errorf("initialize wallet: %w", err)
	}

	return initResp.GetIdentityPubkey(), nil
}

// UnlockWallet unlocks an existing embedded daemon wallet.
func (c *Client) UnlockWallet(ctx context.Context, req UnlockWalletRequest) (
	*UnlockWalletResult, error) {

	resp, err := c.daemon.UnlockWallet(ctx,
		&daemonrpc.UnlockWalletRequest{
			WalletPassword: bytes.Clone(req.WalletPassword),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("unlock wallet: %w", err)
	}

	return &UnlockWalletResult{
		IdentityPubKey: resp.GetIdentityPubkey(),
	}, nil
}

// Balance returns the wallet-level balance summary.
func (c *Client) Balance(ctx context.Context) (*Balance, error) {
	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}

	resp, err := c.wallet.Balance(ctx, &walletdkrpc.BalanceRequest{})
	if err != nil {
		return nil, fmt.Errorf("get wallet balance: %w", err)
	}

	balance := balanceFromProto(resp)

	return &balance, nil
}

// Deposit allocates a fresh tracked boarding address.
func (c *Client) Deposit(ctx context.Context, req DepositRequest) (
	*DepositResult, error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}

	resp, err := c.wallet.Deposit(ctx, &walletdkrpc.DepositRequest{
		AmtSatHint: req.AmountSatHint,
	})
	if err != nil {
		return nil, fmt.Errorf("create wallet deposit: %w", err)
	}

	return &DepositResult{
		Address: resp.GetOnchainAddress(),
		Entry:   entryFromProto(resp.GetEntry()),
	}, nil
}

// Receive creates a Lightning invoice payable into the wallet.
func (c *Client) Receive(ctx context.Context, req ReceiveRequest) (
	*ReceiveResult, error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}
	if req.AmountSat == 0 {
		return nil, fmt.Errorf("amount_sat must be positive")
	}

	resp, err := c.wallet.Recv(ctx, &walletdkrpc.RecvRequest{
		AmtSat: req.AmountSat,
		Memo:   req.Memo,
	})
	if err != nil {
		return nil, fmt.Errorf("create receive invoice: %w", err)
	}

	return &ReceiveResult{
		Invoice: resp.GetInvoice(),
		Entry:   entryFromProto(resp.GetEntry()),
	}, nil
}

// PrepareSend validates and previews an outbound wallet payment.
func (c *Client) PrepareSend(ctx context.Context, req PrepareSendRequest) (
	*PrepareSendResult, error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}

	protoReq := &walletdkrpc.PrepareSendRequest{
		AmtSat:    req.AmountSat,
		Note:      req.Note,
		MaxFeeSat: req.MaxFeeSat,
		SweepAll:  req.SweepAll,
	}
	invoice := strings.TrimSpace(req.Invoice)
	onchainAddress := strings.TrimSpace(req.OnchainAddress)
	switch {
	case invoice != "" && onchainAddress != "":
		return nil, fmt.Errorf("set invoice or onchain_address, not " +
			"both")

	case invoice != "":
		protoReq.Destination = &walletdkrpc.PrepareSendRequest_Invoice{
			Invoice: invoice,
		}

	case onchainAddress != "":
		protoReq.Destination = &walletdkrpc.
			PrepareSendRequest_OnchainAddress{
			OnchainAddress: onchainAddress,
		}

	default:
		return nil, fmt.Errorf("invoice or onchain_address is required")
	}

	resp, err := c.wallet.PrepareSend(ctx, protoReq)
	if err != nil {
		return nil, fmt.Errorf("prepare wallet payment: %w", err)
	}

	return prepareSendResultFromProto(resp), nil
}

// SendPrepared dispatches a previously prepared outbound wallet payment.
func (c *Client) SendPrepared(ctx context.Context, req SendPreparedRequest) (
	*SendResult, error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.SendIntentID) == "" {
		return nil, fmt.Errorf("send_intent_id is required")
	}

	resp, err := c.wallet.Send(ctx, &walletdkrpc.SendRequest{
		SendIntentId: strings.TrimSpace(req.SendIntentID),
	})
	if err != nil {
		return nil, fmt.Errorf("send wallet payment: %w", err)
	}

	return &SendResult{
		Entry:           entryFromProto(resp.GetEntry()),
		ActualAmountSat: resp.GetActualAmountSat(),
	}, nil
}

// List returns the unified wallet view selected by req.View. The
// response carries exactly one of Activity, VTXOs, or Onchain
// populated; callers should switch on the returned View to pick the
// right field.
func (c *Client) List(ctx context.Context, req ListRequest) (*ListResult,
	error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}

	view := req.View
	if view == "" {
		view = ListViewActivity
	}
	protoView, err := listViewToProto(view)
	if err != nil {
		return nil, err
	}

	kinds, err := entryKindsToProto(req.Kinds)
	if err != nil {
		return nil, err
	}

	resp, err := c.wallet.List(ctx, &walletdkrpc.ListRequest{
		View:        protoView,
		PendingOnly: req.PendingOnly,
		Kinds:       kinds,
		Limit:       req.Limit,
		Offset:      req.Offset,
		Cursor:      req.Cursor,
	})
	if err != nil {
		return nil, fmt.Errorf("list wallet entries: %w", err)
	}

	return listResultFromProto(view, resp), nil
}

// Exit requests wallet-facing exit for a single VTXO outpoint. The daemon
// queues cooperative leave by default, generating an internal destination when
// req.Destination is empty. Unilateral unroll is reachable only when
// req.ForceUnrollAck carries the daemon's exact acknowledgement string.
func (c *Client) Exit(ctx context.Context, req ExitRequest) (*ExitResult,
	error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}
	if req.Outpoint == "" {
		return nil, fmt.Errorf("outpoint is required")
	}

	dest := strings.TrimSpace(req.Destination)
	ack := strings.TrimSpace(req.ForceUnrollAck)
	if dest != "" && ack != "" {
		return nil, fmt.Errorf("destination cannot be combined with " +
			"force_unroll_ack")
	}

	resp, err := c.wallet.Exit(ctx, &walletdkrpc.ExitRequest{
		Outpoint:       req.Outpoint,
		OnchainAddress: dest,
		ForceUnrollAck: ack,
	})
	if err != nil {
		return nil, fmt.Errorf("exit: %w", err)
	}

	switch resp.GetMode() {
	case walletdkrpc.ExitMode_EXIT_MODE_COOPERATIVE:
		return &ExitResult{
			Path:            ExitPathCooperative,
			Cooperative:     true,
			QueuedOutpoints: resp.GetQueuedOutpoints(),
		}, nil

	case walletdkrpc.ExitMode_EXIT_MODE_UNILATERAL:
		return &ExitResult{
			Path:    ExitPathUnilateral,
			Created: resp.GetCreated(),
			ActorID: resp.GetActorId(),
		}, nil

	default:
		return nil, fmt.Errorf("exit: daemon returned unknown mode %v",
			resp.GetMode())
	}
}

// ExitStatus reports the current phase of an exit job for the
// specified VTXO outpoint. Found is false when no job exists for the
// outpoint; the call does not return an error in that case.
func (c *Client) ExitStatus(ctx context.Context, req ExitStatusRequest) (
	*ExitStatusResult, error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}
	if req.Outpoint == "" {
		return nil, fmt.Errorf("outpoint is required")
	}

	resp, err := c.wallet.ExitStatus(ctx, &walletdkrpc.ExitStatusRequest{
		Outpoint: req.Outpoint,
	})
	if err != nil {
		return nil, fmt.Errorf("exit status: %w", err)
	}

	return &ExitStatusResult{
		Found:     resp.GetFound(),
		Status:    exitJobStatusFromProto(resp.GetStatus()),
		SweepTxid: resp.GetSweepTxid(),
		LastError: resp.GetLastError(),
	}, nil
}

// GetExitPlan previews whether the backing wallet is ready to start a
// unilateral exit for a slice of VTXOs.
func (c *Client) GetExitPlan(ctx context.Context, req GetExitPlanRequest) (
	*GetExitPlanResult, error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}
	if len(req.Outpoints) == 0 {
		return nil, fmt.Errorf("outpoints is required")
	}

	resp, err := c.wallet.GetExitPlan(
		ctx, &walletdkrpc.GetExitPlanRequest{
			Outpoints:  req.Outpoints,
			ConfTarget: req.ConfTarget,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("get exit plan: %w", err)
	}

	return exitPlanFromProto(resp), nil
}

// SweepWallet previews or broadcasts a sweep of confirmed backing-wallet
// funds to the caller-supplied address.
func (c *Client) SweepWallet(ctx context.Context, req SweepWalletRequest) (
	*SweepWalletResult, error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.DestinationAddress) == "" {
		return nil, fmt.Errorf("destination_address is required")
	}

	resp, err := c.wallet.SweepWallet(
		ctx, &walletdkrpc.SweepWalletRequest{
			DestinationAddress: strings.TrimSpace(
				req.DestinationAddress,
			),
			Broadcast:          req.Broadcast,
			FeeRateSatPerVbyte: req.FeeRateSatPerVByte,
			ConfTarget:         req.ConfTarget,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("sweep wallet: %w", err)
	}

	return sweepWalletFromProto(resp), nil
}

// Status returns wallet readiness, balance, and pending activity counts.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}

	resp, err := c.wallet.Status(ctx, &walletdkrpc.StatusRequest{})
	if err != nil {
		return nil, fmt.Errorf("get wallet status: %w", err)
	}

	return &Status{
		Ready:        resp.GetReady(),
		Unlocked:     resp.GetUnlocked(),
		Network:      resp.GetNetwork(),
		Balance:      balanceFromProto(resp.GetBalance()),
		PendingCount: resp.GetPendingCount(),
	}, nil
}

// Subscribe streams normalized wallet activity entries until ctx ends.
func (c *Client) Subscribe(ctx context.Context, req SubscribeRequest) (
	<-chan Entry, <-chan error, error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, nil, err
	}

	kinds, err := entryKindsToProto(req.Kinds)
	if err != nil {
		return nil, nil, err
	}

	stream, err := c.wallet.SubscribeWallet(
		ctx, &walletdkrpc.SubscribeWalletRequest{
			IncludeExisting: req.IncludeExisting,
			Kinds:           kinds,
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("subscribe wallet: %w", err)
	}

	updates := make(chan Entry)
	errs := make(chan error, 1)
	go func() {
		defer close(updates)
		defer close(errs)

		for {
			resp, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return
				}

				errs <- fmt.Errorf("receive wallet update: %w",
					err)

				return
			}

			// A gap tells the consumer it fell behind the live
			// stream. Surface it as a terminal error so the host
			// reconciles current state via List and re-subscribes,
			// rather than silently dropping the missed updates.
			if gap := resp.GetGap(); gap != nil {
				errs <- fmt.Errorf("wallet subscription gap: "+
					"%s: reconcile via List and "+
					"re-subscribe", gap.GetReason())

				return
			}

			entry := resp.GetEntry()
			if entry == nil {
				continue
			}

			select {
			case updates <- entryFromProto(entry):
			case <-ctx.Done():
				errs <- fmt.Errorf("wallet subscription "+
					"closed: %w", ctx.Err())

				return
			}
		}
	}()

	return updates, errs, nil
}

// requireWalletRPC fails fast when the embedded build cannot register
// walletdkrpc before any RPC is attempted.
func (c *Client) requireWalletRPC() error {
	if c == nil || !c.canWallet {
		return ErrWalletRPCUnavailable
	}

	return nil
}

// newClient assembles all RPC clients that share one gRPC connection.
func newClient(conn grpc.ClientConnInterface, canWallet bool,
	waitCh <-chan error, closeFn func(context.Context) error) *Client {

	return newClientWithRPC(
		conn, daemonrpc.NewDaemonServiceClient(conn),
		swapclientrpc.NewSwapClientServiceClient(conn),
		walletdkrpc.NewWalletServiceClient(conn), canWallet, waitCh,
		closeFn,
	)
}

func newClientWithRPC(conn grpc.ClientConnInterface,
	daemon daemonrpc.DaemonServiceClient,
	swaps swapclientrpc.SwapClientServiceClient,
	wallet walletdkrpc.WalletServiceClient, canWallet bool,
	waitCh <-chan error, closeFn func(context.Context) error) *Client {

	var btcw btcwalletrpc.WalletServiceClient
	var btcwV btcwalletrpc.VersionServiceClient
	if conn != nil {
		btcw = btcwalletrpc.NewWalletServiceClient(conn)
		btcwV = btcwalletrpc.NewVersionServiceClient(conn)
	}

	return &Client{
		conn:      conn,
		daemon:    daemon,
		swaps:     swaps,
		wallet:    wallet,
		btcw:      btcw,
		btcwV:     btcwV,
		canWallet: canWallet,
		waitCh:    waitCh,
		closeFn:   closeFn,
	}
}

// close releases resources once so Stop and Close can be used
// interchangeably by different host integrations.
func (c *Client) close() error {
	if c == nil {
		return nil
	}

	c.closeOnce.Do(func() {
		if c.closeFn == nil {
			return
		}

		closeCtx, cancel := context.WithTimeout(
			context.Background(), defaultCloseTimeout,
		)
		defer cancel()

		c.closeErr = c.closeFn(closeCtx)
	})

	return c.closeErr
}
