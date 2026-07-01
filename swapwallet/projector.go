//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/lightninglabs/darepo-client/db"
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
func (r *Runtime) project(ctx context.Context, entry *walletdkrpc.WalletEntry) {
	if r.deps == nil || r.deps.ActivityStore == nil {
		return
	}

	// A row with no canonical id cannot be keyed; skip it, matching the
	// id-guard the pending tracker already applies.
	if entry.GetId() == "" {
		return
	}

	projection, err := entryToProjection(entry)
	if err != nil {
		r.deps.resolveLog().WarnS(ctx, "Activity projection skipped: "+
			"encode failed", err)

		return
	}

	if err := r.deps.ActivityStore.ProjectEntry(
		ctx, projection,
	); err != nil {

		r.deps.resolveLog().WarnS(ctx, "Activity projection failed",
			err,
		)
	}
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

// backfillActivity seeds the canonical activity log from the existing
// derive-on-read collectors once at startup, so the store reflects current
// state before any new transition lands. It pages through the merged feed and
// projects every row; the upsert is idempotent on canonical_id, so re-running
// across restarts is safe. No-op when no store is wired.
func (r *Runtime) backfillActivity(ctx context.Context) {
	if r.deps == nil || r.deps.ActivityStore == nil {
		return
	}

	log := r.deps.resolveLog()
	h := newHistory(r.deps, r)
	limit := r.deps.resolveMaxListLimit()

	var (
		offset    uint32
		projected int
	)
	for {
		list, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
			Limit:  limit,
			Offset: offset,
		})
		if err != nil {
			log.WarnS(ctx, "Activity backfill stopped: list failed",
				err,
			)

			return
		}

		entries := list.GetEntries()

		// Guard against a non-advancing loop: an empty page (or a
		// zero-resolved limit) would otherwise never advance the offset
		// nor hit the length/total termination below.
		if len(entries) == 0 {
			break
		}

		for _, entry := range entries {
			r.project(ctx, entry)
		}
		projected += len(entries)

		offset += uint32(len(entries))
		if uint32(len(entries)) < limit || offset >= list.GetTotal() {
			break
		}
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
