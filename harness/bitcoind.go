package harness

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/chainbackends/bitcoindrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo-client/txconfirm"
)

// Bitcoind is a typed wrapper over the harness regtest bitcoind RPC. It
// owns one *rpcclient.Client for the standard wallet/chain RPCs the
// fraud-response and unroll itests need, plus a *bitcoindrpc.PackageSubmitter
// for v3 package submission. The shape is deliberately a small handful of
// task-level methods so callers describe what they want (a fee input, a
// change script, a signed wallet tx, a confirmed package) instead of
// hand-rolling JSON-RPC requests at the use site.
type Bitcoind struct {
	rpc       *rpcclient.Client
	submitter *bitcoindrpc.PackageSubmitter
}

// Bitcoind returns a cached Bitcoind helper bound to the harness
// bitcoind, lazily constructed on first call. The harness owns the
// returned helper for the test lifetime; Stop closes it. Use this
// accessor instead of NewBitcoind when chaining several harness
// force-broadcast / wait calls so each one does not pay the cost of
// setting up and tearing down a fresh rpcclient.
func (h *ArkHarness) Bitcoind() (*Bitcoind, error) {
	h.bitcoindMu.Lock()
	defer h.bitcoindMu.Unlock()

	if h.bitcoind != nil {
		return h.bitcoind, nil
	}

	btc, err := NewBitcoind(h)
	if err != nil {
		return nil, err
	}
	h.bitcoind = btc

	return btc, nil
}

// NewBitcoind constructs a Bitcoind helper bound to the harness bitcoind.
// The caller owns the returned helper and must invoke Close when done.
// Most harness callers should use ArkHarness.Bitcoind() instead, which
// caches and shares the helper across the test lifetime.
func NewBitcoind(h *ArkHarness) (*Bitcoind, error) {
	rpc, err := h.BitcoinRPCClient()
	if err != nil {
		return nil, fmt.Errorf("bitcoin rpc client: %w", err)
	}

	submitter := bitcoindrpc.New(
		h.BitcoindRPC,
		client_harness.BitcoindRPCUser,
		client_harness.BitcoindRPCPass,
	)

	return &Bitcoind{rpc: rpc, submitter: submitter}, nil
}

// Close releases the underlying rpcclient.Client. Safe to call repeatedly.
func (b *Bitcoind) Close() {
	if b.rpc != nil {
		b.rpc.Shutdown()
	}
}

// SelectFeeInput picks one confirmed wallet UTXO with at least minValue
// and returns it as a txconfirm.FeeInput suitable for funding a CPFP
// child. The regtest wallet always has spendable outputs because the
// harness mines into it during setup.
func (b *Bitcoind) SelectFeeInput(
	minValue btcutil.Amount) (*txconfirm.FeeInput, error) {

	entries, err := b.rpc.ListUnspentMin(1)
	if err != nil {
		return nil, fmt.Errorf("listunspent: %w", err)
	}

	var pick *btcjson.ListUnspentResult
	for i := range entries {
		e := &entries[i]
		if !e.Spendable {
			continue
		}
		if btcutil.Amount(e.Amount*1e8) < minValue {
			continue
		}
		pick = e

		break
	}
	if pick == nil {
		return nil, fmt.Errorf("no confirmed wallet utxo with "+
			">= %s available", minValue)
	}

	hash, err := chainhash.NewHashFromStr(pick.TxID)
	if err != nil {
		return nil, fmt.Errorf("parse utxo txid %q: %w",
			pick.TxID, err)
	}

	pkScript, err := hex.DecodeString(pick.ScriptPubKey)
	if err != nil {
		return nil, fmt.Errorf("decode utxo pkScript: %w", err)
	}

	return &txconfirm.FeeInput{
		Outpoint: wire.OutPoint{Hash: *hash, Index: pick.Vout},
		Output: &wire.TxOut{
			Value:    int64(btcutil.Amount(pick.Amount * 1e8)),
			PkScript: pkScript,
		},
		Confirmed: true,
	}, nil
}

// NewChangePkScript returns the pkScript of a fresh wallet bech32 change
// address. label tags the address inside the wallet for later inspection.
// Bech32 produces the smallest change output on regtest.
func (b *Bitcoind) NewChangePkScript(label string) ([]byte, error) {
	addr, err := b.rpc.GetNewAddressType(label, "bech32")
	if err != nil {
		return nil, fmt.Errorf("getnewaddress: %w", err)
	}

	info, err := b.rpc.GetAddressInfo(addr.EncodeAddress())
	if err != nil {
		return nil, fmt.Errorf("getaddressinfo: %w", err)
	}

	pkScript, err := hex.DecodeString(info.ScriptPubKey)
	if err != nil {
		return nil, fmt.Errorf("decode change pkScript: %w", err)
	}

	return pkScript, nil
}

// SignWalletInputs hands the unsigned tx to bitcoind for signing of any
// inputs whose keys it owns. Inputs the wallet does not recognise (e.g.
// the P2A anchor input on a v3 CPFP child) are returned unsigned, which
// is the desired behaviour when the caller is building a CPFP child whose
// anchor input is anyone-can-spend.
//
// Returns an error only when the wallet failed to sign anything at all —
// otherwise a partially-signed tx with the wallet input(s) signed and the
// anchor input untouched is exactly what the caller wants.
func (b *Bitcoind) SignWalletInputs(tx *wire.MsgTx) (*wire.MsgTx, error) {
	signed, complete, err := b.rpc.SignRawTransactionWithWallet(tx)
	if err != nil {
		return nil, fmt.Errorf("signrawtransactionwithwallet: %w", err)
	}

	// "complete=false" with a wallet input signed and the anchor input
	// left as anyone-can-spend is exactly what we want for a v3 CPFP
	// child. Surface only when no input was signed at all.
	if !complete {
		signedAny := false
		for _, in := range signed.TxIn {
			if len(in.Witness) > 0 ||
				len(in.SignatureScript) > 0 {

				signedAny = true

				break
			}
		}
		if !signedAny {
			return nil, fmt.Errorf("sign returned no signed inputs")
		}
	}

	return signed, nil
}

// SubmitPackage submits a v3 CPFP package (parents + child) to bitcoind
// via the submitpackage RPC. Returns the child txid on success.
func (b *Bitcoind) SubmitPackage(ctx context.Context,
	parents []*wire.MsgTx, child *wire.MsgTx) (chainhash.Hash, error) {

	if _, err := b.submitter.SubmitPackage(
		ctx, parents, child, nil,
	); err != nil {
		return chainhash.Hash{}, err
	}

	return child.TxHash(), nil
}

// WaitTxConfirmed polls bitcoind for the given txid and returns nil once
// the transaction has at least one confirmation. Returns an error if the
// deadline is reached without a confirmation or if the bitcoind RPC fails.
func (b *Bitcoind) WaitTxConfirmed(ctx context.Context,
	txid chainhash.Hash, timeout time.Duration) error {

	deadline := time.Now().Add(timeout)
	for {
		result, err := b.rpc.GetRawTransactionVerbose(&txid)
		if err == nil && result.Confirmations >= 1 {
			return nil
		}

		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("getrawtransaction %s: %w",
					txid, err)
			}

			return fmt.Errorf("tx %s not confirmed within %s",
				txid, timeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
