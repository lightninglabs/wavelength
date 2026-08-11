package lnruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// CooperativeCloseStateSink is the durable barrier surface needed to store an
// irreversible artifact at both endpoints before either executes the action it
// implies.
type CooperativeCloseStateSink interface {
	arkchannel.ChannelEventSink

	RecordChannelEvent(context.Context, arkchannel.ID,
		arkchannel.Event) (arkchannel.Record, error)

	ResumeChannelAction(context.Context,
		arkchannel.ID) (arkchannel.Record, error)

	GetChannel(context.Context, arkchannel.ID) (arkchannel.Record, error)
}

// CooperativeCloseCounterparty is the authenticated application transport
// boundary for direct channel-policy VTXO settlement.
type CooperativeCloseCounterparty interface {
	QuiesceCooperativeClose(context.Context, arkchannel.ID,
		arkchannel.Terms, arkchannel.VTXOBinding, arkchannel.Backing,
		arkchannel.CooperativeCloseRequest) (CleanChannelState, error)

	ResumeCooperativeClose(arkchannel.Backing)

	RecordChannelEvent(context.Context, arkchannel.ID,
		arkchannel.Event) (arkchannel.Record, error)

	ResumeChannelAction(context.Context,
		arkchannel.ID) (arkchannel.Record, error)

	GetChannel(context.Context, arkchannel.ID) (arkchannel.Record, error)
}

// CooperativeCloseHub is the server-side boundary that accepts the client's
// binding signature, adds the hub and Ark-operator signatures, and durably
// stores the complete settlement before returning it.
type CooperativeCloseHub interface {
	CooperativeCloseCounterparty

	CompleteCooperativeClose(context.Context, arkchannel.ID,
		arkchannel.Terms, arkchannel.VTXOBinding, arkchannel.Backing,
		arkchannel.CooperativeCloseRequest,
		arkchannel.CooperativeCloseProposal,
		input.Signature) (arkchannel.CooperativeClose, error)
}

// CooperativeCloseOperatorSigner supplies the Ark operator's signature after
// independently validating the exact direct settlement proposal.
type CooperativeCloseOperatorSigner interface {
	SignOperatorCooperativeClose(context.Context, arkchannel.ID,
		arkchannel.Terms, arkchannel.VTXOBinding,
		arkchannel.CooperativeCloseRequest,
		arkchannel.CooperativeCloseProposal) (input.Signature, error)
}

// CooperativeClosePublisher materializes Ark ancestry and confirms the exact
// direct VTXO settlement through the common unroller.
type CooperativeClosePublisher interface {
	SettleCooperativeClose(context.Context, arkchannel.ID,
		arkchannel.VTXOBinding, arkchannel.CooperativeClose) error
}

// CooperativeCloseDeliveryValidator proves one endpoint owns the payout script
// assigned to its role before it signs or disables channel traffic.
type CooperativeCloseDeliveryValidator interface {
	ValidateCooperativeCloseDelivery(context.Context, []byte) error
}

// CooperativeCloseDeliveryValidatorFunc adapts a wallet ownership check to the
// cooperative-close endpoint.
type CooperativeCloseDeliveryValidatorFunc func(context.Context, []byte) error

// ValidateCooperativeCloseDelivery invokes the wrapped ownership check.
func (f CooperativeCloseDeliveryValidatorFunc) ValidateCooperativeCloseDelivery(
	ctx context.Context, script []byte) error {

	return f(ctx, script)
}

// NativeCooperativeCloseEndpoint owns one endpoint's lnd database and Ark
// policy signing key.
type NativeCooperativeCloseEndpoint struct {
	party    arkchannel.Party
	runtime  *Runtime
	signer   input.Signer
	keyDesc  keychain.KeyDescriptor
	delivery CooperativeCloseDeliveryValidator

	mu   sync.RWMutex
	sink CooperativeCloseStateSink
}

// NewNativeCooperativeCloseEndpoint constructs a role-bound close endpoint.
func NewNativeCooperativeCloseEndpoint(party arkchannel.Party, runtime *Runtime,
	signer input.Signer, keyDesc keychain.KeyDescriptor,
	delivery CooperativeCloseDeliveryValidator) (
	*NativeCooperativeCloseEndpoint, error) {

	if party != arkchannel.PartyClient && party != arkchannel.PartyHub {
		return nil, fmt.Errorf("cooperative close party is required")
	}
	if runtime == nil {
		return nil, fmt.Errorf("native lnd runtime is required")
	}
	if signer == nil {
		return nil, fmt.Errorf("cooperative close signer is required")
	}
	if keyDesc.PubKey == nil {
		return nil, fmt.Errorf("cooperative close Ark key is required")
	}
	if delivery == nil {
		return nil, fmt.Errorf("cooperative close delivery validator " +
			"is required")
	}

	return &NativeCooperativeCloseEndpoint{
		party:    party,
		runtime:  runtime,
		signer:   signer,
		keyDesc:  keyDesc,
		delivery: delivery,
	}, nil
}

// BindChannelEventSink attaches the endpoint to its local durable channel
// service.
func (e *NativeCooperativeCloseEndpoint) BindChannelEventSink(
	sink arkchannel.ChannelEventSink) error {

	barrier, ok := sink.(CooperativeCloseStateSink)
	if !ok {
		return fmt.Errorf("channel event sink lacks close barriers")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sink != nil {
		return nil
	}
	e.sink = barrier

	return nil
}

// QuiesceCooperativeClose returns this endpoint's authoritative clean lnd
// state after validating the channel identity and funding role.
func (e *NativeCooperativeCloseEndpoint) QuiesceCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding, backing arkchannel.Backing,
	request arkchannel.CooperativeCloseRequest) (CleanChannelState, error) {

	if err := validateCooperativeCloseChannel(
		id, terms, source, backing,
	); err != nil {
		return CleanChannelState{}, err
	}
	record, err := e.GetChannel(ctx, id)
	if err != nil {
		return CleanChannelState{}, err
	}
	if err := validateLocalCooperativeClose(
		record, terms, source, backing, request,
	); err != nil {
		return CleanChannelState{}, err
	}
	deliveryScript := request.ClientDeliveryScript
	if e.party == arkchannel.PartyHub {
		deliveryScript = request.HubDeliveryScript
	}
	if err := e.delivery.ValidateCooperativeCloseDelivery(
		ctx, deliveryScript,
	); err != nil {
		return CleanChannelState{}, fmt.Errorf("validate %s "+
			"cooperative close payout: %w", e.party, err)
	}
	state, err := e.runtime.QuiesceChannel(ctx, backing.ChannelPoint)
	if err != nil {
		return CleanChannelState{}, err
	}
	if err := validateCleanChannelState(
		e.party, terms, backing, state,
	); err != nil {

		e.runtime.ResumeChannel(backing.ChannelPoint)

		return CleanChannelState{}, err
	}

	return state, nil
}

// SignCooperativeClose re-reads the clean lnd state, reconstructs the canonical
// proposal, and signs only this endpoint's immediate Ark policy role.
func (e *NativeCooperativeCloseEndpoint) SignCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding, backing arkchannel.Backing,
	request arkchannel.CooperativeCloseRequest,
	proposal arkchannel.CooperativeCloseProposal) (input.Signature, error) {

	state, err := e.QuiesceCooperativeClose(
		ctx, id, terms, source, backing, request,
	)
	if err != nil {
		return nil, err
	}
	clientBalance, hubBalance := mapCleanBalances(e.party, state)
	if proposal.CommitmentHeight != state.CommitmentHeight ||
		proposal.ClientBalance != clientBalance ||
		proposal.HubBalance != hubBalance {
		return nil, fmt.Errorf("cooperative close proposal does not " +
			"match local clean lnd state")
	}
	template, err := arkchannel.NewCooperativeCloseTemplate(
		terms, source, request, clientBalance, hubBalance,
		state.CommitmentHeight,
	)
	if err != nil {
		return nil, err
	}
	if err := proposal.Validate(terms, source, request); err != nil {
		return nil, err
	}
	desc, err := template.SignDescriptor(terms, e.party, e.keyDesc)
	if err != nil {
		return nil, err
	}
	tx, err := decodeCloseProposal(proposal)
	if err != nil {
		return nil, err
	}

	return e.signer.SignOutputRaw(tx, desc)
}

// ResumeCooperativeClose re-enables traffic after a close aborts before a
// fully signed direct settlement is durable.
func (e *NativeCooperativeCloseEndpoint) ResumeCooperativeClose(
	backing arkchannel.Backing) {

	e.runtime.ResumeChannel(backing.ChannelPoint)
}

// RecordChannelEvent persists one barrier fact through the bound service.
func (e *NativeCooperativeCloseEndpoint) RecordChannelEvent(ctx context.Context,
	id arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	sink, err := e.stateSink()
	if err != nil {
		return arkchannel.Record{}, err
	}

	return sink.RecordChannelEvent(ctx, id, event)
}

// ResumeChannelAction executes this endpoint's already durable close action.
func (e *NativeCooperativeCloseEndpoint) ResumeChannelAction(
	ctx context.Context, id arkchannel.ID) (arkchannel.Record, error) {

	sink, err := e.stateSink()
	if err != nil {
		return arkchannel.Record{}, err
	}

	return sink.ResumeChannelAction(ctx, id)
}

// GetChannel returns this endpoint's latest durable close facts.
func (e *NativeCooperativeCloseEndpoint) GetChannel(ctx context.Context,
	id arkchannel.ID) (arkchannel.Record, error) {

	sink, err := e.stateSink()
	if err != nil {
		return arkchannel.Record{}, err
	}

	return sink.GetChannel(ctx, id)
}

// finalize archives this endpoint's ordinary lnd channel state.
func (e *NativeCooperativeCloseEndpoint) finalize(terms arkchannel.Terms,
	backing arkchannel.Backing, source arkchannel.VTXOBinding,
	request arkchannel.CooperativeCloseRequest,
	settlement arkchannel.CooperativeClose) error {

	if err := settlement.Validate(terms, source, request); err != nil {
		return err
	}
	settledBalance := settlement.Proposal.ClientBalance
	if e.party == arkchannel.PartyHub {
		settledBalance = settlement.Proposal.HubBalance
	}

	return e.runtime.FinalizeExternalCooperativeClose(
		backing.ChannelPoint, settlement.TxID, settledBalance,
		request.Initiator == e.party,
	)
}

// stateSink returns the bound durable channel service.
func (e *NativeCooperativeCloseEndpoint) stateSink() (CooperativeCloseStateSink,
	error) {

	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.sink == nil {
		return nil, fmt.Errorf("cooperative close event sink is not " +
			"bound")
	}

	return e.sink, nil
}

// NativeCooperativeCloseOperatorSigner validates and signs the operator role
// without owning any lnd channel state.
type NativeCooperativeCloseOperatorSigner struct {
	signer  input.Signer
	keyDesc keychain.KeyDescriptor
}

// NewNativeCooperativeCloseOperatorSigner constructs an Ark operator signer.
func NewNativeCooperativeCloseOperatorSigner(signer input.Signer,
	keyDesc keychain.KeyDescriptor) (*NativeCooperativeCloseOperatorSigner,
	error) {

	if signer == nil {
		return nil, fmt.Errorf("Ark operator close signer is required")
	}
	if keyDesc.PubKey == nil {
		return nil, fmt.Errorf("Ark operator close key is required")
	}

	return &NativeCooperativeCloseOperatorSigner{
		signer:  signer,
		keyDesc: keyDesc,
	}, nil
}

// SignOperatorCooperativeClose validates the canonical proposal and signs the
// Ark operator's immediate policy role.
func (s *NativeCooperativeCloseOperatorSigner) SignOperatorCooperativeClose(
	_ context.Context, id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding,
	request arkchannel.CooperativeCloseRequest,
	proposal arkchannel.CooperativeCloseProposal) (input.Signature, error) {

	if id != terms.ID {
		return nil, fmt.Errorf("cooperative close channel ID does " +
			"not match")
	}
	if err := proposal.Validate(terms, source, request); err != nil {
		return nil, err
	}
	template, err := arkchannel.NewCooperativeCloseTemplate(
		terms, source, request, proposal.ClientBalance,
		proposal.HubBalance, proposal.CommitmentHeight,
	)
	if err != nil {
		return nil, err
	}
	desc, err := template.OperatorSignDescriptor(terms, s.keyDesc)
	if err != nil {
		return nil, err
	}
	tx, err := decodeCloseProposal(proposal)
	if err != nil {
		return nil, err
	}

	return s.signer.SignOutputRaw(tx, desc)
}

// CooperativeCloseCoordinator drives only the cross-endpoint barriers. lnd
// remains authoritative for balances and Ark's unroller remains authoritative
// for publication.
type CooperativeCloseCoordinator struct {
	local     *NativeCooperativeCloseEndpoint
	remote    CooperativeCloseCounterparty
	operator  CooperativeCloseOperatorSigner
	publisher CooperativeClosePublisher
}

// NewCooperativeCloseCoordinator constructs the paired close coordinator.
func NewCooperativeCloseCoordinator(local *NativeCooperativeCloseEndpoint,
	remote CooperativeCloseCounterparty,
	operator CooperativeCloseOperatorSigner,
	publisher CooperativeClosePublisher) (*CooperativeCloseCoordinator,
	error) {

	if local == nil {
		return nil, fmt.Errorf("local cooperative close endpoint is " +
			"required")
	}
	if remote == nil {
		return nil, fmt.Errorf("remote cooperative close endpoint is " +
			"required")
	}
	if local.party == arkchannel.PartyHub && operator == nil {
		return nil, fmt.Errorf("Ark operator close signer is required")
	}
	if local.party == arkchannel.PartyClient {
		if _, ok := remote.(CooperativeCloseHub); !ok {
			return nil, fmt.Errorf("client requires cooperative " +
				"close hub")
		}
	}
	if publisher == nil {
		return nil, fmt.Errorf("cooperative close publisher is " +
			"required")
	}

	return &CooperativeCloseCoordinator{
		local:     local,
		remote:    remote,
		operator:  operator,
		publisher: publisher,
	}, nil
}

// BindChannelEventSink attaches the local endpoint to its channel service.
func (c *CooperativeCloseCoordinator) BindChannelEventSink(
	sink arkchannel.ChannelEventSink) error {

	return c.local.BindChannelEventSink(sink)
}

// QuiesceCooperativeClose delegates an authenticated peer request to the
// coordinator's local channel endpoint.
func (c *CooperativeCloseCoordinator) QuiesceCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding, backing arkchannel.Backing,
	request arkchannel.CooperativeCloseRequest) (CleanChannelState, error) {

	return c.local.QuiesceCooperativeClose(
		ctx, id, terms, source, backing, request,
	)
}

// ResumeCooperativeClose delegates an abort to the local channel endpoint.
func (c *CooperativeCloseCoordinator) ResumeCooperativeClose(
	backing arkchannel.Backing) {

	c.local.ResumeCooperativeClose(backing)
}

// RecordChannelEvent persists one remote barrier fact locally.
func (c *CooperativeCloseCoordinator) RecordChannelEvent(ctx context.Context,
	id arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	return c.local.RecordChannelEvent(ctx, id, event)
}

// ResumeChannelAction executes this coordinator's local durable action.
func (c *CooperativeCloseCoordinator) ResumeChannelAction(ctx context.Context,
	id arkchannel.ID) (arkchannel.Record, error) {

	return c.local.ResumeChannelAction(ctx, id)
}

// GetChannel returns this coordinator's local durable channel record.
func (c *CooperativeCloseCoordinator) GetChannel(ctx context.Context,
	id arkchannel.ID) (arkchannel.Record, error) {

	return c.local.GetChannel(ctx, id)
}

// NegotiateCooperativeClose keeps the hub quiesced when its request is first
// persisted. The client later signs first and asks the hub to durably complete
// the transaction before the two-endpoint publication barrier.
func (c *CooperativeCloseCoordinator) NegotiateCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding, backing arkchannel.Backing,
	request arkchannel.CooperativeCloseRequest) error {

	if request.Initiator != arkchannel.PartyClient {
		return fmt.Errorf("cooperative close must be client initiated")
	}
	if c.local.party == arkchannel.PartyHub {
		_, err := c.local.QuiesceCooperativeClose(
			ctx, id, terms, source, backing, request,
		)
		if err == nil {
			return nil
		}

		c.local.ResumeCooperativeClose(backing)
		_, abortErr := c.local.RecordChannelEvent(
			ctx, id, &arkchannel.CooperativeCloseAborted{},
		)

		return errors.Join(err, abortErr)
	}
	if c.local.party != arkchannel.PartyClient {
		return fmt.Errorf("unknown cooperative close party %d",
			c.local.party)
	}
	if request.Initiator != c.local.party {
		return nil
	}
	localRecord, err := c.local.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	if localRecord.Snapshot.CooperativeClose != nil {
		settlement := localRecord.Snapshot.CooperativeClose.Clone()
		if err := c.persistSignedBarrier(
			ctx, id, settlement,
		); err != nil {
			return err
		}
		_, err := c.resumeParty(ctx, id, arkchannel.PartyHub)

		return err
	}
	remoteRecord, err := c.remote.GetChannel(ctx, id)
	if err != nil {

		// A prior completion response may have been lost. Keep the link
		// quiesced until the client can prove the hub did not store a
		// fully signed transaction.
		return fmt.Errorf("read hub cooperative close: %w", err)
	}
	if remoteRecord.Snapshot.CooperativeClose != nil {
		settlement := remoteRecord.Snapshot.CooperativeClose.Clone()
		if err := settlement.Validate(
			terms, source, request,
		); err != nil {
			return fmt.Errorf("validate hub cooperative close: %w",
				err)
		}
		if err := c.persistSignedBarrier(
			ctx, id, settlement,
		); err != nil {
			return err
		}
		_, err := c.resumeParty(ctx, id, arkchannel.PartyHub)

		return err
	}
	localState, err := c.local.QuiesceCooperativeClose(
		ctx, id, terms, source, backing, request,
	)
	if err != nil {
		return c.abort(ctx, id, backing, err)
	}
	remoteState, err := c.remote.QuiesceCooperativeClose(
		ctx, id, terms, source, backing, request,
	)
	if err != nil {
		return c.abort(ctx, id, backing, err)
	}
	clientBalance, hubBalance, err := reconcileCleanChannelStates(
		c.local.party, localState, remoteState,
	)
	if err != nil {
		return c.abort(ctx, id, backing, err)
	}
	template, err := arkchannel.NewCooperativeCloseTemplate(
		terms, source, request, clientBalance, hubBalance,
		localState.CommitmentHeight,
	)
	if err != nil {
		return c.abort(ctx, id, backing, err)
	}
	proposal := template.Proposal()
	localSig, err := c.local.SignCooperativeClose(
		ctx, id, terms, source, backing, request, proposal,
	)
	if err != nil {
		return c.abort(ctx, id, backing, err)
	}
	hub, ok := c.remote.(CooperativeCloseHub)
	if !ok {
		return fmt.Errorf("client cooperative close hub is unavailable")
	}
	settlement, err := hub.CompleteCooperativeClose(
		ctx, id, terms, source, backing, request, proposal, localSig,
	)
	if err != nil {

		// The client signature may have reached the hub. Never resume
		// the channel after this call, even when the response is lost.
		return fmt.Errorf("hub complete cooperative close: %w", err)
	}

	// Once a valid three-signature transaction exists, never re-enable the
	// links or fall back to backing materialization. Publication is enabled
	// only after each endpoint stores the artifact and learns that the
	// other endpoint did the same.
	if err := c.persistSignedBarrier(ctx, id, settlement); err != nil {
		return err
	}
	_, err = c.resumeParty(ctx, id, arkchannel.PartyHub)

	return err
}

// CompleteCooperativeClose adds server-controlled signatures and stores the
// complete transaction at the hub before any signature material is returned to
// the client.
func (c *CooperativeCloseCoordinator) CompleteCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding, backing arkchannel.Backing,
	request arkchannel.CooperativeCloseRequest,
	proposal arkchannel.CooperativeCloseProposal,
	clientSig input.Signature) (arkchannel.CooperativeClose, error) {

	if c.local.party != arkchannel.PartyHub {
		return arkchannel.CooperativeClose{}, fmt.Errorf("only hub " +
			"can complete cooperative close")
	}
	record, err := c.local.GetChannel(ctx, id)
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	if err := validateLocalCooperativeClose(
		record, terms, source, backing, request,
	); err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	if record.Snapshot.CooperativeClose != nil {
		settlement := record.Snapshot.CooperativeClose.Clone()
		if !cooperativeCloseProposalsEqual(
			settlement.Proposal, proposal,
		) {
			return arkchannel.CooperativeClose{}, fmt.Errorf(
				"hub stored another cooperative close proposal")
		}
		if err := settlement.Validate(
			terms, source, request,
		); err != nil {
			return arkchannel.CooperativeClose{}, err
		}

		return settlement, nil
	}
	if err := proposal.Validate(terms, source, request); err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	template, err := arkchannel.NewCooperativeCloseTemplate(
		terms, source, request, proposal.ClientBalance,
		proposal.HubBalance, proposal.CommitmentHeight,
	)
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	if err := template.VerifySignature(
		terms, arkchannel.PartyClient, clientSig,
	); err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	hubSig, err := c.local.SignCooperativeClose(
		ctx, id, terms, source, backing, request, proposal,
	)
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	if err := template.VerifySignature(
		terms, arkchannel.PartyHub, hubSig,
	); err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	operatorSig, err := c.operator.SignOperatorCooperativeClose(
		ctx, id, terms, source, request, proposal,
	)
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	settlement, err := template.Complete(
		terms, source, request, clientSig, hubSig, operatorSig,
	)
	if err != nil {
		return arkchannel.CooperativeClose{}, fmt.Errorf("complete "+
			"cooperative close: %w", err)
	}
	_, err = c.local.RecordChannelEvent(
		ctx, id, &arkchannel.CooperativeCloseSigned{
			Close: settlement,
			Party: arkchannel.PartyHub,
		},
	)
	if err != nil {
		return arkchannel.CooperativeClose{}, fmt.Errorf("store "+
			"complete cooperative close at hub: %w", err)
	}

	return settlement, nil
}

// persistSignedBarrier durably stages the settlement at both endpoints before
// either one receives both acknowledgements. The non-initiator is completed
// first, leaving the initiator in a replayable negotiating phase if a crash
// interrupts the distributed barrier.
func (c *CooperativeCloseCoordinator) persistSignedBarrier(ctx context.Context,
	id arkchannel.ID, settlement arkchannel.CooperativeClose) error {

	remoteParty := otherChannelParty(c.local.party)
	steps := []struct {
		name   string
		party  arkchannel.Party
		record func(context.Context, arkchannel.ID, arkchannel.Event) (
			arkchannel.Record, error)
	}{
		{
			name:   "stage remote",
			party:  remoteParty,
			record: c.remote.RecordChannelEvent,
		},
		{
			name:   "stage local",
			party:  c.local.party,
			record: c.local.RecordChannelEvent,
		},
		{
			name:   "acknowledge local at remote",
			party:  c.local.party,
			record: c.remote.RecordChannelEvent,
		},
		{
			name:   "acknowledge remote at local",
			party:  remoteParty,
			record: c.local.RecordChannelEvent,
		},
	}
	for _, step := range steps {
		_, err := step.record(
			ctx, id, &arkchannel.CooperativeCloseSigned{
				Close: settlement,
				Party: step.party,
			},
		)
		if err != nil {
			return fmt.Errorf("%s cooperative close: %w", step.name,
				err)
		}
	}

	return nil
}

// PublishCooperativeClose runs only at the hub after the client, hub, and Ark
// operator signatures are durable. Confirmation is stored at both endpoints
// before either archives lnd.
func (c *CooperativeCloseCoordinator) PublishCooperativeClose(
	ctx context.Context, id arkchannel.ID, _ arkchannel.Terms,
	source arkchannel.VTXOBinding,
	settlement arkchannel.CooperativeClose) error {

	if c.local.party != arkchannel.PartyHub {
		return nil
	}
	if err := c.publisher.SettleCooperativeClose(
		ctx, id, source, settlement,
	); err != nil {
		return err
	}
	if err := c.recordBoth(
		ctx, id, &arkchannel.CooperativeClosePublished{
			TxID: settlement.TxID,
		},
	); err != nil {
		return err
	}
	if _, err := c.resumeParty(
		ctx, id, otherChannelParty(c.local.party),
	); err != nil {
		return err
	}
	_, err := c.resumeParty(ctx, id, c.local.party)

	return err
}

// FinalizeCooperativeClose archives this endpoint's lnd channel and records the
// acknowledgement in both durable Ark FSMs.
func (c *CooperativeCloseCoordinator) FinalizeCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	backing arkchannel.Backing, source arkchannel.VTXOBinding,
	request arkchannel.CooperativeCloseRequest,
	settlement arkchannel.CooperativeClose) error {

	if err := c.local.finalize(
		terms, backing, source, request, settlement,
	); err != nil {
		return err
	}

	return c.recordBoth(ctx, id, &arkchannel.CooperativeCloseFinalized{
		Party: c.local.party,
	})
}

// abort restores both links and returns both FSMs to active before signatures
// cross the cooperative-close safety boundary.
func (c *CooperativeCloseCoordinator) abort(ctx context.Context,
	id arkchannel.ID, backing arkchannel.Backing, cause error) error {

	c.local.ResumeCooperativeClose(backing)
	c.remote.ResumeCooperativeClose(backing)
	recordErr := c.recordBoth(
		ctx, id, &arkchannel.CooperativeCloseAborted{},
	)

	return errors.Join(cause, recordErr)
}

// recordBoth stores one fact locally and remotely without executing actions.
func (c *CooperativeCloseCoordinator) recordBoth(ctx context.Context,
	id arkchannel.ID, event arkchannel.Event) error {

	if _, err := c.remote.RecordChannelEvent(ctx, id, event); err != nil {
		return fmt.Errorf("record remote cooperative close: %w", err)
	}
	if _, err := c.local.RecordChannelEvent(ctx, id, event); err != nil {
		return fmt.Errorf("record local cooperative close: %w", err)
	}

	return nil
}

// resumeParty executes pending work at the endpoint owning one channel role.
func (c *CooperativeCloseCoordinator) resumeParty(ctx context.Context,
	id arkchannel.ID, party arkchannel.Party) (arkchannel.Record, error) {

	if party == c.local.party {
		return c.local.ResumeChannelAction(ctx, id)
	}

	return c.remote.ResumeChannelAction(ctx, id)
}

// reconcileCleanChannelStates maps relative lnd balances to stable Ark roles
// and requires both endpoint databases to describe the same commitment.
func reconcileCleanChannelStates(localParty arkchannel.Party, local,
	remote CleanChannelState) (btcutil.Amount, btcutil.Amount, error) {

	if local.ChannelPoint != remote.ChannelPoint ||
		local.Capacity != remote.Capacity ||
		local.CommitmentHeight != remote.CommitmentHeight ||
		local.LocalInitiator == remote.LocalInitiator {
		return 0, 0, fmt.Errorf("lnd endpoints disagree on clean " +
			"channel state")
	}
	clientLocal, hubLocal := mapCleanBalances(localParty, local)
	clientRemote, hubRemote := mapCleanBalances(
		otherChannelParty(localParty), remote,
	)
	if clientLocal != clientRemote || hubLocal != hubRemote {
		return 0, 0, fmt.Errorf("lnd endpoints disagree on " +
			"cooperative balances")
	}

	return clientLocal, hubLocal, nil
}

// validateCleanChannelState binds one relative lnd view to Ark channel terms.
func validateCleanChannelState(party arkchannel.Party, terms arkchannel.Terms,
	backing arkchannel.Backing, state CleanChannelState) error {

	if state.ChannelPoint != backing.ChannelPoint ||
		state.Capacity != terms.Capacity {
		return fmt.Errorf("clean lnd state does not match Ark channel")
	}
	if state.LocalInitiator != (terms.Funder == party) {
		return fmt.Errorf("clean lnd state has unexpected channel " +
			"funder")
	}
	if state.LocalBalance < 0 || state.RemoteBalance < 0 ||
		state.LocalBalance+state.RemoteBalance != terms.Capacity {
		return fmt.Errorf("clean lnd balances do not match capacity")
	}

	return nil
}

// mapCleanBalances converts a runtime-relative view into stable Ark roles.
func mapCleanBalances(party arkchannel.Party,
	state CleanChannelState) (btcutil.Amount, btcutil.Amount) {

	if party == arkchannel.PartyClient {
		return state.LocalBalance, state.RemoteBalance
	}

	return state.RemoteBalance, state.LocalBalance
}

// otherChannelParty returns the only counterparty role.
func otherChannelParty(party arkchannel.Party) arkchannel.Party {
	if party == arkchannel.PartyClient {
		return arkchannel.PartyHub
	}

	return arkchannel.PartyClient
}

// validateCooperativeCloseChannel checks immutable identities before lnd state
// changes.
func validateCooperativeCloseChannel(id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding, backing arkchannel.Backing) error {

	if id != terms.ID {
		return fmt.Errorf("cooperative close channel ID does not match")
	}
	if err := terms.Validate(); err != nil {
		return err
	}
	if err := source.Validate(terms); err != nil {
		return err
	}
	if err := backing.Validate(terms, source); err != nil {
		return err
	}

	return nil
}

// validateLocalCooperativeClose proves this signer already persisted the exact
// cross-endpoint request and channel artifacts before traffic is disabled.
func validateLocalCooperativeClose(record arkchannel.Record,
	terms arkchannel.Terms, source arkchannel.VTXOBinding,
	backing arkchannel.Backing,
	request arkchannel.CooperativeCloseRequest) error {

	snapshot := record.Snapshot
	if snapshot.Terms != terms || snapshot.Source == nil ||
		snapshot.Backing == nil {
		return fmt.Errorf("local cooperative close channel facts do " +
			"not match")
	}
	localSource := snapshot.Source
	if localSource.OORSessionID != source.OORSessionID ||
		localSource.OutPoint != source.OutPoint ||
		localSource.Amount != source.Amount ||
		!bytes.Equal(
			localSource.ArkTransaction, source.ArkTransaction,
		) || !bytes.Equal(
		localSource.PolicyTemplate, source.PolicyTemplate,
	) ||
		!bytes.Equal(localSource.PkScript, source.PkScript) {
		return fmt.Errorf("local cooperative close VTXO does not match")
	}
	localBacking := snapshot.Backing
	if localBacking.ChannelPoint != backing.ChannelPoint ||
		!bytes.Equal(localBacking.Transaction, backing.Transaction) {
		return fmt.Errorf("local cooperative close backing does not " +
			"match")
	}
	localRequest := snapshot.CooperativeCloseRequest
	if localRequest == nil || localRequest.Initiator != request.Initiator ||
		localRequest.FeeRate != request.FeeRate ||
		!bytes.Equal(
			localRequest.ClientDeliveryScript,
			request.ClientDeliveryScript,
		) || !bytes.Equal(
		localRequest.HubDeliveryScript,
		request.HubDeliveryScript,
	) {
		return fmt.Errorf("local cooperative close request does not " +
			"match")
	}
	if snapshot.Phase != arkchannel.PhaseCoopClosing {
		return fmt.Errorf("local channel cannot sign close from %s",
			snapshot.Phase)
	}

	return nil
}

// decodeCloseProposal decodes the already validated unsigned transaction.
func decodeCloseProposal(proposal arkchannel.CooperativeCloseProposal) (
	*wire.MsgTx, error) {

	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(
		bytes.NewReader(proposal.Transaction),
	); err != nil {
		return nil, fmt.Errorf("decode cooperative close proposal: %w",
			err)
	}

	return tx, nil
}

// cooperativeCloseProposalsEqual compares every field that identifies one
// exact unsigned direct settlement.
func cooperativeCloseProposalsEqual(
	a, b arkchannel.CooperativeCloseProposal) bool {

	return a.CommitmentHeight == b.CommitmentHeight &&
		a.ClientBalance == b.ClientBalance &&
		a.HubBalance == b.HubBalance &&
		a.ClientOutput == b.ClientOutput &&
		a.HubOutput == b.HubOutput &&
		a.Fee == b.Fee && bytes.Equal(a.Transaction, b.Transaction)
}

var _ arkchannel.ChannelCooperativeCloser = (*CooperativeCloseCoordinator)(nil)

var _ arkchannel.ChannelEventSinkBinder = (*CooperativeCloseCoordinator)(nil)

var _ CooperativeCloseHub = (*CooperativeCloseCoordinator)(nil)

var _ CooperativeCloseCounterparty = (*NativeCooperativeCloseEndpoint)(nil)
