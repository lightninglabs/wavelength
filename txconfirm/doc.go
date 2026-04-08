// Package txconfirm provides a generic shared actor for ensuring that
// transactions are confirmed on-chain.
//
// The package is intentionally subsystem-neutral. Callers submit signed
// transactions with EnsureConfirmedReq, receive terminal notifications through
// TxConfirmed or TxFailed, and can cancel interest in a tracked transaction
// with CancelInterestReq. Internally the actor deduplicates by txid, shares
// chain subscriptions across subscribers, and uses anchor-aware CPFP fee
// bumping when needed.
package txconfirm
