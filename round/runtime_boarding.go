package round

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// encodeBoardingReply freezes the wallet read result before accepting its
// asynchronous reply. Failures remain diagnostic; they never become inputs.
func encodeBoardingReply(reply *boardingIntentsReply) ([]byte, error) {
	var intents, diagnostic []byte
	response, err := reply.Res.Unpack()
	if err != nil {
		diagnostic = []byte(err.Error())
	} else {
		confirmed, ok :=
			response.(*wallet.GetConfirmedBoardingIntentsResponse)
		if !ok {
			return nil, fmt.Errorf("unexpected boarding reply %T",
				response)
		}
		intents, err = encodeDurableList(
			confirmed.Intents,
			func(intent wallet.BoardingIntent) ([]byte, error) {
				return encodeDurableBoarding(
					BoardingIntent{
						BoardingIntent: intent,
					},
				)
			},
		)
		if err != nil {
			return nil, err
		}
	}

	return encodeDurableFields(
		tlv.MakePrimitiveRecord(1, &intents),
		tlv.MakePrimitiveRecord(3, &diagnostic),
	)
}

// decodeBoardingReply restores the read result and its exact queue correlation.
func decodeBoardingReply(raw []byte, fetchID string,
	params *chaincfg.Params) (*boardingIntentsReply, error) {

	var intents, diagnostic []byte
	if err := decodeDurableFields(
		raw, tlv.MakePrimitiveRecord(1, &intents),
		tlv.MakePrimitiveRecord(3, &diagnostic),
	); err != nil {
		return nil, err
	}
	reply := &boardingIntentsReply{FetchID: fetchID}
	if len(diagnostic) != 0 {
		reply.Res = fn.Err[wallet.WalletResp](
			fmt.Errorf("%s", diagnostic),
		)

		return reply, nil
	}
	values, err := decodeDurableList(
		intents,
		func(raw []byte) (wallet.BoardingIntent, error) {
			intent, err := decodeDurableBoarding(raw, params)

			return intent.BoardingIntent, err
		},
	)
	if err != nil {
		return nil, err
	}
	reply.Res = fn.Ok[wallet.WalletResp](
		&wallet.GetConfirmedBoardingIntentsResponse{
			Intents: values,
		},
	)

	return reply, nil
}
