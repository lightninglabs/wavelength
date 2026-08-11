package lnruntime

import (
	"context"
	"fmt"
	"sync"

	"github.com/lightninglabs/wavelength/arkchannel"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"google.golang.org/protobuf/proto"
)

type loopbackCooperativeCloseRPC struct {
	mux *mailboxrpc.ServeMux

	mu        sync.Mutex
	responses map[string]loopbackCooperativeCloseResponse
}

type loopbackCooperativeCloseResponse struct {
	message proto.Message
	err     error
}

// newLoopbackCooperativeCloseRPC constructs an in-process generated RPC edge.
func newLoopbackCooperativeCloseRPC(
	mux *mailboxrpc.ServeMux) *loopbackCooperativeCloseRPC {

	return &loopbackCooperativeCloseRPC{
		mux:       mux,
		responses: make(map[string]loopbackCooperativeCloseResponse),
	}
}

// SendRPC invokes the generated mailbox mux and buffers its typed response.
func (c *loopbackCooperativeCloseRPC) SendRPC(ctx context.Context,
	method mailboxrpc.ServiceMethod, request proto.Message,
	options mailboxrpc.RPCOptions) (mailboxrpc.SendResult, error) {

	correlationID := options.CorrelationID
	if correlationID == "" {
		correlationID = options.IdempotencyKey
	}
	if correlationID == "" {
		return mailboxrpc.SendResult{}, fmt.Errorf("correlation id " +
			"is required")
	}
	payload, err := proto.Marshal(request)
	if err != nil {
		return mailboxrpc.SendResult{}, err
	}
	response, serveErr := c.mux.ServeRPC(
		ctx, method.Service, method.Method, payload,
	)

	c.mu.Lock()
	c.responses[correlationID] = loopbackCooperativeCloseResponse{
		message: response,
		err:     serveErr,
	}
	c.mu.Unlock()

	return mailboxrpc.SendResult{
		CorrelationID:  correlationID,
		IdempotencyKey: options.IdempotencyKey,
	}, nil
}

// AwaitRPC returns the correlated generated response to the caller.
func (c *loopbackCooperativeCloseRPC) AwaitRPC(_ context.Context,
	correlationID string, response proto.Message) error {

	c.mu.Lock()
	result, ok := c.responses[correlationID]
	delete(c.responses, correlationID)
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("response %q not found", correlationID)
	}
	if result.err != nil {
		return result.err
	}
	payload, err := proto.Marshal(result.message)
	if err != nil {
		return err
	}

	return (proto.UnmarshalOptions{
		DiscardUnknown: true,
	}).Unmarshal(payload,
		response,
	)
}

type lossyCooperativeClosePeer struct {
	ProcessCooperativeClosePeer

	mu           sync.Mutex
	loseBegin    bool
	loseComplete bool
}

// BeginCooperativeClose can lose one response after the hub persisted it.
func (p *lossyCooperativeClosePeer) BeginCooperativeClose(ctx context.Context,
	id arkchannel.ID, clientScript []byte, feeRate chainfee.SatPerKWeight) (
	arkchannel.CooperativeCloseRequest, CleanChannelState,
	*arkchannel.CooperativeClose, error) {

	request, state, settlement, err :=
		p.ProcessCooperativeClosePeer.BeginCooperativeClose(
			ctx, id, clientScript, feeRate,
		)
	if err != nil {
		return request, state, settlement, err
	}
	p.mu.Lock()
	lose := p.loseBegin
	p.loseBegin = false
	p.mu.Unlock()
	if lose {
		return arkchannel.CooperativeCloseRequest{}, CleanChannelState{}, //nolint:ll
			nil, fmt.Errorf("injected lost begin response")
	}

	return request, state, settlement, nil
}

// CompleteCooperativeClose can lose one fully signed response after storage.
func (p *lossyCooperativeClosePeer) CompleteCooperativeClose(
	ctx context.Context, id arkchannel.ID,
	proposal arkchannel.CooperativeCloseProposal,
	clientSignature input.Signature) (arkchannel.CooperativeClose, error) {

	settlement, err :=
		p.ProcessCooperativeClosePeer.CompleteCooperativeClose(
			ctx, id, proposal, clientSignature,
		)
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	p.mu.Lock()
	lose := p.loseComplete
	p.loseComplete = false
	p.mu.Unlock()
	if lose {
		return arkchannel.CooperativeClose{}, fmt.Errorf("injected " +
			"lost complete response")
	}

	return settlement, nil
}

type stableCooperativeCloseDelivery struct {
	mu     sync.Mutex
	script []byte
	calls  int
}

// CooperativeCloseDelivery returns the same wallet script for every retry.
func (s *stableCooperativeCloseDelivery) CooperativeCloseDelivery(
	_ context.Context, _ arkchannel.ID) ([]byte, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++

	return append([]byte(nil), s.script...), nil
}

// callCount returns the number of payout allocation attempts.
func (s *stableCooperativeCloseDelivery) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

var _ mailboxrpc.RPCClient = (*loopbackCooperativeCloseRPC)(nil)

var _ ProcessCooperativeClosePeer = (*lossyCooperativeClosePeer)(nil)

var _ CooperativeCloseDeliverySource = (*stableCooperativeCloseDelivery)(nil)
