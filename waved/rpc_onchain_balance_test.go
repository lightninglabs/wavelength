package waved

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/btcwbackend"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// accountBalanceClient models LND's empty-account aggregation and exposes the
// authenticated raw client used by account-scoped reporting.
type accountBalanceClient struct {
	lndclient.LightningClient
	raw     *accountBalanceRPC
	timeout time.Duration
}

// RawClientWithMacAuth preserves the caller context and adds test credentials.
func (c *accountBalanceClient) RawClientWithMacAuth(ctx context.Context) (
	context.Context, time.Duration, lnrpc.LightningClient) {

	return metadata.AppendToOutgoingContext(
			ctx, "macaroon", "test-macaroon",
		),
		c.timeout, c.raw
}

// WalletBalance reproduces the pinned lndclient's node-wide request.
func (c *accountBalanceClient) WalletBalance(ctx context.Context) (
	*lndclient.WalletBalance, error) {

	ctx, timeout, raw := c.RawClientWithMacAuth(ctx)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := raw.WalletBalance(ctx, &lnrpc.WalletBalanceRequest{})
	if err != nil {
		return nil, err
	}

	return &lndclient.WalletBalance{
		Confirmed:   btcutil.Amount(resp.ConfirmedBalance),
		Unconfirmed: btcutil.Amount(resp.UnconfirmedBalance),
	}, nil
}

// accountBalanceRPC returns per-account funds, or their sum for an empty
// filter.
type accountBalanceRPC struct {
	lnrpc.LightningClient
	t        *testing.T
	balances map[string]*lnrpc.WalletBalanceResponse
	err      error
	wait     bool
	contexts []context.Context
}

// WalletBalance checks authentication and deadlines at the transport boundary.
func (c *accountBalanceRPC) WalletBalance(ctx context.Context,
	req *lnrpc.WalletBalanceRequest, _ ...grpc.CallOption) (
	*lnrpc.WalletBalanceResponse, error) {

	md, ok := metadata.FromOutgoingContext(ctx)
	require.True(c.t, ok)
	require.Equal(c.t, []string{"test-macaroon"}, md.Get("macaroon"))
	_, ok = ctx.Deadline()
	require.True(c.t, ok, "raw RPC must have a deadline")
	c.contexts = append(c.contexts, ctx)
	if c.wait {
		<-ctx.Done()

		return nil, ctx.Err()
	}
	if c.err != nil {
		return nil, c.err
	}
	result := &lnrpc.WalletBalanceResponse{}
	for account, balance := range c.balances {
		if req.Account == "" || req.Account == account {
			result.ConfirmedBalance += balance.ConfirmedBalance
			result.UnconfirmedBalance += balance.UnconfirmedBalance
		}
	}
	if req.Account != "" && c.balances[req.Account] == nil {
		return nil, status.Error(codes.NotFound, "account not found")
	}

	return result, nil
}

// newBalanceRPCServer wires the real balance handler to an empty boarding
// store.
func newBalanceRPCServer(t *testing.T) *RPCServer {
	t.Helper()
	ready := make(chan struct{})
	close(ready)

	return NewRPCServer(&Server{
		cfg:         &Config{},
		walletReady: ready,
		db:          db.NewTestSqliteDB(t),
		chainParams: &chaincfg.RegressionNetParams,
		clk:         clock.NewDefaultClock(),
	})
}

// TestLNDAccountBalanceReporting proves both reporting surfaces exclude funds
// in other accounts, including when an empty config selects the default
// account.
func TestLNDAccountBalanceReporting(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		config                 *LndConfig
		confirmed, unconfirmed int64
	}{
		{
			name:        "nil config",
			confirmed:   100_000,
			unconfirmed: 1_000,
		},
		{
			name:        "empty config",
			config:      &LndConfig{},
			confirmed:   100_000,
			unconfirmed: 1_000,
		},
		{
			name: "explicit default",
			config: &LndConfig{
				Account: "default",
			},
			confirmed:   100_000,
			unconfirmed: 1_000,
		},
		{
			name: "configured account",
			config: &LndConfig{
				Account: "tenant",
			},
			confirmed:   250_000,
			unconfirmed: 2_000,
		},
		{
			name: "empty account",
			config: &LndConfig{
				Account: "empty",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			balances := map[string]*lnrpc.WalletBalanceResponse{
				"default": {
					ConfirmedBalance:   100_000,
					UnconfirmedBalance: 1_000,
				},
				"tenant": {
					ConfirmedBalance:   250_000,
					UnconfirmedBalance: 2_000,
				},
				"empty": {},
			}
			raw := &accountBalanceRPC{t: t, balances: balances}
			client := &accountBalanceClient{
				raw:     raw,
				timeout: time.Second,
			}
			rpc := newBalanceRPCServer(t)
			rpc.server.cfg.Lnd = tc.config
			rpc.server.lnd = fn.Some(
				&lndclient.GrpcLndServices{
					LndServices: lndclient.LndServices{
						Client: client,
						WalletKit: &balanceWalletKit{
							t: t,
						},
					},
				},
			)
			// The unscoped negative control must include both
			// funded accounts.
			node, err := client.WalletBalance(t.Context())
			require.NoError(t, err)
			require.EqualValues(t, 350_000, node.Confirmed)
			resp, err := rpc.GetBalance(
				t.Context(), &waverpc.GetBalanceRequest{},
			)
			require.NoError(t, err)
			require.Equal(
				t, tc.confirmed, resp.OnchainWalletConfirmedSat,
			)
			confirmed, unconfirmed, err := rpc.metricsWalletBalance(
				t.Context(),
			)
			require.NoError(t, err)
			require.Equal(t, tc.confirmed, confirmed)
			require.Equal(t, tc.unconfirmed, unconfirmed)
			for _, ctx := range raw.contexts {
				require.ErrorIs(t, ctx.Err(), context.Canceled)
			}
		})
	}
}

// TestLNDAccountBalanceErrors proves failed or canceled reads cannot publish
// a successful zero balance through either RPC or metrics.
func TestLNDAccountBalanceErrors(t *testing.T) {
	boom := errors.New("balance unavailable")
	for _, tc := range []struct {
		name         string
		err          error
		wait, cancel bool
		timeout      time.Duration
	}{
		{
			name:    "RPC error",
			err:     boom,
			timeout: time.Second,
		},
		{
			name:    "missing account",
			timeout: time.Second,
		},
		{
			name:    "RPC timeout",
			wait:    true,
			timeout: time.Millisecond,
		},
		{
			name:    "caller canceled",
			wait:    true,
			cancel:  true,
			timeout: time.Hour,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := &accountBalanceRPC{
				t:    t,
				err:  tc.err,
				wait: tc.wait,
			}
			rpc := newBalanceRPCServer(t)
			rpc.server.cfg.Lnd = &LndConfig{Account: "missing"}
			rpc.server.lnd = fn.Some(
				&lndclient.GrpcLndServices{
					LndServices: lndclient.LndServices{
						Client: &accountBalanceClient{
							raw:     raw,
							timeout: tc.timeout,
						},
						WalletKit: &balanceWalletKit{
							t: t,
						},
					},
				},
			)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			// Exercise the exact fetcher used by GetBalance without
			// letting a canceled context stop first at its earlier
			// database queries.
			balance, err := sumOnchainWalletConfirmed(
				ctx, rpc.walletBalanceFetchers(),
			)
			require.Error(t, err)
			require.Zero(t, balance)
			confirmed, unconfirmed, err := rpc.metricsWalletBalance(
				ctx,
			)
			require.Error(t, err)
			require.Zero(t, confirmed)
			require.Zero(t, unconfirmed)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
			}
			if tc.wait {
				want := context.DeadlineExceeded
				if tc.cancel {
					want = context.Canceled
				}
				require.ErrorIs(t, err, want)
			}
			if !tc.cancel {
				resp, err := rpc.GetBalance(
					ctx, &waverpc.GetBalanceRequest{},
				)
				require.Equal(
					t, codes.Internal, status.Code(err),
				)
				require.Nil(t, resp)
			}
		})
	}
}

// TestLocalWalletBalanceReporting preserves RPC/metrics parity for both local
// backends using their shared real btcwallet balance implementation.
func TestLocalWalletBalanceReporting(t *testing.T) {
	w := newFundedLwWallet(t)
	addr, err := w.NewAddress(t.Context())
	require.NoError(t, err)
	script, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)
	fundConfirmedUTXO(t, w, script)
	for _, backend := range []string{"lwwallet", "btcwallet"} {
		t.Run(backend, func(t *testing.T) {
			rpc := newBalanceRPCServer(t)
			// An LND setting must have no effect on either local
			// backend.
			rpc.server.cfg.Lnd = &LndConfig{Account: "unrelated"}
			if backend == "lwwallet" {
				rpc.server.lwWallet = fn.Some(w)
			} else {
				rpc.server.btcwWallet = fn.Some(
					&btcwbackend.Wallet{
						Wallet: w.Wallet,
					},
				)
			}
			balance, err := sumOnchainWalletConfirmed(
				t.Context(), rpc.walletBalanceFetchers(),
			)
			require.NoError(t, err)
			require.EqualValues(t, 1_000_000, balance)
			confirmed, unconfirmed, err := rpc.metricsWalletBalance(
				t.Context(),
			)
			require.NoError(t, err)
			require.EqualValues(t, balance, confirmed)
			require.Zero(t, unconfirmed)
		})
	}
}

// balanceWalletKit keeps boarding observation unscoped while balance reporting
// uses an explicit account filter.
type balanceWalletKit struct {
	lndclient.WalletKitClient
	t *testing.T
}

// ListUnspent asserts observation still spans imported boarding scripts.
func (w *balanceWalletKit) ListUnspent(_ context.Context, _, _ int32,
	opts ...lndclient.ListUnspentOption) ([]*lnwallet.Utxo, error) {

	require.Empty(w.t, opts)

	return nil, nil
}
