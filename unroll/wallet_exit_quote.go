package unroll

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/txconfirm"
)

// WalletExitTarget carries one selected coin's locally resolved proof. Its
// expiry orders shared funding when several targets need the same fee coin.
type WalletExitTarget struct {
	// Proof contains the signed recovery transactions for this target.
	Proof *recovery.Proof

	// Expiry is the batch expiry height used to order competing targets.
	Expiry int32

	// Asset identifies a target whose final claim needs a wallet-funded
	// child.
	Asset bool
}

// WalletExitQuote estimates full recovery at one rate. Each proof transaction
// and fee child is counted once even if several targets depend on it.
type WalletExitQuote struct {
	// Transactions counts unique recovery parents, fee children and final
	// spends.
	Transactions uint32

	// WalletFee estimates the fees paid from confirmed backing-wallet
	// outputs.
	WalletFee btcutil.Amount

	// NetworkFee includes wallet-funded fees and fees deducted from BTC
	// sweeps.
	NetworkFee btcutil.Amount

	// Deposit is the suggested additional deposit in satoshis, or zero
	// when current coins and their confirmed change cover the quote.
	Deposit btcutil.Amount

	// ReuseChange indicates that a later fee child needs change from an
	// earlier package to confirm.
	ReuseChange bool
}

// QuoteWalletExit prices the union of the selected proofs, in earliest-expiry
// order. The input values come from a confirmed, available wallet snapshot.
// Reuse simulates a later confirmation, never immediate eligibility. If any
// package cannot use the wallet's coins, one deposit covers the unmet fees.
func QuoteWalletExit(targets []WalletExitTarget, confirmed []btcutil.Amount,
	rate btcutil.Amount) (WalletExitQuote, error) {

	if rate <= 0 {
		return WalletExitQuote{}, fmt.Errorf("fee rate must be " +
			"positive")
	}

	// Price the most urgent targets first without reordering the request.
	order := slices.Clone(targets)
	slices.SortStableFunc(order, func(a, b WalletExitTarget) int {
		return cmp.Compare(a.Expiry, b.Expiry)
	})

	var quote WalletExitQuote
	seen := make(map[chainhash.Hash]bool)
	var fees []btcutil.Amount

	// Sibling targets can refer to the same proof transaction. Charge the
	// parent and its child on their first visit across the whole selection.
	for _, target := range order {
		if target.Proof == nil {
			return WalletExitQuote{}, fmt.Errorf("selected exit " +
				"has no proof")
		}
		for _, layer := range target.Proof.Layers() {
			for _, id := range layer {
				if seen[id] {
					continue
				}
				node, _ := target.Proof.Node(id)
				size := extraNodeVBytes(node)
				size += defaultCPFPChildVBytes
				fee := btcutil.Amount(size) * rate
				seen[id] = true
				fees = append(fees, fee)
				quote.Transactions += 2
			}
		}

		// The BTC sweep pays from the target. The asset claim keeps
		// the carrier intact and needs another wallet-funded child.
		quote.Transactions++
		final := AssessExitFeasibility(ExitFeasibilityInput{
			AssetClaim: target.Asset, FeeRateSatPerVByte: rate,
		})
		quote.NetworkFee += final.SweepFeeSat
		if target.Asset {
			quote.Transactions++
			fees = append(fees, final.CPFPFeeTotalSat)
		}
	}

	quote.Deposit, quote.ReuseChange = projectExitFeeCoins(confirmed, fees)
	for _, fee := range fees {
		quote.WalletFee += fee
	}
	quote.NetworkFee += quote.WalletFee

	return quote, nil
}

// projectExitFeeCoins spends distinct snapshot coins where possible, then
// their confirmed change. Unmet fees share a single additional deposit and
// one dust reserve. Amounts in the caller's snapshot are immutable.
func projectExitFeeCoins(confirmed, fees []btcutil.Amount) (btcutil.Amount,
	bool) {

	var fresh, change []btcutil.Amount

	// Apply the admission floor to the initial wallet snapshot. Confirmed
	// change later needs only enough value for its fee and change output.
	for _, amount := range confirmed {
		if amount >= DefaultFeeInputMinAmountSat {
			fresh = append(fresh, amount)
		}
	}

	var missing btcutil.Amount
	var reused bool

	// Try separate coins first, preserving parallelism where possible.
	for _, fee := range fees {
		remaining, ok := takeExitFeeCoin(&fresh, fee)
		if !ok {
			remaining, ok = takeExitFeeCoin(&change, fee)
			reused = reused || ok
		}
		if !ok {
			missing += fee
			continue
		}
		change = append(change, remaining)
	}

	// A single deposit can pay the remaining packages in sequence.
	if missing > 0 {
		missing = max(
			DefaultFeeInputMinAmountSat,
			missing+txconfirm.DustLimit,
		)
	}

	return missing, reused
}

// takeExitFeeCoin consumes the smallest coin covering the fee and change dust.
func takeExitFeeCoin(coins *[]btcutil.Amount,
	fee btcutil.Amount) (btcutil.Amount, bool) {

	best := -1
	for i, value := range *coins {
		if value >= fee+txconfirm.DustLimit &&
			(best < 0 || value < (*coins)[best]) {

			best = i
		}
	}
	if best < 0 {
		return 0, false
	}
	values := *coins
	remaining := values[best] - fee
	values[best] = values[len(values)-1]
	*coins = values[:len(values)-1]

	return remaining, true
}
