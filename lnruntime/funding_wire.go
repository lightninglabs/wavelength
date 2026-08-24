package lnruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"google.golang.org/protobuf/proto"
)

const (
	fundingWireMessageType = lnwire.MessageType(42069)

	fundingWireRequest = arkchannelrpc.
				FundingWireKind_FUNDING_WIRE_KIND_REQUEST
	fundingWireResponse = arkchannelrpc.
				FundingWireKind_FUNDING_WIRE_KIND_RESPONSE
	// The generated enum identifiers cannot be wrapped as Go selectors.
	fundingWireSignBacking       = arkchannelrpc.FundingWireMethod_FUNDING_WIRE_METHOD_SIGN_BACKING        //nolint:ll
	fundingWireInstallBacking    = arkchannelrpc.FundingWireMethod_FUNDING_WIRE_METHOD_INSTALL_BACKING     //nolint:ll
	fundingWireFundingFinalized  = arkchannelrpc.FundingWireMethod_FUNDING_WIRE_METHOD_FUNDING_FINALIZED   //nolint:ll
	fundingWireChannelActive     = arkchannelrpc.FundingWireMethod_FUNDING_WIRE_METHOD_CHANNEL_ACTIVE      //nolint:ll
	fundingWireApplyChannelEvent = arkchannelrpc.FundingWireMethod_FUNDING_WIRE_METHOD_APPLY_CHANNEL_EVENT //nolint:ll
)

var errFundingWireClosed = errors.New("funding wire is closed")

// FundingWireServerConfig contains the client-local objects exposed to the
// hub's funding coordinator over the authenticated peer transport.
type FundingWireServerConfig struct {
	Service *arkchannel.Service
	Funding *NativeFundingEndpoint
}

// FundingWire carries a small idempotent request/response protocol alongside
// BOLT funding messages. The application owns Ark coordination while lnd owns
// the ordinary channel-open stream.
type FundingWire struct {
	peer *Peer

	mu      sync.Mutex
	server  *FundingWireServerConfig
	pending map[[32]byte]chan fundingWireResult
	closed  chan struct{}
}

type fundingWireResult struct {
	body []byte
	err  error
}

// NewFundingWire constructs a reverse funding transport for one logical peer.
func NewFundingWire(peer *Peer) (*FundingWire, error) {
	if peer == nil {
		return nil, fmt.Errorf("funding wire peer is required")
	}

	return &FundingWire{
		peer: peer, pending: make(map[[32]byte]chan fundingWireResult),
		closed: make(chan struct{}),
	}, nil
}

// BindServer installs the client-side request handler exactly once.
func (w *FundingWire) BindServer(cfg FundingWireServerConfig) error {
	if cfg.Service == nil || cfg.Funding == nil {
		return fmt.Errorf("funding wire server is incomplete")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.server != nil {
		return fmt.Errorf("funding wire server is already bound")
	}
	w.server = &cfg

	return nil
}

// Counterparty exposes the hub-side funding coordinator interface.
func (w *FundingWire) Counterparty() FundingCounterparty {
	return &wireFundingCounterparty{wire: w}
}

// Handles reports whether a custom message belongs to this protocol.
func (w *FundingWire) Handles(message lnwire.Message) bool {
	custom, ok := message.(*lnwire.Custom)

	return ok && custom.Type == fundingWireMessageType
}

// Handle processes one funding request or correlated response.
func (w *FundingWire) Handle(ctx context.Context,
	message lnwire.Message) error {

	custom, ok := message.(*lnwire.Custom)
	if !ok || custom.Type != fundingWireMessageType {
		return fmt.Errorf("message is not an Ark funding wire message")
	}
	envelope := &arkchannelrpc.FundingWireEnvelope{}
	if err := proto.Unmarshal(custom.Data, envelope); err != nil {
		return fmt.Errorf("decode funding wire envelope: %w", err)
	}
	requestID, err := fundingWireRequestID(envelope.GetRequestId())
	if err != nil {
		return err
	}

	switch envelope.GetKind() {
	case fundingWireResponse:
		return w.deliverResponse(requestID, envelope)

	case fundingWireRequest:
		return w.handleRequest(ctx, requestID, envelope)

	default:
		return fmt.Errorf("unknown funding wire kind %d",
			envelope.GetKind())
	}
}

// Close releases callers waiting on a stopped endpoint.
func (w *FundingWire) Close() {
	w.mu.Lock()
	select {
	case <-w.closed:
		w.mu.Unlock()

		return

	default:
		close(w.closed)
	}
	for id, response := range w.pending {
		delete(w.pending, id)
		response <- fundingWireResult{err: errFundingWireClosed}
	}
	w.mu.Unlock()
}

func (w *FundingWire) call(ctx context.Context,
	method arkchannelrpc.FundingWireMethod,
	request, response proto.Message) error {

	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return err
	}
	requestID := sha256.Sum256(append(
		[]byte{byte(method)}, body...,
	))
	result := make(chan fundingWireResult, 1)
	w.mu.Lock()
	select {
	case <-w.closed:
		w.mu.Unlock()

		return errFundingWireClosed

	default:
	}
	if _, ok := w.pending[requestID]; ok {
		w.mu.Unlock()

		return fmt.Errorf("funding wire request is already pending")
	}
	w.pending[requestID] = result
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.pending, requestID)
		w.mu.Unlock()
	}()

	if err := w.send(&arkchannelrpc.FundingWireEnvelope{
		RequestId: requestID[:],
		Kind:      fundingWireRequest,
		Method:    method, Body: body,
	}); err != nil {
		return err
	}

	select {
	case result := <-result:
		if result.err != nil {
			return result.err
		}
		if err := proto.Unmarshal(result.body, response); err != nil {
			return fmt.Errorf("decode funding wire response: %w",
				err)
		}

		return nil

	case <-ctx.Done():
		return ctx.Err()

	case <-w.closed:
		return errFundingWireClosed
	}
}

func (w *FundingWire) handleRequest(ctx context.Context, requestID [32]byte,
	envelope *arkchannelrpc.FundingWireEnvelope) error {

	w.mu.Lock()
	server := w.server
	w.mu.Unlock()
	if server == nil {
		return fmt.Errorf("funding wire request handler is unavailable")
	}
	body, handleErr := server.handle(
		ctx, envelope.GetMethod(), envelope.GetBody(),
	)
	response := &arkchannelrpc.FundingWireEnvelope{
		RequestId: requestID[:],
		Kind:      fundingWireResponse,
		Method:    envelope.GetMethod(), Body: body,
	}
	if handleErr != nil {
		response.Error = handleErr.Error()
	}

	return w.send(response)
}

func (w *FundingWire) deliverResponse(requestID [32]byte,
	envelope *arkchannelrpc.FundingWireEnvelope) error {

	w.mu.Lock()
	result := w.pending[requestID]
	w.mu.Unlock()
	if result == nil {

		// A duplicate response can arrive after a caller completed. The
		// original operation was idempotent, so it is safe to
		// acknowledge.
		return nil
	}
	response := fundingWireResult{
		body: append([]byte(nil), envelope.GetBody()...),
	}
	if envelope.GetError() != "" {
		response.err = errors.New(envelope.GetError())
	}
	select {
	case result <- response:
	default:
	}

	return nil
}

func (w *FundingWire) send(envelope *arkchannelrpc.FundingWireEnvelope) error {
	payload, err := proto.MarshalOptions{
		Deterministic: true,
	}.Marshal(
		envelope,
	)
	if err != nil {
		return err
	}
	message, err := lnwire.NewCustom(fundingWireMessageType, payload)
	if err != nil {
		return err
	}

	return w.peer.SendMessage(true, message)
}

func (c *FundingWireServerConfig) handle(ctx context.Context,
	method arkchannelrpc.FundingWireMethod, body []byte) ([]byte, error) {

	marshal := func(message proto.Message, err error) ([]byte, error) {
		if err != nil {
			return nil, err
		}

		return proto.MarshalOptions{
			Deterministic: true,
		}.Marshal(
			message,
		)
	}

	switch method {
	case fundingWireSignBacking:
		request := &arkchannelrpc.SignBackingRequest{}
		if err := proto.Unmarshal(body, request); err != nil {
			return nil, err
		}
		id, terms, binding, err := c.fundingRequest(
			ctx, request.GetChannelId(), request.GetTerms(),
			request.GetBinding(),
		)
		if err != nil {
			return nil, err
		}
		packet, err := psbt.NewFromRawBytes(
			bytes.NewReader(
				request.GetFundingPsbt(),
			),
			false,
		)
		if err != nil {
			return nil, fmt.Errorf("decode lnd funding PSBT: %w",
				err)
		}
		signature, err := c.Funding.SignBacking(
			ctx, id, terms, binding, packet,
		)

		return marshal(&arkchannelrpc.SignBackingResponse{
			Signature: signatureBytes(signature, err),
		}, err)

	case fundingWireInstallBacking:
		request := &arkchannelrpc.InstallBackingRequest{}
		if err := proto.Unmarshal(body, request); err != nil {
			return nil, err
		}
		id, terms, binding, err := c.fundingRequest(
			ctx, request.GetChannelId(), request.GetTerms(),
			request.GetBinding(),
		)
		if err != nil {
			return nil, err
		}
		backing, err := channelBackingFromRPC(request.GetBacking())
		if err != nil {
			return nil, err
		}
		err = c.Funding.InstallBacking(
			ctx, id, terms, binding, backing,
		)

		return marshal(&arkchannelrpc.InstallBackingResponse{}, err)

	case fundingWireFundingFinalized, fundingWireChannelActive:
		request := &arkchannelrpc.FundingStatusRequest{}
		if err := proto.Unmarshal(body, request); err != nil {
			return nil, err
		}
		terms, backing, err := c.fundingStatus(ctx, request)
		if err != nil {
			return nil, err
		}
		var ready bool
		if method == fundingWireFundingFinalized {
			ready, err = c.Funding.FundingFinalized(
				ctx, terms, backing,
			)
		} else {
			ready, err = c.Funding.ChannelActive(
				ctx, terms, backing,
			)
		}

		return marshal(&arkchannelrpc.FundingStatusResponse{
			Ready: ready,
		}, err)

	case fundingWireApplyChannelEvent:
		request := &arkchannelrpc.ApplyChannelEventRequest{}
		if err := proto.Unmarshal(body, request); err != nil {
			return nil, err
		}
		id, err := rpcChannelID(request.GetChannelId())
		if err != nil {
			return nil, err
		}
		if _, err := c.Service.GetChannel(ctx, id); err != nil {
			return nil, err
		}
		event, err := channelEventFromRPC(request)
		if err != nil {
			return nil, err
		}
		var record arkchannel.Record
		if _, ready := event.(*arkchannel.FundingPeerReady); ready {
			record, err = c.Service.RecordChannelEvent(
				ctx, id, event,
			)
		} else {
			record, err = c.Funding.ApplyChannelEvent(
				ctx, id, event,
			)
		}
		if err != nil {
			return nil, err
		}

		return marshal(&arkchannelrpc.ApplyChannelEventResponse{
			Channel: ArkChannelRecordToRPC(record),
		}, nil)

	default:
		return nil, fmt.Errorf("unsupported funding wire method %d",
			method)
	}
}

func (c *FundingWireServerConfig) fundingRequest(ctx context.Context,
	rawID []byte, termsRPC *arkchannelrpc.ChannelTerms,
	bindingRPC *arkchannelrpc.ChannelVTXOBinding) (arkchannel.ID,
	arkchannel.Terms, arkchannel.VTXOBinding, error) {

	id, err := rpcChannelID(rawID)
	if err != nil {
		return arkchannel.ID{}, arkchannel.Terms{},
			arkchannel.VTXOBinding{}, err
	}
	terms, err := channelTermsFromRPC(termsRPC)
	if err != nil {
		return arkchannel.ID{}, arkchannel.Terms{},
			arkchannel.VTXOBinding{}, err
	}
	binding, err := channelBindingFromRPC(bindingRPC)
	if err != nil {
		return arkchannel.ID{}, arkchannel.Terms{},
			arkchannel.VTXOBinding{}, err
	}
	record, err := c.Service.GetChannel(ctx, id)
	if err != nil {
		return arkchannel.ID{}, arkchannel.Terms{},
			arkchannel.VTXOBinding{}, err
	}
	if id != terms.ID || record.Snapshot.Terms != terms ||
		record.Snapshot.Source == nil ||
		!sameFundingWireBinding(*record.Snapshot.Source, binding) {
		return arkchannel.ID{}, arkchannel.Terms{},
			arkchannel.VTXOBinding{}, fmt.Errorf("funding " +
				"request does not match channel FSM")
	}

	return id, terms, binding, nil
}

func (c *FundingWireServerConfig) fundingStatus(ctx context.Context,
	request *arkchannelrpc.FundingStatusRequest) (arkchannel.Terms,
	arkchannel.Backing, error) {

	terms, err := channelTermsFromRPC(request.GetTerms())
	if err != nil {
		return arkchannel.Terms{}, arkchannel.Backing{}, err
	}
	backing, err := channelBackingFromRPC(request.GetBacking())
	if err != nil {
		return arkchannel.Terms{}, arkchannel.Backing{}, err
	}
	record, err := c.Service.GetChannel(ctx, terms.ID)
	if err != nil {
		return arkchannel.Terms{}, arkchannel.Backing{}, err
	}
	if record.Snapshot.Terms != terms || record.Snapshot.Backing == nil ||
		record.Snapshot.Backing.ChannelPoint != backing.ChannelPoint ||
		!bytes.Equal(
			record.Snapshot.Backing.Transaction,
			backing.Transaction,
		) {
		return arkchannel.Terms{}, arkchannel.Backing{}, fmt.Errorf(
			"funding status does not match channel " +
				"FSM")
	}

	return terms, backing, nil
}

func sameFundingWireBinding(a, b arkchannel.VTXOBinding) bool {
	return a.OORSessionID == b.OORSessionID && a.OutPoint == b.OutPoint &&
		a.Amount == b.Amount && bytes.Equal(
		a.ArkTransaction, b.ArkTransaction,
	) && bytes.Equal(a.PolicyTemplate,
		b.PolicyTemplate) && bytes.Equal(a.PkScript, b.PkScript)
}

func fundingWireRequestID(raw []byte) ([32]byte, error) {
	var id [32]byte
	if len(raw) != len(id) {
		return id, fmt.Errorf("funding wire request ID must be 32 " +
			"bytes")
	}
	copy(id[:], raw)

	return id, nil
}

func signatureBytes(signature input.Signature, err error) []byte {
	if err != nil || signature == nil {
		return nil
	}

	return signature.Serialize()
}

type wireFundingCounterparty struct {
	wire *FundingWire
}

func (c *wireFundingCounterparty) SignBacking(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms,
	binding arkchannel.VTXOBinding, packet *psbt.Packet) (input.Signature,
	error) {

	if packet == nil {
		return nil, fmt.Errorf("lnd funding PSBT is required")
	}
	var encoded bytes.Buffer
	if err := packet.Serialize(&encoded); err != nil {
		return nil, err
	}
	response := &arkchannelrpc.SignBackingResponse{}
	err := c.wire.call(
		ctx,
		fundingWireSignBacking,
		&arkchannelrpc.SignBackingRequest{
			ChannelId: id[:], Terms: channelTermsToRPC(terms),
			Binding:     channelBindingToRPC(binding),
			FundingPsbt: encoded.Bytes(),
		}, response,
	)
	if err != nil {
		return nil, err
	}

	return schnorr.ParseSignature(response.GetSignature())
}

func (c *wireFundingCounterparty) InstallBacking(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms,
	binding arkchannel.VTXOBinding, backing arkchannel.Backing) error {

	return c.wire.call(
		ctx,
		fundingWireInstallBacking,
		&arkchannelrpc.InstallBackingRequest{
			ChannelId: id[:], Terms: channelTermsToRPC(terms),
			Binding: channelBindingToRPC(binding),
			Backing: channelBackingToRPC(backing),
		}, &arkchannelrpc.InstallBackingResponse{},
	)
}

func (c *wireFundingCounterparty) FundingFinalized(ctx context.Context,
	terms arkchannel.Terms, backing arkchannel.Backing) (bool, error) {

	return c.status(
		ctx,
		fundingWireFundingFinalized,
		terms, backing,
	)
}

func (c *wireFundingCounterparty) ChannelActive(ctx context.Context,
	terms arkchannel.Terms, backing arkchannel.Backing) (bool, error) {

	return c.status(
		ctx,
		fundingWireChannelActive,
		terms, backing,
	)
}

func (c *wireFundingCounterparty) status(ctx context.Context,
	method arkchannelrpc.FundingWireMethod, terms arkchannel.Terms,
	backing arkchannel.Backing) (bool, error) {

	response := &arkchannelrpc.FundingStatusResponse{}
	err := c.wire.call(
		ctx, method, fundingStatusRequest(terms, backing), response,
	)
	if err != nil {
		return false, err
	}

	return response.GetReady(), nil
}

func (c *wireFundingCounterparty) ApplyChannelEvent(ctx context.Context,
	id arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	request, _, err := channelEventToRPC(id, event)
	if err != nil {
		return arkchannel.Record{}, err
	}
	response := &arkchannelrpc.ApplyChannelEventResponse{}
	if err := c.wire.call(
		ctx, fundingWireApplyChannelEvent, request, response,
	); err != nil {
		return arkchannel.Record{}, err
	}
	if response.GetChannel() == nil {
		return arkchannel.Record{}, fmt.Errorf("funding wire " +
			"returned an empty channel")
	}

	return arkchannel.Record{
		Revision: response.GetChannel().GetRevision(),
	}, nil
}

var _ FundingCounterparty = (*wireFundingCounterparty)(nil)
