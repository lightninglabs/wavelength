package db

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/stretchr/testify/require"
)

// TestDiagnosticPackagesRequiresWalletAncestry proves that a foreign chain
// collapses only from a local root, through actual checkpoint spends. A
// package with an immutable dispatch stays visible without its registry row.
func TestDiagnosticPackagesRequiresWalletAncestry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	database := NewTestDB(t)
	artifacts := newOORArtifactStoreFromBaseDB(database.BaseDB)
	status := NewOORStatusStore(
		NewStore(
			database.DB, database.Queries, database.Backend(),
			btclog.Disabled,
		),
	)

	grandparent, ark, checks, grandparentOutput, _, _, _ :=
		buildTestOORPackage(t, 1)
	require.NoError(
		t, artifacts.UpsertPackage(
			ctx, OORPackageDirectionIncoming, grandparent, ark,
			checks,
		),
	)
	parent, ark, checks, parentOutput, _, _, _ :=
		buildTestOORPackageWithInput(t, 2, grandparentOutput)
	require.NoError(
		t, artifacts.UpsertPackage(
			ctx, OORPackageDirectionIncoming, parent, ark, checks,
		),
	)
	unrelated, ark, extraChecks, _, _, _, _ := buildTestOORPackage(t, 3)
	require.NoError(
		t, artifacts.UpsertPackage(
			ctx, OORPackageDirectionIncoming, unrelated, ark,
			extraChecks,
		),
	)
	falseParent, ark, extraChecks, falseOutput, _, _, _ :=
		buildTestOORPackage(t, 4)
	require.NoError(
		t, artifacts.UpsertPackage(
			ctx, OORPackageDirectionIncoming, falseParent, ark,
			extraChecks,
		),
	)
	_, _, fakeChecks, _, _, _, _ :=
		buildTestOORPackageWithInput(t, 5, falseOutput)
	root, ark, checks, _, _, _, _ :=
		buildTestOORPackageWithInput(t, 6, parentOutput)
	require.NoError(
		t,
		artifacts.UpsertPackage(
			ctx, OORPackageDirectionIncoming, root, ark,
			append(checks, fakeChecks...),
		),
	)
	require.NoError(
		t,
		database.InsertOORDispatchAttempt(
			ctx, sqlc.InsertOORDispatchAttemptParams{
				IdempotencyKey: "wallet-root",
				SessionID:      root[:],
			},
		),
	)

	possible := []chainhash.Hash{
		grandparent,
		parent,
		unrelated,
		falseParent,
	}
	got, err := status.DiagnosticPackages(ctx, possible)
	require.NoError(t, err)
	require.Equal(t, map[chainhash.Hash]OORDiagnosticPackage{
		grandparent: {}, parent: {}, unrelated: {}, falseParent: {},
	}, got)

	got, err = status.DiagnosticPackages(ctx, append(possible, root))
	require.NoError(t, err)
	require.Equal(t, map[chainhash.Hash]OORDiagnosticPackage{
		grandparent: {RecoveryAncestor: true},
		parent:      {RecoveryAncestor: true},
		unrelated:   {},
		falseParent: {},
	}, got)

	// A subsequent real wallet session supplies positive evidence for
	// the same package and takes precedence over its ancestry role.
	require.NoError(
		t,
		database.UpsertOORSessionRegistry(
			ctx, sqlc.UpsertOORSessionRegistryParams{
				SessionID: parent[:],
				ActorID:   "local-receive",
				Direction: int32(OORSessionDirectionIncoming),
			},
		),
	)
	got, err = status.DiagnosticPackages(ctx, append(possible, root))
	require.NoError(t, err)
	require.NotContains(t, got, parent)
	require.True(t, got[grandparent].RecoveryAncestor)
}

// TestDiagnosticPackagesRejectsMissingAncestorOutput pins the exact prevout
// check so a matching transaction hash alone cannot collapse a package.
func TestDiagnosticPackagesRejectsMissingAncestorOutput(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	database := NewTestDB(t)
	artifacts := newOORArtifactStoreFromBaseDB(database.BaseDB)
	status := NewOORStatusStore(
		NewStore(
			database.DB, database.Queries, database.Backend(),
			btclog.Disabled,
		),
	)
	parent, ark, checks, _, _, _, _ := buildTestOORPackage(t, 7)
	require.NoError(
		t, artifacts.UpsertPackage(
			ctx, OORPackageDirectionIncoming, parent, ark, checks,
		),
	)
	root, ark, checks, _, _, _, _ := buildTestOORPackageWithInput(
		t, 8, wire.OutPoint{
			Hash:  parent,
			Index: 99,
		},
	)
	require.NoError(
		t, artifacts.UpsertPackage(
			ctx, OORPackageDirectionOutgoing, root, ark, checks,
		),
	)
	got, err := status.DiagnosticPackages(
		ctx, []chainhash.Hash{parent, root},
	)
	require.NoError(t, err)
	require.Equal(
		t, map[chainhash.Hash]OORDiagnosticPackage{
			parent: {},
		},
		got,
	)
}
