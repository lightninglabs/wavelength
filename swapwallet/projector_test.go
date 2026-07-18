//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// fakeActivityProjector records the projections it is handed and can be made
// to fail, to exercise the projector's best-effort contract.
type fakeActivityProjector struct {
	mu        sync.Mutex
	projected []db.ActivityProjection
	err       error

	// onProject runs inside ProjectEntry before the row is recorded, so a
	// test can observe state at projection time (e.g. that emit has not
	// happened yet).
	onProject func()
}

func (f *fakeActivityProjector) ProjectEntry(_ context.Context,
	p db.ActivityProjection) (int64, error) {

	if f.onProject != nil {
		f.onProject()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return 0, f.err
	}

	f.projected = append(f.projected, p)

	// Return a positive, increasing seq so projectAndEmit treats each
	// successful projection as a real transition worth emitting.
	return int64(len(f.projected)), nil
}

// GetEntry reports no existing row. Tests that need durable merge behavior use
// a real ActivityPersistenceStore.
func (f *fakeActivityProjector) GetEntry(context.Context, string) (
	sqlc.ActivityEntry, error) {

	return sqlc.ActivityEntry{}, sql.ErrNoRows
}

func (f *fakeActivityProjector) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.projected)
}

// ListEntries satisfies waved.ActivityStore. This fake exercises only the
// write path; the store-backed read path is tested against a real DB store, so
// this returns no rows.
func (f *fakeActivityProjector) ListEntries(_ context.Context, _ int64,
	_ string, _ int32) ([]sqlc.ActivityEntry, error) {

	return nil, nil
}

// ListEntriesByKindStatus satisfies waved.ActivityStore. The rehydration
// read path is tested against a real DB store, so this returns no rows.
func (f *fakeActivityProjector) ListEntriesByKindStatus(_ context.Context, _,
	_ int64, _ string, _ int32) ([]sqlc.ActivityEntry, error) {

	return nil, nil
}

// CountByStatus satisfies waved.ActivityStore. The count path is tested
// against a real DB store, so this fake reports nothing.
func (f *fakeActivityProjector) CountByStatus(_ context.Context, _ int64) (
	int64, error) {

	return 0, nil
}

// PullEvents satisfies waved.ActivityStore. This fake exercises only the
// write path; the resumable-subscribe replay is tested against a real DB
// store, so this returns no events.
func (f *fakeActivityProjector) PullEvents(_ context.Context, _ int64,
	_ int32) ([]sqlc.ActivityEvent, error) {

	return nil, nil
}

// lastProjection returns the most recent projection the fake recorded. It
// panics when nothing has been projected, so a caller must guard with count.
func (f *fakeActivityProjector) lastProjection() db.ActivityProjection {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.projected[len(f.projected)-1]
}

// ids returns the set of canonical ids the fake has been asked to project.
func (f *fakeActivityProjector) ids() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make(map[string]bool, len(f.projected))
	for _, p := range f.projected {
		out[p.CanonicalID] = true
	}

	return out
}

// sampleWalletEntry builds a fully populated SEND WalletEntry fixture.
func sampleWalletEntry() *wavewalletrpc.WalletEntry {
	return &wavewalletrpc.WalletEntry{
		Id:            "payment-hash",
		Kind:          wavewalletrpc.EntryKind_ENTRY_KIND_SEND,
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     -1000,
		FeeSat:        10,
		Counterparty:  "ln:invoice",
		Note:          "coffee",
		CreatedAtUnix: 100,
		UpdatedAtUnix: 150,
		Progress: &wavewalletrpc.WalletEntryProgress{
			Phase:              wavewalletrpc.WalletEntryPhase(3),
			PhaseLabel:         "awaiting_preimage",
			PaymentHash:        "aabbcc",
			Txid:               "deadbeef",
			ConfirmationHeight: 42,
		},
	}
}

// TestEntryToProjection verifies the WalletEntry → projection mapping: enum
// integers, signed amount, hex-decoded handles, the confirmation-height
// pointer, and a lossless entry_json round-trip.
func TestEntryToProjection(t *testing.T) {
	t.Parallel()

	entry := sampleWalletEntry()

	p, err := entryToProjection(entry)
	require.NoError(t, err)

	require.Equal(t, "payment-hash", p.CanonicalID)
	require.EqualValues(t, wavewalletrpc.EntryKind_ENTRY_KIND_SEND, p.Kind)
	require.EqualValues(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING, p.Status,
	)
	require.Equal(t, int64(-1000), p.AmountSat)
	require.Equal(t, int64(10), p.FeeSat)
	require.EqualValues(t, 3, p.Phase)
	require.Equal(t, "awaiting_preimage", p.PhaseLabel)
	require.Equal(t, []byte{0xaa, 0xbb, 0xcc}, p.PaymentHash)
	require.Equal(t, []byte{0xde, 0xad, 0xbe, 0xef}, p.Txid)
	require.NotNil(t, p.ConfirmationHeight)
	require.Equal(t, int64(42), *p.ConfirmationHeight)
	require.Equal(t, int64(100), p.CreatedAtUnix)
	require.Equal(t, int64(150), p.UpdatedAtUnix)

	// entry_json must round-trip back to an equal WalletEntry.
	var decoded wavewalletrpc.WalletEntry
	require.NoError(t, protojson.Unmarshal([]byte(p.EntryJSON), &decoded))
	require.True(t, proto.Equal(entry, &decoded))
}

// TestEntryToProjectionEmptyHandles verifies empty and malformed hex handles
// map to nil so the columns stay NULL, and updated_at falls back to created_at.
func TestEntryToProjectionEmptyHandles(t *testing.T) {
	t.Parallel()

	entry := &wavewalletrpc.WalletEntry{
		Id:            "id",
		Kind:          wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		CreatedAtUnix: 100,
		Progress: &wavewalletrpc.WalletEntryProgress{
			PaymentHash: "",
			Txid:        "not-hex",
		},
	}

	p, err := entryToProjection(entry)
	require.NoError(t, err)
	require.Nil(t, p.PaymentHash)
	require.Nil(t, p.Txid)
	require.Nil(t, p.ConfirmationHeight)
	require.Equal(
		t, int64(100), p.UpdatedAtUnix, "updated falls back to created",
	)
}

// newProjectorRuntime builds a runtime wired with the given projector (which
// may be nil) and a subscriber channel.
func newProjectorRuntime(t *testing.T,
	store *fakeActivityProjector) (*Runtime, *subscriber) {

	t.Helper()

	deps := &Deps{}
	if store != nil {
		deps.ActivityStore = store
	}

	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	ch := runtime.subscribe()
	t.Cleanup(func() { runtime.unsubscribe(ch) })

	return runtime, ch
}

// recvEntry reads one entry from the subscriber within a short window.
func recvEntry(t *testing.T, sub *subscriber) *wavewalletrpc.WalletEntry {
	t.Helper()

	select {
	case u := <-sub.ch:
		return u.entry

	case <-time.After(time.Second):
		t.Fatal("expected an emitted entry")

		return nil
	}
}

// TestProjectAndEmitProjectsBeforeEmitting verifies the row is projected before
// it is fanned out: at projection time the subscriber channel is still empty.
func TestProjectAndEmitProjectsBeforeEmitting(t *testing.T) {
	t.Parallel()

	store := &fakeActivityProjector{}

	var chLenAtProject int
	runtime, ch := newProjectorRuntime(t, store)
	store.onProject = func() {
		chLenAtProject = len(ch.ch)
	}

	runtime.projectAndEmit(context.Background(), sampleWalletEntry())

	got := recvEntry(t, ch)
	require.Equal(t, "payment-hash", got.GetId())
	require.Equal(t, 1, store.count())
	require.Equal(t, 0, chLenAtProject, "projection ran before emit")
}

// TestProjectAndEmitNilStore verifies projection is a no-op without a store and
// the emit still reaches subscribers.
func TestProjectAndEmitNilStore(t *testing.T) {
	t.Parallel()

	runtime, ch := newProjectorRuntime(t, nil)

	require.NotPanics(t, func() {
		runtime.projectAndEmit(
			context.Background(), sampleWalletEntry(),
		)
	})
	require.Equal(t, "payment-hash", recvEntry(t, ch).GetId())
}

// TestProjectAndEmitSkipsEmptyID verifies an id-less entry is neither projected
// nor emitted: with a store wired it cannot be keyed, so it has no event_seq to
// carry as a cursor.
func TestProjectAndEmitSkipsEmptyID(t *testing.T) {
	t.Parallel()

	store := &fakeActivityProjector{}
	runtime, ch := newProjectorRuntime(t, store)

	entry := sampleWalletEntry()
	entry.Id = ""
	runtime.projectAndEmit(context.Background(), entry)

	require.Empty(t, drainEntries(ch), "id-less entry must not be emitted")
	require.Equal(t, 0, store.count())
}

// TestProjectAndEmitStoreErrorDoesNotEmit verifies a failed projection is not
// emitted: without a durable event there is no cursor to advance, so the update
// is left for the consumer to recover from the log or a List reconcile rather
// than delivered un-cursored.
func TestProjectAndEmitStoreErrorDoesNotEmit(t *testing.T) {
	t.Parallel()

	store := &fakeActivityProjector{err: context.DeadlineExceeded}
	runtime, ch := newProjectorRuntime(t, store)

	runtime.projectAndEmit(context.Background(), sampleWalletEntry())
	require.Empty(
		t, drainEntries(ch),
		"a failed projection must not be emitted",
	)
}

// TestProjectAndEmitPreservesImmutableContext verifies a sparse terminal
// projection emits and records the effective row enriched by the original
// memo, invoice request, and receive payment hash.
func TestProjectAndEmitPreservesImmutableContext(t *testing.T) {
	t.Parallel()

	history, store := newStoreListFixture(t)
	runtime := history.runtime
	sub := runtime.subscribe()
	t.Cleanup(func() { runtime.unsubscribe(sub) })

	pending := &wavewalletrpc.WalletEntry{
		Id:            "credit-receive",
		Kind:          wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     1,
		Note:          "one sat",
		CreatedAtUnix: 100,
		UpdatedAtUnix: 100,
		Request: &wavewalletrpc.WalletEntryRequest{
			Request: &wavewalletrpc.WalletEntryRequest_LightningInvoice{
				LightningInvoice: &wavewalletrpc.
					LightningInvoiceRequest{
					Invoice:     "lnbc1receive",
					PaymentHash: "aabbcc",
				},
			},
		},
		Progress: &wavewalletrpc.WalletEntryProgress{
			Phase: wavewalletrpc.
				WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_PAYMENT,
			PaymentHash: "aabbcc",
		},
	}
	runtime.projectAndEmit(t.Context(), pending)
	_ = recvEntry(t, sub)

	terminal := &wavewalletrpc.WalletEntry{
		Id:            pending.GetId(),
		Kind:          pending.GetKind(),
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		AmountSat:     pending.GetAmountSat(),
		UpdatedAtUnix: 200,
		Progress: &wavewalletrpc.WalletEntryProgress{
			Phase: wavewalletrpc.
				WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED,
		},
	}
	runtime.projectAndEmit(t.Context(), terminal)

	live := recvEntry(t, sub)
	require.Equal(t, "one sat", live.GetNote())
	require.Equal(
		t, "lnbc1receive",
		live.GetRequest().GetLightningInvoice().GetInvoice(),
	)
	require.Equal(t, "aabbcc", live.GetProgress().GetPaymentHash())

	events, err := store.PullEvents(t.Context(), 0, 10)
	require.NoError(t, err)
	require.Len(t, events, 2)

	var replayed wavewalletrpc.WalletEntry
	require.NoError(
		t,
		protojson.Unmarshal(
			[]byte(events[1].EntryJson), &replayed,
		),
	)
	require.Equal(t, "one sat", replayed.GetNote())
	require.Equal(
		t, "lnbc1receive",
		replayed.GetRequest().GetLightningInvoice().GetInvoice(),
	)
	require.Equal(t, "aabbcc", replayed.GetProgress().GetPaymentHash())
}

// TestProjectAndEmitTerminalBeatsStalePending models a credit child completing
// before the router writes its initial pending row. The stale pending
// projection must neither overwrite the terminal row nor emit a backward live
// event.
func TestProjectAndEmitTerminalBeatsStalePending(t *testing.T) {
	t.Parallel()

	history, store := newStoreListFixture(t)
	runtime := history.runtime
	sub := runtime.subscribe()
	t.Cleanup(func() { runtime.unsubscribe(sub) })

	terminal := &wavewalletrpc.WalletEntry{
		Id:            "fast-credit-pay",
		Kind:          wavewalletrpc.EntryKind_ENTRY_KIND_SEND,
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		AmountSat:     -1,
		CreatedAtUnix: 100,
		UpdatedAtUnix: 200,
		Progress: &wavewalletrpc.WalletEntryProgress{
			Phase: wavewalletrpc.
				WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED,
		},
	}
	runtime.projectAndEmit(t.Context(), terminal)
	require.Equal(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		recvEntry(t, sub).GetStatus(),
	)

	stalePending := proto.Clone(terminal).(*wavewalletrpc.WalletEntry)
	stalePending.Status = wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING
	stalePending.UpdatedAtUnix = 100
	stalePending.Progress.Phase = wavewalletrpc.
		WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING
	runtime.projectAndEmit(t.Context(), stalePending)
	require.Empty(t, drainEntries(sub))

	row, err := store.GetEntry(t.Context(), terminal.GetId())
	require.NoError(t, err)
	require.EqualValues(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE, row.Status,
	)

	events, err := store.PullEvents(t.Context(), 0, 10)
	require.NoError(t, err)
	require.Len(t, events, 1)
}

// TestProjectAndEmitEnrichesRequestWithoutMemo verifies an invoice request can
// enrich a sparse row even when memo is empty and no mutable lifecycle field
// changes.
func TestProjectAndEmitEnrichesRequestWithoutMemo(t *testing.T) {
	t.Parallel()

	history, store := newStoreListFixture(t)
	runtime := history.runtime
	sub := runtime.subscribe()
	t.Cleanup(func() { runtime.unsubscribe(sub) })

	sparse := &wavewalletrpc.WalletEntry{
		Id:            "request-only",
		Kind:          wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     1,
		CreatedAtUnix: 100,
		UpdatedAtUnix: 100,
	}
	runtime.projectAndEmit(t.Context(), sparse)
	_ = recvEntry(t, sub)

	rich := proto.Clone(sparse).(*wavewalletrpc.WalletEntry)
	rich.Request = &wavewalletrpc.WalletEntryRequest{
		Request: &wavewalletrpc.WalletEntryRequest_LightningInvoice{
			LightningInvoice: &wavewalletrpc.LightningInvoiceRequest{
				Invoice:     "lnbc1requestonly",
				PaymentHash: "aabbcc",
			},
		},
	}
	runtime.projectAndEmit(t.Context(), rich)

	live := recvEntry(t, sub)
	require.Empty(t, live.GetNote())
	require.Equal(
		t, "lnbc1requestonly",
		live.GetRequest().GetLightningInvoice().GetInvoice(),
	)

	row, err := store.GetEntry(t.Context(), rich.GetId())
	require.NoError(t, err)
	require.NotEmpty(t, row.RequestJson)

	events, err := store.PullEvents(t.Context(), 0, 10)
	require.NoError(t, err)
	require.Len(t, events, 2)
}

// TestProjectAndEmitSkipsEphemeralBoardingRow verifies the synthetic
// boarding-unconfirmed row is neither persisted nor emitted: it is ephemeral
// live state with no durable id, so it carries no event_seq to stream. The
// store-backed List path overlays it directly instead of persisting it.
func TestProjectAndEmitSkipsEphemeralBoardingRow(t *testing.T) {
	t.Parallel()

	store := &fakeActivityProjector{}
	runtime, ch := newProjectorRuntime(t, store)

	entry := sampleWalletEntry()
	entry.Id = syntheticBoardingUnconfirmedID
	runtime.projectAndEmit(context.Background(), entry)

	require.Empty(
		t, drainEntries(ch),
		"ephemeral boarding row must not be emitted",
	)
	require.Equal(t, 0, store.count(), "ephemeral row must not be stored")
}

// TestProjectAndEmitSkipsAddressScopedBoardingOverlay verifies a stable id does
// not make the live zero-conf row durable. The confirmed ledger projection is
// still allowed through once it carries chain identity.
func TestProjectAndEmitSkipsAddressScopedBoardingOverlay(t *testing.T) {
	t.Parallel()

	store := &fakeActivityProjector{}
	runtime, ch := newProjectorRuntime(t, store)
	entry := &wavewalletrpc.WalletEntry{
		Id:     "deposit-bcrt1qstable",
		Kind:   wavewalletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		Status: wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		Progress: &wavewalletrpc.WalletEntryProgress{
			Phase: wavewalletrpc.
				WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_CONFIRMATION,
		},
	}

	runtime.projectAndEmit(t.Context(), entry)
	require.Empty(t, drainEntries(ch))
	require.Equal(t, 0, store.count())

	entry.Progress.Phase = wavewalletrpc.
		WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING
	entry.Progress.Txid = "confirmed-txid"
	entry.Progress.ConfirmationHeight = 123
	runtime.projectAndEmit(t.Context(), entry)
	require.Len(t, drainEntries(ch), 1)
	require.Equal(t, 1, store.count())
}

// TestRowToWalletEntryDiscardsUnknownRequestFields verifies a stored request
// carrying a field this binary does not know (schema drift from a newer
// daemon) still decodes, while genuinely malformed JSON still fails loudly —
// a corrupt row is inconsistent state that must surface, not be skipped.
func TestRowToWalletEntryDiscardsUnknownRequestFields(t *testing.T) {
	t.Parallel()

	forward := sqlc.ActivityEntry{
		CanonicalID: "a",
		RequestJson: `{"lightningInvoice":{"invoice":"lnbc1"},` +
			`"futureField":42}`,
	}
	got, err := rowToWalletEntry(forward)
	require.NoError(t, err)
	require.Equal(
		t, "lnbc1", got.GetRequest().GetLightningInvoice().GetInvoice(),
	)

	corrupt := sqlc.ActivityEntry{
		CanonicalID: "b",
		RequestJson: `{not valid json`,
	}
	_, err = rowToWalletEntry(corrupt)
	require.Error(t, err, "a corrupt request row must fail loudly")
}

// TestClearProjectedTerminalExitRetainsFeeZeroRows proves a completed EXIT
// projected with a zero fee keeps its pending record for the bounded grace
// window, so a fee ledger row that commits just after the forfeit status
// becomes terminal is still picked up by a later derive pass, while a
// fee-carrying completion and a failure clear the record immediately.
func TestClearProjectedTerminalExitRetainsFeeZeroRows(t *testing.T) {
	t.Parallel()

	r := newRuntime(t.Context(), &Deps{})

	track := func(id string) {
		r.trackPendingEntryWithoutTimeout(&wavewalletrpc.WalletEntry{
			Id:   id,
			Kind: wavewalletrpc.EntryKind_ENTRY_KIND_EXIT,
			Status: wavewalletrpc.
				EntryStatus_ENTRY_STATUS_PENDING,
		})
	}
	tracked := func(id string) bool {
		r.pendingMu.Lock()
		defer r.pendingMu.Unlock()
		_, ok := r.pending[id]

		return ok
	}

	// A zero-fee completion survives the grace window and is cleared only
	// on the pass that exhausts it.
	track("exit-a")
	feeZero := &wavewalletrpc.WalletEntry{
		Id:     "exit-a",
		Kind:   wavewalletrpc.EntryKind_ENTRY_KIND_EXIT,
		Status: wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
	}
	for i := 0; i < feeZeroClearGracePasses-1; i++ {
		r.clearProjectedTerminalExit(feeZero)
		require.True(
			t, tracked("exit-a"),
			"record must survive pass %d of the grace window", i+1,
		)
	}
	r.clearProjectedTerminalExit(feeZero)
	require.False(
		t, tracked("exit-a"),
		"record must clear once the grace window is exhausted",
	)

	// A completion that carries a fee clears immediately.
	track("exit-b")
	r.clearProjectedTerminalExit(&wavewalletrpc.WalletEntry{
		Id:     "exit-b",
		Kind:   wavewalletrpc.EntryKind_ENTRY_KIND_EXIT,
		Status: wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		FeeSat: 541,
	})
	require.False(t, tracked("exit-b"))

	// A failure clears immediately regardless of fee.
	track("exit-c")
	r.clearProjectedTerminalExit(&wavewalletrpc.WalletEntry{
		Id:     "exit-c",
		Kind:   wavewalletrpc.EntryKind_ENTRY_KIND_EXIT,
		Status: wavewalletrpc.EntryStatus_ENTRY_STATUS_FAILED,
	})
	require.False(t, tracked("exit-c"))

	// A fee observed mid-grace clears via the normal fee-carrying path.
	track("exit-d")
	r.clearProjectedTerminalExit(&wavewalletrpc.WalletEntry{
		Id:     "exit-d",
		Kind:   wavewalletrpc.EntryKind_ENTRY_KIND_EXIT,
		Status: wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
	})
	require.True(t, tracked("exit-d"))
	r.clearProjectedTerminalExit(&wavewalletrpc.WalletEntry{
		Id:     "exit-d",
		Kind:   wavewalletrpc.EntryKind_ENTRY_KIND_EXIT,
		Status: wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		FeeSat: 388,
	})
	require.False(t, tracked("exit-d"))
}

// TestMergeActivityContextKeepsSettledFee proves a stored fee is sticky: a
// later projection carrying fee 0 (a completion pass that raced the fee
// ledger commit or hit a transient read error) must not regress the stored
// fee, and the amount is restored together with it since the fee-carrying
// projection also netted the fee out of the amount.
func TestMergeActivityContextKeepsSettledFee(t *testing.T) {
	t.Parallel()

	existing := &wavewalletrpc.WalletEntry{
		Id:        "exit-a",
		AmountSat: -138_816,
		FeeSat:    388,
	}
	next := &wavewalletrpc.WalletEntry{
		Id:        "exit-a",
		AmountSat: -139_204,
	}

	merged := mergeActivityContext(existing, next)
	require.Equal(t, int64(388), merged.GetFeeSat())
	require.Equal(t, int64(-138_816), merged.GetAmountSat())

	// A projection that carries its own fee stays authoritative.
	fresh := &wavewalletrpc.WalletEntry{
		Id:        "exit-a",
		AmountSat: -138_816,
		FeeSat:    400,
	}
	merged = mergeActivityContext(existing, fresh)
	require.Equal(t, int64(400), merged.GetFeeSat())
	require.Equal(t, int64(-138_816), merged.GetAmountSat())
}
