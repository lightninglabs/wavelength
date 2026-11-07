# Check enrolled Bitcoin invoice receipts

A push is a hint. To show an amount, the app enrolls the typed
`ReceiptReference` returned by a newly created Lightning invoice after consent.
`ReadLightningReceipts` checks at most 64 such references in one local call.
It does not enumerate historical wallet receipts.

The ordinary receive adapter reads the persisted swap summary. The credit
adapter reads one original credit operation, including its immutable admission
time. Both validate the complete BOLT-11 invoice against the wallet network,
payment hash and principal. Only typed receive sources qualify. Attached-credit
funding, pay top-ups, redemptions, raw Ark carriers and aggregate deposits do not
supply a presentation amount through this API.

The wallet layer normalizes the source using its existing activity helpers,
projects through the ordinary canonical sink, then reloads the current row.
The source and canonical status must agree. A concurrent transition observed
between snapshots reports pending until a later check. Only complete permits
amount presentation. A completed local source can be projected immediately;
pending does not pretend that a remote reconciliation turn was awaited.

The original admission must be strictly after `created_after_unix`. Equality is
suppressed because source timestamps have one-second precision. A late first
projection cannot make an old credit operation new. Enrollment is separate:
never populate the enrolled set from arbitrary activity backfill or push data.
If the app dies after invoice creation but before saving enrollment, the alert
may be omitted; funds and wallet recovery remain independent.

Go and mobile use these JSON models:

```json
{"references":[{"source":"lightning_swap","id":"payment-hash"}],"created_after_unix":1794000000}
```

```json
[{"reference":{"source":"lightning_swap","id":"payment-hash"},"canonical_id":"payment-hash","state":"complete","amount_sat":100000,"created_at_unix":1794000001}]
```

`credit_receive` uses the local credit operation ID. Unknown, unsupported and
pre-opt-in sources are omitted; errors abort the call. Keep enrolled pending
references and durable canonical-ID presentation deduplication in the host.
Never use an amount or ID from the wake payload. An invoice receive says that
the wallet received value; it does not identify an external payer.

These callbacks borrow the embedded owner through SDK direct-call admission.
Close cancels and joins final access before releasing the daemon. Unregistering
the wallet-layer adapter also waits for current borrowers. Remote SDK handles
return an unsupported error. Mobile calls have a cooperative ten-second context.
No new remote RPC, signing capability, schema or deployment is introduced.

This is the receipt classification/read boundary. A separate host window and
iOS presenter must control execution, preserve locked-wallet fallback, choose
between generic and local alerts, and persist deduplication. This API does not
claim global catch-up, arbitrary Ark/asset receipt discovery or safe suspension.
