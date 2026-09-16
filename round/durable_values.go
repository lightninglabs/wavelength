package round

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/treecodec"
	"github.com/lightningnetwork/lnd/tlv"
)

// durableMapEntry retains a key and value as independent nested records.
type durableMapEntry struct {
	key   []byte
	value []byte
}

// encodeDurableMap sorts encoded keys so a checkpoint has deterministic bytes.
func encodeDurableMap[K comparable, V any](values map[K]V,
	encodeKey func(K) ([]byte, error),
	encodeValue func(V) ([]byte, error)) ([]byte, error) {

	entries := make([]durableMapEntry, 0, len(values))
	for key, value := range values {
		keyBytes, err := encodeKey(key)
		if err != nil {
			return nil, err
		}
		valueBytes, err := encodeValue(value)
		if err != nil {
			return nil, err
		}
		entries = append(
			entries, durableMapEntry{
				key:   keyBytes,
				value: valueBytes,
			},
		)
	}
	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].key, entries[j].key) < 0
	})

	return encodeDurableList(
		entries,
		func(entry durableMapEntry) ([]byte, error) {
			return encodeDurableFields(
				tlv.MakePrimitiveRecord(1, &entry.key),
				tlv.MakePrimitiveRecord(3, &entry.value),
			)
		},
	)
}

// decodeDurableMap rejects duplicate keys instead of discarding ownership data.
func decodeDurableMap[K comparable, V any](raw []byte,
	decodeKey func([]byte) (K, error),
	decodeValue func([]byte) (V, error)) (map[K]V, error) {

	if len(raw) == 0 {
		return nil, nil
	}
	values := make(map[K]V)
	_, err := decodeDurableList(raw, func(entry []byte) (struct{}, error) {
		var item durableMapEntry
		err := decodeDurableFields(
			entry, tlv.MakePrimitiveRecord(1, &item.key),
			tlv.MakePrimitiveRecord(3, &item.value),
		)
		if err != nil {
			return struct{}{}, err
		}
		key, err := decodeKey(item.key)
		if err != nil {
			return struct{}{}, err
		}
		if _, exists := values[key]; exists {
			return struct{}{}, fmt.Errorf("duplicate durable map " +
				"key")
		}
		value, err := decodeValue(item.value)
		if err != nil {
			return struct{}{}, err
		}
		values[key] = value

		return struct{}{}, nil
	})

	return values, err
}

// encodeDurableTree preserves the complete tree rather than a wire projection.
func encodeDurableTree(value *tree.Tree) ([]byte, error) {
	if value == nil {
		return nil, nil
	}

	return treecodec.SerializeSnapshot(value)
}

// decodeDurableTree restores cached keys and local asset signing data.
func decodeDurableTree(raw []byte) (*tree.Tree, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	return treecodec.DeserializeTree(raw)
}

// encodeDurableIndex records a positional index without architecture padding.
func encodeDurableIndex(value int) ([]byte, error) {
	return encodeDurableAmount(int64(value))
}

// decodeDurableIndex refuses values that cannot be represented on this host.
func decodeDurableIndex(raw []byte) (int, error) {
	value, err := decodeDurableAmount(raw)
	if err != nil {
		return 0, err
	}
	index := int(value)
	if int64(index) != value {
		return 0, fmt.Errorf("durable index overflows int")
	}

	return index, nil
}

// encodeDurableSignerKey records the compressed key used for tree routing.
func encodeDurableSignerKey(value SignerKey) ([]byte, error) {
	return value[:], nil
}

// decodeDurableSignerKey requires the complete routing key.
func decodeDurableSignerKey(raw []byte) (SignerKey, error) {
	var key SignerKey
	if len(raw) != len(key) {
		return key, fmt.Errorf("invalid durable signer key")
	}
	copy(key[:], raw)

	return key, nil
}

// encodeDurablePoint adapts outpoint encoding to the shared map/list helpers.
func encodeDurablePoint(value wire.OutPoint) ([]byte, error) {
	return durableOutpoint(value), nil
}

// encodeDurablePSBT retains witness UTXOs and any signatures already attached.
func encodeDurablePSBT(value *psbt.Packet) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	var raw bytes.Buffer
	if err := value.Serialize(&raw); err != nil {
		return nil, err
	}

	return raw.Bytes(), nil
}

// decodeDurablePSBT rejects trailing data rather than accepting a prefix.
func decodeDurablePSBT(raw []byte) (*psbt.Packet, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	reader := bytes.NewReader(raw)
	value, err := psbt.NewFromRawBytes(reader, false)
	if err != nil {
		return nil, err
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("trailing durable PSBT bytes")
	}

	return value, nil
}

// encodeDurableTx preserves an optional complete wire transaction.
func encodeDurableTx(value *wire.MsgTx) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	var raw bytes.Buffer
	if err := value.Serialize(&raw); err != nil {
		return nil, err
	}

	return raw.Bytes(), nil
}

// decodeDurableTx rejects trailing data in a stored transaction.
func decodeDurableTx(raw []byte) (*wire.MsgTx, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	reader := bytes.NewReader(raw)
	value := wire.NewMsgTx(2)
	if err := value.Deserialize(reader); err != nil {
		return nil, err
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("trailing durable tx bytes")
	}

	return value, nil
}

// encodeDurableSignature distinguishes an absent signature from a concrete one.
func encodeDurableSignature(value *schnorr.Signature) []byte {
	if value == nil {
		return nil
	}

	return value.Serialize()
}

// decodeDurableSignature validates a stored Schnorr signature's representation.
func decodeDurableSignature(raw []byte) (*schnorr.Signature, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	return schnorr.ParseSignature(raw)
}
