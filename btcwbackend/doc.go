//go:build !js || !wasm

// Package btcwbackend provides a lightweight in-process Bitcoin wallet backed
// by LND's btcwallet and a neutrino (BIP 157/158) chain backend. It wraps
// lnwallet/btcwallet.BtcWallet with a neutrino-based chain.Interface,
// providing a self-contained SPV wallet that connects directly to the Bitcoin
// P2P network without requiring an external Esplora server or LND node:
//
//   - Full on-chain wallet: HD key management via waddrmgr, UTXO
//     tracking, address generation, balance queries
//   - Ark round participation: Schnorr signing, MuSig2 sessions,
//     boarding address management via btcwallet's signer
//   - Chain monitoring: block subscriptions, confirmation tracking,
//     spend detection via neutrino's native ChainNotifier
//
// The wallet exposes wallet.BoardingBackend (via BoardingBackendAdapter),
// input.Signer + MuSig2 (via BtcWallet), and chainsource.ChainBackend
// (via ChainBackend), making it a drop-in replacement for the LND-backed
// and lwwallet (Esplora-backed) implementations.
package btcwbackend
