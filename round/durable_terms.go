package round

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightningnetwork/lnd/tlv"
)

// durableTerms preserves the policy accepted when the round was created.
type durableTerms struct {
	PubKey                  []byte
	BoardingExitDelay       uint32
	VTXOExitDelay           uint32
	DustLimit               uint64
	MinVTXOAmount           uint64
	MinBoardingAmount       uint64
	MaxVTXOAmount           uint64
	MaxUserBalance          uint64
	FeeRate                 uint64
	MinOperatorFee          uint64
	FreeRefreshWindowBlocks uint32
	MinConfirmations        uint32
	VTXOConfirmations       uint32
	MaxOORLineageVBytes     uint32
}

// records defines the versioned checkpoint fields for operator terms.
func (v *durableTerms) records() []tlv.Record {
	return []tlv.Record{
		tlv.MakePrimitiveRecord(1, &v.PubKey),
		tlv.MakePrimitiveRecord(3, &v.BoardingExitDelay),
		tlv.MakePrimitiveRecord(5, &v.VTXOExitDelay),
		tlv.MakePrimitiveRecord(7, &v.DustLimit),
		tlv.MakePrimitiveRecord(9, &v.MinVTXOAmount),
		tlv.MakePrimitiveRecord(11, &v.MinBoardingAmount),
		tlv.MakePrimitiveRecord(13, &v.MaxVTXOAmount),
		tlv.MakePrimitiveRecord(15, &v.MaxUserBalance),
		tlv.MakePrimitiveRecord(17, &v.FeeRate),
		tlv.MakePrimitiveRecord(19, &v.MinOperatorFee),
		tlv.MakePrimitiveRecord(21, &v.FreeRefreshWindowBlocks),
		tlv.MakePrimitiveRecord(23, &v.MinConfirmations),
		tlv.MakePrimitiveRecord(25, &v.VTXOConfirmations),
		tlv.MakePrimitiveRecord(27, &v.MaxOORLineageVBytes),
	}
}

// encodeDurableTerms freezes policy instead of consulting fresh discovery data.
func encodeDurableTerms(terms *types.OperatorTerms) ([]byte, error) {
	if terms == nil {
		return nil, nil
	}
	v := durableTerms{
		BoardingExitDelay:       terms.BoardingExitDelay,
		VTXOExitDelay:           terms.VTXOExitDelay,
		DustLimit:               uint64(terms.DustLimit),
		MinVTXOAmount:           uint64(terms.MinVTXOAmount),
		MinBoardingAmount:       uint64(terms.MinBoardingAmount),
		MaxVTXOAmount:           uint64(terms.MaxVTXOAmount),
		MaxUserBalance:          uint64(terms.MaxUserBalance),
		FeeRate:                 uint64(terms.FeeRate),
		MinOperatorFee:          uint64(terms.MinOperatorFee),
		FreeRefreshWindowBlocks: terms.FreeRefreshWindowBlocks,
		MinConfirmations:        terms.MinConfirmations,
		VTXOConfirmations:       terms.VTXOConfirmations,
		MaxOORLineageVBytes:     terms.MaxOORLineageVBytes,
	}
	if terms.PubKey != nil {
		v.PubKey = terms.PubKey.SerializeCompressed()
	}

	return encodeDurableFields(v.records()...)
}

// decodeDurableTerms restores all terms, including explicit zero-valued caps.
func decodeDurableTerms(raw []byte) (*types.OperatorTerms, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v durableTerms
	if err := decodeDurableFields(raw, v.records()...); err != nil {
		return nil, err
	}
	terms := &types.OperatorTerms{
		BoardingExitDelay:       v.BoardingExitDelay,
		VTXOExitDelay:           v.VTXOExitDelay,
		DustLimit:               btcutil.Amount(v.DustLimit),
		MinVTXOAmount:           btcutil.Amount(v.MinVTXOAmount),
		MinBoardingAmount:       btcutil.Amount(v.MinBoardingAmount),
		MaxVTXOAmount:           btcutil.Amount(v.MaxVTXOAmount),
		MaxUserBalance:          btcutil.Amount(v.MaxUserBalance),
		FeeRate:                 btcutil.Amount(v.FeeRate),
		MinOperatorFee:          btcutil.Amount(v.MinOperatorFee),
		FreeRefreshWindowBlocks: v.FreeRefreshWindowBlocks,
		MinConfirmations:        v.MinConfirmations,
		VTXOConfirmations:       v.VTXOConfirmations,
		MaxOORLineageVBytes:     v.MaxOORLineageVBytes,
	}
	if len(v.PubKey) != 0 {
		key, err := btcec.ParsePubKey(v.PubKey)
		if err != nil {
			return nil, err
		}
		terms.PubKey = key
	}

	return terms, nil
}
