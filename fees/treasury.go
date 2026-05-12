package fees

import (
	"sync"

	"github.com/btcsuite/btcd/btcutil"
)

// TreasurySnapshot is a point-in-time view of the operator's
// capital position.
type TreasurySnapshot struct {
	// DeployedCapitalSat is the total satoshis currently locked
	// in live VTXOs (capital the operator cannot spend until
	// sweep).
	DeployedCapitalSat int64

	// WalletBalanceSat is the confirmed on-chain wallet
	// balance in satoshis.
	WalletBalanceSat int64

	// PendingSweepSat is capital that has been forfeited but
	// not yet swept back into the wallet. This is tracked
	// separately to prevent a transient utilization spike
	// during the forfeit-to-sweep window.
	PendingSweepSat int64

	// KMaxSat is the operator's total accessible capital:
	// DeployedCapitalSat + WalletBalanceSat + PendingSweepSat.
	KMaxSat int64

	// Utilization is DeployedCapitalSat / KMaxSat. Ranges
	// from 0.0 (idle) to 1.0 (fully deployed).
	Utilization float64

	// LiveVTXOCount is the number of currently live VTXOs.
	LiveVTXOCount int
}

// TreasuryTracker monitors the operator's deployed liquidity and
// wallet balance to compute utilization for congestion pricing.
// All methods are safe for concurrent use.
//
// Capital transitions through three states:
//
//	wallet → deployed (OnRoundConfirmed)
//	deployed → pendingSweep (OnVTXOsForfeited)
//	pendingSweep → wallet (OnSweepCompleted)
//
// The pendingSweep bucket prevents utilization from spiking
// during the window between forfeit and sweep confirmation.
type TreasuryTracker struct {
	mu              sync.RWMutex
	deployedCapital int64
	walletBalance   int64
	pendingSweepSat int64
	liveVTXOCount   int
}

// NewTreasuryTracker creates a new TreasuryTracker with zero
// initial state. Call Initialize after startup to bootstrap from
// the database and wallet.
func NewTreasuryTracker() *TreasuryTracker {
	return &TreasuryTracker{}
}

// Initialize sets the initial deployed capital and wallet balance
// from the database and wallet queries at startup.
func (t *TreasuryTracker) Initialize(liveVTXOTotalSat int64, liveVTXOCount int,
	walletBalance btcutil.Amount) {

	t.mu.Lock()
	defer t.mu.Unlock()

	t.deployedCapital = liveVTXOTotalSat
	t.liveVTXOCount = liveVTXOCount
	t.walletBalance = int64(walletBalance)
	t.pendingSweepSat = 0
}

// Reseed replaces the in-memory capital buckets with values read
// from an authoritative source (typically the persisted
// double-entry ledger on daemon startup). Every field is
// overwritten -- the supplied values must be fully-computed
// totals, not deltas.
//
// The ledger does not distinguish capital that is forfeited and
// pending-sweep from capital that is still backing live VTXOs
// (both live in the deployed_capital account), so the rebuild
// flow folds pendingSweepSat into deployedCapital on Reseed and
// lets subsequent OnVTXOsForfeited / OnSweepCompleted events
// re-establish the split as new activity flows through the
// actor. This is a conservative approximation: over-reporting
// deployed capital inflates utilization, which biases
// congestion pricing upward rather than silently suppressing it.
func (t *TreasuryTracker) Reseed(deployedCapitalSat int64,
	pendingSweepSat int64, liveVTXOCount int,
	walletBalance btcutil.Amount) {

	t.mu.Lock()
	defer t.mu.Unlock()

	t.deployedCapital = deployedCapitalSat
	t.pendingSweepSat = pendingSweepSat
	t.liveVTXOCount = liveVTXOCount
	t.walletBalance = int64(walletBalance)
}

// kMax returns the total capital without holding the lock.
// Caller must hold at least an RLock.
func (t *TreasuryTracker) kMax() int64 {
	return t.deployedCapital + t.walletBalance +
		t.pendingSweepSat
}

// Snapshot returns the current treasury state as an immutable
// point-in-time view.
func (t *TreasuryTracker) Snapshot() *TreasurySnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	kMax := t.kMax()

	utilization := 0.0
	if kMax > 0 {
		utilization = float64(t.deployedCapital) /
			float64(kMax)
	}

	return &TreasurySnapshot{
		DeployedCapitalSat: t.deployedCapital,
		WalletBalanceSat:   t.walletBalance,
		PendingSweepSat:    t.pendingSweepSat,
		KMaxSat:            kMax,
		Utilization:        utilization,
		LiveVTXOCount:      t.liveVTXOCount,
	}
}

// Utilization returns the current treasury utilization ratio
// (0.0 to 1.0). This is the primary input for congestion
// pricing.
func (t *TreasuryTracker) Utilization() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()

	kMax := t.kMax()
	if kMax <= 0 {
		return 0.0
	}

	return float64(t.deployedCapital) / float64(kMax)
}

// OnRoundConfirmed updates deployed capital when new VTXOs
// become live after a round is confirmed on-chain. The total
// amount is the sum of all VTXO values created in the round.
func (t *TreasuryTracker) OnRoundConfirmed(totalVTXOAmountSat int64,
	vtxoCount int) {

	t.mu.Lock()
	defer t.mu.Unlock()

	t.deployedCapital += totalVTXOAmountSat
	t.liveVTXOCount += vtxoCount
}

// OnVTXOsForfeited moves capital from deployed to pending sweep
// when VTXOs are forfeited as part of a refresh or spend. The
// forfeited capital is not yet in the wallet (the sweep has not
// confirmed), but it is no longer backing live VTXOs. Moving it
// to pendingSweep keeps KMax stable and prevents a transient
// utilization spike.
func (t *TreasuryTracker) OnVTXOsForfeited(totalAmountSat int64, count int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.deployedCapital -= totalAmountSat
	t.pendingSweepSat += totalAmountSat
	t.liveVTXOCount -= count

	// Guard against underflow from initialization races.
	if t.deployedCapital < 0 {
		t.deployedCapital = 0
	}
	if t.liveVTXOCount < 0 {
		t.liveVTXOCount = 0
	}
}

// OnSweepCompleted clears pending sweep capital when the
// operator's sweep transaction confirms. The wallet balance
// catches up asynchronously via UpdateWalletBalance when the
// confirmed balance is refreshed.
func (t *TreasuryTracker) OnSweepCompleted(reclaimedAmountSat int64,
	count int) {

	t.mu.Lock()
	defer t.mu.Unlock()

	t.pendingSweepSat -= reclaimedAmountSat
	if t.pendingSweepSat < 0 {
		t.pendingSweepSat = 0
	}
}

// UpdateWalletBalance sets the current confirmed wallet balance.
// Called periodically or after relevant wallet events (e.g.,
// confirmed sweeps, manual deposits).
func (t *TreasuryTracker) UpdateWalletBalance(balance btcutil.Amount) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.walletBalance = int64(balance)
}
