// Package batchcanon holds the client-side batch canonicality data model:
// the durable record of how each batch (commitment) transaction is faring
// against the best chain, the inputs it consumes, the VTXOs it anchors, and
// the reverse-dependency edges needed to restore a provisionally consumed
// VTXO if its consumer batch never becomes canonical.
//
// This package is the data substrate for the reorg-safety epic
// (darepo#454). It deliberately contains NO interpretation or admission
// behavior: it persists and retrieves observations only. The
// BatchCanonicalityManager (a later task) is the sole interpreter that
// drives state transitions from chainsource observations, and the VTXO
// manager remains the admission boundary. Keeping the model here, separate
// from both chainsource (raw observation) and vtxo (admission), preserves
// the observation -> interpretation -> action split the epic mandates.
//
// Two principles shape the model:
//
//   - Identity is by txid / outpoint, never by (txid, block hash). A reorg
//     that re-mines the same batch tx in a different block is the SAME
//     batch; the block hash is only an observation attribute.
//
//   - Expiry is never stored as a terminal fact. The model stores a
//     CSV-relative delta plus the current confirmation height and derives
//     the effective (absolute) expiry on demand, so a reorg-and-reconfirm
//     at a new height recomputes expiry instead of freezing it.
package batchcanon
