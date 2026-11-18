package db

import (
	"context"
	"database/sql"
	"errors"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
)

// OORDiagnosticPackage identifies a package with no evidence of local wallet
// activity. RecoveryAncestor is true only if a chain of checkpoint spends
// connects this package to a wallet package on the selected page.
type OORDiagnosticPackage struct {
	RecoveryAncestor bool
}

// DiagnosticPackages classifies a bounded page of OOR status IDs in one local
// read snapshot. A package with absent or unreadable graph evidence stays
// unclassified as ancestry. The ordinary status RPC never loads PSBTs.
func (s *OORStatusStore) DiagnosticPackages(ctx context.Context,
	ids []chainhash.Hash) (map[chainhash.Hash]OORDiagnosticPackage, error) {

	var result map[chainhash.Hash]OORDiagnosticPackage
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		result = make(map[chainhash.Hash]OORDiagnosticPackage)
		local := make(map[chainhash.Hash]bool, len(ids))
		for _, id := range ids {
			row, err := q.GetOORDiagnosticPackage(ctx, id[:])
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if !row.WalletEvidence.Valid {
				continue
			}
			if row.WalletEvidence.Bool || row.Direction ==
				oorPackageDirectionOutgoingCode {

				local[id] = true
				continue
			}
			if row.Direction == oorPackageDirectionIncomingCode {
				local[id] = false
				result[id] = OORDiagnosticPackage{}
			}
		}
		if len(result) == 0 || len(result) == len(local) {
			return nil
		}

		packages := make(
			map[chainhash.Hash]*OORPackageBundle, len(local),
		)
		for id := range local {
			row, err := q.GetOORPackage(ctx, id[:])
			if err != nil {
				return err
			}
			pkg, err := materializePackageBundle(ctx, q, row)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}

				// One unreadable artifact cannot prove an edge.
				continue
			}
			if pkg.ArkPSBT.UnsignedTx.TxHash() == id {
				packages[id] = pkg
			}
		}
		markOORDiagnosticAncestors(packages, local, result)

		return nil
	})

	return result, err
}

// markOORDiagnosticAncestors follows only checkpoints spent by a known package.
// It never classifies any package for which the wallet has local evidence.
func markOORDiagnosticAncestors(packages map[chainhash.Hash]*OORPackageBundle,
	local map[chainhash.Hash]bool,
	result map[chainhash.Hash]OORDiagnosticPackage) {

	var pending []chainhash.Hash
	visited := make(map[chainhash.Hash]bool, len(packages))
	for id := range packages {
		if local[id] {
			pending = append(pending, id)
			visited[id] = true
		}
	}

	for len(pending) > 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for _, input := range diagnosticCheckpointInputs(packages[id]) {
			parent := input.Hash
			if visited[parent] || !packageProducesAncestorOutput(
				packages[parent], parent, input.Index,
			) {

				continue
			}

			visited[parent] = true
			pending = append(pending, parent)
			result[parent] = OORDiagnosticPackage{
				RecoveryAncestor: true,
			}
		}
	}
}

// diagnosticCheckpointInputs returns the previous outputs of checkpoints whose
// transaction IDs are actually spent by the package's Ark transaction.
func diagnosticCheckpointInputs(pkg *OORPackageBundle) []wire.OutPoint {
	spends := make(map[wire.OutPoint]struct{})
	for _, input := range pkg.ArkPSBT.UnsignedTx.TxIn {
		spends[input.PreviousOutPoint] = struct{}{}
	}

	var outpoints []wire.OutPoint
	for _, checkpoint := range pkg.FinalCheckpointPSBTs {
		tx := checkpoint.UnsignedTx
		// The checkpoint VTXO is always output zero. Its anchor cannot
		// supply an Ark input.
		if len(tx.TxOut) == 0 || arktx.IsAnchorOutput(tx.TxOut[0]) {
			continue
		}
		if _, ok := spends[wire.OutPoint{
			Hash:  tx.TxHash(),
			Index: 0,
		}]; !ok {

			continue
		}
		for _, input := range tx.TxIn {
			outpoints = append(outpoints, input.PreviousOutPoint)
		}
	}

	return outpoints
}
