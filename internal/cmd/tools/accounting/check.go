package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/vtxo"
)

// Check names. They are stable identifiers: admin scripts and CI jobs key on
// them, and the operator guide documents each one, so treat a rename as a
// breaking change to the tool's output.
const (
	checkTrialBalance    = "trial_balance"
	checkClearingClosure = "wallet_clearing_closure"
	checkVTXOInventory   = "vtxo_balance_inventory"
	checkWalletReconcile = "wallet_balance_audit_reconciliation"
	checkDedupIndexes    = "dedup_index_consistency"
	checkLegAmounts      = "outpoint_leg_amount_agreement"
	checkAuditPairing    = "audit_ledger_pairing"
	checkHistoryDistinct = "history_non_duplication"
	anomalyExitSendFlag  = "anomaly_exit_send_followed_flag"
	anomalySelfChangeIn  = "anomaly_oor_self_change_" +
		"as_transfers_in"
	anomalyDepositIntentGap       = "anomaly_deposit_boarding_intent_gap"
	historyPageLimit        int32 = 100_000
)

// ledgerEmittingSpentClassifications names the wallet_utxo_log 'spent'
// classifications whose handler also books a double-entry leg. Every other
// spent classification is audit-only, so the pairing check must not demand a
// ledger row for it. Keeping the set here rather than in SQL puts it next to
// the handler policy it mirrors (ledger.handleUTXOSpent).
var ledgerEmittingSpentClassifications = map[string]struct{}{
	ledger.ClassificationBoardingSweepInput: {},
	ledger.ClassificationDepositFunding:     {},
}

// auditOnlyCreatedClassifications names the 'created' classifications that
// deliberately book no ledger leg. Both are own-wallet proceeds whose value
// an earlier message already credited to wallet_balance -- the ExitCostMsg
// proceeds leg for an exit, the VTXOSentMsg proceeds leg for a leave -- so
// the audit row exists only to give those coins an identity a later boarding
// deposit can recognise. Recycled change is deliberately absent: it has no
// earlier producer and books its own credit leg.
var auditOnlyCreatedClassifications = map[string]struct{}{
	ledger.ClassificationExitProceeds:  {},
	ledger.ClassificationLeaveProceeds: {},
}

// booksLedgerLeg reports whether an audit row of this event and classification
// is expected to have a ledger leg alongside it.
func booksLedgerLeg(event, classification string) bool {
	switch event {
	case "created":
		_, auditOnly := auditOnlyCreatedClassifications[classification]

		return !auditOnly

	case "spent":
		_, emits := ledgerEmittingSpentClassifications[classification]

		return emits

	default:
		return false
	}
}

// checkFinding is one offending row or aggregate a check rejected. Fields
// carries the identifying columns so an operator can go straight to the row.
type checkFinding struct {
	Detail string            `json:"detail"`
	Fields map[string]string `json:"fields,omitempty"`
}

// checkResult is the outcome of one named invariant check.
type checkResult struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Passed      bool           `json:"passed"`
	Findings    []checkFinding `json:"findings,omitempty"`

	// Note records a caveat about what the check could actually prove,
	// for invariants the database alone cannot fully settle.
	Note string `json:"note,omitempty"`
}

// checkReport is the top-level document the check subcommand emits.
type checkReport struct {
	GeneratedAtUnix int64  `json:"generated_at_unix"`
	Backend         string `json:"backend"`

	// LedgerFingerprint is a content-only hash of the journal in entry-ID
	// order. It names no database, host or file, so a report taken on a
	// restored copy and one taken against the daemon carry the same hash
	// exactly when their journals are identical. EntryCount and
	// MaxEntryID are printed alongside it because the hash on its own
	// cannot tell an operator which database they were pointed at.
	LedgerFingerprint string `json:"ledger_fingerprint"`
	EntryCount        int64  `json:"entry_count"`
	MaxEntryID        int64  `json:"max_entry_id"`

	Checks     []checkResult `json:"checks"`
	Violations int           `json:"violations"`
	Passed     bool          `json:"passed"`
}

// runCheck reads the whole ledger inside one read-only transaction, evaluates
// every invariant against that single snapshot, and writes the result. It
// returns errCheckFailed when any check reported a violation so the process
// exits non-zero; a genuine error (bad flags, unreadable database) returns
// that error instead.
func runCheck(ctx context.Context, store *db.Store, cfg *config,
	w io.Writer) error {

	rep, err := buildCheckReport(ctx, store, cfg)
	if err != nil {
		return err
	}

	switch cfg.format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}

	case "text", "csv":
		// CSV is a report-only format; a check result is a list of
		// named outcomes rather than a table of line items, so the
		// human form serves both.
		if err := printCheckReport(w, rep); err != nil {
			return err
		}

	default:
		return fmt.Errorf("unknown --format %q", cfg.format)
	}

	if !rep.Passed {
		return errCheckFailed
	}

	return nil
}

// checkError marks a failed invariant run so main can exit non-zero without
// printing a spurious error line over the report the operator asked for.
type checkError struct{}

// Error returns the sentinel's message.
func (checkError) Error() string {
	return "accounting invariants violated"
}

// errCheckFailed is returned by runCheck when any check found a violation.
var errCheckFailed = checkError{}

// checkSnapshot holds every row the checks run against, read in one
// transaction so no two checks can disagree about the state of the ledger.
type checkSnapshot struct {
	journal      []sqlc.LedgerEntry
	keyed        []sqlc.LedgerEntry
	balances     []sqlc.ListClientAccountBalancesRow
	vtxosByState []sqlc.SumUnspentVTXOAmountsByStatusRow
	auditTotals  []sqlc.SumWalletUTXOLogByEventAndClassificationRow
	walletLegs   []sqlc.SumLedgerWalletLegsRow
	roundDups    []sqlc.ListLedgerRoundKeyDuplicatesRow
	sessionDups  []sqlc.ListLedgerSessionKeyDuplicatesRow
	keyDups      []sqlc.ListLedgerIdempotencyKeyDuplicatesRow
	unpairedLog  []sqlc.WalletUtxoLog
	unpairedLeg  []sqlc.ListWalletLedgerLegsWithoutAuditRowRow
	history      []sqlc.ListTransactionHistoryRow
	exitFlagRows []sqlc.ListFlagFollowingExitSendLegsRow
	selfChange   []sqlc.ListSelfChangeBookedAsTransfersInRow
	depositGap   []sqlc.ListDepositLegsWithoutBoardingIntentRow
	intentGap    []sqlc.ListBoardingIntentsWithoutDepositLegRow
}

// buildCheckReport loads the snapshot and evaluates every check over it.
func buildCheckReport(ctx context.Context, store *db.Store,
	cfg *config) (*checkReport, error) {

	snap, err := loadCheckSnapshot(ctx, store)
	if err != nil {
		return nil, err
	}

	rep := &checkReport{
		GeneratedAtUnix: time.Now().Unix(),
		Backend:         cfg.backend,
		LedgerFingerprint: hex.EncodeToString(
			journalHash(snap.journal),
		),
		EntryCount: int64(len(snap.journal)),
	}
	if n := len(snap.journal); n > 0 {
		rep.MaxEntryID = snap.journal[n-1].EntryID
	}

	rep.Checks = []checkResult{
		checkTrialBalanceCloses(snap),
		checkWalletClearingCloses(snap),
		checkVTXOBalanceMatchesInventory(snap),
		checkWalletBalanceAgainstAudit(snap),
		checkDedupIndexConsistency(snap),
		checkOutpointLegAmounts(snap),
		checkAuditLedgerPairing(snap),
		checkHistoryIsDistinct(snap),
		checkExitSendFollowedFlag(snap),
		checkSelfChangeBookedAsTransfersIn(snap),
		checkDepositBoardingIntentGap(snap),
	}

	rep.Passed = true
	for _, result := range rep.Checks {
		rep.Violations += len(result.Findings)
		if !result.Passed {
			rep.Passed = false
		}
	}

	return rep, nil
}

// loadCheckSnapshot reads every projection the checks need inside a single
// read-only transaction, so the whole run sees one consistent journal.
func loadCheckSnapshot(ctx context.Context,
	store *db.Store) (*checkSnapshot, error) {

	tx, err := store.BaseDB().BeginTx(ctx, db.ReadTxOption())
	if err != nil {
		return nil, fmt.Errorf("begin read tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	q := store.Queries().WithTx(tx)
	snap := &checkSnapshot{}

	// Each read is fatal on error: a check that silently ran against a
	// partial snapshot would report "passed" for an invariant it never
	// evaluated, which is worse than no answer at all.
	if snap.journal, err = q.ListLedgerEntriesForFingerprint(
		ctx,
	); err != nil {
		return nil, fmt.Errorf("list journal: %w", err)
	}
	if snap.keyed, err = q.ListKeyedLedgerEntries(ctx); err != nil {
		return nil, fmt.Errorf("list keyed entries: %w", err)
	}
	if snap.balances, err = q.ListClientAccountBalances(ctx); err != nil {
		return nil, fmt.Errorf("list account balances: %w", err)
	}
	if snap.vtxosByState, err = q.SumUnspentVTXOAmountsByStatus(
		ctx,
	); err != nil {
		return nil, fmt.Errorf("sum vtxos by status: %w", err)
	}
	if snap.auditTotals, err = q.SumWalletUTXOLogByEventAndClassification(
		ctx,
	); err != nil {
		return nil, fmt.Errorf("sum audit log: %w", err)
	}
	if snap.walletLegs, err = q.SumLedgerWalletLegs(ctx); err != nil {
		return nil, fmt.Errorf("sum wallet legs: %w", err)
	}
	if snap.roundDups, err = q.ListLedgerRoundKeyDuplicates(
		ctx,
	); err != nil {
		return nil, fmt.Errorf("list round duplicates: %w", err)
	}
	if snap.sessionDups, err = q.ListLedgerSessionKeyDuplicates(
		ctx,
	); err != nil {
		return nil, fmt.Errorf("list session duplicates: %w", err)
	}
	if snap.keyDups, err = q.ListLedgerIdempotencyKeyDuplicates(
		ctx,
	); err != nil {
		return nil, fmt.Errorf("list key duplicates: %w", err)
	}
	if snap.unpairedLog, err = q.ListWalletUTXOLogWithoutLedgerLeg(
		ctx,
	); err != nil {
		return nil, fmt.Errorf("list unpaired audit rows: %w", err)
	}
	if snap.unpairedLeg, err = q.ListWalletLedgerLegsWithoutAuditRow(
		ctx,
	); err != nil {
		return nil, fmt.Errorf("list unpaired ledger legs: %w", err)
	}
	if snap.history, err = q.ListTransactionHistory(
		ctx, sqlc.ListTransactionHistoryParams{
			TypeFilter: "",
			FromUnixS:  int64(0),
			ToUnixS:    int64(0),
			PageOffset: 0,
			PageLimit:  historyPageLimit,
		},
	); err != nil {
		return nil, fmt.Errorf("list transaction history: %w", err)
	}
	if snap.exitFlagRows, err = q.ListFlagFollowingExitSendLegs(
		ctx,
	); err != nil {
		return nil, fmt.Errorf("list flag-following exits: %w", err)
	}
	if snap.selfChange, err = q.ListSelfChangeBookedAsTransfersIn(
		ctx,
	); err != nil {
		return nil, fmt.Errorf("list self-change receives: %w", err)
	}
	if snap.depositGap, err = q.ListDepositLegsWithoutBoardingIntent(
		ctx,
	); err != nil {
		return nil, fmt.Errorf("list intentless deposits: %w", err)
	}
	if snap.intentGap, err = q.ListBoardingIntentsWithoutDepositLeg(
		ctx,
	); err != nil {
		return nil, fmt.Errorf("list depositless intents: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit read tx: %w", err)
	}

	return snap, nil
}

// journalHash fingerprints every journal field in entry order, one row at a
// time. Each field is length-prefixed and a NULL column is encoded with a
// length no real value can have, so two journals cannot collide by shifting
// bytes across a field boundary. The digest writers never fail, so their
// errors are dropped rather than threaded through every field.
func journalHash(entries []sqlc.LedgerEntry) []byte {
	digest := sha256.New()

	var scratch [8]byte
	writeInt := func(value int64) {
		binary.BigEndian.PutUint64(scratch[:], uint64(value))
		_, _ = digest.Write(scratch[:])
	}
	writeText := func(value string) {
		writeInt(int64(len(value)))
		_, _ = digest.Write([]byte(value))
	}
	writeBlob := func(value []byte) {
		if value == nil {
			writeInt(-1)

			return
		}
		writeInt(int64(len(value)))
		_, _ = digest.Write(value)
	}
	for _, entry := range entries {
		writeInt(entry.EntryID)
		writeText(entry.DebitAccount)
		writeText(entry.CreditAccount)
		writeInt(entry.AmountSat)
		writeBlob(entry.RoundID)
		writeBlob(entry.SessionID)
		writeBlob(entry.IdempotencyKey)
		writeText(entry.EventType)
		writeText(entry.Description)
		writeInt(entry.CreatedAt)
		writeBlob(entry.ChainTxid)

		// A NULL integer column is encoded as -1, a length no valid
		// vout or height carries.
		if entry.ChainVout.Valid {
			writeInt(int64(entry.ChainVout.Int32))
		} else {
			writeInt(-1)
		}
		if entry.ConfirmationHeight.Valid {
			writeInt(int64(entry.ConfirmationHeight.Int32))
		} else {
			writeInt(-1)
		}
		if entry.RoundUuid.Valid {
			writeText(entry.RoundUuid.String)
		} else {
			writeInt(-1)
		}
	}

	return digest.Sum(nil)
}

// accountBalanceMap indexes the signed account balances by account ID.
func accountBalanceMap(snap *checkSnapshot) map[string]int64 {
	out := make(map[string]int64, len(snap.balances))
	for _, row := range snap.balances {
		out[row.AccountID] = row.BalanceSat
	}

	return out
}

// checkTrialBalanceCloses verifies the double-entry closure property: every
// entry books one debit and one credit of the same amount, so the signed
// balances across the whole chart of accounts must sum to exactly zero. A
// non-zero total means a leg was written without its counterpart, or an
// account outside the seeded chart absorbed one side.
func checkTrialBalanceCloses(snap *checkSnapshot) checkResult {
	result := checkResult{
		Name: checkTrialBalance,
		Description: "signed balances across the chart of accounts " +
			"sum to zero",
		Passed: true,
	}

	// This check is close to vacuous by construction, and says so rather
	// than lending its pass more weight than it carries. Every account
	// column carries a foreign key into the chart of accounts, so the
	// out-of-chart branch below cannot fire on a schema-conformant
	// database; and each entry contributes its amount once as a debit and
	// once as a credit, so the signed total closes for any pair of
	// distinct accounts. The residue it can still catch is an entry whose
	// debit and credit name the same account, which no producer writes.
	// It runs because a full journal scan for a cheap tautology is a fair
	// price for noticing the day one of those assumptions stops holding.
	result.Note = "this invariant is enforced structurally by the " +
		"chart-of-accounts foreign keys and by double entry itself; " +
		"a pass here is weak evidence"

	var total int64
	for _, row := range snap.balances {
		total += row.BalanceSat
	}

	// A journal row naming an account the chart does not contain would be
	// invisible to the per-account sum, so count the covered rows too.
	covered := int64(0)
	known := make(map[string]struct{}, len(snap.balances))
	for _, row := range snap.balances {
		known[row.AccountID] = struct{}{}
	}
	for _, entry := range snap.journal {
		_, debitKnown := known[entry.DebitAccount]
		_, creditKnown := known[entry.CreditAccount]
		if debitKnown && creditKnown {
			covered++

			continue
		}

		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "entry names an account outside the chart " +
				"of accounts",
			Fields: map[string]string{
				"entry_id":       itoa64(entry.EntryID),
				"debit_account":  entry.DebitAccount,
				"credit_account": entry.CreditAccount,
			},
		})
	}

	if total != 0 {
		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "trial balance does not close",
			Fields: map[string]string{
				"signed_total_sat": itoa64(total),
				"entries_covered":  itoa64(covered),
			},
		})
	}

	return result
}

// checkWalletClearingCloses verifies that wallet_clearing nets to exactly
// zero. It is a transit account: every sweep debits it for the inputs it
// consumed and credits it for the fee and destination it paid out, so a
// non-zero balance means a sweep's legs were booked without their closing
// counterpart and value is stranded mid-sweep.
func checkWalletClearingCloses(snap *checkSnapshot) checkResult {
	result := checkResult{
		Name:        checkClearingClosure,
		Description: "wallet_clearing nets to exactly zero",
		Note: "a sweep in flight legitimately holds its inputs here " +
			"until it confirms, so a non-zero balance on a live " +
			"daemon may simply be an unconfirmed sweep rather " +
			"than stranded value; re-run once the sweep confirms " +
			"before treating it as a violation",
		Passed: true,
	}

	balance := accountBalanceMap(snap)[ledger.AccountWalletClearing]
	if balance != 0 {
		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "wallet_clearing holds a residual balance",
			Fields: map[string]string{
				"balance_sat": itoa64(balance),
			},
		})
	}

	return result
}

// synchronousVTXOStatuses are the unspent VTXO statuses whose value the
// ledger's vtxo_balance is meant to hold right now, because every leg that
// removes such a VTXO is booked in the same operation that changes its
// status.
//
// Live, PendingForfeit and Forfeiting are the client's plain holdings.
// Spending is a VTXO claimed for an out-of-round spend that has not
// completed. Expired has passed its batch expiry but the value is still
// reclaimable by forfeiting into an ordinary round, and no leg has removed
// it, so both sides still carry it.
var synchronousVTXOStatuses = map[int32]struct{}{
	int32(vtxo.VTXOStatusLive):           {},
	int32(vtxo.VTXOStatusPendingForfeit): {},
	int32(vtxo.VTXOStatusForfeiting):     {},
	int32(vtxo.VTXOStatusSpending):       {},
	int32(vtxo.VTXOStatusExpired):        {},
}

// reconcilingVTXOStatuses are the unspent statuses whose ledger removal is
// not synchronous with the status change, so the two sides legitimately
// disagree by their value for a while, or permanently.
//
// UnilateralExit flips at exit initiation, but the leg that removes the value
// is handleExitCost, driven only once the sweep confirms -- a full CSV delay
// later. Failed has no removing leg at all. Reporting either as a violation
// would make the tool cry wolf on a healthy database, so the check names them
// as reconciling items and accepts a vtxo_balance anywhere in the band they
// span.
var reconcilingVTXOStatuses = map[int32]string{
	int32(vtxo.VTXOStatusUnilateralExit): "exiting",
	int32(vtxo.VTXOStatusFailed):         "failed",
}

// checkVTXOBalanceMatchesInventory verifies the ledger's vtxo_balance against
// the VTXO table.
//
// The comparison is a band, not an equality. vtxo_balance must be at least
// the value of the VTXOs whose ledger removal is synchronous with their
// status (see synchronousVTXOStatuses), and at most that plus the VTXOs the
// ledger has not removed yet or will never remove (see
// reconcilingVTXOStatuses). Both bounds are reported either way, because an
// operator reconciling by hand needs to see the reconciling items whether or
// not the check passed.
func checkVTXOBalanceMatchesInventory(snap *checkSnapshot) checkResult {
	result := checkResult{
		Name: checkVTXOInventory,
		Description: "ledger vtxo_balance equals the live VTXO " +
			"inventory, up to the statuses the ledger has not " +
			"removed yet",
		Passed: true,
	}

	var (
		inventory, inventoryCount int64
		reconciling               int64
		byName                    = make(map[string]int64)
	)
	for _, row := range snap.vtxosByState {
		if _, ok := synchronousVTXOStatuses[row.Status]; ok {
			inventory += row.AmountSat
			inventoryCount += row.VtxoCount

			continue
		}

		// A status in neither set is one this check has not been
		// taught about, most likely a newly added enum value.
		// Counting it into the reconciling band widens the upper
		// bound rather than narrowing the check to silence, and
		// naming it in the Note is what tells an operator why the
		// band grew.
		name, ok := reconcilingVTXOStatuses[row.Status]
		if !ok {
			name = fmt.Sprintf("unrecognised status %q", row.Status)
		}

		reconciling += row.AmountSat
		byName[name] += row.AmountSat
	}

	balance := accountBalanceMap(snap)[ledger.AccountVTXOBalance]
	if balance < inventory || balance > inventory+reconciling {
		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "vtxo_balance falls outside the band the " +
				"VTXO table allows",
			Fields: map[string]string{
				"ledger_vtxo_balance_sat": itoa64(balance),
				"live_vtxo_sum_sat":       itoa64(inventory),
				"live_vtxo_count": itoa64(
					inventoryCount,
				),
				"reconciling_sum_sat": itoa64(reconciling),
				"difference_sat": itoa64(
					balance - inventory,
				),
			},
		})
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	items := make([]string, 0, len(names))
	for _, name := range names {
		items = append(
			items, fmt.Sprintf("%s=%d sat", name, byName[name]),
		)
	}
	if len(items) > 0 {
		result.Note = "vtxo_balance still carries these unspent " +
			"VTXOs whose removing leg has not run: " +
			strings.Join(items, ", ")
	}

	// A VTXO the operator swept after its batch expiry is counted by both
	// sides forever: nothing in the client observes the operator's sweep,
	// so neither the status nor the ledger moves. The check cannot see
	// that, and says so rather than implying it proved more than it did.
	result.Note += " (an expired VTXO the operator has already swept " +
		"stays counted on both sides, which this check cannot detect)"

	return result
}

// checkWalletBalanceAgainstAudit reconciles the wallet-facing figure the
// database can actually produce. The daemon database holds no wallet UTXO
// set: the backing wallet owns that, and the three supported backends expose
// it only over their own RPC. What the database does hold is the
// wallet_utxo_log audit trail, so this check reconciles the ledger's
// wallet_utxo_created and wallet_utxo_spent legs against the audit rows that
// are supposed to have produced them, and reports the resulting
// audit-derived figure alongside the wallet_balance account. A mismatch means
// one of the two writes in handleUTXOCreated / handleUTXOSpent landed without
// the other.
func checkWalletBalanceAgainstAudit(snap *checkSnapshot) checkResult {
	result := checkResult{
		Name: checkWalletReconcile,
		Description: "ledger wallet UTXO legs reconcile with the " +
			"wallet UTXO audit log",
		Note: "the daemon database holds no wallet UTXO set, so this " +
			"is an audit-log-derived reconciliation and not a " +
			"chain UTXO-set reconciliation; wallet_balance also " +
			"moves on boarding, operator fees and exit proceeds, " +
			"which the audit log does not record",
		Passed: true,
	}

	var auditCreated, auditSpent int64
	for _, row := range snap.auditTotals {
		if !booksLedgerLeg(row.Event, row.ClassifiedAs) {
			continue
		}

		switch row.Event {
		case "created":
			auditCreated += row.TotalSat

		case "spent":
			auditSpent += row.TotalSat
		}
	}

	var ledgerCreated, ledgerSpent int64
	for _, row := range snap.walletLegs {
		switch {
		case row.EventType == ledger.EventWalletUTXOCreated &&
			row.WalletSide == "debit":

			ledgerCreated += row.TotalSat

		case row.EventType == ledger.EventWalletUTXOSpent &&
			row.WalletSide == "credit":

			ledgerSpent += row.TotalSat
		}
	}

	if auditCreated != ledgerCreated {
		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "wallet_utxo_created legs disagree with the " +
				"audit log's created rows",
			Fields: map[string]string{
				"audit_created_sat":  itoa64(auditCreated),
				"ledger_created_sat": itoa64(ledgerCreated),
			},
		})
	}
	if auditSpent != ledgerSpent {
		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "wallet_utxo_spent legs disagree with the " +
				"audit log's ledger-emitting spent rows",
			Fields: map[string]string{
				"audit_spent_sat":  itoa64(auditSpent),
				"ledger_spent_sat": itoa64(ledgerSpent),
			},
		})
	}

	// Report the derived figures whether or not the check passed: an
	// operator comparing against their wallet's own balance needs both.
	result.Note += fmt.Sprintf(" (wallet_balance=%d sat, audit-derived "+
		"net=%d sat)",
		accountBalanceMap(snap)[ledger.AccountWalletBalance],
		auditCreated-auditSpent)

	return result
}

// checkDedupIndexConsistency mirrors the three partial unique indexes on
// ledger_entries in migration 000006. Each one keys on
// (event_type, debit_account, credit_account) under a round, a session, or an
// explicit idempotency key, and each is what makes at-least-once durable
// delivery safe. A duplicate here means either the index is missing from the
// deployed schema or a row was written around it.
func checkDedupIndexConsistency(snap *checkSnapshot) checkResult {
	result := checkResult{
		Name: checkDedupIndexes,
		Description: "no two entries share a dedup tuple under an " +
			"equal round, session or idempotency key",
		Passed: true,
	}

	for _, row := range snap.roundDups {
		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "duplicate round-keyed dedup tuple",
			Fields: map[string]string{
				"round_id": hex.EncodeToString(
					row.RoundID,
				),
				"event_type":     row.EventType,
				"debit_account":  row.DebitAccount,
				"credit_account": row.CreditAccount,
				"entry_count":    itoa64(row.EntryCount),
			},
		})
	}
	for _, row := range snap.sessionDups {
		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "duplicate session-keyed dedup tuple",
			Fields: map[string]string{
				"session_id": hex.EncodeToString(
					row.SessionID,
				),
				"event_type":     row.EventType,
				"debit_account":  row.DebitAccount,
				"credit_account": row.CreditAccount,
				"entry_count":    itoa64(row.EntryCount),
			},
		})
	}
	for _, row := range snap.keyDups {
		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "duplicate idempotency-keyed dedup tuple",
			Fields: map[string]string{
				"idempotency_key": keyString(
					row.IdempotencyKey,
				),
				"event_type":     row.EventType,
				"debit_account":  row.DebitAccount,
				"credit_account": row.CreditAccount,
				"entry_count":    itoa64(row.EntryCount),
			},
		})
	}

	return result
}

// legGroup collects the legs one outpoint-scoped operation booked.
type legGroup struct {
	operation string
	payload   string
	legs      map[string]int64
}

// checkOutpointLegAmounts verifies that the legs of one outpoint-scoped
// operation agree on the amount they are supposed to share. A unilateral
// exit's send and proceeds legs both carry the net-of-fee value, and a leave's
// send and proceeds legs likewise: the proceeds leg exists precisely to cancel
// the send leg on transfers_out, so a disagreement leaves a residue on
// transfers_out that no later event clears. The fee leg deliberately carries a
// different amount and is excluded.
func checkOutpointLegAmounts(snap *checkSnapshot) checkResult {
	result := checkResult{
		Name: checkLegAmounts,
		Description: "cancelling legs of one outpoint-scoped " +
			"operation agree in amount",
		Passed: true,
	}

	// A leave's send leg carries the producer's own round-outflow key with
	// no versioned prefix, and its proceeds leg scopes that whole key under
	// the proceeds leg name. Index the unversioned keys so the proceeds leg
	// can be paired back to the send leg it is supposed to cancel.
	rawAmounts := make(map[string]int64, len(snap.keyed))
	for _, entry := range snap.keyed {
		if _, _, _, versioned := parseLedgerKey(
			entry.IdempotencyKey,
		); versioned {

			continue
		}
		rawAmounts[string(entry.IdempotencyKey)] = entry.AmountSat
	}

	groups := make(map[string]*legGroup)
	var order []string
	for _, entry := range snap.keyed {
		operation, leg, payload, ok := parseLedgerKey(
			entry.IdempotencyKey,
		)
		if !ok {
			continue
		}

		// The send-proceeds key wraps the send leg's raw key rather
		// than an outpoint, so resolve the send amount directly.
		if operation == operationSend && leg == legNameProceeds {
			send, known := rawAmounts[rawSendKey(
				entry.IdempotencyKey,
			)]
			if !known || send == entry.AmountSat {
				continue
			}

			result.Passed = false
			result.Findings = append(
				result.Findings, checkFinding{
					Detail: "send and proceeds legs " +
						"disagree in amount",
					Fields: map[string]string{
						"operation": operation,
						"payload":   payload,
						"send_sat":  itoa64(send),
						"proceeds_sat": itoa64(
							entry.AmountSat,
						),
					},
				},
			)

			continue
		}

		id := operation + ":" + payload
		group := groups[id]
		if group == nil {
			group = &legGroup{
				operation: operation,
				payload:   payload,
				legs:      make(map[string]int64),
			}
			groups[id] = group
			order = append(order, id)
		}
		group.legs[leg] = entry.AmountSat
	}

	sort.Strings(order)
	for _, id := range order {
		group := groups[id]

		send, hasSend := group.legs[legNameSend]
		proceeds, hasProceeds := group.legs[legNameProceeds]
		if !hasSend || !hasProceeds || send == proceeds {
			continue
		}

		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "send and proceeds legs disagree in amount",
			Fields: map[string]string{
				"operation":    group.operation,
				"payload":      group.payload,
				"send_sat":     itoa64(send),
				"proceeds_sat": itoa64(proceeds),
			},
		})
	}

	return result
}

// Leg names used by the versioned idempotency keys. They must match the
// constants in the ledger package; the checker parses rather than imports
// them because they are unexported there and the encoding, not the Go
// identifier, is what the persisted rows commit to.
const (
	ledgerKeyPrefix = "ledger:v1:"
	legNameSend     = "send"
	legNameProceeds = "proceeds"
	operationSend   = "send"
)

// rawSendKey recovers the send leg's own idempotency key from the proceeds
// leg's key, which is that key scoped under the versioned proceeds prefix.
func rawSendKey(proceedsKey []byte) string {
	prefix := ledgerKeyPrefix + operationSend + ":" + legNameProceeds + ":"

	return string(proceedsKey[len(prefix):])
}

// parseLedgerKey splits a versioned "ledger:v1:<operation>:<leg>:<payload>"
// idempotency key. It returns false for the plain outpoint payloads and
// caller-supplied keys that carry no versioned prefix.
func parseLedgerKey(key []byte) (string, string, string, bool) {
	if !strings.HasPrefix(string(key), ledgerKeyPrefix) {
		return "", "", "", false
	}

	rest := key[len(ledgerKeyPrefix):]
	operationEnd := bytes.IndexByte(rest, ':')
	if operationEnd < 0 {
		return "", "", "", false
	}
	operation := string(rest[:operationEnd])

	rest = rest[operationEnd+1:]
	legEnd := bytes.IndexByte(rest, ':')
	if legEnd < 0 {
		return "", "", "", false
	}
	leg := string(rest[:legEnd])

	payload := hex.EncodeToString(rest[legEnd+1:])

	return operation, leg, payload, true
}

// checkAuditLedgerPairing verifies that every wallet_utxo_log row with a
// ledger-emitting classification has its ledger leg and vice versa. Both
// writes happen in one transaction inside handleUTXOCreated /
// handleUTXOSpent, so a row on one side alone means the pair was split by
// something outside those handlers.
func checkAuditLedgerPairing(snap *checkSnapshot) checkResult {
	result := checkResult{
		Name: checkAuditPairing,
		Description: "wallet UTXO audit rows and their ledger legs " +
			"exist in pairs",
		Passed: true,
	}

	for _, row := range snap.unpairedLog {
		// Audit-only classifications are expected to appear with no
		// ledger leg; only the ledger-emitting ones are violations.
		if !booksLedgerLeg(row.Event, row.ClassifiedAs) {
			continue
		}

		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "audit row has no ledger leg",
			Fields: map[string]string{
				"audit_entry_id": itoa64(row.EntryID),
				"outpoint": outpointString(
					row.OutpointHash, row.OutpointIndex,
				),
				"event":         row.Event,
				"classified_as": row.ClassifiedAs,
				"amount_sat":    itoa64(row.AmountSat),
			},
		})
	}

	for _, row := range snap.unpairedLeg {
		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "ledger leg has no audit row",
			Fields: map[string]string{
				"entry_id":   itoa64(row.EntryID),
				"event_type": row.EventType,
				"outpoint": outpointString(
					row.ChainTxid, row.ChainVout.Int32,
				),
				"amount_sat": itoa64(row.AmountSat),
			},
		})
	}

	return result
}

// checkHistoryIsDistinct verifies that ListTransactionHistory returns no two
// chain-identified rows with the same (txid, output_index, amount_sat,
// subtype). The history query unions the ledger with OOR bindings and
// boarding sweeps, and it excludes the own-wallet contra legs that share a
// send leg's chain identity; a duplicate here means a consumer would show the
// same on-chain movement twice.
//
// Only rows carrying a txid and a non-negative output index are examined. A
// chain outpoint is a real identity: one output confirms once, so two rows
// naming it with the same subtype describe one movement. Round-level rows
// have no such identity. Their txid is empty and their output index is the
// -1 sentinel, and the projected columns cannot tell two legitimate
// equal-amount recipients in one round apart from a leg booked twice.
// Reporting the guess would fail healthy journals, so those rows are left to
// the ledger-level checks, which see the idempotency keys.
func checkHistoryIsDistinct(snap *checkSnapshot) checkResult {
	result := checkResult{
		Name: checkHistoryDistinct,
		Description: "transaction history holds no two " +
			"chain-identified rows with the same txid, output " +
			"index, amount and subtype",
		Passed: true,
	}

	// The history query pages once, so a journal longer than the page
	// limit is only partly examined. Say so rather than reporting a clean
	// pass over a prefix.
	if int32(len(snap.history)) >= historyPageLimit {
		result.Note = fmt.Sprintf("history was truncated at the "+
			"%d-row page limit, so only the first %d rows were "+
			"examined; duplicates beyond that point are not "+
			"covered by this pass", historyPageLimit,
			len(snap.history))
	}

	seen := make(map[string]int, len(snap.history))
	for _, row := range snap.history {
		if len(row.Txid) == 0 || row.OutputIndex < 0 {
			continue
		}

		id := fmt.Sprintf("%x|%d|%d|%s", row.Txid, row.OutputIndex,
			row.AmountSat, row.Subtype)
		seen[id]++
		if seen[id] != 2 {
			continue
		}

		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "duplicate transaction history row",
			Fields: map[string]string{
				"txid":         hex.EncodeToString(row.Txid),
				"output_index": itoa64(int64(row.OutputIndex)),
				"amount_sat":   itoa64(row.AmountSat),
				"subtype":      row.Subtype,
			},
		})
	}

	return result
}

// checkExitSendFollowedFlag reports the first known anomaly: an exit send leg
// whose debit account followed the own-wallet destination flag, so it reads
// "wallet_balance <- vtxo_balance" under a vtxo_sent event. Because the
// accounts are part of the dedup tuple, such a row lands in a different tuple
// than its correctly-shaped twin and both can persist, crediting vtxo_balance
// twice for one exit. Current code fixes the send leg's accounts and books the
// own-wallet movement in a separately keyed proceeds leg, so any row in this
// shape predates that fix and still sits in the database.
func checkExitSendFollowedFlag(snap *checkSnapshot) checkResult {
	result := checkResult{
		Name: anomalyExitSendFlag,
		Description: "no exit send leg books its debit account from " +
			"the destination flag",
		Passed: true,
	}

	for _, row := range snap.exitFlagRows {
		result.Passed = false
		detail := "exit send leg debited wallet_balance"
		if row.TwinCount > 0 {
			detail += " and a correctly-shaped twin also exists, " +
				"so vtxo_balance is credited twice"
		}

		result.Findings = append(result.Findings, checkFinding{
			Detail: detail,
			Fields: map[string]string{
				"entry_id":   itoa64(row.EntryID),
				"amount_sat": itoa64(row.AmountSat),
				"outpoint": outpointString(
					row.ChainTxid, row.ChainVout.Int32,
				),
				"idempotency_key": keyString(
					row.IdempotencyKey,
				),
				"twin_count": itoa64(row.TwinCount),
			},
		})
	}

	return result
}

// checkSelfChangeBookedAsTransfersIn reports the second known anomaly: an OOR
// receive booked as transfers_in under a session that also carries an
// outgoing vtxo_sent leg. Such a receive is the sender's own change coming
// back, so booking it as revenue inflates both gross directions by the change
// amount. Current code derives the self-change case from the ledger's own
// rows and credits transfers_out instead.
func checkSelfChangeBookedAsTransfersIn(snap *checkSnapshot) checkResult {
	result := checkResult{
		Name: anomalySelfChangeIn,
		Description: "no OOR receive is booked as transfers_in under " +
			"a session that already sent",
		Passed: true,
	}

	for _, row := range snap.selfChange {
		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "own OOR change booked as transfers_in",
			Fields: map[string]string{
				"entry_id":   itoa64(row.EntryID),
				"amount_sat": itoa64(row.AmountSat),
				"session_id": hex.EncodeToString(
					row.SessionID,
				),
				"outpoint": outpointString(
					row.ChainTxid, row.ChainVout.Int32,
				),
			},
		})
	}

	return result
}

// checkDepositBoardingIntentGap reports the third known anomaly in both
// directions: a boarding deposit audit row whose outpoint has no boarding
// intent, and a boarding intent with no wallet_utxo_created ledger leg. The
// deposit leg is what balances the later boarding outflow, so either gap
// drifts wallet_balance by the deposit amount.
func checkDepositBoardingIntentGap(snap *checkSnapshot) checkResult {
	result := checkResult{
		Name: anomalyDepositIntentGap,
		Description: "every boarding deposit leg has its boarding " +
			"intent and every intent has its deposit leg",
		Passed: true,
	}

	for _, row := range snap.depositGap {
		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "deposit-classified audit row has no " +
				"boarding intent",
			Fields: map[string]string{
				"audit_entry_id": itoa64(row.EntryID),
				"outpoint": outpointString(
					row.OutpointHash, row.OutpointIndex,
				),
				"amount_sat": itoa64(row.AmountSat),
			},
		})
	}

	for _, row := range snap.intentGap {
		result.Passed = false
		result.Findings = append(result.Findings, checkFinding{
			Detail: "boarding intent has no deposit ledger leg",
			Fields: map[string]string{
				"outpoint": outpointString(
					row.OutpointHash, row.OutpointIndex,
				),
				"amount_sat": itoa64(row.Amount),
				"status":     row.Status,
			},
		})
	}

	return result
}

// printCheckReport writes the operator-readable form of a check run. Every
// value it prints comes from the operator's own database and goes to their
// terminal, so the taint analyser's XSS finding does not apply.
func printCheckReport(w io.Writer, rep *checkReport) error {
	_, err := fmt.Fprintf(
		w, "Accounting invariant check (%s backend)\n", rep.Backend,
	)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(
		w, "Ledger fingerprint: %s\n", rep.LedgerFingerprint,
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(
		w, "Entries: %d (max entry_id %d)\n\n", rep.EntryCount,
		rep.MaxEntryID,
	); err != nil {
		return err
	}

	for _, result := range rep.Checks {
		status := "PASS"
		if !result.Passed {
			status = "FAIL"
		}
		if _, err := fmt.Fprintf(
			w, "%-4s %s\n     %s\n", status, result.Name,
			result.Description,
		); err != nil {
			return err
		}
		if result.Note != "" {
			if _, err := fmt.Fprintf(w, "     note: %s\n",
				result.Note); err != nil {
				return err
			}
		}
		for _, finding := range result.Findings {
			if _, err := fmt.Fprintf(
				w, "     - %s %s\n", finding.Detail,
				fieldString(finding.Fields),
			); err != nil {
				return err
			}
		}
	}

	outcome := "PASSED"
	if !rep.Passed {
		outcome = "FAILED"
	}
	_, err = fmt.Fprintf(
		w, "\n%s: %d violation(s) across %d checks\n", outcome,
		rep.Violations, len(rep.Checks),
	)

	return err
}

// fieldString renders a finding's fields in a stable, sorted order.
func fieldString(fields map[string]string) string {
	if len(fields) == 0 {
		return ""
	}

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+fields[k])
	}

	return "[" + strings.Join(parts, " ") + "]"
}

// outpointString renders a chain identity as txid:vout, tolerating the NULL
// columns a non-chain entry leaves behind.
func outpointString(txid []byte, vout int32) string {
	if len(txid) == 0 {
		return ""
	}

	return fmt.Sprintf("%x:%d", txid, vout)
}

// keyString renders an idempotency key readably: the versioned keys carry a
// human-readable textual prefix followed by opaque binary, so the prefix is
// printed as text and the payload as hex.
func keyString(key []byte) string {
	operation, leg, payload, ok := parseLedgerKey(key)
	if !ok {
		return hex.EncodeToString(key)
	}

	return fmt.Sprintf("%s%s:%s:%s", ledgerKeyPrefix, operation, leg,
		payload)
}

// itoa64 formats an int64 in base 10.
func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}
