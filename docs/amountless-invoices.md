# Pay invoices without a requested amount

A BOLT11 invoice can omit its amount. The payer then chooses a positive amount
in whole sats. The signed invoice stays unchanged. This implementation follows
that rule and keeps fixed invoices bound to their original signed amount.

The wallet passes `PrepareSendRequest.amt_sat` through quote and dispatch. The
swap client checks the returned quote against that exact amount before funding.
Its durable pay authorization stores the chosen amount before a server request.
A retry with the same payment hash and a different amount fails, including after
a restart or a lost admission response. The existing single-use send intent and
pending-payment reconciliation continue to own dispatch.

`wavewalletdk.DecodeInvoice` validates the complete invoice against an explicit
Bitcoin network without starting a wallet or calling a remote service. The
mobile facade exposes it as JSON bytes with `invoice` and `network`. The result
has a nullable `amount_sat`, `description` and `expires_at_unix`. A missing amount
means the payer chooses one; an invalid or expired invoice returns an error.

`SwapClient.QuotePayViaLightningWithAmount` and
`StartPayViaLightningWithAmount` carry invoice, fee cap, credit cap and payer
amount independently. Legacy methods supply zero and keep their signatures.
Fixed-invoice callers should supply zero. An explicit matching fixed amount is
validated and normalized to zero before transport; a replacement is rejected.
The shared `sdk/bolt11.AmountMSat` resolver bounds amounts to Bitcoin's supply
before converting sats to millisats.

## Compatibility

New clients require a service that accepts `amount_sat` on quote and create for
amountless invoices. Old services reject them; the client cannot invent an amount
from a server quote. Fixed-invoice clients that omit the previously ignored `amt_sat` remain
compatible with new services. A nonzero mismatched `amt_sat` now fails instead
of being ignored. Existing scripts must omit it for fixed invoices. Amountless create owner proofs include the chosen amount. Account
signatures already cover every deterministic protobuf field, including the new
amount field. Fixed-invoice transcripts are unchanged.

The isolated swap-client database moves from schema 2 to 3. Historical
fixed-invoice authorizations gain the zero default. Preserve the database during
upgrades. Dropping the new authorization column loses the binding for an admitted
amountless payment and is not a safe rollback while such payments exist.

## Validation

Run `go test -tags='wavewalletrpc swapruntime mobile' ./sdk/bolt11 ./sdk/swaps
./sdk/wavewalletdk/... ./swapwallet ./swapclientserver ./credit` from the repository
root. Tests cover full invoice decoding, network and expiry, positive/supply-safe
amounts, signed-amount replacement, quote mismatch, single-use dispatch and
replayed authorization with a changed amount. These tests use synthetic invoices
and move no real funds.

The protocol rule comes from [BOLT11](https://github.com/lightning/bolts/blob/master/11-payment-encoding.md#human-readable-part).
[Phoenix's native amount handling](https://github.com/ACINQ/phoenix/blob/master/phoenix-ios/phoenix-ios/views/send/ValidateView.swift)
also distinguishes a decoded invoice with a missing amount before enabling payer
entry. Wavelength uses its existing fee review and confirmation flow.
