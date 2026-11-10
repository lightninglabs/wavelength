package round

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// encodeDurableUint32 keeps heights and positional indices at their full width.
func encodeDurableUint32(v uint32) ([]byte, error) {
	return encodeDurableFields(tlv.MakePrimitiveRecord(1, &v))
}

// decodeDurableUint32 restores one fixed-width scalar.
func decodeDurableUint32(raw []byte) (uint32, error) {
	var v uint32
	err := decodeDurableFields(raw, tlv.MakePrimitiveRecord(1, &v))

	return v, err
}

// encodeDurableAncestry retains every fragment and its input mapping.
func encodeDurableAncestry(v types.Ancestry) ([]byte, error) {
	path, err := encodeDurableTree(v.TreePath)
	if err != nil {
		return nil, err
	}
	indices, err := encodeDurableList(v.InputIndices, encodeDurableUint32)
	if err != nil {
		return nil, err
	}
	height := uint32(v.CommitmentHeight)
	sweepKey := durablePubKey(v.CommitmentSweepKey)

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &path),
		tlv.MakePrimitiveRecord(
			3, (*[32]byte)(&v.CommitmentTxID),
		),
		tlv.MakePrimitiveRecord(5, &indices),
		tlv.MakePrimitiveRecord(7, &v.TreeDepth),
		tlv.MakePrimitiveRecord(9, &height),
		tlv.MakePrimitiveRecord(11, &v.CommitmentSweepDelay),
		tlv.MakePrimitiveRecord(13, &sweepKey),
	)
}

// decodeDurableAncestry restores the complete unilateral-exit dependency.
func decodeDurableAncestry(raw []byte) (types.Ancestry, error) {
	var v types.Ancestry
	var path, indices, sweepKey []byte
	var height uint32
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &path),
		tlv.MakePrimitiveRecord(
			3, (*[32]byte)(&v.CommitmentTxID),
		),
		tlv.MakePrimitiveRecord(5, &indices),
		tlv.MakePrimitiveRecord(7, &v.TreeDepth),
		tlv.MakePrimitiveRecord(9, &height),
		tlv.MakePrimitiveRecord(11, &v.CommitmentSweepDelay),
		tlv.MakePrimitiveRecord(13, &sweepKey),
	)
	if err != nil {
		return v, err
	}
	v.CommitmentHeight = int32(height)
	v.CommitmentSweepKey, err = parseDurablePubKey(sweepKey)
	if err != nil {
		return v, err
	}
	v.TreePath, err = decodeDurableTree(path)
	if err != nil {
		return v, err
	}
	v.InputIndices, err = decodeDurableList(indices, decodeDurableUint32)

	return v, err
}

// encodeDurableClientVTXO preserves the confirmed wallet result for delivery.
func encodeDurableClientVTXO(v *ClientVTXO) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	request, err := encodeDurableVTXO(types.VTXORequest{
		Amount:         v.Amount,
		PolicyTemplate: v.PolicyTemplate,
		PkScript:       v.PkScript,
		Expiry:         v.Expiry,
		OwnerKey:       v.OwnerKey,
		OperatorKey:    v.OperatorKey,
		Origin:         v.Origin,
	})
	if err != nil {
		return nil, err
	}
	ancestry, err := encodeDurableList(v.Ancestry, encodeDurableAncestry)
	if err != nil {
		return nil, err
	}
	point := durableOutpoint(v.Outpoint)
	var roundID []byte
	if v.RoundID.IsSome() {
		id := v.RoundID.UnwrapOr(RoundID{})
		roundID = id[:]
	}
	expiry := uint32(v.BatchExpiry)
	height := uint32(v.CreatedHeight)
	var assetRoot []byte
	if v.TaprootAssetRoot != nil {
		assetRoot = v.TaprootAssetRoot[:]
	}
	assetRef := []byte(v.TaprootAssetRef)

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &request),
		tlv.MakePrimitiveRecord(3, &point),
		tlv.MakePrimitiveRecord(5, &ancestry),
		tlv.MakePrimitiveRecord(7, &roundID),
		tlv.MakePrimitiveRecord(
			9, (*[32]byte)(&v.CommitmentTxID),
		),
		tlv.MakePrimitiveRecord(11, &expiry),
		tlv.MakePrimitiveRecord(13, &height),
		tlv.MakePrimitiveRecord(15, &assetRoot),
		tlv.MakePrimitiveRecord(17, &assetRef),
		tlv.MakePrimitiveRecord(19, &v.TaprootAssetAmount),
		tlv.MakePrimitiveRecord(21, &v.TaprootAssetSealedPackage),
	)
}

// decodeDurableClientVTXO restores ownership without querying or deriving keys.
func decodeDurableClientVTXO(raw []byte) (*ClientVTXO, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var request, point, ancestry, roundID []byte
	var assetRoot, assetRef, assetPackage []byte
	var assetAmount uint64
	var txid chainhash.Hash
	var expiry, height uint32
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &request),
		tlv.MakePrimitiveRecord(3, &point),
		tlv.MakePrimitiveRecord(5, &ancestry),
		tlv.MakePrimitiveRecord(7, &roundID),
		tlv.MakePrimitiveRecord(
			9, (*[32]byte)(&txid),
		),
		tlv.MakePrimitiveRecord(11, &expiry),
		tlv.MakePrimitiveRecord(13, &height),
		tlv.MakePrimitiveRecord(15, &assetRoot),
		tlv.MakePrimitiveRecord(17, &assetRef),
		tlv.MakePrimitiveRecord(19, &assetAmount),
		tlv.MakePrimitiveRecord(21, &assetPackage),
	)
	if err != nil {
		return nil, err
	}
	req, err := decodeDurableVTXO(request)
	if err != nil {
		return nil, err
	}
	v := &ClientVTXO{
		TaprootAssetRef:           string(assetRef),
		TaprootAssetAmount:        assetAmount,
		TaprootAssetSealedPackage: assetPackage,
		Amount:                    req.Amount,
		PolicyTemplate:            req.PolicyTemplate,
		PkScript:                  req.PkScript,
		Expiry:                    req.Expiry,
		OwnerKey:                  req.OwnerKey,
		OperatorKey:               req.OperatorKey,
		Origin:                    req.Origin,
		CommitmentTxID:            txid,
		BatchExpiry:               int32(expiry),
		CreatedHeight:             int32(height),
	}
	if len(assetRoot) != 0 {
		root, err := chainhash.NewHash(assetRoot)
		if err != nil {
			return nil, err
		}
		v.TaprootAssetRoot = root
	}
	v.Outpoint, err = parseDurableOutpoint(point)
	if err != nil {
		return nil, err
	}
	v.Ancestry, err = decodeDurableList(ancestry, decodeDurableAncestry)
	if err != nil {
		return nil, err
	}
	if len(roundID) != 0 {
		id, err := parseDurableRoundID(roundID)
		if err != nil {
			return nil, err
		}
		v.RoundID = fn.Some(id)
	}

	return v, nil
}
