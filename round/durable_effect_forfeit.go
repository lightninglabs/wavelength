package round

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightningnetwork/lnd/tlv"
)

// encodeDurableForfeitRequest preserves both inputs and the exact spend path.
func encodeDurableForfeitRequest(req *ForfeitRequestToVTXO) ([]byte, error) {
	point, connector := durableOutpoint(req.VTXOOutpoint), durableOutpoint(
		req.ConnectorOutpoint,
	)
	id, amount := []byte(req.RoundID), uint64(req.ConnectorAmount)
	spend, err := encodeDurableSpend(req.ForfeitSpend)
	if err != nil {
		return nil, err
	}

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &point),
		tlv.MakePrimitiveRecord(3, &connector),
		tlv.MakePrimitiveRecord(5, &id),
		tlv.MakePrimitiveRecord(7, &amount),
		tlv.MakePrimitiveRecord(9, &req.ConnectorPkScript),
		tlv.MakePrimitiveRecord(11, &req.ServerForfeitPkScript),
		tlv.MakePrimitiveRecord(13, &spend),
	)
}

// decodeDurableForfeitRequest does not regenerate a default custom policy.
func decodeDurableForfeitRequest(raw []byte) (ClientOutMsg, error) {
	var point, connector, id, spend []byte
	var amount uint64
	req := &ForfeitRequestToVTXO{}
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &point),
		tlv.MakePrimitiveRecord(3, &connector),
		tlv.MakePrimitiveRecord(5, &id),
		tlv.MakePrimitiveRecord(7, &amount),
		tlv.MakePrimitiveRecord(9, &req.ConnectorPkScript),
		tlv.MakePrimitiveRecord(11, &req.ServerForfeitPkScript),
		tlv.MakePrimitiveRecord(13, &spend),
	)
	if err != nil {
		return nil, err
	}
	req.VTXOOutpoint, err = parseDurableOutpoint(point)
	if err != nil {
		return nil, err
	}
	req.ConnectorOutpoint, err = parseDurableOutpoint(connector)
	if err != nil {
		return nil, err
	}
	if len(spend) != 0 {
		req.ForfeitSpend, err = arkscript.DecodeSpendPath(spend)
		if err != nil {
			return nil, err
		}
	}
	req.RoundID, req.ConnectorAmount = string(id), int64(amount)

	return req, nil
}

// encodeDurableForfeitConfirmed records the commitment that consumed the input.
func encodeDurableForfeitConfirmed(req *ForfeitConfirmedToVTXO) ([]byte,
	error) {

	point := durableOutpoint(req.VTXOOutpoint)
	height := uint32(req.BlockHeight)
	hash := [32]byte(req.CommitmentTxID)

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &point),
		tlv.MakePrimitiveRecord(3, &hash),
		tlv.MakePrimitiveRecord(5, &height),
	)
}

// decodeDurableForfeitConfirmed restores confirmation provenance for the owner.
func decodeDurableForfeitConfirmed(raw []byte) (ClientOutMsg, error) {
	var point []byte
	var hash [32]byte
	var height uint32
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &point),
		tlv.MakePrimitiveRecord(3, &hash),
		tlv.MakePrimitiveRecord(5, &height),
	)
	if err != nil {
		return nil, err
	}
	outpoint, err := parseDurableOutpoint(point)
	if err != nil {
		return nil, err
	}

	return &ForfeitConfirmedToVTXO{
		VTXOOutpoint: outpoint, CommitmentTxID: chainhash.Hash(hash),
		BlockHeight: int32(height),
	}, nil
}
