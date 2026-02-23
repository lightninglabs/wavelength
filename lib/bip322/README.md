# `bip322` Package

`lib/bip322` implements the [BIP-322](https://github.com/bitcoin/bips/blob/master/bip-0322.mediawiki)
"generic signed message format" in **full format only**.

The package builds and validates the virtual transactions described by the
spec:
- `to_spend`
- `to_sign`
- full-format signature payload (`sig`) = serialized `to_sign`

## Scope

What is implemented:
- message hash construction (`MessageHash`)
- `to_spend` construction (`BuildToSpend`)
- `to_sign` PSBT construction (`BuildToSign`)
- low-level raw `to_sign` construction (`BuildToSignTx`)
- full signature encoding/decoding (`Sig`, `DecodeSig`, base64 helpers)
- validation with BIP-322 result states (`ValidateAuthPkg`)
- application-level block window policy (`WithBlockWindow`,
  `WithCurrentBlockHeight`)
- proof-of-funds additional inputs on `to_sign`

What is intentionally not implemented:
- simple signature format (witness stack only)

## Transaction Model

### 1) Message hash

```text
message bytes
  -> SHA256_tagged("BIP0322-signed-message", message)
  -> 32-byte message hash
```

### 2) `to_spend`

`to_spend` is deterministic for `(message hash, message challenge script)`.

```text
                         message
                            |
                            v
                   BIP-340 tagged hash
                   ("BIP0322-signed-message")
                            |
                            v
                  +-------------------+
                  |     to_spend      |
                  |-------------------|
                  | version: 0        |
                  | locktime: 0       |
                  |                   |
                  | input 0:          |
                  |   prevout: 00..00 |
                  |            :ffff  |
                  |   scriptSig:      |
                  |     OP_0          |
                  |     PUSH32        |
                  |     <msg_hash>    |
                  |   sequence: 0     |
                  |                   |
                  | output 0:         |
                  |   value: 0        |
                  |   pkScript:       |
                  |     <challenge>   |
                  +-------------------+
```

```text
to_spend:
  version: 0
  locktime: 0
  vin[0]:
    prevout: 0000..0000:0xffffffff
    scriptSig: OP_0 <message_hash>
    sequence: 0
  vout[0]:
    value: 0
    scriptPubKey: <message_challenge>
```

### 3) `to_sign`

`to_sign` always spends `to_spend:0` as input 0 and has a single
`OP_RETURN` output. Additional inputs are optional and represent
proof-of-funds UTXOs.

```text
                  +-------------------+
                  |      to_sign      |
                  |-------------------|
                  | version: 0 or 2   |
                  | locktime: config  |
                  |                   |
                  | input 0:          |
                  |   prevout:        |
                  |   <to_spend_txid> |
                  |            :0     |
                  |   sequence: config|
                  |   script/witness: |
                  |   signer data     |
                  |                   |
                  | input 1..N:       |
                  |   proof-of-funds  |
                  |   UTXO inputs     |
                  |                   |
                  | output 0:         |
                  |   value: 0        |
                  |   pkScript:       |
                  |     OP_RETURN     |
                  +-------------------+
```

```text
to_sign:
  version: 0 or 2
  locktime: configurable
  vin[0]:
    prevout: <to_spend_txid>:0
    sequence: configurable (valid-at-age field on success)
    scriptSig/witness: challenge witness data
  vin[1..N] (optional):
    proof-of-funds inputs
  vout[0]:
    value: 0
    scriptPubKey: OP_RETURN
```

### 4) Signature payload (`Sig`)

In full format, signature bytes are exactly:

```text
sig = serialize(to_sign)
sig_base64 = base64(sig)
```

## Validation Flow

`ValidateAuthPkg` returns:
- `valid`
- `inconclusive`
- `invalid`

It validates using this sequence:
1. Check auth package completeness (`message`, challenge script, `sig`).
2. Rebuild deterministic `to_spend` from message/challenge.
3. Copy and structurally validate full-format `to_sign`.
4. Apply upgradeable version rule (`to_sign` version must be `0` or `2`).
5. Build prevout metadata for every input:
   - input 0 from rebuilt `to_spend`
   - inputs 1..N from `ProofPrevOutputs` map
6. Execute `txscript.NewEngine` for each input using standard verify flags.
7. Return:
   - `valid` + `ValidAtTime` = `to_sign.nLockTime`
   - `valid` + `ValidAtAge` = `to_sign.vin[0].nSequence`
   - otherwise `invalid`/`inconclusive` with reason

`inconclusive` is used when the verifier cannot fully evaluate according to
BIP-322 upgradeable behavior (for example unsupported versions/features or
missing proof prevout data).

## Block Window Layer

For application policy, this package provides `BlockWindow`:
- `ValidFromBlock` (inclusive lower bound)
- `ValidUntilBlock` (inclusive upper bound, `0` = no expiry)

This is an **application-layer wrapper** over core BIP-322 checks.

BIP-322 verification returns `valid at time T and age S`
([verification](https://bips.xyz/322#verification)), and the timelock
extension notes that applications may interpret these fields according to
their own policy ([timelocks](https://bips.xyz/322#timelocks)).

This package chooses the following policy mapping:
- `BlockWindow.ValidFromBlock` -> `to_sign.nLockTime`
- `BlockWindow.ValidUntilBlock` -> `to_sign.vin[0].nSequence`

Use:
- `BuildAndSignFullTx(..., WithBlockWindow(window), ...)` to embed the
  window while signing. This automatically sets version to 2.
- `ValidateAuthPkg(..., WithCurrentBlockHeight(height))` to enforce the
  window after core BIP-322 verification succeeds.

## Usage

### Sign with TxSigner (raw tx workflow)

Use `BuildAndSignFullTx` when you have direct access to the signing key
and implement the `TxSigner` interface. This is the simplest path — it
builds both virtual transactions, signs, and returns the signature in one
call.

```go
msg := []byte("Hello World")

// The challenge script is the scriptPubKey the signer must satisfy.
// In practice this is typically a P2TR or P2WPKH script.
challengeScript := myP2TRScript

// Sign the message. The signer fills in witness data for all inputs.
sig, err := bip322.BuildAndSignFullTx(
	msg, challengeScript,
	mySigner, // implements bip322.TxSigner
	bip322.WithToSignVersion(2),
	bip322.WithToSignLockTime(800_000),
)
if err != nil {
	return err
}

// Encode as base64 for transport.
sigB64, err := sig.EncodeBase64()
```

### Sign with PSBT (external signer workflow)

Use `BuildToSign` when the signer speaks PSBT (hardware wallets, remote
signing services). Build the unsigned PSBT, hand it off, then finalize.

```go
msg := []byte("Hello World")
challengeScript := myP2TRScript

// Step 1: Build to_spend.
msgHash := bip322.MessageHash(msg)
toSpend, err := bip322.BuildToSpend(msgHash, challengeScript)
if err != nil {
	return err
}

// Step 2: Build unsigned to_sign PSBT with witness-UTXO metadata
// already attached for the signer.
packet, err := bip322.BuildToSign(toSpend)
if err != nil {
	return err
}

// Step 3: Pass the PSBT to your external signer.
err = externalSigner.SignPSBT(packet)
if err != nil {
	return err
}

// Step 4: Finalize and extract the full-format signature.
sig, err := bip322.FinalizeToSignPSBT(packet)
if err != nil {
	return err
}

sigB64, err := sig.EncodeBase64()
```

### Sign with proof-of-funds

Append additional inputs to prove ownership of on-chain UTXOs alongside
the message signature. The signer must produce witnesses for all inputs.

```go
sig, err := bip322.BuildAndSignFullTx(
	msg, challengeScript, mySigner,

	// Append proof-of-funds UTXOs after input 0.
	bip322.WithToSignAdditionalInputs(
		bip322.AdditionalInput{
			PreviousOutPoint: fundingOutpoint,
			Sequence:         0,
			// WitnessUtxo is required so the signer can
			// compute the correct sighash.
			WitnessUtxo: &wire.TxOut{
				Value:    1_000_000,
				PkScript: fundingPkScript,
			},
		},
	),
)
```

### Verify a signature

`ValidateAuthPkg` returns a three-state result: `valid`, `invalid`, or
`inconclusive` (when upgradeable script features prevent full evaluation).

```go
// Decode the base64 signature received over the wire.
parsedSig, err := bip322.DecodeSigBase64(sigB64)
if err != nil {
	return err
}

result := bip322.ValidateAuthPkg(&bip322.AuthPkg{
	Message:          msg,
	MessageChallenge: challengeScript,
	Sig:              parsedSig,

	// For proof-of-funds verification, supply the UTXO metadata
	// for each additional input. Without this, those inputs are
	// marked inconclusive.
	ProofPrevOutputs: map[wire.OutPoint]*wire.TxOut{
		fundingOutpoint: {
			Value:    1_000_000,
			PkScript: fundingPkScript,
		},
	},
})

switch result.State {
case bip322.VerificationStateValid:
	// result.ValidAtTime = to_sign nLockTime
	// result.ValidAtAge  = to_sign vin[0] nSequence

case bip322.VerificationStateInvalid:
	// result.Reason describes the failure

case bip322.VerificationStateInconclusive:
	// result.Reason describes what couldn't be evaluated
}
```

### Sign and verify with a block window

`WithBlockWindow` embeds a block-height validity range into the signature
and automatically sets version 2. `WithCurrentBlockHeight` enforces the
window on the verification side.

```go
// Define the window: valid from block 840,000 through 840,144.
window := bip322.BlockWindow{
	ValidFromBlock:  840_000,
	ValidUntilBlock: 840_144, // 0 = no expiry
}

// Sign — version is set to 2 automatically.
sig, err := bip322.BuildAndSignFullTx(
	msg, challengeScript, mySigner,
	bip322.WithBlockWindow(window),
)
if err != nil {
	return err
}

// Verify — pass the current chain height to enforce the window.
result := bip322.ValidateAuthPkg(
	&bip322.AuthPkg{
		Message:          msg,
		MessageChallenge: challengeScript,
		Sig:              sig,
	},
	bip322.WithCurrentBlockHeight(840_050),
)
// result.State == VerificationStateValid (840_050 is in range)
```
