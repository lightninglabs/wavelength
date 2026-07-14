// Package lndsubmitter provides a chainbackends.PackageSubmitter backed by
// lnd's WalletKit.SubmitPackage RPC. It lets a waved using the lnd wallet
// relay its zero-fee unilateral-exit v3/TRUC packages through lnd's own chain
// connection, so no separate bitcoind RPC or Esplora endpoint is required.
package lndsubmitter

import (
	"context"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// satoshiPerBTC is the number of satoshis in one bitcoin. It converts
	// the chainbackends interface's BTC/kvB fee-rate ceiling into the
	// sat/vByte unit lnd's SubmitPackage RPC expects.
	satoshiPerBTC = 1e8

	// vBytesPerKvB is the number of virtual bytes in one kilo-virtual-byte
	// (the "kvB" denominator of bitcoind's BTC/kvB maxfeerate).
	vBytesPerKvB = 1000
)

// walletKitSubmitter is the subset of lndclient.WalletKitClient used to submit
// a package. lndclient.WalletKitClient satisfies it; narrowing the surface
// keeps the submitter easy to fake in tests.
type walletKitSubmitter interface {
	// SubmitPackage submits a topologically-sorted package (unconfirmed
	// parents first, child last) to lnd's chain backend. A nil maxFeeRate
	// leaves the node default unchanged.
	SubmitPackage(ctx context.Context, txns []*wire.MsgTx,
		maxFeeRate *chainfee.SatPerVByte) (
		*lndclient.SubmitPackageResult, error)
}

// Submitter relays v3 CPFP packages through lnd's WalletKit.SubmitPackage RPC.
type Submitter struct {
	walletKit walletKitSubmitter
}

// New creates a Submitter backed by the given lndclient WalletKit client.
func New(walletKit walletKitSubmitter) *Submitter {
	return &Submitter{walletKit: walletKit}
}

// SubmitPackage implements chainbackends.PackageSubmitter. It assembles the
// parents-first, child-last package, forwards it to lnd's
// WalletKit.SubmitPackage RPC, and maps the lndclient-native result back to
// the btcjson type callers expect.
func (s *Submitter) SubmitPackage(ctx context.Context, parents []*wire.MsgTx,
	child *wire.MsgTx, maxFeeRate *float64) (*btcjson.SubmitPackageResult,
	error) {

	// Guard against nil transactions up front: they would otherwise panic
	// deep in lndclient/wire serialization with a nil pointer dereference.
	if child == nil {
		return nil, fmt.Errorf("nil child transaction")
	}
	for i, parent := range parents {
		if parent == nil {
			return nil, fmt.Errorf("nil parent transaction at "+
				"index %d", i)
		}
	}

	txns := make([]*wire.MsgTx, 0, len(parents)+1)
	txns = append(txns, parents...)
	txns = append(txns, child)

	// chainbackends.PackageSubmitter carries the optional ceiling as a
	// *float64 in BTC/kvB (bitcoind's maxfeerate shape), while lnd's RPC
	// wants an integer sat/vByte, so convert. Round to nearest rather than
	// truncate: truncation would silently lower the ceiling (e.g. 12.5
	// sat/vByte → 12), making it stricter than the caller asked for. A nil
	// value passes through as the node default.
	var rate *chainfee.SatPerVByte
	if maxFeeRate != nil {
		satPerVByte := chainfee.SatPerVByte(
			math.Round(
				*maxFeeRate * satoshiPerBTC / vBytesPerKvB,
			),
		)
		rate = &satPerVByte
	}

	res, err := s.walletKit.SubmitPackage(ctx, txns, rate)
	if err != nil {
		return nil, fmt.Errorf("lnd submit package: %w", err)
	}
	if res == nil {
		return nil, fmt.Errorf("lnd submit package returned nil result")
	}

	return mapResult(res), nil
}

// mapResult converts the lndclient-native SubmitPackageResult into the
// btcjson.SubmitPackageResult returned by chainbackends.PackageSubmitter.
func mapResult(
	res *lndclient.SubmitPackageResult) *btcjson.SubmitPackageResult {

	out := &btcjson.SubmitPackageResult{
		PackageMsg:           res.PackageMsg,
		ReplacedTransactions: res.ReplacedTransactions,
		TxResults: make(
			map[string]btcjson.SubmitPackageTxResult,
			len(res.TxResults),
		),
	}

	for wtxid, r := range res.TxResults {
		entry := btcjson.SubmitPackageTxResult{TxID: r.Txid}

		// Only surface a rejection reason when lnd reported one; an
		// empty string means the tx was accepted.
		if r.Err != "" {
			reason := r.Err
			entry.Error = &reason
		}

		out.TxResults[wtxid] = entry
	}

	return out
}
