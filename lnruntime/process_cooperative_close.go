package lnruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
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
	BeginCooperativeClose(context.Context, arkchannel.ID, []byte) (
		arkchannel.CooperativeCloseRequest, CleanChannelState,
		*arkchannel.CooperativeClose, error)

	CompleteCooperativeClose(context.Context, arkchannel.ID,
		arkchannel.CooperativeCloseProposal) (
		arkchannel.CooperativeClose, error)

	AcknowledgeCooperativeCloseSigned(context.Context, arkchannel.ID,
		arkchannel.CooperativeClose) error

	PublishCooperativeClose(context.Context, arkchannel.ID,
		chainhash.Hash) (chainhash.Hash, error)

	AcknowledgeCooperativeCloseFinalized(context.Context,
		arkchannel.ID) error

	AbortCooperativeClose(context.Context, arkchannel.ID) error
}

// ClientCooperativeCloseProcess drives an in-Ark OOR close from the client.
// Its peer calls are strictly client-to-hub, which avoids nested
// request deadlocks on a per-client mailbox ingress loop.
type ClientCooperativeCloseProcess struct {
	local     *NativeCooperativeCloseEndpoint
	peer      ProcessCooperativeClosePeer
	publisher CooperativeClosePublisher
	delivery  CooperativeCloseDeliverySource

	mu      sync.RWMutex
	service CooperativeCloseStateSink
	locks   cooperativeCloseOperationLock
}

// NewClientCooperativeCloseProcess constructs the client-owned close process.
func NewClientCooperativeCloseProcess(local *NativeCooperativeCloseEndpoint,
	peer ProcessCooperativeClosePeer, publisher CooperativeClosePublisher,
	delivery CooperativeCloseDeliverySource) (
	*ClientCooperativeCloseProcess, error) {

	if local == nil || local.party != arkchannel.PartyClient {
		return nil, fmt.Errorf("client cooperative close endpoint is " +
			"required")
	}
	if peer == nil {
		return nil, fmt.Errorf("cooperative close peer is required")
	}
	if publisher == nil {
		return nil, fmt.Errorf("client cooperative close publisher " +
			"is required")
	}
	if delivery == nil {
		return nil, fmt.Errorf("client cooperative close delivery " +
			"source is required")
	}

	return &ClientCooperativeCloseProcess{
		local: local, peer: peer, publisher: publisher,
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
	ctx context.Context, id arkchannel.ID) (arkchannel.Record, error) {

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

	if record.Snapshot.CooperativeCloseRequest != nil {
		return service.ResumeChannelAction(ctx, id)
	}
	clientScript, err := p.delivery.CooperativeCloseDelivery(ctx, id)
	if err != nil {
		return arkchannel.Record{}, fmt.Errorf("allocate client close "+
			"delivery: %w", err)
	}
	request, _, _, err := p.peer.BeginCooperativeClose(
		ctx, id, clientScript,
	)
	if err != nil {
		return arkchannel.Record{}, err
	}
	if !bytes.Equal(request.ClientDeliveryScript, clientScript) {
		return arkchannel.Record{}, fmt.Errorf("hub changed client " +
			"cooperative close terms")
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
// lnd state, and completes the two-database hub-authorization barrier.
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
			ctx, id, request.ClientDeliveryScript,
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
		completed, err := p.peer.CompleteCooperativeClose(
			ctx, id, proposal,
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

	if _, err := service.RecordLocalEvent(
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
	if _, err := service.RecordPeerEvent(
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
// stored the hub authorization but before ordinary OOR settlement completed.
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

// FinalizeCooperativeClose resumes local archival after OOR finalization was
// already recorded by the durable channel FSM.
func (p *ClientCooperativeCloseProcess) FinalizeCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	backing arkchannel.Backing, source arkchannel.VTXOBinding,
	request arkchannel.CooperativeCloseRequest,
	settlement arkchannel.CooperativeClose) error {

	return p.finalizePublishedClose(
		ctx, id, terms, backing, source, request, settlement,
	)
}

// publishAndFinalize starts the ordinary OOR actor only after both durable
// authorization barriers exist, then asks the hub to archive its copy.
func (p *ClientCooperativeCloseProcess) publishAndFinalize(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms, backing arkchannel.Backing,
	source arkchannel.VTXOBinding,
	request arkchannel.CooperativeCloseRequest,
	settlement arkchannel.CooperativeClose) error {

	state, err := p.local.QuiesceCooperativeClose(
		ctx, id, terms, source, backing, request,
	)
	if err != nil {
		return fmt.Errorf("revalidate cooperative close state: %w", err)
	}
	if err := validateCooperativeCloseState(
		arkchannel.PartyClient, state, settlement.Proposal,
	); err != nil {
		return fmt.Errorf("revalidate signed cooperative close: %w",
			err)
	}

	if err := p.publisher.SettleCooperativeClose(
		ctx, id, terms, source, request, settlement,
	); err != nil {
		return err
	}
	service, err := p.stateService()
	if err != nil {
		return err
	}
	if _, err := service.RecordLocalEvent(
		ctx, id, &arkchannel.CooperativeClosePublished{
			TxID: settlement.TxID,
		},
	); err != nil {
		return err
	}

	return p.finalizePublishedClose(
		ctx, id, terms, backing, source, request, settlement,
	)
}

// finalizePublishedClose makes the hub publication barrier replayable before
// either endpoint archives its lnd channel.
func (p *ClientCooperativeCloseProcess) finalizePublishedClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	backing arkchannel.Backing, source arkchannel.VTXOBinding,
	request arkchannel.CooperativeCloseRequest,
	settlement arkchannel.CooperativeClose) error {

	service, err := p.stateService()
	if err != nil {
		return err
	}
	txID, err := p.peer.PublishCooperativeClose(
		ctx, id, settlement.TxID,
	)
	if err != nil {
		return err
	}
	if txID != settlement.TxID {
		return fmt.Errorf("hub observed another cooperative close")
	}
	if _, err := service.RecordPeerEvent(
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
	if _, err := service.RecordLocalEvent(
		ctx, id, &arkchannel.CooperativeCloseFinalized{
			Party: arkchannel.PartyClient,
		},
	); err != nil {
		return err
	}

	return p.peer.AcknowledgeCooperativeCloseFinalized(ctx, id)
}

// abort restores both quiesced links before ordinary OOR settlement starts.
func (p *ClientCooperativeCloseProcess) abort(ctx context.Context,
	id arkchannel.ID, backing arkchannel.Backing, cause error) error {

	remoteErr := p.peer.AbortCooperativeClose(ctx, id)
	p.local.ResumeCooperativeClose(backing)
	service, serviceErr := p.stateService()
	if serviceErr == nil {
		_, serviceErr = service.RecordLocalEvent(
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

// processCooperativeCloseRequestsEqual compares every negotiated close term.
func processCooperativeCloseRequestsEqual(
	a, b arkchannel.CooperativeCloseRequest) bool {

	return a.Initiator == b.Initiator &&
		bytes.Equal(a.ClientDeliveryScript, b.ClientDeliveryScript) &&
		bytes.Equal(a.HubDeliveryScript, b.HubDeliveryScript)
}
