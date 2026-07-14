package bip322

import (
	"bytes"
	"fmt"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// intentMessageVersion is the canonical encoding version for
	// Intent.SigningMessage.
	intentMessageVersion uint64 = 1

	// intentMessageDomainTag domain-separates BIP-322 intent payloads from
	// other message formats.
	intentMessageDomainTag = "wavelength-bip322-intent"
)

const (
	intentMessageVersionRecordType    tlv.Type = 1
	intentMessageDomainRecordType     tlv.Type = 2
	intentMessageValidFromRecordType  tlv.Type = 3
	intentMessageValidUntilRecordType tlv.Type = 4
	intentMessagePayloadRecordType    tlv.Type = 5
)

// Intent defines application-layer validity metadata for a signed payload.
//
// Payload is the canonical application message that should be authorized.
// ValidFrom and ValidUntil define the height range where the signature is
// accepted by the application.
type Intent struct {
	// Payload is the canonical application payload being authorized.
	Payload []byte

	// ValidFrom is the first block height where this intent is accepted.
	ValidFrom uint32

	// ValidUntil is the last block height where this intent is accepted.
	//
	// A value of 0 means there is no upper bound.
	ValidUntil uint32
}

// NewIntent builds and validates an intent from application payload and
// validity metadata.
func NewIntent(payload []byte, validFrom uint32,
	validUntil uint32) (*Intent, error) {

	intent := &Intent{
		Payload:    bytes.Clone(payload),
		ValidFrom:  validFrom,
		ValidUntil: validUntil,
	}

	err := intent.Validate()
	if err != nil {
		return nil, err
	}

	return intent, nil
}

// Validate checks whether the intent is internally consistent.
func (i *Intent) Validate() error {
	if i == nil {
		return fmt.Errorf("intent must be provided")
	}

	if len(i.Payload) == 0 {
		return fmt.Errorf("intent payload must be provided")
	}

	if i.ValidUntil != 0 && i.ValidUntil < i.ValidFrom {
		return fmt.Errorf("valid-until block %d must be greater than "+
			"or equal to valid-from block %d", i.ValidUntil,
			i.ValidFrom)
	}

	return nil
}

// ValidateAtHeight checks whether this intent is valid at the given chain
// height.
func (i *Intent) ValidateAtHeight(currentBlockHeight uint32) error {
	err := i.Validate()
	if err != nil {
		return err
	}

	if currentBlockHeight < i.ValidFrom {
		return fmt.Errorf("signature not yet valid: current block %d "+
			"is below valid-from block %d", currentBlockHeight,
			i.ValidFrom)
	}

	if i.ValidUntil != 0 && currentBlockHeight > i.ValidUntil {
		return fmt.Errorf("signature expired: current block %d is "+
			"above valid-until block %d", currentBlockHeight,
			i.ValidUntil)
	}

	return nil
}

// SigningMessage serializes the intent into deterministic bytes that should
// be hashed by BIP-322.
func (i *Intent) SigningMessage() ([]byte, error) {
	err := i.Validate()
	if err != nil {
		return nil, err
	}

	version := intentMessageVersion
	domain := []byte(intentMessageDomainTag)
	validFrom := uint64(i.ValidFrom)
	validUntil := uint64(i.ValidUntil)
	payload := bytes.Clone(i.Payload)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			intentMessageVersionRecordType, &version,
		),
		tlv.MakePrimitiveRecord(
			intentMessageDomainRecordType, &domain,
		),
		tlv.MakePrimitiveRecord(
			intentMessageValidFromRecordType, &validFrom,
		),
		tlv.MakePrimitiveRecord(
			intentMessageValidUntilRecordType, &validUntil,
		),
		tlv.MakePrimitiveRecord(
			intentMessagePayloadRecordType, &payload,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create intent encode stream: %w", err)
	}

	var b bytes.Buffer
	err = stream.Encode(&b)
	if err != nil {
		return nil, fmt.Errorf("encode intent message: %w", err)
	}

	return b.Bytes(), nil
}
