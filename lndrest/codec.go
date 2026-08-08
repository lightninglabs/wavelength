package lndrest

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// encodeTx serializes a transaction with witness data, matching the raw tx
// encoding lnd's RPCs expect.
func encodeTx(tx *wire.MsgTx) ([]byte, error) {
	var buf bytes.Buffer
	if err := tx.BtcEncode(&buf, 0, wire.WitnessEncoding); err != nil {
		return nil, fmt.Errorf("serialize tx: %w", err)
	}

	return buf.Bytes(), nil
}

// decodeTx deserializes raw witness-encoded transaction bytes.
func decodeTx(rawTx []byte) (*wire.MsgTx, error) {
	tx := &wire.MsgTx{}
	if err := tx.BtcDecode(
		bytes.NewReader(rawTx), 0, wire.WitnessEncoding,
	); err != nil {
		return nil, fmt.Errorf("deserialize tx: %w", err)
	}

	return tx, nil
}

// decodeBlock deserializes a raw block.
func decodeBlock(rawBlock []byte) (*wire.MsgBlock, error) {
	block := &wire.MsgBlock{}
	if err := block.Deserialize(bytes.NewReader(rawBlock)); err != nil {
		return nil, fmt.Errorf("deserialize block: %w", err)
	}

	return block, nil
}

// btcAmount converts a satoshi count into a btcutil.Amount.
func btcAmount(sats int64) btcutil.Amount {
	return btcutil.Amount(sats)
}
