package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// drawComposedIntent generates a random pre-designation pair of
// (vtxos, leaves) shaped like the composed intent that
// PendingRoundAssembly hands to designateChangeMarker. Each entry
// independently rolls IsChange so the property test exercises:
//
//   - All-unset slices (refresh-only / leave-only auto paths).
//   - Slices where exactly one entry has IsChange already set
//     (boarding-change or directed-send self-change paths).
//   - Slices where multiple entries already have IsChange set
//     (defensive dedup path; can only happen via a regression in
//     an entry-point handler).
//
// The slice lengths are bounded so a single rapid example runs in
// well under a millisecond.
func drawComposedIntent(rt *rapid.T) ([]types.VTXORequest,
	[]*types.LeaveRequest) {

	vtxoCount := rapid.IntRange(0, 4).Draw(rt, "vtxo_count")
	leaveCount := rapid.IntRange(0, 4).Draw(rt, "leave_count")

	vtxos := make([]types.VTXORequest, vtxoCount)
	for i := range vtxos {
		isChange := rapid.Bool().Draw(
			rt, "vtxo_is_change",
		)
		vtxos[i] = types.VTXORequest{
			Amount:   btcutil.Amount(50_000),
			IsChange: isChange,
		}
	}

	leaves := make([]*types.LeaveRequest, leaveCount)
	for i := range leaves {
		isChange := rapid.Bool().Draw(
			rt, "leave_is_change",
		)
		leaves[i] = &types.LeaveRequest{
			Output: &wire.TxOut{
				Value: 50_000,
				PkScript: []byte{
					0x51,
					0x20,
				},
			},
			IsChange: isChange,
		}
	}

	return vtxos, leaves
}

func countChangeMarkers(
	vtxos []types.VTXORequest, leaves []*types.LeaveRequest,
) int {

	var n int
	for _, req := range vtxos {
		if req.IsChange {
			n++
		}
	}
	for _, leave := range leaves {
		if leave != nil && leave.IsChange {
			n++
		}
	}

	return n
}

// TestPropertyDesignateChangeMarkerExactlyOneOrZero is the
// rapid-driven property test the codex agent suggested as Layer 1
// coverage for the #270 single-change invariant. For every random
// composed intent that PendingRoundAssembly might hand to
// designateChangeMarker:
//
//  1. After designateChangeMarker, the marker count must be
//     exactly 1 when total outputs > 1, and exactly 0 when total
//     outputs ≤ 1 (single-output intents need no marker by proto
//     contract).
//
//  2. If the input had at least one marker already set,
//     designateChangeMarker must NOT add a new marker — only
//     keep / dedup. This is the "respect explicit wallet
//     decisions" guarantee that boarding change and directed-send
//     self-change rely on.
//
//  3. The first marker (in VTXOs-then-leaves order) is always
//     preserved when at least one was set on input. This pins the
//     deterministic dedup ordering — a regression that picked the
//     last marker instead would still satisfy property 1 but
//     would misroute the residual to a different slot.
//
// Without this property test, a future change to the entry-point
// handlers (auto-refresh, manual refresh, manual leave, directed
// send, future paths) could re-introduce double-stamping in a
// shape the targeted unit tests miss.
func TestPropertyDesignateChangeMarkerExactlyOneOrZero(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		vtxos, leaves := drawComposedIntent(rt)
		totalOutputs := len(vtxos) + len(leaves)
		inputMarkers := countChangeMarkers(vtxos, leaves)

		// Capture the first-marker position (if any) BEFORE
		// running designateChangeMarker so we can assert it
		// survives.
		var (
			haveFirstVTXOMark  bool
			firstVTXOMarkIdx   int
			haveFirstLeaveMark bool
			firstLeaveMarkIdx  int
		)
		for i, req := range vtxos {
			if !req.IsChange {
				continue
			}
			haveFirstVTXOMark = true
			firstVTXOMarkIdx = i

			break
		}
		for i, leave := range leaves {
			if leave == nil || !leave.IsChange {
				continue
			}
			haveFirstLeaveMark = true
			firstLeaveMarkIdx = i

			break
		}

		designateChangeMarker(vtxos, leaves)

		got := countChangeMarkers(vtxos, leaves)
		switch {
		case totalOutputs == 0:
			require.Equal(
				t, 0, got,
				"empty intent must remain marker-free",
			)

		case totalOutputs == 1:
			// Single-output intents may carry an explicit
			// marker (e.g., the wallet pre-stamped one) or
			// no marker at all. designateChangeMarker must
			// NOT add a marker for the single-output case;
			// it may leave an explicit one alone.
			require.LessOrEqual(
				t, got, 1, "single-output intent must have "+
					"at most one marker after designation",
			)
			if inputMarkers == 0 {
				require.Equal(
					t, 0, got, "single-output intent "+
						"without a pre-stamped "+
						"marker must stay marker-free",
				)
			}

		default:
			require.Equal(
				t, 1, got, "multi-output intent must have "+
					"exactly one IsChange=true marker",
			)
		}

		// First-marker preservation — only meaningful when the
		// input already had a marker.
		if !haveFirstVTXOMark && !haveFirstLeaveMark {
			return
		}

		// VTXOs win over leaves: if the input had a VTXO
		// marker, it should survive at the same position. If
		// not, the first leave marker (if any) survives.
		switch {
		case haveFirstVTXOMark:
			require.True(
				t, vtxos[firstVTXOMarkIdx].IsChange,
				"first VTXO marker must survive dedup",
			)

		case haveFirstLeaveMark:
			require.True(
				t, leaves[firstLeaveMarkIdx].IsChange, "firs"+
					"t leave marker must survive dedup "+
					"when no VTXO marker was set",
			)
		}
	})
}
