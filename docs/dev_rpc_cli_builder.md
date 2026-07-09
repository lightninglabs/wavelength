# Generated Dev RPC CLI Builder

`darepocli dev` is a generated, low-level RPC escape hatch for daemon
services. It is intentionally separate from the curated user-facing commands
so developers can reach every exposed daemon RPC without hand-writing a CLI
command for each method.

## Generation Flow

`make rpc` regenerates protobuf stubs and then runs:

```shell
go run ./cmd/darepocli/internal/gen-devrpc
```

The generator reads the linked Go descriptors for:

- `daemonrpc.File_daemon_proto` (`daemonrpc.DaemonService`, alias `daemon`)
- `swapclientrpc.File_swap_client_proto` (`swapclientrpc.SwapClientService`,
  alias `swapclient`)
- `walletdkrpc.File_wallet_proto` (`walletdkrpc.WalletService` and
  `WalletInspectionService`, aliases `wallet` and `wallet-inspection`)
- `btcwalletrpc.File_api_proto` (`walletrpc.VersionService` and
  `walletrpc.WalletService`, aliases `btcwallet-version` and `btcwallet`)

It writes `cmd/darepocli/darepoclicommands/devrpc/registry_generated.go`.
That generated file contains only service and method metadata. The runtime
builder in `cmd/darepocli/darepoclicommands/devrpc` owns flag parsing,
dynamic request construction, RPC invocation, streaming response rendering, and
error mapping.

## Command Shape

The canonical form is:

```shell
darepocli dev <grpc_service> <call>
```

Examples:

```shell
darepocli dev daemonrpc.DaemonService GetInfo
darepocli dev daemon getinfo
darepocli dev daemon list-vtxos --status_filter live
darepocli dev swapclient start-pay --invoice <bolt11>
```

The generated registry also provides one stable short alias for each service
and one stable kebab-case alias for each method.

## Request Flags

The runtime builds request messages dynamically from protobuf descriptors.
Generated flags use proto field names so the command stays predictable and
does not drift from the wire contract.

Field handling rules:

- Scalar fields become `--field_name`.
- Boolean fields become normal Cobra bool flags.
- Repeated scalar fields become repeatable `--field_name` flags.
- Bytes fields accept hex strings, with or without a `0x` prefix.
- Enums accept numeric values, full enum value names, or lower aliases.
- Singular nested message fields are flattened with dotted flag names.
- Repeated messages and maps stay JSON via `--field_name-json`.
- The global `--json` flag remains a raw protojson escape hatch and takes
  precedence over generated flags.

Flattening is deliberately bounded to singular messages. For example:

```shell
darepocli dev daemon prepare-oor \
  --recipient.address bcrt1... \
  --recipient.amount_sat 1000

darepocli dev daemon refresh-vtxos \
  --outpoints.outpoints txid:0 \
  --outpoints.outpoints txid:1 \
  --dry_run
```

Repeated message fields are not flattened because indexed flags would need a
larger form language for grouping, ordering, merging, and partial validation.
Use JSON for those fields:

```shell
darepocli dev daemon send-vtxo \
  --recipients-json '[{"address":"bcrt1...","amount_sat":1000}]'
```

## Oneof Handling

The builder checks oneof conflicts while applying flags, including nested
oneofs reached through flattened paths. These two flags conflict:

```shell
--recipient.address bcrt1... --recipient.pubkey 00
```

But multiple fields under the same selected message are allowed:

```shell
--recipient.address bcrt1... --recipient.amount_sat 1000
```

Daemon-side validation remains authoritative. The generated command only
ensures the request can be represented as a valid protobuf message before it is
sent.
