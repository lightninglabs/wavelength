//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/credit"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/wallet"
)

// RPCServer is the narrow contract swapwallet composes against from the
// daemon. The concrete *darepod.RPCServer satisfies it; tests can supply a
// fake that exercises router/recv/history/service without bringing up the
// full daemon. The interface is intentionally minimal: every method is
// already a stable public method on *darepod.RPCServer, and the surface
// grows only as new wallet-layer capabilities are added.
//
// Create/Unlock/Exit/ExitStatus are admin-shape methods that the wallet
// service proxies to daemonrpc. They are reachable BEFORE the swap runtime
// is live — Create and Unlock obviously run before the wallet is even
// usable — so the admin handlers must never depend on Runtime state.
type RPCServer interface {
	LeaveVTXOs(ctx context.Context,
		req *daemonrpc.LeaveVTXOsRequest) (
		*daemonrpc.LeaveVTXOsResponse, error)

	SendOnChain(ctx context.Context,
		req *daemonrpc.SendOnChainRequest) (
		*daemonrpc.SendOnChainResponse, error)

	SendOOR(ctx context.Context,
		req *daemonrpc.SendOORRequest) (
		*daemonrpc.SendOORResponse,
		error,
	)

	ListVTXOs(ctx context.Context,
		req *daemonrpc.ListVTXOsRequest) (
		*daemonrpc.ListVTXOsResponse,
		error,
	)

	ListTransactions(ctx context.Context,
		req *daemonrpc.ListTransactionsRequest) (
		*daemonrpc.ListTransactionsResponse, error)

	GetInfo(ctx context.Context,
		req *daemonrpc.GetInfoRequest) (
		*daemonrpc.GetInfoResponse,
		error,
	)

	EstimateFee(ctx context.Context,
		req *daemonrpc.EstimateFeeRequest) (
		*daemonrpc.EstimateFeeResponse, error)

	GetBalance(ctx context.Context,
		req *daemonrpc.GetBalanceRequest) (
		*daemonrpc.GetBalanceResponse, error)

	NewAddress(ctx context.Context,
		req *daemonrpc.NewAddressRequest) (
		*daemonrpc.NewAddressResponse, error)

	NewWalletAddress(ctx context.Context) (string, error)

	ListWalletUnspent(ctx context.Context, minConfs,
		maxConfs int32) ([]*wallet.Utxo, error)

	GenSeed(ctx context.Context,
		req *daemonrpc.GenSeedRequest) (
		*daemonrpc.GenSeedResponse,
		error,
	)

	InitWallet(ctx context.Context,
		req *daemonrpc.InitWalletRequest) (
		*daemonrpc.InitWalletResponse, error)

	UnlockWallet(ctx context.Context,
		req *daemonrpc.UnlockWalletRequest) (
		*daemonrpc.UnlockWalletResponse, error)

	Unroll(ctx context.Context,
		req *daemonrpc.UnrollRequest) (*daemonrpc.UnrollResponse, error)

	GetUnrollStatus(ctx context.Context,
		req *daemonrpc.GetUnrollStatusRequest) (
		*daemonrpc.GetUnrollStatusResponse, error)

	ExitSummary(ctx context.Context) (*darepod.ExitSummaryResult, error)

	GetExitPlan(ctx context.Context,
		req *darepod.ExitPlanRequest) (*darepod.ExitPlanResponse, error)

	SweepWallet(ctx context.Context,
		req *darepod.SweepWalletRequest) (
		*darepod.SweepWalletResponse,
		error,
	)

	JoinNextRound(ctx context.Context,
		req *daemonrpc.JoinNextRoundRequest) (
		*daemonrpc.JoinNextRoundResponse, error)
}

// activeBoardingAddressProvider is implemented by the in-process daemon RPC
// server. It remains optional so older embeddings can retain the aggregate
// unconfirmed-deposit fallback.
type activeBoardingAddressProvider interface {
	ListActiveBoardingAddresses(ctx context.Context) ([]string, error)

	ListUnconfirmedBoardingUTXOs(ctx context.Context) (
		[]*wallet.Utxo,
		error,
	)
}

// defaultWalletDeadline caps how long wallet-local entries can remain PENDING
// before the runtime projects them as FAILED with a timeout reason. Swap rows
// use the swap FSM's own terminal state instead.
const defaultWalletDeadline = 30 * time.Minute

// defaultListLimitConst is the package-default page size used when neither
// the caller nor cfg.SwapWallet.DefaultListLimit specifies one. Exposed as
// a constant so the Deps fallback path uses the same value as the runtime
// snapshots.
const defaultListLimitConst uint32 = 100

// maxListLimitConst caps the page size a caller can request. Larger values
// are clamped to this maximum so a malformed request cannot fan out
// unbounded DB work. Exposed as a constant so override paths in Deps can
// reuse it without recomputing.
const maxListLimitConst uint32 = 1000

// defaultSubscribeBufferConst is the per-subscriber channel buffer used
// when neither the caller nor cfg.SwapWallet.SubscribeBuffer specifies one.
const defaultSubscribeBufferConst uint32 = 32

// Deps is the composition struct that wires the swapwallet subserver to
// existing daemon-owned abstractions. The fields are intentionally typed
// against pre-existing interfaces so this layer never grows a parallel
// signer, key-derivation, or coin-selection implementation.
type Deps struct {
	// SwapBackend exposes the daemon-owned swap runtime as a typed in-Go
	// handle. swapwallet drives ResumePending through it during the
	// unified resume sweep; future methods (StartPay/StartReceive in-Go)
	// will be added as the backend interface grows.
	SwapBackend darepod.SwapBackend

	// SwapService is the gRPC-shaped handle for the swap subserver. It
	// implements every swap RPC method so swapwallet can dispatch Send,
	// Recv, List, and Subscribe to the underlying swap runtime without
	// going through a gRPC dial or bufconn. The handle is reachable
	// through the same *swapClientService that publishes SwapBackend, so
	// no separate registration is required.
	SwapService swapclientrpc.SwapClientServiceServer

	// RPCServer is the daemon's core gRPC handler typed against the
	// narrow RPCServer interface above. swapwallet calls it directly
	// (in-process) for capabilities the wallet layer composes over:
	// LeaveVTXOs (cooperative-exit onchain sends), ListTransactions
	// (unified history), GetInfo / GetBalance (status), NewAddress
	// (deposit), ListVTXOs (coin selection for onchain sends).
	RPCServer RPCServer

	// CreditRegistry is the lazy reference to the credit registry actor
	// used to route credit-backed Send/Recv through the durable credit
	// subsystem. Nil when the swap runtime did not publish it, in which
	// case the router falls back to declining credit-backed sends.
	CreditRegistry actor.ActorRef[credit.CreditMsg, credit.CreditResp]

	// ChainParams is the daemon's configured Bitcoin network. Invoice
	// prepare must decode against this exact network so a cross-network
	// BOLT-11 invoice is rejected before a send intent is issued.
	ChainParams *chaincfg.Params

	// Log is the swapwallet subsystem logger; falls back to btclog.Disabled
	// when nil.
	Log btclog.Logger

	// WalletDeadline is the wallet-level deadline applied to wallet-local
	// PENDING entries. Zero falls back to defaultWalletDeadline.
	WalletDeadline time.Duration

	// DefaultListLimit overrides the default page size used by List when
	// a request omits the limit. Zero falls back to
	// defaultListLimitConst.
	DefaultListLimit uint32

	// MaxListLimit caps the page size a caller can request. Zero falls
	// back to maxListLimitConst.
	MaxListLimit uint32

	// SubscribeBuffer is the per-subscriber channel buffer used by
	// SubscribeWallet. Zero falls back to defaultSubscribeBufferConst.
	SubscribeBuffer uint32

	// ActivityStore is the canonical activity-log projector the runtime
	// writes each emitted WalletEntry through as state advances; the
	// startup backfill seeds it from the history collectors. Nil disables
	// projection.
	ActivityStore darepod.ActivityStore
}

// resolveDeadline returns the effective wallet deadline, applying the
// package default when the caller did not configure one.
func (d *Deps) resolveDeadline() time.Duration {
	if d.WalletDeadline > 0 {
		return d.WalletDeadline
	}

	return defaultWalletDeadline
}

// resolveListLimit returns the effective list limit for one call.
// req=0 falls back to either the caller-supplied default or the package
// constant. Values above the resolved maximum are clamped so a malformed
// request cannot fan out unbounded DB work.
func (d *Deps) resolveListLimit(req uint32) uint32 {
	max := d.resolveMaxListLimit()
	if req == 0 {
		def := d.DefaultListLimit
		if def == 0 {
			def = defaultListLimitConst
		}
		if def > max {
			return max
		}

		return def
	}
	if req > max {
		return max
	}

	return req
}

// resolveMaxListLimit returns the effective hard cap on a List page.
func (d *Deps) resolveMaxListLimit() uint32 {
	if d.MaxListLimit > 0 {
		return d.MaxListLimit
	}

	return maxListLimitConst
}

// resolveSubscribeBuffer returns the per-subscriber channel buffer size
// honoring the caller override, with the package default as the fallback.
func (d *Deps) resolveSubscribeBuffer() uint32 {
	if d.SubscribeBuffer > 0 {
		return d.SubscribeBuffer
	}

	return defaultSubscribeBufferConst
}

// resolveLog returns a non-nil logger, falling back to a disabled logger so
// call sites can log unconditionally without a nil guard.
func (d *Deps) resolveLog() btclog.Logger {
	if d.Log != nil {
		return d.Log
	}

	return btclog.Disabled
}

// chainParamsForWalletNetwork converts the daemon network string into the
// btcd chain parameters used by wallet-layer invoice validation.
func chainParamsForWalletNetwork(network string) (*chaincfg.Params, error) {
	switch network {
	case "mainnet", "bitcoin":
		return &chaincfg.MainNetParams, nil

	case "testnet", "testnet3":
		return &chaincfg.TestNet3Params, nil

	case "testnet4":
		return &chaincfg.TestNet4Params, nil

	case "regtest":
		return &chaincfg.RegressionNetParams, nil

	case "simnet":
		return &chaincfg.SimNetParams, nil

	case "signet":
		return &chaincfg.SigNetParams, nil

	default:
		return nil, fmt.Errorf("unknown network %q", network)
	}
}
