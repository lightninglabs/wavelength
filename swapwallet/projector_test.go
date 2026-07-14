//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
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
func sampleWalletEntry() *walletdkrpc.WalletEntry {
	return &walletdkrpc.WalletEntry{
		Id:            "payment-hash",
		Kind:          walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     -1000,
		FeeSat:        10,
		Counterparty:  "ln:invoice",
		Note:          "coffee",
		CreatedAtUnix: 100,
		UpdatedAtUnix: 150,
		Progress: &walletdkrpc.WalletEntryProgress{
			Phase:              walletdkrpc.WalletEntryPhase(3),
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
	require.EqualValues(t, walletdkrpc.EntryKind_ENTRY_KIND_SEND, p.Kind)
	require.EqualValues(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING, p.Status,
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
	var decoded walletdkrpc.WalletEntry
	require.NoError(t, protojson.Unmarshal([]byte(p.EntryJSON), &decoded))
	require.True(t, proto.Equal(entry, &decoded))
}

// TestEntryToProjectionEmptyHandles verifies empty and malformed hex handles
// map to nil so the columns stay NULL, and updated_at falls back to created_at.
func TestEntryToProjectionEmptyHandles(t *testing.T) {
	t.Parallel()

	entry := &walletdkrpc.WalletEntry{
		Id:            "id",
		Kind:          walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		CreatedAtUnix: 100,
		Progress: &walletdkrpc.WalletEntryProgress{
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
func recvEntry(t *testing.T, sub *subscriber) *walletdkrpc.WalletEntry {
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
	entry := &walletdkrpc.WalletEntry{
		Id:     "deposit-bcrt1qstable",
		Kind:   walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		Status: walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		Progress: &walletdkrpc.WalletEntryProgress{
			Phase: walletdkrpc.
				WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_CONFIRMATION,
		},
	}

	runtime.projectAndEmit(t.Context(), entry)
	require.Empty(t, drainEntries(ch))
	require.Equal(t, 0, store.count())

	entry.Progress.Phase = walletdkrpc.
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
