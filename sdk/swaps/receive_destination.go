package swaps

import (
	"context"
	"errors"
	"fmt"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
)

// ErrInvalidClaimAddress identifies an invalid external Ark destination.
var ErrInvalidClaimAddress = errors.New("invalid claim address")

// ErrExternalReceiveCredits prevents forwarding the local credit balance.
var ErrExternalReceiveCredits = errors.New("external receives require a " +
	"vHTLC without attached wallet credits")

// ReceiveOptions configures a Lightning receive before its invoice is issued.
type ReceiveOptions struct {
	// Memo is the human-readable invoice description.
	Memo string

	// ClaimAddress is another wallet's Ark receive address on the same
	// operator and Bitcoin network. Empty allocates a local destination.
	ClaimAddress string
}

// addressClaimSender preserves the exact destination script for external
// claims. An address's output key must never be used as an owner pubkey.
type addressClaimSender interface {
	SendOORWithCustomInputsToAddress(context.Context, string, int64,
		[]CustomInput) (string, error)
}

// receiveClaimScript validates the network and taproot shape before a route
// or invoice is created. The recipient must supply an Ark receive address;
// a Bitcoin address alone cannot prove registration with the Ark operator.
func receiveClaimScript(address string,
	params *chaincfg.Params) ([]byte, error) {

	if params == nil {
		return nil, fmt.Errorf("%w: Bitcoin network is not configured",
			ErrInvalidClaimAddress)
	}
	addr, err := btcaddr.DecodeAddress(address, params)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidClaimAddress, err)
	}
	if !addr.IsForNet(params) {
		return nil, fmt.Errorf("%w: wrong Bitcoin network",
			ErrInvalidClaimAddress)
	}
	if _, ok := addr.(*btcaddr.AddressTaproot); !ok {
		return nil, fmt.Errorf("%w: Ark receive address must "+
			"be taproot", ErrInvalidClaimAddress)
	}

	return txscript.PayToAddrScript(addr)
}

// receiveClaimAddress restores an external destination from the script
// persisted with the session. Local destinations retain their owner pubkey;
// external ones persist only the exact script, never an inferred owner key.
func receiveClaimAddress(pubkey, script []byte,
	params *chaincfg.Params) (string, error) {

	if len(pubkey) != 0 || len(script) == 0 {
		return "", nil
	}
	if params == nil || !txscript.IsPayToTaproot(script) {
		return "", fmt.Errorf("%w: invalid saved destination "+
			"or network", ErrInvalidClaimAddress)
	}
	addr, err := btcaddr.NewAddressTaproot(script[2:], params)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidClaimAddress, err)
	}

	return addr.EncodeAddress(), nil
}
