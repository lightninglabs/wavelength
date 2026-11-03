# Signed exit ancestry export

`wavecli exit export --outpoint TXID:VOUT --output ancestry.json` exports the
same JSON object returned by `wavewalletrpc.WalletService.ExportExit` and
`POST /v1/wallet/exit-export`. The request selects one `outpoint`. This is a
portable collection of signed Bitcoin transactions for externally funded
materialization of a VTXO. Add `--sweep-address ADDRESS` to include a presigned
final timeout sweep so the service can finish the exit after CSV maturity:

```bash
wavecli exit export --outpoint TXID:VOUT \
  --sweep-address bc1p... --output exit.json
```

The service supplies confirmed fee inputs for both ancestry and the optional
final sweep. The user needs the VTXO signing key, but no backing-wallet fee
inputs. The bundle contains no private keys or service fee agreement.
Wavelength does not implement the external executor.

## Version 2 format

The JSON uses protobuf field names in snake_case. Transaction hashes use
Bitcoin's conventional display order; transaction and script bytes use hex.
Protobuf JSON encodes `int64` amounts as decimal strings, preserving precision.

| Field | Meaning |
| --- | --- |
| `format_version` | `2`; reject unsupported versions. Version 1 contained ancestry only. |
| `network` | Bitcoin network name, such as `mainnet` or `regtest`. |
| `genesis_hash` | Hash of the configured network's genesis block. Custom signet users must additionally agree on the challenge. |
| `target_outpoint` | `txid:index` of the VTXO to materialize. |
| `batch_expiry_height` | Saved earliest authenticated commitment sweep height; zero means unknown or unusable. Advisory: verify independently. |
| `csv_delay` | Target timeout delay in blocks, measured from its confirmation. This is metadata; the output's spending script remains authoritative. |
| `transactions` | Unique transactions sorted by dependency layer and then by raw txid bytes. Every parent appears before its children. |
| `external_inputs` | Previous outputs consumed by the graph whose transactions are outside the bundle. Sorted by raw txid bytes and then output index. |
| `final_sweep` | Optional signed timeout transaction and payout metadata; `null` when no sweep address was requested. |

Each transaction has `txid`, `kind` (`tree`, `checkpoint`, or `ark`),
`raw_tx_hex` (witness-inclusive wire serialization), `parent_txids`
(dependencies within the bundle), and `anchor_outpoint` (recognized P2A anchor,
or an empty string). Parent IDs are sorted by raw hash bytes. The Bitcoin
transaction is authoritative; these labels assist an executor and must be
checked on import.

Each external input has `outpoint`, `amount_sat`, `pk_script_hex`, and optional
`commitment_sweep` evidence. The evidence contains `delay_blocks`,
`leaf_script_hex`, and `control_block_hex`. Export verifies that the leaf and
control block commit to the external output and that the leaf encodes the
advertised block-based CSV delay. Legacy records without saved public sweep
policy omit this evidence; any saved deadline remains advisory and cannot be
independently checked from that bundle alone. A zero deadline does not mean
unlimited lifetime.
These are commitment outputs already funded outside the bundle, not inputs
the user must provide to pay for CPFP. Their funding transactions are not
included. Export validates signatures using locally stored previous outputs;
it does not query chain state or guarantee the outputs remain spendable.

## Executor contract

The executor should validate the version and network, parse each transaction
without trailing bytes, recompute transaction IDs, reject conflicting
duplicates, and reconstruct dependencies from actual inputs. Every input must
resolve to an earlier transaction output or an explicitly supplied external
output. Verify output indexes, values, scripts, and all input witnesses.
Check the declared target exists, all nodes contribute to its ancestry, and
anchor references identify actual P2A outputs.

Bind every external previous output to the executor's own Bitcoin chain view.
Check confirmations and competing spends. Do not trust exported amounts,
scripts, confirmation status, or transaction ordering as chain evidence.
Already confirmed ancestry can be skipped; an incompatible confirmed spend
requires stopping instead of repeatedly submitting an impossible exit.

Before scheduling an exit, verify each external input's `commitment_sweep`
against the chain-authenticated scriptPubKey. Check the control block and leaf
version, and parse the leaf as the expected single-key block-based CSV policy.
Do not trust `delay_blocks` without checking its script encoding. Add that delay
to the funding transaction's current confirmation height and take the minimum
across every external input. This reproduces the deadline independently of the
saved `batch_expiry_height`. Treat any missing evidence as an unknown deadline;
never omit that input from the minimum and claim the remaining result is safe.
Recompute after reorgs. Export itself performs no chain lookup and ignores
indexer-supplied confirmation-height hints.

The deadline is the earliest height at which an operator sweep can compete with
the ancestry. An executor must plan enough time to confirm the required tree
layers before then and adjust fees as the deadline approaches. At or after the
deadline, inspect actual competing spends: the bundle may still be recoverable
if no incompatible spend has confirmed. Re-exporting the same VTXO does not
extend its lifetime. Refreshing it creates a replacement VTXO that needs a new
backup, and makes the old bundle stale.

For each remaining transaction, wait for its parents to confirm and respect
its input sequences and locktime. Build a CPFP child spending its P2A anchor
together with the service's own confirmed fee input, returning change to the
service. The anchor is keyless; the service signs its own fee input and does
not need the user's private key. Use a suitable package feerate and submit
parent plus child under the node's current v3/TRUC and package relay rules.
Do not modify presigned ancestry transactions or assume a whole dependency
graph can be submitted as one package. Retry and fee bump children as needed,
and reconcile confirmation state after reorgs.

An export is a snapshot of available signed material. It does not reserve
the VTXO or prevent cooperative spends, forfeits, or automatic refresh. Agree
on handoff with the owner and recheck live state before execution. Possession
permits initiating unrolling and discloses ancestry, so transfer the bundle
only to the intended executor.

## Optional final sweep

A supplied `sweep_address` must be a Bitcoin address for the daemon's network.
The daemon builds a version-3 transaction with one input spending
`target_outpoint`, a sequence encoding the target CSV delay, and two outputs:
the full VTXO value to the destination and a zero-value P2A anchor. It signs
with the VTXO owner key using `SIGHASH_DEFAULT` and verifies the complete
script-path witness before exporting. Invalid or dust payouts, mismatched
local descriptors, unsupported scripts, and asset-bearing VTXOs are refused.
A requested sweep that cannot be signed fails the export; it never silently
falls back to an ancestry-only result.

`final_sweep` contains `txid`, witness-inclusive `raw_tx_hex`,
`anchor_outpoint`, `destination_address`, and `amount_sat`. Its raw transaction
is authoritative. An importer should verify that its sole input spends the
exported target with the required CSV sequence, its locktime is zero, its
version is 3, its payout matches the specified address and full target value,
and its second output is the declared zero-value P2A anchor. Verify its
script-path witness against the materialized target output. Keep it separate
from the immediately executable ancestry steps.

After the ancestry confirms and the target CSV delay matures, the executor
builds a child spending the final sweep anchor plus its own confirmed fee
input. Submit the zero-fee sweep and fee-paying child together as a package.
The signature commits to the complete destination payout and anchor, so the
service cannot redirect funds or deduct its fee from the payout. Future fee
adjustments are made to the service-owned child without another user signature.
A sweep with a zero-value anchor must remain zero-fee itself under ephemeral
anchor policy.

Presigning does not bypass the delay: the node still rejects the sweep before
CSV maturity. Check maturity and spendability again after reorgs. Sharing a
bundle with a sweep authorizes completion to that exact destination, while
sharing ancestry alone still requires a later owner-signed final spend.
Neither mode reserves the VTXO, disables automatic refresh, starts a local
unroll, or broadcasts anything. For asset-bearing outputs, ancestry-only
export remains available; final recovery requires an asset-aware spend.


## Batch backups

`WalletService.ExportExits` (`POST /v1/wallet/exits-export`) accepts an `exits`
array of 1 to 100 single-export requests, each with its own `outpoint` and
optional `sweep_address`. Duplicate targets are rejected, including different
text encodings of the same outpoint. Any failed target fails the entire call;
no partial bundle is returned and no exit is started. Per-target failures name
the failing outpoint and retain the original gRPC status code and structured
error details, so a backup client can identify which target to leave out.

```json
{"exits":[
  {"outpoint":"TXID1:0","sweep_address":"bc1p..."},
  {"outpoint":"TXID2:0","sweep_address":"bc1q..."}
]}
```

The response has `format_version`, `network`, `genesis_hash`, shared
`transactions` and `external_inputs`, plus `targets` in request order. Each
target has `target_outpoint`, `csv_delay`, `batch_expiry_height`, and optional
`final_sweep`. Shared ancestry appears once, ordered by dependency layer and
raw txid bytes. Witness checks are reused within the request, and conflicting
shared transactions, prevouts, or sweep evidence fail closed. The single-target
RPC and CLI keep their existing schema.

A batch is a collection of read-only exports, not a transactional wallet
snapshot or a reservation. Reconcile the saved target set with ongoing sends
and refreshes, and validate every target and its reachable ancestry on import.
