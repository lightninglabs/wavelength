//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// project writes one emitted WalletEntry into the canonical activity log: it
// upserts the current-state row and appends the transition event, atomically.
// It returns the event_seq and effective entry assigned to the appended
// transition with a nil error on success, (0, entry, nil) when projection is
// intentionally skipped because no store or canonical id exists, (0,
// effectiveEntry, nil) for a change-suppressed no-op, and (0, nil, err) when
// the durable write failed. The effective entry carries immutable context
// merged from the current durable row, so persisted and live snapshots agree.
// A caller stamps a live update with the returned seq and can gate a follow-up
// side effect on the row actually landing. When no store is wired, projection
// is a no-op and the legacy derive-on-read path continues unchanged.
func (r *Runtime) project(ctx context.Context,
	entry *wavewalletrpc.WalletEntry) (int64, *wavewalletrpc.WalletEntry,
	error) {

	if r.deps == nil || r.deps.ActivityStore == nil {
		return 0, entry, nil
	}

	// A row with no canonical id cannot be keyed; skip it, matching the
	// id-guard the pending tracker already applies. Zero-conf boarding rows
	// are also ephemeral live state recomputed from wallet UTXOs. Even when
	// one has the stable deposit-<address> id, persisting it could strand a
	// PENDING row if the transaction never confirms. The later ledger row
	// has a txid/height and is therefore projected under that same id.
	if entry.GetId() == "" ||
		entry.GetId() == syntheticBoardingUnconfirmedID ||
		isUnconfirmedBoardingOverlay(entry) {
		return 0, entry, nil
	}

	effectiveEntry := proto.Clone(entry).(*wavewalletrpc.WalletEntry)
	existingRow, err := r.deps.ActivityStore.GetEntry(ctx, entry.GetId())
	switch {
	case err == nil:
		existingEntry, err := rowToWalletEntry(existingRow)
		if err != nil {
			return 0, nil, fmt.Errorf("decode existing "+
				"activity row: %w", err)
		}
		effectiveEntry = mergeActivityContext(
			existingEntry, effectiveEntry,
		)

	case errors.Is(err, sql.ErrNoRows):
	default:
		return 0, nil, fmt.Errorf("read existing activity row: %w", err)
	}

	projection, err := entryToProjection(effectiveEntry)
	if err != nil {
		r.deps.resolveLog().WarnS(ctx, "Activity projection skipped: "+
			"encode failed", err)

		return 0, nil, err
	}

	seq, err := r.deps.ActivityStore.ProjectEntry(ctx, projection)
	if err != nil {
		r.deps.resolveLog().WarnS(ctx, "Activity projection failed",
			err,
		)

		return 0, nil, err
	}

	return seq, effectiveEntry, nil
}

// mergeActivityContext carries immutable request and correlation context from
// the durable current row into a later sparse lifecycle projection. The next
// projection remains authoritative for mutable status, amount, phase, and
// failure fields.
func mergeActivityContext(
	existing, next *wavewalletrpc.WalletEntry) *wavewalletrpc.WalletEntry {

	if next.GetNote() == "" {
		next.Note = existing.GetNote()
	}
	if next.GetRequest() == nil && existing.GetRequest() != nil {
		next.Request =
			proto.Clone(existing.GetRequest()).(*wavewalletrpc.WalletEntryRequest)
	}
	if next.GetCreatedAtUnix() == 0 {
		next.CreatedAtUnix = existing.GetCreatedAtUnix()
	}

	existingProgress := existing.GetProgress()
	if existingProgress == nil {
		return next
	}
	if next.Progress == nil {
		next.Progress = &wavewalletrpc.WalletEntryProgress{}
	}
	if next.Progress.GetPaymentHash() == "" {
		next.Progress.PaymentHash = existingProgress.GetPaymentHash()
	}
	if next.Progress.GetTxid() == "" {
		next.Progress.Txid = existingProgress.GetTxid()
	}
	if next.Progress.GetConfirmationHeight() == 0 {
		next.Progress.ConfirmationHeight =
			existingProgress.GetConfirmationHeight()
	}
	if next.Progress.GetVtxoOutpoint() == "" {
		next.Progress.VtxoOutpoint = existingProgress.GetVtxoOutpoint()
	}

	return next
}

// isUnconfirmedBoardingOverlay identifies the address-scoped live row without
// relying on its id. Confirmed-but-still-boarding deposits share the same id
// and PENDING status, but carry a txid and confirmation height and must land in
// the canonical store.
func isUnconfirmedBoardingOverlay(entry *wavewalletrpc.WalletEntry) bool {
	return entry.GetKind() == wavewalletrpc.EntryKind_ENTRY_KIND_DEPOSIT &&
		entry.GetStatus() == wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING &&
		entry.GetProgress().GetPhase() == wavewalletrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_CONFIRMATION &&
		entry.GetProgress().GetTxid() == "" &&
		entry.GetProgress().GetConfirmationHeight() == 0
}

// projectAndEmit projects the entry into the canonical activity log and then
// fans it out to live subscribers. Projection runs first so a row is durable
// before any subscriber observes it.
//
// With a canonical store, a live update carries the event_seq the transition
// was assigned, and only real transitions (seq > 0) are emitted: a
// change-suppressed re-projection is not a new event, and a failed projection
// is not durable, so neither reaches subscribers — they recover from the event
// log or a List reconcile. Without a store there is no event log, so it emits
// best-effort at seq 0 for a live-only stream.
func (r *Runtime) projectAndEmit(ctx context.Context,
	entry *wavewalletrpc.WalletEntry) {

	r.projectEmitLocked(ctx, entry)
}

// projectEmitLocked projects entry and fans it out, holding projectMu across
// both so the event_seq assigned in the projection and the emit that carries it
// stay in the same order across concurrent producers. It returns the projected
// seq (0 when change-suppressed / no store) and the projection error, so the
// reconciler can gate its pending-clear on a durable write. See projectMu.
func (r *Runtime) projectEmitLocked(ctx context.Context,
	entry *wavewalletrpc.WalletEntry) (int64, error) {

	r.projectMu.Lock()
	defer r.projectMu.Unlock()

	seq, effectiveEntry, err := r.project(ctx, entry)

	switch {
	case r.deps == nil || r.deps.ActivityStore == nil:
		r.emit(0, effectiveEntry)

	case seq > 0:
		r.emit(seq, effectiveEntry)
	}

	return seq, err
}

// projectDerivedPage re-projects one page of derived entries into the canonical
// store, shared by the full-history (reprojectActivity) and bounded-window
// (reprojectRecentActivity) reconcile passes. projectEmitLocked serializes the
// project+emit so a reconciled transition (a confirmed deposit, a forfeited
// leave) reaches live subscribers in seq order; a change-suppressed no-op fans
// out nothing. A per-entry failure is best-effort — skipped and retried on the
// next pass — and the pending record is cleared only after a durable project,
// so a partial page never strands or corrupts a row.
func (r *Runtime) projectDerivedPage(ctx context.Context,
	entries []*wavewalletrpc.WalletEntry) {

	for _, entry := range entries {
		if _, err := r.projectEmitLocked(ctx, entry); err != nil {
			continue
		}

		r.clearProjectedTerminalExit(entry)
	}
}

// reprojectActivity pages the derive-on-read feed and projects every row into
// the canonical activity store, returning the number of rows projected. Because
// ProjectEntry is idempotent on canonical_id and suppresses no-op changes,
// re-running observes any terminal transition since the last pass (a confirmed
// deposit, a forfeited leave, a completed unroll) while leaving unchanged rows
// untouched. It backs both the one-time startup backfill and the ongoing
// reconciler. Returns (0, nil) when no store is wired.
//
// kinds optionally restricts the derived feed: the startup backfill passes nil
// to seed every kind, while the reconciler passes only the low-volume
// DEPOSIT/EXIT producers here and reconciles the high-volume SEND/RECV kinds
// separately over a bounded window (reprojectRecentActivity).
//
// A wallet-local EXIT's pending record is cleared only from here and
// reprojectRecentActivity (via projectDerivedPage), and only after its terminal
// row is durably projected — never from the read/derive path — so a failed
// projection or a row that paged out is retried on a later pass instead of
// being stranded PENDING in the store.
func (r *Runtime) reprojectActivity(ctx context.Context,
	kinds []wavewalletrpc.EntryKind) (int, error) {

	if r.deps == nil || r.deps.ActivityStore == nil {
		return 0, nil
	}

	h := newHistory(r.deps, r)
	limit := r.deps.resolveMaxListLimit()

	var (
		offset    uint32
		projected int
	)
	for {
		list, err := h.deriveActivity(ctx, &wavewalletrpc.ListRequest{
			Limit:  limit,
			Offset: offset,
			Kinds:  kinds,
		})
		if err != nil {
			return projected, err
		}

		entries := list.GetEntries()

		// Guard against a non-advancing loop: an empty page (or a
		// zero-resolved limit) would otherwise never advance the offset
		// nor hit the length/total termination below.
		if len(entries) == 0 {
			break
		}

		r.projectDerivedPage(ctx, entries)
		projected += len(entries)

		offset += uint32(len(entries))
		if uint32(len(entries)) < limit || offset >= list.GetTotal() {
			break
		}
	}

	return projected, nil
}

// reprojectRecentActivity re-derives and re-projects only the most recent
// `limit` rows of the given kinds — the single-page counterpart to
// reprojectActivity's full-history paging loop. It exists for the high-volume
// SEND/RECV kinds, where a raw out-of-round send/receive has no live projector
// and must be reconciled into the store (issue #903), but the full history
// grows without bound. Deriving still reads the history sources, but only the
// top `limit` rows are projected, so the per-pass ProjectEntry (DB read) work
// is bounded at O(limit) instead of O(N). A raw OOR settles immediately and
// sorts to the top by updated_at, so the recent window catches it; older
// SEND/RECV rows are terminal and immutable, and change suppression makes an
// already-stored row a no-op regardless. It returns the number of rows
// processed and a nil error on success. A per-row projection failure is
// best-effort: it is skipped and retried on the next pass, so a partial pass
// never strands a row.
func (r *Runtime) reprojectRecentActivity(ctx context.Context,
	kinds []wavewalletrpc.EntryKind, limit uint32) (int, error) {

	if r.deps == nil || r.deps.ActivityStore == nil {
		return 0, nil
	}

	h := newHistory(r.deps, r)
	list, err := h.deriveActivity(ctx, &wavewalletrpc.ListRequest{
		Limit:  limit,
		Offset: 0,
		Kinds:  kinds,
	})
	if err != nil {
		return 0, err
	}

	entries := list.GetEntries()
	r.projectDerivedPage(ctx, entries)

	return len(entries), nil
}

// clearProjectedTerminalExit drops a wallet-local EXIT's in-memory pending
// record once its terminal row has been durably projected. A cooperative-leave
// EXIT exists only in the pending map, so this must run strictly after a
// successful project (not while decorating): the store now holds the terminal
// row, and dropping the record stops later passes from re-decorating a settled
// row. It is a no-op for ids not in the map (e.g. unilateral rows synthesized
// from ListVTXOs) and for non-terminal or non-EXIT rows.
func (r *Runtime) clearProjectedTerminalExit(entry *wavewalletrpc.WalletEntry) {
	if entry.GetKind() != wavewalletrpc.EntryKind_ENTRY_KIND_EXIT {
		return
	}

	switch entry.GetStatus() {
	case wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		wavewalletrpc.EntryStatus_ENTRY_STATUS_FAILED:

		r.clearPending(entry.GetId())
	}
}

// backfillActivity seeds the canonical activity log from the existing
// derive-on-read collectors once at startup, so the store reflects current
// state before any new transition lands. The upsert is idempotent on
// canonical_id, so re-running across restarts is safe. No-op when no store is
// wired.
func (r *Runtime) backfillActivity(ctx context.Context) {
	if r.deps == nil || r.deps.ActivityStore == nil {
		return
	}

	log := r.deps.resolveLog()
	projected, err := r.reprojectActivity(ctx, nil)
	if err != nil {
		log.WarnS(ctx, "Activity backfill stopped: list failed", err)

		return
	}

	log.InfoS(ctx, "Activity backfill complete",
		slog.Int("entries", projected),
	)
}

// entryToProjection maps an emitted WalletEntry to the store's projection DTO.
// kind/status/phase/failure_code are stored as the proto enum integers so the
// activity_kinds/activity_statuses foreign keys reject any value that is not a
// defined wire enum. The full entry and its request are kept as protojson so
// the schema stays stable as those shapes evolve. Empty hex handles map to nil
// (never a zero-length slice) to avoid the Postgres BYTEA x” pitfall.
func entryToProjection(entry *wavewalletrpc.WalletEntry) (db.ActivityProjection,
	error) {

	progress := entry.GetProgress()

	entryJSON, err := protojson.Marshal(entry)
	if err != nil {
		return db.ActivityProjection{}, fmt.Errorf("marshal entry: %w",
			err)
	}

	var requestJSON string
	if req := entry.GetRequest(); req != nil {
		raw, err := protojson.Marshal(req)
		if err != nil {
			return db.ActivityProjection{}, fmt.Errorf("marshal "+
				"request: %w", err)
		}
		requestJSON = string(raw)
	}

	var confHeight *int64
	if h := progress.GetConfirmationHeight(); h > 0 {
		v := int64(h)
		confHeight = &v
	}

	// updated_at falls back to created_at so an entry that omits the update
	// timestamp still sorts deterministically rather than at the epoch.
	createdAt := entry.GetCreatedAtUnix()
	updatedAt := entry.GetUpdatedAtUnix()
	if updatedAt == 0 {
		updatedAt = createdAt
	}

	return db.ActivityProjection{
		CanonicalID:   entry.GetId(),
		Kind:          int64(entry.GetKind()),
		Status:        int64(entry.GetStatus()),
		AmountSat:     entry.GetAmountSat(),
		FeeSat:        entry.GetFeeSat(),
		Counterparty:  entry.GetCounterparty(),
		Note:          entry.GetNote(),
		Phase:         int64(progress.GetPhase()),
		PhaseLabel:    progress.GetPhaseLabel(),
		FailureCode:   int64(entry.GetFailureCode()),
		FailureReason: entry.GetFailureReason(),
		PendingStatus: int64(
			wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		),
		PaymentHash:        hexBytesOrNil(progress.GetPaymentHash()),
		Txid:               hexBytesOrNil(progress.GetTxid()),
		ConfirmationHeight: confHeight,
		VtxoOutpoint:       progress.GetVtxoOutpoint(),
		RequestJSON:        requestJSON,
		EntryJSON:          string(entryJSON),
		CreatedAtUnix:      createdAt,
		UpdatedAtUnix:      updatedAt,
	}, nil
}

// hexBytesOrNil decodes a hex string to raw bytes, returning nil for an empty
// or malformed input so the column stays NULL rather than storing an empty or
// garbage blob.
func hexBytesOrNil(s string) []byte {
	if s == "" {
		return nil
	}

	b, err := hex.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil
	}

	return b
}

// rowToWalletEntry reconstructs a WalletEntry from a stored current-state row —
// the inverse of entryToProjection, used by the store-backed List read path.
// Every WalletEntry field has a backing column: BLOB handles are hex-encoded
// back, the confirmation height is widened, and the request oneof is decoded
// from its protojson form. The reconstruction is not byte-for-byte identical in
// one case: Progress is always materialized, so an entry projected with a nil
// Progress round-trips to a non-nil empty Progress (no current producer emits a
// nil-Progress row, so this is latent).
//
// request_json is decoded with DiscardUnknown so a row written by a newer
// daemon (carrying a WalletEntryRequest field this binary does not know) still
// decodes instead of failing the whole page. A genuinely malformed request
// still errors — a corrupt row is inconsistent state that should surface, not
// be silently skipped.
func rowToWalletEntry(row sqlc.ActivityEntry) (*wavewalletrpc.WalletEntry,
	error) {

	var request *wavewalletrpc.WalletEntryRequest
	if row.RequestJson != "" {
		request = &wavewalletrpc.WalletEntryRequest{}
		opts := protojson.UnmarshalOptions{DiscardUnknown: true}
		if err := opts.Unmarshal(
			[]byte(row.RequestJson), request,
		); err != nil {
			return nil, fmt.Errorf("unmarshal request: %w", err)
		}
	}

	var confHeight int32
	if row.ConfirmationHeight.Valid {
		confHeight = int32(row.ConfirmationHeight.Int64)
	}

	entry := &wavewalletrpc.WalletEntry{
		Id:            row.CanonicalID,
		Kind:          wavewalletrpc.EntryKind(row.Kind),
		Status:        wavewalletrpc.EntryStatus(row.Status),
		AmountSat:     row.AmountSat,
		FeeSat:        row.FeeSat,
		Counterparty:  row.Counterparty,
		Note:          row.Note,
		FailureReason: row.FailureReason,
		Request:       request,
		Progress: &wavewalletrpc.WalletEntryProgress{
			Phase: wavewalletrpc.WalletEntryPhase(
				row.Phase,
			),
			PhaseLabel:         row.PhaseLabel,
			PaymentHash:        hex.EncodeToString(row.PaymentHash),
			Txid:               hex.EncodeToString(row.Txid),
			ConfirmationHeight: confHeight,
			VtxoOutpoint:       row.VtxoOutpoint,
		},
		CreatedAtUnix: row.CreatedAtUnix,
		UpdatedAtUnix: row.UpdatedAtUnix,
	}

	// failure_code is presence-tracked on the wire: absent means "no
	// failure", so only set it for a non-zero stored code.
	if row.FailureCode != 0 {
		code := wavewalletrpc.EntryFailureCode(row.FailureCode)
		entry.FailureCode = &code
	}

	return entry, nil
}
