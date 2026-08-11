package arkchannel

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/btcutil/v2/txsort"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// CooperativeCloseRequest fixes the close initiator, settlement scripts, and
// fee rate before either channel endpoint is quiesced.
type CooperativeCloseRequest struct {
	Initiator            Party
	ClientDeliveryScript []byte
	HubDeliveryScript    []byte
	FeeRate              chainfee.SatPerKWeight
}

// Clone returns a request without aliases to delivery scripts.
func (r CooperativeCloseRequest) Clone() CooperativeCloseRequest {
	r.ClientDeliveryScript = slices.Clone(r.ClientDeliveryScript)
	r.HubDeliveryScript = slices.Clone(r.HubDeliveryScript)

	return r
}

// Validate rejects a close request that cannot create ordinary spendable
// settlement outputs.
func (r CooperativeCloseRequest) Validate() error {
	if r.Initiator != PartyClient {
		return fmt.Errorf("cooperative close must be client initiated")
	}
	if err := validateDeliveryScript(
		"client", r.ClientDeliveryScript,
	); err != nil {
		return err
	}
	if err := validateDeliveryScript(
		"hub", r.HubDeliveryScript,
	); err != nil {
		return err
	}
	if r.FeeRate < chainfee.FeePerKwFloor {
		return fmt.Errorf("cooperative close fee rate %d is below "+
			"floor %d", r.FeeRate, chainfee.FeePerKwFloor)
	}

	return nil
}

// CooperativeCloseProposal is the exact unsigned VTXO settlement transaction
// and the clean lnd commitment state from which its outputs were derived.
type CooperativeCloseProposal struct {
	Transaction      []byte
	CommitmentHeight uint64
	ClientBalance    btcutil.Amount
	HubBalance       btcutil.Amount
	ClientOutput     btcutil.Amount
	HubOutput        btcutil.Amount
	Fee              btcutil.Amount
}

// Clone returns a proposal without an alias to its transaction bytes.
func (p CooperativeCloseProposal) Clone() CooperativeCloseProposal {
	p.Transaction = slices.Clone(p.Transaction)

	return p
}

// Validate reconstructs the canonical proposal from immutable channel facts.
func (p CooperativeCloseProposal) Validate(terms Terms, source VTXOBinding,
	request CooperativeCloseRequest) error {

	expected, _, _, err := buildCooperativeCloseProposal(
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
		p.HubOutput != expected.HubOutput || p.Fee != expected.Fee ||
		!bytes.Equal(p.Transaction, expected.Transaction) {
		return fmt.Errorf("cooperative close proposal is not canonical")
	}

	return nil
}

// CooperativeClose is the fully signed direct spend of the channel-policy
// VTXO. It closes the virtual lnd channel without first publishing its backing
// transaction and channel point.
type CooperativeClose struct {
	Proposal    CooperativeCloseProposal
	Transaction []byte
	TxID        chainhash.Hash
}

// Clone returns a settlement without aliases to transaction bytes.
func (c CooperativeClose) Clone() CooperativeClose {
	c.Proposal = c.Proposal.Clone()
	c.Transaction = slices.Clone(c.Transaction)

	return c
}

// Validate proves the settlement is the canonical proposal with a valid
// client, hub, and Ark-operator witness.
func (c CooperativeClose) Validate(terms Terms, source VTXOBinding,
	request CooperativeCloseRequest) error {

	if err := c.Proposal.Validate(terms, source, request); err != nil {
		return err
	}
	tx, err := decodeCooperativeCloseTransaction(c.Transaction)
	if err != nil {
		return err
	}
	if tx.TxHash() != c.TxID {
		return fmt.Errorf("cooperative close transaction ID does not " +
			"match")
	}
	if len(tx.TxIn) != 1 || tx.TxIn[0].PreviousOutPoint != source.OutPoint {
		return fmt.Errorf("cooperative close does not spend channel " +
			"VTXO")
	}
	if len(tx.TxIn[0].Witness) == 0 {
		return fmt.Errorf("cooperative close is not fully signed")
	}
	unsigned := tx.Copy()
	unsigned.TxIn[0].Witness = nil
	unsignedRaw, err := serializeCooperativeCloseTransaction(unsigned)
	if err != nil {
		return err
	}
	if !bytes.Equal(unsignedRaw, c.Proposal.Transaction) {
		return fmt.Errorf("cooperative close witness changes proposal")
	}
	previousOut := &wire.TxOut{
		Value:    int64(source.Amount),
		PkScript: slices.Clone(source.PkScript),
	}
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		previousOut.PkScript, previousOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)
	engine, err := txscript.NewEngine(
		previousOut.PkScript, tx, 0, txscript.StandardVerifyFlags, nil,
		sigHashes, previousOut.Value, prevFetcher,
	)
	if err != nil {
		return fmt.Errorf("construct cooperative close verifier: %w",
			err)
	}
	if err := engine.Execute(); err != nil {
		return fmt.Errorf("verify cooperative close signatures: %w",
			err)
	}

	return nil
}

// CooperativeCloseTemplate owns the canonical proposal and immediate
// three-party spend path while signatures are collected.
type CooperativeCloseTemplate struct {
	proposal    CooperativeCloseProposal
	previousOut *wire.TxOut
	spendPath   *arkscript.SpendPath
}

// NewCooperativeCloseTemplate builds a direct settlement from the same clean
// commitment height and balance allocation observed by both lnd endpoints.
func NewCooperativeCloseTemplate(terms Terms, source VTXOBinding,
	request CooperativeCloseRequest,
	clientBalance, hubBalance btcutil.Amount, commitmentHeight uint64) (
	*CooperativeCloseTemplate, error) {

	proposal, previousOut, spendPath, err := buildCooperativeCloseProposal(
		terms, source, request, clientBalance, hubBalance,
		commitmentHeight,
	)
	if err != nil {
		return nil, err
	}

	return &CooperativeCloseTemplate{
		proposal:    proposal,
		previousOut: previousOut,
		spendPath:   spendPath,
	}, nil
}

// Proposal returns an isolated copy of the unsigned settlement.
func (t *CooperativeCloseTemplate) Proposal() CooperativeCloseProposal {
	return t.proposal.Clone()
}

// SignDescriptor returns the exact cooperative tapscript descriptor after
// checking that the signing key belongs to the requested role.
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

// OperatorSignDescriptor returns the descriptor for the Ark operator's third
// signature after checking key ownership.
func (t *CooperativeCloseTemplate) OperatorSignDescriptor(terms Terms,
	keyDesc keychain.KeyDescriptor) (*input.SignDescriptor, error) {

	return t.signDescriptor(terms.VTXO.ArkOperatorKey, keyDesc)
}

// VerifySignature proves one endpoint signed the canonical proposal with the
// key assigned to its immediate cooperative policy role.
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
	tx, err := decodeCooperativeCloseTransaction(t.proposal.Transaction)
	if err != nil {
		return err
	}
	leaf := txscript.NewBaseTapLeaf(desc.WitnessScript)
	sigHash, err := txscript.CalcTapscriptSignaturehash(
		desc.SigHashes, desc.HashType, tx, desc.InputIndex,
		desc.PrevOutputFetcher, leaf,
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

// signDescriptor binds one expected key to the canonical proposal.
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
	if !bytes.Equal(
		schnorr.SerializePubKey(keyDesc.PubKey),
		schnorr.SerializePubKey(expectedKey),
	) {
		return nil, fmt.Errorf("cooperative close signing key does " +
			"not match policy role")
	}
	tx, err := decodeCooperativeCloseTransaction(t.proposal.Transaction)
	if err != nil {
		return nil, err
	}
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		t.previousOut.PkScript, t.previousOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)

	return t.spendPath.BuildSignDescriptor(
		keyDesc, t.previousOut, sigHashes, prevFetcher, 0,
	), nil
}

// Complete attaches all three signatures and validates the resulting
// settlement before returning it for durable persistence.
func (t *CooperativeCloseTemplate) Complete(terms Terms, source VTXOBinding,
	request CooperativeCloseRequest, clientSig, hubSig,
	operatorSig input.Signature) (CooperativeClose, error) {

	if clientSig == nil || hubSig == nil || operatorSig == nil {
		return CooperativeClose{}, fmt.Errorf("client, hub, and Ark " +
			"operator cooperative close signatures are required")
	}
	witness, err := t.spendPath.Witness(
		arkscript.MaybeAppendSighash(
			operatorSig, txscript.SigHashDefault,
		),
		arkscript.MaybeAppendSighash(hubSig, txscript.SigHashDefault),
		arkscript.MaybeAppendSighash(
			clientSig, txscript.SigHashDefault,
		),
	)
	if err != nil {
		return CooperativeClose{}, err
	}
	tx, err := decodeCooperativeCloseTransaction(t.proposal.Transaction)
	if err != nil {
		return CooperativeClose{}, err
	}
	tx.TxIn[0].Witness = witness
	raw, err := serializeCooperativeCloseTransaction(tx)
	if err != nil {
		return CooperativeClose{}, err
	}
	settlement := CooperativeClose{
		Proposal:    t.proposal.Clone(),
		Transaction: raw,
		TxID:        tx.TxHash(),
	}
	if err := settlement.Validate(terms, source, request); err != nil {
		return CooperativeClose{}, err
	}

	return settlement, nil
}

// buildCooperativeCloseProposal derives outputs and fee only from immutable
// channel facts and lnd's clean balance allocation.
func buildCooperativeCloseProposal(terms Terms, source VTXOBinding,
	request CooperativeCloseRequest,
	clientBalance, hubBalance btcutil.Amount, commitmentHeight uint64) (
	CooperativeCloseProposal, *wire.TxOut, *arkscript.SpendPath, error) {

	if err := terms.Validate(); err != nil {
		return CooperativeCloseProposal{}, nil, nil, err
	}
	if err := source.Validate(terms); err != nil {
		return CooperativeCloseProposal{}, nil, nil, err
	}
	if err := request.Validate(); err != nil {
		return CooperativeCloseProposal{}, nil, nil, err
	}
	if clientBalance < 0 || hubBalance < 0 ||
		clientBalance+hubBalance != terms.Capacity {
		return CooperativeCloseProposal{}, nil, nil, fmt.Errorf(
			"cooperative close balances %d + %d do not match "+
				"capacity %d", clientBalance, hubBalance,
			terms.Capacity)
	}
	reserve := source.Amount - terms.Capacity
	if reserve <= 0 {
		return CooperativeCloseProposal{}, nil, nil, fmt.Errorf(
			"channel VTXO has no cooperative close fee reserve")
	}
	policy, err := channelPolicy(terms.VTXO)
	if err != nil {
		return CooperativeCloseProposal{}, nil, nil, err
	}
	spendPath, err := policy.CooperativeSpendPath()
	if err != nil {
		return CooperativeCloseProposal{}, nil, nil, fmt.Errorf(
			"derive cooperative close path: %w", err)
	}
	if err := spendPath.VerifyBindsToPkScript(source.PkScript); err != nil {
		return CooperativeCloseProposal{}, nil, nil, fmt.Errorf(
			"validate cooperative close path: %w", err)
	}
	previousOut := &wire.TxOut{
		Value:    int64(source.Amount),
		PkScript: slices.Clone(source.PkScript),
	}

	targetFee := btcutil.Amount(0)
	var tx *wire.MsgTx
	var clientOutput, hubOutput btcutil.Amount
	converged := false
	for range 8 {
		clientOutput = clientBalance
		hubOutput = hubBalance
		reserveRemainder := reserve - targetFee
		if reserveRemainder < 0 {
			return CooperativeCloseProposal{}, nil, nil, fmt.Errorf(
				"cooperative close fee %d exceeds "+
					"reserve %d", targetFee, reserve)
		}
		if terms.Funder == PartyClient {
			clientOutput += reserveRemainder
		} else {
			hubOutput += reserveRemainder
		}
		clientOutput = trimCooperativeCloseOutput(
			clientOutput, request.ClientDeliveryScript,
		)
		hubOutput = trimCooperativeCloseOutput(
			hubOutput, request.HubDeliveryScript,
		)
		tx, err = cooperativeCloseTransaction(
			source.OutPoint, spendPath, request, clientOutput,
			hubOutput,
		)
		if err != nil {
			return CooperativeCloseProposal{}, nil, nil, err
		}
		dummySig := bytes.Repeat([]byte{1}, schnorr.SignatureSize)
		dummyWitness, err := spendPath.Witness(
			dummySig, dummySig, dummySig,
		)
		if err != nil {
			return CooperativeCloseProposal{}, nil, nil, err
		}
		tx.TxIn[0].Witness = dummyWitness
		weight := blockchain.GetTransactionWeight(btcutil.NewTx(tx))
		nextFee := request.FeeRate.FeeForWeight(
			lntypes.WeightUnit(weight),
		)
		tx.TxIn[0].Witness = nil
		if nextFee == targetFee {
			converged = true
			break
		}
		targetFee = nextFee
	}
	if !converged {
		return CooperativeCloseProposal{}, nil, nil, fmt.Errorf(
			"cooperative close fee did not converge")
	}
	if targetFee <= 0 || targetFee > reserve {
		return CooperativeCloseProposal{}, nil, nil, fmt.Errorf(
			"cooperative close fee %d exceeds usable reserve %d",
			targetFee, reserve)
	}
	fee := source.Amount - clientOutput - hubOutput
	if fee < targetFee {
		return CooperativeCloseProposal{}, nil, nil, fmt.Errorf(
			"cooperative close fee %d is below target %d", fee,
			targetFee)
	}
	raw, err := serializeCooperativeCloseTransaction(tx)
	if err != nil {
		return CooperativeCloseProposal{}, nil, nil, err
	}

	return CooperativeCloseProposal{
		Transaction:      raw,
		CommitmentHeight: commitmentHeight,
		ClientBalance:    clientBalance,
		HubBalance:       hubBalance,
		ClientOutput:     clientOutput,
		HubOutput:        hubOutput,
		Fee:              fee,
	}, previousOut, spendPath, nil
}

// trimCooperativeCloseOutput applies standard on-chain dust semantics while
// retaining the exact clean lnd balance separately in the proposal.
func trimCooperativeCloseOutput(amount btcutil.Amount,
	script []byte) btcutil.Amount {

	if amount > 0 && amount < lnwallet.DustLimitForSize(len(script)) {
		return 0
	}

	return amount
}

// cooperativeCloseTransaction builds deterministic settlement outputs.
func cooperativeCloseTransaction(source wire.OutPoint,
	spendPath *arkscript.SpendPath, request CooperativeCloseRequest,
	clientOutput, hubOutput btcutil.Amount) (*wire.MsgTx, error) {

	tx := wire.NewMsgTx(2)
	tx.LockTime = spendPath.RequiredLockTime
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: source,
		Sequence:         spendPath.RequiredSequence,
	})
	for _, output := range []struct {
		name   string
		amount btcutil.Amount
		script []byte
	}{
		{
			name:   "client",
			amount: clientOutput,
			script: request.ClientDeliveryScript,
		},
		{
			name:   "hub",
			amount: hubOutput,
			script: request.HubDeliveryScript,
		},
	} {
		if output.amount == 0 {
			continue
		}
		if output.amount < 0 {
			return nil, fmt.Errorf("%s cooperative close output "+
				"is negative", output.name)
		}
		dustLimit := lnwallet.DustLimitForSize(len(output.script))
		if output.amount < dustLimit {
			return nil, fmt.Errorf("%s cooperative close output "+
				"%d is below dust limit %d", output.name,
				output.amount, dustLimit)
		}
		tx.AddTxOut(&wire.TxOut{
			Value:    int64(output.amount),
			PkScript: slices.Clone(output.script),
		})
	}
	if len(tx.TxOut) == 0 {
		return nil, fmt.Errorf("cooperative close has no spendable " +
			"outputs")
	}
	txsort.InPlaceSort(tx)

	return tx, nil
}

// validateDeliveryScript accepts standard spendable scripts and rejects data
// outputs or malformed scripts before channel traffic is stopped.
func validateDeliveryScript(name string, script []byte) error {
	if len(script) == 0 {
		return fmt.Errorf("%s cooperative close delivery script is "+
			"required", name)
	}
	class := txscript.GetScriptClass(script)
	if class == txscript.NonStandardTy || class == txscript.NullDataTy ||
		class == txscript.PayToAnchorTy {
		return fmt.Errorf("%s cooperative close delivery script is "+
			"not spendable", name)
	}

	return nil
}

// decodeCooperativeCloseTransaction decodes one transaction with no trailing
// bytes.
func decodeCooperativeCloseTransaction(raw []byte) (*wire.MsgTx, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("cooperative close transaction is " +
			"required")
	}
	tx := wire.NewMsgTx(2)
	reader := bytes.NewReader(raw)
	if err := tx.Deserialize(reader); err != nil {
		return nil, fmt.Errorf("decode cooperative close "+
			"transaction: %w", err)
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("cooperative close transaction has %d "+
			"trailing bytes", reader.Len())
	}

	return tx, nil
}

// serializeCooperativeCloseTransaction returns canonical wire bytes.
func serializeCooperativeCloseTransaction(tx *wire.MsgTx) ([]byte, error) {
	var raw bytes.Buffer
	if err := tx.Serialize(&raw); err != nil {
		return nil, fmt.Errorf("serialize cooperative close "+
			"transaction: %w", err)
	}

	return raw.Bytes(), nil
}
