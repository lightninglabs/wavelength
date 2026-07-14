package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
)

// vtxoReqWithChange is a tiny helper that constructs a minimal
// VTXORequest with the IsChange bit set as requested. The other
// fields are intentionally left zero — designateChangeMarker only
// reads IsChange.
func vtxoReqWithChange(isChange bool) types.VTXORequest {
	return types.VTXORequest{
		Amount:   btcutil.Amount(50_000),
		IsChange: isChange,
	}
}

// fixedVTXOReqWithChange mirrors vtxoReqWithChange for fixed contract outputs.
func fixedVTXOReqWithChange(isChange bool) types.VTXORequest {
	req := vtxoReqWithChange(isChange)
	req.FixedAmount = true

	return req
}

// leaveReqWithChange mirrors vtxoReqWithChange for the leave path.
func leaveReqWithChange(isChange bool) *types.LeaveRequest {
	return &types.LeaveRequest{
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

// vtxoIsChangeAt returns the IsChange flag at the given index for
// readability in assertions.
func vtxoIsChangeAt(reqs []types.VTXORequest, i int) bool {
	return reqs[i].IsChange
}

func leaveIsChangeAt(reqs []*types.LeaveRequest, i int) bool {
	return reqs[i].IsChange
}

// TestDesignateChangeMarkerRefreshOnlyMultipleVTXOs covers the bug
// flagged by the PR review on round/actor.go (auto-refresh) and on
// wallet/wallet.go:874 (manual refresh): N expiring VTXOs flowing
// into a single assembling round must produce exactly one change
// marker, not N. With the entry points stripped of in-line
// IsChange=true, designateChangeMarker stamps the marker on the
// FIRST VTXO of the composed intent.
func TestDesignateChangeMarkerRefreshOnlyMultipleVTXOs(t *testing.T) {
	t.Parallel()

	vtxos := []types.VTXORequest{
		vtxoReqWithChange(false),
		vtxoReqWithChange(false),
		vtxoReqWithChange(false),
	}
	var leaves []*types.LeaveRequest

	designateChangeMarker(vtxos, leaves)

	require.True(t, vtxoIsChangeAt(vtxos, 0),
		"first VTXO must be marked")
	require.False(
		t, vtxoIsChangeAt(vtxos, 1),
		"second VTXO must remain unmarked",
	)
	require.False(
		t, vtxoIsChangeAt(vtxos, 2),
		"third VTXO must remain unmarked",
	)
}

// TestDesignateChangeMarkerSkipsFixedVTXOs verifies the default marker is not
// stamped on a fixed contract output, because that output must preserve its
// amount exactly and cannot absorb seal-time refresh fees.
func TestDesignateChangeMarkerSkipsFixedVTXOs(t *testing.T) {
	t.Parallel()

	vtxos := []types.VTXORequest{
		fixedVTXOReqWithChange(false),
		vtxoReqWithChange(false),
	}
	var leaves []*types.LeaveRequest

	designateChangeMarker(vtxos, leaves)

	require.False(t, vtxoIsChangeAt(vtxos, 0))
	require.True(t, vtxoIsChangeAt(vtxos, 1))
}

// TestDesignateChangeMarkerUsesLeaveWhenAllVTXOsFixed verifies an on-chain
// leave output can absorb the residual when every VTXO output is fixed.
func TestDesignateChangeMarkerUsesLeaveWhenAllVTXOsFixed(t *testing.T) {
	t.Parallel()

	vtxos := []types.VTXORequest{
		fixedVTXOReqWithChange(false),
		fixedVTXOReqWithChange(false),
	}
	leaves := []*types.LeaveRequest{
		leaveReqWithChange(false),
	}

	designateChangeMarker(vtxos, leaves)

	require.False(t, vtxoIsChangeAt(vtxos, 0))
	require.False(t, vtxoIsChangeAt(vtxos, 1))
	require.True(t, leaveIsChangeAt(leaves, 0))
}

// TestDesignateChangeMarkerLeaveOnlyMultipleLeaves covers the
// cooperative leave-only path (no VTXOs in the composed intent).
// The first leave request takes the change marker.
func TestDesignateChangeMarkerLeaveOnlyMultipleLeaves(t *testing.T) {
	t.Parallel()

	var vtxos []types.VTXORequest
	leaves := []*types.LeaveRequest{
		leaveReqWithChange(false),
		leaveReqWithChange(false),
	}

	designateChangeMarker(vtxos, leaves)

	require.True(
		t, leaveIsChangeAt(leaves, 0),
		"first leave must be marked",
	)
	require.False(
		t, leaveIsChangeAt(leaves, 1),
		"second leave must remain unmarked",
	)
}

// TestDesignateChangeMarkerPreservesExplicitVTXOMarker captures
// the boarding / directed-send self-change case: when a wallet
// path has explicitly stamped IsChange=true on a specific output,
// designateChangeMarker must NOT move or re-stamp the marker.
func TestDesignateChangeMarkerPreservesExplicitVTXOMarker(t *testing.T) {
	t.Parallel()

	vtxos := []types.VTXORequest{
		vtxoReqWithChange(false),
		vtxoReqWithChange(true),
		vtxoReqWithChange(false),
	}
	var leaves []*types.LeaveRequest

	designateChangeMarker(vtxos, leaves)

	require.False(t, vtxoIsChangeAt(vtxos, 0))
	require.True(
		t, vtxoIsChangeAt(vtxos, 1),
		"explicit second-position marker must survive",
	)
	require.False(t, vtxoIsChangeAt(vtxos, 2))
}

// TestDesignateChangeMarkerPreservesExplicitLeaveMarker covers
// the same invariant for an explicit leave-side marker (e.g., a
// future entry point that wants leaf 1 to absorb the residual).
func TestDesignateChangeMarkerPreservesExplicitLeaveMarker(t *testing.T) {
	t.Parallel()

	var vtxos []types.VTXORequest
	leaves := []*types.LeaveRequest{
		leaveReqWithChange(false),
		leaveReqWithChange(true),
	}

	designateChangeMarker(vtxos, leaves)

	require.False(t, leaveIsChangeAt(leaves, 0))
	require.True(t, leaveIsChangeAt(leaves, 1))
}

// TestDesignateChangeMarkerSingleOutputNoMarker pins the corner
// case for single-output intents: the proto contract does not
// require a marker (the lone output is implicitly the change
// slot), so the designator must not stamp one.
func TestDesignateChangeMarkerSingleOutputNoMarker(t *testing.T) {
	t.Parallel()

	t.Run("single_vtxo_no_marker", func(t *testing.T) {
		t.Parallel()

		vtxos := []types.VTXORequest{
			vtxoReqWithChange(false),
		}
		var leaves []*types.LeaveRequest

		designateChangeMarker(vtxos, leaves)

		require.False(
			t, vtxoIsChangeAt(vtxos, 0),
			"single VTXO must not get a marker stamped",
		)
	})

	t.Run("single_leave_no_marker", func(t *testing.T) {
		t.Parallel()

		var vtxos []types.VTXORequest
		leaves := []*types.LeaveRequest{
			leaveReqWithChange(false),
		}

		designateChangeMarker(vtxos, leaves)

		require.False(
			t, leaveIsChangeAt(leaves, 0),
			"single leave must not get a marker stamped",
		)
	})
}

// TestDesignateChangeMarkerMixedVTXOAndLeave covers the mixed-
// pool composed intent: when both VTXOs and leaves are present
// and neither has a marker, the FIRST VTXO gets the marker (VTXOs
// are preferred over leaves so the residual is absorbed off-chain
// when possible).
func TestDesignateChangeMarkerMixedVTXOAndLeave(t *testing.T) {
	t.Parallel()

	vtxos := []types.VTXORequest{
		vtxoReqWithChange(false),
		vtxoReqWithChange(false),
	}
	leaves := []*types.LeaveRequest{
		leaveReqWithChange(false),
	}

	designateChangeMarker(vtxos, leaves)

	require.True(
		t, vtxoIsChangeAt(vtxos, 0),
		"VTXO must be preferred over leave for the marker",
	)
	require.False(t, vtxoIsChangeAt(vtxos, 1))
	require.False(
		t, leaveIsChangeAt(leaves, 0),
		"leave must remain unmarked when a VTXO took the marker",
	)
}

// TestDesignateChangeMarkerDeduplicatesMultipleMarkersVTXOOnly
// captures the defensive dedup rule: if two paths each stamp a
// VTXO marker (e.g., a future entry-point regression), keep the
// first marker and clear the rest — the proto invariant is
// "exactly one", not "at least one".
func TestDesignateChangeMarkerDeduplicatesMultipleMarkersVTXOOnly(
	t *testing.T) {

	t.Parallel()

	vtxos := []types.VTXORequest{
		vtxoReqWithChange(true),
		vtxoReqWithChange(true),
		vtxoReqWithChange(false),
	}
	var leaves []*types.LeaveRequest

	designateChangeMarker(vtxos, leaves)

	require.True(t, vtxoIsChangeAt(vtxos, 0))
	require.False(
		t, vtxoIsChangeAt(vtxos, 1),
		"second marker must be cleared in dedup",
	)
	require.False(t, vtxoIsChangeAt(vtxos, 2))
}

// TestDesignateChangeMarkerDeduplicatesMixedVTXOAndLeaveMarkers
// covers the cross-pool dedup: a VTXO marker beats a leave
// marker. Collapsing in this direction matches the
// VTXO-preferred-over-leave rule used when no marker is set.
func TestDesignateChangeMarkerDeduplicatesMixedVTXOAndLeaveMarkers(
	t *testing.T) {

	t.Parallel()

	vtxos := []types.VTXORequest{
		vtxoReqWithChange(true),
		vtxoReqWithChange(false),
	}
	leaves := []*types.LeaveRequest{
		leaveReqWithChange(true),
	}

	designateChangeMarker(vtxos, leaves)

	require.True(t, vtxoIsChangeAt(vtxos, 0))
	require.False(t, vtxoIsChangeAt(vtxos, 1))
	require.False(
		t, leaveIsChangeAt(leaves, 0),
		"leave marker must be cleared when a VTXO already has one",
	)
}

// TestDesignateChangeMarkerDeduplicatesLeaveOnly covers dedup
// when the multi-marker case only involves leaves — keep first,
// clear rest.
func TestDesignateChangeMarkerDeduplicatesLeaveOnly(t *testing.T) {
	t.Parallel()

	var vtxos []types.VTXORequest
	leaves := []*types.LeaveRequest{
		leaveReqWithChange(true),
		leaveReqWithChange(true),
	}

	designateChangeMarker(vtxos, leaves)

	require.True(t, leaveIsChangeAt(leaves, 0))
	require.False(
		t, leaveIsChangeAt(leaves, 1),
		"trailing leave marker must be cleared in dedup",
	)
}

// TestDesignateChangeMarkerEmpty captures the trivial empty
// case — no VTXOs and no leaves means nothing to mark and
// nothing to panic on.
func TestDesignateChangeMarkerEmpty(t *testing.T) {
	t.Parallel()

	var vtxos []types.VTXORequest
	var leaves []*types.LeaveRequest

	designateChangeMarker(vtxos, leaves)

	require.Empty(t, vtxos)
	require.Empty(t, leaves)
}
