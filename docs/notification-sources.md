# Register the operator and swap notification sources

A wallet may receive work from two independent services. Operator subscriptions
cover mailbox and indexer work. Lightning swap mailboxes and credit-only receives
belong to the configured swap service and need a separate registration. This
addition builds on the signed control protocol in #1536; it does not change that
protocol or carry any notification amount.

Call `Client.SetNotificationSubscription` once for each supported source. The
existing empty `source` remains the operator. Use `"source":"swap_server"` for
the separately configured swap endpoint. Removal uses the same source selector
and subscription ID. Each call obtains a fresh challenge and signs the complete
mutation. A lost response needs another invocation, never a replayed signature.
The relay alias, grant, ID and expiry remain the mutation's signed fields.

The swap source requires an explicit compressed public identity pin. In the Go
facade, set `Config.SwapNotificationIssuer`. In gomobile JSON, set
`swap_notification_issuer` to that 33-byte key's hex encoding. In a daemon config,
use `swap.notificationissuer`. The app publisher obtains this public identity
from its trusted service configuration. A push payload or an unverified server
challenge must never supply the pin. Endpoint overrides need their matching
trusted pin; an empty pin leaves swap registration unsupported.

The registrar shares the daemon's configured gRPC swap connection and existing
TLS settings. No registration request supplies a URL, certificate or signing
key. The client verifies the challenge's issuer against the configured pin
before invoking the wallet signer. Operator registration still checks its own
negotiated operator identity. Source selectors are a closed set; unknown values
return `ErrInvalidNotificationSource` before RPC or signing. Issuer/proof errors
retain their distinct authorization error. Remote SDK handles have no local signing bridge.
REST swap transport currently returns unsupported registration and retains
foreground recovery.

The swap subserver validates the pin before opening storage or installing signer
hooks, including when REST is selected. It publishes an in-process callback under
a dedicated mutex and clears it before closing its transport.
The wallet never reads a concurrently assigned generic backend field to find
it. Signing uses the actual wallet identity bridge and does not wait for ingress
or start an ingress worker. This is a programmatic hook, not a new remote signing
RPC. The enclosing daemon owns the configured transport and its shutdown.

The app should give the operator only a wake capability. It may additionally
give the swap source a scoped generic-alert capability when the user chooses
that presentation. The producer must classify an authorized incoming event;
registration alone authorizes no amount or arbitrary notification text. Exact
amounts come from the wallet's verified persisted receipt. Registering both
sources does not enable bounded background execution by itself.

Validation:

```sh
go test -race -tags='swapruntime,wavewalletrpc,mobile' ./waved ./swapclientserver ./sdk/wavewalletdk ./sdk/wavewalletdk/mobile -count=1 -timeout=5m
make fmt-changed
make lint-changed-local
```

The facade test uses distinct issuer keys and real signed challenges to prove
source separation. The daemon test uses real gRPC and its production wallet
signing bridge before ingress readiness; a wrong issuer fails without another
wallet signature. The server must implement the direct control service and its
own transactional source hooks before registration can succeed. No service is
activated, no artifact is published and no live push is sent by this change.
