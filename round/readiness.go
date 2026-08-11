package round

import (
	"context"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/tree"
)

const maxRoundReadinessTokenSize = 4096

// RoundReadinessOutput is one exact VTXO after round-tree validation.
type RoundReadinessOutput struct {
	SigningKey     SignerKey
	VTXOOutpoint   wire.OutPoint
	Amount         btcutil.Amount
	PolicyTemplate []byte
	PkScript       []byte
	TreePath       *tree.Tree
}

// Clone returns an output without aliases to mutable byte slices.
func (o RoundReadinessOutput) Clone() RoundReadinessOutput {
	o.PolicyTemplate = slices.Clone(o.PolicyTemplate)
	o.PkScript = slices.Clone(o.PkScript)

	return o
}

// RoundReadinessRequest contains all exact outputs in one validated round.
type RoundReadinessRequest struct {
	RoundID        RoundID
	CommitmentTxID chainhash.Hash
	Outputs        []RoundReadinessOutput
}

// Clone returns a request copy suitable for an asynchronous worker.
func (r RoundReadinessRequest) Clone() RoundReadinessRequest {
	r.Outputs = make([]RoundReadinessOutput, len(r.Outputs))
	for i := range r.Outputs {
		r.Outputs[i] = r.Outputs[i].Clone()
	}

	return r
}

// RoundReadinessGate prepares constrained outputs and marks the exact point
// where the round releases its signing nonce.
type RoundReadinessGate interface {
	// AwaitSigningAuthorization may negotiate with a peer. It runs outside
	// the round actor and returns an opaque durable correlation token.
	AwaitSigningAuthorization(context.Context,
		RoundReadinessRequest) ([]byte, error)

	// CommitSigningAuthorization is a short durable operation invoked in
	// the FSM immediately before nonce generation.
	CommitSigningAuthorization(context.Context, RoundReadinessRequest,
		[]byte) error
}

// AwaitRoundReadinessRequest asks the actor to run slow preparation work.
type AwaitRoundReadinessRequest struct {
	Request RoundReadinessRequest
}

func (*AwaitRoundReadinessRequest) clientOutMsgSealed() {}

// RoundReadinessResolved returns asynchronous preparation to the round FSM.
type RoundReadinessResolved struct {
	actor.BaseMessage

	RoundID RoundID
	Token   []byte
	Err     error
}

func (*RoundReadinessResolved) clientEventSealed() {}

// RoundReceivable marks the result as a round actor message.
func (*RoundReadinessResolved) RoundReceivable() {}

// MessageType returns the stable actor message type.
func (*RoundReadinessResolved) MessageType() string {
	return "RoundReadinessResolved"
}

// validate checks correlation and bounds before the result enters the FSM.
func (e *RoundReadinessResolved) validate(roundID RoundID) error {
	if e == nil {
		return fmt.Errorf("round readiness result is required")
	}
	if e.RoundID != roundID {
		return fmt.Errorf("round readiness result belongs to "+
			"%s, want %s", e.RoundID, roundID)
	}
	if len(e.Token) > maxRoundReadinessTokenSize {
		return fmt.Errorf("round readiness token has length %d, "+
			"maximum %d", len(e.Token), maxRoundReadinessTokenSize)
	}

	return nil
}
