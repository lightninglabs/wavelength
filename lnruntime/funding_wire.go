package lnruntime

import (
	"errors"

	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	fundingWireMessageType = lnwire.MessageType(42069)
)

var (

	// errFundingWireRequestRejected proves the peer returned an error
	// response. Unlike a send or wait failure, its outcome is definitive.
	errFundingWireRequestRejected = errors.New("funding wire request " +
		"rejected")
)
