package arkchannel

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
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
	proposal CooperativeCloseProposal

	spendPath  *arkscript.SpendPath
	policy     arkscript.CheckpointPolicy
	recipients []oortx.RecipientOutput
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
		proposal: proposal,

		spendPath:  artifacts.spendPath,
		policy:     artifacts.policy,
		recipients: artifacts.recipients,
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

type cooperativeCloseArtifacts struct {
	spendPath  *arkscript.SpendPath
	policy     arkscript.CheckpointPolicy
	recipients []oortx.RecipientOutput
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

		spendPath:  spendPath,
		policy:     checkpointPolicy,
		recipients: recipients,
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
