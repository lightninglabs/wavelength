package oor

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	libtypes "github.com/lightninglabs/wavelength/lib/types"
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

// TaprootAssetPackagePersistence is the optional package-store extension that
// durably retains the sealed asset transition container beside the Bitcoin
// package. Asset-bearing sessions fail closed when the configured store does
// not implement this interface.
type TaprootAssetPackagePersistence interface {
	UpsertPackageWithAssets(ctx context.Context, direction PackageDirection,
		sessionID chainhash.Hash, ark *psbt.Packet,
		checkpoints []*psbt.Packet,
		assetTransfer *oortx.TaprootAssetTransfer) error
}

func upsertPackage(ctx context.Context, store PackagePersistence,
	direction PackageDirection, sessionID chainhash.Hash, ark *psbt.Packet,
	checkpoints []*psbt.Packet,
	assetTransfer *oortx.TaprootAssetTransfer) error {

	if assetTransfer == nil {
		return store.UpsertPackage(
			ctx, direction, sessionID, ark, checkpoints,
		)
	}

	assetStore, ok := store.(TaprootAssetPackagePersistence)
	if !ok {
		return fmt.Errorf("package store does not support Taproot " +
			"Asset transfers")
	}

	return assetStore.UpsertPackageWithAssets(
		ctx, direction, sessionID, ark, checkpoints, assetTransfer,
	)
}

const (
	// ReservationOwnerKindOOROutgoing is the owner_kind value recorded for
	// reservations held by an outgoing OOR session.
	ReservationOwnerKindOOROutgoing = 0

	// ReservationOwnerKindTaprootAssetPreparation records a reservation
	// before the first external Taproot Asset commit. The deterministic
	// request digest is the owner ID, allowing retries and restart recovery
	// to upsert the same reservation.
	ReservationOwnerKindTaprootAssetPreparation = 1
)

// ReservationStore is the minimal storage contract the OOR runtime needs to
// record durable spending reservations. A row is written either before the
// first external Taproot Asset commit or when a new outgoing session is
// checkpointed, so a startup sweep can tell an in-flight or quarantined spend
// from an orphaned one.
//
// It is intentionally small and defined here so the runtime is not coupled to
// a specific database package.
type ReservationStore interface {
	// UpsertReservation records that the given outpoint is reserved by the
	// owner identified by ownerKind/ownerID.
	UpsertReservation(ctx context.Context, outpoint wire.OutPoint,
		ownerKind int, ownerID chainhash.Hash) error
}

// PackageArtifact carries the finalized package data for one OOR session.
//
// Incoming chained transfers use this to persist ancestor packages that are
// required for unilateral exit but do not correspond to wallet-owned VTXOs.
type PackageArtifact struct {
	// SessionID is the deterministic Ark txid for the package.
	SessionID SessionID

	// ArkPSBT is the canonical Ark transaction package.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs are the finalized checkpoints for the package.
	FinalCheckpointPSBTs []*psbt.Packet

	// TaprootAssetTransfer is the optional immutable sealed asset package
	// container for this session.
	TaprootAssetTransfer *oortx.TaprootAssetTransfer
}
