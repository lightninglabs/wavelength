package waved

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/internal/indexerlimits"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/vhtlcrecovery"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
)

const recoveryTargetFetchTimeout = 30 * time.Second

// fetchIndexedRecoveryTarget authenticates as the local recovery signer for
// this exact policy. A receiver need not register the sender's funding script
// or have authored its OOR session to obtain its signed recovery material.
func (s *Server) fetchIndexedRecoveryTarget(ctx context.Context,
	job vhtlcrecovery.RecoveryJob) (*arkrpc.VTXO, error) {

	ctx, cancel := context.WithTimeout(ctx, recoveryTargetFetchTimeout)
	defer cancel()

	if s.indexer == nil {
		return nil, fmt.Errorf("recovery indexer not initialized")
	}
	policy, err := recoveryPolicy(job)
	if err != nil {
		return nil, err
	}
	script, err := policy.PkScript()
	if err != nil {
		return nil, err
	}
	template, err := policy.Template.Encode()
	if err != nil {
		return nil, err
	}
	key, err := recoverySigningKey(job)
	if err != nil {
		return nil, err
	}
	factory, err := s.indexerProofSignerFactory()
	if err != nil {
		return nil, err
	}
	idx := s.indexer.WithSigner(
		factory(
			keychain.KeyDescriptor{
				PubKey: key,
				KeyLocator: keychain.KeyLocator{
					Family: keychain.KeyFamily(
						job.SignerKeyFamily,
					),
					Index: uint32(job.SignerKeyIndex),
				},
			},
		),
	)

	var cursor []byte
	var scanned uint64
	for {
		resp, err := idx.ListVTXOsByScriptsTaproot(
			ctx,
			[]indexer.TaprootScriptScope{{
				PkScript: script, PolicyTemplate: template,
			}},
			cursor, vtxo.DefaultIncomingAncestryIndexPageSize, nil,
		)
		if err != nil {
			return nil, fmt.Errorf("query recovery target: %w", err)
		}
		page := vtxo.FlattenListVTXOsByScriptsResponse(resp)
		for _, candidate := range page {
			scanned++
			if scanned > vtxo.DefaultIncomingAncestryMaxScan {
				return nil, fmt.Errorf("recovery index scan " +
					"limit")
			}
			op, ok := indexerVTXOOutpoint(candidate)
			if ok && op == job.VTXOOutpoint {
				return candidate, nil
			}
		}
		next := resp.GetNextCursor()
		if len(page) == 0 || len(next) == 0 ||
			bytes.Equal(next, cursor) {
			return nil, fmt.Errorf("indexed recovery target "+
				"%s missing", job.VTXOOutpoint)
		}
		if scanned >= vtxo.DefaultIncomingAncestryMaxScan {
			return nil, fmt.Errorf("recovery index scan limit")
		}
		if err := indexerlimits.ValidateVTXOsByScriptsCursor(
			next,
		); err != nil {
			return nil, fmt.Errorf("recovery index cursor: %w", err)
		}
		cursor = bytes.Clone(next)
	}
}

// buildIndexedRecoveryDescriptor verifies the complete signed path to the
// exact funded output and authenticates its expiry against the local chain
// before persisting any foreign artifacts. Only the recovery target becomes a
// descriptor; foreign inputs never enter wallet coin selection.
func (m *vhtlcRecoveryTargetMaterializer) buildIndexedRecoveryDescriptor(
	ctx context.Context, job vhtlcrecovery.RecoveryJob) (*vtxo.Descriptor,
	error) {

	ctx, cancel := context.WithTimeout(ctx, recoveryTargetFetchTimeout)
	defer cancel()

	if m.authenticateExpiry == nil {
		return nil, fmt.Errorf("recovery expiry authenticator missing")
	}
	candidate, err := m.fetchTarget(ctx, job)
	if err != nil {
		return nil, err
	}
	op, ok := indexerVTXOOutpoint(candidate)
	if !ok || op != job.VTXOOutpoint {
		return nil, fmt.Errorf("indexed recovery outpoint mismatch")
	}
	if _, err := recoveryPolicyTemplate(
		job, candidate.GetPkScript(),
	); err != nil {
		return nil, err
	}
	version := arkrpc.ConstructionVersion(
		candidate.GetConstructionVersion(),
	)
	if err := arkrpc.ValidateConstructionVersion(version); err != nil {
		return nil, err
	}
	meta, err := customRefreshMetadataFromRPC(candidate)
	if err != nil {
		return nil, err
	}
	evidence, err := m.authenticateExpiry(ctx, meta.Ancestry)
	if err != nil {
		return nil, fmt.Errorf("authenticate recovery expiry: %w", err)
	}
	meta.BatchExpiry = evidence.BatchExpiry
	meta.CreatedHeight = evidence.EarliestCommitmentHeight

	operatorKey, err := recoveryPubKey(job.ServerPubkey, "server")
	if err != nil {
		return nil, err
	}
	desc, err := recoveryDescriptor(job, &vtxo.Descriptor{
		Outpoint:            op,
		Amount:              btcutil.Amount(candidate.GetValueSat()),
		PkScript:            bytes.Clone(candidate.GetPkScript()),
		OperatorKey:         operatorKey,
		Ancestry:            meta.Ancestry,
		RoundID:             meta.RoundID,
		CommitmentTxID:      meta.CommitmentTxID,
		BatchExpiry:         meta.BatchExpiry,
		ChainDepth:          meta.ChainDepth,
		CreatedHeight:       meta.CreatedHeight,
		ConstructionVersion: version,
	})
	if err != nil {
		return nil, err
	}

	// Decode the entire validated graph before committing any package.
	// The indexer proof must include the creating package, not merely
	// supply unrelated ancestry plus unverified standalone OOR fields.
	packages := make(
		[]*db.OORPackageBundle, 0,
		len(
			candidate.GetAncestryPackages(),
		),
	)
	foundTarget := false
	for _, pkg := range candidate.GetAncestryPackages() {
		ark, err := psbtutil.Parse(pkg.GetArkPsbt())
		if err != nil {
			return nil, fmt.Errorf("decode recovery ark: %w", err)
		}
		checkpoints := make(
			[]*psbt.Packet, 0,
			len(
				pkg.GetCheckpointPsbts(),
			),
		)
		for _, raw := range pkg.GetCheckpointPsbts() {
			checkpoint, err := psbtutil.Parse(raw)
			if err != nil {
				return nil, fmt.Errorf("decode recovery "+
					"checkpoint: %w", err)
			}
			checkpoints = append(checkpoints, checkpoint)
		}
		id := ark.UnsignedTx.TxHash()
		foundTarget = foundTarget || id == op.Hash
		packages = append(packages, &db.OORPackageBundle{
			SessionID: id, ArkPSBT: ark,
			FinalCheckpointPSBTs: checkpoints,
		})
	}
	if !foundTarget {
		return nil, fmt.Errorf("indexed recovery creating package " +
			"missing")
	}
	for _, pkg := range packages {
		// An ancestor may already be an outgoing local package. Reuse
		// it through its existing direction; immutable upsert still
		// checks the exact signed payload before accepting it.
		direction := db.OORPackageDirectionIncoming
		existing, err := m.packages.GetPackage(ctx, pkg.SessionID)
		if err == nil {
			direction = existing.Direction
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err := m.packages.UpsertPackage(
			ctx, direction, pkg.SessionID, pkg.ArkPSBT,
			pkg.FinalCheckpointPSBTs,
		); err != nil {
			return nil, fmt.Errorf("persist recovery package: %w",
				err)
		}
	}

	return desc, nil
}

// recoveryDescriptor retains trusted funding metadata while deriving policy,
// signing key, and CSV from the current recovery obligation on every retry.
func recoveryDescriptor(job vhtlcrecovery.RecoveryJob,
	meta *vtxo.Descriptor) (*vtxo.Descriptor, error) {

	if meta.Outpoint != job.VTXOOutpoint || meta.Amount <= 0 ||
		(job.VTXOAmountSat > 0 &&
			int64(meta.Amount) != job.VTXOAmountSat) {
		return nil, fmt.Errorf("recovery target amount or outpoint " +
			"mismatch")
	}
	if len(meta.Ancestry) == 0 || meta.RoundID == "" ||
		meta.CommitmentTxID == (chainhash.Hash{}) ||
		meta.BatchExpiry <= 0 || meta.CreatedHeight <= 0 ||
		meta.ChainDepth < 0 {
		return nil, fmt.Errorf("recovery target metadata incomplete")
	}
	operator, err := recoveryPubKey(job.ServerPubkey, "server")
	if err != nil {
		return nil, err
	}
	// Taproot commits to x-only keys. Stored round metadata may normalize
	// the key's parity while the recovery tuple retains its original key.
	if meta.OperatorKey == nil || !bytes.Equal(
		schnorr.SerializePubKey(operator),
		schnorr.SerializePubKey(meta.OperatorKey),
	) {
		return nil, fmt.Errorf("recovery operator key mismatch")
	}
	template, err := recoveryPolicyTemplate(job, meta.PkScript)
	if err != nil {
		return nil, err
	}
	key, err := recoverySigningKey(job)
	if err != nil {
		return nil, err
	}
	delay, err := recoveryCSVDelay(job)
	if err != nil {
		return nil, err
	}
	desc := *meta
	desc.PolicyTemplate = template
	desc.RelativeExpiry = delay
	desc.Status = vtxo.VTXOStatusUnilateralExit
	desc.ClientKey = keychain.KeyDescriptor{
		PubKey: key,
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(job.SignerKeyFamily),
			Index:  uint32(job.SignerKeyIndex),
		},
	}

	return &desc, nil
}
