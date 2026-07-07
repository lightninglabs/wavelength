//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"google.golang.org/protobuf/encoding/protojson"
)

// project writes one emitted WalletEntry into the canonical activity log: it
// upserts the current-state row and appends the transition event, atomically.
// Projection is best-effort and MUST NOT block or fail emit — a store error is
// logged at warn level (it is an operational hiccup, not an internal bug) and
// the caller still fans the update out to live subscribers. When no store is
// wired (tests, or a build without a database) projection is a no-op and the
// legacy derive-on-read path continues unchanged.
// project writes entry into the canonical activity store. It returns nil on a
// successful (or intentionally skipped) projection and the underlying error
// when the durable write failed, so a caller can gate a follow-up side effect
// on the row actually landing.
func (r *Runtime) project(ctx context.Context,
	entry *walletdkrpc.WalletEntry) error {

	if r.deps == nil || r.deps.ActivityStore == nil {
		return nil
	}

	// A row with no canonical id cannot be keyed; skip it, matching the
	// id-guard the pending tracker already applies. The synthetic
	// boarding-unconfirmed row is skipped for the same reason: it is
	// ephemeral live state recomputed from GetBalance with no durable
	// identity, so persisting it into a delete-free store would strand a
	// PENDING row that never clears once the deposit confirms under its
	// real txid:vout id.
	if entry.GetId() == "" ||
		entry.GetId() == syntheticBoardingUnconfirmedID {
		return nil
	}

	projection, err := entryToProjection(entry)
	if err != nil {
		r.deps.resolveLog().WarnS(ctx, "Activity projection skipped: "+
			"encode failed", err)

		return err
	}

	if err := r.deps.ActivityStore.ProjectEntry(
		ctx, projection,
	); err != nil {

		r.deps.resolveLog().WarnS(ctx, "Activity projection failed",
			err,
		)

		return err
	}

	return nil
}

// projectAndEmit projects the entry into the canonical activity log and then
// fans it out to live subscribers. Projection runs first so a row is durable
// before any subscriber observes it, but a projection failure never suppresses
// the emit.
func (r *Runtime) projectAndEmit(ctx context.Context,
	entry *walletdkrpc.WalletEntry) {

	r.project(ctx, entry)
	r.emit(entry)
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
// to seed every kind, while the reconciler passes only the backfill-only
// producers (DEPOSIT, EXIT) so it neither re-projects nor contends with the
// monitor-owned SEND/RECV rows.
//
// A wallet-local EXIT's pending record is cleared only here, and only after its
// terminal row is durably projected — never from the read/derive path — so a
// failed projection or a row that paged out is retried on a later pass instead
// of being stranded PENDING in the store.
func (r *Runtime) reprojectActivity(ctx context.Context,
	kinds []walletdkrpc.EntryKind) (int, error) {

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
		list, err := h.deriveActivity(ctx, &walletdkrpc.ListRequest{
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

		for _, entry := range entries {
			if err := r.project(ctx, entry); err != nil {
				// Best-effort: the next pass retries. Do not
				// clear any pending record when the write did
				// not land.
				continue
			}

			r.clearProjectedTerminalExit(entry)
		}
		projected += len(entries)

		offset += uint32(len(entries))
		if uint32(len(entries)) < limit || offset >= list.GetTotal() {
			break
		}
	}

	return projected, nil
}

// clearProjectedTerminalExit drops a wallet-local EXIT's in-memory pending
// record once its terminal row has been durably projected. A cooperative-leave
// EXIT exists only in the pending map, so this must run strictly after a
// successful project (not while decorating): the store now holds the terminal
// row, and dropping the record stops later passes from re-decorating a settled
// row. It is a no-op for ids not in the map (e.g. unilateral rows synthesized
// from ListVTXOs) and for non-terminal or non-EXIT rows.
func (r *Runtime) clearProjectedTerminalExit(entry *walletdkrpc.WalletEntry) {
	if entry.GetKind() != walletdkrpc.EntryKind_ENTRY_KIND_EXIT {
		return
	}

	switch entry.GetStatus() {
	case walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED:

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
func entryToProjection(entry *walletdkrpc.WalletEntry) (db.ActivityProjection,
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
		CanonicalID:        entry.GetId(),
		Kind:               int64(entry.GetKind()),
		Status:             int64(entry.GetStatus()),
		AmountSat:          entry.GetAmountSat(),
		FeeSat:             entry.GetFeeSat(),
		Counterparty:       entry.GetCounterparty(),
		Note:               entry.GetNote(),
		Phase:              int64(progress.GetPhase()),
		PhaseLabel:         progress.GetPhaseLabel(),
		FailureCode:        int64(entry.GetFailureCode()),
		FailureReason:      entry.GetFailureReason(),
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
func rowToWalletEntry(row sqlc.ActivityEntry) (*walletdkrpc.WalletEntry,
	error) {

	var request *walletdkrpc.WalletEntryRequest
	if row.RequestJson != "" {
		request = &walletdkrpc.WalletEntryRequest{}
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

	entry := &walletdkrpc.WalletEntry{
		Id:            row.CanonicalID,
		Kind:          walletdkrpc.EntryKind(row.Kind),
		Status:        walletdkrpc.EntryStatus(row.Status),
		AmountSat:     row.AmountSat,
		FeeSat:        row.FeeSat,
		Counterparty:  row.Counterparty,
		Note:          row.Note,
		FailureReason: row.FailureReason,
		Request:       request,
		Progress: &walletdkrpc.WalletEntryProgress{
			Phase: walletdkrpc.WalletEntryPhase(
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
		code := walletdkrpc.EntryFailureCode(row.FailureCode)
		entry.FailureCode = &code
	}

	return entry, nil
}
