package round

import (
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// durableEnvironment separates fixed round parameters from live dependencies.
type durableEnvironment struct {
	Terms               []byte
	Deadline            []byte
	RoundKey            []byte
	StartHeight         uint32
	MaxFee              uint64
	RefreshFloor        uint64
	RefreshRate         uint32
	ForfeitTimeout      uint64
	RegistrationTimeout uint64
	ReconcileTimeout    uint64
	DisableAuth         uint8
}

// records defines the per-round environment checkpoint layout.
func (v *durableEnvironment) records() []tlv.Record {
	return []tlv.Record{
		tlv.MakePrimitiveRecord(1, &v.Terms),
		tlv.MakePrimitiveRecord(3, &v.Deadline),
		tlv.MakePrimitiveRecord(5, &v.RoundKey),
		tlv.MakePrimitiveRecord(7, &v.StartHeight),
		tlv.MakePrimitiveRecord(9, &v.MaxFee),
		tlv.MakePrimitiveRecord(11, &v.RefreshFloor),
		tlv.MakePrimitiveRecord(13, &v.RefreshRate),
		tlv.MakePrimitiveRecord(15, &v.ForfeitTimeout),
		tlv.MakePrimitiveRecord(17, &v.RegistrationTimeout),
		tlv.MakePrimitiveRecord(19, &v.ReconcileTimeout),
		tlv.MakePrimitiveRecord(21, &v.DisableAuth),
	}
}

// encodeDurableEnvironment stores absolute deadlines and original fee policy.
func encodeDurableEnvironment(env *ClientEnvironment) ([]byte, error) {
	if env == nil {
		return nil, fmt.Errorf("round environment is missing")
	}
	terms, err := encodeDurableTerms(env.OperatorTerms)
	if err != nil {
		return nil, err
	}
	deadline, err := env.ParticipationDeadline.UTC().MarshalBinary()
	if err != nil {
		return nil, err
	}
	v := durableEnvironment{
		Terms:               terms,
		Deadline:            deadline,
		RoundKey:            []byte(env.RoundKey),
		StartHeight:         env.StartHeight,
		MaxFee:              uint64(env.MaxOperatorFee),
		RefreshFloor:        uint64(env.AutoRefreshFeeFloor),
		RefreshRate:         env.AutoRefreshFeeRatePPM,
		ForfeitTimeout:      uint64(env.ForfeitCollectionTimeout),
		RegistrationTimeout: uint64(env.RegistrationTimeout),
		ReconcileTimeout:    uint64(env.StatusReconcileTimeout),
	}
	if env.DisableJoinRequestAuth {
		v.DisableAuth = 1
	}

	return encodeDurableFields(v.records()...)
}

// decodeDurableEnvironment binds saved policy to the current actor
// dependencies.
func decodeDurableEnvironment(raw []byte,
	base *ClientEnvironment) (*ClientEnvironment, error) {

	if base == nil {
		return nil, fmt.Errorf("base environment is missing")
	}
	var v durableEnvironment
	if err := decodeDurableFields(raw, v.records()...); err != nil {
		return nil, err
	}
	if v.DisableAuth > 1 {
		return nil, fmt.Errorf("invalid auth flag")
	}
	terms, err := decodeDurableTerms(v.Terms)
	if err != nil {
		return nil, err
	}
	var deadline time.Time
	if err := deadline.UnmarshalBinary(v.Deadline); err != nil {
		return nil, err
	}
	env := *base
	env.OperatorTerms = terms
	env.ParticipationDeadline = deadline
	env.RoundKey = RoundKeyStr(v.RoundKey)
	env.StartHeight = v.StartHeight
	env.MaxOperatorFee = btcutil.Amount(v.MaxFee)
	env.AutoRefreshFeeFloor = btcutil.Amount(v.RefreshFloor)
	env.AutoRefreshFeeRatePPM = v.RefreshRate
	env.ForfeitCollectionTimeout = time.Duration(v.ForfeitTimeout)
	env.RegistrationTimeout = time.Duration(v.RegistrationTimeout)
	env.StatusReconcileTimeout = time.Duration(v.ReconcileTimeout)
	env.DisableJoinRequestAuth = v.DisableAuth == 1

	return &env, nil
}
