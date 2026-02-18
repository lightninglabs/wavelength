package db

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestResolveUnrollPackagesUnknownOutpoint verifies the resolver returns the
// underlying not-found error when no package binding exists for the target
// outpoint.
func TestResolveUnrollPackagesUnknownOutpoint(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newOORArtifactStoreForTest(t)

	target := wire.OutPoint{
		Hash:  chainhash.Hash{0x99, 0x01},
		Index: 7,
	}

	_, err := store.ResolveUnrollPackages(ctx, target)
	require.Error(t, err)
	require.True(t, errors.Is(err, sql.ErrNoRows))
}

// TestResolveUnrollPackagesWithKnownAncestor verifies that resolver traversal
// returns a deterministic ancestor-first package chain and surfaces unresolved
// outermost checkpoint inputs.
func TestResolveUnrollPackagesWithKnownAncestor(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, roundStore := newOORArtifactStoreForTest(t)

	externalInput := wire.OutPoint{
		Hash:  chainhash.Hash{0x44, 0xaa},
		Index: 0,
	}

	parentSession, parentArk, parentCheckpoints, parentOutpoint,
		parentScript, parentValue, _ := buildTestOORPackageWithInput(
		t, 0x44, externalInput,
	)

	err := store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, parentSession, parentArk,
		parentCheckpoints,
	)
	require.NoError(t, err)

	seedBindingOutpoint(
		t, ctx, roundStore, parentOutpoint, parentScript, parentValue,
	)

	err = store.UpsertBinding(
		ctx, parentOutpoint, parentSession, 0,
		OORPackageLinkKindCreatedOutput,
	)
	require.NoError(t, err)

	childSession, childArk, childCheckpoints, childOutpoint, childScript,
		childValue, _ := buildTestOORPackageWithInput(
		t, 0x45, parentOutpoint,
	)

	err = store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, childSession, childArk,
		childCheckpoints,
	)
	require.NoError(t, err)

	seedBindingOutpoint(
		t, ctx, roundStore, childOutpoint, childScript, childValue,
	)

	err = store.UpsertBinding(
		ctx, childOutpoint, childSession, 0,
		OORPackageLinkKindCreatedOutput,
	)
	require.NoError(t, err)

	resolved, err := store.ResolveUnrollPackages(ctx, childOutpoint)
	require.NoError(t, err)
	require.NotNil(t, resolved)
	require.Equal(t, childOutpoint, resolved.TargetOutpoint)
	require.Len(t, resolved.Packages, 2)
	require.Equal(t, parentSession, resolved.Packages[0].SessionID)
	require.Equal(t, childSession, resolved.Packages[1].SessionID)
	require.Len(t, resolved.UnresolvedCheckpointInputs, 1)
	require.Equal(t, externalInput, resolved.UnresolvedCheckpointInputs[0])
}

// TestResolveUnrollPackagesDeduplicatesUnresolvedInputs verifies duplicate
// checkpoint references do not produce repeated unresolved outpoint entries.
func TestResolveUnrollPackagesDeduplicatesUnresolvedInputs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, roundStore := newOORArtifactStoreForTest(t)

	sessionID, arkPSBT, checkpoints, outpoint, script, valueSat,
		unresolvedInput := buildTestOORPackage(t, 0x61)

	duplicatedCheckpoints := []*psbt.Packet{
		checkpoints[0],
		checkpoints[0],
	}

	err := store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, sessionID, arkPSBT,
		duplicatedCheckpoints,
	)
	require.NoError(t, err)

	seedBindingOutpoint(t, ctx, roundStore, outpoint, script, valueSat)

	err = store.UpsertBinding(
		ctx, outpoint, sessionID, 0, OORPackageLinkKindCreatedOutput,
	)
	require.NoError(t, err)

	resolved, err := store.ResolveUnrollPackages(ctx, outpoint)
	require.NoError(t, err)
	require.Len(t, resolved.Packages, 1)
	require.Len(t, resolved.UnresolvedCheckpointInputs, 1)
	require.Equal(
		t, unresolvedInput,
		resolved.UnresolvedCheckpointInputs[0],
	)
}

// TestResolveUnrollPackagesCreatedOutputPreferred verifies ancestry traversal
// follows created-output bindings even when a consumed-input binding exists for
// the same outpoint.
func TestResolveUnrollPackagesCreatedOutputPreferred(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, roundStore := newOORArtifactStoreForTest(t)

	externalInput := wire.OutPoint{
		Hash:  chainhash.Hash{0x71, 0xaa},
		Index: 0,
	}

	parentSession, parentArk, parentCheckpoints, parentOutpoint,
		parentScript, parentValue, _ := buildTestOORPackageWithInput(
		t, 0x71, externalInput,
	)

	err := store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, parentSession, parentArk,
		parentCheckpoints,
	)
	require.NoError(t, err)

	seedBindingOutpoint(
		t, ctx, roundStore, parentOutpoint, parentScript, parentValue,
	)

	err = store.UpsertBinding(
		ctx, parentOutpoint, parentSession, 0,
		OORPackageLinkKindCreatedOutput,
	)
	require.NoError(t, err)

	childSession, childArk, childCheckpoints, childOutpoint, childScript,
		childValue, _ := buildTestOORPackageWithInput(
		t, 0x72, parentOutpoint,
	)

	err = store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, childSession, childArk,
		childCheckpoints,
	)
	require.NoError(t, err)

	seedBindingOutpoint(
		t, ctx, roundStore, childOutpoint, childScript, childValue,
	)

	err = store.UpsertBinding(
		ctx, childOutpoint, childSession, 0,
		OORPackageLinkKindCreatedOutput,
	)
	require.NoError(t, err)

	// Record that this outpoint was consumed in a later package.
	// This must not erase the created-output binding used for ancestry
	// traversal.
	seedBindingOutpoint(t, ctx, roundStore, parentOutpoint, nil, 0)

	err = store.UpsertBinding(
		ctx, parentOutpoint, childSession, 0,
		OORPackageLinkKindConsumedInput,
	)
	require.NoError(t, err)

	resolved, err := store.ResolveUnrollPackages(ctx, childOutpoint)
	require.NoError(t, err)
	require.Len(t, resolved.Packages, 2)
	require.Equal(t, parentSession, resolved.Packages[0].SessionID)
	require.Equal(t, childSession, resolved.Packages[1].SessionID)
	require.Len(t, resolved.UnresolvedCheckpointInputs, 1)
	require.Equal(t, externalInput, resolved.UnresolvedCheckpointInputs[0])
}

// TestResolveUnrollPackagesMaxDepthExceeded verifies resolver traversal stops
// once the configured depth bound is exceeded.
func TestResolveUnrollPackagesMaxDepthExceeded(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, roundStore := newOORArtifactStoreForTest(t)
	store.maxUnrollDepth = 2

	externalInput := wire.OutPoint{
		Hash:  chainhash.Hash{0x91, 0xaa},
		Index: 0,
	}

	prevInput := externalInput
	var targetOutpoint wire.OutPoint

	for i := 0; i < 4; i++ {
		seed := byte(0x91 + i)
		sessionID, arkPSBT, checkpoints, outpoint,
			script, valueSat, _ := buildTestOORPackageWithInput(
			t, seed, prevInput,
		)

		err := store.UpsertPackage(
			ctx, OORPackageDirectionIncoming, sessionID, arkPSBT,
			checkpoints,
		)
		require.NoError(t, err)

		seedBindingOutpoint(
			t, ctx, roundStore, outpoint, script, valueSat,
		)

		err = store.UpsertBinding(
			ctx, outpoint, sessionID, 0,
			OORPackageLinkKindCreatedOutput,
		)
		require.NoError(t, err)

		prevInput = outpoint
		targetOutpoint = outpoint
	}

	_, err := store.ResolveUnrollPackages(ctx, targetOutpoint)
	require.ErrorIs(t, err, ErrResolveUnrollMaxDepthExceeded)
}
