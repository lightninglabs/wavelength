package main

import (
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/lndbackend"
)

// LndBoardingBackend is an alias for lndbackend.BoardingBackend for backwards
// compatibility in the main package.
type LndBoardingBackend = lndbackend.BoardingBackend

// NewLndBoardingBackend creates a new LND boarding backend.
func NewLndBoardingBackend(walletKit lndclient.WalletKitClient,
	chainKit lndclient.ChainKitClient) *LndBoardingBackend {

	return lndbackend.NewBoardingBackend(walletKit, chainKit)
}
