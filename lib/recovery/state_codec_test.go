package recovery

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestEncodeSessionStateNilRejected verifies the encoder refuses a nil input
// rather than panicking.
func TestEncodeSessionStateNilRejected(t *testing.T) {
	_, err := EncodeSessionState(nil)
	require.ErrorContains(t, err, "session state cannot be nil")
}

// TestSessionStateCodecRoundTrip exercises a deliberately chosen concrete
// state including both success and failure arms so a regression on any
// single field gets caught in isolation.
func TestSessionStateCodecRoundTrip(t *testing.T) {
	h1 := hashFromByte(1)
	h2 := hashFromByte(2)
	h3 := hashFromByte(3)

	cases := []struct {
		name  string
		state *SessionState
	}{
		{
			name: "happy_path",
			state: &SessionState{
				TxStates: map[chainhash.Hash]TxState{
					h1: TxStatePending,
					h2: TxStateBroadcasted,
					h3: TxStateConfirmed,
				},
				ConfirmHeights: map[chainhash.Hash]int32{
					h3: 123,
				},
			},
		},
		{
			name: "failure",
			state: &SessionState{
				TxStates: map[chainhash.Hash]TxState{
					h1: TxStateConfirmed,
					h2: TxStatePending,
				},
				ConfirmHeights: map[chainhash.Hash]int32{
					h1: 0,
				},
				FailedTxid: fn.Some(h2),
				LastError:  "package rejected",
			},
		},
		{
			name: "empty",
			state: &SessionState{
				TxStates:       map[chainhash.Hash]TxState{},
				ConfirmHeights: map[chainhash.Hash]int32{},
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			raw, err := EncodeSessionState(tc.state)
			require.NoError(t, err)

			decoded, err := DecodeSessionState(raw)
			require.NoError(t, err)
			require.Equal(t, tc.state, decoded)

			// Encoding the decoded state must round-trip
			// byte-for-byte. This is the invariant that makes the
			// codec suitable for hashing/signing by downstream
			// consumers.
			raw2, err := EncodeSessionState(decoded)
			require.NoError(t, err)
			require.True(
				t, bytes.Equal(raw, raw2),
				"encoding must be deterministic",
			)
		})
	}
}

// TestSessionStateCodecVersionMismatchRejected verifies that a blob written
// under an unknown version is rejected with a clear error.
func TestSessionStateCodecVersionMismatchRejected(t *testing.T) {
	raw, err := EncodeSessionState(&SessionState{
		TxStates:       map[chainhash.Hash]TxState{},
		ConfirmHeights: map[chainhash.Hash]int32{},
	})
	require.NoError(t, err)

	// Corrupt the version byte. The version record is the first TLV and
	// its payload byte is at offset 2 (type=1, length=1, value=version).
	require.GreaterOrEqual(t, len(raw), 3)
	raw[2] = 99

	_, err = DecodeSessionState(raw)
	require.ErrorContains(t, err, "unsupported session state codec")
}

// TestSessionStateCodecDuplicateKeyRejected verifies that a crafted blob with
// duplicate txids in a map decodes with an explicit error rather than
// silently folding entries together.
func TestSessionStateCodecDuplicateKeyRejected(t *testing.T) {
	h := hashFromByte(1)

	// Two entries for the same hash in tx_states. decodeTxStateMap must
	// reject so a tampered file cannot mask a confirmation.
	bad := make([]byte, 0, 4+2*(chainhash.HashSize+1))
	bad = append(bad, 0, 0, 0, 2)
	bad = append(bad, h[:]...)
	bad = append(bad, byte(TxStateConfirmed))
	bad = append(bad, h[:]...)
	bad = append(bad, byte(TxStatePending))

	_, err := decodeTxStateMap(bad)
	require.ErrorContains(t, err, "duplicate tx state key")

	h2 := hashFromByte(1)
	bad2 := make([]byte, 0, 4+2*(chainhash.HashSize+4))
	bad2 = append(bad2, 0, 0, 0, 2)
	bad2 = append(bad2, h2[:]...)
	bad2 = append(bad2, 0, 0, 0, 100)
	bad2 = append(bad2, h2[:]...)
	bad2 = append(bad2, 0, 0, 0, 200)

	_, err = decodeConfirmHeightMap(bad2)
	require.ErrorContains(t, err, "duplicate confirm height key")
}

// TestSessionStateCodecTruncatedRejected verifies that short blobs surface
// explicit decode errors instead of trusting the length prefix blindly.
func TestSessionStateCodecTruncatedRejected(t *testing.T) {
	_, err := decodeTxStateMap([]byte{0, 0})
	require.ErrorContains(t, err, "truncated tx state map")

	_, err = decodeConfirmHeightMap([]byte{0, 0})
	require.ErrorContains(t, err, "truncated confirm height map")

	// Count claims two entries but payload holds only one.
	shortHash := hashFromByte(1)
	short := make([]byte, 0, 4+chainhash.HashSize+1)
	short = append(short, 0, 0, 0, 2)
	short = append(short, shortHash[:]...)
	short = append(short, byte(TxStatePending))
	_, err = decodeTxStateMap(short)
	require.ErrorContains(t, err, "length mismatch")
}

// TestSessionStateRapidRoundTrip exercises the codec over randomly generated
// states. If any (TxStates, ConfirmHeights, FailedTxid, LastError)
// combination fails to round-trip exactly, rapid shrinks to the minimal
// counterexample automatically.
func TestSessionStateRapidRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		state := drawSessionState(t)

		raw, err := EncodeSessionState(state)
		if err != nil {
			t.Fatalf("encode failed: %v", err)
		}

		decoded, err := DecodeSessionState(raw)
		if err != nil {
			t.Fatalf("decode failed: %v", err)
		}

		if !sessionStatesEqual(state, decoded) {
			t.Fatalf("round-trip mismatch:\nwant %+v\ngot  %+v",
				state, decoded)
		}

		// Encoding the decoded state must match the original bytes:
		// the codec is required to be canonical so downstream
		// hash/signing paths produce stable outputs.
		raw2, err := EncodeSessionState(decoded)
		if err != nil {
			t.Fatalf("re-encode failed: %v", err)
		}
		if !bytes.Equal(raw, raw2) {
			t.Fatalf("encoding is not canonical")
		}
	})
}

// drawSessionState builds a random, internally-consistent SessionState.
func drawSessionState(t *rapid.T) *SessionState {
	numNodes := rapid.IntRange(0, 8).Draw(t, "numNodes")

	txStates := make(map[chainhash.Hash]TxState, numNodes)
	confirmHeights := make(map[chainhash.Hash]int32)

	for i := 0; i < numNodes; i++ {
		var h chainhash.Hash
		bytesSlice := rapid.SliceOfN(
			rapid.Byte(), chainhash.HashSize,
			chainhash.HashSize,
		).Draw(t, fmt.Sprintf("hash-%d", i))
		copy(h[:], bytesSlice)

		if _, exists := txStates[h]; exists {
			continue
		}

		stateVal := TxState(
			rapid.IntRange(
				int(TxStatePending), int(TxStateConfirmed),
			).Draw(t, fmt.Sprintf("state-%d", i)),
		)
		txStates[h] = stateVal

		if stateVal == TxStateConfirmed {
			confirmHeights[h] = rapid.Int32Range(
				0, 1_000_000,
			).Draw(t, fmt.Sprintf("height-%d", i))
		}
	}

	state := &SessionState{
		TxStates:       txStates,
		ConfirmHeights: confirmHeights,
	}

	hasFailure := rapid.Bool().Draw(t, "hasFailure")
	if hasFailure && numNodes > 0 {
		// Pick any tx at random to fail.
		for txid := range txStates {
			hash := txid
			state.FailedTxid = fn.Some(hash)
			break
		}
		state.LastError = rapid.StringN(1, 40, -1).Draw(
			t, "lastError",
		)
	}

	return state
}

// sessionStatesEqual deeply compares two SessionState values including the
// fn.Option fields that require WhenSome unwrapping.
func sessionStatesEqual(a, b *SessionState) bool {
	if len(a.TxStates) != len(b.TxStates) {
		return false
	}
	for k, v := range a.TxStates {
		if b.TxStates[k] != v {
			return false
		}
	}

	if len(a.ConfirmHeights) != len(b.ConfirmHeights) {
		return false
	}
	for k, v := range a.ConfirmHeights {
		if b.ConfirmHeights[k] != v {
			return false
		}
	}

	if a.FailedTxid.IsSome() != b.FailedTxid.IsSome() {
		return false
	}
	if a.FailedTxid.IsSome() {
		aTxid := a.FailedTxid.UnsafeFromSome()
		bTxid := b.FailedTxid.UnsafeFromSome()
		if aTxid != bTxid {
			return false
		}
	}

	return a.LastError == b.LastError
}

// hashFromByte constructs a chainhash.Hash whose first byte is the given
// marker; useful for building stable, human-readable test fixtures.
func hashFromByte(b byte) chainhash.Hash {
	var h chainhash.Hash
	h[0] = b

	return h
}
