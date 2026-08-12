package arkchannel

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/chainhash/v2"
)

// RecoveryPackage is the complete endpoint-neutral data needed to watch and
// unroll a channel-policy VTXO. It deliberately contains no private key or
// wallet ownership marker.
type RecoveryPackage struct {
	Descriptor RecoveryDescriptor
	Packages   []RecoveryOORPackage
}

// RecoveryDescriptor carries the round ancestry inherited by the
// channel-policy VTXO.
type RecoveryDescriptor struct {
	Ancestry            []RecoveryAncestry
	RoundID             string
	CommitmentTxID      chainhash.Hash
	BatchExpiry         int32
	ChainDepth          int
	CreatedHeight       int32
	ConstructionVersion int32
}

// RecoveryAncestry is one serialized commitment-tree path in the source
// descriptor.
type RecoveryAncestry struct {
	TreePath         []byte
	CommitmentTxID   chainhash.Hash
	InputIndices     []uint32
	TreeDepth        uint32
	CommitmentHeight int32
}

// RecoveryOORPackage is one finalized Ark/checkpoint package in farthest
// ancestor to target order.
type RecoveryOORPackage struct {
	SessionID   chainhash.Hash
	Direction   int32
	ArkPSBT     []byte
	Checkpoints [][]byte
}

// Clone returns an isolated recovery package.
func (p RecoveryPackage) Clone() RecoveryPackage {
	p.Descriptor.Ancestry = make(
		[]RecoveryAncestry, len(p.Descriptor.Ancestry),
	)
	for i := range p.Descriptor.Ancestry {
		entry := p.Descriptor.Ancestry[i]
		entry.TreePath = slices.Clone(entry.TreePath)
		entry.InputIndices = slices.Clone(entry.InputIndices)
		p.Descriptor.Ancestry[i] = entry
	}
	p.Packages = make([]RecoveryOORPackage, len(p.Packages))
	for i := range p.Packages {
		entry := p.Packages[i]
		entry.ArkPSBT = slices.Clone(entry.ArkPSBT)
		entry.Checkpoints = make([][]byte, len(entry.Checkpoints))
		for j := range entry.Checkpoints {
			entry.Checkpoints[j] = slices.Clone(
				entry.Checkpoints[j],
			)
		}
		p.Packages[i] = entry
	}

	return p
}

// Validate checks the package shape and binds its target package to source.
// Database and transaction codecs perform the deeper tree and PSBT checks.
func (p RecoveryPackage) Validate(source VTXOBinding) error {
	desc := p.Descriptor
	if desc.RoundID == "" || desc.CommitmentTxID == (chainhash.Hash{}) ||
		desc.BatchExpiry <= 0 || desc.CreatedHeight <= 0 ||
		desc.ChainDepth <= 0 {
		return fmt.Errorf("channel recovery descriptor is incomplete")
	}
	if len(desc.Ancestry) == 0 {
		return fmt.Errorf("channel recovery ancestry is empty")
	}
	for i := range desc.Ancestry {
		entry := desc.Ancestry[i]
		if len(entry.TreePath) == 0 ||
			entry.CommitmentTxID == (chainhash.Hash{}) {
			return fmt.Errorf("channel recovery ancestry %d is "+
				"incomplete", i)
		}
	}
	if len(p.Packages) == 0 {
		return fmt.Errorf("channel recovery OOR packages are empty")
	}
	targetFound := false
	seen := make(map[chainhash.Hash][]byte, len(p.Packages))
	for i := range p.Packages {
		entry := p.Packages[i]
		if entry.SessionID == (chainhash.Hash{}) ||
			len(entry.ArkPSBT) == 0 || len(entry.Checkpoints) == 0 {
			return fmt.Errorf("channel recovery OOR package %d is "+
				"incomplete", i)
		}
		if entry.Direction != 0 && entry.Direction != 1 {
			return fmt.Errorf("channel recovery OOR package %d "+
				"has invalid direction %d", i, entry.Direction)
		}
		for j := range entry.Checkpoints {
			if len(entry.Checkpoints[j]) == 0 {
				return fmt.Errorf("channel recovery "+
					"checkpoint %d/%d is empty", i, j)
			}
		}
		if prior, ok := seen[entry.SessionID]; ok {
			if !bytes.Equal(prior, entry.ArkPSBT) {
				return fmt.Errorf("channel recovery package "+
					"%s is duplicated with another payload",
					entry.SessionID)
			}

			continue
		}
		seen[entry.SessionID] = entry.ArkPSBT
		if entry.SessionID == source.OORSessionID {
			targetFound = true
		}
	}
	if !targetFound {
		return fmt.Errorf("channel recovery is missing target OOR " +
			"package")
	}

	return nil
}
