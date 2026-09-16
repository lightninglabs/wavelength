package round

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/tlv"
)

// durableVTXO preserves local intent metadata that the join wire deliberately
// omits. In particular, an assembling intent may not have a signing key yet;
// encoding it must neither derive one nor infer local ownership from a policy.
type durableVTXO struct {
	Amount        uint64
	Flags         uint8
	AssetRef      []byte
	AssetAmount   uint64
	Policy        []byte
	PkScript      []byte
	Expiry        uint32
	ClientPubKey  []byte
	OperatorKey   []byte
	OwnerKey      []byte
	SigningKey    []byte
	Origin        uint8
	RefreshSource []byte
}

// stream assigns stable fields independently of the network request codec.
func (d *durableVTXO) stream() (*tlv.Stream, error) {
	return tlv.NewStream(
		tlv.MakePrimitiveRecord(1, &d.Amount),
		tlv.MakePrimitiveRecord(3, &d.Flags),
		tlv.MakePrimitiveRecord(5, &d.AssetRef),
		tlv.MakePrimitiveRecord(7, &d.AssetAmount),
		tlv.MakePrimitiveRecord(9, &d.Policy),
		tlv.MakePrimitiveRecord(11, &d.PkScript),
		tlv.MakePrimitiveRecord(13, &d.Expiry),
		tlv.MakePrimitiveRecord(15, &d.ClientPubKey),
		tlv.MakePrimitiveRecord(17, &d.OperatorKey),
		tlv.MakePrimitiveRecord(19, &d.OwnerKey),
		tlv.MakePrimitiveRecord(21, &d.SigningKey),
		tlv.MakePrimitiveRecord(23, &d.Origin),
		tlv.MakePrimitiveRecord(25, &d.RefreshSource),
	)
}

// encodeDurableVTXO records the exact local request, including unset keys and
// the original policy bytes. Normalization belongs to the protocol transition.
func encodeDurableVTXO(req types.VTXORequest) ([]byte, error) {
	d := durableVTXO{
		Amount: uint64(req.Amount), AssetRef: []byte(req.AssetRef),
		AssetAmount: req.AssetAmount, Policy: req.PolicyTemplate,
		PkScript: req.PkScript, Expiry: req.Expiry,
		ClientPubKey: durablePubKey(req.ClientKey),
		OperatorKey:  durablePubKey(req.OperatorKey),
		OwnerKey:     durableKeyDescriptor(req.OwnerKey),
		SigningKey:   durableKeyDescriptor(req.SigningKey),
		Origin:       uint8(req.Origin),
	}
	if req.IsChange {
		d.Flags |= 1
	}
	if req.FixedAmount {
		d.Flags |= 2
	}
	if req.RefreshSourceOutpoint != nil {
		d.RefreshSource = durableOutpoint(*req.RefreshSourceOutpoint)
	}
	stream, err := d.stream()
	if err != nil {
		return nil, err
	}
	var encoded bytes.Buffer
	if err := stream.Encode(&encoded); err != nil {
		return nil, err
	}

	return encoded.Bytes(), nil
}

// decodeDurableVTXO restores wallet key locators without deriving fresh keys.
// Amount uses the original signed bit pattern so invalid queued input reaches
// normal admission validation with the same value it had before persistence.
func decodeDurableVTXO(raw []byte) (types.VTXORequest, error) {
	var d durableVTXO
	var req types.VTXORequest
	stream, err := d.stream()
	if err != nil {
		return req, err
	}
	if err := stream.Decode(bytes.NewReader(raw)); err != nil {
		return req, err
	}
	if d.Flags & ^uint8(3) != 0 {
		return req, fmt.Errorf("unknown durable VTXO flags: %d",
			d.Flags)
	}
	req = types.VTXORequest{
		Amount:   btcutil.Amount(int64(d.Amount)),
		IsChange: d.Flags&1 != 0, FixedAmount: d.Flags&2 != 0,
		AssetRef: string(d.AssetRef), AssetAmount: d.AssetAmount,
		PolicyTemplate: d.Policy,
		PkScript:       d.PkScript,
		Expiry:         d.Expiry,
		Origin:         types.VTXOOrigin(d.Origin),
	}
	req.ClientKey, err = parseDurablePubKey(d.ClientPubKey)
	if err != nil {
		return req, err
	}
	req.OperatorKey, err = parseDurablePubKey(d.OperatorKey)
	if err != nil {
		return req, err
	}
	req.OwnerKey, err = parseDurableKeyDescriptor(d.OwnerKey)
	if err != nil {
		return req, err
	}
	req.SigningKey, err = parseDurableKeyDescriptor(d.SigningKey)
	if err != nil {
		return req, err
	}
	if len(d.RefreshSource) != 0 {
		point, err := parseDurableOutpoint(d.RefreshSource)
		if err != nil {
			return req, err
		}
		req.RefreshSourceOutpoint = &point
	}

	return req, nil
}

// durablePubKey keeps an absent key distinct from a concrete public key.
func durablePubKey(key *btcec.PublicKey) []byte {
	if key == nil {
		return nil
	}

	return key.SerializeCompressed()
}

// parseDurablePubKey accepts only the compressed format emitted by the codec.
func parseDurablePubKey(raw []byte) (*btcec.PublicKey, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) != btcec.PubKeyBytesLenCompressed {
		return nil, fmt.Errorf("invalid durable public key size: %d",
			len(raw))
	}

	return btcec.ParsePubKey(raw)
}

// durableKeyDescriptor preserves the locator even when no pubkey is present.
func durableKeyDescriptor(key keychain.KeyDescriptor) []byte {
	raw := make([]byte, 8, 8+btcec.PubKeyBytesLenCompressed)
	binary.BigEndian.PutUint32(raw[:4], uint32(key.Family))
	binary.BigEndian.PutUint32(raw[4:8], key.Index)

	return append(raw, durablePubKey(key.PubKey)...)
}

// parseDurableKeyDescriptor rejects truncated or extended locator encodings.
func parseDurableKeyDescriptor(raw []byte) (keychain.KeyDescriptor, error) {
	var key keychain.KeyDescriptor
	if len(raw) != 8 && len(raw) != 8+btcec.PubKeyBytesLenCompressed {
		return key, fmt.Errorf("invalid durable key descriptor "+
			"size: %d", len(raw))
	}
	key.Family = keychain.KeyFamily(int32(binary.BigEndian.Uint32(raw[:4])))
	key.Index = binary.BigEndian.Uint32(raw[4:8])
	var err error
	key.PubKey, err = parseDurablePubKey(raw[8:])

	return key, err
}

// durableOutpoint encodes an outpoint without losing an optional zero value.
func durableOutpoint(point wire.OutPoint) []byte {
	raw := make([]byte, 36)
	copy(raw[:32], point.Hash[:])
	binary.BigEndian.PutUint32(raw[32:], point.Index)

	return raw
}

// parseDurableOutpoint requires the complete hash and output index.
func parseDurableOutpoint(raw []byte) (wire.OutPoint, error) {
	var point wire.OutPoint
	if len(raw) != 36 {
		return point, fmt.Errorf("invalid durable outpoint size: %d",
			len(raw))
	}
	copy(point.Hash[:], raw[:32])
	point.Index = binary.BigEndian.Uint32(raw[32:])

	return point, nil
}
