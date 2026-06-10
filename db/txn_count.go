package db

import (
	"sort"
	"sync"
	"sync/atomic"
)

// txnAccountKey identifies one transaction accounting bucket: the domain
// call site that owns the transaction plus whether it ran read-only.
type txnAccountKey struct {
	caller   string
	readOnly bool
}

var (
	// txnAccountingEnabled gates the per-commit accounting hook in
	// ExecTx. The gate costs one atomic load per commit when disabled,
	// so production pays nothing unless a harness opts in.
	txnAccountingEnabled atomic.Bool

	// txnAccountingMu guards txnAccountingCounts.
	txnAccountingMu sync.Mutex

	// txnAccountingCounts accumulates committed-transaction counts keyed
	// by owning call site.
	txnAccountingCounts = make(map[txnAccountKey]uint64)
)

// TxnCommitCount reports the number of committed transactions attributed to
// one call site.
type TxnCommitCount struct {
	// Caller is the fully qualified function name of the first frame
	// outside the db packages that started the transaction.
	Caller string

	// ReadOnly reports whether the bucket's transactions ran read-only.
	// On SQLite only read-write commits pay the single-writer lock and
	// the fsync, so the split separates durability load from read load.
	ReadOnly bool

	// Count is the number of committed transactions in this bucket.
	Count uint64
}

// EnableTxnAccounting turns on per-call-site commit accounting. It exists
// for benchmarks and tests that need an empirical transaction ledger; the
// gate is process-global and stays on until process exit.
func EnableTxnAccounting() {
	txnAccountingEnabled.Store(true)
}

// ResetTxnAccounting clears all accumulated accounting buckets, typically
// to scope counting to a workload phase, for example after setup but before
// the measured payments begin.
func ResetTxnAccounting() {
	txnAccountingMu.Lock()
	defer txnAccountingMu.Unlock()

	txnAccountingCounts = make(map[txnAccountKey]uint64)
}

// TxnAccountingSnapshot returns the accumulated commit counts sorted by
// descending count.
func TxnAccountingSnapshot() []TxnCommitCount {
	txnAccountingMu.Lock()
	defer txnAccountingMu.Unlock()

	counts := make([]TxnCommitCount, 0, len(txnAccountingCounts))
	for key, count := range txnAccountingCounts {
		counts = append(counts, TxnCommitCount{
			Caller:   key.caller,
			ReadOnly: key.readOnly,
			Count:    count,
		})
	}

	sort.Slice(counts, func(i, j int) bool {
		return counts[i].Count > counts[j].Count
	})

	return counts
}

// recordTxnCommit attributes one committed transaction to its owning call
// site. Only invoked when accounting is enabled; the caller walk costs about
// a microsecond, well below the cost of the commit it accounts for.
func recordTxnCommit(readOnly bool) {
	key := txnAccountKey{
		caller:   execTxCallerHint(),
		readOnly: readOnly,
	}

	txnAccountingMu.Lock()
	defer txnAccountingMu.Unlock()

	txnAccountingCounts[key]++
}
