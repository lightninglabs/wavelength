package waved

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/lightninglabs/wavelength/vtxo"
)

// VTXOAnchor contains public Bitcoin output coordinates from locally stored
// ancestry. ConfirmationHeight is the recorded height, not a fresh chain proof.
// Nothing here asserts the output is currently unspent.
type VTXOAnchor struct {
	Outpoint           string
	AmountSat          int64
	PkScript           string
	ConfirmationHeight int32
}

// ReadVTXOAnchors reads the complete ancestry of one local VTXO without
// exporting signing material or starting recovery. The embedded SDK retains
// the daemon through this call; no tree or store object escapes to the host.
func (s *Server) ReadVTXOAnchors(ctx context.Context, outpoint string) (
	[]VTXOAnchor, error) {

	op, err := parseOutpointString(outpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid VTXO outpoint: %w", err)
	}
	if s.vtxoStore == nil {
		return nil, fmt.Errorf("VTXO store not ready")
	}
	desc, err := s.vtxoStore.GetVTXO(ctx, op)
	if err != nil {
		return nil, fmt.Errorf("read VTXO ancestry: %w", err)
	}

	return diagnosticAnchors(desc), nil
}

// diagnosticAnchors deduplicates batch outputs shared by several tree paths.
// It preserves the actual vout and never substitutes the virtual leaf index.
func diagnosticAnchors(desc *vtxo.Descriptor) []VTXOAnchor {
	anchors := make(map[string]VTXOAnchor)
	for _, ancestry := range desc.Ancestry {
		path := ancestry.TreePath
		if path == nil || path.BatchOutput == nil {
			continue
		}
		point := path.BatchOutpoint.String()
		previous, exists := anchors[point]
		if exists {
			if previous.ConfirmationHeight == 0 {
				previous.ConfirmationHeight =
					ancestry.CommitmentHeight
				anchors[point] = previous
			}

			continue
		}
		anchors[point] = VTXOAnchor{
			Outpoint:  point,
			AmountSat: path.BatchOutput.Value,
			PkScript: hex.EncodeToString(
				path.BatchOutput.PkScript,
			),
			ConfirmationHeight: ancestry.CommitmentHeight,
		}
	}
	result := make([]VTXOAnchor, 0, len(anchors))
	for _, anchor := range anchors {
		result = append(result, anchor)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Outpoint < result[j].Outpoint
	})

	return result
}
