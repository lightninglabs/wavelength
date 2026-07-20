package types

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// VTXOClaimNonceSize is the required replay-prevention nonce length.
	VTXOClaimNonceSize = 32

	// VTXOClaimSignatureSize is the serialized BIP-340 signature length.
	VTXOClaimSignatureSize = 64

	vtxoClaimVersion uint64 = 1
	vtxoClaimDomain         = "wavelength-vtxo-claim"
)

// VTXOClaimAuthTag returns the BIP-340 tag used to sign VTXO claim
// authorization messages. A fresh byte slice is returned so callers cannot
// mutate the package-level domain separator.
func VTXOClaimAuthTag() []byte {
	return []byte(vtxoClaimDomain)
}

const (
	vtxoClaimVersionRecordType        tlv.Type = 1
	vtxoClaimDomainRecordType         tlv.Type = 2
	vtxoClaimServerKeyRecordType      tlv.Type = 3
	vtxoClaimRoundIDRecordType        tlv.Type = 4
	vtxoClaimSourceHashRecordType     tlv.Type = 5
	vtxoClaimSourceIndexRecordType    tlv.Type = 6
	vtxoClaimAmountRecordType         tlv.Type = 7
	vtxoClaimPolicyHashRecordType     tlv.Type = 8
	vtxoClaimPkScriptHashRecordType   tlv.Type = 9
	vtxoClaimParticipantRecordType    tlv.Type = 10
	vtxoClaimReplacementKeyRecordType tlv.Type = 11
	vtxoClaimNonceRecordType          tlv.Type = 12
	vtxoClaimValidFromRecordType      tlv.Type = 13
	vtxoClaimValidUntilRecordType     tlv.Type = 14
)

// VTXOClaimAuthMessage returns the deterministic TLV message authorized by a
// VTXO claim signature. The operator derives the amount and policy metadata
// from its source registry entry before reproducing this message. roundID must
// identify the exact round requested by the client. Signature is intentionally
// excluded.
func VTXOClaimAuthMessage(claim *VTXOClaimInput, serverKey *btcec.PublicKey,
	roundID []byte, amount btcutil.Amount, policyTemplate,
	pkScript []byte) ([]byte, error) {

	if claim == nil {
		return nil, fmt.Errorf("vtxo claim must be provided")
	}
	if serverKey == nil {
		return nil, fmt.Errorf("server key must be provided")
	}
	if amount < 0 {
		return nil, fmt.Errorf("claim amount must be non-negative")
	}
	if len(policyTemplate) == 0 {
		return nil, fmt.Errorf("policy template must be provided")
	}
	if len(pkScript) == 0 {
		return nil, fmt.Errorf("pkScript must be provided")
	}
	if len(roundID) == 0 {
		return nil, fmt.Errorf("claim round ID must be provided")
	}
	if claim.ParticipantPubKey == nil {
		return nil, fmt.Errorf("participant pubkey must be provided")
	}
	if claim.ReplacementSigningKey.PubKey == nil {
		return nil, fmt.Errorf("replacement signing pubkey must be " +
			"provided")
	}
	if claim.Nonce == ([VTXOClaimNonceSize]byte{}) {
		return nil, fmt.Errorf("claim nonce must be non-zero")
	}
	if claim.ValidUntil == 0 || claim.ValidUntil < claim.ValidFrom {
		return nil, fmt.Errorf("invalid claim validity window %d..%d",
			claim.ValidFrom, claim.ValidUntil)
	}

	version := vtxoClaimVersion
	domain := []byte(vtxoClaimDomain)
	server := serverKey.SerializeCompressed()
	round := bytes.Clone(roundID)
	sourceHash := bytes.Clone(claim.SourceOutpoint.Hash[:])
	sourceIndex := uint64(claim.SourceOutpoint.Index)
	amountSat := uint64(amount)
	policyHash := sha256.Sum256(policyTemplate)
	scriptHash := sha256.Sum256(pkScript)
	policyDigest := bytes.Clone(policyHash[:])
	scriptDigest := bytes.Clone(scriptHash[:])
	participant := claim.ParticipantPubKey.SerializeCompressed()
	replacement := claim.ReplacementSigningKey.PubKey.SerializeCompressed()
	nonce := bytes.Clone(claim.Nonce[:])
	validFrom := uint64(claim.ValidFrom)
	validUntil := uint64(claim.ValidUntil)

	return encodeJoinAuthTLV([]tlv.Record{
		tlv.MakePrimitiveRecord(vtxoClaimVersionRecordType, &version),
		tlv.MakePrimitiveRecord(vtxoClaimDomainRecordType, &domain),
		tlv.MakePrimitiveRecord(vtxoClaimServerKeyRecordType, &server),
		tlv.MakePrimitiveRecord(vtxoClaimRoundIDRecordType, &round),
		tlv.MakePrimitiveRecord(
			vtxoClaimSourceHashRecordType, &sourceHash,
		),
		tlv.MakePrimitiveRecord(
			vtxoClaimSourceIndexRecordType, &sourceIndex,
		),
		tlv.MakePrimitiveRecord(vtxoClaimAmountRecordType, &amountSat),
		tlv.MakePrimitiveRecord(
			vtxoClaimPolicyHashRecordType, &policyDigest,
		),
		tlv.MakePrimitiveRecord(
			vtxoClaimPkScriptHashRecordType, &scriptDigest,
		),
		tlv.MakePrimitiveRecord(
			vtxoClaimParticipantRecordType, &participant,
		),
		tlv.MakePrimitiveRecord(
			vtxoClaimReplacementKeyRecordType, &replacement,
		),
		tlv.MakePrimitiveRecord(vtxoClaimNonceRecordType, &nonce),
		tlv.MakePrimitiveRecord(
			vtxoClaimValidFromRecordType, &validFrom,
		),
		tlv.MakePrimitiveRecord(
			vtxoClaimValidUntilRecordType, &validUntil,
		),
	})
}

// VTXOClaimAuthDigest returns the BIP-340 tagged hash signed by the
// participant.
func VTXOClaimAuthDigest(claim *VTXOClaimInput, serverKey *btcec.PublicKey,
	roundID []byte, amount btcutil.Amount, policyTemplate,
	pkScript []byte) ([32]byte, error) {

	message, err := VTXOClaimAuthMessage(
		claim, serverKey, roundID, amount, policyTemplate, pkScript,
	)
	if err != nil {
		return [32]byte{}, err
	}

	digest := chainhash.TaggedHash([]byte(vtxoClaimDomain), message)

	return [32]byte(*digest), nil
}

// VerifyVTXOClaimAuth verifies the claim's structural fields and Schnorr
// signature. Registry policy membership and current block-height validity
// remain the operator's responsibility.
func VerifyVTXOClaimAuth(claim *VTXOClaimInput, serverKey *btcec.PublicKey,
	roundID []byte, amount btcutil.Amount, policyTemplate,
	pkScript []byte) error {

	digest, err := VTXOClaimAuthDigest(
		claim, serverKey, roundID, amount, policyTemplate, pkScript,
	)
	if err != nil {
		return err
	}
	if len(claim.Signature) != VTXOClaimSignatureSize {
		return fmt.Errorf("claim signature must be %d bytes",
			VTXOClaimSignatureSize)
	}

	sig, err := schnorr.ParseSignature(claim.Signature)
	if err != nil {
		return fmt.Errorf("parse claim signature: %w", err)
	}
	if !sig.Verify(digest[:], claim.ParticipantPubKey) {
		return fmt.Errorf("invalid claim signature")
	}

	return nil
}
