package round

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightningnetwork/lnd/tlv"
)

// encodeDurableConnector preserves local VTXO amounts as well as wire fields.
func encodeDurableConnector(v *ConnectorLeafInfo) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	index := uint64(int64(v.LeafIndex))
	point := durableOutpoint(v.ConnectorOutpoint)
	amount := uint64(v.ConnectorAmount)
	vtxoAmount := uint64(v.VTXOAmount)

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &index),
		tlv.MakePrimitiveRecord(3, &point),
		tlv.MakePrimitiveRecord(5, &v.ConnectorPkScript),
		tlv.MakePrimitiveRecord(7, &amount),
		tlv.MakePrimitiveRecord(9, &vtxoAmount),
		tlv.MakePrimitiveRecord(11, &v.RootOutputIndex),
		tlv.MakePrimitiveRecord(13, &v.NumLeaves),
		tlv.MakePrimitiveRecord(15, &v.Radix),
	)
}

// decodeDurableConnector restores the exact connector assignment.
func decodeDurableConnector(raw []byte) (*ConnectorLeafInfo, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	v := &ConnectorLeafInfo{}
	var index, amount, vtxoAmount uint64
	var point []byte
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &index),
		tlv.MakePrimitiveRecord(3, &point),
		tlv.MakePrimitiveRecord(5, &v.ConnectorPkScript),
		tlv.MakePrimitiveRecord(7, &amount),
		tlv.MakePrimitiveRecord(9, &vtxoAmount),
		tlv.MakePrimitiveRecord(11, &v.RootOutputIndex),
		tlv.MakePrimitiveRecord(13, &v.NumLeaves),
		tlv.MakePrimitiveRecord(15, &v.Radix),
	)
	if err != nil {
		return nil, err
	}
	v.LeafIndex = int(int64(index))
	v.ConnectorAmount = int64(amount)
	v.VTXOAmount = btcutil.Amount(int64(vtxoAmount))
	v.ConnectorOutpoint, err = parseDurableOutpoint(point)

	return v, err
}

// encodeDurableInputSignature stores an already-produced signature for replay.
func encodeDurableInputSignature(v *types.BoardingInputSignature) ([]byte,
	error) {

	if v == nil {
		return nil, nil
	}
	index := uint64(int64(v.InputIndex))
	point := durableOutpoint(v.Outpoint)
	sig := encodeDurableSignature(v.ClientSignature)

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &index),
		tlv.MakePrimitiveRecord(3, &point),
		tlv.MakePrimitiveRecord(5, &sig),
	)
}

// decodeDurableInputSignature restores bytes without invoking the signer.
func decodeDurableInputSignature(raw []byte) (*types.BoardingInputSignature,
	error) {

	if len(raw) == 0 {
		return nil, nil
	}
	v := &types.BoardingInputSignature{}
	var index uint64
	var point, sig []byte
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &index),
		tlv.MakePrimitiveRecord(3, &point),
		tlv.MakePrimitiveRecord(5, &sig),
	)
	if err != nil {
		return nil, err
	}
	v.InputIndex = int(int64(index))
	v.Outpoint, err = parseDurableOutpoint(point)
	if err != nil {
		return nil, err
	}
	v.ClientSignature, err = decodeDurableSignature(sig)

	return v, err
}

// encodeDurableParticipant retains the key that identifies each signature.
func encodeDurableParticipant(v *types.ForfeitParticipantSig) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	key := durablePubKey(v.PubKey)
	sig := encodeDurableSignature(v.Signature)

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &key),
		tlv.MakePrimitiveRecord(3, &sig),
	)
}

// decodeDurableParticipant restores one keyed signature without re-signing.
func decodeDurableParticipant(raw []byte) (*types.ForfeitParticipantSig,
	error) {

	if len(raw) == 0 {
		return nil, nil
	}
	var key, sig []byte
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &key),
		tlv.MakePrimitiveRecord(3, &sig),
	)
	if err != nil {
		return nil, err
	}
	v := &types.ForfeitParticipantSig{}
	v.PubKey, err = parseDurablePubKey(key)
	if err != nil {
		return nil, err
	}
	v.Signature, err = decodeDurableSignature(sig)

	return v, err
}

// encodeDurableForfeitResponse records the complete result of external signing.
func encodeDurableForfeitResponse(v *ForfeitSignatureResponse) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	point := durableOutpoint(v.VTXOOutpoint)
	roundID := []byte(v.RoundID)
	tx, err := encodeDurableTx(v.ForfeitTx)
	if err != nil {
		return nil, err
	}
	signature := encodeDurableSignature(v.Signature)
	participants, err := encodeDurableList(
		v.ParticipantVTXOSigs, encodeDurableParticipant,
	)
	if err != nil {
		return nil, err
	}
	spend, err := encodeDurableSpend(v.SpendPath)
	if err != nil {
		return nil, err
	}

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &point),
		tlv.MakePrimitiveRecord(3, &roundID),
		tlv.MakePrimitiveRecord(5, &tx),
		tlv.MakePrimitiveRecord(7, &signature),
		tlv.MakePrimitiveRecord(9, &participants),
		tlv.MakePrimitiveRecord(11, &spend),
	)
}

// decodeDurableForfeitResponse restores the result that must precede emission.
func decodeDurableForfeitResponse(raw []byte) (*ForfeitSignatureResponse,
	error) {

	if len(raw) == 0 {
		return nil, nil
	}
	var point, roundID, tx, signature, participants, spend []byte
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &point),
		tlv.MakePrimitiveRecord(3, &roundID),
		tlv.MakePrimitiveRecord(5, &tx),
		tlv.MakePrimitiveRecord(7, &signature),
		tlv.MakePrimitiveRecord(9, &participants),
		tlv.MakePrimitiveRecord(11, &spend),
	)
	if err != nil {
		return nil, err
	}
	v := &ForfeitSignatureResponse{RoundID: string(roundID)}
	v.VTXOOutpoint, err = parseDurableOutpoint(point)
	if err != nil {
		return nil, err
	}
	v.ForfeitTx, err = decodeDurableTx(tx)
	if err != nil {
		return nil, err
	}
	v.Signature, err = decodeDurableSignature(signature)
	if err != nil {
		return nil, err
	}
	v.ParticipantVTXOSigs, err = decodeDurableList(
		participants, decodeDurableParticipant,
	)
	if err != nil {
		return nil, err
	}
	if len(spend) != 0 {
		v.SpendPath, err = arkscript.DecodeSpendPath(spend)
		if err != nil {
			return nil, err
		}
	}

	return v, nil
}
