package bip322

import (
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

const (
	// toSpendVersion is the fixed transaction version for BIP-322
	// to_spend transactions.
	toSpendVersion int32 = 0

	// toSpendLockTime is the fixed locktime for BIP-322 to_spend
	// transactions.
	toSpendLockTime uint32 = 0

	// toSpendSequence is the fixed input sequence for BIP-322 to_spend
	// transactions.
	toSpendSequence uint32 = 0

	// toSignDefaultVersion is the default version for to_sign transactions.
	toSignDefaultVersion int32 = 0

	// toSignDefaultLockTime is the default locktime for to_sign
	// transactions.
	toSignDefaultLockTime uint32 = 0

	// toSignDefaultSequence is the default sequence for the first to_sign
	// input.
	toSignDefaultSequence uint32 = 0
)

// AdditionalInput describes an extra to_sign input used for proof-of-funds.
type AdditionalInput struct {
	// PreviousOutPoint identifies the output this additional input spends.
	PreviousOutPoint wire.OutPoint

	// Sequence sets nSequence for this additional input.
	Sequence uint32

	// SignatureScript sets scriptSig for this additional input.
	SignatureScript []byte

	// Witness sets scriptWitness for this additional input.
	Witness wire.TxWitness

	// WitnessUtxo provides prevout metadata required by PSBT signers.
	WitnessUtxo *wire.TxOut
}

// ToSignOption configures to_sign transaction construction.
type ToSignOption func(*toSignBuildOptions) error

// toSignBuildOptions is the internal configuration for BuildToSign.
type toSignBuildOptions struct {
	version          int32
	lockTime         uint32
	sequence         uint32
	signatureScript  []byte
	witness          wire.TxWitness
	additionalInputs []AdditionalInput
}

// WithToSignVersion sets to_sign nVersion. BIP-322's upgradeable rule permits
// 0 or 2.
func WithToSignVersion(version int32) ToSignOption {
	return func(opts *toSignBuildOptions) error {
		opts.version = version

		return nil
	}
}

// WithToSignLockTime sets to_sign nLockTime.
func WithToSignLockTime(lockTime uint32) ToSignOption {
	return func(opts *toSignBuildOptions) error {
		opts.lockTime = lockTime

		return nil
	}
}

// WithToSignSequence sets nSequence of the first to_sign input.
func WithToSignSequence(sequence uint32) ToSignOption {
	return func(opts *toSignBuildOptions) error {
		opts.sequence = sequence

		return nil
	}
}

// WithToSignSignatureScript sets scriptSig for the first to_sign input.
func WithToSignSignatureScript(signatureScript []byte) ToSignOption {
	return func(opts *toSignBuildOptions) error {
		opts.signatureScript = cloneBytes(signatureScript)

		return nil
	}
}

// WithToSignWitness sets scriptWitness for the first to_sign input.
func WithToSignWitness(witness wire.TxWitness) ToSignOption {
	return func(opts *toSignBuildOptions) error {
		opts.witness = cloneWitness(witness)

		return nil
	}
}

// WithToSignAdditionalInputs appends proof-of-funds inputs after input 0.
func WithToSignAdditionalInputs(inputs ...AdditionalInput) ToSignOption {
	return func(opts *toSignBuildOptions) error {
		copied := make([]AdditionalInput, len(inputs))
		for i := 0; i < len(inputs); i++ {
			copied[i] = cloneAdditionalInput(inputs[i])
		}

		opts.additionalInputs = copied

		return nil
	}
}

// defaultToSignBuildOptions returns default options for BuildToSign.
func defaultToSignBuildOptions() toSignBuildOptions {
	return toSignBuildOptions{
		version:  toSignDefaultVersion,
		lockTime: toSignDefaultLockTime,
		sequence: toSignDefaultSequence,
	}
}

// applyToSignOptions applies functional options to default build settings.
func applyToSignOptions(opts []ToSignOption) (toSignBuildOptions, error) {
	buildOpts := defaultToSignBuildOptions()

	for i := 0; i < len(opts); i++ {
		opt := opts[i]
		if opt == nil {
			return toSignBuildOptions{}, fmt.Errorf("to_sign "+
				"option %d must be provided", i)
		}

		err := opt(&buildOpts)
		if err != nil {
			return toSignBuildOptions{}, err
		}
	}

	return buildOpts, nil
}

// cloneAdditionalInput deep-copies an AdditionalInput and all nested script
// buffers.
func cloneAdditionalInput(src AdditionalInput) AdditionalInput {
	var witnessUtxo *wire.TxOut
	if src.WitnessUtxo != nil {
		witnessUtxo = cloneTxOut(src.WitnessUtxo)
	}

	return AdditionalInput{
		PreviousOutPoint: src.PreviousOutPoint,
		Sequence:         src.Sequence,
		SignatureScript:  cloneBytes(src.SignatureScript),
		Witness:          cloneWitness(src.Witness),
		WitnessUtxo:      witnessUtxo,
	}
}

// BuildToSpend constructs the deterministic BIP-322 to_spend virtual
// transaction from a pre-computed message hash and a challenge script
// (the scriptPubKey the signer must satisfy).
//
// The to_spend transaction is never broadcast; it exists only so that the
// to_sign transaction has something to spend. Its structure is fully
// determined by the (msgHash, msgChallenge) pair:
//
//	+------------------------------------------+
//	|               to_spend                   |
//	|------------------------------------------|
//	| version:  0          locktime: 0         |
//	|                                          |
//	| vin[0]:                                  |
//	|   prevout:  0x00…00:0xffffffff           |
//	|   scriptSig: OP_0 PUSH32 <msgHash>       |
//	|   sequence: 0                            |
//	|                                          |
//	| vout[0]:                                 |
//	|   value:    0                            |
//	|   pkScript: <msgChallenge>               |
//	+------------------------------------------+
//
// The input spends a null outpoint (all-zero txid, index 0xffffffff) with a
// scriptSig that commits to the message hash via OP_0 || PUSH32. The single
// output carries the challenge script that the corresponding to_sign
// transaction must satisfy with a valid witness.
func BuildToSpend(msgHash [32]byte, msgChallenge []byte) (*wire.MsgTx, error) {
	if len(msgChallenge) == 0 {
		return nil, fmt.Errorf("message challenge script must be " +
			"provided")
	}

	messageCommitment, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_0).
		AddData(msgHash[:]).
		Script()
	if err != nil {
		return nil, fmt.Errorf("build to_spend message commitment: %w",
			err)
	}

	tx := wire.NewMsgTx(toSpendVersion)
	tx.LockTime = toSpendLockTime

	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: wire.MaxPrevOutIndex,
		},
		Sequence:        toSpendSequence,
		SignatureScript: messageCommitment,
		Witness:         wire.TxWitness{},
	})

	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: cloneBytes(msgChallenge),
	})

	return tx, nil
}

// BuildToSign constructs an unsigned BIP-322 to_sign PSBT that spends
// output 0 of the provided to_spend transaction.
//
// This is the PSBT signing workflow: the caller passes the returned packet
// to an external PSBT-aware signer (hardware wallet, remote signing
// service, etc.), then calls FinalizeToSignPSBT to extract the
// full-format signature. For callers that have direct access to signing
// keys and implement TxSigner, use BuildToSignTx (or the all-in-one
// BuildAndSignFullTx) instead.
//
// Input 0 always references to_spend:0 and carries the challenge witness.
// Optional proof-of-funds inputs (1..N) can be appended via
// WithToSignAdditionalInputs. The transaction has a single OP_RETURN
// output with zero value.
//
//	+------------------------------------------+
//	|               to_sign (PSBT)             |
//	|------------------------------------------|
//	| version:  0 or 2     locktime: config    |
//	|                                          |
//	| vin[0]:                                  |
//	|   prevout:  <to_spend txid>:0            |
//	|   sequence: config                       |
//	|   (unsigned — signer fills witness)      |
//	|                                          |
//	| vin[1..N]: (optional)                    |
//	|   proof-of-funds UTXO inputs             |
//	|                                          |
//	| vout[0]:                                 |
//	|   value:    0                            |
//	|   pkScript: OP_RETURN                    |
//	+------------------------------------------+
//
// Witness-UTXO and sighash metadata are automatically attached to every
// PSBT input so that downstream signers have the context they need.
func BuildToSign(toSpend *wire.MsgTx,
	opts ...ToSignOption) (*psbt.Packet, error) {

	buildOpts, err := applyAndValidateToSignOptions(toSpend, opts)
	if err != nil {
		return nil, err
	}

	err = validateToSignPSBTOptions(buildOpts)
	if err != nil {
		return nil, err
	}

	toSignTx, err := buildToSignTxFromOptions(toSpend, buildOpts)
	if err != nil {
		return nil, err
	}

	packet, err := psbt.NewFromUnsignedTx(toSignTx)
	if err != nil {
		return nil, fmt.Errorf("build to_sign psbt: %w", err)
	}

	err = attachToSignPSBTInputMetadata(
		packet, toSpend, buildOpts.additionalInputs,
	)
	if err != nil {
		return nil, err
	}

	return packet, nil
}

// BuildToSignTx constructs a raw BIP-322 to_sign transaction (not wrapped
// in PSBT) that spends output 0 of the provided to_spend transaction.
//
// This is the raw-tx signing workflow intended for callers that implement
// TxSigner and sign the transaction directly. BuildAndSignFullTx uses
// this internally. For callers that need PSBT-based signing (hardware
// wallets, external signers), use BuildToSign instead.
func BuildToSignTx(toSpend *wire.MsgTx,
	opts ...ToSignOption) (*wire.MsgTx, error) {

	buildOpts, err := applyAndValidateToSignOptions(toSpend, opts)
	if err != nil {
		return nil, err
	}

	return buildToSignTxFromOptions(toSpend, buildOpts)
}

// applyAndValidateToSignOptions applies functional options and validates
// common to_sign parameters shared by tx and PSBT builders.
func applyAndValidateToSignOptions(toSpend *wire.MsgTx,
	opts []ToSignOption) (toSignBuildOptions, error) {

	if toSpend == nil {
		return toSignBuildOptions{}, fmt.Errorf("to_spend " +
			"transaction must be provided")
	}

	buildOpts, err := applyToSignOptions(opts)
	if err != nil {
		return toSignBuildOptions{}, err
	}

	err = validateToSignVersion(buildOpts.version)
	if err != nil {
		return toSignBuildOptions{}, err
	}

	return buildOpts, nil
}

// buildToSignTxFromOptions constructs the to_sign transaction from validated
// internal builder options.
func buildToSignTxFromOptions(toSpend *wire.MsgTx,
	buildOpts toSignBuildOptions) (*wire.MsgTx, error) {

	opReturnScript, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_RETURN).
		Script()
	if err != nil {
		return nil, fmt.Errorf("build to_sign OP_RETURN output: %w",
			err)
	}

	tx := wire.NewMsgTx(buildOpts.version)
	tx.LockTime = buildOpts.lockTime

	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  toSpend.TxHash(),
			Index: 0,
		},
		Sequence:        buildOpts.sequence,
		SignatureScript: cloneBytes(buildOpts.signatureScript),
		Witness:         cloneWitness(buildOpts.witness),
	})

	for i := 0; i < len(buildOpts.additionalInputs); i++ {
		additionalIn := buildOpts.additionalInputs[i]

		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: additionalIn.PreviousOutPoint,
			Sequence:         additionalIn.Sequence,
			SignatureScript: cloneBytes(
				additionalIn.SignatureScript,
			),
			Witness: cloneWitness(additionalIn.Witness),
		})
	}

	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: opReturnScript,
	})

	return tx, nil
}

// validateToSignPSBTOptions validates options specific to PSBT-based to_sign
// construction.
func validateToSignPSBTOptions(opts toSignBuildOptions) error {
	if len(opts.signatureScript) != 0 {
		return fmt.Errorf("to_sign psbt input 0 signature script " +
			"must be empty")
	}

	if len(opts.witness) != 0 {
		return fmt.Errorf("to_sign psbt input 0 witness must be empty")
	}

	for i := 0; i < len(opts.additionalInputs); i++ {
		additionalIn := opts.additionalInputs[i]

		if len(additionalIn.SignatureScript) != 0 {
			return fmt.Errorf("to_sign psbt input %d signature "+
				"script must be empty", i+1)
		}

		if len(additionalIn.Witness) != 0 {
			return fmt.Errorf("to_sign psbt input %d witness must "+
				"be empty", i+1)
		}

		if additionalIn.WitnessUtxo == nil {
			return fmt.Errorf("to_sign psbt input %d witness utxo "+
				"must be provided", i+1)
		}

		if len(additionalIn.WitnessUtxo.PkScript) == 0 {
			return fmt.Errorf("to_sign psbt input %d witness utxo "+
				"script must be provided", i+1)
		}
	}

	return nil
}

// attachToSignPSBTInputMetadata writes witness-utxo and sighash metadata for
// all to_sign inputs into the PSBT packet.
func attachToSignPSBTInputMetadata(packet *psbt.Packet, toSpend *wire.MsgTx,
	additionalInputs []AdditionalInput) error {

	if packet == nil {
		return fmt.Errorf("to_sign psbt must be provided")
	}

	if toSpend == nil {
		return fmt.Errorf("to_spend transaction must be provided")
	}

	if len(toSpend.TxOut) == 0 {
		return fmt.Errorf("to_spend output must be provided")
	}

	updater, err := psbt.NewUpdater(packet)
	if err != nil {
		return fmt.Errorf("create to_sign psbt updater: %w", err)
	}

	err = updater.AddInWitnessUtxo(cloneTxOut(toSpend.TxOut[0]), 0)
	if err != nil {
		return fmt.Errorf("attach to_sign input 0 witness utxo: %w",
			err)
	}

	toSpendSighashType := psbtInputSighashType(toSpend.TxOut[0].PkScript)

	err = updater.AddInSighashType(toSpendSighashType, 0)
	if err != nil {
		return fmt.Errorf("attach to_sign input 0 sighash type: %w",
			err)
	}

	for i := 0; i < len(additionalInputs); i++ {
		packetIndex := i + 1
		additionalIn := additionalInputs[i]

		err = updater.AddInWitnessUtxo(
			cloneTxOut(additionalIn.WitnessUtxo), packetIndex,
		)
		if err != nil {
			return fmt.Errorf("attach to_sign input %d witness "+
				"utxo: %w", packetIndex, err)
		}

		additionalSighashType := psbtInputSighashType(
			additionalIn.WitnessUtxo.PkScript,
		)

		err = updater.AddInSighashType(
			additionalSighashType, packetIndex,
		)
		if err != nil {
			return fmt.Errorf("attach to_sign input %d sighash "+
				"type: %w", packetIndex, err)
		}
	}

	return nil
}

// psbtInputSighashType returns the default sighash metadata to attach for a
// PSBT input based on its prevout script type.
func psbtInputSighashType(pkScript []byte) txscript.SigHashType {
	if txscript.IsPayToTaproot(pkScript) {
		return txscript.SigHashDefault
	}

	return txscript.SigHashAll
}

// validateToSignVersion enforces the BIP-322 upgradeable rule for transaction
// version values.
func validateToSignVersion(version int32) error {
	switch version {
	case 0:
		return nil

	case 2:
		return nil

	default:
		return fmt.Errorf("to_sign version must be 0 or 2, got %d",
			version)
	}
}
