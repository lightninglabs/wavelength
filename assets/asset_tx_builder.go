package assets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/assetwalletrpc"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"google.golang.org/grpc"
)

// SignerRole identifies a signer participating in MuSig2 aggregation.
type SignerRole string

// AnchorKeyMode describes the admissible spend path on the Bitcoin key path.
type AnchorKeyMode uint8

const (
	// AnchorKeyModeMuSig2 indicates a MuSig2 cooperative spend path.
	AnchorKeyModeMuSig2 AnchorKeyMode = iota

	// AnchorKeyModeStatic indicates an externally-managed static key.
	// The caller is responsible for signing.
	AnchorKeyModeStatic

	// AnchorKeyModeTapdManaged indicates the key is managed by tapd.
	// The builder will query tapd for key derivation info so LND can sign.
	AnchorKeyModeTapdManaged
)

// AnchorKeySpec captures the key material for the anchor key path.
type AnchorKeySpec struct {
	// Mode states whether the anchor spends via MuSig2 or a static script.
	Mode AnchorKeyMode

	// MuSig2 describes the cooperative signers when Mode is
	// AnchorKeyModeMuSig2.
	MuSig2 *MuSig2Spec

	// Key is the x-only internal key when Mode is AnchorKeyModeStatic.
	Key []byte
}

// MuSig2Spec defines the cooperative key spend participants.
type MuSig2Spec struct {
	// Participants enumerates the MuSig2 signers in their logical order.
	Participants []MuSig2Participant

	// SortKeys requests deterministic key ordering before aggregation.
	SortKeys bool

	// Tweaks mirrors the taproot tweaks used during aggregation.
	Tweaks MuSig2Tweaks
}

// MuSig2Participant links a signer role to a compressed pubkey.
type MuSig2Participant struct {
	// Role declares why this participant is signing (user, operator, etc).
	Role SignerRole

	// PubKey is the participant’s 33-byte compressed MuSig2 key.
	PubKey []byte
}

// MuSig2Tweaks mirrors input.MuSig2Tweaks whilst keeping the builder decoupled.
type MuSig2Tweaks struct {
	// TaprootBIP0086Tweak applies the standard BIP-0086 tweak during
	// aggregation.
	TaprootBIP0086Tweak bool

	// TaprootTweak is the taproot script root used for key tweaking. When
	// set, the MuSig2 aggregate key is tweaked with this value instead of
	// using BIP-0086. This is mutually exclusive with TaprootBIP0086Tweak.
	TaprootTweak []byte
}

// closuresToSiblingPreimage builds a tapscript sibling preimage from the
// provided closures. When there's a single closure, returns a leaf preimage.
// For multiple closures, constructs a branch preimage.
func closuresToSiblingPreimage(
	closures []ScriptClosure) (*commitment.TapscriptPreimage, error) {

	if len(closures) == 0 {
		return nil, nil
	}

	// Single closure: create a leaf preimage.
	if len(closures) == 1 {
		tapLeaf, err := closures[0].TapLeaf()
		if err != nil {
			return nil, fmt.Errorf("get tap leaf: %w", err)
		}

		preimage, err := commitment.NewPreimageFromLeaf(tapLeaf)
		if err != nil {
			return nil, fmt.Errorf("create leaf preimage: %w", err)
		}

		return preimage, nil
	}

	// Multiple closures: build a tapscript tree from all leaves and return
	// a branch preimage. We use AssembleTaprootScriptTree to construct the
	// tree which handles optimal node placement.
	leaves := make([]txscript.TapLeaf, 0, len(closures))
	for _, c := range closures {
		tapLeaf, err := c.TapLeaf()
		if err != nil {
			return nil, fmt.Errorf("get tap leaf for %q: %w",
				c.ID, err)
		}
		leaves = append(leaves, tapLeaf)
	}

	// Build tree from leaves.
	tapTree := txscript.AssembleTaprootScriptTree(leaves...)
	rootNode := tapTree.RootNode

	// Get the root as a branch. The root is always a branch when there are
	// 2+ leaves.
	branch, ok := rootNode.(txscript.TapBranch)
	if !ok {
		return nil, fmt.Errorf("expected branch node, got %T", rootNode)
	}

	preimage := commitment.NewPreimageFromBranch(branch)

	return &preimage, nil
}

// InputConfig associates a proof chain with anchor metadata.
type InputConfig struct {
	// ProofFile is the raw proof blob exported from tapd.
	ProofFile []byte

	// AnchorKey describes how the Bitcoin anchor spends this input.
	AnchorKey AnchorKeySpec

	// Closures lists optional tapscript branches attached to the anchor.
	Closures []ScriptClosure

	// Sequence optionally sets the input sequence number. A zero value
	// uses the default (MaxTxInSequenceNum). For CSV script path spends,
	// this must be set to the CSV delay value.
	Sequence uint32
}

// OutputConfig defines the anchor we are assembling.
type OutputConfig struct {
	// Amount is the asset amount assigned to the output.
	Amount uint64

	// Type selects the virtual output type. Defaults to
	// tappsbt.TypeSimple when unset.
	Type tappsbt.VOutputType

	// Interactive controls whether the output is spent interactively by
	// the recipient (defaults to true when nil).
	Interactive *bool

	// AnchorKey describes how the anchor output can be spent.
	AnchorKey AnchorKeySpec

	// Closures enumerates tapscript branches the anchor commits to.
	Closures []ScriptClosure

	// Script defines the asset-level script committed to the vpacket
	// output.
	Script AssetScriptSpec
}

// AssetTxPlan exposes the assembled VPacket and witness plans.
type AssetTxPlan struct {
	// Packet is the fully assembled virtual transaction packet.
	Packet *tappsbt.VPacket

	// OutputPlans mirrors Packet.Outputs and records witness metadata per
	// output.
	OutputPlans []AnchorPlan

	// AnchorOutputs lists the BTC anchor transaction outputs corresponding
	// to each asset output in Packet order. This is populated after Commit
	// when the anchor PSBT is available. Entries may be nil if anchors
	// were not committed.
	AnchorOutputs []*wire.TxOut

	// BtcInputs enumerates BTC-only inputs that will be attached to the
	// anchor PSBT.
	BtcInputs []BtcInputPlan

	// BtcOutputs lists BTC-only outputs that the builder will append to
	// the anchor PSBT.
	BtcOutputs []BtcOutputPlan

	// EphemeralAnchor is the optional zero-value P2A anchor the builder
	// will append to the PSBT after Commit(). Nil if no anchor is
	// configured.
	EphemeralAnchor *EphemeralAnchorPlan
}

// AssetScriptSpec describes how to construct the asset-level script for a
// virtual output. Implementations can provide OP_TRUE scripts, direct wallet
// keys or any future scripting variant.
type AssetScriptSpec interface {
	Prepare(anchorKey AnchorKeySpec, params *address.ChainParams) (
		*AssetScriptArtifacts, error)
}

// AssetScriptArtifacts bundle the data required to populate a tappsbt output
// and to later construct spend witnesses for the asset script.
type AssetScriptArtifacts struct {
	// ScriptKey is the taproot-assets script key committed to the vpacket.
	ScriptKey *asset.ScriptKey

	// TapscriptSibling is an optional sibling preimage when the anchor
	// output carries a tapscript path.
	TapscriptSibling *commitment.TapscriptPreimage

	// AnchorInternal is the internal key corresponding to the anchor
	// output.
	AnchorInternal *btcec.PublicKey

	// Witness is the optional asset-level witness stack (e.g., OP_TRUE).
	Witness wire.TxWitness

	// OutputKey is the tweaked taproot output key, if applicable.
	OutputKey *btcec.PublicKey

	// Details carries script-specific data exposed by the spec.
	Details AssetScriptDetails
}

// AssetScriptDetails describes additional information about a script.
type AssetScriptDetails interface {
	// Type returns the identifier for the script implementation.
	Type() AssetScriptType
}

// AssetScriptType names a concrete asset script implementation.
type AssetScriptType string

const (
	// AssetScriptTypeOpTrue identifies the simple OP_TRUE script.
	AssetScriptTypeOpTrue AssetScriptType = "op_true"
)

// AnchorChildOptions drives the CPFP child builder.
type AnchorChildOptions struct {
	ChangeAddress btcutil.Address
	FeeRate       chainfee.SatPerKWeight
}

// AnchorFundingWallet is the minimal wallet interface required to fund/sign
// the CPFP child transaction.
type AnchorFundingWallet interface {
	FundPsbt(ctx context.Context, packet *psbt.Packet, changeIndex int,
		feeRate chainfee.SatPerKWeight) (*psbt.Packet, error)

	SignPsbt(ctx context.Context, packet *psbt.Packet) (*psbt.Packet, error)
}

// EphemeralAnchorPlan tracks the output index of a zero-value P2A (Pay-to-
// Anchor) output used for CPFP fee bumping.
type EphemeralAnchorPlan struct {
	// OutputIndex is the PSBT output index assigned after Commit().
	OutputIndex int
}

// BtcInputSpec describes a Bitcoin-only input that should be attached to the
// anchor PSBT alongside the asset inputs. These inputs do not carry Taproot
// Assets state but must appear in the on-chain transaction (for example,
// connectors that fund forfeit paths).
type BtcInputSpec struct {
	// Description is an optional human readable label for the input.
	Description string

	// Outpoint identifies the UTXO consumed by the anchor transaction.
	Outpoint wire.OutPoint

	// WitnessUtxo is the previous output referenced by Outpoint.
	WitnessUtxo *wire.TxOut

	// Sequence optionally overrides the default MaxTxInSequenceNum. A zero
	// value requests the default.
	Sequence uint32

	// SighashType optionally overrides the default SigHashDefault.
	SighashType *txscript.SigHashType

	// TaprootLeafScript enumerates tapscript branches committed to the
	// referenced UTXO. This mirrors psbt.PInput.TaprootLeafScript.
	TaprootLeafScript []*psbt.TaprootTapLeafScript

	// TaprootBip32Derivation mirrors psbt.PInput.TaprootBip32Derivation.
	TaprootBip32Derivation []*psbt.TaprootBip32Derivation

	// Bip32Derivation mirrors psbt.PInput.Bip32Derivation.
	Bip32Derivation []*psbt.Bip32Derivation
}

// BtcInputPlan records the normalized data for a BTC-only input that was
// attached to the anchor PSBT.
type BtcInputPlan struct {
	// Description echoes the caller supplied label.
	Description string

	// Outpoint references the consumed UTXO.
	Outpoint wire.OutPoint

	// WitnessUtxo is the previous output committed in the PSBT.
	WitnessUtxo *wire.TxOut

	// Sequence records the sequence number applied when the input was
	// written to the anchor transaction.
	Sequence uint32
}

// BtcOutputSpec declares a Bitcoin-only anchor output that should be added
// to the PSBT after the asset virtual transaction commits. This is useful for
// modelling connector trees or other Ark-specific anchoring scripts that do
// not carry Taproot Assets state.
type BtcOutputSpec struct {
	// Description is an optional human readable label for the output.
	Description string

	// ValueSat is the satoshi amount assigned to the output.
	ValueSat int64

	// PkScript is the Bitcoin script committed to the output.
	PkScript []byte
}

// BtcOutputPlan captures the PSBT level metadata for a BTC-only anchor
// output.
type BtcOutputPlan struct {
	// Description echoes the spec label.
	Description string

	// ValueSat mirrors the satoshi amount assigned to the output.
	ValueSat int64

	// PkScript is the Bitcoin script committed to the output.
	PkScript []byte

	// OutputIndex records the final PSBT output index assigned after
	// Commit().
	OutputIndex int
}

// OpTrueScriptDetails exposes the pre-built artifacts for OP_TRUE scripts.
type OpTrueScriptDetails struct {
	Artifacts *OpTrueArtifacts
}

// Type implements AssetScriptDetails.
func (o *OpTrueScriptDetails) Type() AssetScriptType {
	return AssetScriptTypeOpTrue
}

// OpTrueScript returns an AssetScriptSpec that uses the global NUMS-based
// OP_TRUE script key. All outputs using this share the same script key.
func OpTrueScript() AssetScriptSpec {
	return &OpTrueScriptSpec{InternalKey: asset.NUMSPubKey}
}

// OpTrueUniqueScript returns an AssetScriptSpec that creates a unique OP_TRUE
// script key by using the provided internal key. This allows multiple outputs
// to have distinct script keys while still being spendable via the OP_TRUE
// tapscript path.
//
// Unlike OpTrueScript which uses NUMS and produces the same script key for all
// outputs, this function produces unique script keys based on the internal key.
// The TweakedScriptKey info is properly populated for proof verification.
func OpTrueUniqueScript(internalKey *btcec.PublicKey) AssetScriptSpec {
	return &OpTrueScriptSpec{InternalKey: internalKey}
}

// OpTrueScriptSpec implements the AssetScriptSpec interface for the simple
// OP_TRUE asset script.
type OpTrueScriptSpec struct {
	// InternalKey is used for both the OP_TRUE script key derivation and
	// as the anchor internal key. Use asset.NUMSPubKey for a global script
	// key, or a unique key per output for distinct script keys.
	InternalKey *btcec.PublicKey
}

func (s *OpTrueScriptSpec) Prepare(anchorKey AnchorKeySpec,
	params *address.ChainParams) (*AssetScriptArtifacts, error) {

	if s.InternalKey == nil {
		return nil, errors.New("op_true spec requires internal key")
	}

	// Build OP_TRUE artifacts using the internal key.
	opTrue, err := BuildOpTrueArtifacts(s.InternalKey)
	if err != nil {
		return nil, fmt.Errorf("build op_true artifacts: %w", err)
	}

	return &AssetScriptArtifacts{
		ScriptKey:        &opTrue.ScriptKey,
		TapscriptSibling: opTrue.SiblingPreimage,
		AnchorInternal:   s.InternalKey,
		Witness:          opTrue.Witness,
		OutputKey:        opTrue.OutputKey,
		Details: &OpTrueScriptDetails{
			Artifacts: opTrue,
		},
	}, nil
}

// DirectWalletScript constructs a spec that uses the provided wallet-derived
// taproot key directly with no additional script tweaks.
func DirectWalletScript(scriptKey *asset.ScriptKey) AssetScriptSpec {
	return &DirectWalletScriptSpec{ScriptKey: scriptKey}
}

// DirectWalletScriptSpec passes through a wallet derived taproot output key
// without adding an OP_TRUE tapscript sibling.
type DirectWalletScriptSpec struct {
	ScriptKey *asset.ScriptKey
}

func (s *DirectWalletScriptSpec) Prepare(anchorKey AnchorKeySpec,
	params *address.ChainParams) (*AssetScriptArtifacts, error) {

	if s.ScriptKey == nil {
		return nil, errors.New("wallet script spec requires script key")
	}

	return &AssetScriptArtifacts{
		ScriptKey:      s.ScriptKey,
		AnchorInternal: s.ScriptKey.PubKey,
		OutputKey:      s.ScriptKey.PubKey,
	}, nil
}

// CommitOptions capture the configurable parameters when embedding the
// assembled virtual transaction into a BTC-level anchor PSBT.
type CommitOptions struct {
	// FeeRate encodes the desired sat/vbyte rate. A zero value instructs
	// tapd to skip adding fee inputs.
	FeeRate chainfee.SatPerVByte

	// SkipWalletFunding requests that the wallet does not contribute any
	// BTC inputs to the anchor transaction (caller will CPFP later).
	SkipWalletFunding bool

	// ChangeOutput optionally points to an existing change key descriptor
	// that should be reused for the anchor transaction.
	ChangeOutput *keychain.KeyDescriptor

	// Leases are optional pre-leased UTXOs that must be attached to the
	// anchor template before committing.
	Leases []lndclient.LeaseDescriptor

	// LockID identifies the lease applied to any BTC inputs the wallet
	// adds.
	LockID *wtxmgr.LockID

	// LockDuration determines how long newly locked anchor inputs remain
	// held.
	LockDuration time.Duration

	// SkipZeroFeeBalance disables the automatic zero-fee balancing output
	// that is normally added when skipping wallet funding. Use this when
	// the caller will manually adjust output values after commit.
	SkipZeroFeeBalance bool

	// AssetOutputValues optionally specifies custom BTC values for asset
	// outputs. The map key is the anchor output index and value is the
	// desired satoshi amount. When provided, these values are applied to
	// the anchor template BEFORE calling CommitVirtualPsbts, ensuring that
	// proofs reference the correct transaction hash. This is essential for
	// zero-fee tree propagation where outputs need specific BTC values.
	AssetOutputValues map[uint32]int64

	// NoChangeOutput disables the automatic change output that tapd adds
	// during CommitVirtualPsbts. Use this when the transaction already has
	// exact balance (all input BTC goes to asset outputs + fee).
	NoChangeOutput bool
}

// PublishOptions controls how Publish behaves.
type PublishOptions struct {
	// SkipBroadcast instructs tapd to log the transfer but skip anchor
	// broadcast (caller will publish via package).
	SkipBroadcast bool
}

// AnchorPlan retains per-output witness metadata.
type AnchorPlan struct {
	// OutputIndex matches the index in the virtual packet output list.
	OutputIndex int

	// AnchorKey echoes the spec used when the output was added.
	AnchorKey AnchorKeySpec

	// Closures lists the tapscript closures committed for this output.
	Closures []ScriptClosure

	// AssetScriptKey is the serialized script key committed to the packet.
	AssetScriptKey []byte

	// AnchorInternal is the serialized internal key exposed to tapd.
	AnchorInternal []byte

	// Witness summarizes how this anchor output will be spent.
	Witness WitnessPlan
}

// WitnessPlan records how an anchor output will be spent.
type WitnessPlan struct {
	// Mode states whether the anchor uses MuSig2 or a static script path.
	Mode AnchorKeyMode

	// MuSig2 stores the MuSig2 configuration when Mode is
	// AnchorKeyModeMuSig2.
	MuSig2 *MuSig2Spec

	// Script references the default script closure when spending via
	// tapscript.
	Script *ScriptClosure

	// ScriptDetails carries script-specific data returned by the spec.
	ScriptDetails AssetScriptDetails

	// Witness is the asset-level witness (e.g. OP_TRUE stack) if one
	// exists.
	Witness wire.TxWitness

	// ScriptPlans caches per-closure proofs for tapscript spends.
	ScriptPlans map[string]*ScriptWitnessPlan
}

// ScriptWitnessPlan captures the static data required to assemble a tapscript
// witness for a particular closure.
type ScriptWitnessPlan struct {
	// Closure is the script branch this plan corresponds to.
	Closure ScriptClosure

	// TapLeaf is the fully constructed tapscript leaf for the closure.
	TapLeaf txscript.TapLeaf

	// ControlBlock is the serialized control block (excluding the script).
	ControlBlock []byte

	// OutputKey is the tweaked taproot output key backing the anchor.
	OutputKey *btcec.PublicKey

	// AssetRoot is the taproot asset commitment root from the proof.
	AssetRoot []byte

	// InclusionProof is the raw sibling hash path used in the control
	// block.
	InclusionProof []byte

	// TaprootRoot is the combined tweak committed in the anchor output.
	TaprootRoot []byte

	// ScriptRoot is the hash of the tapscript tree without the asset root.
	ScriptRoot []byte
}

// ScriptSpendDetails describes the data required to produce a tapscript
// signature for an anchor input that must be spent via a script path.
type ScriptSpendDetails struct {
	// InputIndex is the anchor PSBT input index this plan applies to.
	InputIndex int

	// ClosureID identifies which tapscript closure the caller selected.
	ClosureID string

	// Closure is the script closure instance referenced by ClosureID.
	Closure ScriptClosure

	// SigHash is the 32-byte tapscript signature hash (BIP-341).
	SigHash [32]byte

	// TapLeaf is the tapscript leaf committed in the anchor.
	TapLeaf txscript.TapLeaf

	// InternalKey is the x-only internal key specified for the anchor.
	InternalKey *btcec.PublicKey

	// ControlBlock is the serialized control block supplied to the witness.
	ControlBlock []byte

	// OutputKey is the tweaked taproot output key for the anchor.
	OutputKey *btcec.PublicKey

	// AssetRoot is the tapscript root of the Taproot Asset commitment.
	AssetRoot []byte

	// ScriptRoot is the closure-only tapscript merkle root.
	ScriptRoot []byte

	// TaprootRoot is the combined tweak committed in the output.
	TaprootRoot []byte
}

// AssetTxBuilder orchestrates Taproot Asset transaction assembly/lifecycle.
type AssetTxBuilder struct {
	// assetID is the single asset this builder instance manages.
	assetID asset.ID

	// params are the Taproot Assets chain parameters.
	params *address.ChainParams

	// inputs holds the staged asset inputs together with decoded proof
	// data.
	inputs []inputSpec

	// outputs are the requested anchor outputs and asset destinations.
	outputs []outputSpec

	// vpacket is the virtual transaction assembled during Compile().
	vpacket *tappsbt.VPacket

	// proofs caches the decoded proofs backing each input.
	proofs []*proof.Proof

	// plans stores the witness plans for each output in vpacket order.
	plans []AnchorPlan

	// commitResp keeps the last CommitVirtualPsbts response from tapd.
	commitResp *assetwalletrpc.CommitVirtualPsbtsResponse

	// activePkts are the virtual packets that move assets in this transfer.
	activePkts []*tappsbt.VPacket

	// passivePkts are extra packets tapd asks us to carry alongside active
	// ones.
	passivePkts []*tappsbt.VPacket

	// anchorPsbt is the Bitcoin PSBT template returned by tapd.
	anchorPsbt *psbt.Packet

	// anchorOutputs caches the anchor transaction outputs for asset
	// outputs. Populated after Commit().
	anchorOutputs []*wire.TxOut

	// hasEphemeralAnchor indicates whether a P2A anchor should be added.
	hasEphemeralAnchor bool

	// ephemeralAnchorPlan tracks the output index for the P2A anchor.
	// Nil until Commit() populates it.
	ephemeralAnchorPlan *EphemeralAnchorPlan

	// btcInputs stores BTC-only inputs configured by the caller.
	btcInputs []btcInputEntry

	// btcOutputs stores BTC-only outputs configured by the caller.
	btcOutputs []btcOutputEntry

	// btcInputPlans mirror btcInputs in plan form.
	btcInputPlans []BtcInputPlan

	// btcOutputPlans mirror btcOutputs while tracking PSBT indices.
	btcOutputPlans []BtcOutputPlan

	// skipWalletFinalize instructs FinalizeAnchor to bypass wallet signing.
	skipWalletFinalize bool

	// scriptWitnesses keeps asset-level witness stacks per virtual output.
	scriptWitnesses map[int]wire.TxWitness

	// anchorWitnesses caches script-path anchor witnesses per PSBT input.
	anchorWitnesses map[int]wire.TxWitness
}

// AssetWalletClient is the exported alias for the internal wallet client
// interface used during Commit/Publish.
//
//nolint:iface
type AssetWalletClient interface {
	assetWalletClient
}

// ActivePackets returns the committed active virtual packets, if any. A
// shallow copy is returned to avoid exposing internal slices to mutation.
func (b *AssetTxBuilder) ActivePackets() []*tappsbt.VPacket {
	if len(b.activePkts) == 0 {
		return nil
	}

	out := make([]*tappsbt.VPacket, len(b.activePkts))
	copy(out, b.activePkts)

	return out
}

type inputSpec struct {
	cfg          InputConfig
	proof        []*proof.Proof
	assetRoot    [32]byte
	hasAssetRoot bool
	scriptPlans  map[string]*scriptSpendPlan
}

type outputSpec struct {
	cfg OutputConfig
}

type btcInputEntry struct {
	plan                   BtcInputPlan
	sighashType            txscript.SigHashType
	taprootLeafScript      []*psbt.TaprootTapLeafScript
	taprootBip32Derivation []*psbt.TaprootBip32Derivation
	bip32Derivation        []*psbt.Bip32Derivation
}

type btcOutputEntry struct {
	plan BtcOutputPlan
}

func (b *AssetTxBuilder) buildVOutput(index int, version asset.Version,
	cfg OutputConfig) (AnchorPlan, *tappsbt.VOutput, error) {

	if cfg.Script == nil {
		return AnchorPlan{}, nil, errors.New("output script spec " +
			"missing")
	}

	artifacts, err := cfg.Script.Prepare(cfg.AnchorKey, b.params)
	if err != nil {
		return AnchorPlan{}, nil, fmt.Errorf("prepare asset script: "+
			"%w", err)
	}
	if artifacts == nil || artifacts.ScriptKey == nil {
		return AnchorPlan{}, nil, errors.New("script spec returned " +
			"no script key")
	}

	outputType := cfg.Type
	if outputType == 0 {
		outputType = tappsbt.TypeSimple
	}

	interactive := true
	if cfg.Interactive != nil {
		interactive = *cfg.Interactive
	}

	anchorInternal := artifacts.AnchorInternal
	if anchorInternal == nil && artifacts.ScriptKey.PubKey != nil {
		anchorInternal = artifacts.ScriptKey.PubKey
	}

	vOut := &tappsbt.VOutput{
		Amount:            cfg.Amount,
		AssetVersion:      version,
		Type:              outputType,
		Interactive:       interactive,
		AnchorOutputIndex: uint32(index),
	}
	vOut.ScriptKey = *artifacts.ScriptKey

	if anchorInternal != nil {
		vOut.AnchorOutputInternalKey = anchorInternal
	}

	// Determine the anchor-level tapscript sibling. When closures are
	// present, they form the sibling of the asset commitment in the anchor
	// tapscript tree. Without closures, use the asset script's sibling
	// (typically OP_TRUE).
	if len(cfg.Closures) > 0 {
		closureSibling, err := closuresToSiblingPreimage(cfg.Closures)
		if err != nil {
			return AnchorPlan{}, nil, fmt.Errorf("build closure "+
				"sibling preimage: %w", err)
		}

		vOut.AnchorOutputTapscriptSibling = closureSibling
	} else if artifacts.TapscriptSibling != nil {
		vOut.AnchorOutputTapscriptSibling = artifacts.TapscriptSibling
	}

	assetScriptKeyBytes := []byte(nil)
	if artifacts.ScriptKey.PubKey != nil {
		assetScriptKeyBytes =
			artifacts.ScriptKey.PubKey.SerializeCompressed()
	}

	anchorInternalBytes := []byte(nil)
	if anchorInternal != nil {
		anchorInternalBytes = schnorr.SerializePubKey(anchorInternal)
	}

	plan := AnchorPlan{
		OutputIndex:    index,
		AnchorKey:      cfg.AnchorKey,
		Closures:       cfg.Closures,
		AssetScriptKey: assetScriptKeyBytes,
		AnchorInternal: anchorInternalBytes,
		Witness: WitnessPlan{
			Mode:    cfg.AnchorKey.Mode,
			MuSig2:  cfg.AnchorKey.MuSig2,
			Witness: artifacts.Witness,
		},
	}

	if artifacts.Details != nil {
		plan.Witness.ScriptDetails = artifacts.Details
	}

	if len(cfg.Closures) > 0 {
		plan.Witness.ScriptPlans = make(
			map[string]*ScriptWitnessPlan, len(cfg.Closures),
		)

		// Each closure receives a witness plan. The plan is populated
		// later with the control block, script/taproot roots, and
		// cached witness.  By keeping the plan keyed by closure ID we
		// can look up the taproot tweak without regenerating the proof.
		for _, closure := range cfg.Closures {
			tapLeaf, err := closure.TapLeaf()
			if err != nil {
				return AnchorPlan{}, nil, fmt.Errorf("derive "+
					"tap leaf for closure %q: %w",
					closure.ID, err)
			}

			plan.Witness.ScriptPlans[closure.ID] =
				&ScriptWitnessPlan{
					Closure: closure,
					TapLeaf: tapLeaf,
				}
		}
	}

	if cfg.AnchorKey.Mode == AnchorKeyModeStatic && len(cfg.Closures) > 0 {
		plan.Witness.Script = &cfg.Closures[0]
	}

	return plan, vOut, nil
}

// NewAssetTxBuilder constructs a builder for the given asset on the specified
// network.
func NewAssetTxBuilder(assetID asset.ID,
	params *address.ChainParams) *AssetTxBuilder {

	return &AssetTxBuilder{
		assetID:         assetID,
		params:          params,
		btcInputs:       make([]btcInputEntry, 0),
		btcOutputs:      make([]btcOutputEntry, 0),
		btcInputPlans:   make([]BtcInputPlan, 0),
		btcOutputPlans:  make([]BtcOutputPlan, 0),
		scriptWitnesses: make(map[int]wire.TxWitness),
		anchorWitnesses: make(map[int]wire.TxWitness),
	}
}

// AddEphemeralAnchor configures the builder to include a zero-value P2A
// (Pay-to-Anchor) output in the transaction. This output is used for CPFP fee
// bumping via package relay. The anchor uses the standard P2A witness program
// (OP_1 <0x4e73>) which is anyone-can-spend and recognized by Bitcoin Core for
// ephemeral anchor packages. Only one ephemeral anchor is supported per
// transaction.
func (b *AssetTxBuilder) AddEphemeralAnchor() {
	b.hasEphemeralAnchor = true
}

// AddBtcInput appends a BTC-only input specification that will be inserted
// into the anchor PSBT. These inputs are not part of the Taproot Assets
// virtual transaction but must be consumed by the on-chain transaction (for
// example connector leaves used in ARK forfeits).
func (b *AssetTxBuilder) AddBtcInput(spec BtcInputSpec) error {
	if spec.WitnessUtxo == nil {
		return errors.New("anchor input witness utxo missing")
	}

	if len(spec.WitnessUtxo.PkScript) == 0 {
		return errors.New("anchor input witness utxo script missing")
	}

	var zeroHash chainhash.Hash
	if spec.Outpoint.Hash == zeroHash && spec.Outpoint.Index == 0 {
		return errors.New("anchor input outpoint missing")
	}

	sequence := spec.Sequence
	if sequence == 0 {
		sequence = wire.MaxTxInSequenceNum
	}

	sighash := txscript.SigHashDefault
	if spec.SighashType != nil {
		sighash = *spec.SighashType
	}

	plan := BtcInputPlan{
		Description: spec.Description,
		Outpoint:    spec.Outpoint,
		WitnessUtxo: cloneTxOut(spec.WitnessUtxo),
		Sequence:    sequence,
	}

	inputSpec := btcInputEntry{
		plan:        plan,
		sighashType: sighash,
		taprootLeafScript: cloneTaprootLeafScripts(
			spec.TaprootLeafScript,
		),
		taprootBip32Derivation: cloneTaprootBip32(
			spec.TaprootBip32Derivation,
		),
		bip32Derivation: cloneBip32(
			spec.Bip32Derivation,
		),
	}

	b.btcInputs = append(b.btcInputs, inputSpec)
	b.btcInputPlans = append(b.btcInputPlans, cloneBtcInputPlan(plan))

	return nil
}

// AddBtcOutput appends a BTC-only anchor output that does not carry Taproot
// Assets state. The output is materialised in the PSBT after Commit() runs.
func (b *AssetTxBuilder) AddBtcOutput(spec BtcOutputSpec) error {
	if spec.ValueSat < 0 {
		return errors.New("anchor output value must be non-negative")
	}

	if len(spec.PkScript) == 0 {
		return errors.New("anchor output script missing")
	}

	plan := BtcOutputPlan{
		Description: spec.Description,
		ValueSat:    spec.ValueSat,
		PkScript:    append([]byte(nil), spec.PkScript...),
		OutputIndex: -1,
	}

	b.btcOutputs = append(b.btcOutputs, btcOutputEntry{
		plan: plan,
	})
	b.btcOutputPlans = append(b.btcOutputPlans, cloneBtcOutputPlan(plan))

	return nil
}

// tapdAssetOutputDust is the default dust value tapd assigns to each asset
// output when committing virtual transactions.
const tapdAssetOutputDust = int64(1000)

// ensureZeroFeeAnchor balances the anchor PSBT by adding or adjusting a BTC
// output so total inputs equal total outputs. This is useful when callers
// skip wallet funding but still need a dust-safe, fee-neutral anchor tx.
func (b *AssetTxBuilder) ensureZeroFeeAnchor() error {
	// If caller already provided BTC outputs, assume they manage balance.
	if len(b.btcOutputs) > 0 {
		return nil
	}

	// Sum BTC value from asset proofs.
	var totalInputValue int64
	for _, in := range b.inputs {
		if len(in.proof) == 0 {
			continue
		}

		lastProof := in.proof[len(in.proof)-1]
		if lastProof == nil || lastProof.AnchorTx.TxOut == nil ||
			len(lastProof.AnchorTx.TxOut) == 0 {

			return errors.New("missing anchor tx in proof")
		}

		outIdx := lastProof.InclusionProof.OutputIndex
		if int(outIdx) >= len(lastProof.AnchorTx.TxOut) {
			return fmt.Errorf("proof output index %d out of range",
				outIdx)
		}

		totalInputValue += lastProof.AnchorTx.TxOut[outIdx].Value
	}

	if totalInputValue == 0 {
		return nil
	}

	// Subtract the dust value tapd will assign to each asset output. This
	// ensures the auto-balance output correctly compensates so the
	// transaction has zero fee.
	assetOutputDust := int64(len(b.outputs)) * tapdAssetOutputDust
	balanceValue := totalInputValue - assetOutputDust
	if balanceValue < 0 {
		balanceValue = 0
	}

	// If the balance would be zero or negative, don't add an auto-balance
	// output - the asset output dust will consume the input value.
	if balanceValue == 0 {
		return nil
	}

	// Pick the first available internal key from proofs as the balancing
	// destination.
	var changeKey *btcec.PublicKey
	for _, in := range b.inputs {
		if len(in.proof) == 0 {
			continue
		}

		lastProof := in.proof[len(in.proof)-1]
		internalKey := lastProof.InclusionProof.InternalKey
		if lastProof != nil && internalKey != nil {
			changeKey = internalKey
			break
		}
	}
	if changeKey == nil {
		return errors.New("no internal key found for balancing output")
	}

	pkScript, err := txscript.PayToTaprootScript(changeKey)
	if err != nil {
		return fmt.Errorf("build balancing script: %w", err)
	}

	return b.AddBtcOutput(BtcOutputSpec{
		Description: "auto-balance",
		ValueSat:    balanceValue,
		PkScript:    pkScript,
	})
}

// AddAssetInput queues an asset input proof for the builder.
func (b *AssetTxBuilder) AddAssetInput(cfg InputConfig) error {
	if len(cfg.ProofFile) == 0 {
		return errors.New("input proof file missing")
	}

	if err := validateAnchorKey(cfg.AnchorKey); err != nil {
		return fmt.Errorf("invalid input anchor key: %w", err)
	}

	for i, closure := range cfg.Closures {
		if closure.Closure == nil && closure.Script == nil {
			return fmt.Errorf("input closure %d missing script "+
				"definition", i)
		}
	}

	b.inputs = append(b.inputs, inputSpec{cfg: cfg})

	return nil
}

// AddAssetOutput queues an asset anchor output for the builder.
func (b *AssetTxBuilder) AddAssetOutput(cfg OutputConfig) error {
	if cfg.Amount == 0 && cfg.Type != tappsbt.TypeSplitRoot {
		return errors.New("output amount must be greater than zero")
	}

	if err := validateAnchorKey(cfg.AnchorKey); err != nil {
		return fmt.Errorf("invalid output anchor key: %w", err)
	}

	if cfg.Script == nil {
		return errors.New("output asset script spec required")
	}

	for i, closure := range cfg.Closures {
		if closure.Closure == nil && closure.Script == nil {
			return fmt.Errorf("output closure %d missing script "+
				"definition", i)
		}
	}

	b.outputs = append(b.outputs, outputSpec{cfg: cfg})

	return nil
}

// Compile assembles the virtual packet and witness plans.
//
//nolint:funlen
func (b *AssetTxBuilder) Compile(ctx context.Context) (*AssetTxPlan, error) {
	if len(b.inputs) == 0 {
		return nil, errors.New("no inputs configured")
	}

	if len(b.outputs) == 0 {
		return nil, errors.New("no outputs configured")
	}

	if b.params == nil {
		return nil, errors.New("chain params not configured")
	}

	var (
		allProofs    []*proof.Proof
		assetVersion asset.Version
	)

	// From this point onward we are committed to a single asset ID. As each
	// proof file is decoded we store the taproot asset root (needed for
	// witness construction later) and verify that every proof refers to the
	// builder’s target asset. Any mismatch is caught before we talk to
	// tapd.
	for idx := range b.inputs {
		spec := &b.inputs[idx]

		pfFile, err := proof.DecodeFile(spec.cfg.ProofFile)
		if err != nil {
			return nil, fmt.Errorf("decode proof file %d: %w", idx,
				err)
		}

		proofCount := pfFile.NumProofs()
		if proofCount == 0 {
			return nil, fmt.Errorf("proof file %d contains no "+
				"proofs", idx)
		}

		lastProofIdx := uint32(proofCount - 1)
		pr, err := pfFile.ProofAt(lastProofIdx)
		if err != nil {
			return nil, fmt.Errorf("extract proof %d from file "+
				"%d: %w", lastProofIdx, idx, err)
		}

		// If the asset has a SplitCommitment in its witness, strip it.
		// The SplitCommitment is added by proof generation to link
		// split outputs back to the split root, but it's not part of
		// the on-chain commitment (the output was created without it).
		// tapd's ValidateAnchorInputs will recompute the commitment
		// from the input assets, so we need to remove the
		// SplitCommitment to match the original on-chain script.
		if len(pr.Asset.PrevWitnesses) == 1 &&
			pr.Asset.PrevWitnesses[0].SplitCommitment != nil {

			pr.Asset.PrevWitnesses[0].SplitCommitment = nil
		}

		id := pr.Asset.ID()
		if id != b.assetID {
			return nil, fmt.Errorf("input %d asset mismatch", idx)
		}

		if len(allProofs) == 0 {
			assetVersion = pr.Asset.Version
		}

		assetRootBytes, err := GenTaprootAssetRootFromProof(pr)
		if err != nil {
			return nil, fmt.Errorf("compute asset root for input "+
				"%d: %w", idx, err)
		}
		if len(assetRootBytes) != 32 {
			return nil, fmt.Errorf("asset root for input %d must "+
				"be 32 bytes, got %d", idx, len(assetRootBytes))
		}

		copy(spec.assetRoot[:], assetRootBytes)
		spec.hasAssetRoot = true

		if len(spec.cfg.Closures) > 0 {
			internalKey := pr.InclusionProof.InternalKey
			if internalKey == nil {
				return nil, fmt.Errorf("input %d missing "+
					"internal key in inclusion proof", idx)
			}

			plans, err := buildScriptSpendPlans(
				internalKey, spec.assetRoot, spec.cfg.Closures,
				pr,
			)
			if err != nil {
				return nil, fmt.Errorf("build script plans "+
					"for input %d: %w", idx, err)
			}

			spec.scriptPlans = plans
		}

		spec.proof = []*proof.Proof{pr}
		allProofs = append(allProofs, pr)
	}

	if len(allProofs) == 0 {
		return nil, errors.New("no proofs decoded")
	}

	vpkt, err := tappsbt.FromProofs(allProofs, b.params, tappsbt.V1)
	if err != nil {
		return nil, fmt.Errorf("build vpacket from proofs: %w", err)
	}

	// Reset outputs, the builder controls them explicitly.
	vpkt.Outputs = nil

	// Debug totals: track summed input amount for full-value send checks.
	var debugInputAmt uint64
	for _, in := range vpkt.Inputs {
		assetIn := in.Asset()
		if assetIn == nil {
			continue
		}
		debugInputAmt += assetIn.Amount
	}

	outputPlans := make([]AnchorPlan, 0, len(b.outputs))
	for index, out := range b.outputs {
		plan, vOut, err := b.buildVOutput(index, assetVersion, out.cfg)
		if err != nil {
			return nil, fmt.Errorf("build output %d: %w", index,
				err)
		}

		if len(b.outputs) == 1 && debugInputAmt > 0 &&
			vOut.Amount != debugInputAmt {

			vOut.Amount = debugInputAmt
		}

		vpkt.Outputs = append(vpkt.Outputs, vOut)
		outputPlans = append(outputPlans, plan)
	}

	if err := tapsend.PrepareOutputAssets(ctx, vpkt); err != nil {
		return nil, fmt.Errorf("prepare output assets: %w", err)
	}

	scriptWitnesses := make(map[int]wire.TxWitness)
	for index, plan := range outputPlans {
		witness := plan.Witness.Witness
		if len(witness) == 0 {
			continue
		}

		vOut := vpkt.Outputs[index]
		if vOut.Asset == nil {
			return nil, fmt.Errorf("output %d missing asset "+
				"payload", index)
		}
		if err := vOut.Asset.UpdateTxWitness(0, witness); err != nil {
			return nil, fmt.Errorf("set output %d witness: %w",
				index, err)
		}

		scriptWitnesses[index] = witness
		outputPlans[index].Witness.Witness = witness
	}

	// Create ephemeral anchor plan (output index set during Commit).
	var ephemeralPlan *EphemeralAnchorPlan
	if b.hasEphemeralAnchor {
		ephemeralPlan = &EphemeralAnchorPlan{OutputIndex: -1}
	}

	b.vpacket = vpkt
	b.proofs = allProofs
	b.plans = outputPlans
	b.ephemeralAnchorPlan = ephemeralPlan
	b.skipWalletFinalize = false
	b.scriptWitnesses = scriptWitnesses

	btcInputs := make([]BtcInputPlan, len(b.btcInputPlans))
	for i, plan := range b.btcInputPlans {
		btcInputs[i] = cloneBtcInputPlan(plan)
	}

	btcOutputs := make([]BtcOutputPlan, len(b.btcOutputPlans))
	for i, plan := range b.btcOutputPlans {
		btcOutputs[i] = cloneBtcOutputPlan(plan)
	}

	plan := &AssetTxPlan{
		Packet:          vpkt,
		OutputPlans:     outputPlans,
		BtcInputs:       btcInputs,
		BtcOutputs:      btcOutputs,
		EphemeralAnchor: ephemeralPlan,
		AnchorOutputs:   cloneTxOuts(b.anchorOutputs),
	}

	return plan, nil
}

// Commit invokes CommitVirtualPsbts and stores anchor metadata.
//
//nolint:funlen
func (b *AssetTxBuilder) Commit(ctx context.Context, wallet assetWalletClient,
	opts CommitOptions) error {

	if b.vpacket == nil {
		return errors.New("compile must be called before commit")
	}

	skipWalletFunding := opts.SkipWalletFunding || b.hasEphemeralAnchor
	if skipWalletFunding && opts.FeeRate > 0 {
		return errors.New("fee rate must be zero when skipping " +
			"wallet funding")
	}

	if skipWalletFunding && !opts.SkipZeroFeeBalance {
		if err := b.ensureZeroFeeAnchor(); err != nil {
			return err
		}
	}

	encodedVpkt, err := tappsbt.Encode(b.vpacket)
	if err != nil {
		return fmt.Errorf("encode vpacket: %w", err)
	}

	anchorPkt, err := tapsend.PrepareAnchoringTemplate(
		[]*tappsbt.VPacket{b.vpacket},
	)
	if err != nil {
		return fmt.Errorf("prepare anchoring template: %w", err)
	}

	for idx, spec := range b.inputs {
		if len(spec.proof) == 0 {
			continue
		}

		lastProof := spec.proof[len(spec.proof)-1]
		if lastProof == nil ||
			lastProof.InclusionProof.InternalKey == nil {

			continue
		}

		internalKey := lastProof.InclusionProof.InternalKey
		xOnlyInternalKey := schnorr.SerializePubKey(internalKey)
		anchorPkt.Inputs[idx].TaprootBip32Derivation =
			[]*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: xOnlyInternalKey,
				},
			}

		anchorPkt.Inputs[idx].Bip32Derivation =
			[]*psbt.Bip32Derivation{
				{
					PubKey: internalKey.
						SerializeCompressed(),
				},
			}
	}

	if len(b.btcInputs) > 0 {
		for _, spec := range b.btcInputs {
			anchorPkt.UnsignedTx.TxIn = append(
				anchorPkt.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: spec.plan.Outpoint,
					Sequence:         spec.plan.Sequence,
				},
			)

			input := psbt.PInput{
				WitnessUtxo: cloneTxOut(spec.plan.WitnessUtxo),
				SighashType: spec.sighashType,
			}

			if len(spec.taprootLeafScript) > 0 {
				input.TaprootLeafScript =
					cloneTaprootLeafScripts(
						spec.taprootLeafScript,
					)
			}
			if len(spec.taprootBip32Derivation) > 0 {
				input.TaprootBip32Derivation =
					cloneTaprootBip32(
						spec.taprootBip32Derivation,
					)
			}
			if len(spec.bip32Derivation) > 0 {
				input.Bip32Derivation = cloneBip32(
					spec.bip32Derivation,
				)
			}

			anchorPkt.Inputs = append(anchorPkt.Inputs, input)
		}
	}

	// Attach any pre-leased UTXOs to the anchor template so the wallet
	// reuses them when committing the virtual transaction.
	for _, lease := range opts.Leases {
		anchorPkt.UnsignedTx.TxIn = append(
			anchorPkt.UnsignedTx.TxIn, &wire.TxIn{
				PreviousOutPoint: lease.Outpoint,
			},
		)
		anchorPkt.Inputs = append(
			anchorPkt.Inputs, psbt.PInput{
				WitnessUtxo: wire.NewTxOut(
					int64(lease.Value), lease.PkScript,
				),
			},
		)
	}

	// Apply custom asset output values before serializing if specified.
	// This ensures proofs generated by tapd reference the correct txid.
	for idx, value := range opts.AssetOutputValues {
		if int(idx) < len(anchorPkt.UnsignedTx.TxOut) {
			anchorPkt.UnsignedTx.TxOut[idx].Value = value
		}
	}

	// Append ephemeral P2A anchor to the template BEFORE sending to tapd.
	// This ensures proofs reference the correct txid including all outputs.
	if b.hasEphemeralAnchor {
		txOut := wire.NewTxOut(0, payToAnchorPkScript())

		anchorPkt.UnsignedTx.TxOut = append(
			anchorPkt.UnsignedTx.TxOut, txOut,
		)

		anchorPkt.Outputs = append(anchorPkt.Outputs, psbt.POutput{})

		if b.ephemeralAnchorPlan != nil {
			index := len(anchorPkt.UnsignedTx.TxOut) - 1
			b.ephemeralAnchorPlan.OutputIndex = index
		}
	}

	// Include BTC-only outputs so tapd commits/proofs reference the final
	// txid. These outputs carry no asset state. We insert them BEFORE
	// CommitVirtualPsbts so exclusion proofs see the final output set and
	// txid, avoiding the “missing internal key” issue tapd hits when a
	// taproot output is appended after commit.
	if len(b.btcOutputs) > 0 {
		for i, spec := range b.btcOutputs {
			txOut := wire.NewTxOut(
				spec.plan.ValueSat,
				append([]byte(nil), spec.plan.PkScript...),
			)

			anchorPkt.UnsignedTx.TxOut = append(
				anchorPkt.UnsignedTx.TxOut, txOut,
			)
			anchorPkt.Outputs = append(
				anchorPkt.Outputs, psbt.POutput{},
			)

			index := len(anchorPkt.UnsignedTx.TxOut) - 1
			if i < len(b.btcOutputPlans) {
				b.btcOutputPlans[i].OutputIndex = index
			}
			if i < len(b.btcOutputs) {
				b.btcOutputs[i].plan.OutputIndex = index
			}
		}
	}

	var anchorBuf bytes.Buffer
	if err := anchorPkt.Serialize(&anchorBuf); err != nil {
		return fmt.Errorf("serialize anchor template: %w", err)
	}

	commitReq := &assetwalletrpc.CommitVirtualPsbtsRequest{
		AnchorPsbt:   anchorBuf.Bytes(),
		VirtualPsbts: [][]byte{encodedVpkt},
	}

	// Set up the change output handling based on options.
	switch {
	case opts.NoChangeOutput:
		// Caller explicitly does not want a change output (tx is
		// balanced or excess goes to fee). Point to a non-asset output
		// (like the ephemeral anchor) to prevent tapd from adding a new
		// change output. If there's an ephemeral anchor output, use
		// that as the "change".
		if b.ephemeralAnchorPlan != nil {
			commitReq.AnchorChangeOutput = &assetwalletrpc.
				CommitVirtualPsbtsRequest_ExistingOutputIndex{
				ExistingOutputIndex: int32(
					b.ephemeralAnchorPlan.OutputIndex,
				),
			}
		}
		// If no BTC anchor exists, leave AnchorChangeOutput nil/unset.

	case opts.ChangeOutput != nil:
		params := b.params
		if params == nil || params.Params == nil {
			return errors.New("chain params not configured for " +
				"change output")
		}

		index, err := addP2WPKHOutputWithIndex(
			anchorPkt, *opts.ChangeOutput, btcutil.Amount(1),
			params.Params,
		)
		if err != nil {
			return fmt.Errorf("add change output: %w", err)
		}

		commitReq.AnchorChangeOutput = &assetwalletrpc.
			CommitVirtualPsbtsRequest_ExistingOutputIndex{
			ExistingOutputIndex: int32(index),
		}

	default:
		// Default: let tapd add a change output automatically.
		commitReq.AnchorChangeOutput = &assetwalletrpc.
			CommitVirtualPsbtsRequest_Add{
			Add: true,
		}
	}

	if !skipWalletFunding && opts.FeeRate > 0 {
		commitReq.Fees =
			&assetwalletrpc.CommitVirtualPsbtsRequest_SatPerVbyte{
				SatPerVbyte: uint64(opts.FeeRate),
			}
	} else {
		commitReq.SkipFunding = true
	}

	if opts.LockID != nil {
		lockID := *opts.LockID
		commitReq.CustomLockId = lockID[:]
	}

	if opts.LockDuration > 0 {
		commitReq.LockExpirationSeconds = uint64(
			opts.LockDuration.Seconds(),
		)
	}

	resp, err := wallet.CommitVirtualPsbts(ctx, commitReq)
	if err != nil {
		return fmt.Errorf("commit virtual psbts: %w", err)
	}

	// The response gives us the funded anchor PSBT together with the set of
	// vpackets that tapd will accept for publication. We keep a copy of
	// both so later edits (witness population, proof export) always operate
	// on the latest state.
	anchorPsbt, err := psbt.NewFromRawBytes(
		bytes.NewReader(resp.AnchorPsbt), false,
	)
	if err != nil {
		return fmt.Errorf("decode anchor psbt: %w", err)
	}
	anchorPsbt.UnsignedTx.Version = 3
	for i := range anchorPsbt.UnsignedTx.TxIn {
		// Check if the input has a custom sequence (for CSV spends).
		// Otherwise use the default TRUC sequence.
		if i < len(b.inputs) && b.inputs[i].cfg.Sequence != 0 {
			anchorPsbt.UnsignedTx.TxIn[i].Sequence =
				b.inputs[i].cfg.Sequence
		} else {
			anchorPsbt.UnsignedTx.TxIn[i].Sequence =
				wire.MaxTxInSequenceNum - 2
		}
	}
	b.anchorOutputs = cloneTxOuts(anchorPsbt.UnsignedTx.TxOut)

	newActive := make([]*tappsbt.VPacket, len(resp.VirtualPsbts))
	for i := range resp.VirtualPsbts {
		pkt, err := tappsbt.Decode(resp.VirtualPsbts[i])
		if err != nil {
			return fmt.Errorf("decode committed vpacket %d: %w", i,
				err)
		}
		newActive[i] = pkt
	}

	newPassive := make([]*tappsbt.VPacket, len(resp.PassiveAssetPsbts))
	for i := range resp.PassiveAssetPsbts {
		pkt, err := tappsbt.Decode(resp.PassiveAssetPsbts[i])
		if err != nil {
			return fmt.Errorf("decode committed passive vpacket "+
				"%d: %w", i, err)
		}
		newPassive[i] = pkt
	}

	b.commitResp = resp
	b.anchorPsbt = anchorPsbt
	b.activePkts = newActive
	b.passivePkts = newPassive
	// Note: We no longer set skipWalletFinalize based on skipWalletFunding.
	// SkipWalletFunding only affects whether we add wallet UTXOs during
	// Commit. We still need LND to sign any tapd-managed inputs in
	// FinalizeAnchor. The FinalizeAnchor method checks if all inputs are
	// already finalized and skips wallet signing in that case.

	if len(newActive) > 0 {
		b.vpacket = newActive[0]

		// Ensure previously prepared witnesses are re-applied to the
		// updated virtual packet representation returned by tapd.
		for index, witness := range b.scriptWitnesses {
			if index < len(b.vpacket.Outputs) &&
				b.vpacket.Outputs[index].Asset != nil {

				asset := b.vpacket.Outputs[index].Asset
				err = asset.UpdateTxWitness(
					0, witness,
				)
				if err != nil {
					return fmt.Errorf("re-applying output "+
						"%d witness failed: %w", index,
						err)
				}
			}
		}
	}

	return nil
}

func addP2WPKHOutputWithIndex(packet *psbt.Packet,
	keyDesc keychain.KeyDescriptor, amount btcutil.Amount,
	params *chaincfg.Params) (int, error) {

	derivation, _, _ := btcwallet.Bip32DerivationFromKeyDesc(
		keyDesc, params.HDCoinType,
	)

	pubKeyBytes := keyDesc.PubKey.SerializeCompressed()
	pubKeyHash := btcutil.Hash160(pubKeyBytes)
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, params)
	if err != nil {
		return 0, err
	}

	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return 0, err
	}

	packet.UnsignedTx.AddTxOut(wire.NewTxOut(int64(amount), script))
	packet.Outputs = append(packet.Outputs, psbt.POutput{
		Bip32Derivation: []*psbt.Bip32Derivation{derivation},
	})

	return len(packet.UnsignedTx.TxOut) - 1, nil
}

// SetScriptWitness injects the tapscript witness for an output.
func (b *AssetTxBuilder) SetScriptWitness(outputIndex int,
	witness wire.TxWitness) error {

	if len(b.activePkts) == 0 {
		return errors.New("no active packets available")
	}
	if outputIndex < 0 || outputIndex >= len(b.activePkts[0].Outputs) {
		return fmt.Errorf("output index %d out of range", outputIndex)
	}
	if len(witness) == 0 {
		return nil
	}

	out := b.activePkts[0].Outputs[outputIndex]
	if out.Asset == nil {
		return errors.New("output asset missing")
	}
	if err := out.Asset.UpdateTxWitness(0, witness); err != nil {
		return fmt.Errorf("update asset witness: %w", err)
	}

	b.scriptWitnesses[outputIndex] = witness
	if outputIndex < len(b.plans) {
		b.plans[outputIndex].Witness.Witness = witness
	}

	return nil
}

// ApplyKeySpendSignature records an aggregated MuSig2 key-spend signature for
// the given anchor input.
func (b *AssetTxBuilder) ApplyKeySpendSignature(inputIndex int,
	sig []byte) error {

	if len(sig) == 0 {
		return errors.New("signature missing")
	}

	if b.anchorPsbt != nil {
		if inputIndex < 0 || inputIndex >= len(b.anchorPsbt.Inputs) {
			return fmt.Errorf("input index %d out of range",
				inputIndex)
		}

		// Store the aggregated MuSig2 key-spend signature so the wallet
		// can finalize the PSBT. Taproot key spends are satisfied
		// entirely by the signature (unlike script-path spends there is
		// no witness stack to craft) so we populate TaprootKeySpendSig
		// and let FinalizePsbt build the final witness.
		b.anchorPsbt.Inputs[inputIndex].TaprootKeySpendSig = sig
	}

	return nil
}

// GetKeySpendSigHash returns the BIP-341 key-path signature digest for the
// specified anchor input. This is the value MuSig2 signing sessions consume for
// cooperative key spends. Script-path spends require a different hash that
// commits to the tapscript and control block, so this helper should only be
// used for key-spend transitions.
func (b *AssetTxBuilder) GetKeySpendSigHash(inputIndex int) ([32]byte, error) {
	if b.anchorPsbt == nil {
		return [32]byte{}, errors.New("commit must be called before " +
			"digest")
	}

	return GetSigHash(b.anchorPsbt, inputIndex)
}

// GetTaprootRoots returns the script tree root, asset commitment root and
// combined taproot tweak for the specified input/closure pair. The combined
// root matches the tweak committed into the anchor output key and is what
// MuSig2 signers must feed into their aggregation flow.
func (b *AssetTxBuilder) GetTaprootRoots(inputIndex int,
	closureID string) ([]byte, []byte, []byte, error) {

	plan, err := b.scriptPlanForInput(inputIndex, closureID)
	if err != nil {
		return nil, nil, nil, err
	}

	scriptRoot := make([]byte, len(plan.scriptRoot))
	copy(scriptRoot, plan.scriptRoot[:])

	assetRoot := make([]byte, len(plan.assetRoot))
	copy(assetRoot, plan.assetRoot[:])

	taprootRoot := make([]byte, len(plan.taprootRoot))
	copy(taprootRoot, plan.taprootRoot[:])

	return scriptRoot, assetRoot, taprootRoot, nil
}

func (b *AssetTxBuilder) scriptPlanForInput(inputIndex int,
	closureID string) (*scriptSpendPlan, error) {

	if inputIndex < 0 || inputIndex >= len(b.inputs) {
		return nil, fmt.Errorf("input index %d not available",
			inputIndex)
	}

	spec := &b.inputs[inputIndex]
	if len(spec.cfg.Closures) == 0 {
		return nil, errors.New("script spend requires at least one " +
			"closure")
	}

	if spec.scriptPlans == nil {
		return nil, errors.New("script spend plans not available")
	}

	if closureID != "" {
		plan, ok := spec.scriptPlans[closureID]
		if !ok {
			return nil, fmt.Errorf("closure %q not found",
				closureID)
		}

		return plan, nil
	}

	for _, closure := range spec.cfg.Closures {
		if plan, ok := spec.scriptPlans[closure.ID]; ok {
			return plan, nil
		}
	}

	return nil, errors.New("no script plan found for default closure")
}

// PrepareScriptSpend gathers the information required to produce a tapscript
// signature for a script-path spend of the specified anchor input using the
// closure identified by closureID. If closureID is empty, the first closure is
// used.
func (b *AssetTxBuilder) PrepareScriptSpend(inputIndex int,
	closureID string) (*ScriptSpendDetails, error) {

	if b.anchorPsbt == nil {
		return nil, errors.New("commit must be called before script " +
			"spend")
	}

	if inputIndex < 0 || inputIndex >= len(b.anchorPsbt.Inputs) {
		return nil, fmt.Errorf("input index %d out of range",
			inputIndex)
	}

	if inputIndex >= len(b.inputs) {
		return nil, fmt.Errorf("input index %d not available",
			inputIndex)
	}

	if b.inputs[inputIndex].scriptPlans == nil {
		return nil, errors.New("script plans unavailable: compile " +
			"must be called")
	}

	plan, err := b.scriptPlanForInput(inputIndex, closureID)
	if err != nil {
		return nil, err
	}

	tapLeaf := plan.tapLeaf

	// BIP341 requires all previous outputs to be included in the sighash
	// computation, not just the one being signed. We need to create a
	// MultiPrevOutFetcher that provides information for ALL inputs.
	prevOuts := make(map[wire.OutPoint]*wire.TxOut)
	for i := range b.anchorPsbt.UnsignedTx.TxIn {
		txIn := b.anchorPsbt.UnsignedTx.TxIn[i]
		psbtIn := b.anchorPsbt.Inputs[i]

		var utxo *wire.TxOut
		switch {
		case psbtIn.WitnessUtxo != nil:
			utxo = psbtIn.WitnessUtxo

		case psbtIn.NonWitnessUtxo != nil:
			prevIdx := txIn.PreviousOutPoint.Index
			if int(prevIdx) >= len(psbtIn.NonWitnessUtxo.TxOut) {
				return nil, fmt.Errorf("non-witness utxo "+
					"missing output %d for input %d",
					prevIdx, i)
			}

			utxo = psbtIn.NonWitnessUtxo.TxOut[prevIdx]

		default:
			return nil, fmt.Errorf("no utxo information for input "+
				"%d", i)
		}

		prevOuts[txIn.PreviousOutPoint] = utxo
	}

	prevFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)

	sigHashes := txscript.NewTxSigHashes(
		b.anchorPsbt.UnsignedTx, prevFetcher,
	)

	digest, err := txscript.CalcTapscriptSignaturehash(
		sigHashes, txscript.SigHashDefault, b.anchorPsbt.UnsignedTx,
		inputIndex, prevFetcher, tapLeaf,
	)
	if err != nil {
		return nil, fmt.Errorf("calc tapscript sighash: %w", err)
	}
	if len(digest) != 32 {
		return nil, fmt.Errorf("unexpected sighash length %d",
			len(digest))
	}

	inputCfg := b.inputs[inputIndex].cfg
	if inputCfg.AnchorKey.Mode != AnchorKeyModeStatic {
		return nil, errors.New("script spend requires static anchor " +
			"key")
	}

	var sigHash [32]byte
	copy(sigHash[:], digest)

	assetRootCopy := make([]byte, len(plan.assetRoot))
	copy(assetRootCopy, plan.assetRoot[:])

	scriptRootCopy := make([]byte, len(plan.scriptRoot))
	copy(scriptRootCopy, plan.scriptRoot[:])

	taprootRootCopy := make([]byte, len(plan.taprootRoot))
	copy(taprootRootCopy, plan.taprootRoot[:])

	controlBlockCopy := append([]byte(nil), plan.controlBlock...)

	return &ScriptSpendDetails{
		InputIndex:   inputIndex,
		ClosureID:    plan.closure.ID,
		Closure:      plan.closure,
		SigHash:      sigHash,
		TapLeaf:      tapLeaf,
		InternalKey:  plan.internalKey,
		ControlBlock: controlBlockCopy,
		OutputKey:    plan.outputKey,
		AssetRoot:    assetRootCopy,
		ScriptRoot:   scriptRootCopy,
		TaprootRoot:  taprootRootCopy,
	}, nil
}

// ApplyScriptSpend constructs and records the tapscript witness for the
// provided script spend details using the supplied signature map.
func (b *AssetTxBuilder) ApplyScriptSpend(details *ScriptSpendDetails,
	signatures map[string][]byte) error {

	if details == nil {
		return errors.New("script spend details missing")
	}
	if signatures == nil {
		return errors.New("script spend signatures missing")
	}

	plan, err := b.scriptPlanForInput(details.InputIndex, details.ClosureID)
	if err != nil {
		return err
	}

	// The closure either wraps an arklib witness helper or a bespoke
	// witness function supplied by the caller. In both cases the control
	// block we cached during Compile() already contains the concatenated
	// inclusion proof, so the caller only has to provide signatures.
	witness, err := plan.closure.Witness(plan.controlBlock, signatures)
	if err != nil {
		return fmt.Errorf("assemble tapscript witness: %w", err)
	}

	return b.ApplyScriptPathWitness(details.InputIndex, witness)
}

// ApplyScriptPathWitness records a fully constructed tapscript witness for the
// specified anchor input. The witness is set on the PSBT's FinalScriptWitness
// immediately, marking this input as finalized.
func (b *AssetTxBuilder) ApplyScriptPathWitness(inputIndex int,
	witness wire.TxWitness) error {

	if b.anchorPsbt == nil {
		return errors.New("commit must be called before witness")
	}
	if inputIndex < 0 || inputIndex >= len(b.anchorPsbt.Inputs) {
		return fmt.Errorf("input index %d out of range", inputIndex)
	}
	if len(witness) == 0 {
		return errors.New("witness stack empty")
	}

	psbtInput := &b.anchorPsbt.Inputs[inputIndex]
	var witnessBuf bytes.Buffer
	if err := psbt.WriteTxWitness(&witnessBuf, witness); err != nil {
		return fmt.Errorf("serialize script witness: %w", err)
	}
	psbtInput.FinalScriptWitness = witnessBuf.Bytes()
	psbtInput.TaprootKeySpendSig = nil
	psbtInput.TaprootScriptSpendSig = nil
	psbtInput.TaprootLeafScript = nil

	b.anchorWitnesses[inputIndex] = append(
		wire.TxWitness(nil), witness...,
	)

	return nil
}

// FinalizeAnchor finalises the anchor PSBT using the operator wallet.
// Inputs that already have FinalScriptWitness set (via ApplyScriptPathWitness)
// are preserved. For other inputs, the wallet is used for signing and
// finalization.
func (b *AssetTxBuilder) FinalizeAnchor(ctx context.Context,
	wallet WalletClient) (*psbt.Packet, error) {

	if b.anchorPsbt == nil {
		return nil, errors.New("commit must be called before finalize")
	}

	finalized := b.anchorPsbt
	if !b.skipWalletFinalize {
		// Check if all inputs are already finalized (have
		// FinalScriptWitness). If so, skip wallet finalization
		// entirely. This handles the case where all inputs are script
		// path spends that we've already finalized via
		// ApplyScriptPathWitness.
		allFinalized := true
		for _, input := range b.anchorPsbt.Inputs {
			if len(input.FinalScriptWitness) == 0 {
				allFinalized = false
				break
			}
		}

		if !allFinalized {
			signed, err := wallet.SignPsbt(ctx, b.anchorPsbt)
			if err != nil {
				return nil, fmt.Errorf("sign anchor psbt: %w",
					err)
			}

			finalizedPacket, _, err := wallet.FinalizePsbt(
				ctx, signed, "",
			)
			if err != nil {
				return nil, fmt.Errorf(
					"finalize anchor psbt: %w", err,
				)
			}
			finalized = finalizedPacket
		}
	}

	// Re-apply custom witnesses from anchorWitnesses. This ensures our
	// witnesses are preserved even if wallet finalization overwrote them.
	for idx, witness := range b.anchorWitnesses {
		if idx >= len(finalized.Inputs) || len(witness) == 0 {
			continue
		}

		var witnessBuf bytes.Buffer
		err := psbt.WriteTxWitness(&witnessBuf, witness)
		if err != nil {
			return nil, fmt.Errorf("serialize anchor witness: %w",
				err)
		}

		input := &finalized.Inputs[idx]
		input.FinalScriptWitness = witnessBuf.Bytes()
		input.TaprootKeySpendSig = nil
		input.TaprootScriptSpendSig = nil

		if idx < len(finalized.UnsignedTx.TxIn) {
			finalized.UnsignedTx.TxIn[idx].Witness = witness
		}
	}

	b.anchorPsbt = finalized

	return finalized, nil
}

// Publish executes PublishAndLogTransfer with the prepared artefacts.
func (b *AssetTxBuilder) Publish(ctx context.Context, wallet assetWalletClient,
	label string, opts PublishOptions) (*taprpc.SendAssetResponse, error) {

	if b.anchorPsbt == nil || b.commitResp == nil {
		return nil, errors.New("commit/finalize must be completed " +
			"before publish")
	}

	var anchorBuf bytes.Buffer
	if err := b.anchorPsbt.Serialize(&anchorBuf); err != nil {
		return nil, fmt.Errorf("serialize anchor psbt: %w", err)
	}

	virtualBytes := make([][]byte, len(b.activePkts))
	for i := range b.activePkts {
		encoded, err := tappsbt.Encode(b.activePkts[i])
		if err != nil {
			return nil, fmt.Errorf("encode active vpacket %d: %w",
				i, err)
		}
		virtualBytes[i] = encoded
	}

	passiveBytes := make([][]byte, len(b.passivePkts))
	for i := range b.passivePkts {
		encoded, err := tappsbt.Encode(b.passivePkts[i])
		if err != nil {
			return nil, fmt.Errorf("encode passive vpacket %d: %w",
				i, err)
		}
		passiveBytes[i] = encoded
	}

	pubReq := &assetwalletrpc.PublishAndLogRequest{
		AnchorPsbt:            anchorBuf.Bytes(),
		VirtualPsbts:          virtualBytes,
		PassiveAssetPsbts:     passiveBytes,
		ChangeOutputIndex:     b.commitResp.GetChangeOutputIndex(),
		LndLockedUtxos:        b.commitResp.GetLndLockedUtxos(),
		SkipAnchorTxBroadcast: opts.SkipBroadcast,
		Label:                 label,
	}

	resp, err := wallet.PublishAndLogTransfer(ctx, pubReq)
	if err != nil {
		return nil, fmt.Errorf("publish transfer: %w", err)
	}

	return resp, nil
}

// AnchorPsbt returns the current anchor PSBT managed by the builder.
func (b *AssetTxBuilder) AnchorPsbt() *psbt.Packet {
	return b.anchorPsbt
}

// SetAnchorPsbt replaces the builder's anchor PSBT with the provided packet.
// This is useful for injecting a pre-signed PSBT (e.g., when wallet signing
// must be performed manually before FinalizeAnchor applies custom witnesses).
func (b *AssetTxBuilder) SetAnchorPsbt(pkt *psbt.Packet) {
	b.anchorPsbt = pkt
}

// SetInputOutpoint overrides the PrevID.OutPoint for the specified input in
// the virtual packet. This must be called after Compile() and before Commit()
// to ensure the proofs generated by tapd reference the correct parent outpoint.
//
// This is essential for tree structures where the parent node's tx hash isn't
// known until signing, but proofs need to reference the actual parent outpoint
// for universe lookups to work correctly.
//
// After updating the input outpoint, this function re-runs PrepareOutputAssets
// to regenerate the output assets with the correct PrevWitnesses[].PrevID.
func (b *AssetTxBuilder) SetInputOutpoint(ctx context.Context, inputIdx int,
	outpoint wire.OutPoint) error {

	if b.vpacket == nil {
		return errors.New("vpacket not initialized; call Compile first")
	}

	if inputIdx < 0 || inputIdx >= len(b.vpacket.Inputs) {
		return fmt.Errorf("input index %d out of range [0, %d)",
			inputIdx, len(b.vpacket.Inputs))
	}

	b.vpacket.Inputs[inputIdx].PrevID.OutPoint = outpoint

	// Re-run PrepareOutputAssets to regenerate output assets with the
	// updated input outpoint. The output assets' PrevWitnesses[].PrevID
	// are derived from the vpacket input's PrevID.
	if err := tapsend.PrepareOutputAssets(ctx, b.vpacket); err != nil {
		return fmt.Errorf("re-prepare output assets: %w", err)
	}

	return nil
}

// EphemeralAnchor returns a copy of the ephemeral anchor plan if one was
// configured, or nil otherwise.
func (b *AssetTxBuilder) EphemeralAnchor() *EphemeralAnchorPlan {
	if b.ephemeralAnchorPlan == nil {
		return nil
	}

	return &EphemeralAnchorPlan{
		OutputIndex: b.ephemeralAnchorPlan.OutputIndex,
	}
}

// BtcInputs returns a copy of the BTC-only anchor input plans configured on
// the builder.
func (b *AssetTxBuilder) BtcInputs() []BtcInputPlan {
	inputs := make([]BtcInputPlan, len(b.btcInputPlans))
	for i, plan := range b.btcInputPlans {
		inputs[i] = cloneBtcInputPlan(plan)
	}

	return inputs
}

// BtcOutputs returns a copy of the BTC-only anchor output plans recorded on
// the builder. Output indices are populated after Commit().
func (b *AssetTxBuilder) BtcOutputs() []BtcOutputPlan {
	outputs := make([]BtcOutputPlan, len(b.btcOutputPlans))
	for i, plan := range b.btcOutputPlans {
		outputs[i] = cloneBtcOutputPlan(plan)
	}

	return outputs
}

// TapdKeyQuerier abstracts the tapd RPC needed to look up key derivation info.
type TapdKeyQuerier interface {
	QueryInternalKey(ctx context.Context, internalKey []byte) (
		keychain.KeyDescriptor, error)
}

// PopulateTapdKeyInfo queries tapd for key derivation information and populates
// the anchor PSBT inputs with BIP32 derivation paths. This enables LND's wallet
// to sign inputs for keys that tapd manages.
//
// This method must be called after Commit() and before FinalizeAnchor() for any
// inputs that use AnchorKeyModeTapdManaged. The method only processes inputs
// where AnchorKey.Mode == AnchorKeyModeTapdManaged; other inputs are left
// unchanged.
//
// The coinType parameter should match the network (0 for mainnet, 1 for
// testnet/regtest). This is used to construct the BIP32 derivation path.
func (b *AssetTxBuilder) PopulateTapdKeyInfo(ctx context.Context,
	querier TapdKeyQuerier, coinType uint32) error {

	if b.anchorPsbt == nil {
		return errors.New(
			"anchor PSBT not available; call Commit first",
		)
	}

	for idx, inputSpec := range b.inputs {
		// Only process tapd-managed keys.
		if inputSpec.cfg.AnchorKey.Mode != AnchorKeyModeTapdManaged {
			continue
		}

		// Get the internal key from the input config.
		internalKeyBytes := inputSpec.cfg.AnchorKey.Key
		if len(internalKeyBytes) == 0 {
			return fmt.Errorf("input %d: tapd-managed key missing "+
				"internal key bytes", idx)
		}

		// Query tapd for the key descriptor.
		keyDesc, err := querier.QueryInternalKey(ctx, internalKeyBytes)
		if err != nil {
			return fmt.Errorf("input %d: query internal key: %w",
				idx, err)
		}

		// Build BIP32 derivation info using the key descriptor.
		bip32Deriv, taprootBip32Deriv, _ :=
			btcwallet.Bip32DerivationFromKeyDesc(keyDesc, coinType)

		// Populate the PSBT input with derivation info so LND can sign.
		if idx < len(b.anchorPsbt.Inputs) {
			b.anchorPsbt.Inputs[idx].Bip32Derivation =
				[]*psbt.Bip32Derivation{bip32Deriv}

			b.anchorPsbt.Inputs[idx].TaprootBip32Derivation =
				[]*psbt.TaprootBip32Derivation{
					taprootBip32Deriv,
				}
		}
	}

	return nil
}

// Dependencies kept abstract for easier unit testing.
//
//nolint:iface
type assetWalletClient interface {
	CommitVirtualPsbts(context.Context,
		*assetwalletrpc.CommitVirtualPsbtsRequest, ...grpc.CallOption) (
		*assetwalletrpc.CommitVirtualPsbtsResponse, error)

	PublishAndLogTransfer(context.Context,
		*assetwalletrpc.PublishAndLogRequest, ...grpc.CallOption) (
		*taprpc.SendAssetResponse, error)
}

type WalletClient interface {
	SignPsbt(ctx context.Context, packet *psbt.Packet) (*psbt.Packet, error)

	FinalizePsbt(ctx context.Context, packet *psbt.Packet, account string) (
		*psbt.Packet, *wire.MsgTx, error)
}
