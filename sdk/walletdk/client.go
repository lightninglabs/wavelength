package walletdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"google.golang.org/grpc"
)

const defaultCloseTimeout = 5 * time.Second

// Client is the wallet-facing SDK handle. It is safe for concurrent use.
type Client struct {
	conn    grpc.ClientConnInterface
	daemon  daemonrpc.DaemonServiceClient
	swaps   swapclientrpc.SwapClientServiceClient
	canSwap bool

	waitCh <-chan error

	closeFn   func(context.Context) error
	closeOnce sync.Once
	closeErr  error
}

// Stop shuts down the embedded daemon and releases the private transport.
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
		WalletReady:     resp.GetWalletReady(),
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

// ListBalance returns the wallet's simplified balance buckets.
func (c *Client) ListBalance(ctx context.Context) (*Balance, error) {
	resp, err := c.daemon.GetBalance(ctx, &daemonrpc.GetBalanceRequest{})
	if err != nil {
		return nil, fmt.Errorf("get wallet balance: %w", err)
	}

	return &Balance{
		BoardingConfirmedSat:      resp.GetBoardingConfirmedSat(),
		BoardingUnconfirmedSat:    resp.GetBoardingUnconfirmedSat(),
		VTXOBalanceSat:            resp.GetVtxoBalanceSat(),
		TotalConfirmedSat:         resp.GetTotalConfirmedSat(),
		OnchainWalletConfirmedSat: resp.GetOnchainWalletConfirmedSat(),
	}, nil
}

// GetOnchainAddress allocates a fresh onboarding address.
func (c *Client) GetOnchainAddress(ctx context.Context) (*OnchainAddress,
	error) {

	resp, err := c.daemon.NewAddress(ctx, &daemonrpc.NewAddressRequest{})
	if err != nil {
		return nil, fmt.Errorf("get on-chain address: %w", err)
	}

	return &OnchainAddress{Address: resp.GetAddress()}, nil
}

// Receive starts a Lightning-to-Ark receive swap in the embedded daemon.
func (c *Client) Receive(ctx context.Context, req ReceiveRequest) (
	*ReceiveResult, error) {

	if err := c.requireSwaps(); err != nil {
		return nil, err
	}
	if req.AmountSat <= 0 {
		return nil, fmt.Errorf("amount_sat must be positive")
	}

	resp, err := c.swaps.StartReceive(ctx,
		&swapclientrpc.StartReceiveRequest{
			AmountSat: req.AmountSat,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("start receive swap: %w", err)
	}

	return &ReceiveResult{
		PaymentHash: resp.GetPaymentHash(),
		Invoice:     resp.GetInvoice(),
		Swap:        swapSummaryFromProto(resp.GetSwap()),
	}, nil
}

// Send starts an Ark-to-Lightning payment in the embedded daemon.
func (c *Client) Send(ctx context.Context, req SendRequest) (*SendResult,
	error) {

	if err := c.requireSwaps(); err != nil {
		return nil, err
	}
	if req.Invoice == "" {
		return nil, fmt.Errorf("invoice is required")
	}

	resp, err := c.swaps.StartPay(ctx, &swapclientrpc.StartPayRequest{
		Invoice:   req.Invoice,
		MaxFeeSat: req.MaxFeeSat,
	})
	if err != nil {
		return nil, fmt.Errorf("start pay swap: %w", err)
	}

	return &SendResult{
		PaymentHash: resp.GetPaymentHash(),
		Swap:        swapSummaryFromProto(resp.GetSwap()),
	}, nil
}

// ListSwaps returns persisted daemon-owned swap summaries.
func (c *Client) ListSwaps(ctx context.Context, req ListSwapsRequest) (
	[]SwapSummary, error) {

	if err := c.requireSwaps(); err != nil {
		return nil, err
	}

	resp, err := c.swaps.ListSwaps(ctx, &swapclientrpc.ListSwapsRequest{
		PendingOnly: req.PendingOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("list swaps: %w", err)
	}

	swaps := make([]SwapSummary, 0, len(resp.GetSwaps()))
	for _, swap := range resp.GetSwaps() {
		swaps = append(swaps, swapSummaryFromProto(swap))
	}

	return swaps, nil
}

// GetSwap returns one persisted swap summary.
func (c *Client) GetSwap(ctx context.Context, req GetSwapRequest) (*SwapSummary,
	error) {

	if err := c.requireSwaps(); err != nil {
		return nil, err
	}
	if req.PaymentHash == "" {
		return nil, fmt.Errorf("payment_hash is required")
	}

	resp, err := c.swaps.GetSwap(ctx, &swapclientrpc.GetSwapRequest{
		PaymentHash: req.PaymentHash,
	})
	if err != nil {
		return nil, fmt.Errorf("get swap: %w", err)
	}

	summary := swapSummaryFromProto(resp.GetSwap())

	return &summary, nil
}

// ResumeSwap asks the daemon to wake one persisted pending swap.
func (c *Client) ResumeSwap(ctx context.Context, req ResumeSwapRequest) (
	*SwapSummary, error) {

	if err := c.requireSwaps(); err != nil {
		return nil, err
	}
	if req.PaymentHash == "" {
		return nil, fmt.Errorf("payment_hash is required")
	}

	resp, err := c.swaps.ResumeSwap(ctx, &swapclientrpc.ResumeSwapRequest{
		PaymentHash: req.PaymentHash,
		Direction:   swapDirectionToProto(req.Direction),
	})
	if err != nil {
		return nil, fmt.Errorf("resume swap: %w", err)
	}

	summary := swapSummaryFromProto(resp.GetSwap())

	return &summary, nil
}

// SubscribeSwaps streams daemon-owned swap summary updates until ctx ends.
func (c *Client) SubscribeSwaps(ctx context.Context,
	req SubscribeSwapsRequest) (<-chan SwapSummary, <-chan error, error) {

	if err := c.requireSwaps(); err != nil {
		return nil, nil, err
	}

	stream, err := c.swaps.SubscribeSwaps(
		ctx, &swapclientrpc.SubscribeSwapsRequest{
			IncludeExisting: req.IncludeExisting,
			PendingOnly:     req.PendingOnly,
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("subscribe swaps: %w", err)
	}

	updates := make(chan SwapSummary)
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

				errs <- fmt.Errorf("receive swap update: %w",
					err)

				return
			}

			select {
			case updates <- swapSummaryFromProto(resp.GetSwap()):
			case <-ctx.Done():
				errs <- fmt.Errorf("swap subscription "+
					"closed: %w", ctx.Err())

				return
			}
		}
	}()

	return updates, errs, nil
}

// requireSwaps fails fast when the build does not include the daemon-owned
// swap executor, giving embedders a stable error before any RPC is attempted.
func (c *Client) requireSwaps() error {
	if c == nil || !c.canSwap {
		return ErrSwapRuntimeUnavailable
	}

	return nil
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
