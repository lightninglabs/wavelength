// Package walletcore provides shared btcwallet wrapping used by both
// the lwwallet (Esplora-backed) and btcwbackend (neutrino-backed)
// wallet implementations. It extracts common HD key management,
// signing, address generation, and balance operations that delegate
// to btcwallet.BtcWallet regardless of the underlying chain source.
//
// Chain-specific implementations (lwwallet, btcwbackend) embed
// walletcore.Wallet and walletcore.BoardingBackendBase, adding their
// own chain data sources for UTXO queries, block fetching, and
// chain monitoring.
package walletcore
