// Package lwwallet provides a lightweight in-process Bitcoin wallet backed
// by LND's btcwallet and an Esplora/mempool.space chain backend. It wraps
// lnwallet/btcwallet.BtcWallet with an Esplora-based chain.Interface,
// providing a self-contained wallet without an external LND node:
//
//   - Full on-chain wallet: HD key management via waddrmgr, UTXO
//     tracking, address generation, balance queries
//   - Ark round participation: Schnorr signing, MuSig2 sessions,
//     boarding address management via btcwallet's signer
//   - Chain monitoring: block subscriptions, confirmation tracking,
//     spend detection via Esplora polling
//
// The wallet exposes wallet.BoardingBackend (via BoardingBackendAdapter),
// input.Signer + MuSig2 (via BtcWallet), and chainsource.ChainBackend
// (via ChainBackend), making it a drop-in replacement for the LND-backed
// implementations.
package lwwallet
