package oor

import (
	"context"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	libtypes "github.com/lightninglabs/darepo-client/lib/types"
)

// PackageDirection aliases the shared OOR package direction enum.
type PackageDirection = libtypes.OORPackageDirection

const (
	// PackageDirectionIncoming marks a package received by this wallet.
	PackageDirectionIncoming = libtypes.OORPackageDirectionIncoming

	// PackageDirectionOutgoing marks a package sent by this wallet.
	PackageDirectionOutgoing = libtypes.OORPackageDirectionOutgoing
)

// PackageLinkKind aliases the shared OOR package-link-kind enum.
type PackageLinkKind = libtypes.OORPackageLinkKind

const (
	// PackageLinkKindCreatedOutput marks a binding where the local outpoint
	// is created by the package Ark transaction.
	PackageLinkKindCreatedOutput = libtypes.OORPackageLinkKindCreatedOutput

	// PackageLinkKindConsumedInput marks a binding where the local outpoint
	// is consumed as an input by this package.
	PackageLinkKindConsumedInput = libtypes.OORPackageLinkKindConsumedInput
)

// PackagePersistence is the minimal storage contract needed by the OOR runtime
// to persist packages and local outpoint bindings.
//
// This is intentionally small and defined in the OOR package so the runtime is
// not coupled to a specific database package.
type PackagePersistence interface {
	// UpsertPackage stores or updates one session's finalized package
	// artifacts.
	//
	// Implementations should ensure retries are idempotent for the same
	// session ID and package payload.
	UpsertPackage(ctx context.Context, direction PackageDirection,
		sessionID chainhash.Hash, ark *psbt.Packet,
		checkpoints []*psbt.Packet) error

	// UpsertBinding stores or updates one local outpoint-to-session
	// relation.
	//
	// Bindings map local spendable outputs to persisted package
	// artifacts so callers can resolve unroll data by outpoint.
	UpsertBinding(ctx context.Context, outpoint wire.OutPoint,
		sessionID chainhash.Hash, outputIndex uint32,
		linkKind PackageLinkKind) error
}
