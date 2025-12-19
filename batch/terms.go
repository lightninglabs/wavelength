package batch

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/keychain"
)

// Terms encapsulates the various parameters and conditions that define a batch.
type Terms struct {
	// OperatorKey is the key descriptor for the operator's identity key.
	// This is the key that will be used as the signer in the musig2
	// signing sessions.
	OperatorKey keychain.KeyDescriptor

	// SweepKey is the public key used in the sweep path of VTXO trees.
	SweepKey keychain.KeyDescriptor

	// SweepDelay is the CSV delay for the sweep path in VTXO trees.
	SweepDelay uint32

	// MaxVTXOsPerTree is the maximum number of VTXOs in a single tree.
	MaxVTXOsPerTree uint32

	// TreeRadix is the branching factor for VTXO trees.
	TreeRadix uint32

	// BoardingExitDelay is the minimum exit delay for boarding inputs.
	BoardingExitDelay uint32

	// BoardingExitDelaySafetyMargin is the number of blocks before the
	// exit delay that we stop accepting boarding inputs. This ensures the
	// operator has enough time to construct and broadcast the round
	// transaction before the client can claim via the delay path.
	BoardingExitDelaySafetyMargin uint32

	// MinBoardingConfirmations is the minimum confirmation requirement for
	// boarding inputs.
	MinBoardingConfirmations uint32

	// MinVTXOAmount is the minimum amount for a VTXO request.
	MinVTXOAmount btcutil.Amount

	// MaxVTXOAmount is the maximum amount for a VTXO request.
	MaxVTXOAmount btcutil.Amount

	// VTXOExitDelay is the minimum exit delay for VTXO requests.
	VTXOExitDelay uint32

	// MinLeaveAmount is the minimum amount for a leave request output.
	MinLeaveAmount btcutil.Amount

	// RegistrationTimeout is the duration to wait for client registrations
	// before sealing a round.
	RegistrationTimeout time.Duration
}
