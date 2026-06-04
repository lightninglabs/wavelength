package oor

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/stretchr/testify/require"
)

// fakeReservationStore is a test double for the durable spending-reservation
// index. It records the upserts it receives and can be configured to fail on a
// chosen call index to exercise the write-failure path.
type fakeReservationStore struct {
	outpoints []wire.OutPoint
	owners    []chainhash.Hash
	kinds     []int

	attempts int

	// failAt is the zero-based attempt index that returns failErr. A
	// negative value never fails.
	failAt  int
	failErr error
}

func (f *fakeReservationStore) UpsertReservation(_ context.Context,
	op wire.OutPoint, ownerKind int, ownerID chainhash.Hash) error {

	attempt := f.attempts
	f.attempts++

	if f.failAt >= 0 && attempt == f.failAt {
		return f.failErr
	}

	f.outpoints = append(f.outpoints, op)
	f.owners = append(f.owners, ownerID)
	f.kinds = append(f.kinds, ownerKind)

	return nil
}

func transferInput(idx byte) TransferInput {
	return TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					idx,
				},
				Index: uint32(idx),
			},
		},
	}
}

// TestRecordReservationsNilStoreNoop verifies that a nil reservation store
// disables the work entirely and is not an error.
func TestRecordReservationsNilStoreNoop(t *testing.T) {
	t.Parallel()

	b := &oorDurableBehavior{cfg: ClientActorCfg{ReservationStore: nil}}

	err := b.recordReservations(
		t.Context(), SessionID{0x01}, []TransferInput{transferInput(1)},
	)
	require.NoError(t, err)
}

// TestRecordReservationsWritesRowPerInput verifies that a row is recorded per
// input outpoint, owned by the session id under the OOR-outgoing owner kind.
func TestRecordReservationsWritesRowPerInput(t *testing.T) {
	t.Parallel()

	store := &fakeReservationStore{failAt: -1}
	b := &oorDurableBehavior{cfg: ClientActorCfg{ReservationStore: store}}

	sessionID := SessionID{0xab, 0xcd}
	inputs := []TransferInput{transferInput(1), transferInput(2)}

	err := b.recordReservations(t.Context(), sessionID, inputs)
	require.NoError(t, err)

	require.Equal(t, InputOutpoints(inputs), store.outpoints)

	wantOwner := chainhash.Hash(sessionID)
	for i := range inputs {
		require.Equal(t, wantOwner, store.owners[i])
		require.Equal(
			t, ReservationOwnerKindOOROutgoing, store.kinds[i],
		)
	}
}

// TestRecordReservationsPropagatesWriteError verifies that a reservation write
// failure surfaces as an error rather than being swallowed. This is the
// atomicity contract: recordReservations runs inside the same durable actor
// turn transaction as the checkpoint, so returning the error aborts the turn
// and rolls the checkpoint back. A swallowed error would leave a durably
// checkpointed session with no reservation rows, which the startup sweep would
// then misread as an orphan and release out from under the live spend.
func TestRecordReservationsPropagatesWriteError(t *testing.T) {
	t.Parallel()

	writeErr := errors.New("disk full")
	store := &fakeReservationStore{failAt: 1, failErr: writeErr}
	b := &oorDurableBehavior{cfg: ClientActorCfg{ReservationStore: store}}

	inputs := []TransferInput{transferInput(1), transferInput(2)}

	err := b.recordReservations(t.Context(), SessionID{0x01}, inputs)
	require.ErrorIs(t, err, writeErr)

	// The first input was written before the second failed; the failure
	// aborts the work without attempting further writes.
	require.Len(t, store.outpoints, 1)
}
