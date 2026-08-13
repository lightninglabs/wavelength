package arkchannel

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// CooperativeCloseRequest fixes the ordinary replacement VTXO owners before
// either channel endpoint is quiesced. DeliveryScript retains its historical
// name in the durable/RPC envelope, but now contains a compressed owner key.
type CooperativeCloseRequest struct {
	Initiator            Party
	ClientDeliveryScript []byte
	HubDeliveryScript    []byte
}

// Clone returns a request without aliases to owner keys.
func (r CooperativeCloseRequest) Clone() CooperativeCloseRequest {
	r.ClientDeliveryScript = slices.Clone(r.ClientDeliveryScript)
	r.HubDeliveryScript = slices.Clone(r.HubDeliveryScript)

	return r
}

// Validate rejects a close request whose replacement VTXOs cannot be bound to
// the two expected owners.
func (r CooperativeCloseRequest) Validate() error {
	if r.Initiator != PartyClient {
		return fmt.Errorf("cooperative close must be client initiated")
	}
	clientKey, err := parseCooperativeCloseOwner(
		"client", r.ClientDeliveryScript,
	)
	if err != nil {
		return err
	}
	hubKey, err := parseCooperativeCloseOwner("hub", r.HubDeliveryScript)
	if err != nil {
		return err
	}
	if clientKey.IsEqual(hubKey) {
		return fmt.Errorf("cooperative close owners must differ")
	}

	return nil
}

// CooperativeCloseProposal is the exact unsigned OOR checkpoint and the clean
// lnd commitment state from which its replacement VTXO amounts were derived.
type CooperativeCloseProposal struct {
	Transaction      []byte
	CommitmentHeight uint64
	ClientBalance    btcutil.Amount
	HubBalance       btcutil.Amount
	ClientOutput     btcutil.Amount
	HubOutput        btcutil.Amount
}

// Clone returns a proposal without an alias to its checkpoint PSBT.
func (p CooperativeCloseProposal) Clone() CooperativeCloseProposal {
	p.Transaction = slices.Clone(p.Transaction)

	return p
}

// Validate reconstructs the canonical OOR package from immutable channel
// facts and requires byte-identical checkpoint signing material.
func (p CooperativeCloseProposal) Validate(terms Terms, source VTXOBinding,
	request CooperativeCloseRequest) error {

	expected, _, err := buildCooperativeCloseProposal(
		terms, source, request, p.ClientBalance, p.HubBalance,
		p.CommitmentHeight,
	)
	if err != nil {
		return err
	}
	if p.CommitmentHeight != expected.CommitmentHeight ||
		p.ClientBalance != expected.ClientBalance ||
		p.HubBalance != expected.HubBalance ||
		p.ClientOutput != expected.ClientOutput ||
		p.HubOutput != expected.HubOutput ||
		!bytes.Equal(p.Transaction, expected.Transaction) {
		return fmt.Errorf("cooperative close proposal is not canonical")
	}

	return nil
}

// CooperativeClose is the hub-authorized OOR close artifact. Transaction
// contains the hub signature for the channel VTXO's no-delay 3-of-3 leaf;
// TxID is the deterministic OOR Ark transaction/session ID.
type CooperativeClose struct {
	Proposal    CooperativeCloseProposal
	Transaction []byte
	TxID        chainhash.Hash
}

// Clone returns a close artifact without aliases to signature or PSBT bytes.
func (c CooperativeClose) Clone() CooperativeClose {
	c.Proposal = c.Proposal.Clone()
	c.Transaction = slices.Clone(c.Transaction)

	return c
}

// Validate proves the proposal is canonical, its session ID is deterministic,
// and the hub authorized the exact OOR checkpoint through the 3-of-3 leaf.
func (c CooperativeClose) Validate(terms Terms, source VTXOBinding,
	request CooperativeCloseRequest) error {

	if err := c.Proposal.Validate(terms, source, request); err != nil {
		return err
	}
	template, err := NewCooperativeCloseTemplate(
		terms, source, request, c.Proposal.ClientBalance,
		c.Proposal.HubBalance, c.Proposal.CommitmentHeight,
	)
	if err != nil {
		return err
	}
	if c.TxID != template.sessionID {
		return fmt.Errorf("cooperative close OOR session ID does not " +
			"match")
	}
	sig, err := schnorr.ParseSignature(c.Transaction)
	if err != nil {
		return fmt.Errorf("parse hub cooperative close signature: %w",
			err)
	}

	return template.VerifySignature(terms, PartyHub, sig)
}

// CooperativeCloseOORSpec contains the deterministic ordinary OOR actor input
// data that is safe to hand across the core channel package boundary.
type CooperativeCloseOORSpec struct {
	CheckpointPolicy arkscript.CheckpointPolicy
	SpendPath        []byte
	Recipients       []oortx.RecipientOutput
}

// CooperativeCloseTemplate owns the canonical OOR proposal and the immediate
// three-party spend path while the hub signature is collected.
type CooperativeCloseTemplate struct {
	proposal    CooperativeCloseProposal
	sessionID   chainhash.Hash
	previousOut *wire.TxOut
	checkpoint  *psbt.Packet
	spendPath   *arkscript.SpendPath
	policy      arkscript.CheckpointPolicy
	recipients  []oortx.RecipientOutput
}

// NewCooperativeCloseTemplate builds an OOR transfer from the same clean
// commitment height and balance allocation observed by both lnd endpoints.
func NewCooperativeCloseTemplate(terms Terms, source VTXOBinding,
	request CooperativeCloseRequest,
	clientBalance, hubBalance btcutil.Amount, commitmentHeight uint64) (
	*CooperativeCloseTemplate, error) {

	proposal, artifacts, err := buildCooperativeCloseProposal(
		terms, source, request, clientBalance, hubBalance,
		commitmentHeight,
	)
	if err != nil {
		return nil, err
	}

	return &CooperativeCloseTemplate{
		proposal: proposal, sessionID: artifacts.sessionID,
		previousOut: artifacts.previousOut,
		checkpoint:  artifacts.checkpoint,
		spendPath:   artifacts.spendPath,
		policy:      artifacts.policy,
		recipients:  artifacts.recipients,
	}, nil
}

// Proposal returns an isolated copy of the unsigned OOR close proposal.
func (t *CooperativeCloseTemplate) Proposal() CooperativeCloseProposal {
	return t.proposal.Clone()
}

// OORSpec returns isolated deterministic inputs for the ordinary OOR actor.
func (t *CooperativeCloseTemplate) OORSpec() (CooperativeCloseOORSpec, error) {
	spendPath, err := t.spendPath.Encode()
	if err != nil {
		return CooperativeCloseOORSpec{}, err
	}

	return CooperativeCloseOORSpec{
		CheckpointPolicy: t.policy,
		SpendPath:        spendPath,
		Recipients:       cloneCooperativeCloseRecipients(t.recipients),
	}, nil
}

// SignDescriptor returns the exact checkpoint descriptor after checking that
// the signer owns the requested endpoint role in the 3-of-3 policy leaf.
func (t *CooperativeCloseTemplate) SignDescriptor(terms Terms, party Party,
	keyDesc keychain.KeyDescriptor) (*input.SignDescriptor, error) {

	var expected [33]byte
	switch party {
	case PartyClient:
		expected = terms.VTXO.ClientArkKey

	case PartyHub:
		expected = terms.VTXO.HubArkKey

	default:
		return nil, fmt.Errorf("unknown cooperative close party %d",
			party)
	}

	return t.signDescriptor(expected, keyDesc)
}

// VerifySignature proves one participant signed the canonical checkpoint.
func (t *CooperativeCloseTemplate) VerifySignature(terms Terms, party Party,
	sig input.Signature) error {

	if sig == nil {
		return fmt.Errorf("%s cooperative close signature is required",
			party)
	}
	var expected [33]byte
	switch party {
	case PartyClient:
		expected = terms.VTXO.ClientArkKey

	case PartyHub:
		expected = terms.VTXO.HubArkKey

	default:
		return fmt.Errorf("unknown cooperative close party %d", party)
	}
	pubKey, err := parseChannelKey("cooperative close", expected)
	if err != nil {
		return err
	}
	desc, err := t.signDescriptor(
		expected, keychain.KeyDescriptor{
			PubKey: pubKey,
		},
	)
	if err != nil {
		return err
	}
	leaf := txscript.NewBaseTapLeaf(desc.WitnessScript)
	sigHash, err := txscript.CalcTapscriptSignaturehash(
		desc.SigHashes, desc.HashType, t.checkpoint.UnsignedTx,
		desc.InputIndex, desc.PrevOutputFetcher, leaf,
	)
	if err != nil {
		return fmt.Errorf("derive %s cooperative close sighash: %w",
			party, err)
	}
	if !sig.Verify(sigHash, pubKey) {
		return fmt.Errorf("invalid %s cooperative close signature",
			party)
	}

	return nil
}

// Complete records the hub signature after verifying it against the exact
// checkpoint. Client and Ark operator signatures remain absent until OOR
// submission, where the operator-first custody rule is enforced.
func (t *CooperativeCloseTemplate) Complete(terms Terms, source VTXOBinding,
	request CooperativeCloseRequest, hubSig input.Signature) (
	CooperativeClose, error) {

	if err := t.VerifySignature(terms, PartyHub, hubSig); err != nil {
		return CooperativeClose{}, err
	}
	settlement := CooperativeClose{
		Proposal:    t.proposal.Clone(),
		Transaction: hubSig.Serialize(),
		TxID:        t.sessionID,
	}
	if err := settlement.Validate(terms, source, request); err != nil {
		return CooperativeClose{}, err
	}

	return settlement, nil
}

// signDescriptor binds one expected key to the canonical checkpoint input.
func (t *CooperativeCloseTemplate) signDescriptor(expected [33]byte,
	keyDesc keychain.KeyDescriptor) (*input.SignDescriptor, error) {

	if keyDesc.PubKey == nil {
		return nil, fmt.Errorf("cooperative close signing key is " +
			"required")
	}
	expectedKey, err := parseChannelKey("cooperative close", expected)
	if err != nil {
		return nil, err
	}
	if !keyDesc.PubKey.IsEqual(expectedKey) {
		return nil, fmt.Errorf("cooperative close signing key does " +
			"not match policy role")
	}
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		t.previousOut.PkScript, t.previousOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(
		t.checkpoint.UnsignedTx, prevFetcher,
	)

	return t.spendPath.BuildSignDescriptor(
		keyDesc, t.previousOut, sigHashes, prevFetcher, 0,
	), nil
}

type cooperativeCloseArtifacts struct {
	sessionID   chainhash.Hash
	previousOut *wire.TxOut
	checkpoint  *psbt.Packet
	spendPath   *arkscript.SpendPath
	policy      arkscript.CheckpointPolicy
	recipients  []oortx.RecipientOutput
}

// buildCooperativeCloseProposal derives OOR outputs only from immutable
// channel facts and lnd's clean balance allocation.
func buildCooperativeCloseProposal(terms Terms, source VTXOBinding,
	request CooperativeCloseRequest,
	clientBalance, hubBalance btcutil.Amount, commitmentHeight uint64) (
	CooperativeCloseProposal, cooperativeCloseArtifacts, error) {

	if err := terms.Validate(); err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	if err := source.Validate(terms); err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	if err := request.Validate(); err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	if clientBalance < 0 || hubBalance < 0 ||
		clientBalance+hubBalance != terms.Capacity {
		return CooperativeCloseProposal{}, cooperativeCloseArtifacts{},
			fmt.Errorf("cooperative close balances %d + %d do not "+
				"match capacity %d", clientBalance, hubBalance,
				terms.Capacity)
	}
	reserve := source.Amount - terms.Capacity
	if reserve <= 0 {
		return CooperativeCloseProposal{}, cooperativeCloseArtifacts{},
			fmt.Errorf("channel VTXO has no backing reserve")
	}
	clientOutput := clientBalance
	hubOutput := hubBalance
	if terms.Funder == PartyClient {
		clientOutput += reserve
	} else {
		hubOutput += reserve
	}

	operatorKey, err := parseChannelKey(
		"Ark operator", terms.VTXO.ArkOperatorKey,
	)
	if err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	clientKey, err := parseChannelKey(
		"client Ark", terms.VTXO.ClientArkKey,
	)
	if err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	channelPolicy, err := channelPolicy(terms.VTXO)
	if err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	spendPath, err := channelPolicy.CooperativeSpendPath()
	if err != nil {
		return CooperativeCloseProposal{}, cooperativeCloseArtifacts{},
			fmt.Errorf("derive cooperative close path: %w", err)
	}
	if err := spendPath.VerifyBindsToPkScript(source.PkScript); err != nil {
		return CooperativeCloseProposal{}, cooperativeCloseArtifacts{},
			fmt.Errorf("validate cooperative close path: %w", err)
	}

	ownerLeaf, err := arkscript.MultiSigCollabTapLeaf(
		clientKey, operatorKey,
	)
	if err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	ownerPolicy, err := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{
				clientKey,
				operatorKey,
			},
		},
	}.Encode()
	if err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	previousOut := &wire.TxOut{
		Value: int64(source.Amount), PkScript: slices.Clone(
			source.PkScript,
		),
	}
	checkpointPolicy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey, CSVDelay: terms.VTXO.MinExitDelay,
	}
	checkpoint, err := oortx.BuildCheckpointPSBT(
		checkpointPolicy, oortx.CheckpointInput{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: source.OutPoint, Output: previousOut,
			},
			OwnerLeafScript: ownerLeaf.Script,
			OwnerLeafPolicy: ownerPolicy,
		},
	)
	if err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	checkpoint.PSBT.UnsignedTx.LockTime = spendPath.RequiredLockTime
	checkpoint.PSBT.UnsignedTx.TxIn[0].Sequence = spendPath.RequiredSequence

	recipients := make([]oortx.RecipientOutput, 0, 2)
	if clientOutput > 0 {
		recipient, err := cooperativeCloseRecipient(
			request.ClientDeliveryScript, operatorKey,
			terms.VTXO.MinExitDelay, clientOutput,
		)
		if err != nil {
			return CooperativeCloseProposal{},
				cooperativeCloseArtifacts{}, err
		}
		recipients = append(recipients, recipient)
	}
	if hubOutput > 0 {
		recipient, err := cooperativeCloseRecipient(
			request.HubDeliveryScript, operatorKey,
			terms.VTXO.MinExitDelay, hubOutput,
		)
		if err != nil {
			return CooperativeCloseProposal{},
				cooperativeCloseArtifacts{}, err
		}
		recipients = append(recipients, recipient)
	}
	if len(recipients) == 0 {
		return CooperativeCloseProposal{}, cooperativeCloseArtifacts{},
			fmt.Errorf("cooperative close has no replacement VTXO")
	}
	checkpointOut, err := checkpoint.ToCheckpointOutput()
	if err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	arkPSBT, err := oortx.BuildArkPSBT(
		[]oortx.CheckpointOutput{checkpointOut}, recipients,
	)
	if err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	arkPSBT.UnsignedTx.LockTime = spendPath.RequiredLockTime
	arkPSBT.UnsignedTx.TxIn[0].Sequence = spendPath.RequiredSequence
	checkpointRaw, err := psbtutil.Serialize(checkpoint.PSBT)
	if err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}

	return CooperativeCloseProposal{
			Transaction:      checkpointRaw,
			CommitmentHeight: commitmentHeight,
			ClientBalance:    clientBalance,
			HubBalance:       hubBalance,
			ClientOutput:     clientOutput,
			HubOutput:        hubOutput,
		}, cooperativeCloseArtifacts{
			sessionID:   arkPSBT.UnsignedTx.TxHash(),
			previousOut: previousOut,
			checkpoint:  checkpoint.PSBT,
			spendPath:   spendPath,
			policy:      checkpointPolicy,
			recipients:  recipients,
		}, nil
}

// cooperativeCloseRecipient constructs one ordinary Ark VTXO output.
func cooperativeCloseRecipient(rawOwnerKey []byte, operatorKey *btcec.PublicKey,
	exitDelay uint32,
	amount btcutil.Amount) (oortx.RecipientOutput, error) {

	ownerKey, err := parseCooperativeCloseOwner(
		"replacement", rawOwnerKey,
	)
	if err != nil {
		return oortx.RecipientOutput{}, err
	}
	policy, err := arkscript.NewVTXOPolicy(
		ownerKey, operatorKey, exitDelay,
	)
	if err != nil {
		return oortx.RecipientOutput{}, err
	}
	policyTemplate, err := policy.Template.Encode()
	if err != nil {
		return oortx.RecipientOutput{}, err
	}
	pkScript, err := policy.Template.PkScript()
	if err != nil {
		return oortx.RecipientOutput{}, err
	}

	return oortx.RecipientOutput{
		PkScript: pkScript, Value: amount,
		VTXOPolicyTemplate: policyTemplate,
	}, nil
}

// parseCooperativeCloseOwner accepts compressed secp256k1 public keys only.
func parseCooperativeCloseOwner(role string,
	raw []byte) (*btcec.PublicKey, error) {

	if len(raw) != btcec.PubKeyBytesLenCompressed {
		return nil, fmt.Errorf("%s cooperative close owner key has %d "+
			"bytes, expected %d", role, len(raw),
			btcec.PubKeyBytesLenCompressed)
	}
	key, err := btcec.ParsePubKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s cooperative close "+
			"owner key: %w", role, err)
	}

	return key, nil
}

// cloneCooperativeCloseRecipients returns recipients without mutable aliases.
func cloneCooperativeCloseRecipients(
	recipients []oortx.RecipientOutput) []oortx.RecipientOutput {

	cloned := make([]oortx.RecipientOutput, len(recipients))
	for i := range recipients {
		cloned[i] = recipients[i]
		cloned[i].PkScript = slices.Clone(recipients[i].PkScript)
		cloned[i].VTXOPolicyTemplate = slices.Clone(
			recipients[i].VTXOPolicyTemplate,
		)
	}

	return cloned
}
