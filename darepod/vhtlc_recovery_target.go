package darepod

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/vhtlcrecovery"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
)

var errRecoveryTargetPackageMissing = errors.New("recovery target package " +
	"not found")

const (
	recoveryTargetPackagePollInterval = 100 * time.Millisecond
	recoveryTargetPackagePollTimeout  = 10 * time.Second
)

// vhtlcRecoveryTargetMaterializer prepares daemon-owned vHTLC outputs for the
// generic unroll subsystem. vHTLC outputs are not normal wallet coin-selection
// inventory: they are application-owned recovery targets. This adapter binds
// the OOR package that created the vHTLC output and stores a recovery-only VTXO
// descriptor so unroll can assemble lineage and watch the target without
// teaching generic unroll about swap semantics.
type vhtlcRecoveryTargetMaterializer struct {
	vtxos    vtxo.VTXOStore
	packages *db.OORArtifactPersistenceStore
	log      btclog.Logger
}

// newVHTLCRecoveryTargetMaterializer creates the darepod adapter used by the
// vHTLC recovery coordinator. The adapter is deliberately small and local to
// darepod because it stitches together darepod-owned stores rather than adding
// recovery-specific knowledge to unroll.
func newVHTLCRecoveryTargetMaterializer(vtxos vtxo.VTXOStore,
	packages *db.OORArtifactPersistenceStore,
	log btclog.Logger) *vhtlcRecoveryTargetMaterializer {

	return &vhtlcRecoveryTargetMaterializer{
		vtxos:    vtxos,
		packages: packages,
		log:      log,
	}
}

// EnsureRecoveryTarget makes job.VTXOOutpoint loadable by unroll. The method is
// idempotent: package bindings are upserts, SaveVTXO heals existing descriptor
// rows, and the status update keeps the target out of wallet coin selection on
// every retry.
func (m *vhtlcRecoveryTargetMaterializer) EnsureRecoveryTarget(
	ctx context.Context, job vhtlcrecovery.RecoveryJob) error {

	if m == nil {
		return fmt.Errorf("vhtlc recovery materializer is nil")
	}
	if m.vtxos == nil {
		return fmt.Errorf("vtxo store is required")
	}
	if m.packages == nil {
		return fmt.Errorf("oor package store is required")
	}

	desc, err := m.buildRecoveryDescriptor(ctx, job)
	if err != nil {
		return err
	}

	if err := m.vtxos.SaveVTXO(ctx, desc); err != nil {
		return fmt.Errorf("save recovery target descriptor: %w", err)
	}

	if err := m.vtxos.UpdateVTXOStatus(
		ctx, desc.Outpoint, vtxo.VTXOStatusSpending,
	); err != nil {
		return fmt.Errorf("reserve recovery target descriptor: %w", err)
	}

	if err := m.bindRecoveryTarget(ctx, desc); err != nil {
		return err
	}

	m.log.DebugS(ctx, "vhtlc recovery target materialized",
		slog.String("recovery_id", job.ID),
		slog.String("outpoint", desc.Outpoint.String()),
		slog.Int64("amount_sat", int64(desc.Amount)),
		slog.Uint64("csv_delay", uint64(desc.RelativeExpiry)),
		slog.Int("ancestry_paths", len(desc.Ancestry)),
		slog.Int("chain_depth", desc.ChainDepth),
	)

	return nil
}

// buildRecoveryDescriptor derives the recovery-only descriptor for the vHTLC
// target. It first binds the package output by the OOR session id (the Ark txid
// is the target outpoint hash), then resolves the package chain and copies the
// root ancestry from the locally stored input VTXO descriptors.
func (m *vhtlcRecoveryTargetMaterializer) buildRecoveryDescriptor(
	ctx context.Context, job vhtlcrecovery.RecoveryJob) (*vtxo.Descriptor,
	error) {

	target := job.VTXOOutpoint
	if target.Hash == (chainhash.Hash{}) {
		return nil, fmt.Errorf("recovery target outpoint hash is empty")
	}

	targetPkg, targetOutput, err := m.loadTargetPackage(ctx, job)
	if err != nil {
		return nil, err
	}

	roots, err := m.loadRootDescriptors(
		ctx, recoveryCheckpointInputs(targetPkg),
	)
	if err != nil {
		return nil, err
	}

	signingKey, err := recoverySigningKey(job)
	if err != nil {
		return nil, err
	}

	operatorKey, err := recoveryOperatorKey(roots)
	if err != nil {
		return nil, err
	}

	csvDelay, err := recoveryCSVDelay(job)
	if err != nil {
		return nil, err
	}

	policyTemplate, err := recoveryPolicyTemplate(
		job, targetOutput.PkScript,
	)
	if err != nil {
		return nil, err
	}

	ancestry := recoveryAncestry(roots)
	if len(ancestry) == 0 {
		return nil, fmt.Errorf("recovery target ancestry is empty")
	}

	roundID, commitmentTxID, batchExpiry, createdHeight := recoveryRootMeta(
		roots,
	)
	if roundID == "" || commitmentTxID == (chainhash.Hash{}) ||
		batchExpiry == 0 || createdHeight == 0 {
		return nil, fmt.Errorf("recovery root metadata incomplete")
	}

	chainDepth := recoveryChainDepth(roots, 1)

	return &vtxo.Descriptor{
		Outpoint: target,
		Amount:   btcutil.Amount(targetOutput.Value),
		PkScript: append([]byte(nil), targetOutput.PkScript...),
		ClientKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(job.SignerKeyFamily),
				Index:  uint32(job.SignerKeyIndex),
			},
			PubKey: signingKey,
		},
		OperatorKey:    operatorKey,
		Ancestry:       ancestry,
		RoundID:        roundID,
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    batchExpiry,
		RelativeExpiry: csvDelay,
		PolicyTemplate: policyTemplate,
		ChainDepth:     chainDepth,
		CreatedHeight:  createdHeight,
		Status:         vtxo.VTXOStatusSpending,
	}, nil
}

// loadTargetPackage verifies that the OOR package whose session id equals the
// target txid exists and returns the exact vHTLC output within that Ark
// transaction. The created-output binding is written only after the descriptor
// row exists, because the binding table intentionally references known VTXOs.
func (m *vhtlcRecoveryTargetMaterializer) loadTargetPackage(ctx context.Context,
	job vhtlcrecovery.RecoveryJob) (*db.OORPackageBundle, *wire.TxOut,
	error) {

	timeout := time.NewTimer(recoveryTargetPackagePollTimeout)
	defer timeout.Stop()

	ticker := time.NewTicker(recoveryTargetPackagePollInterval)
	defer ticker.Stop()

	for {
		pkg, output, err := m.loadTargetPackageOnce(ctx, job)
		if err == nil {
			return pkg, output, nil
		}
		if !errors.Is(err, errRecoveryTargetPackageMissing) {
			return nil, nil, err
		}

		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()

		case <-timeout.C:
			return nil, nil, err

		case <-ticker.C:
		}
	}
}

// loadTargetPackageOnce performs one package lookup and structural validation
// pass for the recovery target. A missing package is returned as a sentinel so
// the outer loader can absorb short persistence races without hiding malformed
// package contents or real database errors.
func (m *vhtlcRecoveryTargetMaterializer) loadTargetPackageOnce(
	ctx context.Context, job vhtlcrecovery.RecoveryJob) (
	*db.OORPackageBundle, *wire.TxOut, error) {

	target := job.VTXOOutpoint
	targetPkg, err := m.packages.GetPackage(ctx, target.Hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("%w: %s",
				errRecoveryTargetPackageMissing, target.Hash)
		}

		return nil, nil, fmt.Errorf("load recovery target package: %w",
			err)
	}

	if targetPkg.ArkPSBT == nil || targetPkg.ArkPSBT.UnsignedTx == nil {
		return nil, nil, fmt.Errorf("recovery target package %s has "+
			"no ark transaction", target.Hash)
	}

	arkTx := targetPkg.ArkPSBT.UnsignedTx
	if arkTx.TxHash() != target.Hash {
		return nil, nil, fmt.Errorf("recovery target package txid "+
			"does not match target: got %s want %s", arkTx.TxHash(),
			target.Hash)
	}
	if int(target.Index) >= len(arkTx.TxOut) {
		return nil, nil, fmt.Errorf("recovery target output index %d "+
			"out of range", target.Index)
	}

	targetOutput := arkTx.TxOut[target.Index]
	if job.VTXOAmountSat > 0 && targetOutput.Value != job.VTXOAmountSat {
		return nil, nil, fmt.Errorf("recovery target amount mismatch: "+
			"got %d want %d", targetOutput.Value, job.VTXOAmountSat)
	}

	return targetPkg, targetOutput, nil
}

// bindRecoveryTarget writes the created-output package binding for desc after
// SaveVTXO has made the target outpoint visible to the binding table's VTXO
// existence check. A final resolver pass catches any package-chain gap before
// the recovery service admits the target into unroll.
func (m *vhtlcRecoveryTargetMaterializer) bindRecoveryTarget(
	ctx context.Context, desc *vtxo.Descriptor) error {

	err := m.packages.UpsertBinding(
		ctx, desc.Outpoint, desc.Outpoint.Hash, desc.Outpoint.Index,
		db.OORPackageLinkKindCreatedOutput,
	)
	if err != nil {
		return fmt.Errorf("bind recovery target package: %w", err)
	}

	if _, err := m.packages.ResolveUnrollPackages(
		ctx, desc.Outpoint,
	); err != nil {
		return fmt.Errorf("verify recovery package chain: %w", err)
	}

	return nil
}

// loadRootDescriptors loads the locally known VTXOs that anchor the earliest
// unresolved checkpoint inputs in the OOR package graph. Those root descriptors
// are allowed to be terminal, because they were already spent by the OOR
// package; their ancestry remains the authoritative round-birth material for
// the recovery target.
func (m *vhtlcRecoveryTargetMaterializer) loadRootDescriptors(
	ctx context.Context, roots []wire.OutPoint) ([]*vtxo.Descriptor,
	error) {

	if len(roots) == 0 {
		return nil, fmt.Errorf("recovery package chain has no root " +
			"checkpoint inputs")
	}

	descs := make([]*vtxo.Descriptor, 0, len(roots))
	seen := make(map[wire.OutPoint]struct{}, len(roots))
	for _, root := range roots {
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}

		desc, err := m.vtxos.GetVTXO(ctx, root)
		if err != nil {
			return nil, fmt.Errorf("load recovery root %s: %w",
				root, err)
		}
		if len(desc.Ancestry) == 0 {
			return nil, fmt.Errorf("recovery root %s has no "+
				"ancestry", root)
		}

		descs = append(descs, desc)
	}

	return descs, nil
}

// recoveryCheckpointInputs returns the unique checkpoint inputs referenced by
// the target package. These inputs are the immediate local roots from which the
// synthesized recovery descriptor inherits its round-birth ancestry.
func recoveryCheckpointInputs(pkg *db.OORPackageBundle) []wire.OutPoint {
	if pkg == nil {
		return nil
	}

	seen := make(map[wire.OutPoint]struct{})
	var outpoints []wire.OutPoint
	for _, checkpoint := range pkg.FinalCheckpointPSBTs {
		if checkpoint == nil || checkpoint.UnsignedTx == nil {
			continue
		}

		for _, txIn := range checkpoint.UnsignedTx.TxIn {
			outpoint := txIn.PreviousOutPoint
			if _, ok := seen[outpoint]; ok {
				continue
			}
			seen[outpoint] = struct{}{}
			outpoints = append(outpoints, outpoint)
		}
	}

	return outpoints
}

// recoverySigningKey returns the key that signs the selected vHTLC unilateral
// leaf. It mirrors the unroll policy resolver so the descriptor key locator and
// final spend signer agree.
func recoverySigningKey(job vhtlcrecovery.RecoveryJob) (*btcec.PublicKey,
	error) {

	var raw []byte
	switch job.Action {
	case vhtlcrecovery.ActionClaim:
		raw = job.ReceiverPubkey

	case vhtlcrecovery.ActionRefundWithoutReceiver:
		raw = job.SenderPubkey

	default:
		return nil, fmt.Errorf("unknown vhtlc recovery action %q",
			job.Action)
	}

	key, err := btcec.ParsePubKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse recovery signing key: %w", err)
	}

	return key, nil
}

// recoveryOperatorKey derives the Ark operator key for the synthesized recovery
// descriptor from the local root VTXOs. The target vHTLC was created by an OOR
// package spending those roots, so all roots must agree on the operator key; a
// mismatch would mean the local ancestry cannot safely describe one coherent
// Ark lineage.
func recoveryOperatorKey(roots []*vtxo.Descriptor) (*btcec.PublicKey, error) {
	var operatorKey *btcec.PublicKey
	for _, root := range roots {
		if root == nil || root.OperatorKey == nil {
			continue
		}

		if operatorKey == nil {
			operatorKey = root.OperatorKey
			continue
		}

		if !operatorKey.IsEqual(root.OperatorKey) {
			return nil, fmt.Errorf("recovery roots use multiple " +
				"operator keys")
		}
	}

	if operatorKey == nil {
		return nil, fmt.Errorf("recovery roots have no operator key")
	}

	return operatorKey, nil
}

// recoveryCSVDelay returns the vHTLC leaf CSV that unroll must wait after the
// target is materialized. The final spend policy still derives the on-wire
// sequence from the script path; this value feeds unroll's waiting scheduler.
func recoveryCSVDelay(job vhtlcrecovery.RecoveryJob) (uint32, error) {
	var delay int32
	switch job.Action {
	case vhtlcrecovery.ActionClaim:
		delay = job.UnilateralClaimDelay

	case vhtlcrecovery.ActionRefundWithoutReceiver:
		delay = job.UnilateralRefundWithoutReceiverDelay

	default:
		return 0, fmt.Errorf("unknown vhtlc recovery action %q",
			job.Action)
	}

	if delay <= 0 {
		return 0, fmt.Errorf("recovery csv delay must be positive")
	}

	return uint32(delay), nil
}

// recoveryPolicyTemplate rebuilds the semantic vHTLC policy from the durable
// recovery tuple and verifies it still commits to the target output script.
func recoveryPolicyTemplate(job vhtlcrecovery.RecoveryJob,
	targetPkScript []byte) ([]byte, error) {

	sender, err := recoveryPubKey(job.SenderPubkey, "sender")
	if err != nil {
		return nil, err
	}
	receiver, err := recoveryPubKey(job.ReceiverPubkey, "receiver")
	if err != nil {
		return nil, err
	}
	server, err := recoveryPubKey(job.ServerPubkey, "server")
	if err != nil {
		return nil, err
	}

	var preimageHash lntypes.Hash
	if len(job.PreimageHash) != len(preimageHash) {
		return nil, fmt.Errorf("recovery preimage hash must be %d "+
			"bytes, got %d", len(preimageHash),
			len(job.PreimageHash))
	}
	copy(preimageHash[:], job.PreimageHash)

	refundLocktime, err := recoveryUint32(
		job.RefundLocktime, "recovery refund locktime",
	)
	if err != nil {
		return nil, err
	}
	claimDelay, err := recoveryUint32(
		job.UnilateralClaimDelay, "recovery claim delay",
	)
	if err != nil {
		return nil, err
	}
	refundDelay, err := recoveryUint32(
		job.UnilateralRefundDelay, "recovery refund delay",
	)
	if err != nil {
		return nil, err
	}
	refundWithoutReceiverDelay, err := recoveryUint32(
		job.UnilateralRefundWithoutReceiverDelay,
		"recovery refund-without-receiver delay",
	)
	if err != nil {
		return nil, err
	}

	refundWithoutReceiver := refundWithoutReceiverDelay
	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:                               sender,
		Receiver:                             receiver,
		Server:                               server,
		PreimageHash:                         preimageHash,
		RefundLocktime:                       refundLocktime,
		UnilateralClaimDelay:                 claimDelay,
		UnilateralRefundDelay:                refundDelay,
		UnilateralRefundWithoutReceiverDelay: refundWithoutReceiver,
	})
	if err != nil {
		return nil, fmt.Errorf("build recovery vhtlc policy: %w", err)
	}
	if policy.Template == nil {
		return nil, fmt.Errorf("recovery vhtlc policy template is nil")
	}
	if !policy.Template.MatchesPkScript(targetPkScript) {
		return nil, fmt.Errorf("recovery vhtlc policy does not match " +
			"target output script")
	}

	policyTemplate, err := policy.Template.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode recovery vhtlc policy: %w", err)
	}

	return policyTemplate, nil
}

func recoveryPubKey(raw []byte, label string) (*btcec.PublicKey, error) {
	key, err := btcec.ParsePubKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse recovery %s key: %w", label, err)
	}

	return key, nil
}

func recoveryUint32(value int32, label string) (uint32, error) {
	if value <= 0 {
		return 0, fmt.Errorf("%s must be positive", label)
	}

	return uint32(value), nil
}

// recoveryAncestry clones and de-duplicates ancestry fragments from the root
// descriptors. Distinct roots may share a commitment fragment, so
// de-duplication keeps the synthesized target descriptor compact and
// deterministic.
func recoveryAncestry(roots []*vtxo.Descriptor) []vtxo.Ancestry {
	seen := make(map[chainhash.Hash]struct{})
	var ancestry []vtxo.Ancestry
	for _, root := range roots {
		if root == nil {
			continue
		}

		for _, fragment := range root.Ancestry {
			if _, ok := seen[fragment.CommitmentTxID]; ok {
				continue
			}
			seen[fragment.CommitmentTxID] = struct{}{}

			ancestry = append(ancestry, fragment)
		}
	}

	return ancestry
}

// recoveryRootMeta derives scalar descriptor metadata from the earliest local
// roots. For expiry-style fields the most conservative non-zero value wins so a
// multi-input recovery target cannot outlive one of its contributing roots.
func recoveryRootMeta(roots []*vtxo.Descriptor) (string, chainhash.Hash, int32,
	int32) {

	var (
		roundID        string
		commitmentTxID chainhash.Hash
		batchExpiry    int32
		createdHeight  int32
	)

	for _, root := range roots {
		if root == nil {
			continue
		}

		if roundID == "" {
			roundID = root.RoundID
		}
		if commitmentTxID == (chainhash.Hash{}) {
			commitmentTxID = root.CommitmentTxID
		}
		if root.BatchExpiry > 0 &&
			(batchExpiry == 0 || root.BatchExpiry < batchExpiry) {

			batchExpiry = root.BatchExpiry
		}
		if root.CreatedHeight > 0 &&
			(createdHeight == 0 ||
				root.CreatedHeight < createdHeight) {

			createdHeight = root.CreatedHeight
		}
	}

	return roundID, commitmentTxID, batchExpiry, createdHeight
}

// recoveryChainDepth computes a conservative OOR hop depth for the synthesized
// target descriptor from its roots and the resolved package chain.
func recoveryChainDepth(roots []*vtxo.Descriptor, packageCount int) int {
	var maxRootDepth int
	for _, root := range roots {
		if root != nil && root.ChainDepth > maxRootDepth {
			maxRootDepth = root.ChainDepth
		}
	}

	if packageCount <= 0 {
		packageCount = 1
	}

	return maxRootDepth + packageCount
}
