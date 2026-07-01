//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
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
	p db.ActivityProjection) error {

	if f.onProject != nil {
		f.onProject()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.err
	}

	f.projected = append(f.projected, p)

	return nil
}

func (f *fakeActivityProjector) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.projected)
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
	store *fakeActivityProjector) (*Runtime,
	chan *walletdkrpc.WalletEntry) {

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

// recvEntry reads one entry from the subscriber channel within a short window.
func recvEntry(t *testing.T,
	ch chan *walletdkrpc.WalletEntry) *walletdkrpc.WalletEntry {

	t.Helper()

	select {
	case e := <-ch:
		return e

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
		chLenAtProject = len(ch)
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

// TestProjectAndEmitSkipsEmptyID verifies an id-less entry is not projected but
// is still emitted.
func TestProjectAndEmitSkipsEmptyID(t *testing.T) {
	t.Parallel()

	store := &fakeActivityProjector{}
	runtime, ch := newProjectorRuntime(t, store)

	entry := sampleWalletEntry()
	entry.Id = ""
	runtime.projectAndEmit(context.Background(), entry)

	require.NotNil(t, recvEntry(t, ch))
	require.Equal(t, 0, store.count())
}

// TestProjectAndEmitStoreErrorStillEmits verifies a store failure never
// suppresses the emit.
func TestProjectAndEmitStoreErrorStillEmits(t *testing.T) {
	t.Parallel()

	store := &fakeActivityProjector{err: context.DeadlineExceeded}
	runtime, ch := newProjectorRuntime(t, store)

	runtime.projectAndEmit(context.Background(), sampleWalletEntry())
	require.Equal(t, "payment-hash", recvEntry(t, ch).GetId())
}
