package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
)

// CarrierOrigin identifies who originally funded the Bitcoin carrier of an
// asset-bearing VTXO. Zero is intentionally invalid so callers must explicitly
// classify every asset input rather than silently defaulting a user-funded
// boarding carrier to operator reclaim.
type CarrierOrigin uint8

const (
	// CarrierOriginUnspecified is the invalid zero value.
	CarrierOriginUnspecified CarrierOrigin = 0

	// CarrierOriginRound marks an asset leaf minted in a round (boarded or
	// refreshed), so its carrier is the sender's own Bitcoin and returns as
	// plain sender change on its first OOR spend.
	CarrierOriginRound CarrierOrigin = 1

	// CarrierOriginOOR marks an asset leaf created by a previous OOR
	// transfer whose carrier came from the operator's float and is
	// reclaimed into the operator's float change when spent.
	CarrierOriginOOR CarrierOrigin = 2
)

// CarrierInput describes the Bitcoin carrier of one asset-bearing VTXO spent
// by an out-of-round transfer.
type CarrierInput struct {
	// Amount is the Bitcoin carrier value of the spent asset VTXO.
	Amount btcutil.Amount

	// Origin identifies whether the carrier came from a round (returns to
	// sender) or an OOR transfer (reclaimed into operator float change).
	Origin CarrierOrigin
}

// CarrierPlanRequest holds the inputs to deterministic carrier allocation for
// an operator-funded asset OOR transfer.
type CarrierPlanRequest struct {
	// Inputs are the asset-bearing VTXOs spent by the transfer.
	Inputs []CarrierInput

	// NewAssetLeafCount is the number of new asset VTXO outputs created by
	// the transfer (1 on a full send, 2 on a partial send with asset
	// change).
	NewAssetLeafCount int

	// OutputFloor is the operator's minimum VTXO carrier value
	// (min_vtxo_amount_sat). Every new asset leaf receives exactly this
	// value out of the leased float.
	OutputFloor btcutil.Amount

	// LeaseValue is the full Bitcoin value of the operator carrier-float
	// VTXO leased for this transfer.
	LeaseValue btcutil.Amount
}

// CarrierPlan is the Bitcoin-side output allocation for an operator-funded
// asset OOR transfer.
//
// Conservation invariant:
//
//	sum(input carriers) + LeaseValue ==
//		NewAssetLeafTotal + SenderChange + OperatorChange
type CarrierPlan struct {
	// NewAssetLeafCarrier is the carrier value assigned to each new asset
	// leaf (equal to OutputFloor).
	NewAssetLeafCarrier btcutil.Amount

	// NewAssetLeafTotal is NewAssetLeafCarrier * NewAssetLeafCount.
	NewAssetLeafTotal btcutil.Amount

	// SenderChange is the sender's plain Bitcoin change output: the summed
	// carriers of the round-created asset inputs. Zero means every spent
	// asset input was OOR-created and no sender Bitcoin change output is
	// produced.
	SenderChange btcutil.Amount

	// ReclaimedCarriers is the summed carriers of the OOR-created asset
	// inputs reclaimed into the operator's float change.
	ReclaimedCarriers btcutil.Amount

	// OperatorChange is the value returned to the float pkScript:
	//
	//	LeaseValue - NewAssetLeafTotal + ReclaimedCarriers
	//
	// Zero means the leased float was consumed exactly with nothing
	// reclaimed and no operator change output exists. Any non-zero value
	// is guaranteed to be at least OutputFloor so the chained float VTXO
	// never violates the operator's VTXO floor.
	OperatorChange btcutil.Amount
}

// RequiredCarrierLeaseSat returns the minimum float value a transfer must
// request from LeaseOORCarrier to fund newAssetLeafCount leaves at
// outputFloor.
func RequiredCarrierLeaseSat(newAssetLeafCount int,
	outputFloor btcutil.Amount) (btcutil.Amount, error) {

	switch {
	case newAssetLeafCount <= 0:
		return 0, fmt.Errorf("new asset leaf count must be positive")

	case outputFloor <= 0 || outputFloor > btcutil.MaxSatoshi:
		return 0, fmt.Errorf("carrier output floor must be in (0, %d]",
			int64(btcutil.MaxSatoshi))

	case outputFloor > btcutil.MaxSatoshi/btcutil.Amount(newAssetLeafCount):
		return 0, fmt.Errorf("carrier floor sum overflows")
	}

	return outputFloor * btcutil.Amount(newAssetLeafCount), nil
}

// PlanAssetCarriers computes the closed-sum Bitcoin carrier allocation for an
// operator-funded asset OOR transfer. Every new asset leaf receives
// req.OutputFloor from the leased float, round-created input carriers return
// to the sender as plain Bitcoin change, and OOR-created input carriers are
// reclaimed into the operator's float change.
func PlanAssetCarriers(req CarrierPlanRequest) (CarrierPlan, error) {
	if len(req.Inputs) == 0 {
		return CarrierPlan{}, fmt.Errorf("at least one asset input " +
			"carrier is required")
	}
	if req.LeaseValue <= 0 || req.LeaseValue > btcutil.MaxSatoshi {
		return CarrierPlan{}, fmt.Errorf("carrier lease value must be "+
			"in (0, %d]", int64(btcutil.MaxSatoshi))
	}

	floors, err := RequiredCarrierLeaseSat(
		req.NewAssetLeafCount, req.OutputFloor,
	)
	if err != nil {
		return CarrierPlan{}, err
	}

	if req.LeaseValue < floors {
		return CarrierPlan{}, fmt.Errorf("carrier float lease value "+
			"%d sat is below the %d sat needed for %d asset leaves",
			req.LeaseValue, floors, req.NewAssetLeafCount)
	}

	var senderChange, reclaimed btcutil.Amount
	for i, in := range req.Inputs {
		if in.Amount <= 0 || in.Amount > btcutil.MaxSatoshi {
			return CarrierPlan{}, fmt.Errorf("asset input %d "+
				"carrier value %d is invalid", i, in.Amount)
		}

		switch in.Origin {
		case CarrierOriginRound:
			if in.Amount > btcutil.MaxSatoshi-senderChange {
				return CarrierPlan{}, fmt.Errorf("sender " +
					"carrier sum overflows")
			}
			senderChange += in.Amount

		case CarrierOriginOOR:
			if in.Amount > btcutil.MaxSatoshi-reclaimed {
				return CarrierPlan{}, fmt.Errorf("reclaimed " +
					"carrier sum overflows")
			}
			reclaimed += in.Amount

		default:
			return CarrierPlan{}, fmt.Errorf("asset input %d "+
				"carrier origin %d is invalid", i, in.Origin)
		}
	}

	if senderChange > 0 && senderChange < req.OutputFloor {
		return CarrierPlan{}, fmt.Errorf("sender Bitcoin change %d "+
			"sat is below output floor %d sat", senderChange,
			req.OutputFloor)
	}

	residual := req.LeaseValue - floors
	if reclaimed > btcutil.MaxSatoshi-residual {
		return CarrierPlan{}, fmt.Errorf("operator float change " +
			"overflows")
	}
	operatorChange := residual + reclaimed
	if operatorChange > 0 && operatorChange < req.OutputFloor {
		return CarrierPlan{}, fmt.Errorf("operator float change %d "+
			"sat is below output floor %d sat", operatorChange,
			req.OutputFloor)
	}

	return CarrierPlan{
		NewAssetLeafCarrier: req.OutputFloor,
		NewAssetLeafTotal:   floors,
		SenderChange:        senderChange,
		ReclaimedCarriers:   reclaimed,
		OperatorChange:      operatorChange,
	}, nil
}

// ParseCarrierPubKey parses the 33-byte compressed secp256k1 public key
// advertised in GetInfoResponse.oor_carrier_pubkey.
func ParseCarrierPubKey(raw []byte) (*btcec.PublicKey, error) {
	if len(raw) != btcec.PubKeyBytesLenCompressed {
		return nil, fmt.Errorf("carrier pubkey must be %d compressed "+
			"bytes, got %d", btcec.PubKeyBytesLenCompressed,
			len(raw))
	}

	pub, err := btcec.ParsePubKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse carrier pubkey: %w", err)
	}

	return pub, nil
}

// ValidateCarrierFloatKeys verifies that the operator's carrier float owner
// key and the operator's signing key are non-nil and have distinct x-only
// public keys. A collaborative 2-of-2 leaf with identical x-only keys cannot
// be spent cleanly because duplicate pubkey signatures collide in PSBT witness
// assembly.
func ValidateCarrierFloatKeys(
	floatOwnerKey, operatorKey *btcec.PublicKey) error {

	switch {
	case floatOwnerKey == nil:
		return fmt.Errorf("carrier float owner key must be provided")

	case operatorKey == nil:
		return fmt.Errorf("operator key must be provided")
	}

	if bytes.Equal(
		schnorr.SerializePubKey(floatOwnerKey),
		schnorr.SerializePubKey(operatorKey),
	) {
		return fmt.Errorf("carrier float owner key must differ from " +
			"operator key")
	}

	return nil
}
