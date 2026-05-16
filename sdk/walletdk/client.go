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

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const defaultCloseTimeout = 5 * time.Second

// Client is the wallet-facing SDK handle. It is safe for concurrent use.
type Client struct {
	conn   grpc.ClientConnInterface
	daemon daemonrpc.DaemonServiceClient
	swaps  swapclientrpc.SwapClientServiceClient
	wallet walletrpc.WalletServiceClient

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

	dialOpts := append([]grpc.DialOption(nil), cfg.DialOptions...)
	if len(dialOpts) == 0 {
		creds := insecure.NewCredentials()
		dialOpts = append(
			dialOpts, grpc.WithTransportCredentials(creds),
		)
	}

	conn, err := grpc.NewClient(cfg.Address, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial wallet daemon: %w", err)
	}
	if err := waitForReady(ctx, conn, nil); err != nil {
		closeErr := conn.Close()

		return nil, fmt.Errorf("wait for wallet daemon readiness: %w",
			errors.Join(err, closeErr))
	}

	closeFn := func(context.Context) error {
		return conn.Close()
	}

	return newClient(conn, true, closedWaitChan(), closeFn), nil
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
func (c *Client) WalletRPC() walletrpc.WalletServiceClient {
	if c == nil {
		return nil
	}

	return c.wallet
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
		},
	)
	if err != nil {
		return nil, fmt.Errorf("initialize wallet: %w", err)
	}

	return &CreateWalletResult{
		Mnemonic:       mnemonic,
		EncipheredSeed: encipheredSeed,
		IdentityPubKey: initResp.GetIdentityPubkey(),
	}, nil
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

	resp, err := c.wallet.Balance(ctx, &walletrpc.BalanceRequest{})
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

	resp, err := c.wallet.Deposit(ctx, &walletrpc.DepositRequest{
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

	resp, err := c.wallet.Recv(ctx, &walletrpc.RecvRequest{
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

// Send dispatches an outbound wallet payment.
func (c *Client) Send(ctx context.Context, req SendRequest) (*SendResult,
	error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}

	protoReq := &walletrpc.SendRequest{
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
		protoReq.Destination = &walletrpc.SendRequest_Invoice{
			Invoice: invoice,
		}

	case onchainAddress != "":
		protoReq.Destination = &walletrpc.SendRequest_OnchainAddress{
			OnchainAddress: onchainAddress,
		}

	default:
		return nil, fmt.Errorf("invoice or onchain_address is required")
	}

	resp, err := c.wallet.Send(ctx, protoReq)
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

	resp, err := c.wallet.List(ctx, &walletrpc.ListRequest{
		View:        protoView,
		PendingOnly: req.PendingOnly,
		Kinds:       kinds,
		Limit:       req.Limit,
		Offset:      req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list wallet entries: %w", err)
	}

	return listResultFromProto(view, resp), nil
}

// Exit triggers a unilateral exit (unroll) for the specified VTXO
// outpoint. The daemon assembles a recovery proof, spawns a durable
// exit job, and drives the on-chain recovery to completion.
func (c *Client) Exit(ctx context.Context, req ExitRequest) (*ExitResult,
	error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}
	if req.Outpoint == "" {
		return nil, fmt.Errorf("outpoint is required")
	}

	resp, err := c.wallet.Exit(ctx, &walletrpc.ExitRequest{
		Outpoint: req.Outpoint,
	})
	if err != nil {
		return nil, fmt.Errorf("exit: %w", err)
	}

	return &ExitResult{
		Created: resp.GetCreated(),
		ActorID: resp.GetActorId(),
	}, nil
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

	resp, err := c.wallet.ExitStatus(ctx, &walletrpc.ExitStatusRequest{
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

// Status returns wallet readiness, balance, and pending activity counts.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}

	resp, err := c.wallet.Status(ctx, &walletrpc.StatusRequest{})
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
		ctx, &walletrpc.SubscribeWalletRequest{
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

			select {
			case updates <- entryFromProto(resp):
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
// walletrpc before any RPC is attempted.
func (c *Client) requireWalletRPC() error {
	if c == nil || !c.canWallet {
		return ErrWalletRPCUnavailable
	}

	return nil
}

// newClient assembles all RPC clients that share one gRPC connection.
func newClient(conn grpc.ClientConnInterface, canWallet bool,
	waitCh <-chan error, closeFn func(context.Context) error) *Client {

	return &Client{
		conn:      conn,
		daemon:    daemonrpc.NewDaemonServiceClient(conn),
		swaps:     swapclientrpc.NewSwapClientServiceClient(conn),
		wallet:    walletrpc.NewWalletServiceClient(conn),
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
