package round

import (
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// encodeDurableConfirmation preserves the original scan hint and watch
// identity.
func encodeDurableConfirmation(req *RegisterConfirmationRequest) ([]byte,
	error) {

	caller := []byte(req.CallerID)
	var txid []byte
	if req.Txid != nil {
		txid = req.Txid[:]
	}

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &caller),
		tlv.MakePrimitiveRecord(3, &req.PkScript),
		tlv.MakePrimitiveRecord(5, &txid),
		tlv.MakePrimitiveRecord(7, &req.TargetConfs),
		tlv.MakePrimitiveRecord(9, &req.HeightHint),
	)
}

// decodeDurableConfirmation binds the watch callback only when delivered.
func decodeDurableConfirmation(raw []byte) (ClientOutMsg, error) {
	var caller, txid []byte
	req := &RegisterConfirmationRequest{}
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &caller),
		tlv.MakePrimitiveRecord(3, &req.PkScript),
		tlv.MakePrimitiveRecord(5, &txid),
		tlv.MakePrimitiveRecord(7, &req.TargetConfs),
		tlv.MakePrimitiveRecord(9, &req.HeightHint),
	)
	if err != nil {
		return nil, err
	}
	req.CallerID = string(caller)
	if len(txid) != 0 {
		if len(txid) != chainhash.HashSize {
			return nil, fmt.Errorf("invalid confirmation txid")
		}
		hash := chainhash.Hash(txid)
		req.Txid = &hash
	}

	return req, nil
}
