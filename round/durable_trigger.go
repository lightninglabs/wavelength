package round

import (
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightningnetwork/lnd/tlv"
)

// encodeDurableTrigger retains the exact input set used to size boarding.
func encodeDurableTrigger(req *actormsg.TriggerBoardMsg) ([]byte, error) {
	var service []byte
	var err error
	if req.Service != nil {
		service, err = req.Service.Encode()
		if err != nil {
			return nil, err
		}
	}
	amounts, err := encodeDurableList(req.Amounts, encodeDurableBTCAmount)
	if err != nil {
		return nil, err
	}
	points, err := encodeDurableList(req.Outpoints, encodeDurablePoint)
	if err != nil {
		return nil, err
	}
	change, err := encodeDurableLeave(req.Change)
	if err != nil {
		return nil, err
	}

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &service),
		tlv.MakePrimitiveRecord(3, &amounts),
		tlv.MakePrimitiveRecord(5, &points),
		tlv.MakePrimitiveRecord(7, &change),
	)
}

// decodeDurableTrigger restores input selection without fetching fresh balance.
func decodeDurableTrigger(raw []byte) (*actormsg.TriggerBoardMsg, error) {
	var service, amounts, points, change []byte
	err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &service),
		tlv.MakePrimitiveRecord(3, &amounts),
		tlv.MakePrimitiveRecord(5, &points),
		tlv.MakePrimitiveRecord(7, &change),
	)
	if err != nil {
		return nil, err
	}
	req := &actormsg.TriggerBoardMsg{}
	if len(service) != 0 {
		req.Service, err = types.DecodeServiceRequest(service)
		if err != nil {
			return nil, err
		}
	}
	req.Amounts, err = decodeDurableList(amounts, decodeDurableBTCAmount)
	if err != nil {
		return nil, err
	}
	req.Outpoints, err = decodeDurableList(points, parseDurableOutpoint)
	if err != nil {
		return nil, err
	}
	req.Change, err = decodeDurableLeave(change)
	if err != nil {
		return nil, err
	}

	return req, nil
}
