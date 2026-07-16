package unroll

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestEncodeCheckpointNilRejected verifies the guard at the top of
// encodeCheckpoint so a misuse surfaces with a clear error rather than a
// segfault.
func TestEncodeCheckpointNilRejected(t *testing.T) {
	_, err := encodeCheckpoint(nil)
	require.ErrorContains(t, err, "checkpoint cannot be nil")
}

// TestCheckpointCodecRoundTripHandcrafted exercises the codec across the
// canonical shapes: fresh (idle), started without sweep, started with sweep
// tx, started with failure recorded.
func TestCheckpointCodecRoundTripHandcrafted(t *testing.T) {
	targetTxid := hashFromByteCk(0xAA)
	sweepTxid := hashFromByteCk(0xBB)
	sweepTx := buildDummySweepTx(t, sweepTxid)

	cases := []struct {
		name       string
		checkpoint *actorCheckpoint
	}{
		{
			name: "fresh_idle",
			checkpoint: &actorCheckpoint{
				Version: checkpointVersion,
			},
		},
		{
			name: "started_materializing",
			checkpoint: &actorCheckpoint{
				Version: checkpointVersion,
				Height:  150,
				Started: true,
				Trigger: TriggerManual,
				State: unrollplan.State{
					ConfirmedTxids: []chainhash.Hash{
						targetTxid,
					},
					TargetConfirmHeight: fn.Some[int32](
						150,
					),
				},
				SweepAttempts: 0,
			},
		},
		{
			name: "sweep_broadcast",
			checkpoint: &actorCheckpoint{
				Version: checkpointVersion,
				Height:  200,
				Started: true,
				Trigger: TriggerRestart,
				State: unrollplan.State{
					ConfirmedTxids: []chainhash.Hash{
						targetTxid,
					},
					TargetConfirmHeight: fn.Some[int32](
						180,
					),
					Sweep: unrollplan.SweepState{
						Status: unrollplan.
							SweepStatusBroadcasted,
						Txid: fn.Some(sweepTxid),
					},
				},
				SweepTx:       sweepTx,
				SweepAttempts: 1,
			},
		},
		{
			name: "sweep_finalized",
			checkpoint: &actorCheckpoint{
				Version: checkpointVersion,
				Height:  210,
				Started: true,
				Trigger: TriggerManual,
				State: unrollplan.State{
					ConfirmedTxids: []chainhash.Hash{
						targetTxid,
					},
					Sweep: unrollplan.SweepState{
						Status: unrollplan.
							SweepStatusBroadcasted,
						Txid: fn.Some(sweepTxid),
					},
				},
				SweepTx:        sweepTx,
				SweepAttempts:  1,
				SweepFinalized: true,
			},
		},
		{
			name: "failed",
			checkpoint: &actorCheckpoint{
				Version: checkpointVersion,
				Height:  123,
				Started: true,
				Trigger: TriggerManual,
				State: unrollplan.State{
					ConfirmedTxids: []chainhash.Hash{
						targetTxid,
					},
				},
				Fail:          "broadcaster rejected tx",
				SweepAttempts: 3,
			},
		},
		{
			name: "deferred_checkpoint",
			checkpoint: &actorCheckpoint{
				Version: checkpointVersion,
				Height:  150,
				Started: true,
				Trigger: TriggerFraudSpend,
				State:   unrollplan.State{},
				DeferredCheckpoints: []DeferredCheckpoint{{
					Txid:           targetTxid,
					DeadlineHeight: 270,
				}},
			},
		},
		{
			name: "provisional_external_spend",
			checkpoint: &actorCheckpoint{
				Version: checkpointVersion,
				Height:  155,
				Started: true,
				Trigger: TriggerManual,
				State: unrollplan.State{
					ConfirmedTxids: []chainhash.Hash{
						targetTxid,
					},
					TargetConfirmHeight: fn.Some[int32](
						154,
					),
				},
				ProvisionalExternalSpend: fn.Some(
					ExternalSpendAnchor{
						SpendingTxid: hashFromByteCk(
							0xee,
						),
						SpendingHeight: 155,
					},
				),
			},
		},
		{
			name: "finalized_external_spend",
			checkpoint: &actorCheckpoint{
				Version:                checkpointVersion,
				Height:                 185,
				Started:                true,
				Trigger:                TriggerManual,
				ExternalSpendFinalized: true,
			},
		},
		{
			name: "restart_relive_unsafe",
			checkpoint: &actorCheckpoint{
				Version:      checkpointVersion,
				Height:       190,
				Started:      true,
				Trigger:      TriggerManual,
				ReliveUnsafe: true,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			raw, err := encodeCheckpoint(tc.checkpoint)
			require.NoError(t, err)

			decoded, err := decodeCheckpoint(raw)
			require.NoError(t, err)
			requireCheckpointEqual(t, tc.checkpoint, decoded)

			// Canonical encoding: the second encode of the decoded
			// value must match the first encode byte-for-byte.
			raw2, err := encodeCheckpoint(decoded)
			require.NoError(t, err)
			require.True(
				t, bytes.Equal(raw, raw2),
				"encoding must be canonical",
			)
		})
	}
}

// TestCheckpointCodecVersionMismatch verifies that a checkpoint encoded with
// an unsupported version byte is rejected by the decoder. This is the safety
// net that prevents us from silently loading data written by an older or
// future codec.
func TestCheckpointCodecVersionMismatch(t *testing.T) {
	raw, err := encodeCheckpoint(&actorCheckpoint{
		Version: checkpointVersion,
	})
	require.NoError(t, err)

	// The version record is the first TLV; its payload byte sits at
	// offset 2 (type=1, length=1, value=1). Flip it to an unsupported
	// version and confirm the decoder rejects the blob.
	require.GreaterOrEqual(t, len(raw), 3)
	raw[2] = 99

	_, err = decodeCheckpoint(raw)
	require.ErrorContains(t, err, "unsupported checkpoint version")
}

// TestCheckpointCodecCorruptDataRejected asserts that malformed input (empty,
// truncated, random garbage) is rejected with an error rather than panicking
// or returning a zero-value checkpoint.
func TestCheckpointCodecCorruptDataRejected(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{
			name: "empty",
			raw:  nil,
		},
		{
			name: "single_byte",
			raw: []byte{
				0x01,
			},
		},
		{
			name: "garbage",
			raw: []byte{
				0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeCheckpoint(tc.raw)
			require.Error(t, err)
		})
	}
}

// TestCheckpointCodecRapidRoundTrip is the property-based guarantee that any
// internally-consistent actorCheckpoint survives Encode → Decode → Encode
// with byte-for-byte canonical output.
func TestCheckpointCodecRapidRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cp := drawCheckpoint(t)

		raw, err := encodeCheckpoint(cp)
		if err != nil {
			t.Fatalf("encode failed: %v", err)
		}

		decoded, err := decodeCheckpoint(raw)
		if err != nil {
			t.Fatalf("decode failed: %v", err)
		}

		if !checkpointsEqual(cp, decoded) {
			t.Fatalf("round-trip mismatch:\nwant %#v\ngot  %#v", cp,
				decoded)
		}

		raw2, err := encodeCheckpoint(decoded)
		if err != nil {
			t.Fatalf("re-encode failed: %v", err)
		}
		if !bytes.Equal(raw, raw2) {
			t.Fatalf("canonical encoding violated")
		}
	})
}

// drawCheckpoint produces a random, internally consistent actorCheckpoint
// with enough variability to exercise every optional field combination.
func drawCheckpoint(t *rapid.T) *actorCheckpoint {
	cp := &actorCheckpoint{
		Version: checkpointVersion,
		Height: rapid.Int32Range(0, 10_000_000).
			Draw(t, "height"),
		Started: rapid.Bool().Draw(t, "started"),
		Trigger: StartTrigger(
			rapid.Int32Range(
				int32(TriggerManual), int32(TriggerRestart),
			).Draw(t, "trigger"),
		),
		SweepAttempts: rapid.IntRange(0, 16).
			Draw(t, "sweepAttempts"),
		State: drawPlannerState(t),
	}

	if rapid.Bool().Draw(t, "hasSweepTx") {
		cp.SweepTx = buildRandomTx(t)
	}

	if rapid.Bool().Draw(t, "hasFail") {
		cp.Fail = rapid.StringN(1, 128, -1).Draw(t, "failReason")
	}

	if rapid.Bool().Draw(t, "hasDeferredCheckpoints") {
		numDeferred := rapid.IntRange(1, 4).Draw(t, "numDeferred")
		for i, txid := range drawDistinctHashesCk(
			t, numDeferred, "deferred", nil,
		) {
			cp.DeferredCheckpoints = append(
				cp.DeferredCheckpoints, DeferredCheckpoint{
					Txid:           txid,
					DeadlineHeight: int32(100 + i),
				},
			)
		}
	}

	return cp
}

// drawPlannerState mirrors unrollplan's own rapid generator but operates
// against its exported types so the checkpoint test does not depend on
// private helpers.
func drawPlannerState(t *rapid.T) unrollplan.State {
	state := unrollplan.State{}

	numConfirmed := rapid.IntRange(0, 4).Draw(t, "numConfirmed")
	confirmed := drawDistinctHashesCk(t, numConfirmed, "confirmed", nil)
	state.ConfirmedTxids = confirmed

	used := make(map[chainhash.Hash]struct{}, len(confirmed))
	for _, h := range confirmed {
		used[h] = struct{}{}
	}

	numInflight := rapid.IntRange(0, 4).Draw(t, "numInflight")
	state.InFlightTxids = drawDistinctHashesCk(
		t, numInflight, "inflight", used,
	)

	if rapid.Bool().Draw(t, "hasTargetHeight") {
		state.TargetConfirmHeight = fn.Some(
			rapid.Int32Range(
				0, 1_000_000,
			).Draw(t, "targetHeight"),
		)
	}

	state.Sweep = drawSweepStateCk(t)

	return state
}

// drawSweepStateCk produces a SweepState whose optional fields are consistent
// with its Status value so the underlying planner codec accepts it.
func drawSweepStateCk(t *rapid.T) unrollplan.SweepState {
	status := unrollplan.SweepStatus(
		rapid.IntRange(
			int(unrollplan.SweepStatusPending),
			int(unrollplan.SweepStatusConfirmed),
		).Draw(t, "sweepStatus"),
	)

	sweep := unrollplan.SweepState{Status: status}

	switch status {
	case unrollplan.SweepStatusPending:
		// Pending sweep: neither optional field is set.

	case unrollplan.SweepStatusBroadcasted:
		sweep.Txid = fn.Some(drawHashCk(t, "sweepTxid"))

	case unrollplan.SweepStatusConfirmed:
		sweep.Txid = fn.Some(drawHashCk(t, "sweepTxid"))
		sweep.ConfirmHeight = fn.Some(
			rapid.Int32Range(
				0, 1_000_000,
			).Draw(t, "sweepHeight"),
		)
	}

	return sweep
}

// drawDistinctHashesCk draws n distinct random hashes, skipping any that
// collide with the caller-provided used set. The duplicate-input guards in
// unrollplan.EncodeState would otherwise reject the generated state.
func drawDistinctHashesCk(t *rapid.T, n int, label string,
	used map[chainhash.Hash]struct{}) []chainhash.Hash {

	if used == nil {
		used = make(map[chainhash.Hash]struct{})
	}

	out := make([]chainhash.Hash, 0, n)
	attempts := 0
	for len(out) < n && attempts < n*4 {
		attempts++
		h := drawHashCk(t, fmt.Sprintf("%s-%d", label, attempts))
		if _, dup := used[h]; dup {
			continue
		}
		used[h] = struct{}{}
		out = append(out, h)
	}

	return out
}

// drawHashCk draws one 32-byte hash uniformly.
func drawHashCk(t *rapid.T, label string) chainhash.Hash {
	raw := rapid.SliceOfN(
		rapid.Byte(), chainhash.HashSize, chainhash.HashSize,
	).Draw(t, label)

	var h chainhash.Hash
	copy(h[:], raw)

	return h
}

// buildRandomTx produces a deterministically-generated wire.MsgTx with random
// inputs and outputs. It must round-trip unchanged through
// wire.MsgTx.Serialize / Deserialize.
func buildRandomTx(t *rapid.T) *wire.MsgTx {
	tx := wire.NewMsgTx(2)

	numIn := rapid.IntRange(1, 3).Draw(t, "numIn")
	for i := 0; i < numIn; i++ {
		hash := drawHashCk(t, fmt.Sprintf("inHash-%d", i))
		idx := rapid.Uint32Range(0, 10).
			Draw(t, fmt.Sprintf("inIdx-%d", i))

		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash:  hash,
				Index: idx,
			},
			Sequence: wire.MaxTxInSequenceNum,
		})
	}

	numOut := rapid.IntRange(1, 3).Draw(t, "numOut")
	for i := 0; i < numOut; i++ {
		amount := rapid.Int64Range(1, 1_000_000).
			Draw(t, fmt.Sprintf("outAmt-%d", i))
		pkSize := rapid.IntRange(1, 32).
			Draw(t, fmt.Sprintf("pkSize-%d", i))
		pkScript := rapid.SliceOfN(rapid.Byte(), pkSize, pkSize).
			Draw(t, fmt.Sprintf("pkScript-%d", i))

		tx.AddTxOut(&wire.TxOut{
			Value:    amount,
			PkScript: pkScript,
		})
	}

	return tx
}

// buildDummySweepTx produces a small but well-formed sweep transaction
// spending the supplied parent txid. Used by the handcrafted round-trip
// cases so they don't need to allocate a rapid generator.
func buildDummySweepTx(t *testing.T, parent chainhash.Hash) *wire.MsgTx {
	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: parent, Index: 0},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    1_000,
		PkScript: []byte{0x00, 0x14, 0x01, 0x02, 0x03, 0x04},
	})

	return tx
}

// checkpointsEqual deep-compares two actorCheckpoints. Planner-state fields
// use set-equality for the txid slices because the encoder sorts them; a
// bitwise comparison would otherwise fail on equivalent but differently-
// ordered inputs.
func checkpointsEqual(a, b *actorCheckpoint) bool {
	if a.Version != b.Version {
		return false
	}
	if a.Height != b.Height {
		return false
	}
	if a.Started != b.Started {
		return false
	}
	if a.Trigger != b.Trigger {
		return false
	}
	if a.SweepAttempts != b.SweepAttempts {
		return false
	}
	if a.Fail != b.Fail {
		return false
	}
	if !plannerStatesEqualCk(a.State, b.State) {
		return false
	}
	if !deferredCheckpointsEqualCk(
		a.DeferredCheckpoints, b.DeferredCheckpoints,
	) {
		return false
	}

	return txsEqualCk(a.SweepTx, b.SweepTx)
}

// deferredCheckpointsEqualCk compares deferred checkpoints by value after
// canonical sorting.
func deferredCheckpointsEqualCk(a, b []DeferredCheckpoint) bool {
	a = copyDeferredCheckpoints(a)
	b = copyDeferredCheckpoints(b)
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// plannerStatesEqualCk compares two unrollplan.State values using
// order-insensitive semantics for the txid slices.
func plannerStatesEqualCk(a, b unrollplan.State) bool {
	if !hashSlicesEqualAsSetCk(a.ConfirmedTxids, b.ConfirmedTxids) {
		return false
	}
	if !hashSlicesEqualAsSetCk(a.InFlightTxids, b.InFlightTxids) {
		return false
	}
	if !optsEqualCk(a.TargetConfirmHeight, b.TargetConfirmHeight) {
		return false
	}

	return sweepStatesEqualCk(a.Sweep, b.Sweep)
}

// sweepStatesEqualCk compares two SweepState values field by field.
func sweepStatesEqualCk(a, b unrollplan.SweepState) bool {
	if a.Status != b.Status {
		return false
	}
	if !optsEqualCk(a.Txid, b.Txid) {
		return false
	}

	return optsEqualCk(a.ConfirmHeight, b.ConfirmHeight)
}

// txsEqualCk compares two serialized transactions. A byte-wise comparison
// avoids depending on MsgTx equality semantics (which does not exist as a
// method and whose struct equality is sensitive to uninitialised witness
// slices).
func txsEqualCk(a, b *wire.MsgTx) bool {
	switch {
	case a == nil && b == nil:
		return true

	case a == nil || b == nil:
		return false
	}

	var abuf, bbuf bytes.Buffer
	if err := a.Serialize(&abuf); err != nil {
		return false
	}
	if err := b.Serialize(&bbuf); err != nil {
		return false
	}

	return bytes.Equal(abuf.Bytes(), bbuf.Bytes())
}

// optsEqualCk compares two fn.Option values by presence + inner equality.
func optsEqualCk[T comparable](a, b fn.Option[T]) bool {
	if a.IsSome() != b.IsSome() {
		return false
	}
	if a.IsNone() {
		return true
	}

	return a.UnsafeFromSome() == b.UnsafeFromSome()
}

// hashSlicesEqualAsSetCk compares two hash slices ignoring order.
func hashSlicesEqualAsSetCk(a, b []chainhash.Hash) bool {
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

// requireCheckpointEqual compares two checkpoints and emits a clear diagnostic
// on mismatch. Used by the handcrafted cases.
func requireCheckpointEqual(t *testing.T, want, got *actorCheckpoint) {
	t.Helper()

	require.Equal(t, want.Version, got.Version)
	require.Equal(t, want.Height, got.Height)
	require.Equal(t, want.Started, got.Started)
	require.Equal(t, want.Trigger, got.Trigger)
	require.Equal(t, want.Fail, got.Fail)
	require.Equal(t, want.SweepAttempts, got.SweepAttempts)
	require.Equal(
		t, want.ExternalSpendFinalized, got.ExternalSpendFinalized,
	)
	require.Equal(t, want.ReliveUnsafe, got.ReliveUnsafe)
	require.ElementsMatch(
		t, want.DeferredCheckpoints, got.DeferredCheckpoints,
	)
	require.ElementsMatch(
		t, want.State.ConfirmedTxids, got.State.ConfirmedTxids,
	)
	require.ElementsMatch(
		t, want.State.InFlightTxids, got.State.InFlightTxids,
	)
	require.Equal(
		t, want.State.TargetConfirmHeight.IsSome(),
		got.State.TargetConfirmHeight.IsSome(),
	)
	if want.State.TargetConfirmHeight.IsSome() {
		require.Equal(
			t, want.State.TargetConfirmHeight.UnsafeFromSome(),
			got.State.TargetConfirmHeight.UnsafeFromSome(),
		)
	}
	require.Equal(t, want.State.Sweep.Status, got.State.Sweep.Status)
	require.True(
		t, txsEqualCk(want.SweepTx, got.SweepTx),
		"sweep transactions differ",
	)
	require.Equal(
		t, want.ProvisionalExternalSpend.IsSome(),
		got.ProvisionalExternalSpend.IsSome(),
	)
	if want.ProvisionalExternalSpend.IsSome() {
		require.Equal(
			t, want.ProvisionalExternalSpend.UnsafeFromSome(),
			got.ProvisionalExternalSpend.UnsafeFromSome(),
		)
	}
}

// hashFromByteCk builds a chainhash.Hash with the first byte set to b, useful
// for hand-rolled distinct test hashes.
func hashFromByteCk(b byte) chainhash.Hash {
	var h chainhash.Hash
	h[0] = b

	return h
}
