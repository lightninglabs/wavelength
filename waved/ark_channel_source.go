package waved

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/unrollbridge"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
)

// arkChannelRecoveryArchive exports and installs the exact finalized source
// package. The channel-policy VTXO is recovery-only, so it never appears in
// wallet balance, coin selection, refresh, or ordinary VTXO actor recovery.
type arkChannelRecoveryArchive struct {
	vtxos    *db.VTXOPersistenceStore
	packages *db.OORArtifactPersistenceStore
	watcher  *arkChannelSourceWatcher
}

// newArkChannelRecoveryArchive constructs one endpoint-local recovery store.
func newArkChannelRecoveryArchive(vtxos *db.VTXOPersistenceStore,
	packages *db.OORArtifactPersistenceStore,
	backend chainsource.ChainBackend,
	log btclog.Logger) (*arkChannelRecoveryArchive, error) {

	switch {
	case vtxos == nil:
		return nil, fmt.Errorf("Ark channel VTXO store is required")

	case packages == nil:
		return nil, fmt.Errorf("Ark channel OOR package store is " +
			"required")

	case backend == nil:
		return nil, fmt.Errorf("Ark channel chain backend is required")
	}

	return &arkChannelRecoveryArchive{
		vtxos: vtxos, packages: packages,
		watcher: newArkChannelSourceWatcher(backend, log),
	}, nil
}

// BindChannelEventSink connects confirmed ancestry spends to the channel FSM.
func (a *arkChannelRecoveryArchive) BindChannelEventSink(
	sink arkchannel.ChannelEventSink) error {

	return a.watcher.BindChannelEventSink(sink)
}

// ExportRecoveryPackage builds the endpoint-neutral package only after the
// funder's finalized OOR artifacts are durable.
func (a *arkChannelRecoveryArchive) ExportRecoveryPackage(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding) (arkchannel.RecoveryPackage, error) {

	if err := validateArkChannelRecoveryRequest(
		id, terms, source,
	); err != nil {
		return arkchannel.RecoveryPackage{}, err
	}
	target, err := a.loadSourcePackage(ctx, source)
	if err != nil {
		return arkchannel.RecoveryPackage{}, err
	}
	roots, err := a.loadSourceRoots(
		ctx, recoveryCheckpointInputs(target),
	)
	if err != nil {
		return arkchannel.RecoveryPackage{}, err
	}
	ancestry, err := recoveryAncestry(roots)
	if err != nil {
		return arkchannel.RecoveryPackage{}, err
	}
	roundID, commitmentTxID, batchExpiry, createdHeight :=
		recoveryRootMeta(roots)
	if roundID == "" || commitmentTxID == ([32]byte{}) ||
		batchExpiry == 0 || createdHeight == 0 {
		return arkchannel.RecoveryPackage{}, fmt.Errorf("Ark channel " +
			"source root metadata incomplete")
	}
	if _, err := channelOperatorKey(terms, roots); err != nil {
		return arkchannel.RecoveryPackage{}, err
	}
	// A sender persists the finalized package by session ID but does not
	// normally own or bind the output it created for the recipient. Resolve
	// from that immutable package identity before installing the output as
	// shared recovery state.
	resolved, err := a.packages.ResolveUnrollPackagesBySessionID(
		ctx, source.OutPoint, source.OORSessionID,
	)
	if err != nil {
		return arkchannel.RecoveryPackage{}, fmt.Errorf("resolve Ark "+
			"channel OOR lineage: %w", err)
	}
	if err := validateRecoveryPackageRoots(
		ancestry, resolved.UnresolvedCheckpointInputs,
	); err != nil {
		return arkchannel.RecoveryPackage{}, err
	}
	constructionVersion, err := recoveryConstructionVersion(roots)
	if err != nil {
		return arkchannel.RecoveryPackage{}, err
	}
	recovery := arkchannel.RecoveryPackage{
		Descriptor: arkchannel.RecoveryDescriptor{
			RoundID: roundID, CommitmentTxID: commitmentTxID,
			BatchExpiry: batchExpiry,
			ChainDepth: recoveryChainDepth(
				roots, len(resolved.Packages),
			),
			CreatedHeight:       createdHeight,
			ConstructionVersion: int32(constructionVersion),
		},
		Packages: make(
			[]arkchannel.RecoveryOORPackage, 0,
			len(resolved.Packages),
		),
	}
	for i := range ancestry {
		treePath, err := db.SerializeTree(ancestry[i].TreePath)
		if err != nil {
			return arkchannel.RecoveryPackage{}, fmt.Errorf(
				"serialize Ark channel ancestry %d: %w", i, err)
		}
		recovery.Descriptor.Ancestry = append(
			recovery.Descriptor.Ancestry,
			arkchannel.RecoveryAncestry{
				TreePath:       treePath,
				CommitmentTxID: ancestry[i].CommitmentTxID,
				InputIndices: append(
					[]uint32(nil),
					ancestry[i].InputIndices...,
				),
				TreeDepth:        ancestry[i].TreeDepth,
				CommitmentHeight: ancestry[i].CommitmentHeight,
			},
		)
	}
	for i := range resolved.Packages {
		entry, err := recoveryOORPackage(resolved.Packages[i])
		if err != nil {
			return arkchannel.RecoveryPackage{}, fmt.Errorf(
				"serialize Ark channel OOR package %d: %w", i,
				err)
		}
		recovery.Packages = append(recovery.Packages, entry)
	}
	if err := recovery.Validate(source); err != nil {
		return arkchannel.RecoveryPackage{}, err
	}
	desc, err := recoveryDescriptor(terms, source, recovery.Descriptor)
	if err != nil {
		return arkchannel.RecoveryPackage{}, err
	}
	if err := a.vtxos.SaveRecoveryOnlyVTXO(ctx, desc); err != nil {
		return arkchannel.RecoveryPackage{}, fmt.Errorf("save Ark "+
			"channel recovery source: %w", err)
	}
	if err := a.packages.UpsertBinding(
		ctx, source.OutPoint, source.OORSessionID,
		source.OutPoint.Index, db.OORPackageLinkKindCreatedOutput,
	); err != nil {
		return arkchannel.RecoveryPackage{}, fmt.Errorf("bind Ark "+
			"channel recovery source: %w", err)
	}
	if err := a.verifySourceLineage(ctx, source.OutPoint); err != nil {
		return arkchannel.RecoveryPackage{}, fmt.Errorf("verify "+
			"exported Ark channel lineage: %w", err)
	}

	return recovery, nil
}

// InstallRecoveryPackage writes every artifact, validates local resolution,
// and arms all ancestry watches before returning the activation barrier.
func (a *arkChannelRecoveryArchive) InstallRecoveryPackage(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms, source arkchannel.VTXOBinding,
	recovery arkchannel.RecoveryPackage) error {

	if err := validateArkChannelRecoveryRequest(
		id, terms, source,
	); err != nil {
		return err
	}
	if err := recovery.Validate(source); err != nil {
		return err
	}
	for i := range recovery.Packages {
		entry := recovery.Packages[i]
		arkPSBT, checkpoints, err := parseRecoveryOORPackage(entry)
		if err != nil {
			return fmt.Errorf("parse Ark channel OOR package "+
				"%d: %w", i, err)
		}
		if err := a.packages.UpsertPackage(
			ctx, db.OORPackageDirection(entry.Direction),
			entry.SessionID, arkPSBT, checkpoints,
		); err != nil {
			return fmt.Errorf("store Ark channel OOR package "+
				"%d: %w", i, err)
		}
	}
	desc, err := recoveryDescriptor(terms, source, recovery.Descriptor)
	if err != nil {
		return err
	}
	existing, err := a.vtxos.GetVTXO(ctx, source.OutPoint)
	switch {
	case err == nil:
		// Recovery installation is replayable after a close path has
		// selected its final CSV delay. Preserve that mutable field
		// while SaveRecoveryOnlyVTXO verifies every immutable
		// descriptor field.
		desc.RelativeExpiry = existing.RelativeExpiry

	case !errors.Is(err, vtxo.ErrVTXONotFound):
		return fmt.Errorf("load Ark channel recovery source: %w", err)
	}
	if err := a.vtxos.SaveRecoveryOnlyVTXO(ctx, desc); err != nil {
		return fmt.Errorf("save Ark channel recovery source: %w", err)
	}
	if err := a.packages.UpsertBinding(
		ctx, source.OutPoint, source.OORSessionID,
		source.OutPoint.Index, db.OORPackageLinkKindCreatedOutput,
	); err != nil {
		return fmt.Errorf("bind Ark channel OOR source: %w", err)
	}
	if err := a.verifySourceLineage(ctx, source.OutPoint); err != nil {
		return fmt.Errorf("verify installed Ark channel lineage: %w",
			err)
	}
	if err := a.watcher.Track(ctx, id, desc); err != nil {
		return fmt.Errorf("watch Ark channel source: %w", err)
	}

	return nil
}

// RestoreWatches re-arms every recovery-ready non-terminal channel at boot.
func (a *arkChannelRecoveryArchive) RestoreWatches(ctx context.Context,
	records []arkchannel.Record) error {

	for i := range records {
		snapshot := records[i].Snapshot
		if !snapshot.RecoveryReady || snapshot.Source == nil ||
			snapshot.Phase.IsTerminal() {

			continue
		}
		desc, err := a.vtxos.GetVTXO(ctx, snapshot.Source.OutPoint)
		if err != nil {
			return fmt.Errorf("restore Ark channel recovery "+
				"source %x: %w", snapshot.Terms.ID[:4], err)
		}
		if err := a.watcher.Track(
			ctx, snapshot.Terms.ID, desc,
		); err != nil {
			return err
		}
	}

	return nil
}

// EnsureChannelSource selects the final-spend delay and verifies that the
// pre-activation recovery package remains complete for the common unroller.
func (a *arkChannelRecoveryArchive) EnsureChannelSource(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms, source arkchannel.VTXOBinding,
	kind unroll.ExitPolicyKind) error {

	if err := validateArkChannelRecoveryRequest(
		id, terms, source,
	); err != nil {
		return err
	}
	csvDelay, err := channelSourceCSVDelay(terms, kind)
	if err != nil {
		return err
	}
	if err := a.vtxos.SetRecoveryOnlyVTXORelativeExpiry(
		ctx, source.OutPoint, csvDelay,
	); err != nil {
		return fmt.Errorf("set Ark channel recovery delay: %w", err)
	}
	if err := a.verifySourceLineage(ctx, source.OutPoint); err != nil {
		return fmt.Errorf("verify Ark channel lineage: %w", err)
	}

	return nil
}

// verifySourceLineage runs the common production resolver against the exact
// recovery-only descriptor that a later unroll job will consume.
func (a *arkChannelRecoveryArchive) verifySourceLineage(ctx context.Context,
	outpoint wire.OutPoint) error {

	resolver := unroll.DescriptorLineageResolver{
		VTXOStore:     a.vtxos,
		ArtifactStore: a.packages,
	}
	_, err := resolver.ResolveLineage(ctx, outpoint)

	return err
}

// validateRecoveryPackageRoots accepts package traversal endpoints only when
// the transported round ancestry supplies the transaction they spend.
func validateRecoveryPackageRoots(ancestry []vtxo.Ancestry,
	unresolved []wire.OutPoint) error {

	if len(unresolved) == 0 {
		return nil
	}
	treeTxIDs := make(map[[32]byte]struct{})
	for i := range ancestry {
		fragment := ancestry[i]
		if fragment.TreePath == nil || fragment.TreePath.Root == nil {
			return fmt.Errorf("Ark channel ancestry %d has no tree",
				i)
		}
		for node := range fragment.TreePath.Root.NodesIter() {
			tx, err := node.ToTx()
			if err != nil {
				return fmt.Errorf("build Ark channel ancestry "+
					"%d: %w", i, err)
			}
			treeTxIDs[tx.TxHash()] = struct{}{}
		}
	}
	for _, outpoint := range unresolved {
		if _, ok := treeTxIDs[outpoint.Hash]; !ok {
			return fmt.Errorf("Ark channel OOR lineage is "+
				"missing root %s", outpoint)
		}
	}

	return nil
}

// Stop releases all passive source watches.
func (a *arkChannelRecoveryArchive) Stop() {
	if a != nil && a.watcher != nil {
		a.watcher.Stop()
	}
}

// loadSourcePackage verifies that the finalized target package creates the
// exact channel-policy output committed by the FSM.
func (a *arkChannelRecoveryArchive) loadSourcePackage(ctx context.Context,
	source arkchannel.VTXOBinding) (*db.OORPackageBundle, error) {

	pkg, err := a.packages.GetPackage(ctx, source.OORSessionID)
	if err != nil {
		return nil, fmt.Errorf("load Ark channel OOR package: %w", err)
	}
	if pkg.ArkPSBT == nil || pkg.ArkPSBT.UnsignedTx == nil {
		return nil, fmt.Errorf("Ark channel OOR package has no " +
			"transaction")
	}
	tx := pkg.ArkPSBT.UnsignedTx
	if pkg.SessionID != source.OORSessionID ||
		tx.TxHash() != source.OutPoint.Hash {
		return nil, fmt.Errorf("Ark channel OOR package ID mismatch")
	}
	if int(source.OutPoint.Index) >= len(tx.TxOut) {
		return nil, fmt.Errorf("Ark channel OOR output is out of range")
	}
	output := tx.TxOut[source.OutPoint.Index]
	if output.Value != int64(source.Amount) ||
		!bytes.Equal(output.PkScript, source.PkScript) {
		return nil, fmt.Errorf("Ark channel OOR output mismatch")
	}
	if len(pkg.FinalCheckpointPSBTs) == 0 {
		return nil, fmt.Errorf("Ark channel OOR package has no " +
			"checkpoints")
	}

	return pkg, nil
}

// loadSourceRoots loads the wallet VTXOs consumed by the channel OOR package.
func (a *arkChannelRecoveryArchive) loadSourceRoots(ctx context.Context,
	outpoints []wire.OutPoint) ([]*vtxo.Descriptor, error) {

	if len(outpoints) == 0 {
		return nil, fmt.Errorf("Ark channel OOR package has no roots")
	}
	roots := make([]*vtxo.Descriptor, 0, len(outpoints))
	for _, outpoint := range outpoints {
		desc, err := a.vtxos.GetVTXO(ctx, outpoint)
		if err != nil {
			return nil, fmt.Errorf("load Ark channel root %s: %w",
				outpoint, err)
		}
		if len(desc.Ancestry) == 0 {
			return nil, fmt.Errorf("Ark channel root %s has no "+
				"ancestry", outpoint)
		}
		roots = append(roots, desc)
	}

	return roots, nil
}

// channelOperatorKey verifies that the channel and all roots share one Ark
// operator under BIP-340 x-only key semantics.
func channelOperatorKey(terms arkchannel.Terms,
	roots []*vtxo.Descriptor) (*btcec.PublicKey, error) {

	operatorKey, err := btcec.ParsePubKey(terms.VTXO.ArkOperatorKey[:])
	if err != nil {
		return nil, fmt.Errorf("parse Ark channel operator key: %w",
			err)
	}
	rootOperatorKey, err := recoveryOperatorKey(roots)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(
		schnorr.SerializePubKey(operatorKey),
		schnorr.SerializePubKey(rootOperatorKey),
	) {
		return nil, fmt.Errorf("Ark channel operator does not match " +
			"roots")
	}

	return operatorKey, nil
}

// recoveryConstructionVersion requires every contributing root to use the
// same Ark construction rules.
func recoveryConstructionVersion(roots []*vtxo.Descriptor) (
	arkrpc.ConstructionVersion, error) {

	if len(roots) == 0 || roots[0] == nil {
		return 0, fmt.Errorf("Ark channel recovery roots are empty")
	}
	version := roots[0].ConstructionVersion
	for i := 1; i < len(roots); i++ {
		if roots[i] == nil || roots[i].ConstructionVersion != version {
			return 0, fmt.Errorf("Ark channel roots use multiple " +
				"construction versions")
		}
	}

	return version, nil
}

// recoveryOORPackage serializes one persisted OOR package for transport.
func recoveryOORPackage(pkg *db.OORPackageBundle) (
	arkchannel.RecoveryOORPackage, error) {

	if pkg == nil || pkg.ArkPSBT == nil {
		return arkchannel.RecoveryOORPackage{}, fmt.Errorf("package " +
			"is nil")
	}
	arkPSBT, err := psbtutil.Serialize(pkg.ArkPSBT)
	if err != nil {
		return arkchannel.RecoveryOORPackage{}, err
	}
	entry := arkchannel.RecoveryOORPackage{
		SessionID: pkg.SessionID, Direction: int32(pkg.Direction),
		ArkPSBT:     arkPSBT,
		Checkpoints: make([][]byte, len(pkg.FinalCheckpointPSBTs)),
	}
	for i := range pkg.FinalCheckpointPSBTs {
		entry.Checkpoints[i], err = psbtutil.Serialize(
			pkg.FinalCheckpointPSBTs[i],
		)
		if err != nil {
			return arkchannel.RecoveryOORPackage{}, err
		}
	}

	return entry, nil
}

// parseRecoveryOORPackage decodes and verifies one transported package.
func parseRecoveryOORPackage(entry arkchannel.RecoveryOORPackage) (*psbt.Packet,
	[]*psbt.Packet, error) {

	arkPSBT, err := psbt.NewFromRawBytes(
		bytes.NewReader(entry.ArkPSBT), false,
	)
	if err != nil {
		return nil, nil, err
	}
	if arkPSBT.UnsignedTx == nil ||
		arkPSBT.UnsignedTx.TxHash() != entry.SessionID {
		return nil, nil, fmt.Errorf("OOR session ID does not match " +
			"Ark tx")
	}
	checkpoints := make([]*psbt.Packet, len(entry.Checkpoints))
	for i := range entry.Checkpoints {
		checkpoints[i], err = psbt.NewFromRawBytes(
			bytes.NewReader(entry.Checkpoints[i]), false,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("checkpoint %d: %w", i, err)
		}
		if checkpoints[i].UnsignedTx == nil {
			return nil, nil, fmt.Errorf("checkpoint %d has no tx",
				i)
		}
	}

	return arkPSBT, checkpoints, nil
}

// recoveryDescriptor reconstructs the local recovery-only VTXO descriptor.
func recoveryDescriptor(terms arkchannel.Terms, source arkchannel.VTXOBinding,
	recovery arkchannel.RecoveryDescriptor) (*vtxo.Descriptor, error) {

	clientKey, err := btcec.ParsePubKey(terms.VTXO.ClientArkKey[:])
	if err != nil {
		return nil, err
	}
	operatorKey, err := btcec.ParsePubKey(terms.VTXO.ArkOperatorKey[:])
	if err != nil {
		return nil, err
	}
	ancestry := make([]vtxo.Ancestry, len(recovery.Ancestry))
	for i := range recovery.Ancestry {
		treePath, err := db.DeserializeTree(
			recovery.Ancestry[i].TreePath,
		)
		if err != nil {
			return nil, fmt.Errorf("decode Ark channel ancestry "+
				"%d: %w", i, err)
		}
		ancestry[i] = vtxo.Ancestry{
			TreePath:       treePath,
			CommitmentTxID: recovery.Ancestry[i].CommitmentTxID,
			InputIndices: append(
				[]uint32(nil),
				recovery.Ancestry[i].InputIndices...,
			),
			TreeDepth:        recovery.Ancestry[i].TreeDepth,
			CommitmentHeight: recovery.Ancestry[i].CommitmentHeight,
		}
	}

	return &vtxo.Descriptor{
		Outpoint: source.OutPoint, Amount: source.Amount,
		PolicyTemplate: append([]byte(nil), source.PolicyTemplate...),
		PkScript:       append([]byte(nil), source.PkScript...),
		ClientKey: keychain.KeyDescriptor{
			PubKey: clientKey,
		},
		OperatorKey: operatorKey, Ancestry: ancestry,
		RoundID:        recovery.RoundID,
		CommitmentTxID: recovery.CommitmentTxID,
		BatchExpiry:    recovery.BatchExpiry,
		// The non-interactive fallback is the pre-signed client+hub
		// channel path. Persist its delay before watches are armed so a
		// confirmed ancestor cannot race proof assembly against a later
		// mutable update. A fully signed cooperative settlement selects
		// its immediate path through EnsureChannelSource before
		// admission.
		RelativeExpiry: terms.VTXO.ChannelDelay,
		ChainDepth:     recovery.ChainDepth,
		CreatedHeight:  recovery.CreatedHeight,
		Status:         vtxo.VTXOStatusRecoveryOnly,
		ConstructionVersion: arkrpc.ConstructionVersion(
			recovery.ConstructionVersion,
		),
	}, nil
}

// validateArkChannelRecoveryRequest binds all archive operations to one FSM.
func validateArkChannelRecoveryRequest(id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding) error {

	if id != terms.ID {
		return fmt.Errorf("Ark channel ID does not match terms")
	}
	if err := terms.Validate(); err != nil {
		return err
	}

	return source.Validate(terms)
}

// channelSourceCSVDelay selects the delay of the exact final spend policy.
func channelSourceCSVDelay(terms arkchannel.Terms,
	kind unroll.ExitPolicyKind) (uint32, error) {

	switch kind {
	case unrollbridge.ExitPolicyKind:
		return terms.VTXO.ChannelDelay, nil

	case unrollbridge.CooperativeCloseExitPolicyKind:
		return 0, nil

	default:
		return 0, fmt.Errorf("unsupported Ark channel exit policy %q",
			kind)
	}
}

var _ unrollbridge.SourcePreparer = (*arkChannelRecoveryArchive)(nil)
var _ arkchannel.ChannelEventSinkBinder = (*arkChannelRecoveryArchive)(nil)
