package lnruntime

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
)

// fundingAuthorizer is local evidence; it is never exposed as a peer event.
type fundingAuthorizer interface {
	AuthorizeFundingBacking(context.Context, arkchannel.Terms,
		arkchannel.VTXOBinding, wire.OutPoint, func() error) error

	SignAuthorizedBacking(context.Context, arkchannel.Terms, chainhash.Hash,
		func() ([]byte, error)) ([]byte, error)
}

// fundingAuthorizer is shared by both native endpoints, including the local
// initiator which does not pass through a funding RPC.
func (e *NativeFundingEndpoint) fundingAuthorizer() (fundingAuthorizer, error) {
	e.mu.RLock()
	authorizer, ok := e.sink.(fundingAuthorizer)
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("durable funding authorizer is required")
	}

	return authorizer, nil
}
