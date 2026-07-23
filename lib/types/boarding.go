package types

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// VTXOOwnerKeyFamily is the key family used for long-lived VTXO owner
	// keys. Owner keys are committed into the VTXO policy and must remain
	// stable across refreshes.
	VTXOOwnerKeyFamily keychain.KeyFamily = 44

	// VTXOSigningKeyFamily is the key family used for per-round VTXO
	// MuSig2 signing keys. It is intentionally distinct from LND's
	// internal multisig family and from the server operator family so a
	// client sharing an LND signer with the server cannot derive the
	// operator key as its first VTXO signing key.
	VTXOSigningKeyFamily keychain.KeyFamily = 45
)

// OperatorTerms holds the information that the operator will share with
// clients. It communicates the server's terms to the client.
type OperatorTerms struct {
	// PubKey is the operator's main public key. This should be used for
	// constructing boarding scripts.
	PubKey *btcec.PublicKey

	// BoardingExitDelay is the minimum CSV delay to use for boarding
	// outputs that the operator expects.
	BoardingExitDelay uint32

	// VTXOExitDelay is the minimum CSV delay to use for VTXO outputs. This
	// delay will give the server time to respond to unilateral spends of
	// a VTXO that has been forfeit or spent.
	VTXOExitDelay uint32

	// DustLimit enforces minimum output value for boarding/funding flows.
	DustLimit btcutil.Amount

	// MinVTXOAmount is the operator-advertised minimum VTXO output
	// amount.
	MinVTXOAmount btcutil.Amount

	// MinBoardingAmount is the minimum amount clients must contribute.
	MinBoardingAmount btcutil.Amount

	// MaxVTXOAmount caps the amount accepted per VTXO (optional). The
	// operator applies the same cap to boarding requests, round outputs
	// and OOR recipient outputs.
	MaxVTXOAmount btcutil.Amount

	// MaxUserBalance caps the total balance a single user should hold
	// in the system (optional). The cap is enforced client-side on
	// receive and boarding flows before funds enter the system. Zero
	// means no cap.
	MaxUserBalance btcutil.Amount

	// FeeRate reflects the operator's target package feerate (sat/vByte).
	FeeRate btcutil.Amount

	// MinOperatorFee is the minimum fee (satoshis) the operator
	// requires per join request. The fee is the difference between
	// total input value and total output value.
	MinOperatorFee btcutil.Amount

	// FreeRefreshWindowBlocks is the number of blocks before batch expiry
	// in which a pure refresh qualifies for a complete fee waiver. Zero
	// disables the policy.
	FreeRefreshWindowBlocks uint32

	// MinConfirmations is the minimum confs required on boarding inputs.
	MinConfirmations uint32

	// MaxOORLineageVBytes is the operator-published cap on the
	// cumulative on-chain virtual bytes a recipient must publish to
	// claim a VTXO produced by an OOR submit unilaterally. Clients
	// mirror this cap when selecting OOR inputs so a submit that the
	// operator would reject is rejected pre-network. Zero means the
	// operator does not enforce a cap (the client should fall back to
	// its own conservative default before submitting).
	MaxOORLineageVBytes uint32
}

// MinVTXOAmountFloor returns the effective minimum for a VTXO output. The
// advertised VTXO minimum is authoritative when it is above dust, but dust
// remains the floor for older or misconfigured operator snapshots.
func (t *OperatorTerms) MinVTXOAmountFloor() btcutil.Amount {
	if t == nil {
		return 0
	}

	if t.MinVTXOAmount < t.DustLimit {
		return t.DustLimit
	}

	return t.MinVTXOAmount
}

// JoinRoundRequest represents a participant's request to join a round.
type JoinRoundRequest struct {
	// Identifier is the participant's public key identifier associated with
	// this request.
	Identifier *btcec.PublicKey

	// VTXOReqs specifies the new VTXOs the client wants to receive.
	VTXOReqs []*VTXORequest

	// BoardingReqs specifies the boarding UTXOs the client wants to use
	// to board the Ark.
	BoardingReqs []*BoardingRequest

	// LeaveReqs specifies the requests to leave the Ark with on-chain
	// UTXOs.
	LeaveReqs []*LeaveRequest

	// ForfeitReqs specifies the requests to forfeit VTXOs.
	ForfeitReqs []*ForfeitRequest

	// Auth contains the BIP-322 payload that authorizes this join
	// request.
	Auth *JoinRoundAuth
}

// LeaveRequest represents a request to leave the Ark with an on-chain UTXO.
//
// Under the #270 seal-time fee handshake `Output.Value` is the
// client's target amount in satoshis. When `IsChange` is true the
// server overrides the value with the residual
// (`Σin − Σ(fixed outputs) − fee`) at seal time; exactly one
// output across the intent's VTXORequests + LeaveRequests list may
// have IsChange=true.
type LeaveRequest struct {
	// Output is the output that will be created to return funds to the
	// client when leaving the Ark. Its Value is the client's target
	// amount; server-filled when IsChange=true.
	Output *wire.TxOut

	// IsChange marks this LeaveRequest as the client's designated
	// fee-bearing change output for the intent.
	IsChange bool
}

// ForfeitRequest represents a request to forfeit a VTXO.
type ForfeitRequest struct {
	// VTXOOutpoint is the outpoint of the VTXO to forfeit.
	VTXOOutpoint *wire.OutPoint

	// Amount is the local value of the forfeited VTXO in satoshis. This
	// is used by the client when validating a round before registration.
	// It is not part of the join-round wire encoding, where the outpoint
	// remains the source of truth.
	Amount btcutil.Amount

	// AuthSpend is the unilateral proof/auth spend path used for join-auth
	// when settling a custom-script output into a round. Standard wallet
	// VTXOs leave this nil and let the operator load the canonical
	// path from the VTXO registry. Custom VTXOs serialize it onto the
	// join-round wire so the operator can validate the caller-provided
	// path.
	AuthSpend *arkscript.SpendPath

	// ForfeitSpend is the operator-backed spend path used locally
	// to build the actual round forfeit transaction for a
	// custom-script output. Standard wallet VTXOs leave this nil and
	// let the operator derive the path from the registered VTXO
	// descriptor. Custom
	// VTXOs serialize it onto the join-round wire so the operator can build
	// the exact connector-bound forfeit request later.
	ForfeitSpend *arkscript.SpendPath
}

// VTXOOrigin classifies how a locally-owned round VTXO came into
// existence. The classification is decided at wallet intent-
// composition time because only the wallet knows whether a VTXO
// is funded by a boarding input, a forfeited VTXO, or a remote
// participant's directed send. It is used downstream by the
// round actor to route a correctly-classified VTXOReceivedMsg
// to the ledger actor (boarding vs refresh vs participant
// transfer book to different ledger account pairs).
//
// The field is local-only and is NOT serialized on the
// join-round wire; remote peers never see it.
type VTXOOrigin uint8

const (
	// VTXOOriginUnknown is the zero value. Used for requests
	// whose origin has not been set (e.g. remote recipient
	// outputs on a directed send where HasLocalOwner is false)
	// and as a defensive default. The round actor treats this
	// as "do not emit a ledger event" to avoid misclassifying.
	VTXOOriginUnknown VTXOOrigin = iota

	// VTXOOriginRoundBoarding means the VTXO is the on-round
	// output of a boarding input the client owns: funds moved
	// from wallet_balance into vtxo_balance. Emitted as
	// VTXOReceivedMsg{Source=SourceRoundBoarding}.
	VTXOOriginRoundBoarding

	// VTXOOriginRoundRefresh means the VTXO materialized as the
	// output side of a refresh or directed-send flow in which
	// the client also forfeited VTXOs of roughly equal value.
	// Emitted as VTXOReceivedMsg{Source=SourceRoundRefresh}
	// paired with a VTXOSentMsg for the gross forfeited amount;
	// the two legs cancel on transfers_out. Used both for
	// straight refreshes and for self-change on directed sends.
	VTXOOriginRoundRefresh

	// VTXOOriginRoundTransfer means the VTXO was produced in-
	// round by another participant's directed send to this
	// client. Emitted as
	// VTXOReceivedMsg{Source=SourceRoundTransfer}, crediting
	// transfers_in as a genuine counterparty revenue flow.
	VTXOOriginRoundTransfer
)

// String returns a short human-readable label for the origin,
// matching the underscore-separated ledger source strings so a
// log line can include the origin verbatim.
func (o VTXOOrigin) String() string {
	switch o {
	case VTXOOriginRoundBoarding:
		return "round_boarding"

	case VTXOOriginRoundRefresh:
		return "round_refresh"

	case VTXOOriginRoundTransfer:
		return "round_transfer"

	default:
		return "unknown"
	}
}

// VTXORequest describes a requested round output. The policy template is the
// authoritative join-round representation, while local owner metadata is kept
// only when this client controls the resulting VTXO.
//
// Under the #270 seal-time fee handshake `Amount` carries the
// client's target amount and is honored verbatim except for the
// designated change output, whose amount the server computes as
// the residual (`Σin − Σ(fixed outputs) − fee`) at seal time. The
// per-intent invariant is exactly one output across VTXORequests
// + LeaveRequests with `IsChange=true`.
type VTXORequest struct {
	// Amount is the client's target amount in satoshis for this
	// output. Server-filled on the responding JoinRoundQuote when
	// IsChange=true.
	Amount btcutil.Amount

	// IsChange marks this request as the intent's designated
	// fee-bearing change output.
	IsChange bool

	// FixedAmount requires the operator quote to preserve Amount exactly.
	// It is used for contract outputs where shrinking the replacement
	// output would invalidate the higher-level protocol. A fixed single
	// output is not eligible for the implicit-change exception.
	FixedAmount bool

	// PolicyTemplate is the semantic arkscript policy for the requested
	// output. This is the authoritative join-round representation.
	PolicyTemplate []byte

	// PkScript is the output script of the VTXO. This will have
	// both a collaborative and unilateral spend path.
	PkScript []byte

	// Expiry is the CSV delay used in the unilateral timeout script path
	// of the VTXO.
	Expiry uint32

	// ClientKey is the public key of the client used in the construction
	// of the collaborative spend path of the VTXO.
	ClientKey *btcec.PublicKey

	// OwnerKey is the local key descriptor for the VTXO owner when this
	// client controls the resulting output. This is local-only
	// metadata and is not serialized onto the join-round wire.
	OwnerKey keychain.KeyDescriptor

	// OperatorKey is the public key of the operator used in the
	// construction of the collaborative spend path of the VTXO.
	OperatorKey *btcec.PublicKey

	// SigningKey is the key descriptor that the client will use in the
	// building of the VTXO tree during Musig2 signing sessions. We use
	// keychain.KeyDescriptor instead of just *btcec.PublicKey because we
	// need the key locator for signing operations.
	SigningKey keychain.KeyDescriptor

	// ExternalTreeSigner marks this VTXO's tree-signing (MuSig2 cosigner)
	// key as living outside this daemon: the round FSM must not derive a
	// wallet key for it and must route its tree nonce and partial-signature
	// production to an external party (e.g. an aggregate FROST key the
	// client controls off-box) instead of the local wallet signer. When
	// set, SigningKey.PubKey must be the external cosigner public key; the
	// key locator is ignored because the key is not wallet-derivable.
	ExternalTreeSigner bool

	// Origin classifies how a locally-owned VTXO came into
	// existence (boarding, refresh, or participant transfer). It
	// is set by the wallet at intent-composition time and flows
	// through the FSM so the round actor can emit a correctly-
	// typed VTXOReceivedMsg to the ledger actor. Local-only;
	// never serialized over the wire.
	Origin VTXOOrigin
}

// HasLocalOwner reports whether the request carries a local owner descriptor
// that should be preserved through confirmation and persistence. A nil owner
// pubkey, not a zero-valued key locator, is the sentinel for foreign outputs.
func (r *VTXORequest) HasLocalOwner() bool {
	return r != nil && r.OwnerKey.PubKey != nil
}

// BoardingRequest represents a request to board the Ark via a UTXO.
type BoardingRequest struct {
	// Outpoint represents the UTXO that will be used as input to the batch
	// transaction.
	Outpoint *wire.OutPoint

	// PolicyTemplate is the semantic arkscript policy for the boarding
	// output. This is the authoritative join-round representation.
	PolicyTemplate []byte

	// ClientKey is the public key used for the client in the boarding
	// tapscripts.
	ClientKey *btcec.PublicKey

	// OperatorKey is the public key used for the operator in the boarding
	// tapscript collaborative spend path.
	OperatorKey *btcec.PublicKey

	// ExitDelay is the CSV delay used in the unilateral timeout script
	// path of the boarding output. This must be at least the operator's
	// minimum boarding exit delay.
	ExitDelay uint32

	// TxProof is the SPV proof that the boarding UTXO exists in a
	// confirmed block. This allows the server to verify the UTXO without
	// querying its own chain source. The proof includes the transaction,
	// block header, merkle proof, and the taproot output construction
	// details (internal key and merkle root).
	//
	// None means "rely on the server's chain source." This is acceptable
	// when the operator runs a full bitcoind / btcd. Under standalone
	// (lwwallet) deployments the server has no chain source and rejects
	// join requests carrying TxProof=None with "TxProof is required when
	// server has no chain source". Clients SHOULD always populate this
	// field when they can; the wallet's first-confirmation pipeline
	// builds it inline (see wallet.processUtxo) and the
	// maybeRebuildBoardingProof recovery path reconstructs it from the
	// chain backend for any persisted intent that lacks one.
	TxProof fn.Option[proof.TxProof]
}

// BoardingInputSignature represents the client's signature for a boarding
// input in the batch transaction.
type BoardingInputSignature struct {
	// InputIndex is the index of the input in the transaction
	InputIndex int

	// Outpoint identifies which boarding input this signature is for
	Outpoint wire.OutPoint

	// ClientSignature is the client's schnorr signature
	ClientSignature *schnorr.Signature
}

// ForfeitTxSig represents an unsigned forfeit transaction with the client's
// VTXO signature.
type ForfeitTxSig struct {
	// UnsignedTx is the forfeit transaction without any witness data
	UnsignedTx *wire.MsgTx

	// ClientVTXOSig is the client's schnorr signature for the VTXO input
	ClientVTXOSig *schnorr.Signature

	// ParticipantVTXOSigs carries tapscript signatures from all
	// non-operator participants that must authorize the selected
	// spend path. Standard VTXO forfeits need only ClientVTXOSig;
	// custom policies such as vHTLC refund-style paths may require
	// multiple client-side signatures for one forfeited VTXO.
	ParticipantVTXOSigs []*ForfeitParticipantSig

	// SpendPath is the canonical arkscript spend path for the
	// forfeited VTXO input. This makes the custom or standard
	// tapscript leaf an explicit part of round messaging instead
	// of implicit witness metadata.
	SpendPath *arkscript.SpendPath
}

// ForfeitParticipantSig is one non-operator participant's tapscript
// signature for a forfeited VTXO input.
type ForfeitParticipantSig struct {
	// PubKey is the x-only key that produced Signature.
	PubKey *btcec.PublicKey

	// Signature authorizes the forfeit transaction under PubKey.
	Signature *schnorr.Signature
}

// ConnectorLeafInfo contains information about a connector leaf assigned to a
// specific forfeit request.
type ConnectorLeafInfo struct {
	// LeafOutpoint is the outpoint of the connector leaf that the forfeit
	// transaction should spend. This is the actual outpoint from the leaf
	// transaction in the connector tree.
	LeafOutpoint wire.OutPoint

	// LeafOutput is the transaction output for the connector leaf. This
	// contains the value and pkScript needed to construct the forfeit
	// transaction witness.
	LeafOutput *wire.TxOut

	// RootOutputIndex is the commitment-tx output index that this
	// connector tree's root transaction spends. It lets the client bind
	// the connector tree to the commitment tx it is about to sign into.
	RootOutputIndex uint32

	// NumLeaves is the total number of connector leaves in the tree this
	// leaf belongs to. Connector trees have identical leaves, so this plus
	// the radix and operator key fully determine the tree shape.
	NumLeaves uint32

	// Radix is the branching factor used to build the connector tree. It
	// is not derivable from the commitment tx, so the operator must supply
	// it for deterministic reconstruction.
	Radix uint32

	// LeafIndex is the position of this leaf within the connector tree's
	// flattened leaf list. Zero is a valid index.
	LeafIndex uint32
}

// BatchOutputInfo contains the information about a batch output in the
// batch transaction. A batch transaction can have multiple batch outputs,
// each with its own VTXO tree.
type BatchOutputInfo struct {
	// Idx is the index of this batch output in the batch transaction.
	Idx int

	// SignerKey is they key that the operator will use for the Musig2
	// signing sessions for this batch output.
	SignerKey *btcec.PublicKey

	// Tree is the VTXO tree for this batch output. The tree embeds the
	// per-round sweep key, sweep delay, and PrevOut.
	Tree *tree.Tree
}

// ClientBatchInfo contains batch information specific to a client. It contains
// all the info the client needs in order to validate that their requests were
// included correctly in the batch transaction.
//   - any boarding request will have a corresponding boarding input in the
//     batch transaction.
//   - any VTXO request will have a corresponding output in the batch
//     transaction.
//   - any forfeit request will have a corresponding connector leaf.
//   - any leave request will have a corresponding output in the batch
//     transaction.
type ClientBatchInfo struct {
	// Transaction is the batch transaction.
	Transaction *wire.MsgTx

	// BatchOutputs contains the batch output info for each batch output
	// that is relevant to the client. The number of VTXO leaves should
	// match the number of VTXO requests made by the client.
	BatchOutputs []*BatchOutputInfo

	// ConnectorLeafMap maps each forfeited VTXO outpoint to its assigned
	// connector leaf information. This allows the client to determine which
	// connector leaf corresponds to each of their forfeit requests.
	ConnectorLeafMap map[wire.OutPoint]*ConnectorLeafInfo
}
