package round

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// encodeDurableOutputRequest records amounts before wallet key derivation.
func encodeDurableOutputRequest(req *RegisterVTXORequestsRequest) ([]byte,
	error) {

	amounts, err := encodeDurableList(req.Amounts, encodeDurableBTCAmount)
	if err != nil {
		return nil, err
	}
	assets, err := encodeDurableList(req.Assets, encodeDurableAssetRequest)
	if err != nil {
		return nil, err
	}
	var change []byte
	if req.ChangeIndex != nil {
		change, err = encodeDurableIndex(*req.ChangeIndex)
		if err != nil {
			return nil, err
		}
	}

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &amounts),
		tlv.MakePrimitiveRecord(3, &assets),
		tlv.MakePrimitiveRecord(5, &change),
	)
}

// decodeDurableOutputRequest distinguishes an absent index from explicit zero.
func decodeDurableOutputRequest(raw []byte) (*RegisterVTXORequestsRequest,
	error) {

	var amounts, assets, change []byte
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &amounts),
		tlv.MakePrimitiveRecord(3, &assets),
		tlv.MakePrimitiveRecord(5, &change),
	)
	if err != nil {
		return nil, err
	}
	req := &RegisterVTXORequestsRequest{}
	req.Amounts, err = decodeDurableList(amounts, decodeDurableBTCAmount)
	if err != nil {
		return nil, err
	}
	req.Assets, err = decodeDurableList(assets, decodeDurableAssetRequest)
	if err != nil {
		return nil, err
	}
	if len(change) != 0 {
		index, err := decodeDurableIndex(change)
		if err != nil {
			return nil, err
		}
		req.ChangeIndex = &index
	}

	return req, nil
}

// encodeDurableBTCAmount preserves the amount's signed bit pattern.
func encodeDurableBTCAmount(value btcutil.Amount) ([]byte, error) {
	return encodeDurableAmount(int64(value))
}

// decodeDurableBTCAmount restores the value before admission validates it.
func decodeDurableBTCAmount(raw []byte) (btcutil.Amount, error) {
	value, err := decodeDurableAmount(raw)

	return btcutil.Amount(value), err
}

// encodeDurableAssetRequest preserves both asset units and Bitcoin value.
func encodeDurableAssetRequest(req AssetVTXORequest) ([]byte, error) {
	amount := uint64(req.AmountSat)
	ref := []byte(req.AssetRef)

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &amount),
		tlv.MakePrimitiveRecord(3, &ref),
		tlv.MakePrimitiveRecord(5, &req.AssetAmount),
	)
}

// decodeDurableAssetRequest restores the unmodified local asset reference.
func decodeDurableAssetRequest(raw []byte) (AssetVTXORequest, error) {
	var amount, units uint64
	var ref []byte
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &amount),
		tlv.MakePrimitiveRecord(3, &ref),
		tlv.MakePrimitiveRecord(5, &units),
	)

	return AssetVTXORequest{
		AmountSat: btcutil.Amount(amount),
		AssetRef:  string(ref), AssetAmount: units,
	}, err
}
