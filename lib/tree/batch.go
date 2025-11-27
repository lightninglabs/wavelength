package tree

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/closure"
	"github.com/lightninglabs/darepo-client/lib/scripts"
)

// VTXODescriptor defines the complete specification for a single VTXO leaf.
// It stores the underlying tapscript closures as hex-encoded strings, allowing
// the full closure information to be preserved and retrieved.
type VTXODescriptor struct {
	// Scripts contains the hex-encoded tapscript leaves for this VTXO.
	// These can be decoded into Closure objects using closure.ParseVtxoScript().
	// The scripts define the spending conditions (exit paths, collab paths,
	// etc.) for this VTXO.
	Scripts []string

	// Amount is the value of this VTXO in satoshis.
	Amount btcutil.Amount

	// CoSignerKey is the public key of the VTXO owner who must co-sign
	// along with the operator to spend this VTXO collaboratively.
	CoSignerKey *btcec.PublicKey
}

// PkScript derives the P2TR output script from the stored tapscripts.
func (v *VTXODescriptor) PkScript() ([]byte, error) {
	vtxoScript, err := closure.ParseVtxoScript(v.Scripts)
	if err != nil {
		return nil, fmt.Errorf("failed to parse vtxo scripts: %w", err)
	}

	key, _, err := vtxoScript.TapTree()
	if err != nil {
		return nil, fmt.Errorf("failed to compute tap tree: %w", err)
	}

	return txscript.PayToTaprootScript(key)
}

// VtxoScript parses the stored scripts into a TapscriptsVtxoScript.
func (v *VTXODescriptor) VtxoScript() (*closure.TapscriptsVtxoScript, error) {
	return closure.ParseVtxoScript(v.Scripts)
}

// NewVTXODescriptor creates a descriptor from a TapscriptsVtxoScript.
// The vtxoScript closures are encoded to hex strings for storage, allowing
// the full closure information to be preserved and retrieved later.
func NewVTXODescriptor(amount btcutil.Amount,
	vtxoScript *closure.TapscriptsVtxoScript,
	coSignerKey *btcec.PublicKey) (*VTXODescriptor, error) {

	if vtxoScript == nil {
		return nil, fmt.Errorf("vtxo script cannot be nil")
	}

	if coSignerKey == nil {
		return nil, fmt.Errorf("cosigner key cannot be nil")
	}

	// Encode closures to hex strings for storage.
	scripts, err := vtxoScript.Encode()
	if err != nil {
		return nil, fmt.Errorf("failed to encode vtxo scripts: %w", err)
	}

	return &VTXODescriptor{
		Scripts:     scripts,
		Amount:      amount,
		CoSignerKey: coSignerKey,
	}, nil
}

// NewDefaultVTXODescriptor creates a descriptor with the standard exit + collab
// closures. This is a convenience function for creating VTXOs with the default
// structure: owner can exit after CSV delay, or owner + cosigner can spend
// collaboratively.
func NewDefaultVTXODescriptor(amount btcutil.Amount, ownerKey,
	cosignerKey *btcec.PublicKey,
	exitDelay closure.RelativeLocktime) (*VTXODescriptor, error) {

	vtxoScript := closure.NewDefaultVtxoScript(
		ownerKey, cosignerKey, exitDelay,
	)

	return NewVTXODescriptor(amount, vtxoScript, ownerKey)
}

// ToLeafDescriptor converts a VTXODescriptor to a generic LeafDescriptor by
// deriving the PkScript from the stored tapscripts.
func (v *VTXODescriptor) ToLeafDescriptor() (LeafDescriptor, error) {
	pkScript, err := v.PkScript()
	if err != nil {
		return LeafDescriptor{}, err
	}

	return LeafDescriptor{
		PkScript:    pkScript,
		Amount:      v.Amount,
		CoSignerKey: v.CoSignerKey,
	}, nil
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

	for i := range vtxos {
		vtxo := &vtxos[i]

		if vtxo.Amount <= 0 {
			return fmt.Errorf("VTXO %d has invalid amount: %d", i,
				vtxo.Amount)
		}

		if len(vtxo.Scripts) == 0 {
			return fmt.Errorf("VTXO %d has empty Scripts", i)
		}

		// Validate that scripts can be parsed and produce valid
		// taproot output.
		pkScript, err := vtxo.PkScript()
		if err != nil {
			return fmt.Errorf("VTXO %d has invalid scripts: %w",
				i, err)
		}

		if !txscript.IsPayToTaproot(pkScript) {
			return fmt.Errorf(
				"VTXO %d has invalid taproot script", i,
			)
		}

		if vtxo.CoSignerKey == nil {
			return fmt.Errorf("VTXO %d has nil co-signer key", i)
		}

		keyStr := hex.EncodeToString(
			schnorr.SerializePubKey(vtxo.CoSignerKey),
		)
		if _, exists := seen[keyStr]; exists {
			return fmt.Errorf(
				"VTXO %d has duplicate co-signer key", i,
			)
		}
		seen[keyStr] = struct{}{}
	}

	return nil
}

// ValidateConnectorDescriptor validates a connector descriptor ensuring it is
// suitable for connector tree construction.
func ValidateConnectorDescriptor(conn ConnectorDescriptor) error {
	if conn.NumLeaves <= 0 {
		return fmt.Errorf(
			"connector descriptor has invalid NumLeaves: %d",
			conn.NumLeaves,
		)
	}
	if conn.Amount <= 0 {
		return fmt.Errorf("connector descriptor has invalid amount: %d",
			conn.Amount)
	}
	if len(conn.PkScript) == 0 {
		return fmt.Errorf("connector descriptor has empty PkScript")
	}
	if !txscript.IsPayToTaproot(conn.PkScript) {
		return fmt.Errorf(
			"connector descriptor has invalid taproot script",
		)
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
// using a BFS (breadth-first search) algorithm. It returns a Tree with all
// transactions fully constructed and linked.
func BuildVTXOTree(
	batchOutpoint wire.OutPoint,
	batchOutput *wire.TxOut,
	vtxos []VTXODescriptor,
	operatorCoSignKey *btcec.PublicKey,
	sweepKey *btcec.PublicKey,
	sweepDelay closure.RelativeLocktime,
	radix int,
) (*Tree, error) {

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
	for i := range vtxos {
		leaf, err := vtxos[i].ToLeafDescriptor()
		if err != nil {
			return nil, fmt.Errorf("VTXO %d: %w", i, err)
		}
		leaves[i] = leaf
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
	sweepTapLeaf, err := scripts.CSVTimeoutTapLeaf(sweepKey, sweepDelay)
	if err != nil {
		return nil, fmt.Errorf("failed to create sweep tap leaf: %w",
			err)
	}
	sweepTapRoot := sweepTapLeaf.TapHash()

	return NewTree(
		batchOutpoint, batchOutput, leaves,
		operatorCoSignKey, sweepTapRoot[:], radix,
	)
}

// BuildConnectorTree constructs a connector tree from a connector descriptor.
// Unlike VTXO trees, connector trees have identical leaves (same script, same
// amount) and are signed only by the operator. This is used for forfeit
// transaction inputs.
func BuildConnectorTree(
	batchOutpoint wire.OutPoint,
	batchOutput *wire.TxOut,
	connector ConnectorDescriptor,
	operatorKey *btcec.PublicKey,
	radix int,
) (*Tree, error) {

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

	return NewTree(
		batchOutpoint, batchOutput, leaves,
		operatorKey, nil, radix,
	)
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
func BuildBatchOutput(vtxos []VTXODescriptor,
	operatorMuSigKey *btcec.PublicKey, sweepKey *btcec.PublicKey,
	sweepDelay closure.RelativeLocktime) (*wire.TxOut, error) {

	if len(vtxos) == 0 {
		return nil, fmt.Errorf("batch output requires at least " +
			"one VTXO")
	}

	if operatorMuSigKey == nil {
		return nil, fmt.Errorf("operator musig key cannot be nil")
	}

	if sweepKey == nil {
		return nil, fmt.Errorf("sweep key cannot be nil")
	}

	// Compute the sweep tap leaf for tweaking.
	sweepTapLeaf, err := scripts.CSVTimeoutTapLeaf(sweepKey, sweepDelay)
	if err != nil {
		return nil, fmt.Errorf("failed to create sweep tap leaf: %w",
			err)
	}
	sweepTapRoot := sweepTapLeaf.TapHash()

	// Collect unique cosigners (operator + all client cosigners).
	signers := []*btcec.PublicKey{operatorMuSigKey}
	seenSigners := make(map[string]struct{})
	operatorKeyStr := hex.EncodeToString(
		schnorr.SerializePubKey(operatorMuSigKey),
	)
	seenSigners[operatorKeyStr] = struct{}{}

	var totalAmount btcutil.Amount
	for _, vtxo := range vtxos {
		if vtxo.Amount < 0 {
			return nil, fmt.Errorf("vtxo amount cannot be negative")
		}

		// Check overflow before adding.
		if totalAmount > math.MaxInt64-vtxo.Amount {
			return nil, fmt.Errorf("total amount exceeds int64 " +
				"range")
		}

		totalAmount += vtxo.Amount

		// Add cosigner if not already seen.
		keyStr := hex.EncodeToString(
			schnorr.SerializePubKey(vtxo.CoSignerKey),
		)
		if _, seen := seenSigners[keyStr]; !seen {
			seenSigners[keyStr] = struct{}{}
			signers = append(signers, vtxo.CoSignerKey)
		}
	}

	// Aggregate cosigner keys and tweak with sweep tapscript root.
	aggKey, _, _, err := musig2.AggregateKeys(
		signers, true,
		musig2.WithTaprootKeyTweak(sweepTapRoot[:]),
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

// BuildConnectorOutput computes the pkscript and output amount for a connector
// output in the commitment transaction. This output will be spent by the root
// of the connector tree.
//
// The total amount is numConnectors * dustAmount since each connector leaf
// receives the dust amount.
func BuildConnectorOutput(
	numConnectors int,
	dustAmount btcutil.Amount,
	connectorAddr btcutil.Address,
) (*wire.TxOut, error) {

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
func NewBranchSweepSpendInfo(
	internalKey, sweepKey *btcec.PublicKey,
	csvDelay closure.RelativeLocktime,
) (*scripts.VTXOSpendData, error) {

	if internalKey == nil {
		return nil, fmt.Errorf("internal key cannot be nil")
	}

	if sweepKey == nil {
		return nil, fmt.Errorf("sweep key cannot be nil")
	}

	sweepLeaf, err := scripts.CSVTimeoutTapLeaf(sweepKey, csvDelay)
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

	return &scripts.VTXOSpendData{
		WitnessScript: sweepLeaf.Script,
		ControlBlock:  ctrlBytes,
	}, nil
}
