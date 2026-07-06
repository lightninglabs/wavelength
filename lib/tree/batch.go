package tree

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"sort"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
)

// VTXODescriptor defines the tree-construction inputs for a single VTXO leaf.
// It carries the compiled output script and the owner-side collaborative
// signer needed by the generic tree builder, while keeping local wallet
// ownership metadata out of the tree layer.
type VTXODescriptor struct {
	// PolicyTemplate is the semantic arkscript policy that compiles to
	// PkScript. This is the authoritative policy representation for
	// persistence and higher-layer validation.
	PolicyTemplate []byte

	// PkScript is the P2TR script for the VTXO output. This is typically
	// generated using arkscript.VTXOTapKey which creates a taproot script
	// with both keyspend (collaborative) and scriptspend (timeout) paths.
	PkScript []byte

	// Amount is the value of this VTXO in satoshis.
	Amount btcutil.Amount

	// CoSignerKey is the owner-side public key that participates in the
	// collaborative spend path for this leaf. This is a tree-construction
	// signer role, not local wallet ownership metadata.
	CoSignerKey *btcec.PublicKey
}

// NewVTXODescriptor constructs a VTXODescriptor by building the VTXO taproot
// script using arkscript. This helper builds the standard Ark VTXO shape,
// encoding the owner/operator policy while preserving only the owner-side
// collaborative signer that the tree builder needs.
func NewVTXODescriptor(amount btcutil.Amount, ownerKey *btcec.PublicKey,
	operatorKey *btcec.PublicKey,
	exitDelay uint32) (*VTXODescriptor, error) {

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		ownerKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to encode policy template: %w",
			err)
	}

	// Use arkscript to compute the VTXO output key.
	outputKey, err := arkscript.VTXOTapKey(ownerKey, operatorKey, exitDelay)
	if err != nil {
		return nil, fmt.Errorf("failed to compute VTXO tap key: %w",
			err)
	}

	// Create the P2TR script.
	pkScript, err := txscript.PayToTaprootScript(outputKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create taproot script: %w",
			err)
	}

	return &VTXODescriptor{
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
		Amount:         amount,
		CoSignerKey:    ownerKey,
	}, nil
}

// ToLeafDescriptor converts a VTXODescriptor to a generic LeafDescriptor.
func (v VTXODescriptor) ToLeafDescriptor() LeafDescriptor {
	pkScript := make([]byte, len(v.PkScript))
	copy(pkScript, v.PkScript)

	cosignerKey := *v.CoSignerKey

	return LeafDescriptor{
		PkScript:    pkScript,
		Amount:      v.Amount,
		CoSignerKey: &cosignerKey,
	}
}

// ConnectorDescriptor defines the specification for a connector tree.
// Connector trees are used for forfeit transactions and have identical leaves,
// all signed by the operator only.
type ConnectorDescriptor struct {
	// PkScript is the P2TR script for each connector leaf output. All
	// connector leaves in the tree will use this same script.
	PkScript []byte

	// NumLeaves is the number of identical connector leaves to create in
	// the tree. Each leaf will have the same PkScript and Amount.
	NumLeaves int

	// Amount is the value (in satoshis) for each individual connector
	// leaf. The total output amount will be NumLeaves * Amount.
	Amount btcutil.Amount
}

// ValidateVTXODescriptors validates a slice of VTXO descriptors ensuring they
// meet all requirements for tree construction.
func ValidateVTXODescriptors(vtxos []VTXODescriptor) error {
	if len(vtxos) == 0 {
		return fmt.Errorf("no VTXO descriptors provided")
	}

	seen := make(map[string]struct{})

	for i, vtxo := range vtxos {
		if vtxo.Amount <= 0 {
			return fmt.Errorf("VTXO %d has invalid amount: %d", i,
				vtxo.Amount)
		}
		if len(vtxo.PkScript) == 0 {
			return fmt.Errorf("VTXO %d has empty PkScript", i)
		}
		if !txscript.IsPayToTaproot(vtxo.PkScript) {
			return fmt.Errorf("VTXO %d has invalid taproot script",
				i)
		}
		if vtxo.CoSignerKey == nil {
			return fmt.Errorf("VTXO %d has nil co-signer key", i)
		}

		keyStr := hex.EncodeToString(
			schnorr.SerializePubKey(vtxo.CoSignerKey),
		)
		if _, exists := seen[keyStr]; exists {
			return fmt.Errorf("VTXO %d has duplicate co-signer key",
				i)
		}
		seen[keyStr] = struct{}{}
	}

	return nil
}

// ValidateConnectorDescriptor validates a connector descriptor ensuring it is
// suitable for connector tree construction.
func ValidateConnectorDescriptor(conn ConnectorDescriptor) error {
	if conn.NumLeaves <= 0 {
		return fmt.Errorf("connector descriptor has invalid "+
			"NumLeaves: %d", conn.NumLeaves)
	}
	if conn.Amount <= 0 {
		return fmt.Errorf("connector descriptor has invalid amount: %d",
			conn.Amount)
	}
	if len(conn.PkScript) == 0 {
		return fmt.Errorf("connector descriptor has empty PkScript")
	}
	if !txscript.IsPayToTaproot(conn.PkScript) {
		return fmt.Errorf("connector descriptor has invalid taproot " +
			"script")
	}

	return nil
}

// ToLeafDescriptors expands a connector descriptor into identical leaves using
// the provided operator key as cosigner.
func (c ConnectorDescriptor) ToLeafDescriptors(
	operatorKey *btcec.PublicKey,
) []LeafDescriptor {

	leaves := make([]LeafDescriptor, c.NumLeaves)
	for i := 0; i < c.NumLeaves; i++ {
		leaves[i] = LeafDescriptor{
			PkScript:    c.PkScript,
			Amount:      c.Amount,
			CoSignerKey: operatorKey,
		}
	}

	return leaves
}

// BuildVTXOTree constructs a complete transaction tree from VTXO descriptors
// using a two-phase approach (structure building + materialization). It returns
// a Tree with all transactions fully constructed and linked.
func BuildVTXOTree(batchOutpoint wire.OutPoint, batchOutput *wire.TxOut,
	vtxos []VTXODescriptor, operatorCoSignKey *btcec.PublicKey,
	sweepKey *btcec.PublicKey, sweepDelay uint32,
	radix int) (*Tree, error) {

	// Validate inputs.
	if err := ValidateVTXODescriptors(vtxos); err != nil {
		return nil, fmt.Errorf("invalid VTXO descriptors: %w", err)
	}

	if radix < 2 {
		return nil, fmt.Errorf("radix must be at least 2")
	}

	if operatorCoSignKey == nil {
		return nil, fmt.Errorf("operator co-sign key cannot be nil")
	}

	if sweepKey == nil {
		return nil, fmt.Errorf("sweep key cannot be nil")
	}

	// Convert to generic leaf descriptors.
	leaves := make([]LeafDescriptor, len(vtxos))
	for i, v := range vtxos {
		leaves[i] = v.ToLeafDescriptor()
	}

	// Sort leaves by amount (descending) using LPT (Longest Processing
	// Time) heuristic for better balance. LPT is a bin packing algorithm
	// that processes larger items first, which tends to produce more
	// balanced partitions. We use a stable sort with PkScript as
	// tiebreaker to ensure deterministic tree construction.
	sort.SliceStable(leaves, func(i, j int) bool {
		if leaves[i].Amount != leaves[j].Amount {
			return leaves[i].Amount > leaves[j].Amount
		}

		// Tiebreaker: sort by PkScript for determinism.
		return bytes.Compare(leaves[i].PkScript, leaves[j].PkScript) < 0
	})

	// Compute the sweep tap leaf for branch tweaking.
	sweepTapLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		sweepKey, sweepDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create sweep tap leaf: %w",
			err)
	}
	sweepTapRoot := sweepTapLeaf.TapHash()

	// Use BTCTreeAssembler for two-phase tree construction.
	assembler := NewTreeAssembler(TreeConfig{
		OperatorKey:        operatorCoSignKey,
		SweepTapscriptRoot: sweepTapRoot[:],
		Radix:              radix,
	})

	return assembler.BuildTree(batchOutpoint, batchOutput, leaves)
}

// BuildConnectorTree constructs a connector tree from a connector descriptor
// using a two-phase approach (structure building + materialization). Unlike
// VTXO trees, connector trees have identical leaves (same script, same amount)
// and are signed only by the operator. This is used for forfeit transaction
// inputs.
func BuildConnectorTree(batchOutpoint wire.OutPoint, batchOutput *wire.TxOut,
	connector ConnectorDescriptor, operatorKey *btcec.PublicKey,
	radix int) (*Tree, error) {

	// Validate inputs.
	if err := ValidateConnectorDescriptor(connector); err != nil {
		return nil, fmt.Errorf("invalid connector descriptor: %w", err)
	}

	if radix < 2 {
		return nil, fmt.Errorf("radix must be at least 2")
	}

	if operatorKey == nil {
		return nil, fmt.Errorf("operator key cannot be nil")
	}

	// Convert to leaf descriptors (creates unique keys for each).
	leaves := connector.ToLeafDescriptors(operatorKey)

	// Use the Bitcoin only TreeAssembler for two-phase tree construction.
	// Connector trees have no sweep script (nil SweepTapscriptRoot).
	assembler := NewTreeAssembler(TreeConfig{
		OperatorKey:        operatorKey,
		SweepTapscriptRoot: nil,
		Radix:              radix,
	})

	return assembler.BuildTree(batchOutpoint, batchOutput, leaves)
}

// BuildBatchOutput computes the pkscript and output amount for a batch output
// in the commitment transaction. This output will be spent by the root of the
// VTXO tree.
//
// The output has two spend paths:
//  1. Collaborative keyspend path: MuSig2 aggregation of operator + all client
//     cosigners, tweaked with the sweep tapscript root.
//  2. CSV timeout script path: Allows operator to sweep after batch expiry.
//
// Since all transactions in the VTXT include ephemeral anchor outputs, the
// total amount is simply the sum of all VTXO amounts.
func BuildBatchOutput(vtxos []VTXODescriptor, operatorMuSigKey *btcec.PublicKey,
	sweepKey *btcec.PublicKey, sweepDelay uint32) (*wire.TxOut, error) {

	if len(vtxos) == 0 {
		return nil, fmt.Errorf("batch output requires at least one " +
			"VTXO")
	}

	if operatorMuSigKey == nil {
		return nil, fmt.Errorf("operator musig key cannot be nil")
	}

	if sweepKey == nil {
		return nil, fmt.Errorf("sweep key cannot be nil")
	}

	// Compute the sweep tap leaf for tweaking.
	sweepTapLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		sweepKey, sweepDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create sweep tap leaf: %w",
			err)
	}
	sweepTapRoot := sweepTapLeaf.TapHash()

	// Collect unique cosigners (operator + all client cosigners).
	signers := []*btcec.PublicKey{operatorMuSigKey}
	seenSigners := make(map[string]struct{})
	operatorKeyStr := xOnlyKeyString(operatorMuSigKey)
	seenSigners[operatorKeyStr] = struct{}{}

	var totalAmount btcutil.Amount
	for i, vtxo := range vtxos {
		if vtxo.Amount < 0 {
			return nil, fmt.Errorf("vtxo amount cannot be negative")
		}
		if vtxo.CoSignerKey == nil {
			return nil, fmt.Errorf("VTXO %d has nil co-signer key",
				i)
		}

		// Check overflow before adding.
		if totalAmount > math.MaxInt64-vtxo.Amount {
			return nil, fmt.Errorf("total amount exceeds int64 " +
				"range")
		}

		totalAmount += vtxo.Amount

		// Add cosigner if not already seen.
		keyStr := xOnlyKeyString(vtxo.CoSignerKey)
		if keyStr == operatorKeyStr {
			return nil, fmt.Errorf("VTXO %d co-signer key matches "+
				"operator key", i)
		}

		if _, seen := seenSigners[keyStr]; !seen {
			seenSigners[keyStr] = struct{}{}
			signers = append(signers, vtxo.CoSignerKey)
		}
	}

	// Aggregate cosigner keys and tweak with sweep tapscript root.
	aggKey, _, _, err := musig2.AggregateKeys(
		signers, true, musig2.WithTaprootKeyTweak(sweepTapRoot[:]),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to aggregate keys: %w", err)
	}

	// Create the P2TR script.
	pkScript, err := txscript.PayToTaprootScript(aggKey.FinalKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create taproot script: %w",
			err)
	}

	return &wire.TxOut{
		Value:    int64(totalAmount),
		PkScript: pkScript,
	}, nil
}

// xOnlyKeyString serializes a public key into the stable x-only key form used
// for tree signer de-duplication.
func xOnlyKeyString(key *btcec.PublicKey) string {
	return hex.EncodeToString(schnorr.SerializePubKey(key))
}

// BuildConnectorOutput computes the pkscript and output amount for a connector
// output in the commitment transaction. This output will be spent by the root
// of the connector tree.
//
// The total amount is numConnectors * dustAmount since each connector leaf
// receives the dust amount.
func BuildConnectorOutput(numConnectors int, dustAmount btcutil.Amount,
	connectorAddr btcaddr.Address) (*wire.TxOut, error) {

	if numConnectors == 0 {
		return nil, fmt.Errorf("num connectors must be > 0")
	}

	if dustAmount <= 0 {
		return nil, fmt.Errorf("dust amount must be > 0")
	}

	if connectorAddr == nil {
		return nil, fmt.Errorf("connector address cannot be nil")
	}

	// The total amount is the number of connectors times the dust amount.
	totalAmount := dustAmount * btcutil.Amount(numConnectors)

	pkScript, err := txscript.PayToAddrScript(connectorAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to create connector script: %w",
			err)
	}

	return &wire.TxOut{
		Value:    int64(totalAmount),
		PkScript: pkScript,
	}, nil
}

// NewBranchSweepSpendInfo derives the spend information for the operator's
// sweep path that is committed to in branch transactions. The internalKey
// parameter must be the pre-tweaked MuSig2 aggregate key for the branch output.
func NewBranchSweepSpendInfo(internalKey, sweepKey *btcec.PublicKey,
	csvDelay uint32) (*arkscript.SpendInfo, error) {

	if internalKey == nil {
		return nil, fmt.Errorf("internal key cannot be nil")
	}

	if sweepKey == nil {
		return nil, fmt.Errorf("sweep key cannot be nil")
	}

	sweepLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		sweepKey, csvDelay,
	)
	if err != nil {
		return nil, err
	}

	tapTree := txscript.AssembleTaprootScriptTree(sweepLeaf)
	if len(tapTree.LeafMerkleProofs) == 0 {
		return nil, fmt.Errorf("missing taproot proof for sweep leaf")
	}

	controlBlock := tapTree.LeafMerkleProofs[0].ToControlBlock(
		internalKey,
	)
	ctrlBytes, err := controlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	return &arkscript.SpendInfo{
		WitnessScript: sweepLeaf.Script,
		ControlBlock:  ctrlBytes,
	}, nil
}
