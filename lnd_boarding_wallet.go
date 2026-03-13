package main

import (
	"github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/lndclient"
)

// LndBoardingBackend is an alias for lndbackend.BoardingBackend for backwards
// compatibility in the main package.
type LndBoardingBackend = lndbackend.BoardingBackend

// NewLndBoardingBackend creates a new LND boarding backend.
func NewLndBoardingBackend(walletKit lndclient.WalletKitClient,
	chainKit lndclient.ChainKitClient) *LndBoardingBackend {

	return lndbackend.NewBoardingBackend(walletKit, chainKit)
}
