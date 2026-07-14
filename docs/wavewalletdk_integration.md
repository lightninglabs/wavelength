# wavewalletdk Integration Guide

`wavewalletdk` is the wallet-facing SDK for applications that want a small API over
`waved`. `Start` embeds the daemon in-process and connects to it over private
`bufconn`; `Connect` attaches the same client API to an external daemon that
exposes `wavewalletrpc`. Advanced callers can also reach btcsuite btcwallet's
native `walletrpc.WalletService` through the same gRPC connection when the
daemon is using the `lwwallet` or `btcwallet` backend.

Build embedded wallet payment support with both wallet runtime tags:

```sh
go build -tags wavewalletrpc,swapruntime ./cmd/your-wallet
go test -tags wavewalletrpc,swapruntime ./sdk/wavewalletdk
```

Without those tags, `Start` fails with `wavewalletdk.ErrWalletRPCUnavailable`.
`Connect` can still talk to a remote daemon that was built with
`wavewalletrpc,swapruntime`.

## Runtime Flow

`wavewalletdk.DefaultConfig()` leaves `ServerAddress` and `SwapServerAddress`
empty. `Start` treats an empty value as "no explicit override", then resolves
the effective endpoint from `Network` and the matching transport through the
embedded `waved` config. Host apps that need to display or forward a concrete
address should set it explicitly. See [signet.md](signet.md) for the built-in
testnet3, testnet4, and signet endpoints.

1. Build a `wavewalletdk.Config`.
2. Start the embedded daemon with `wavewalletdk.Start`, or connect to an external
   daemon with `wavewalletdk.Connect`.
3. Create or unlock the wallet.
4. Use `Status`, `Balance`, `Deposit`, `Receive`, `PrepareSend` /
   `SendPrepared`, `List`, and `Subscribe`.
5. Call `Stop` when the host app shuts down.

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

cfg := wavewalletdk.DefaultConfig()
cfg.DataDir = "/tmp/example-wallet"
cfg.Network = "regtest"
cfg.ServerAddress = "127.0.0.1:10010"
cfg.ServerInsecure = true
cfg.WalletType = "lwwallet"
cfg.WalletEsploraURL = "http://127.0.0.1:3002"
cfg.SwapServerAddress = "127.0.0.1:11010"
cfg.SwapServerInsecure = true
cfg.LogWriter = io.Discard

client, err := wavewalletdk.Start(ctx, cfg)
if err != nil {
	panic(err)
}
defer client.Stop()
```

Remote daemon mode uses the same methods:

```go
client, err := wavewalletdk.Connect(ctx, wavewalletdk.ConnectConfig{
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

created, err := client.CreateWallet(ctx, wavewalletdk.CreateWalletRequest{
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
unlocked, err := client.UnlockWallet(ctx, wavewalletdk.UnlockWalletRequest{
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
deposit, err := client.Deposit(ctx, wavewalletdk.DepositRequest{
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
receive, err := client.Receive(ctx, wavewalletdk.ReceiveRequest{
	AmountSat: 50_000,
	Memo:      "demo receive",
})
if err != nil {
	panic(err)
}

fmt.Println("invoice:", receive.Invoice)
fmt.Println("entry:", receive.Entry.ID, receive.Entry.Status)
```

Send to a Lightning invoice or on-chain address. Sending is a two-step flow:
`PrepareSend` validates and quotes the payment and returns a single-use
`SendIntentID`; `SendPrepared` then dispatches that intent. This lets a UI show
the fee/rail quote before the user commits.

```go
prepared, err := client.PrepareSend(ctx, wavewalletdk.PrepareSendRequest{
	Invoice:   bolt11Invoice,
	MaxFeeSat: 1_000,
	Note:      "demo payment",
})
if err != nil {
	panic(err)
}

// Inspect the quote before committing: prepared.AmountSat,
// prepared.ExpectedFeeSat / prepared.FeeKnown, prepared.Rail,
// prepared.QuoteStatus, and prepared.ExpiresAtUnix.
send, err := client.SendPrepared(ctx, wavewalletdk.SendPreparedRequest{
	SendIntentID: prepared.SendIntentID,
})
if err != nil {
	panic(err)
}

// ActualAmountSat equals the requested amount for a bounded send; for a
// sweep-all send it is the swept total. Echo it before treating the send
// as confirmed.
fmt.Println("entry:", send.Entry.ID, send.Entry.Status)
fmt.Println("actual outflow:", send.ActualAmountSat)
```

## Native btcwallet RPC

`BtcwalletRPC` exposes btcsuite btcwallet's native `walletrpc.WalletService`
for lower-level on-chain wallet operations such as fresh external addresses,
funding PSBTs, signing, and publishing. The service uses wavewalletdk's existing
private `bufconn` when started with `Start`, so host apps do not need a second
listener.

```go
btcw := client.BtcwalletRPC()
addr, err := btcw.NextAddress(ctx, &walletrpc.NextAddressRequest{
	Account: 0,
	Kind:    walletrpc.NextAddressRequest_BIP0044_EXTERNAL,
})
if err != nil {
	panic(err)
}

fmt.Println("on-chain address:", addr.Address)
```

The native service is only backed by self-managed wallet modes. It returns a
gRPC failed-precondition error when the daemon is using the `lnd` backend or
before the self-managed wallet has been created or unlocked.

## Wallet Activity

`List` returns a `ListResult` tagged union: read the variant named by `View`
(`Activity`, `VTXOs`, or `Onchain`) and treat the others as `nil`. The default
view is `ListViewActivity`, whose `Activity.Entries` are normalized `Entry` rows
for sends, receives, deposits, and exits.

```go
history, err := client.List(ctx, wavewalletdk.ListRequest{
	View: wavewalletdk.ListViewActivity,
})
if err != nil {
	panic(err)
}

for _, entry := range history.Activity.Entries {
	fmt.Println(entry.Kind, entry.ID, entry.Status, entry.AmountSat)
}
```

Use `Subscribe` to drive live UI updates:

```go
subCtx, stopSub := context.WithCancel(context.Background())
defer stopSub()

updates, errs, err := client.Subscribe(subCtx, wavewalletdk.SubscribeRequest{
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

- Own one `*wavewalletdk.Client` per wallet runtime.
- Expose explicit `Start` or `Connect`, `Stop`, `CreateWallet`,
  `UnlockWallet`, `Status`, `Balance`, `Deposit`, `Receive`, `PrepareSend`,
  `SendPrepared`, `List`, and `Subscribe` methods.
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

When generating wallet code against `wavewalletdk`, follow this checklist:

1. Import `github.com/lightninglabs/wavelength/sdk/wavewalletdk`.
2. Build embedded wallets with `-tags wavewalletrpc,swapruntime`.
3. Use `wavewalletdk.DefaultConfig()` and override only deployment-specific fields.
4. Set a durable `DataDir`.
5. Set `Network`, Ark operator connection fields, wallet backend fields, and
   swap server fields.
6. Start with `wavewalletdk.Start(ctx, cfg)`.
7. Create or unlock the wallet before balance, address, receive, or send
   operations.
8. Display `ReceiveResult.Invoice` as the canonical BOLT-11 value.
9. Use `List` and `Subscribe` for payment accounting.
10. Call `Stop` during app shutdown.
11. Never log wallet passwords, seed passphrases, mnemonics, or full invoices
    unless the product explicitly asks for that debug behavior.
