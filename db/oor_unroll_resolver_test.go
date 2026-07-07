package db

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
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
		Hash: chainhash.Hash{
			0x99,
			0x01,
		},
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
		Hash: chainhash.Hash{
			0x44,
			0xaa,
		},
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

// TestResolveUnrollPackagesUsesPersistedAncestorPackage verifies chained
// receive artifacts can satisfy ancestry resolution without a local VTXO
// binding for the intermediate parent output.
func TestResolveUnrollPackagesUsesPersistedAncestorPackage(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, roundStore := newOORArtifactStoreForTest(t)

	externalInput := wire.OutPoint{
		Hash: chainhash.Hash{
			0x51,
			0xaa,
		},
		Index: 0,
	}

	parentSession, parentArk, parentCheckpoints, parentOutpoint,
		_, _, _ := buildTestOORPackageWithInput(
		t, 0x51, externalInput,
	)

	err := store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, parentSession, parentArk,
		parentCheckpoints,
	)
	require.NoError(t, err)

	require.Equal(t, parentSession, parentOutpoint.Hash)

	childSession, childArk, childCheckpoints, childOutpoint, childScript,
		childValue, _ := buildTestOORPackageWithInput(
		t, 0x52, parentOutpoint,
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
		t, unresolvedInput, resolved.UnresolvedCheckpointInputs[0],
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
		Hash: chainhash.Hash{
			0x71,
			0xaa,
		},
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
		Hash: chainhash.Hash{
			0x91,
			0xaa,
		},
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

// TestResolveUnrollPackagesRejectsTxidMismatchedAncestor verifies that the
// foreign-ancestor fallback rejects a persisted package whose stored Ark tx
// does not actually hash to the requested session id. Such a row would
// otherwise let a poisoned operator/indexer response stand in as the parent
// of any checkpoint input whose previous hash happened to match the rogue
// session id.
func TestResolveUnrollPackagesRejectsTxidMismatchedAncestor(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, roundStore := newOORArtifactStoreForTest(t)

	// Build a parent package and rebind its row under a session id that
	// does not match the actual ark txid. This simulates a poisoned
	// session-keyed row written by a buggy or malicious code path that
	// did not enforce the txid binding at write time.
	parentSession, parentArk, parentCheckpoints, _,
		_, _, _ := buildTestOORPackageWithInput(
		t, 0xa1, wire.OutPoint{
			Hash: chainhash.Hash{0xa1, 0xaa},
		},
	)

	rogueSession := chainhash.Hash{0xde, 0xad, 0xbe, 0xef}
	require.NotEqual(t, parentSession, rogueSession)

	err := store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, rogueSession, parentArk,
		parentCheckpoints,
	)
	require.NoError(t, err)

	// Build a child package whose checkpoint input claims the rogue
	// session id as its parent. The fallback lookup will succeed at the
	// DB layer, so the txid-binding check is what must keep the
	// poisoned ancestor out of the resolved chain.
	rogueParentOutpoint := wire.OutPoint{
		Hash:  rogueSession,
		Index: 0,
	}

	childSession, childArk, childCheckpoints, childOutpoint, childScript,
		childValue, _ := buildTestOORPackageWithInput(
		t, 0xa2, rogueParentOutpoint,
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

	// The poisoned ancestor must be rejected: only the child package
	// remains and the rogue parent outpoint is surfaced as unresolved.
	require.Len(t, resolved.Packages, 1)
	require.Equal(t, childSession, resolved.Packages[0].SessionID)
	require.Len(t, resolved.UnresolvedCheckpointInputs, 1)
	require.Equal(
		t, rogueParentOutpoint, resolved.UnresolvedCheckpointInputs[0],
	)
}

// TestResolveUnrollPackagesRejectsAnchorOutputAncestor verifies the
// foreign-ancestor fallback rejects a persisted package whose claimed
// parent output index lands on the Ark anchor output. Anchor outputs are
// never spendable VTXOs, so a child checkpoint that purports to spend one
// could only originate from a malformed or malicious package and must not
// be accepted as resolved ancestry.
func TestResolveUnrollPackagesRejectsAnchorOutputAncestor(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, roundStore := newOORArtifactStoreForTest(t)

	// The fixture builds an Ark PSBT with output 0 = recipient and
	// output 1 = anchor. We point the child checkpoint input at output 1.
	parentSession, parentArk, parentCheckpoints, _,
		_, _, _ := buildTestOORPackageWithInput(
		t, 0xb1, wire.OutPoint{
			Hash: chainhash.Hash{0xb1, 0xaa},
		},
	)

	err := store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, parentSession, parentArk,
		parentCheckpoints,
	)
	require.NoError(t, err)

	require.True(
		t, len(parentArk.UnsignedTx.TxOut) >= 2,
		"fixture must produce both recipient and anchor outputs",
	)

	anchorParentOutpoint := wire.OutPoint{
		Hash:  parentSession,
		Index: 1,
	}

	childSession, childArk, childCheckpoints, childOutpoint, childScript,
		childValue, _ := buildTestOORPackageWithInput(
		t, 0xb2, anchorParentOutpoint,
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

	// The parent package exists and the txid binding is correct, but
	// the referenced output is the anchor, so the fallback must refuse
	// to graft it onto the unroll chain.
	require.Len(t, resolved.Packages, 1)
	require.Equal(t, childSession, resolved.Packages[0].SessionID)
	require.Len(t, resolved.UnresolvedCheckpointInputs, 1)
	require.Equal(
		t, anchorParentOutpoint, resolved.UnresolvedCheckpointInputs[0],
	)
}
