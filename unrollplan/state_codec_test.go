package unrollplan

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestEncodeStateNilRejected verifies the guard at the top of EncodeState.
func TestEncodeStateNilRejected(t *testing.T) {
	_, err := EncodeState(nil)
	require.ErrorContains(t, err, "state cannot be nil")
}

// TestStateCodecRoundTrip exercises hand-built states across the three sweep
// statuses plus empty / target-only shapes.
func TestStateCodecRoundTrip(t *testing.T) {
	h1 := hashFromByte(1)
	h2 := hashFromByte(2)
	h3 := hashFromByte(3)
	sweep := hashFromByte(9)

	cases := []struct {
		name  string
		state *State
	}{
		{
			name:  "empty",
			state: &State{},
		},
		{
			name: "confirmed_without_sweep",
			state: &State{
				ConfirmedTxids: []chainhash.Hash{
					h1,
					h2,
				},
				InFlightTxids: []chainhash.Hash{
					h3,
				},
				TargetConfirmHeight: fn.Some(int32(
					200,
				)),
			},
		},
		{
			name: "sweep_broadcasted",
			state: &State{
				ConfirmedTxids: []chainhash.Hash{
					h1,
					h2,
				},
				Sweep: SweepState{
					Status: SweepStatusBroadcasted,
					Txid:   fn.Some(sweep),
				},
			},
		},
		{
			name: "sweep_confirmed",
			state: &State{
				ConfirmedTxids: []chainhash.Hash{
					h1,
					h2,
				},
				Sweep: SweepState{
					Status:        SweepStatusConfirmed,
					Txid:          fn.Some(sweep),
					ConfirmHeight: fn.Some(int32(210)),
				},
				TargetConfirmHeight: fn.Some(int32(100)),
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			raw, err := EncodeState(tc.state)
			require.NoError(t, err)

			decoded, err := DecodeState(raw)
			require.NoError(t, err)
			requireStateEqual(t, tc.state, decoded)

			raw2, err := EncodeState(decoded)
			require.NoError(t, err)
			require.True(
				t, bytes.Equal(raw, raw2),
				"encoding must be canonical",
			)
		})
	}
}

// TestStateCodecVersionMismatch verifies unknown versions are rejected.
func TestStateCodecVersionMismatch(t *testing.T) {
	raw, err := EncodeState(&State{})
	require.NoError(t, err)

	// Version record is the first TLV; payload byte at offset 2.
	require.GreaterOrEqual(t, len(raw), 3)
	raw[2] = 99

	_, err = DecodeState(raw)
	require.ErrorContains(t, err, "unsupported state codec")
}

// TestStateCodecDuplicateHashRejected covers the encoder's duplicate-input
// guard on both the confirmed and in-flight slices.
func TestStateCodecDuplicateHashRejected(t *testing.T) {
	dup := hashFromByte(1)
	_, err := EncodeState(&State{
		ConfirmedTxids: []chainhash.Hash{dup, dup},
	})
	require.ErrorContains(t, err, "duplicate confirmed")

	_, err = EncodeState(&State{
		InFlightTxids: []chainhash.Hash{dup, dup},
	})
	require.ErrorContains(t, err, "duplicate in-flight")
}

// TestDecodeHashListRejectsShort exercises the truncation guard.
func TestDecodeHashListRejectsShort(t *testing.T) {
	_, err := decodeHashList([]byte{0, 0}, "test")
	require.ErrorContains(t, err, "truncated")

	only := hashFromByte(1)
	bad := []byte{0, 0, 0, 2}
	bad = append(bad, only[:]...)
	_, err = decodeHashList(bad, "test")
	require.ErrorContains(t, err, "length mismatch")
}

// TestStateCodecRapidRoundTrip asserts that every logically-consistent State
// round-trips byte-for-byte through Encode/Decode.
func TestStateCodecRapidRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		state := drawState(t)

		raw, err := EncodeState(state)
		if err != nil {
			t.Fatalf("encode failed: %v", err)
		}

		decoded, err := DecodeState(raw)
		if err != nil {
			t.Fatalf("decode failed: %v", err)
		}

		if !statesEqual(state, decoded) {
			t.Fatalf("round-trip mismatch:\nwant %#v\ngot  %#v",
				state, decoded)
		}

		raw2, err := EncodeState(decoded)
		if err != nil {
			t.Fatalf("re-encode failed: %v", err)
		}
		if !bytes.Equal(raw, raw2) {
			t.Fatalf("canonical encoding violated")
		}
	})
}

// drawState builds a random, internally consistent State.
func drawState(t *rapid.T) *State {
	state := &State{}

	numConfirmed := rapid.IntRange(0, 5).Draw(t, "numConfirmed")
	confirmed := drawDistinctHashes(
		t, numConfirmed, "confirmed",
	)
	state.ConfirmedTxids = confirmed

	numInflight := rapid.IntRange(0, 5).Draw(t, "numInflight")
	// In-flight hashes must be disjoint from confirmed ones to satisfy
	// Validate; we pre-seed the disallowed set below.
	used := make(map[chainhash.Hash]struct{}, len(confirmed))
	for _, h := range confirmed {
		used[h] = struct{}{}
	}
	inflight := drawDistinctHashesExcluding(
		t, numInflight, "inflight", used,
	)
	state.InFlightTxids = inflight

	if rapid.Bool().Draw(t, "hasTargetHeight") {
		state.TargetConfirmHeight = fn.Some(
			rapid.Int32Range(
				0, 1_000_000,
			).Draw(t, "targetHeight"),
		)
	}

	state.Sweep = drawSweepState(t)

	return state
}

func drawSweepState(t *rapid.T) SweepState {
	status := SweepStatus(
		rapid.IntRange(
			int(SweepStatusPending), int(SweepStatusConfirmed),
		).Draw(t, "sweepStatus"),
	)

	sweep := SweepState{Status: status}

	switch status {
	case SweepStatusPending:
		// Pending sweep: neither field is set.

	case SweepStatusBroadcasted:
		sweep.Txid = fn.Some(drawHash(t, "sweepTxid"))

	case SweepStatusConfirmed:
		sweep.Txid = fn.Some(drawHash(t, "sweepTxid"))
		sweep.ConfirmHeight = fn.Some(
			rapid.Int32Range(
				0, 1_000_000,
			).Draw(t, "sweepHeight"),
		)
	}

	return sweep
}

func drawDistinctHashes(t *rapid.T, n int, label string) []chainhash.Hash {
	return drawDistinctHashesExcluding(
		t, n, label, map[chainhash.Hash]struct{}{},
	)
}

func drawDistinctHashesExcluding(t *rapid.T, n int, label string,
	used map[chainhash.Hash]struct{}) []chainhash.Hash {

	out := make([]chainhash.Hash, 0, n)
	attempts := 0
	for len(out) < n && attempts < n*4 {
		attempts++
		h := drawHash(
			t, fmt.Sprintf("%s-%d", label, attempts),
		)
		if _, dup := used[h]; dup {
			continue
		}
		used[h] = struct{}{}
		out = append(out, h)
	}

	return out
}

func drawHash(t *rapid.T, label string) chainhash.Hash {
	bytesSlice := rapid.SliceOfN(
		rapid.Byte(), chainhash.HashSize, chainhash.HashSize,
	).Draw(t, label)
	var h chainhash.Hash
	copy(h[:], bytesSlice)

	return h
}

// statesEqual deeply compares two State values including fn.Option fields.
func statesEqual(a, b *State) bool {
	if !hashSlicesEqualAsSet(a.ConfirmedTxids, b.ConfirmedTxids) {
		return false
	}
	if !hashSlicesEqualAsSet(a.InFlightTxids, b.InFlightTxids) {
		return false
	}
	if !optsEqual(a.TargetConfirmHeight, b.TargetConfirmHeight) {
		return false
	}

	return sweepStatesEqual(a.Sweep, b.Sweep)
}

func sweepStatesEqual(a, b SweepState) bool {
	if a.Status != b.Status {
		return false
	}
	if !optsEqual(a.Txid, b.Txid) {
		return false
	}

	return optsEqual(a.ConfirmHeight, b.ConfirmHeight)
}

func optsEqual[T comparable](a, b fn.Option[T]) bool {
	if a.IsSome() != b.IsSome() {
		return false
	}
	if a.IsNone() {
		return true
	}

	return a.UnsafeFromSome() == b.UnsafeFromSome()
}

func hashSlicesEqualAsSet(a, b []chainhash.Hash) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[chainhash.Hash]int, len(a))
	for _, h := range a {
		seen[h]++
	}
	for _, h := range b {
		seen[h]--
	}
	for _, v := range seen {
		if v != 0 {
			return false
		}
	}

	return true
}

// requireStateEqual compares two States and fails with a clear diagnostic.
func requireStateEqual(t *testing.T, want, got *State) {
	t.Helper()
	require.ElementsMatch(t, want.ConfirmedTxids, got.ConfirmedTxids)
	require.ElementsMatch(t, want.InFlightTxids, got.InFlightTxids)
	require.Equal(
		t, want.TargetConfirmHeight.IsSome(),
		got.TargetConfirmHeight.IsSome(),
	)
	if want.TargetConfirmHeight.IsSome() {
		require.Equal(
			t, want.TargetConfirmHeight.UnsafeFromSome(),
			got.TargetConfirmHeight.UnsafeFromSome(),
		)
	}
	require.Equal(t, want.Sweep.Status, got.Sweep.Status)
	require.Equal(t, want.Sweep.Txid.IsSome(), got.Sweep.Txid.IsSome())
	if want.Sweep.Txid.IsSome() {
		require.Equal(
			t, want.Sweep.Txid.UnsafeFromSome(),
			got.Sweep.Txid.UnsafeFromSome(),
		)
	}
	require.Equal(
		t, want.Sweep.ConfirmHeight.IsSome(),
		got.Sweep.ConfirmHeight.IsSome(),
	)
	if want.Sweep.ConfirmHeight.IsSome() {
		require.Equal(
			t, want.Sweep.ConfirmHeight.UnsafeFromSome(),
			got.Sweep.ConfirmHeight.UnsafeFromSome(),
		)
	}
}

func hashFromByte(b byte) chainhash.Hash {
	var h chainhash.Hash
	h[0] = b

	return h
}
