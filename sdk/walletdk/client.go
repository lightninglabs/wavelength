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
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	transport := restclient.New(
		cfg.Address, restclient.WithHTTPClient(cfg.HTTPClient),
	)
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

// OnchainAddress allocates a fresh taproot address from the backing wallet.
func (c *Client) OnchainAddress(ctx context.Context) (*OnchainAddressResult,
	error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}

	resp, err := c.wallet.OnchainAddress(
		ctx, &walletdkrpc.WalletOnchainAddressRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("create wallet onchain address: %w", err)
	}

	return &OnchainAddressResult{
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

// Send dispatches an outbound wallet payment.
func (c *Client) Send(ctx context.Context, req SendRequest) (*SendResult,
	error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}

	protoReq := &walletdkrpc.SendRequest{
		AmtSat:      req.AmountSat,
		Note:        req.Note,
		MaxFeeSat:   req.MaxFeeSat,
		SweepAll:    req.SweepAll,
		FromOnchain: req.FromOnchain,
	}
	invoice := strings.TrimSpace(req.Invoice)
	onchainAddress := strings.TrimSpace(req.OnchainAddress)
	switch {
	case invoice != "" && onchainAddress != "":
		return nil, fmt.Errorf("set invoice or onchain_address, not " +
			"both")

	case invoice != "":
		protoReq.Destination = &walletdkrpc.SendRequest_Invoice{
			Invoice: invoice,
		}

	case onchainAddress != "":
		protoReq.Destination = &walletdkrpc.SendRequest_OnchainAddress{
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

	resp, err := c.wallet.List(ctx, &walletdkrpc.ListRequest{
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

// Exit drives the SDK's two-track exit policy for a single VTXO
// outpoint. When req.Destination is set, Exit first attempts a
// server-cooperative leave (LeaveVTXOs RPC) so the VTXO is unwound
// through the next assembling round and the leave output lands on
// the caller-supplied on-chain address. The SDK only falls back to
// unilateral unroll on transport-class errors (operator unreachable
// or the caller's ctx fired); caller-side errors such as
// InvalidArgument are returned verbatim so a typo on Destination is
// loud rather than silently routed to a wallet-derived script. A
// nil-error LeaveVTXOs response that does not echo req.Outpoint in
// QueuedOutpoints (H-2) is treated as a per-outpoint cooperative
// failure and routed through the same fallback pipeline. When
// req.Destination is empty the cooperative path is skipped and Exit
// goes straight to unilateral unroll.
//
// Before falling back, Exit cross-checks via ListVTXOs that the
// daemon did not already admit the cooperative leave server-side
// (which can happen when the caller's ctx cancels mid-handler but
// the wallet actor keeps processing on its own root-anchored ctx).
// The fallback is refused if the VTXO is in PendingForfeit /
// Forfeiting, since racing an Unroll in either state risks a
// double-claim of the same VTXO.
//
// Read ExitResult.Path to dispatch on which branch the daemon took;
// the remaining fields are zero-valued for paths they do not apply
// to.
func (c *Client) Exit(ctx context.Context, req ExitRequest) (*ExitResult,
	error) {

	if err := c.requireWalletRPC(); err != nil {
		return nil, err
	}
	if req.Outpoint == "" {
		return nil, fmt.Errorf("outpoint is required")
	}

	dest := strings.TrimSpace(req.Destination)
	if dest == "" {
		res, err := c.unilateralExit(ctx, req.Outpoint)
		if err != nil {
			return nil, err
		}
		res.Path = ExitPathUnilateral

		return res, nil
	}

	leaveResp, leaveErr := c.cooperativeLeave(ctx, req.Outpoint, dest)
	if leaveErr == nil {
		// H-2 guard: the daemon's LeaveVTXOs surfaces per-outpoint
		// wallet failures as log lines rather than as a top-level
		// error, returning a (possibly empty) queued set. Treat a
		// queued list that does not echo our outpoint as cooperative
		// failure and fall through to the fallback decision tree.
		queued := leaveResp.GetQueuedOutpoints()
		if outpointInList(req.Outpoint, queued) {
			return &ExitResult{
				Path:            ExitPathCooperative,
				Cooperative:     true,
				QueuedOutpoints: queued,
			}, nil
		}
		leaveErr = fmt.Errorf("%w for %s", errCooperativeEmptyQueued,
			req.Outpoint)
	}

	// H-1 triage: caller-class errors are not eligible for
	// silent fallback. Returning them verbatim keeps caller bugs
	// (typo'd address, wrong-network destination, malformed
	// outpoint) loud rather than rerouting funds to a wallet
	// script via the unilateral path.
	if !isCooperativeRetryable(leaveErr) {
		return nil, fmt.Errorf("exit: cooperative leave failed: %w",
			leaveErr)
	}

	// M-6 short-circuit: if the caller's ctx already fired,
	// the fallback would also fail against the same ctx. Return
	// the cooperative error wrapped in the ctx error so the
	// caller sees what happened without a misleading
	// "fallback failed" suffix.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("exit: cooperative leave failed (%w) "+
			"and ctx cancelled: %v", ctxErr, leaveErr)
	}

	// H-3 ListVTXOs guard: the wallet actor processes the leave
	// against its own root-anchored ctx, so a caller-side ctx
	// cancel can race the daemon's admission to completion. If
	// the VTXO is already PendingForfeit / Forfeiting the
	// cooperative leave is in flight on the operator; firing
	// Unroll on top would risk a double-claim.
	if admitted, err := c.cooperativeAttemptAdmitted(
		ctx, req.Outpoint,
	); err != nil {
		// Lookup failure is itself a transport-class problem;
		// surface it alongside the cooperative error rather
		// than silently choosing for the caller.
		return nil, fmt.Errorf("exit: cooperative leave failed (%w) "+
			"and admission probe failed: %v", leaveErr, err)
	} else if admitted {
		return nil, fmt.Errorf("exit: cooperative leave failed at the "+
			"RPC boundary but the daemon already admitted the "+
			"intent; retry the cooperative path or wait for the "+
			"round to settle: %w", leaveErr)
	}

	fallback, err := c.unilateralExit(ctx, req.Outpoint)
	if err != nil {
		return nil, fmt.Errorf("exit: cooperative leave failed (%v) "+
			"and unilateral fallback failed: %w", leaveErr, err)
	}
	fallback.Path = ExitPathUnilateralFallback
	fallback.CooperativeError = leaveErr.Error()

	return fallback, nil
}

// cooperativeLeave builds the single-outpoint LeaveVTXOs request and
// dispatches it. Factored out of Exit so the cooperative dispatch and
// the post-call triage logic read cleanly.
func (c *Client) cooperativeLeave(ctx context.Context, outpoint,
	destination string) (*daemonrpc.LeaveVTXOsResponse, error) {

	leaveReq := &daemonrpc.LeaveVTXOsRequest{
		Selection: &daemonrpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &daemonrpc.OutpointSelection{
				Outpoints: []string{
					outpoint,
				},
			},
		},
		DefaultDestination: &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_Address{
				Address: destination,
			},
		},
	}

	return c.daemon.LeaveVTXOs(ctx, leaveReq)
}

// cooperativeAttemptAdmitted reports whether the daemon's wallet
// state shows the supplied outpoint already locked into a
// cooperative leave (PendingForfeit / Forfeiting). The probe is used
// by Exit to guard the unilateral fallback against a double-claim
// race when a caller-side ctx cancel returned an error from
// LeaveVTXOs but the wallet actor kept processing on its root ctx.
func (c *Client) cooperativeAttemptAdmitted(ctx context.Context,
	outpoint string) (bool, error) {

	for _, st := range []daemonrpc.VTXOStatus{
		daemonrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT,
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITING,
	} {
		resp, err := c.daemon.ListVTXOs(
			ctx, &daemonrpc.ListVTXOsRequest{
				StatusFilter: st,
			},
		)
		if err != nil {
			return false, err
		}
		for _, vtxo := range resp.GetVtxos() {
			if vtxo.GetOutpoint() == outpoint {
				return true, nil
			}
		}
	}

	return false, nil
}

// errCooperativeEmptyQueued sentinels the H-2 case where the
// daemon's LeaveVTXOs accepted the request but returned no queued
// outpoint for the caller's target — typically a per-outpoint
// wallet failure that LeaveVTXOs logs and swallows. We treat this
// like a transport-class failure so the SDK falls back to
// unilateral exit.
var errCooperativeEmptyQueued = errors.New("cooperative leave returned no " +
	"queued outpoint")

// isCooperativeRetryable reports whether an error from the
// cooperative LeaveVTXOs RPC is eligible for the unilateral
// fallback. Only transport- or operator-availability-class codes
// qualify, plus the empty-queued sentinel; caller-side errors
// (InvalidArgument, FailedPrecondition, NotFound, PermissionDenied)
// are returned verbatim so callers see their mistakes directly
// instead of having the SDK silently route the funds via the
// unilateral path.
func isCooperativeRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errCooperativeEmptyQueued) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded,
		codes.Canceled, codes.Aborted, codes.ResourceExhausted:

		return true

	default:
		return false
	}
}

// outpointInList reports whether target is contained in the supplied
// outpoint string slice. Used by Exit's H-2 guard against the
// daemon's "queued nothing but no error" failure mode.
func outpointInList(target string, list []string) bool {
	for _, op := range list {
		if op == target {
			return true
		}
	}

	return false
}

// unilateralExit calls walletdkrpc.Exit (which proxies
// daemonrpc.Unroll). The caller sets Path to ExitPathUnilateral or
// ExitPathUnilateralFallback depending on the branch.
func (c *Client) unilateralExit(ctx context.Context, outpoint string) (
	*ExitResult, error) {

	resp, err := c.wallet.Exit(ctx, &walletdkrpc.ExitRequest{
		Outpoint: outpoint,
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
