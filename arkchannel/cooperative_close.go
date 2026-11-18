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
	"github.com/lightningnetwork/lnd/tlv"
)

// CooperativeCloseRequest fixes the ordinary replacement VTXO owners before
// either channel endpoint is quiesced. A compressed owner key retains the
// source policy for legacy requests. After rotation both delivery fields
// carry canonical standard VTXO templates, including the new session policy.
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
	client, err := decodeCloseDelivery(
		"client", r.ClientDeliveryScript,
	)
	if err != nil {
		return err
	}
	hub, err := decodeCloseDelivery("hub", r.HubDeliveryScript)
	if err != nil {
		return err
	}
	if client.OwnerKey.IsEqual(hub.OwnerKey) {
		return fmt.Errorf("cooperative close owners must differ")
	}
	if (client.OperatorKey == nil) != (hub.OperatorKey == nil) ||
		client.ExitDelay != hub.ExitDelay ||
		(client.OperatorKey != nil &&
			!client.OperatorKey.IsEqual(hub.OperatorKey)) {
		return fmt.Errorf("cooperative close delivery policies differ")
	}

	return nil
}

// checkpointPolicy resolves the session terms fixed by both destinations.
// Only legacy owner-only requests inherit the source's original policy.
func (r CooperativeCloseRequest) checkpointPolicy(terms Terms) (
	arkscript.CheckpointPolicy, error) {

	params, err := decodeCloseDelivery("client", r.ClientDeliveryScript)
	if err != nil {
		return arkscript.CheckpointPolicy{}, err
	}
	if params.OperatorKey != nil {
		return arkscript.CheckpointPolicy{
			OperatorKey: params.OperatorKey,
			CSVDelay:    params.ExitDelay,
		}, nil
	}
	operator, err := parseChannelKey(
		"Ark operator", terms.VTXO.ArkOperatorKey,
	)

	return arkscript.CheckpointPolicy{
		OperatorKey: operator, CSVDelay: terms.VTXO.MinExitDelay,
	}, err
}

// CooperativeCloseProposal is the exact unsigned OOR checkpoint and the
// full-capacity role balance allocation used for its replacement VTXO
// amounts. ClientBalance and HubBalance must already sum exactly to
// Terms.Capacity; core does not infer or redistribute a missing balance.
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
// contains the hub signatures for both the checkpoint and final allocation;
// TxID is the deterministic OOR Ark transaction/session ID.
type CooperativeClose struct {
	Proposal    CooperativeCloseProposal
	Transaction []byte
	TxID        chainhash.Hash
}

// AuthorizationReceipt binds a client's receipt to both hub signatures. The
// public OOR txid alone does not prove receipt of the signed package.
func (c CooperativeClose) AuthorizationReceipt() chainhash.Hash {
	return chainhash.HashH(c.Transaction)
}

// Clone returns a close artifact without aliases to signature or PSBT bytes.
func (c CooperativeClose) Clone() CooperativeClose {
	c.Proposal = c.Proposal.Clone()
	c.Transaction = slices.Clone(c.Transaction)

	return c
}

// Validate proves the proposal is canonical, its session ID is deterministic,
// and the hub authorized both the checkpoint and the final payout allocation.
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
	checkpointSig, arkSig, err := c.Signatures()
	if err != nil {
		return err
	}
	if err := template.VerifySignature(
		terms, PartyHub, checkpointSig,
	); err != nil {
		return err
	}

	return template.VerifyArkSignature(terms, PartyHub, arkSig)
}

// Signatures decodes both durable authorizations. A legacy checkpoint-only
// signature cannot prove that the hub agreed to the final payout.
func (c CooperativeClose) Signatures() (*schnorr.Signature, *schnorr.Signature,
	error) {

	if len(c.Transaction) > 1024 {
		return nil, nil, fmt.Errorf("close authorization exceeds " +
			"size limit")
	}
	var checkpointRaw, arkRaw []byte
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(1, &checkpointRaw),
		tlv.MakePrimitiveRecord(3, &arkRaw),
	)
	if err != nil {
		return nil, nil, err
	}
	if err := stream.Decode(bytes.NewReader(c.Transaction)); err != nil {
		return nil, nil, fmt.Errorf("decode close authorization: %w",
			err)
	}
	checkpointSig, err := schnorr.ParseSignature(checkpointRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("parse checkpoint "+
			"authorization: %w", err)
	}
	arkSig, err := schnorr.ParseSignature(arkRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("parse payout authorization: %w",
			err)
	}

	return checkpointSig, arkSig, nil
}

// ReplacementOutPoint derives the exact ordinary VTXO assigned to one close
// participant. Reconstructing the canonical Ark transaction avoids assuming
// recipient order, which is changed by BIP69 output sorting.
func (c CooperativeClose) ReplacementOutPoint(terms Terms, source VTXOBinding,
	request CooperativeCloseRequest, party Party) (wire.OutPoint, error) {

	if err := c.Validate(terms, source, request); err != nil {
		return wire.OutPoint{}, err
	}

	var (
		owner  []byte
		amount btcutil.Amount
	)
	switch party {
	case PartyClient:
		owner = request.ClientDeliveryScript
		amount = c.Proposal.ClientOutput

	case PartyHub:
		owner = request.HubDeliveryScript
		amount = c.Proposal.HubOutput

	default:
		return wire.OutPoint{}, fmt.Errorf("unknown cooperative close "+
			"party %d", party)
	}
	if amount <= 0 {
		return wire.OutPoint{}, fmt.Errorf("%s cooperative close has "+
			"no replacement output", party)
	}

	policy, err := request.checkpointPolicy(terms)
	if err != nil {
		return wire.OutPoint{}, err
	}
	target, err := cooperativeCloseRecipient(
		owner, policy.OperatorKey, policy.CSVDelay, amount,
	)
	if err != nil {
		return wire.OutPoint{}, err
	}
	template, err := NewCooperativeCloseTemplate(
		terms, source, request, c.Proposal.ClientBalance,
		c.Proposal.HubBalance, c.Proposal.CommitmentHeight,
	)
	if err != nil {
		return wire.OutPoint{}, err
	}
	outpoint, err := oortx.RecipientOutPoint(
		c.TxID, template.recipients, target,
	)
	if err != nil {
		return wire.OutPoint{}, fmt.Errorf("derive %s cooperative "+
			"close replacement: %w", party, err)
	}

	return outpoint, nil
}

// CooperativeCloseOORSpec contains the deterministic ordinary OOR actor input
// data that is safe to hand across the core channel package boundary.
type CooperativeCloseOORSpec struct {
	CheckpointPolicy arkscript.CheckpointPolicy
	SpendPath        []byte
	Recipients       []oortx.RecipientOutput

	// OwnerLeafScript retains every source owner in the checkpoint spend.
	OwnerLeafScript []byte

	// OwnerLeafPolicy encodes that exact owner set for durable OOR replay.
	OwnerLeafPolicy []byte
}

// CooperativeCloseTemplate owns the canonical OOR proposal and the immediate
// three-party spend path while the hub signature is collected.
type CooperativeCloseTemplate struct {
	proposal    CooperativeCloseProposal
	sessionID   chainhash.Hash
	previousOut *wire.TxOut
	checkpoint  *psbt.Packet
	ark         *psbt.Packet
	ownerPolicy []byte
	spendPath   *arkscript.SpendPath
	policy      arkscript.CheckpointPolicy
	recipients  []oortx.RecipientOutput
}

// NewCooperativeCloseTemplate builds an OOR transfer from full-capacity role
// balances supplied by the runtime at the same clean commitment height.
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
		ark:         artifacts.ark,
		ownerPolicy: artifacts.ownerPolicy,
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
		OwnerLeafScript: slices.Clone(
			t.ark.Inputs[0].TaprootLeafScript[0].Script,
		),
		OwnerLeafPolicy: slices.Clone(t.ownerPolicy),
	}, nil
}

// ArkTransaction returns the exact payout transaction the hub must authorize.
func (t *CooperativeCloseTemplate) ArkTransaction() *wire.MsgTx {
	return t.ark.UnsignedTx.Copy()
}

// ArkSignDescriptor binds the signer's channel role to the final allocation.
func (t *CooperativeCloseTemplate) ArkSignDescriptor(terms Terms, party Party,
	keyDesc keychain.KeyDescriptor) (*input.SignDescriptor, error) {

	desc, err := t.SignDescriptor(terms, party, keyDesc)
	if err != nil {
		return nil, err
	}
	output := t.ark.Inputs[0].WitnessUtxo
	leaf := t.ark.Inputs[0].TaprootLeafScript[0]
	desc.Output = output
	desc.WitnessScript = slices.Clone(leaf.Script)
	desc.ControlBlock = slices.Clone(leaf.ControlBlock)
	desc.PrevOutputFetcher = txscript.NewCannedPrevOutputFetcher(
		output.PkScript, output.Value,
	)
	desc.SigHashes = txscript.NewTxSigHashes(
		t.ark.UnsignedTx, desc.PrevOutputFetcher,
	)

	return desc, nil
}

// VerifyArkSignature rejects authorization of another transaction or payout.
func (t *CooperativeCloseTemplate) VerifyArkSignature(terms Terms, party Party,
	sig input.Signature) error {

	key := terms.VTXO.HubArkKey
	if party == PartyClient {
		key = terms.VTXO.ClientArkKey
	}
	pubKey, err := parseChannelKey("payout", key)
	if err != nil {
		return err
	}
	desc, err := t.ArkSignDescriptor(terms, party, keychain.KeyDescriptor{
		PubKey: pubKey,
	})
	if err != nil {
		return err
	}
	if sig == nil {
		return fmt.Errorf("payout authorization is required")
	}
	hash, err := txscript.CalcTapscriptSignaturehash(
		desc.SigHashes, desc.HashType, t.ark.UnsignedTx,
		desc.InputIndex, desc.PrevOutputFetcher,
		txscript.NewBaseTapLeaf(desc.WitnessScript),
	)
	if err != nil {
		return err
	}
	if !sig.Verify(hash, pubKey) {
		return fmt.Errorf("invalid %s payout authorization", party)
	}

	return nil
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

// Complete verifies and records the hub's checkpoint and final payout
// signatures. Client and Ark operator signatures remain absent until OOR
// submission, where the operator-first custody rule is enforced.
func (t *CooperativeCloseTemplate) Complete(terms Terms, source VTXOBinding,
	request CooperativeCloseRequest, hubSig, hubArkSig input.Signature) (
	CooperativeClose, error) {

	if err := t.VerifySignature(terms, PartyHub, hubSig); err != nil {
		return CooperativeClose{}, err
	}
	if err := t.VerifyArkSignature(terms, PartyHub, hubArkSig); err != nil {
		return CooperativeClose{}, err
	}
	checkpointRaw, arkRaw := hubSig.Serialize(), hubArkSig.Serialize()
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(1, &checkpointRaw),
		tlv.MakePrimitiveRecord(3, &arkRaw),
	)
	if err != nil {
		return CooperativeClose{}, err
	}
	var authorization bytes.Buffer
	if err := stream.Encode(&authorization); err != nil {
		return CooperativeClose{}, err
	}
	settlement := CooperativeClose{
		Proposal:    t.proposal.Clone(),
		Transaction: authorization.Bytes(),
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
	ark         *psbt.Packet
	ownerPolicy []byte
	spendPath   *arkscript.SpendPath
	policy      arkscript.CheckpointPolicy
	recipients  []oortx.RecipientOutput
}

// buildCooperativeCloseProposal derives OOR outputs only from immutable
// channel facts and a full-capacity role balance allocation.
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
	// Promotion is client-funded. The unused backing reserve returns
	// to the client; it was never part of the Lightning capacity.
	clientOutput := clientBalance + reserve
	hubOutput := hubBalance

	checkpointPolicy, err := request.checkpointPolicy(terms)
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

	hubKey, err := parseChannelKey("hub Ark", terms.VTXO.HubArkKey)
	if err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	// The checkpoint has three independent signers even when its
	// operator is no longer the operator on the input spend path.
	if len(request.ClientDeliveryScript) != btcec.PubKeyBytesLenCompressed {
		key := schnorr.SerializePubKey(checkpointPolicy.OperatorKey)
		if bytes.Equal(key, schnorr.SerializePubKey(clientKey)) ||
			bytes.Equal(key, schnorr.SerializePubKey(hubKey)) {

			err := fmt.Errorf("checkpoint operator duplicates a " +
				"channel owner")

			return CooperativeCloseProposal{},
				cooperativeCloseArtifacts{}, err
		}
	}
	ownerLeaf := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{
				clientKey,
				hubKey,
				checkpointPolicy.OperatorKey,
			},
		},
	}
	ownerScript, err := ownerLeaf.Script()
	if err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	ownerPolicy, err := ownerLeaf.Encode()
	if err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	previousOut := &wire.TxOut{
		Value: int64(source.Amount), PkScript: slices.Clone(
			source.PkScript,
		),
	}
	checkpoint, err := oortx.BuildCheckpointPSBT(
		checkpointPolicy, oortx.CheckpointInput{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: source.OutPoint, Output: previousOut,
			},
			OwnerLeafScript: ownerScript,
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
			request.ClientDeliveryScript,
			checkpointPolicy.OperatorKey, checkpointPolicy.CSVDelay,
			clientOutput,
		)
		if err != nil {
			return CooperativeCloseProposal{},
				cooperativeCloseArtifacts{}, err
		}
		recipients = append(recipients, recipient)
	}
	if hubOutput > 0 {
		recipient, err := cooperativeCloseRecipient(
			request.HubDeliveryScript, checkpointPolicy.OperatorKey,
			checkpointPolicy.CSVDelay, hubOutput,
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
	ownerSpend, err := oortx.BuildTaprootTapLeafScript(
		checkpoint.TapTreeEncoded, ownerScript,
	)
	if err != nil {
		return CooperativeCloseProposal{},
			cooperativeCloseArtifacts{}, err
	}
	arkPSBT.Inputs[0].TaprootLeafScript = []*psbt.TaprootTapLeafScript{
		ownerSpend,
	}
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
		ark:         arkPSBT,
		ownerPolicy: ownerPolicy,
		spendPath:   spendPath,
		policy:      checkpointPolicy,
		recipients:  recipients,
	}, nil
}

// cooperativeCloseRecipient constructs one ordinary Ark VTXO output.
func cooperativeCloseRecipient(rawOwnerKey []byte, operatorKey *btcec.PublicKey,
	exitDelay uint32,
	amount btcutil.Amount) (oortx.RecipientOutput, error) {

	ownerKey, err := CooperativeCloseDeliveryOwner(rawOwnerKey)
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

// CooperativeCloseDeliveryOwner identifies the payout owner independently
// of later key rotations when replaying an already signed close.
func CooperativeCloseDeliveryOwner(raw []byte) (*btcec.PublicKey, error) {
	params, err := decodeCloseDelivery("replacement", raw)
	if err != nil {
		return nil, err
	}

	return params.OwnerKey, nil
}

// decodeCloseDelivery accepts a legacy owner or an exact canonical standard
// VTXO policy. No other spending paths may accompany the quoted owner.
func decodeCloseDelivery(role string,
	raw []byte) (*arkscript.StandardVTXOParams, error) {

	if len(raw) == btcec.PubKeyBytesLenCompressed {
		key, err := btcec.ParsePubKey(raw)
		if err != nil {
			return nil, fmt.Errorf("parse %s close owner: %w", role,
				err)
		}

		return &arkscript.StandardVTXOParams{OwnerKey: key}, nil
	}
	template, err := arkscript.DecodePolicyTemplate(raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s close delivery: %w", role,
			err)
	}
	params, err := arkscript.DecodeStandardVTXOParams(template)
	if err != nil {
		return nil, err
	}
	canonical, err := arkscript.EncodeStandardVTXOTemplate(
		params.OwnerKey, params.OperatorKey, params.ExitDelay,
	)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, raw) {
		return nil, fmt.Errorf("noncanonical %s close delivery", role)
	}

	return params, nil
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
