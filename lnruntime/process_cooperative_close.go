package lnruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// CooperativeCloseDeliverySource returns a wallet-owned settlement script.
// Repeated calls for one channel ID must return the same script so a client can
// recover after the hub stored the request but its response was lost.
type CooperativeCloseDeliverySource interface {
	CooperativeCloseDelivery(context.Context, arkchannel.ID) ([]byte, error)
}

// CooperativeCloseDeliverySourceFunc adapts an idempotent wallet address
// allocator to the cooperative-close process.
type CooperativeCloseDeliverySourceFunc func(context.Context, arkchannel.ID) (
	[]byte, error)

// CooperativeCloseDelivery invokes the wrapped wallet allocator.
func (f CooperativeCloseDeliverySourceFunc) CooperativeCloseDelivery(
	ctx context.Context, id arkchannel.ID) ([]byte, error) {

	return f(ctx, id)
}

// cooperativeCloseOperationLock serializes public and peer operations per
// channel while allowing unrelated channels to make progress concurrently.
type cooperativeCloseOperationLock struct {
	mu    sync.Mutex
	locks map[arkchannel.ID]*cooperativeCloseOperation
}

// cooperativeCloseOperation owns one channel's process-local mutex and tracks
// references so idle entries can be removed.
type cooperativeCloseOperation struct {
	mu   sync.Mutex
	refs uint32
}

// lock acquires one channel's operation mutex and returns its release function.
func (l *cooperativeCloseOperationLock) lock(id arkchannel.ID) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[arkchannel.ID]*cooperativeCloseOperation)
	}
	operation := l.locks[id]
	if operation == nil {
		operation = &cooperativeCloseOperation{}
		l.locks[id] = operation
	}
	operation.refs++
	l.mu.Unlock()

	operation.mu.Lock()

	return func() {
		operation.mu.Unlock()

		l.mu.Lock()
		defer l.mu.Unlock()
		operation.refs--
		if operation.refs == 0 {
			delete(l.locks, id)
		}
	}
}

// ProcessCooperativeClosePeer is the one-way authenticated session driven by
// the client. The hub never calls back into the client's mailbox while a
// request is in flight.
type ProcessCooperativeClosePeer interface {
	BeginCooperativeClose(context.Context, arkchannel.ID, []byte,
		chainfee.SatPerKWeight) (arkchannel.CooperativeCloseRequest,
		CleanChannelState, *arkchannel.CooperativeClose, error)

	CompleteCooperativeClose(context.Context, arkchannel.ID,
		arkchannel.CooperativeCloseProposal,
		input.Signature) (arkchannel.CooperativeClose, error)

	AcknowledgeCooperativeCloseSigned(context.Context, arkchannel.ID,
		arkchannel.CooperativeClose) error

	PublishCooperativeClose(context.Context,
		arkchannel.ID) (chainhash.Hash, error)

	AcknowledgeCooperativeCloseFinalized(context.Context,
		arkchannel.ID) error

	AbortCooperativeClose(context.Context, arkchannel.ID) error
}

// ClientCooperativeCloseProcess drives a direct close entirely from the
// client. Its peer calls are strictly client-to-hub, which avoids nested
// request deadlocks on a per-client mailbox ingress loop.
type ClientCooperativeCloseProcess struct {
	local    *NativeCooperativeCloseEndpoint
	peer     ProcessCooperativeClosePeer
	delivery CooperativeCloseDeliverySource

	mu      sync.RWMutex
	service CooperativeCloseStateSink
	locks   cooperativeCloseOperationLock
}

// NewClientCooperativeCloseProcess constructs the client-owned close process.
func NewClientCooperativeCloseProcess(local *NativeCooperativeCloseEndpoint,
	peer ProcessCooperativeClosePeer,
	delivery CooperativeCloseDeliverySource) (
	*ClientCooperativeCloseProcess, error) {

	if local == nil || local.party != arkchannel.PartyClient {
		return nil, fmt.Errorf("client cooperative close endpoint is " +
			"required")
	}
	if peer == nil {
		return nil, fmt.Errorf("cooperative close peer is required")
	}
	if delivery == nil {
		return nil, fmt.Errorf("client cooperative close delivery " +
			"source is required")
	}

	return &ClientCooperativeCloseProcess{
		local:    local,
		peer:     peer,
		delivery: delivery,
	}, nil
}

// BindChannelEventSink attaches the process to its durable channel service.
func (p *ClientCooperativeCloseProcess) BindChannelEventSink(
	sink arkchannel.ChannelEventSink) error {

	service, ok := sink.(CooperativeCloseStateSink)
	if !ok {
		return fmt.Errorf("channel event sink lacks close barriers")
	}
	if err := p.local.BindChannelEventSink(sink); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.service == nil {
		p.service = service
	}

	return nil
}

// RequestCooperativeClose allocates the client's payout, lets the hub fix its
// own payout, then persists and executes the local durable request.
func (p *ClientCooperativeCloseProcess) RequestCooperativeClose(
	ctx context.Context, id arkchannel.ID, feeRate chainfee.SatPerKWeight) (
	arkchannel.Record, error) {

	unlock := p.locks.lock(id)
	defer unlock()

	service, err := p.stateService()
	if err != nil {
		return arkchannel.Record{}, err
	}
	record, err := service.GetChannel(ctx, id)
	if err != nil {
		return arkchannel.Record{}, err
	}
	if record.Snapshot.CooperativeClose != nil &&
		record.Snapshot.ClientCloseFinalized {

		if err := p.peer.AcknowledgeCooperativeCloseFinalized(
			ctx, id,
		); err != nil {
			return arkchannel.Record{}, err
		}

		return record, nil
	}

	var request arkchannel.CooperativeCloseRequest
	if record.Snapshot.CooperativeCloseRequest != nil {
		request = record.Snapshot.CooperativeCloseRequest.Clone()
		if request.FeeRate != feeRate {
			return arkchannel.Record{}, fmt.Errorf("channel " +
				"already has a cooperative close at another " +
				"fee rate")
		}

		return service.ResumeChannelAction(ctx, id)
	} else {
		clientScript, err := p.delivery.CooperativeCloseDelivery(
			ctx, id,
		)
		if err != nil {
			return arkchannel.Record{}, fmt.Errorf("allocate "+
				"client close delivery: %w", err)
		}
		request, _, _, err = p.peer.BeginCooperativeClose(
			ctx, id, clientScript, feeRate,
		)
		if err != nil {
			return arkchannel.Record{}, err
		}
		if !bytes.Equal(request.ClientDeliveryScript, clientScript) ||
			request.FeeRate != feeRate {
			return arkchannel.Record{}, fmt.Errorf("hub changed " +
				"client cooperative close terms")
		}
	}

	return service.RequestCooperativeClose(ctx, id, request)
}

// GetChannel returns the local durable channel record.
func (p *ClientCooperativeCloseProcess) GetChannel(ctx context.Context,
	id arkchannel.ID) (arkchannel.Record, error) {

	service, err := p.stateService()
	if err != nil {
		return arkchannel.Record{}, err
	}

	return service.GetChannel(ctx, id)
}

// NegotiateCooperativeClose quiesces both endpoints, reconciles their native
// lnd state, and completes the two-database signed-artifact barrier.
func (p *ClientCooperativeCloseProcess) NegotiateCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding, backing arkchannel.Backing,
	request arkchannel.CooperativeCloseRequest) error {

	service, err := p.stateService()
	if err != nil {
		return err
	}
	localRecord, err := service.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	remoteRequest, remoteState, remoteSettlement, err :=
		p.peer.BeginCooperativeClose(
			ctx, id, request.ClientDeliveryScript, request.FeeRate,
		)
	if err != nil {
		return err
	}
	if !processCooperativeCloseRequestsEqual(request, remoteRequest) {
		return fmt.Errorf("hub stored another cooperative close " +
			"request")
	}

	settlement := localRecord.Snapshot.CooperativeClose
	if settlement == nil {
		settlement = remoteSettlement
	}
	if settlement == nil {
		localState, err := p.local.QuiesceCooperativeClose(
			ctx, id, terms, source, backing, request,
		)
		if err != nil {
			return p.abort(ctx, id, backing, err)
		}
		clientBalance, hubBalance, err := reconcileCleanChannelStates(
			arkchannel.PartyClient, localState, remoteState,
		)
		if err != nil {
			return p.abort(ctx, id, backing, err)
		}
		template, err := arkchannel.NewCooperativeCloseTemplate(
			terms, source, request, clientBalance, hubBalance,
			localState.CommitmentHeight,
		)
		if err != nil {
			return p.abort(ctx, id, backing, err)
		}
		proposal := template.Proposal()
		clientSig, err := p.local.SignCooperativeClose(
			ctx, id, terms, source, backing, request, proposal,
		)
		if err != nil {
			return p.abort(ctx, id, backing, err)
		}

		completed, err := p.peer.CompleteCooperativeClose(
			ctx, id, proposal, clientSig,
		)
		if err != nil {

			// The hub may have stored the signed transaction before
			// its response was lost. Keep both links quiesced for
			// replay.
			return fmt.Errorf("hub complete cooperative close: %w",
				err)
		}
		settlement = &completed
	}
	if err := settlement.Validate(terms, source, request); err != nil {
		return fmt.Errorf("validate hub cooperative close: %w", err)
	}

	if _, err := service.RecordChannelEvent(
		ctx, id, &arkchannel.CooperativeCloseSigned{
			Close: settlement.Clone(),
			Party: arkchannel.PartyClient,
		},
	); err != nil {
		return err
	}
	if err := p.peer.AcknowledgeCooperativeCloseSigned(
		ctx, id, settlement.Clone(),
	); err != nil {
		return err
	}
	if _, err := service.RecordChannelEvent(
		ctx, id, &arkchannel.CooperativeCloseSigned{
			Close: settlement.Clone(), Party: arkchannel.PartyHub,
		},
	); err != nil {
		return err
	}

	return p.publishAndFinalize(
		ctx, id, terms, backing, source, request, settlement.Clone(),
	)
}

// PublishCooperativeClose resumes a client that crashed after both databases
// stored the signed settlement but before hub publication completed.
func (p *ClientCooperativeCloseProcess) PublishCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding,
	settlement arkchannel.CooperativeClose) error {

	service, err := p.stateService()
	if err != nil {
		return err
	}
	record, err := service.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	if record.Snapshot.Backing == nil ||
		record.Snapshot.CooperativeCloseRequest == nil {
		return fmt.Errorf("client cooperative close artifacts are " +
			"incomplete")
	}

	return p.publishAndFinalize(
		ctx, id, terms, *record.Snapshot.Backing, source,
		*record.Snapshot.CooperativeCloseRequest, settlement,
	)
}

// FinalizeCooperativeClose resumes local archival after publication was
// already recorded by the durable channel FSM.
func (p *ClientCooperativeCloseProcess) FinalizeCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	backing arkchannel.Backing, source arkchannel.VTXOBinding,
	request arkchannel.CooperativeCloseRequest,
	settlement arkchannel.CooperativeClose) error {

	return p.finalizeClient(
		ctx, id, terms, backing, source, request, settlement,
	)
}

// publishAndFinalize asks the hub to publish only after both durable signed
// barriers exist, then archives the client endpoint.
func (p *ClientCooperativeCloseProcess) publishAndFinalize(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms, backing arkchannel.Backing,
	source arkchannel.VTXOBinding,
	request arkchannel.CooperativeCloseRequest,
	settlement arkchannel.CooperativeClose) error {

	txID, err := p.peer.PublishCooperativeClose(ctx, id)
	if err != nil {
		return err
	}
	if txID != settlement.TxID {
		return fmt.Errorf("hub published another cooperative close")
	}
	service, err := p.stateService()
	if err != nil {
		return err
	}
	if _, err := service.RecordChannelEvent(
		ctx, id, &arkchannel.CooperativeClosePublished{
			TxID: txID,
		},
	); err != nil {
		return err
	}
	if _, err := service.RecordChannelEvent(
		ctx, id, &arkchannel.CooperativeCloseFinalized{
			Party: arkchannel.PartyHub,
		},
	); err != nil {
		return err
	}

	return p.finalizeClient(
		ctx, id, terms, backing, source, request, settlement,
	)
}

// finalizeClient archives the local lnd channel and acknowledges that durable
// fact to the hub.
func (p *ClientCooperativeCloseProcess) finalizeClient(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms, backing arkchannel.Backing,
	source arkchannel.VTXOBinding,
	request arkchannel.CooperativeCloseRequest,
	settlement arkchannel.CooperativeClose) error {

	if err := p.local.finalize(
		terms, backing, source, request, settlement,
	); err != nil {
		return err
	}
	service, err := p.stateService()
	if err != nil {
		return err
	}
	if _, err := service.RecordChannelEvent(
		ctx, id, &arkchannel.CooperativeCloseFinalized{
			Party: arkchannel.PartyClient,
		},
	); err != nil {
		return err
	}

	return p.peer.AcknowledgeCooperativeCloseFinalized(ctx, id)
}

// abort restores both quiesced links while no fully signed settlement exists.
func (p *ClientCooperativeCloseProcess) abort(ctx context.Context,
	id arkchannel.ID, backing arkchannel.Backing, cause error) error {

	remoteErr := p.peer.AbortCooperativeClose(ctx, id)
	p.local.ResumeCooperativeClose(backing)
	service, serviceErr := p.stateService()
	if serviceErr == nil {
		_, serviceErr = service.RecordChannelEvent(
			ctx, id, &arkchannel.CooperativeCloseAborted{},
		)
	}

	return errors.Join(cause, remoteErr, serviceErr)
}

// stateService returns the bound durable channel FSM service.
func (p *ClientCooperativeCloseProcess) stateService() (
	CooperativeCloseStateSink, error) {

	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.service == nil {
		return nil, fmt.Errorf("client cooperative close service is " +
			"not bound")
	}

	return p.service, nil
}

// HubCooperativeCloseProcess owns the operator endpoint's local actions. It
// never calls the client while serving a client request.
type HubCooperativeCloseProcess struct {
	local     *NativeCooperativeCloseEndpoint
	operator  CooperativeCloseOperatorSigner
	publisher CooperativeClosePublisher
	delivery  CooperativeCloseDeliverySource

	mu      sync.RWMutex
	service CooperativeCloseStateSink
	locks   cooperativeCloseOperationLock
}

// NewHubCooperativeCloseProcess constructs the hub-owned close process.
func NewHubCooperativeCloseProcess(local *NativeCooperativeCloseEndpoint,
	operator CooperativeCloseOperatorSigner,
	publisher CooperativeClosePublisher,
	delivery CooperativeCloseDeliverySource) (*HubCooperativeCloseProcess,
	error) {

	if local == nil || local.party != arkchannel.PartyHub {
		return nil, fmt.Errorf("hub cooperative close endpoint is " +
			"required")
	}
	if operator == nil {
		return nil, fmt.Errorf("Ark operator close signer is required")
	}
	if publisher == nil {
		return nil, fmt.Errorf("cooperative close publisher is " +
			"required")
	}
	if delivery == nil {
		return nil, fmt.Errorf("hub cooperative close delivery " +
			"source is required")
	}

	return &HubCooperativeCloseProcess{
		local: local, operator: operator, publisher: publisher,
		delivery: delivery,
	}, nil
}

// BindChannelEventSink attaches the process to its durable channel service.
func (p *HubCooperativeCloseProcess) BindChannelEventSink(
	sink arkchannel.ChannelEventSink) error {

	service, ok := sink.(CooperativeCloseStateSink)
	if !ok {
		return fmt.Errorf("channel event sink lacks close barriers")
	}
	if err := p.local.BindChannelEventSink(sink); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.service == nil {
		p.service = service
	}

	return nil
}

// BeginCooperativeClose allocates the hub payout exactly once, persists the
// request, and returns the hub's authoritative clean channel state.
func (p *HubCooperativeCloseProcess) BeginCooperativeClose(ctx context.Context,
	id arkchannel.ID, clientScript []byte, feeRate chainfee.SatPerKWeight) (
	arkchannel.CooperativeCloseRequest, CleanChannelState,
	*arkchannel.CooperativeClose, error) {

	unlock := p.locks.lock(id)
	defer unlock()

	service, err := p.stateService()
	if err != nil {
		return arkchannel.CooperativeCloseRequest{},
			CleanChannelState{}, nil, err
	}
	record, err := service.GetChannel(ctx, id)
	if err != nil {
		return arkchannel.CooperativeCloseRequest{},
			CleanChannelState{}, nil, err
	}
	var request arkchannel.CooperativeCloseRequest
	if record.Snapshot.CooperativeCloseRequest != nil {
		request = record.Snapshot.CooperativeCloseRequest.Clone()
		if !bytes.Equal(request.ClientDeliveryScript, clientScript) ||
			request.FeeRate != feeRate {
			return arkchannel.CooperativeCloseRequest{},
				CleanChannelState{}, nil, fmt.Errorf(
					"channel already has another " +
						"cooperative close request")
		}
	} else {
		hubScript, err := p.delivery.CooperativeCloseDelivery(ctx, id)
		if err != nil {
			return arkchannel.CooperativeCloseRequest{},
				CleanChannelState{}, nil, fmt.Errorf(
					"allocate hub close delivery: %w", err)
		}
		request = arkchannel.CooperativeCloseRequest{
			Initiator: arkchannel.PartyClient,
			ClientDeliveryScript: append(
				[]byte(nil), clientScript...,
			),
			HubDeliveryScript: append([]byte(nil), hubScript...),
			FeeRate:           feeRate,
		}
		if _, err := service.RequestCooperativeClose(
			ctx, id, request,
		); err != nil {
			return arkchannel.CooperativeCloseRequest{},
				CleanChannelState{}, nil, err
		}
		record, err = service.GetChannel(ctx, id)
		if err != nil {
			return arkchannel.CooperativeCloseRequest{},
				CleanChannelState{}, nil, err
		}
	}
	if record.Snapshot.Source == nil || record.Snapshot.Backing == nil {
		return arkchannel.CooperativeCloseRequest{}, CleanChannelState{}, //nolint:ll
			nil, fmt.Errorf("hub cooperative close artifacts are " +
				"incomplete")
	}
	cleanState, err := p.local.QuiesceCooperativeClose(
		ctx, id, record.Snapshot.Terms, *record.Snapshot.Source,
		*record.Snapshot.Backing, request,
	)
	if err != nil {
		return arkchannel.CooperativeCloseRequest{}, CleanChannelState{}, //nolint:ll
			nil, err
	}
	var settlement *arkchannel.CooperativeClose
	if record.Snapshot.CooperativeClose != nil {
		closeCopy := record.Snapshot.CooperativeClose.Clone()
		settlement = &closeCopy
	}

	return request, cleanState, settlement, nil
}

// CompleteCooperativeClose assembles and stores the three-signature spend.
func (p *HubCooperativeCloseProcess) CompleteCooperativeClose(
	ctx context.Context, id arkchannel.ID,
	proposal arkchannel.CooperativeCloseProposal,
	clientSig input.Signature) (arkchannel.CooperativeClose, error) {

	unlock := p.locks.lock(id)
	defer unlock()

	service, err := p.stateService()
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	record, err := service.GetChannel(ctx, id)
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	if record.Snapshot.Source == nil || record.Snapshot.Backing == nil ||
		record.Snapshot.CooperativeCloseRequest == nil {
		return arkchannel.CooperativeClose{}, fmt.Errorf("hub " +
			"cooperative close artifacts are incomplete")
	}

	return completeCooperativeClose(
		ctx, p.local, p.operator, id, record.Snapshot.Terms,
		*record.Snapshot.Source, *record.Snapshot.Backing,
		*record.Snapshot.CooperativeCloseRequest, proposal, clientSig,
	)
}

// AcknowledgeCooperativeCloseSigned records that the client durably stored the
// same fully signed transaction.
func (p *HubCooperativeCloseProcess) AcknowledgeCooperativeCloseSigned(
	ctx context.Context, id arkchannel.ID,
	settlement arkchannel.CooperativeClose) error {

	unlock := p.locks.lock(id)
	defer unlock()

	service, err := p.stateService()
	if err != nil {
		return err
	}
	_, err = service.RecordChannelEvent(
		ctx, id, &arkchannel.CooperativeCloseSigned{
			Close: settlement, Party: arkchannel.PartyClient,
		},
	)

	return err
}

// PublishCooperativeClose publishes once both signed acknowledgements are
// durable, then archives the hub's native channel before replying.
func (p *HubCooperativeCloseProcess) PublishCooperativeClose(
	ctx context.Context, id arkchannel.ID) (chainhash.Hash, error) {

	unlock := p.locks.lock(id)
	defer unlock()

	service, err := p.stateService()
	if err != nil {
		return chainhash.Hash{}, err
	}
	for range 3 {
		record, err := service.GetChannel(ctx, id)
		if err != nil {
			return chainhash.Hash{}, err
		}
		if record.Snapshot.CooperativeClose == nil {
			return chainhash.Hash{}, fmt.Errorf("hub has no " +
				"signed cooperative close")
		}
		if record.Snapshot.HubCloseFinalized {
			return record.Snapshot.CooperativeClose.TxID, nil
		}
		if _, err := service.ResumeChannelAction(ctx, id); err != nil {
			return chainhash.Hash{}, err
		}
	}

	return chainhash.Hash{}, fmt.Errorf("hub cooperative close did not " +
		"reach local finalization")
}

// AcknowledgeCooperativeCloseFinalized records client-side channel archival.
func (p *HubCooperativeCloseProcess) AcknowledgeCooperativeCloseFinalized(
	ctx context.Context, id arkchannel.ID) error {

	unlock := p.locks.lock(id)
	defer unlock()

	service, err := p.stateService()
	if err != nil {
		return err
	}
	_, err = service.RecordChannelEvent(
		ctx, id, &arkchannel.CooperativeCloseFinalized{
			Party: arkchannel.PartyClient,
		},
	)

	return err
}

// AbortCooperativeClose restores the hub link before signatures exist.
func (p *HubCooperativeCloseProcess) AbortCooperativeClose(ctx context.Context,
	id arkchannel.ID) error {

	unlock := p.locks.lock(id)
	defer unlock()

	service, err := p.stateService()
	if err != nil {
		return err
	}
	record, err := service.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	if record.Snapshot.CooperativeClose != nil {
		return fmt.Errorf("cannot abort a signed cooperative close")
	}
	if record.Snapshot.Backing == nil {
		return fmt.Errorf("hub cooperative close backing is missing")
	}
	p.local.ResumeCooperativeClose(*record.Snapshot.Backing)
	_, err = service.RecordChannelEvent(
		ctx, id, &arkchannel.CooperativeCloseAborted{},
	)

	return err
}

// NegotiateCooperativeClose performs only the hub-local quiescence action.
func (p *HubCooperativeCloseProcess) NegotiateCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding, backing arkchannel.Backing,
	request arkchannel.CooperativeCloseRequest) error {

	_, err := p.local.QuiesceCooperativeClose(
		ctx, id, terms, source, backing, request,
	)

	return err
}

// publishCooperativeCloseAction settles and records the hub-local publication
// action without calling back into the client process.
func (p *HubCooperativeCloseProcess) publishCooperativeCloseAction(
	ctx context.Context, id arkchannel.ID, source arkchannel.VTXOBinding,
	settlement arkchannel.CooperativeClose) error {

	if err := p.publisher.SettleCooperativeClose(
		ctx, id, source, settlement,
	); err != nil {
		return err
	}
	service, err := p.stateService()
	if err != nil {
		return err
	}
	_, err = service.RecordChannelEvent(
		ctx, id, &arkchannel.CooperativeClosePublished{
			TxID: settlement.TxID,
		},
	)

	return err
}

// FinalizeCooperativeClose archives the hub-local channel and records its
// acknowledgement without a nested client RPC.
func (p *HubCooperativeCloseProcess) FinalizeCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	backing arkchannel.Backing, source arkchannel.VTXOBinding,
	request arkchannel.CooperativeCloseRequest,
	settlement arkchannel.CooperativeClose) error {

	if err := p.local.finalize(
		terms, backing, source, request, settlement,
	); err != nil {
		return err
	}
	service, err := p.stateService()
	if err != nil {
		return err
	}
	_, err = service.RecordChannelEvent(
		ctx, id, &arkchannel.CooperativeCloseFinalized{
			Party: arkchannel.PartyHub,
		},
	)

	return err
}

// stateService returns the bound durable channel FSM service.
func (p *HubCooperativeCloseProcess) stateService() (CooperativeCloseStateSink,
	error) {

	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.service == nil {
		return nil, fmt.Errorf("hub cooperative close service is not " +
			"bound")
	}

	return p.service, nil
}

// HubCooperativeCloseExecutor adapts the process to NativeExecutor without
// overloading the peer-facing PublishCooperativeClose method name.
type HubCooperativeCloseExecutor struct {
	*HubCooperativeCloseProcess
}

// PublishCooperativeClose executes the hub-local publication action.
func (e *HubCooperativeCloseExecutor) PublishCooperativeClose(
	ctx context.Context, id arkchannel.ID, _ arkchannel.Terms,
	source arkchannel.VTXOBinding,
	settlement arkchannel.CooperativeClose) error {

	return e.publishCooperativeCloseAction(ctx, id, source, settlement)
}

var _ arkchannel.ChannelCooperativeCloser = (*ClientCooperativeCloseProcess)(nil) //nolint:ll

var _ arkchannel.ChannelCooperativeCloser = (*HubCooperativeCloseExecutor)(nil)

// processCooperativeCloseRequestsEqual compares every negotiated close term.
func processCooperativeCloseRequestsEqual(
	a, b arkchannel.CooperativeCloseRequest) bool {

	return a.Initiator == b.Initiator && a.FeeRate == b.FeeRate &&
		bytes.Equal(a.ClientDeliveryScript, b.ClientDeliveryScript) &&
		bytes.Equal(a.HubDeliveryScript, b.HubDeliveryScript)
}
