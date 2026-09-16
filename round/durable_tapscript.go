package round

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/tlv"
)

// durableTapscript retains the wallet's exact signing material. Leaf versions
// and order are part of the commitment and must survive checkpoint restoration.
type durableTapscript struct {
	Kind      uint8
	Control   []byte
	Leaves    []byte
	Revealed  []byte
	Root      []byte
	OutputKey []byte
}

// stream assigns independent TLV fields to each wallet representation.
func (d *durableTapscript) stream() (*tlv.Stream, error) {
	return tlv.NewStream(
		tlv.MakePrimitiveRecord(1, &d.Kind),
		tlv.MakePrimitiveRecord(3, &d.Control),
		tlv.MakePrimitiveRecord(5, &d.Leaves),
		tlv.MakePrimitiveRecord(7, &d.Revealed),
		tlv.MakePrimitiveRecord(9, &d.Root),
		tlv.MakePrimitiveRecord(11, &d.OutputKey),
	)
}

// encodeDurableTapscript preserves absent scripts without deriving a new tree.
func encodeDurableTapscript(script *waddrmgr.Tapscript) ([]byte, error) {
	if script == nil {
		return nil, nil
	}
	d := durableTapscript{
		Kind: uint8(script.Type), Revealed: script.RevealedScript,
		Root: script.RootHash, OutputKey: durablePubKey(
			script.FullOutputKey,
		),
	}
	if script.ControlBlock != nil {
		var err error
		d.Control, err = script.ControlBlock.ToBytes()
		if err != nil {
			return nil, err
		}
	}
	var leaves bytes.Buffer
	for _, leaf := range script.Leaves {
		// Each length-delimited entry includes its original leaf
		// version.
		entry := append([]byte{byte(leaf.LeafVersion)}, leaf.Script...)
		if err := wire.WriteVarBytes(&leaves, 0, entry); err != nil {
			return nil, err
		}
	}
	d.Leaves = leaves.Bytes()
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

// decodeDurableTapscript restores all variants without policy normalization.
func decodeDurableTapscript(raw []byte) (*waddrmgr.Tapscript, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var d durableTapscript
	stream, err := d.stream()
	if err != nil {
		return nil, err
	}
	if err := stream.Decode(bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	script := &waddrmgr.Tapscript{
		Type:           waddrmgr.TapscriptType(d.Kind),
		RevealedScript: d.Revealed, RootHash: d.Root,
	}
	if len(d.Control) != 0 {
		script.ControlBlock, err = txscript.ParseControlBlock(d.Control)
		if err != nil {
			return nil, err
		}
	}
	script.FullOutputKey, err = parseDurablePubKey(d.OutputKey)
	if err != nil {
		return nil, err
	}
	reader := bytes.NewReader(d.Leaves)
	for reader.Len() > 0 {
		// Bound every allocation by bytes remaining in this checkpoint.
		entry, err := wire.ReadVarBytes(
			reader, 0,
			uint32(
				reader.Len(),
			),
			"durable tap leaf",
		)
		if err != nil {
			return nil, err
		}
		if len(entry) == 0 {
			return nil, fmt.Errorf("durable tap leaf has no " +
				"version")
		}
		script.Leaves = append(script.Leaves, txscript.TapLeaf{
			LeafVersion: txscript.TapscriptLeafVersion(entry[0]),
			Script:      entry[1:],
		})
	}

	return script, nil
}
