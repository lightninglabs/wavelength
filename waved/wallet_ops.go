package waved

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/lightningnetwork/lnd/keychain"
)

// WalletSeedFromMnemonic validates a mnemonic and wallet password and
// derives the raw seed and aezeed birthday. This is the core logic
// behind the InitWallet RPC, extracted as a package-level function so
// it can be reused by an SDK that bypasses gRPC. The seed is handed to
// the wallet backend, which persists it only inside btcwallet's own
// passphrase-encrypted key store.
func WalletSeedFromMnemonic(mnemonic []string, seedPassphrase,
	walletPassword []byte) ([rawSeedLen]byte, time.Time, error) {

	// Validate the mnemonic length.
	if len(mnemonic) != aezeed.NumMnemonicWords {
		return [rawSeedLen]byte{}, time.Time{}, fmt.Errorf("mnemonic "+
			"must be %d words, got %d", aezeed.NumMnemonicWords,
			len(mnemonic))
	}

	// Validate password length.
	if err := ValidateWalletPassword(walletPassword); err != nil {
		return [rawSeedLen]byte{}, time.Time{}, err
	}

	// Convert the string slice to an aezeed.Mnemonic array.
	var m aezeed.Mnemonic
	copy(m[:], mnemonic)

	// Derive the raw seed and birthday from the mnemonic.
	seed, birthday, err := MnemonicToSeedWithBirthday(m, seedPassphrase)
	if err != nil {
		return [rawSeedLen]byte{}, time.Time{}, fmt.Errorf("invalid "+
			"mnemonic: %w", err)
	}

	return seed, birthday, nil
}

// ValidateWalletPassword enforces the daemon-wide minimum wallet
// password length. btcwallet itself accepts any non-empty private
// passphrase, so this floor is applied at every entry point that
// accepts a user-chosen password.
func ValidateWalletPassword(walletPassword []byte) error {
	if len(walletPassword) < minPasswordLen {
		return fmt.Errorf("wallet password must be at least %d bytes",
			minPasswordLen)
	}

	return nil
}

// BuildTransferInputs looks up full VTXO descriptors from the store
// for the given outpoints and converts them into OOR transfer inputs.
// This is extracted from the SendOOR RPC handler so a future SDK can
// prepare transfer inputs without going through gRPC.
func BuildTransferInputs(ctx context.Context, store vtxo.VTXOStore,
	outpoints []wire.OutPoint) ([]oor.TransferInput, error) {

	inputs := make([]oor.TransferInput, 0, len(outpoints))

	for _, op := range outpoints {
		desc, err := store.GetVTXO(ctx, op)
		if err != nil {
			return nil, fmt.Errorf("look up VTXO %s: %w", op, err)
		}

		// The checkpoint output collab path is a 2-of-2 multisig
		// between the VTXO owner and the operator, matching the
		// VTXO's own collaborative spend path. This ensures both
		// parties must sign the Ark tx that spends the checkpoint.
		collabLeaf, err := arkscript.MultiSigCollabTapLeaf(
			desc.ClientKey.PubKey, desc.OperatorKey,
		)
		if err != nil {
			return nil, fmt.Errorf("build collab leaf for %s: %w",
				op, err)
		}

		inputs = append(inputs, oor.TransferInput{
			VTXO:             desc,
			OwnerLeafScript:  collabLeaf.Script,
			TaprootAssetRoot: desc.TaprootAssetRoot,
		})
	}

	return inputs, nil
}

// BuildCustomTransferInputs constructs OOR transfer inputs from
// explicit custom input specifications. This bypasses wallet VTXO
// selection for non-standard spend paths (e.g., vHTLC claims).
func BuildCustomTransferInputs(ctx context.Context, store vtxo.VTXOStore,
	customInputs []*waverpc.CustomOORInput,
	clientKey keychain.KeyDescriptor, operatorKey *btcec.PublicKey,
	exitDelay uint32) ([]oor.TransferInput, error) {

	inputs := make([]oor.TransferInput, 0, len(customInputs))

	for _, ci := range customInputs {
		outpoint, err := parseOutpointString(ci.Outpoint)
		if err != nil {
			return nil, fmt.Errorf("parse outpoint %q: %w",
				ci.Outpoint, err)
		}

		var desc *vtxo.Descriptor

		// If the caller provided amount and pkscript, build
		// the descriptor directly (for VTXOs not in the local
		// store, e.g., received via OOR).
		if ci.AmountSat > 0 && len(ci.PkScript) > 0 {
			desc = &vtxo.Descriptor{
				Outpoint: outpoint,
				Amount:   btcutil.Amount(ci.AmountSat),
				PolicyTemplate: append(
					[]byte(nil), ci.VtxoPolicyTemplate...,
				),
				PkScript:       ci.PkScript,
				ClientKey:      clientKey,
				OperatorKey:    operatorKey,
				RelativeExpiry: exitDelay,
			}
		} else {
			// Fall back to store lookup.
			desc, err = store.GetVTXO(ctx, outpoint)
			if err != nil {
				return nil, fmt.Errorf("look up VTXO %s: %w",
					outpoint, err)
			}
		}

		var (
			ownerLeaf       []byte
			ownerLeafPolicy []byte
		)

		// Validate the policy template against Ark invariants
		// when one is provided. This catches malformed policies
		// before they reach the server.
		if len(ci.VtxoPolicyTemplate) > 0 {
			template, err := arkscript.DecodePolicyTemplate(
				ci.VtxoPolicyTemplate,
			)
			if err != nil {
				return nil, fmt.Errorf("decode policy for "+
					"%s: %w", outpoint, err)
			}

			// When the caller also supplied a pkScript, verify
			// the policy template compiles to that pkScript. This
			// binds the semantic policy to the on-chain output
			// before any signature is produced and closes the
			// signature-oracle path where a caller could claim
			// one policy on the wire and produce signatures
			// against an unrelated tap tree.
			if len(ci.PkScript) > 0 &&
				!template.MatchesPkScript(ci.PkScript) {
				return nil, fmt.Errorf("policy template for "+
					"%s does not match supplied pkScript",
					outpoint)
			}

			nodes := make(
				[]arkscript.Node, len(template.Leaves),
			)
			for i, leaf := range template.Leaves {
				nodes[i] = leaf.Node
			}

			// Custom OOR inputs (e.g. vHTLC claims) have
			// protocol-specific unilateral delays independent of
			// the operator's standard VTXO exit delay. Validate
			// structural invariants (collab leaf, exit leaf,
			// CSV-gated exits, no operator-unilateral spend) but
			// do not impose the standard-VTXO MinExitDelay
			// minimum here.
			err = arkscript.ValidatePolicy(
				nodes, arkscript.PolicyValidationOpts{
					OperatorKey: operatorKey,
				},
			)
			if err != nil {
				return nil, fmt.Errorf("invalid policy for "+
					"%s: %w", outpoint, err)
			}

			if len(ci.SpendPath) > 0 {
				ownerLeaf, ownerLeafPolicy, err =
					findSettlementOwnerLeaf(
						template, clientKey.PubKey,
						operatorKey, ci.SpendPath,
					)
				if err != nil {
					return nil, fmt.Errorf("derive "+
						"settlement owner leaf for "+
						"%s: %w", outpoint, err)
				}
			}
		}

		input := oor.TransferInput{
			VTXO:               desc,
			VTXOPolicyTemplate: ci.VtxoPolicyTemplate,
			OwnerLeafScript:    ownerLeaf,
			OwnerLeafPolicy:    ownerLeafPolicy,
		}

		if len(ci.SpendPath) > 0 {
			spendPath, err := arkscript.DecodeSpendPath(
				ci.SpendPath,
			)
			if err != nil {
				return nil, fmt.Errorf("decode spend path for "+
					"%s: %w", outpoint, err)
			}

			// The spend path carries its own witness script and
			// control block. Verify the control block commits to
			// a taproot tree whose output key is exactly the VTXO
			// pkScript. Without this check a caller could supply a
			// control block for an unrelated tap tree and coerce
			// the wallet into emitting a Schnorr signature over an
			// attacker-chosen tapscript.
			if err := spendPath.VerifyBindsToPkScript(
				desc.PkScript,
			); err != nil {
				return nil, fmt.Errorf("spend path for %s "+
					"does not bind to VTXO pkScript: %w",
					outpoint, err)
			}

			input.CustomSpend = spendPath
		}

		input.ExternalSignatures, err = customTaprootScriptSignatures(
			ci.ExternalSignatures,
		)
		if err != nil {
			return nil, fmt.Errorf("decode external signatures "+
				"for %s: %w", outpoint, err)
		}

		inputs = append(inputs, input)
	}

	return inputs, nil
}

// customTaprootScriptSignatures decodes RPC external signature records into
// the OOR domain representation.
func customTaprootScriptSignatures(rpcSigs []*waverpc.TaprootScriptSignature) (
	[]oor.ExternalTaprootScriptSignature, error) {

	result := make([]oor.ExternalTaprootScriptSignature, 0, len(rpcSigs))
	for i, rpcSig := range rpcSigs {
		if rpcSig == nil {
			return nil, fmt.Errorf("signature %d is nil", i)
		}

		pubKey, err := btcec.ParsePubKey(rpcSig.GetPubkey())
		if err != nil {
			return nil, fmt.Errorf("parse pubkey %d: %w", i, err)
		}

		if len(rpcSig.GetWitnessScript()) == 0 {
			return nil, fmt.Errorf("signature %d witness script "+
				"is required", i)
		}

		if len(rpcSig.GetSignature()) == 0 {
			return nil, fmt.Errorf("signature %d is required", i)
		}

		result = append(result, oor.ExternalTaprootScriptSignature{
			PubKey:        pubKey,
			WitnessScript: bytes.Clone(rpcSig.GetWitnessScript()),
			Signature:     bytes.Clone(rpcSig.GetSignature()),
			SigHash: txscript.SigHashType(
				rpcSig.GetSighash(),
			),
		})
	}

	return result, nil
}

// findSettlementOwnerLeaf maps a custom auth spend path to the
// operator-backed forfeit leaf that the later Ark tx must use for the
// checkpoint output owner path.
func findSettlementOwnerLeaf(template *arkscript.PolicyTemplate,
	participant, operator *btcec.PublicKey, rawSpendPath []byte) ([]byte,
	[]byte, error) {

	if template == nil {
		return nil, nil, fmt.Errorf("policy template is required")
	}

	if participant == nil {
		return nil, nil, fmt.Errorf("participant key is required")
	}

	if operator == nil {
		return nil, nil, fmt.Errorf("operator key is required")
	}

	spendPath, err := arkscript.DecodeSpendPath(rawSpendPath)
	if err != nil {
		return nil, nil, fmt.Errorf("decode spend path: %w", err)
	}

	// Prefer the exact collaborative leaf when the caller is
	// already spending an operator-backed branch such as the
	// vHTLC claim/refund closures.
	for _, leaf := range template.Leaves {
		script, err := leaf.Script()
		if err != nil {
			return nil, nil, fmt.Errorf("compile settlement "+
				"leaf: %w", err)
		}

		if !bytes.Equal(script, spendPath.WitnessScript) {
			continue
		}

		if !localOperatorOnlyLeaf(
			leaf.Node, participant, operator,
		) {
			return nil, nil, nil
		}

		encodedLeaf, err := leaf.Encode()
		if err != nil {
			return nil, nil, fmt.Errorf("encode settlement "+
				"leaf: %w", err)
		}

		return bytes.Clone(script), encodedLeaf, nil
	}

	// Defensive fallback: every current Ark policy enumerates both the
	// auth and forfeit leaves in template.Leaves, so the loop above
	// always resolves the caller's spend path. This second stage
	// handles future policies whose auth path is derived rather than
	// enumerated; spendPathsMatch intentionally ignores runtime
	// condition witnesses so a claim-style spend with a preimage still
	// matches its derived AuthPath.
	pairs, err := template.SettlementPairsForParticipant(
		participant, operator,
	)
	if err != nil {
		return nil, nil, err
	}

	for _, pair := range pairs {
		if !spendPathsMatch(spendPath, pair.AuthPath) {
			continue
		}

		for _, leaf := range template.Leaves {
			script, err := leaf.Script()
			if err != nil {
				return nil, nil, fmt.Errorf("compile "+
					"settlement leaf: %w", err)
			}

			if !bytes.Equal(
				script, pair.ForfeitPath.WitnessScript,
			) {

				continue
			}

			if !localOperatorOnlyLeaf(
				leaf.Node, participant, operator,
			) {
				return nil, nil, nil
			}

			encodedLeaf, err := leaf.Encode()
			if err != nil {
				return nil, nil, fmt.Errorf("encode "+
					"settlement leaf: %w", err)
			}

			return bytes.Clone(script), encodedLeaf, nil
		}

		return nil, nil, fmt.Errorf("forfeit leaf not found in policy")
	}

	return nil, nil, fmt.Errorf("no settlement pair matches spend path")
}

// localOperatorOnlyLeaf reports whether the leaf's signing keys are exactly
// the local participant and Ark operator. Other custom policy leaves still
// remain valid as the checkpoint input spend, but the checkpoint output owner
// path must be signable by the local OOR actor plus the operator cosigner.
func localOperatorOnlyLeaf(node arkscript.Node,
	participant, operator *btcec.PublicKey) bool {

	keys := firstMultisigKeys(node)
	if len(keys) != 2 {
		return false
	}

	return sameXOnlyPubKey(keys[0], participant) &&
		sameXOnlyPubKey(keys[1], operator)
}

// firstMultisigKeys extracts the signing key set from the first multisig node.
func firstMultisigKeys(node arkscript.Node) []*btcec.PublicKey {
	switch n := node.(type) {
	case *arkscript.Multisig:
		return append([]*btcec.PublicKey(nil), n.Keys...)

	case *arkscript.Condition:
		return firstMultisigKeys(n.Inner)

	case *arkscript.CSV:
		return firstMultisigKeys(n.Inner)

	default:
		return nil
	}
}

// sameXOnlyPubKey compares pubkeys by their x-only taproot encoding.
func sameXOnlyPubKey(a, b *btcec.PublicKey) bool {
	if a == nil || b == nil {
		return false
	}

	return bytes.Equal(
		schnorr.SerializePubKey(a), schnorr.SerializePubKey(b),
	)
}

// spendPathsMatch reports whether two semantic spend paths describe the same
// authenticated policy branch. Witness conditions are compared separately at
// spend time and are not part of the branch identity.
func spendPathsMatch(a, b *arkscript.SpendPath) bool {
	switch {
	case a == nil || b == nil:
		return false

	case !bytes.Equal(a.WitnessScript, b.WitnessScript):
		return false

	case !bytes.Equal(a.ControlBlock, b.ControlBlock):
		return false

	case a.RequiredSequence != b.RequiredSequence:
		return false

	case a.RequiredLockTime != b.RequiredLockTime:
		return false
	}

	return true
}
