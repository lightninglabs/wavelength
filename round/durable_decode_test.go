package round

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestDurableClientStateRejectsHugeRecord rejects an impossible blob length
// before the generic TLV decoder can allocate it during checkpoint recovery.
func TestDurableClientStateRejectsHugeRecord(t *testing.T) {
	raw := []byte{7, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	require.NotPanics(t, func() {
		_, _, err := decodeClientState(
			raw, &chaincfg.RegressionNetParams,
		)
		require.ErrorContains(t, err, "remaining bytes")
	})
}

// TestDurableFieldsRejectMissingRecord rejects whole-field truncation even
// when the remaining TLV framing is otherwise well formed.
func TestDurableFieldsRejectMissingRecord(t *testing.T) {
	var first, second uint64
	raw, err := encodeDurableFields(tlv.MakePrimitiveRecord(1, &first))
	require.NoError(t, err)
	err = decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &first),
		tlv.MakePrimitiveRecord(3, &second),
	)
	require.ErrorContains(t, err, "missing checkpoint field 3")
}

// TestDurableMapRejectsDuplicateKeys prevents decoding from silently replacing
// one operation's state with a second value carrying the same key.
func TestDurableMapRejectsDuplicateKeys(t *testing.T) {
	raw, err := encodeDurableMap(
		map[int]int64{
			1: 2,
		},
		encodeDurableIndex,
		encodeDurableAmount,
	)
	require.NoError(t, err)
	duplicate := append(bytes.Clone(raw), raw[1:]...)
	_, err = decodeDurableMap(
		duplicate, decodeDurableIndex, decodeDurableAmount,
	)
	require.ErrorContains(t, err, "duplicate durable map key")
}

// FuzzDurableClientState exercises the public-state recovery boundary with
// valid union members and arbitrary record framing. Successful decodes must
// produce a state that can itself be checkpointed and recovered again.
func FuzzDurableClientState(f *testing.F) {
	for _, state := range []ClientState{&Idle{},
		&PendingRoundAssembly{},
		&IntentSentState{},
		&QuoteReceivedState{},
		&RoundJoinedState{},
		&CommitmentTxReceivedState{},
		&CommitmentTxValidatedState{},
		&ForfeitSignaturesCollectingState{},
		&NoncesSentState{},
		&NoncesAggregatedState{},
		&PartialSigsSentState{},
		&InputSigSentState{},
		&ConfirmedState{},
		&ClientFailedState{},
		&RecoveryInitiatedState{}} {
		raw, err := encodeClientState(state)
		require.NoError(f, err)
		f.Add(raw)
	}
	treeState := &InputSigSentState{VTXOTreePaths: map[int]*tree.Tree{1: {
		Root: &tree.Node{
			Amount: 123,
			Children: map[uint32]*tree.Node{
				0: {
					Amount: 45,
				},
			},
		},
	}}}
	raw, err := encodeClientState(treeState)
	require.NoError(f, err)
	f.Add(raw)
	boardingState := &PendingRoundAssembly{
		Boarding: []BoardingIntent{
			durableBoardingFixture(f),
		},
	}
	raw, err = encodeClientState(boardingState)
	require.NoError(f, err)
	f.Add(raw)
	f.Add([]byte{7, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			return
		}
		state, lost, err := decodeClientState(
			raw, &chaincfg.RegressionNetParams,
		)
		if err != nil {
			return
		}
		again, err := encodeClientState(state)
		require.NoError(t, err)
		restored, lostAgain, err := decodeClientState(
			again, &chaincfg.RegressionNetParams,
		)
		require.NoError(t, err)
		require.IsType(t, state, restored)
		require.Equal(t, lost, lostAgain)
	})
}
