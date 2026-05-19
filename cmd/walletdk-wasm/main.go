//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"syscall/js"
	"time"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/sdk/walletdk"
)

var walletRuntime = &runtimeState{}

type runtimeState struct {
	mu     sync.Mutex
	client *walletdk.Client
}

// main installs the browser RPC function and then parks the Go runtime.
func main() {
	js.Global().Set("walletdkCall", js.FuncOf(walletCall))
	js.Global().Call("dispatchEvent", js.Global().Get("CustomEvent").New(
		"walletdk-ready",
	))

	select {}
}

// walletCall dispatches one asynchronous UI request into walletdk.
func walletCall(_ js.Value, args []js.Value) any {
	if len(args) == 0 {
		return promise(func() (any, error) {
			return nil, fmt.Errorf("method is required")
		})
	}

	method := args[0].String()
	var req js.Value
	if len(args) > 1 {
		req = args[1]
	}

	return promise(func() (any, error) {
		ctx, cancel := context.WithTimeout(
			context.Background(), 60*time.Second,
		)
		defer cancel()

		return walletRuntime.call(ctx, method, req)
	})
}

// call executes one wallet operation against the current runtime state.
func (r *runtimeState) call(ctx context.Context, method string, req js.Value) (
	any, error) {

	switch method {
	case "start":
		return r.start(ctx, req)

	case "stop":
		return r.stop()

	case "getInfo":
		client, err := r.currentClient()
		if err != nil {
			return nil, err
		}

		info, err := client.GetInfo(ctx)
		if err != nil {
			return nil, err
		}

		return legacyInfo(info), nil

	case "createWallet":
		client, err := r.currentClient()
		if err != nil {
			return nil, err
		}

		result, err := client.CreateWallet(ctx, walletdk.CreateWalletRequest{
			Mnemonic: stringSlice(req.Get("mnemonic")),
			SeedPassphrase: bytesFromString(
				req.Get("seedPassphrase"),
			),
			WalletPassword: bytesFromString(req.Get("password")),
		})
		if err != nil {
			return nil, err
		}
		if err := waitWalletUsable(ctx, client); err != nil {
			return nil, err
		}

		return result, nil

	case "unlockWallet":
		client, err := r.currentClient()
		if err != nil {
			return nil, err
		}

		result, err := client.UnlockWallet(ctx, walletdk.UnlockWalletRequest{
			WalletPassword: bytesFromString(req.Get("password")),
		})
		if err != nil {
			return nil, err
		}
		if err := waitWalletUsable(ctx, client); err != nil {
			return nil, err
		}

		return result, nil

	case "listBalance":
		client, err := r.currentClient()
		if err != nil {
			return nil, err
		}

		balance, err := client.Balance(ctx)
		if err != nil {
			return nil, err
		}

		return legacyBalance(balance), nil

	case "getOnchainAddress":
		client, err := r.currentClient()
		if err != nil {
			return nil, err
		}

		return retryWalletCall(ctx, func() (*walletdk.DepositResult, error) {
			return client.Deposit(ctx, walletdk.DepositRequest{})
		})

	case "board":
		client, err := r.currentClient()
		if err != nil {
			return nil, err
		}

		return client.ArkRPC().Board(ctx, &daemonrpc.BoardRequest{})

	case "getRawBalance":
		client, err := r.currentClient()
		if err != nil {
			return nil, err
		}

		balance, err := client.ArkRPC().GetBalance(
			ctx, &daemonrpc.GetBalanceRequest{},
		)
		if err != nil {
			return nil, err
		}

		return legacyRawBalance(balance), nil

	case "receive":
		client, err := r.currentClient()
		if err != nil {
			return nil, err
		}

		result, err := client.Receive(ctx, walletdk.ReceiveRequest{
			AmountSat: uint64(req.Get("amountSat").Int()),
			Memo:      stringValue(req.Get("memo")),
		})
		if err != nil {
			return nil, err
		}

		return legacyReceive(result), nil

	case "send":
		client, err := r.currentClient()
		if err != nil {
			return nil, err
		}

		result, err := client.Send(ctx, walletdk.SendRequest{
			Invoice:        stringValue(req.Get("invoice")),
			OnchainAddress: stringValue(req.Get("onchainAddress")),
			AmountSat:      uint64Value(req.Get("amountSat")),
			MaxFeeSat:      uint64Value(req.Get("maxFeeSat")),
		})
		if err != nil {
			return nil, err
		}

		return legacySend(result), nil

	case "listSwaps":
		client, err := r.currentClient()
		if err != nil {
			return nil, err
		}

		result, err := client.List(ctx, walletdk.ListRequest{
			PendingOnly: boolValue(req.Get("pendingOnly")),
			View:        walletdk.ListViewActivity,
		})
		if err != nil {
			return nil, err
		}

		return legacyActivityRows(result), nil

	case "getSwap":
		client, err := r.currentClient()
		if err != nil {
			return nil, err
		}

		resp, err := client.SwapRPC().GetSwap(ctx,
			&swapclientrpc.GetSwapRequest{
				PaymentHash: stringValue(req.Get("paymentHash")),
			},
		)
		if err != nil {
			return nil, err
		}

		return legacySwapSummary(resp.GetSwap()), nil

	case "resumeSwap":
		client, err := r.currentClient()
		if err != nil {
			return nil, err
		}

		resp, err := client.SwapRPC().ResumeSwap(ctx,
			&swapclientrpc.ResumeSwapRequest{
				Direction: swapDirection(
					stringValue(req.Get("direction")),
				),
				PaymentHash: stringValue(req.Get("paymentHash")),
			},
		)
		if err != nil {
			return nil, err
		}

		return legacySwapSummary(resp.GetSwap()), nil

	case "listRawSwaps":
		client, err := r.currentClient()
		if err != nil {
			return nil, err
		}

		resp, err := client.SwapRPC().ListSwaps(ctx,
			&swapclientrpc.ListSwapsRequest{
				PendingOnly: boolValue(req.Get("pendingOnly")),
			},
		)
		if err != nil {
			return nil, err
		}

		rows := make([]map[string]any, 0, len(resp.GetSwaps()))
		for _, swap := range resp.GetSwaps() {
			rows = append(rows, legacySwapSummary(swap))
		}

		return rows, nil

	case "subscribeActivity":
		return nil, fmt.Errorf("activity subscriptions are not exposed " +
			"by this demo bridge yet")

	case "resumeActivity":
		return nil, fmt.Errorf("activity resume is not exposed by this " +
			"demo bridge")

	default:
		return nil, fmt.Errorf("unknown walletdk method %q", method)
	}
}

func legacyBalance(balance *walletdk.Balance) map[string]int64 {
	if balance == nil {
		return map[string]int64{}
	}

	return map[string]int64{
		"BoardingConfirmedSat":      balance.ConfirmedSat,
		"BoardingUnconfirmedSat":    balance.PendingInSat,
		"VTXOBalanceSat":            balance.ConfirmedSat,
		"TotalConfirmedSat":         balance.ConfirmedSat,
		"OnchainWalletConfirmedSat": balance.ConfirmedSat,
	}
}

func legacyRawBalance(balance *daemonrpc.GetBalanceResponse) map[string]int64 {
	if balance == nil {
		return map[string]int64{}
	}

	return map[string]int64{
		"BoardingConfirmedSat":      balance.GetBoardingConfirmedSat(),
		"BoardingUnconfirmedSat":    balance.GetBoardingUnconfirmedSat(),
		"VtxoBalanceSat":            balance.GetVtxoBalanceSat(),
		"VTXOBalanceSat":            balance.GetVtxoBalanceSat(),
		"TotalConfirmedSat":         balance.GetTotalConfirmedSat(),
		"OnchainWalletConfirmedSat": balance.GetOnchainWalletConfirmedSat(),
		"BoardingPendingSweepSat":   balance.GetBoardingPendingSweepSat(),
		"BoardingSweptSat":          balance.GetBoardingSweptSat(),
	}
}

func legacyInfo(info *walletdk.Info) map[string]any {
	if info == nil {
		return map[string]any{}
	}

	return map[string]any{
		"Version":         info.Version,
		"Commit":          info.Commit,
		"Network":         info.Network,
		"BlockHeight":     info.BlockHeight,
		"ServerConnected": info.ServerConnected,
		"WalletType":      info.WalletType,
		"WalletState":     info.WalletState,
		"WalletReady":     info.WalletReady(),
		"IdentityPubKey":  info.IdentityPubKey,
	}
}

func legacyReceive(result *walletdk.ReceiveResult) map[string]any {
	if result == nil {
		return map[string]any{}
	}

	return map[string]any{
		"Invoice":     result.Invoice,
		"PaymentHash": result.Entry.ID,
		"Entry":       result.Entry,
	}
}

func legacySend(result *walletdk.SendResult) map[string]any {
	if result == nil {
		return map[string]any{}
	}

	return map[string]any{
		"PaymentHash":     result.Entry.ID,
		"Entry":           result.Entry,
		"ActualAmountSat": result.ActualAmountSat,
	}
}

func legacyActivityRows(result *walletdk.ListResult) []map[string]any {
	if result == nil || result.Activity == nil {
		return nil
	}

	rows := make([]map[string]any, 0, len(result.Activity.Entries))
	for _, entry := range result.Activity.Entries {
		rows = append(rows, map[string]any{
			"Direction":   entry.Kind,
			"State":       entry.Status,
			"AmountSat":   entry.AmountSat,
			"PaymentHash": entry.ID,
		})
	}

	return rows
}

func legacySwapSummary(swap *swapclientrpc.SwapSummary) map[string]any {
	if swap == nil {
		return map[string]any{}
	}

	return map[string]any{
		"Direction":   swap.GetDirection().String(),
		"State":       swap.GetState().String(),
		"Pending":     swap.GetPending(),
		"AmountSat":   swap.GetAmountSat(),
		"PaymentHash": swap.GetPaymentHash(),
	}
}

func swapDirection(direction string) swapclientrpc.SwapDirection {
	switch direction {
	case "pay", "PAY", "SWAP_DIRECTION_PAY":
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY

	case "receive", "RECEIVE", "SWAP_DIRECTION_RECEIVE":
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE

	default:
		return swapclientrpc.SwapDirection_SWAP_DIRECTION_UNSPECIFIED
	}
}

func waitWalletUsable(ctx context.Context, client *walletdk.Client) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		status, err := client.Status(ctx)
		if err == nil && status.Ready {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for wallet RPC readiness: %w",
				ctx.Err())

		case <-ticker.C:
		}
	}
}

func retryWalletCall[T any](ctx context.Context, fn func() (T, error)) (T,
	error) {

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	var zero T
	var lastErr error
	for {
		result, err := fn()
		if err == nil {
			return result, nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return zero, fmt.Errorf("%w: %v", ctx.Err(), lastErr)

		case <-ticker.C:
		}
	}
}

// start creates the embedded wallet runtime from browser-supplied config.
func (r *runtimeState) start(ctx context.Context, req js.Value) (any, error) {

	cfg := walletdk.DefaultConfig()
	cfg.Network = stringDefault(req.Get("network"), "regtest")
	cfg.DataDir = stringDefault(req.Get("dataDir"), "/walletdk-demo")
	cfg.DebugLevel = stringDefault(req.Get("debugLevel"), "info")
	cfg.WalletType = stringDefault(req.Get("walletType"), "lwwallet")
	cfg.WalletEsploraURL = stringValue(req.Get("walletEsploraURL"))
	cfg.ServerInsecure = boolValue(req.Get("serverInsecure"))
	cfg.SwapServerInsecure = boolValue(req.Get("swapServerInsecure"))
	cfg.DisableSwaps = boolValue(req.Get("disableSwaps"))
	cfg.SwapDatabaseFileName = stringDefault(
		req.Get("swapDatabaseFileName"),
		"/walletdk-swaps.db",
	)

	arkGatewayURL := stringValue(req.Get("arkGatewayURL"))
	if arkGatewayURL != "" {
		cfg.ServerAddress = arkGatewayURL
		cfg.ServerTransport = walletdk.TransportREST
		cfg.ServerInsecure = true
	} else {
		cfg.ServerAddress = stringValue(req.Get("serverAddress"))
	}

	swapGatewayURL := stringValue(req.Get("swapServerGatewayURL"))
	if swapGatewayURL != "" {
		cfg.SwapServerAddress = swapGatewayURL
		cfg.SwapServerTransport = walletdk.TransportREST
		cfg.SwapServerInsecure = true
	} else {
		cfg.SwapServerAddress = stringValue(req.Get("swapServerAddress"))
	}

	client, err := walletdk.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	oldClient := r.client
	r.client = client
	r.mu.Unlock()

	if oldClient != nil {
		_ = oldClient.Stop()
	}

	info, err := client.GetInfo(ctx)
	if err != nil {
		return nil, err
	}

	return legacyInfo(info), nil
}

// stop shuts down the current embedded wallet runtime.
func (r *runtimeState) stop() (map[string]bool, error) {
	r.mu.Lock()
	client := r.client
	r.client = nil
	r.mu.Unlock()

	if client != nil {
		if err := client.Stop(); err != nil {
			return nil, err
		}
	}

	return map[string]bool{"stopped": true}, nil
}

// currentClient returns the active runtime client.
func (r *runtimeState) currentClient() (*walletdk.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.client == nil {
		return nil, fmt.Errorf("wallet runtime is not started")
	}

	return r.client, nil
}

// promise adapts a Go async operation to a JavaScript Promise.
func promise(fn func() (any, error)) js.Value {
	handler := js.FuncOf(func(_ js.Value, args []js.Value) any {
		resolve := args[0]
		reject := args[1]

		go func() {
			result, err := fn()
			if err != nil {
				reject.Invoke(err.Error())

				return
			}

			resolve.Invoke(toJS(result))
		}()

		return nil
	})

	return js.Global().Get("Promise").New(handler)
}

// toJS converts a Go result to a JavaScript value through JSON.
func toJS(value any) js.Value {
	if value == nil {
		return js.Null()
	}

	data, err := json.Marshal(value)
	if err != nil {
		return js.ValueOf(map[string]any{
			"marshal_error": err.Error(),
		})
	}

	return js.Global().Get("JSON").Call("parse", string(data))
}

// stringValue extracts a JavaScript string.
func stringValue(value js.Value) string {
	if value.IsUndefined() || value.IsNull() {
		return ""
	}

	return value.String()
}

// stringDefault extracts a JavaScript string or returns a fallback.
func stringDefault(value js.Value, fallback string) string {
	if str := stringValue(value); str != "" {
		return str
	}

	return fallback
}

// boolValue extracts a JavaScript boolean.
func boolValue(value js.Value) bool {
	if value.IsUndefined() || value.IsNull() {
		return false
	}

	return value.Bool()
}

// uint64Value extracts a JavaScript number, treating missing fields as zero.
func uint64Value(value js.Value) uint64 {
	if value.IsUndefined() || value.IsNull() {
		return 0
	}

	return uint64(value.Int())
}

// bytesFromString extracts a JavaScript string as bytes.
func bytesFromString(value js.Value) []byte {
	if value.IsUndefined() || value.IsNull() {
		return nil
	}

	return []byte(value.String())
}

// stringSlice extracts a JavaScript string array.
func stringSlice(value js.Value) []string {
	if value.IsUndefined() || value.IsNull() {
		return nil
	}

	length := value.Length()
	result := make([]string, 0, length)
	for i := 0; i < length; i++ {
		result = append(result, value.Index(i).String())
	}

	return result
}
