# walletdk Integration Guide

`walletdk` is the wallet-facing SDK for applications that want a small,
stable API over the embedded `darepod` client daemon. It starts the daemon
in-process, connects to it over a private `bufconn` gRPC transport, and exposes
wallet-shaped methods for onboarding, balances, Lightning receives, Lightning
sends, and swap accounting.

The package is intended to be the layer that app developers wrap for mobile,
desktop, React Native, gomobile, or host-language bridges. Application code
should call `walletdk` rather than reimplement daemon RPC wiring or swap state
machines.

## Package

```go
import "github.com/lightninglabs/darepo-client/sdk/walletdk"
```

Swap send and receive support requires the `swapruntime` build tag:

```sh
go build -tags swapruntime ./cmd/your-wallet
go test -tags swapruntime ./sdk/walletdk
```

Without that tag, wallet bootstrapping and Ark daemon RPC wrappers still build,
but `Receive`, `Send`, `ListSwaps`, `GetSwap`, `ResumeSwap`, and
`SubscribeSwaps` fail with `walletdk.ErrSwapRuntimeUnavailable`.

## Runtime Model

`walletdk.Start` owns a full embedded daemon runtime:

1. Build a `walletdk.Config`.
2. Start the embedded daemon with `walletdk.Start`.
3. Create or unlock the wallet.
4. Poll or subscribe to wallet and swap state.
5. Call `Stop` when the host app shuts down.

The returned `*walletdk.Client` is safe for concurrent use. The client owns
daemon shutdown, private gRPC connection shutdown, and the swap RPC clients
registered on the embedded daemon.

## Minimal Startup

```go
package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/lightninglabs/darepo-client/sdk/walletdk"
	"github.com/lightningnetwork/lnd/fn/v2"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := walletdk.DefaultConfig()
	cfg.DataDir = "/tmp/example-wallet"
	cfg.Network = "regtest"
	cfg.ServerAddress = "127.0.0.1:10010"
	cfg.ServerInsecure = fn.Some(true)
	cfg.WalletType = "lwwallet"
	cfg.WalletEsploraURL = "http://127.0.0.1:3002"
	cfg.SwapServerAddress = "127.0.0.1:11010"
	cfg.SwapServerInsecure = fn.Some(true)
	cfg.LogWriter = io.Discard

	client, err := walletdk.Start(ctx, cfg)
	if err != nil {
		panic(err)
	}
	defer client.Stop()

	info, err := client.GetInfo(ctx)
	if err != nil {
		panic(err)
	}

	fmt.Printf("network=%s wallet_ready=%v\n",
		info.Network, info.WalletReady)
}
```

Use a durable app-specific `DataDir`. It contains wallet state, daemon state,
and swap accounting. Deleting it deletes the local wallet database and local
swap history.

## Create Or Unlock

Most apps should treat wallet bootstrap as a two-branch flow:

1. Start `walletdk`.
2. Call `GetInfo`.
3. If `WalletReady` is false and this is first run, call `CreateWallet`.
4. If a wallet already exists but is locked, call `UnlockWallet`.
5. Persist only the user-approved backup material or password material your
   product is designed to own.

```go
password := []byte("correct horse battery staple")

created, err := client.CreateWallet(ctx, walletdk.CreateWalletRequest{
	WalletPassword: password,
})
if err != nil {
	panic(err)
}

fmt.Println("identity:", created.IdentityPubKey)
fmt.Println("mnemonic:", created.Mnemonic)
```

For an existing wallet:

```go
unlocked, err := client.UnlockWallet(ctx, walletdk.UnlockWalletRequest{
	WalletPassword: password,
})
if err != nil {
	panic(err)
}

fmt.Println("identity:", unlocked.IdentityPubKey)
```

`CreateWallet` returns the mnemonic when it generated a new seed. Show it once,
require the user to back it up, and avoid writing it to logs.

## Wallet Operations

Fetch a balance:

```go
balance, err := client.ListBalance(ctx)
if err != nil {
	panic(err)
}

fmt.Println("confirmed:", balance.TotalConfirmedSat)
fmt.Println("ark vtxo:", balance.VTXOBalanceSat)
```

Allocate an onboarding address:

```go
addr, err := client.GetOnchainAddress(ctx)
if err != nil {
	panic(err)
}

fmt.Println(addr.Address)
```

Start a Lightning-to-Ark receive:

```go
receive, err := client.Receive(ctx, walletdk.ReceiveRequest{
	AmountSat: 50_000,
})
if err != nil {
	panic(err)
}

fmt.Println("invoice:", receive.Invoice)
fmt.Println("payment hash:", receive.PaymentHash)
fmt.Println("initial state:", receive.Swap.State)
```

Start an Ark-to-Lightning send:

```go
send, err := client.Send(ctx, walletdk.SendRequest{
	Invoice:   bolt11Invoice,
	MaxFeeSat: 1_000,
})
if err != nil {
	panic(err)
}

fmt.Println("payment hash:", send.PaymentHash)
fmt.Println("initial state:", send.Swap.State)
```

## Swap Accounting

Use `ListSwaps` for the durable accounting view:

```go
swaps, err := client.ListSwaps(ctx, walletdk.ListSwapsRequest{
	PendingOnly: false,
})
if err != nil {
	panic(err)
}

for _, swap := range swaps {
	fmt.Println(swap.Direction, swap.PaymentHash, swap.State, swap.Pending)
}
```

Use `SubscribeSwaps` to drive live UI updates:

```go
subCtx, stopSub := context.WithCancel(context.Background())
defer stopSub()

updates, errs, err := client.SubscribeSwaps(
	subCtx, walletdk.SubscribeSwapsRequest{
		IncludeExisting: true,
		PendingOnly:     false,
	},
)
if err != nil {
	panic(err)
}

go func() {
	for {
		select {
		case update, ok := <-updates:
			if !ok {
				return
			}

			fmt.Println("swap update:", update.PaymentHash, update.State)

		case err, ok := <-errs:
			if ok && err != nil {
				fmt.Println("swap subscription error:", err)
			}

			return
		}
	}
}()
```

After startup, applications can call `ResumeSwap` for pending swaps or rely on
future app-specific startup logic to resume all pending work. Keep the durable
swap list as the UI source of truth instead of inventing a second local payment
state model.

## Shutdown

Always stop the SDK on application shutdown:

```go
if err := client.Stop(); err != nil {
	// Log at warn/info level for normal app shutdown paths.
	fmt.Println("wallet shutdown:", err)
}
```

`Stop` and `Close` are aliases and are idempotent. `Wait` exposes the embedded
daemon's terminal run error for hosts that want to monitor daemon death:

```go
go func() {
	if err := <-client.Wait(); err != nil {
		fmt.Println("wallet runtime stopped:", err)
	}
}()
```

## Wrapper Guidance

Keep host-language bindings thin:

- Own one `*walletdk.Client` per wallet runtime.
- Expose explicit `Start`, `Stop`, `CreateWallet`, `UnlockWallet`,
  `ListBalance`, `GetOnchainAddress`, `Receive`, `Send`, `ListSwaps`, and
  `SubscribeSwaps` methods.
- Convert SDK structs into plain host DTOs. Do not expose protobuf messages to
  mobile or JavaScript callers.
- Accept caller-provided timeouts or cancellation handles for every operation.
- Route `Config.LogWriter` into the host logging system or a UI log buffer.
- Treat `DataDir`, wallet password handling, and mnemonic display as product
  security decisions owned by the host app.

For gomobile or React Native bridges, prefer a small manager object with string
or JSON DTO methods. For example, a bridge can hold the Go client internally and
return JSON-encoded `Balance`, `ReceiveResult`, and `SwapSummary` values to the
host UI.

For browser WASM, do not assume the current embedded daemon can run unchanged
inside a browser sandbox. The daemon expects filesystem, networking, timers,
and gRPC behavior that are more natural in native Go targets. A practical WASM
path is to keep the same wallet-shaped API but back it with a remote daemon or
host-provided transport until the daemon runtime is adapted for browser storage
and networking.

## LLM Integration Checklist

When generating wallet code against `walletdk`, follow this checklist:

1. Import `github.com/lightninglabs/darepo-client/sdk/walletdk`.
2. Build with `-tags swapruntime` if the code calls swap methods.
3. Use `walletdk.DefaultConfig()` and override only deployment-specific fields.
4. Set a durable `DataDir`.
5. Set `Network`, Ark operator connection fields, wallet backend fields, and
   swap server fields.
6. Start with `walletdk.Start(ctx, cfg)`.
7. Create or unlock the wallet before balance, address, receive, or send
   operations.
8. Display `ReceiveResult.Invoice` as the canonical BOLT-11 value.
9. Use `ListSwaps` and `SubscribeSwaps` for payment accounting.
10. Call `Stop` during app shutdown.
11. Never log wallet passwords, seed passphrases, mnemonics, or full invoices
    unless the product explicitly asks for that debug behavior.
