# walletdk Integration Guide

`walletdk` is the wallet-facing SDK for applications that want a small API over
`darepod`. `Start` embeds the daemon in-process and connects to it over private
`bufconn`; `Connect` attaches the same client API to an external daemon that
exposes `walletdkrpc`.

Build embedded wallet payment support with both wallet runtime tags:

```sh
go build -tags walletdkrpc,swapruntime ./cmd/your-wallet
go test -tags walletdkrpc,swapruntime ./sdk/walletdk
```

Without those tags, `Start` fails with `walletdk.ErrWalletRPCUnavailable`.
`Connect` can still talk to a remote daemon that was built with
`walletdkrpc,swapruntime`.

## Runtime Flow

1. Build a `walletdk.Config`.
2. Start the embedded daemon with `walletdk.Start`, or connect to an external
   daemon with `walletdk.Connect`.
3. Create or unlock the wallet.
4. Use `Status`, `Balance`, `Deposit`, `Receive`, `Send`, `List`, and
   `Subscribe`.
5. Call `Stop` when the host app shuts down.

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

cfg := walletdk.DefaultConfig()
cfg.DataDir = "/tmp/example-wallet"
cfg.Network = "regtest"
cfg.ServerAddress = "127.0.0.1:10010"
cfg.ServerInsecure = true
cfg.WalletType = "lwwallet"
cfg.WalletEsploraURL = "http://127.0.0.1:3002"
cfg.SwapServerAddress = "127.0.0.1:11010"
cfg.SwapServerInsecure = true
cfg.LogWriter = io.Discard

client, err := walletdk.Start(ctx, cfg)
if err != nil {
	panic(err)
}
defer client.Stop()
```

Remote daemon mode uses the same methods:

```go
client, err := walletdk.Connect(ctx, walletdk.ConnectConfig{
	Address: "127.0.0.1:10009",
})
if err != nil {
	panic(err)
}
defer client.Stop()
```

## Wallet Bootstrap

Use `GetInfo` to decide whether to create or unlock the daemon wallet:

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

## Wallet Operations

Fetch readiness and balances:

```go
status, err := client.Status(ctx)
if err != nil {
	panic(err)
}

balance, err := client.Balance(ctx)
if err != nil {
	panic(err)
}

fmt.Println("ready:", status.Ready)
fmt.Println("confirmed:", balance.ConfirmedSat)
fmt.Println("pending in:", balance.PendingInSat)
fmt.Println("pending out:", balance.PendingOutSat)
```

Create a boarding deposit address:

```go
deposit, err := client.Deposit(ctx, walletdk.DepositRequest{
	AmountSatHint: 50_000,
})
if err != nil {
	panic(err)
}

fmt.Println("address:", deposit.Address)
fmt.Println("entry:", deposit.Entry.ID, deposit.Entry.Status)
```

Create a Lightning invoice payable into the wallet:

```go
receive, err := client.Receive(ctx, walletdk.ReceiveRequest{
	AmountSat: 50_000,
	Memo:      "demo receive",
})
if err != nil {
	panic(err)
}

fmt.Println("invoice:", receive.Invoice)
fmt.Println("entry:", receive.Entry.ID, receive.Entry.Status)
```

Send to a Lightning invoice or on-chain address:

```go
send, err := client.Send(ctx, walletdk.SendRequest{
	Invoice:   bolt11Invoice,
	MaxFeeSat: 1_000,
	Note:      "demo payment",
})
if err != nil {
	panic(err)
}

fmt.Println("entry:", send.Entry.ID, send.Entry.Status)
```

## Wallet Activity

`List` is the durable wallet activity view. It returns normalized `Entry` rows
for sends, receives, deposits, and exits.

```go
history, err := client.List(ctx, walletdk.ListRequest{
	PendingOnly: false,
})
if err != nil {
	panic(err)
}

for _, entry := range history.Entries {
	fmt.Println(entry.Kind, entry.ID, entry.Status, entry.AmountSat)
}
```

Use `Subscribe` to drive live UI updates:

```go
subCtx, stopSub := context.WithCancel(context.Background())
defer stopSub()

updates, errs, err := client.Subscribe(subCtx, walletdk.SubscribeRequest{
	IncludeExisting: true,
})
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

			fmt.Println("wallet update:", update.ID, update.Status)

		case err, ok := <-errs:
			if ok && err != nil {
				fmt.Println("wallet subscription error:", err)
			}

			return
		}
	}
}()
```

## Wrapper Guidance

Keep host-language bindings thin:

- Own one `*walletdk.Client` per wallet runtime.
- Expose explicit `Start` or `Connect`, `Stop`, `CreateWallet`,
  `UnlockWallet`, `Status`, `Balance`, `Deposit`, `Receive`, `Send`, `List`,
  and `Subscribe` methods.
- Convert SDK structs into plain host DTOs. Do not expose protobuf messages to
  mobile or JavaScript callers.
- Accept caller-provided timeouts or cancellation handles for every operation.
- Route `Config.LogWriter` into the host logging system or a UI log buffer.
- Treat `DataDir`, wallet password handling, and mnemonic display as product
  security decisions owned by the host app.

For gomobile or React Native bridges, prefer a small manager object with string
or JSON DTO methods. For browser WASM, prefer the `Connect` shape against a
remote or host-provided daemon until the embedded daemon storage and transport
stack is browser-ready.

## LLM Integration Checklist

When generating wallet code against `walletdk`, follow this checklist:

1. Import `github.com/lightninglabs/darepo-client/sdk/walletdk`.
2. Build embedded wallets with `-tags walletdkrpc,swapruntime`.
3. Use `walletdk.DefaultConfig()` and override only deployment-specific fields.
4. Set a durable `DataDir`.
5. Set `Network`, Ark operator connection fields, wallet backend fields, and
   swap server fields.
6. Start with `walletdk.Start(ctx, cfg)`.
7. Create or unlock the wallet before balance, address, receive, or send
   operations.
8. Display `ReceiveResult.Invoice` as the canonical BOLT-11 value.
9. Use `List` and `Subscribe` for payment accounting.
10. Call `Stop` during app shutdown.
11. Never log wallet passwords, seed passphrases, mnemonics, or full invoices
    unless the product explicitly asks for that debug behavior.
