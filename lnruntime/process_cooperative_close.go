package lnruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

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
	GetCooperativeCloseTerms(context.Context, arkchannel.ID,
		[]byte) (arkchannel.CooperativeCloseRequest, error)

	CompleteCooperativeClose(context.Context, arkchannel.ID,
		arkchannel.CooperativeCloseRequest,
		arkchannel.CooperativeCloseProposal) (
		arkchannel.CooperativeClose, error)

	ConfirmCooperativeClose(context.Context, arkchannel.ID,
		chainhash.Hash) error

	PublishCooperativeClose(context.Context, arkchannel.ID,
		chainhash.Hash) (chainhash.Hash, error)

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

	locks cooperativeCloseOperationLock
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

	return p.local.BindChannelEventSink(sink)
}

// RequestCooperativeClose quotes the payout destinations and stores them before
// either link quiesces. Repeating the request resumes the same OOR.
func (p *ClientCooperativeCloseProcess) RequestCooperativeClose(
	ctx context.Context, id arkchannel.ID) (arkchannel.Record, error) {

	unlock := p.locks.lock(id)
	defer unlock()

	service, err := p.local.stateSink()
	if err != nil {
		return arkchannel.Record{}, err
	}
	record, err := service.GetChannel(ctx, id)
	if err != nil {
		return arkchannel.Record{}, err
	}
	if record.Snapshot.Phase == arkchannel.PhaseClosed &&
		record.Snapshot.CooperativeClose != nil {
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
	request, err := p.peer.GetCooperativeCloseTerms(
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

	return p.local.GetChannel(ctx, id)
}

// NegotiateCooperativeClose quiesces both endpoints, reconciles their native
// lnd state, and completes the two-database hub-authorization barrier.
func (p *ClientCooperativeCloseProcess) NegotiateCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding, backing arkchannel.Backing,
	request arkchannel.CooperativeCloseRequest) error {

	service, err := p.local.stateSink()
	if err != nil {
		return err
	}
	localRecord, err := service.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	settlement := localRecord.Snapshot.CooperativeClose
	if settlement == nil {
		localState, err := p.local.QuiesceCooperativeClose(
			ctx, id, terms, source, backing, request,
		)
		if err != nil {
			return p.abort(ctx, id, backing, err)
		}
		clientBalance, hubBalance := mapCleanBalances(
			arkchannel.PartyClient, localState,
		)
		template, err := arkchannel.NewCooperativeCloseTemplate(
			terms, source, request, clientBalance, hubBalance,
			localState.CommitmentHeight,
		)
		if err != nil {
			return p.abort(ctx, id, backing, err)
		}
		proposal := template.Proposal()
		if err := p.local.ValidateCooperativeCloseOutputs(
			proposal,
		); err != nil {
			return p.abort(ctx, id, backing, err)
		}
		completed, err := p.peer.CompleteCooperativeClose(
			ctx, id, request, proposal,
		)
		if err != nil {

			// The hub may have stored the signed transaction before
			// its response was lost. Keep both links quiesced for
			// replay.
			return fmt.Errorf("hub complete cooperative close: %w",
				err)
		}
		if !cooperativeCloseProposalsEqual(
			completed.Proposal, proposal,
		) {
			return fmt.Errorf("hub authorized a different close " +
				"proposal")
		}
		settlement = &completed
	}
	if err := settlement.Validate(terms, source, request); err != nil {
		return fmt.Errorf("validate hub cooperative close: %w", err)
	}

	if _, err := service.RecordLocalEvent(
		ctx, id, &arkchannel.CooperativeCloseSigned{
			Close: settlement.Clone(),
		},
	); err != nil {
		return err
	}

	receipt := settlement.AuthorizationReceipt()
	if err := p.peer.ConfirmCooperativeClose(ctx, id, receipt); err != nil {
		return err
	}
	if _, err := service.RecordLocalEvent(
		ctx, id, &arkchannel.CooperativeCloseReady{
			Receipt: receipt,
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
// An archived close instead leaves replacement defense to its owning wallet.
func (p *ClientCooperativeCloseProcess) PublishCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding,
	settlement arkchannel.CooperativeClose) error {

	service, err := p.local.stateSink()
	if err != nil {
		return err
	}
	record, err := service.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	snapshot := record.Snapshot
	if snapshot.Source == nil || snapshot.Backing == nil ||
		snapshot.CooperativeCloseRequest == nil {
		return fmt.Errorf("client cooperative close artifacts are " +
			"incomplete")
	}
	if snapshot.Phase == arkchannel.PhaseClosed {
		if snapshot.CooperativeClose == nil ||
			snapshot.CooperativeClose.TxID != settlement.TxID {
			return fmt.Errorf("client archived close does not " +
				"match finalized settlement")
		}
		request := *snapshot.CooperativeCloseRequest
		if err := settlement.Validate(
			snapshot.Terms, *snapshot.Source, request,
		); err != nil {
			return fmt.Errorf("validate client archived close: %w",
				err)
		}
		if settlement.Proposal.ClientOutput == 0 {
			return nil
		}

		// The registered incoming OOR destination owns this
		// replacement. Its normal wallet fraud watches (restored from
		// live VTXOs on restart) defend ancestor spends. This shared
		// FSM action must not republish the refresh or re-quiesce the
		// archived lnd link.
		return p.local.delivery.ValidateCooperativeCloseDelivery(
			ctx, id, request.ClientDeliveryScript,
		)
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
	service, err := p.local.stateSink()
	if err != nil {
		return err
	}
	record, err := service.RecordLocalEvent(
		ctx, id, &arkchannel.CooperativeClosePublished{
			TxID: settlement.TxID,
		},
	)
	if err != nil {
		return err
	}
	if record.Snapshot.Phase != arkchannel.PhaseCoopClosePublished {
		return fmt.Errorf("cannot archive cooperative close from %s",
			record.Snapshot.Phase)
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

	txID, err := p.peer.PublishCooperativeClose(
		ctx, id, settlement.TxID,
	)
	if err != nil {
		return err
	}
	if txID != settlement.TxID {
		return fmt.Errorf("hub observed another cooperative close")
	}

	if err := p.local.finalize(
		terms, backing, source, request, settlement,
	); err != nil {
		return err
	}
	service, err := p.local.stateSink()
	if err != nil {
		return err
	}
	_, err = service.RecordLocalEvent(
		ctx, id, &arkchannel.CooperativeCloseFinalized{},
	)

	return err
}

// unsignedCloseAbortTimeout limits cleanup independently of an expired caller.
const unsignedCloseAbortTimeout = 10 * time.Second

// abort is called only before asking the hub to sign. A retry may follow a
// lost signing reply, so a fresh successful hub abort remains mandatory.
func (p *ClientCooperativeCloseProcess) abort(ctx context.Context,
	id arkchannel.ID, backing arkchannel.Backing, cause error) error {

	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), unsignedCloseAbortTimeout,
	)
	defer cancel()

	remoteErr := p.peer.AbortCooperativeClose(ctx, id)
	if remoteErr != nil {
		return errors.Join(cause, remoteErr)
	}
	service, serviceErr := p.local.stateSink()
	if serviceErr == nil {
		_, serviceErr = service.RecordLocalEvent(
			ctx, id, &arkchannel.CooperativeCloseAborted{},
		)
	}
	if serviceErr == nil {
		serviceErr = p.local.resumeAbortedClose(ctx, id, backing)
	}

	return errors.Join(cause, remoteErr, serviceErr)
}

// HubCooperativeCloseProcess owns the operator endpoint's local actions. It
// never calls the client while serving a client request.
type HubCooperativeCloseProcess struct {
	local    *NativeCooperativeCloseEndpoint
	delivery CooperativeCloseDeliverySource
	observer CooperativeCloseObserver
	defender CooperativeCloseDefender

	locks cooperativeCloseOperationLock
}

// NewHubCooperativeCloseProcess constructs the hub-owned close process.
func NewHubCooperativeCloseProcess(local *NativeCooperativeCloseEndpoint,
	delivery CooperativeCloseDeliverySource,
	observer CooperativeCloseObserver,
	defender CooperativeCloseDefender) (*HubCooperativeCloseProcess,
	error) {

	if local == nil || local.party != arkchannel.PartyHub {
		return nil, fmt.Errorf("hub cooperative close endpoint is " +
			"required")
	}
	if delivery == nil {
		return nil, fmt.Errorf("hub cooperative close delivery " +
			"source is required")
	}
	if observer == nil {
		return nil, fmt.Errorf("hub cooperative close observer is " +
			"required")
	}
	if defender == nil {
		return nil, fmt.Errorf("hub cooperative close defender is " +
			"required")
	}

	return &HubCooperativeCloseProcess{
		local: local, delivery: delivery, observer: observer,
		defender: defender,
	}, nil
}

// BindChannelEventSink attaches the process to its durable channel service.
func (p *HubCooperativeCloseProcess) BindChannelEventSink(
	sink arkchannel.ChannelEventSink) error {

	return p.local.BindChannelEventSink(sink)
}

// GetCooperativeCloseTerms quotes payout destinations without quiescing the
// hub. A lost reply leaves the channel usable and needs no remote rollback.
func (p *HubCooperativeCloseProcess) GetCooperativeCloseTerms(
	ctx context.Context, id arkchannel.ID, clientScript []byte) (
	arkchannel.CooperativeCloseRequest, error) {

	hubScript, err := p.delivery.CooperativeCloseDelivery(ctx, id)
	if err != nil {
		return arkchannel.CooperativeCloseRequest{}, err
	}
	request := arkchannel.CooperativeCloseRequest{
		Initiator:            arkchannel.PartyClient,
		ClientDeliveryScript: bytes.Clone(clientScript),
		HubDeliveryScript:    bytes.Clone(hubScript),
	}

	return request, request.Validate()
}

// CompleteCooperativeClose checks and persists the hub's exact OOR
// authorization.
func (p *HubCooperativeCloseProcess) CompleteCooperativeClose(
	ctx context.Context, id arkchannel.ID,
	request arkchannel.CooperativeCloseRequest,
	proposal arkchannel.CooperativeCloseProposal) (
	arkchannel.CooperativeClose, error) {

	unlock := p.locks.lock(id)
	defer unlock()

	service, err := p.local.stateSink()
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	record, err := service.GetChannel(ctx, id)
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	if record.Snapshot.Source == nil || record.Snapshot.Backing == nil {
		return arkchannel.CooperativeClose{}, fmt.Errorf("hub close " +
			"backing missing")
	}
	if err := p.local.delivery.ValidateCooperativeCloseDelivery(
		ctx, id, request.HubDeliveryScript,
	); err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	// Recheck the hub's registered payout policy before issuing a new
	// authorization. A signed retry must replay the original policy.
	if record.Snapshot.CooperativeClose == nil {
		registered, err := p.delivery.CooperativeCloseDelivery(ctx, id)
		if err != nil {
			return arkchannel.CooperativeClose{}, err
		}
		if !bytes.Equal(registered, request.HubDeliveryScript) {
			return arkchannel.CooperativeClose{}, fmt.Errorf(
				"hub refresh policy changed since quote")
		}
	}
	if err := proposal.Validate(
		record.Snapshot.Terms, *record.Snapshot.Source, request,
	); err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	// Persist immutable destinations before quiescing or releasing
	// signatures.
	if _, err := service.RecordPeerEvent(
		ctx, id, &arkchannel.RequestCooperativeClose{
			Request: request,
		},
	); err != nil {
		return arkchannel.CooperativeClose{}, err
	}

	return completeCooperativeClose(
		ctx, p.local, id, record.Snapshot.Terms,
		*record.Snapshot.Source, *record.Snapshot.Backing, request,
		proposal,
	)
}

// ConfirmCooperativeClose enables submission only after the client has stored
// the same authorization the hub committed before returning its signatures.
func (p *HubCooperativeCloseProcess) ConfirmCooperativeClose(
	ctx context.Context, id arkchannel.ID, receipt chainhash.Hash) error {

	unlock := p.locks.lock(id)
	defer unlock()

	service, err := p.local.stateSink()
	if err != nil {
		return err
	}
	_, err = service.RecordPeerEvent(
		ctx, id, &arkchannel.CooperativeCloseReady{
			Receipt: receipt,
		},
	)

	return err
}

// PublishCooperativeClose accepts the client-confirmed OOR session and archives
// the hub's native channel before replying.
func (p *HubCooperativeCloseProcess) PublishCooperativeClose(
	ctx context.Context, id arkchannel.ID, txID chainhash.Hash) (
	chainhash.Hash, error) {

	for {
		done, err := p.resumeSettlement(ctx, id, txID, true)
		if err != nil {
			return chainhash.Hash{}, err
		}
		if done {
			return txID, nil
		}
		select {
		case <-ctx.Done():
			return chainhash.Hash{}, ctx.Err()

		case <-time.After(100 * time.Millisecond):
		}
	}
}

// resumeSettlement performs one recovery step. A hub with a payout can prove
// settlement from its wallet alone. With no output it needs the client's
// authenticated finalization notice; absence is never proof.
func (p *HubCooperativeCloseProcess) resumeSettlement(ctx context.Context,
	id arkchannel.ID, txID chainhash.Hash, notified bool) (bool, error) {

	unlock := p.locks.lock(id)
	defer unlock()

	service, err := p.local.stateSink()
	if err != nil {
		return false, err
	}
	record, err := service.GetChannel(ctx, id)
	if err != nil {
		return false, err
	}
	if record.Snapshot.CooperativeClose == nil {
		return false, fmt.Errorf("hub has no signed cooperative close")
	}
	if record.Snapshot.CooperativeClose.TxID != txID {
		return false, fmt.Errorf("client finalized another " +
			"cooperative close")
	}
	if record.Snapshot.Phase == arkchannel.PhaseClosed {

		// A replacement may already have been spent since archival.
		return true, nil
	}
	if record.Snapshot.Phase == arkchannel.PhaseCoopCloseSigned {
		settlement := record.Snapshot.CooperativeClose
		publish := service.RecordPeerEvent
		if settlement.Proposal.HubOutput > 0 {
			outpoint, err := settlement.ReplacementOutPoint(
				record.Snapshot.Terms, *record.Snapshot.Source,
				*record.Snapshot.CooperativeCloseRequest,
				arkchannel.PartyHub,
			)
			if err != nil {
				return false, err
			}
			observed, err := p.observer.HasCooperativeClose(
				ctx, outpoint,
			)
			if err != nil || !observed {
				return false, err
			}
			publish = service.RecordLocalEvent
		} else if !notified {
			return false, nil
		}
		record, err = publish(
			ctx, id, &arkchannel.CooperativeClosePublished{
				TxID: txID,
			},
		)
		if err != nil {
			return false, err
		}
	}
	if record.Snapshot.Phase != arkchannel.PhaseCoopClosePublished {
		return false, fmt.Errorf("cannot archive cooperative "+
			"close from %s", record.Snapshot.Phase)
	}
	snapshot := record.Snapshot
	err = p.FinalizeCooperativeClose(
		ctx, id, snapshot.Terms, *snapshot.Backing, *snapshot.Source,
		*snapshot.CooperativeCloseRequest, *snapshot.CooperativeClose,
	)

	return err == nil, err
}

// AbortCooperativeClose restores the hub link before signatures exist.
func (p *HubCooperativeCloseProcess) AbortCooperativeClose(ctx context.Context,
	id arkchannel.ID) error {

	unlock := p.locks.lock(id)
	defer unlock()

	service, err := p.local.stateSink()
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
	_, err = service.RecordPeerEvent(
		ctx, id, &arkchannel.CooperativeCloseAborted{},
	)
	if err == nil {
		err = p.local.resumeAbortedClose(
			ctx, id, *record.Snapshot.Backing,
		)
	}

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

// FinalizeCooperativeClose archives the hub's lnd record and closes its local
// FSM.
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
	service, err := p.local.stateSink()
	if err != nil {
		return err
	}
	_, err = service.RecordLocalEvent(
		ctx, id, &arkchannel.CooperativeCloseFinalized{},
	)

	return err
}

// DefendCooperativeClose starts ordinary wallet recovery for the exact hub
// replacement VTXO after a closed channel's source ancestry is spent.
func (p *HubCooperativeCloseProcess) DefendCooperativeClose(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms, source arkchannel.VTXOBinding,
	settlement arkchannel.CooperativeClose) error {

	unlock := p.locks.lock(id)
	defer unlock()

	service, err := p.local.stateSink()
	if err != nil {
		return err
	}
	record, err := service.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	snapshot := record.Snapshot
	if snapshot.SourceConflict == nil {
		return nil
	}
	if snapshot.Phase != arkchannel.PhaseClosed ||
		snapshot.CooperativeCloseRequest == nil ||
		snapshot.CooperativeClose == nil {
		return fmt.Errorf("cooperative close defense state is " +
			"incomplete")
	}
	if snapshot.CooperativeClose.TxID != settlement.TxID {
		return fmt.Errorf("cooperative close defense settlement " +
			"changed")
	}
	if settlement.Proposal.HubOutput == 0 {
		return nil
	}
	outpoint, err := settlement.ReplacementOutPoint(
		terms, source, *snapshot.CooperativeCloseRequest,
		arkchannel.PartyHub,
	)
	if err != nil {
		return fmt.Errorf("derive hub cooperative close "+
			"replacement: %w", err)
	}
	if err := p.defender.DefendCooperativeClose(ctx, outpoint); err != nil {
		return fmt.Errorf("defend hub cooperative close "+
			"replacement: %w", err)
	}

	return nil
}

// HubCooperativeCloseExecutor adapts the process to NativeExecutor without
// overloading the legacy peer-facing PublishCooperativeClose method name.
type HubCooperativeCloseExecutor struct {
	*HubCooperativeCloseProcess
}

// PublishCooperativeClose defers initial OOR submission to the client. If the
// same durable action is emitted after the close because old source ancestry
// appeared, it instead protects the hub's replacement through its wallet.
func (e *HubCooperativeCloseExecutor) PublishCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding,
	settlement arkchannel.CooperativeClose) error {

	record, err := e.local.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	if record.Snapshot.Phase == arkchannel.PhaseClosed {
		return e.DefendCooperativeClose(
			ctx, id, terms, source, settlement,
		)
	}
	_, err = e.resumeSettlement(ctx, id, settlement.TxID, false)

	return err
}

var _ arkchannel.ChannelCooperativeCloser = (*ClientCooperativeCloseProcess)(nil) //nolint:ll

var _ arkchannel.ChannelCooperativeCloser = (*HubCooperativeCloseExecutor)(nil)
