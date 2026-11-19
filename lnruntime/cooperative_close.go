package lnruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
)

// unsignedCloseDrainTimeout bounds preparation during daemon-owned replay as
// well as RPCs without a deadline. It is not a lease on a signed payout.
const unsignedCloseDrainTimeout = time.Minute

// CooperativeCloseStateSink is the durable barrier surface needed to store an
// irreversible artifact at both endpoints before either executes the action it
// implies.
type CooperativeCloseStateSink interface {
	arkchannel.ChannelEventSink

	ApplyPeerEvent(context.Context, arkchannel.ID,
		arkchannel.Event) (arkchannel.Record, error)

	RequestCooperativeClose(context.Context, arkchannel.ID,
		arkchannel.CooperativeCloseRequest) (arkchannel.Record, error)

	RecordLocalEvent(context.Context, arkchannel.ID,
		arkchannel.Event) (arkchannel.Record, error)

	RecordPeerEvent(context.Context, arkchannel.ID,
		arkchannel.Event) (arkchannel.Record, error)

	ResumeChannelAction(context.Context,
		arkchannel.ID) (arkchannel.Record, error)

	GetChannel(context.Context, arkchannel.ID) (arkchannel.Record, error)
}

// CooperativeClosePublisher executes the exact 3-of-3 OOR transfer and waits
// until the ordinary OOR actor durably finalizes it.
type CooperativeClosePublisher interface {
	SettleCooperativeClose(context.Context, arkchannel.ID, arkchannel.Terms,
		arkchannel.VTXOBinding, arkchannel.CooperativeCloseRequest,
		arkchannel.CooperativeClose) error
}

// CooperativeClosePublisherFunc adapts a process-owned OOR settlement function
// to the cooperative close protocol.
type CooperativeClosePublisherFunc func(context.Context, arkchannel.ID,
	arkchannel.Terms, arkchannel.VTXOBinding,
	arkchannel.CooperativeCloseRequest, arkchannel.CooperativeClose) error

// SettleCooperativeClose invokes the wrapped OOR settlement function.
func (f CooperativeClosePublisherFunc) SettleCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding,
	request arkchannel.CooperativeCloseRequest,
	settlement arkchannel.CooperativeClose) error {

	return f(ctx, id, terms, source, request, settlement)
}

// CooperativeCloseObserver verifies that the hub's ordinary Ark account has
// durably accepted the expected incoming OOR transfer before channel archival.
type CooperativeCloseObserver interface {
	HasCooperativeClose(context.Context, wire.OutPoint) (bool, error)
}

// CooperativeCloseObserverFunc adapts a daemon session lookup to the close
// process.
type CooperativeCloseObserverFunc func(context.Context, wire.OutPoint) (bool,
	error)

// HasCooperativeClose invokes the wrapped session observer.
func (f CooperativeCloseObserverFunc) HasCooperativeClose(ctx context.Context,
	outpoint wire.OutPoint) (bool, error) {

	return f(ctx, outpoint)
}

// CooperativeCloseDefender asks the ordinary Ark wallet to unroll one exact
// replacement VTXO when the closed channel's former source ancestry appears
// on chain.
type CooperativeCloseDefender interface {
	DefendCooperativeClose(context.Context, wire.OutPoint) error
}

// CooperativeCloseDefenderFunc adapts an ordinary wallet unroll request to
// the cooperative-close process.
type CooperativeCloseDefenderFunc func(context.Context, wire.OutPoint) error

// DefendCooperativeClose invokes the wrapped wallet recovery function.
func (f CooperativeCloseDefenderFunc) DefendCooperativeClose(
	ctx context.Context, outpoint wire.OutPoint) error {

	return f(ctx, outpoint)
}

// CooperativeCloseDeliveryValidator proves one endpoint owns the replacement
// VTXO key assigned to its role before the hub signs or traffic is disabled.
type CooperativeCloseDeliveryValidator interface {
	ValidateCooperativeCloseDelivery(context.Context, arkchannel.ID,
		[]byte) error
}

// CooperativeCloseDeliveryValidatorFunc adapts a wallet ownership check to the
// cooperative-close endpoint.
type CooperativeCloseDeliveryValidatorFunc func(context.Context,
	arkchannel.ID, []byte) error

// ValidateCooperativeCloseDelivery invokes the wrapped ownership check.
func (f CooperativeCloseDeliveryValidatorFunc) ValidateCooperativeCloseDelivery(
	ctx context.Context, id arkchannel.ID, script []byte) error {

	return f(ctx, id, script)
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
	if party == arkchannel.PartyHub && signer == nil {
		return nil, fmt.Errorf("hub cooperative close signer is " +
			"required")
	}
	if party == arkchannel.PartyHub && keyDesc.PubKey == nil {
		return nil, fmt.Errorf("hub cooperative close Ark key is " +
			"required")
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

// ValidateCooperativeCloseOutputs applies process-owned payout admission before
// native signing. Existing signed artifacts remain replayable independently of
// a later operator policy change.
func (e *NativeCooperativeCloseEndpoint) ValidateCooperativeCloseOutputs(
	proposal arkchannel.CooperativeCloseProposal) error {

	validator, ok := e.delivery.(interface {
		ValidateCooperativeCloseOutputs(
			arkchannel.CooperativeCloseProposal) error
	})
	if !ok {
		return nil
	}

	return validator.ValidateCooperativeCloseOutputs(proposal)
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
	if record.Snapshot.CooperativeClose == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(
			ctx, unsignedCloseDrainTimeout,
		)
		defer cancel()
	}
	if e.party == arkchannel.PartyHub {
		deliveryScript = request.HubDeliveryScript
	}
	if err := e.delivery.ValidateCooperativeCloseDelivery(
		ctx, id, deliveryScript,
	); err != nil {
		return CleanChannelState{}, fmt.Errorf("validate %s "+
			"cooperative close payout: %w", e.party, err)
	}
	state, err := e.runtime.quiesceChannel(ctx, backing.ChannelPoint, true)
	if err != nil {
		return CleanChannelState{}, err
	}
	if err := validateCleanChannelState(
		e.party, terms, backing, state,
	); err != nil {
		return CleanChannelState{}, err
	}

	return state, nil
}

// SignHubCooperativeClose re-reads the clean lnd state, reconstructs the
// canonical proposal, and signs only the hub's immediate Ark policy role.
func (e *NativeCooperativeCloseEndpoint) SignHubCooperativeClose(
	ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding, backing arkchannel.Backing,
	request arkchannel.CooperativeCloseRequest,
	proposal arkchannel.CooperativeCloseProposal) (input.Signature, error) {

	if e.party != arkchannel.PartyHub {
		return nil, fmt.Errorf("only hub can authorize cooperative " +
			"close")
	}

	state, err := e.QuiesceCooperativeClose(
		ctx, id, terms, source, backing, request,
	)
	if err != nil {
		return nil, err
	}
	clientBalance, hubBalance := mapCleanBalances(e.party, state)
	if err := validateCooperativeCloseState(
		e.party, state, proposal,
	); err != nil {
		return nil, err
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

// resumeAbortedClose releases only the external hold after an unsigned abort.
// Serialize the durable reread with installation and quiescence so a delayed
// abort cannot reopen a newer close or a replacement link.
func (e *NativeCooperativeCloseEndpoint) resumeAbortedClose(ctx context.Context,
	id arkchannel.ID, backing arkchannel.Backing) error {

	r := e.runtime
	r.linkMu.Lock()
	defer r.linkMu.Unlock()

	record, err := e.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	s := record.Snapshot
	if s.Phase != arkchannel.PhaseActive || s.CooperativeClose != nil ||
		s.CooperativeCloseRequest != nil || s.Backing == nil ||
		s.Backing.ChannelPoint != backing.ChannelPoint {
		return fmt.Errorf("channel no longer has an unsigned abort")
	}
	link, err := r.switcher.GetLink(
		lnwire.NewChanIDFromOutPoint(backing.ChannelPoint),
	)
	if errors.Is(err, htlcswitch.ErrChannelLinkNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	_, err = setExternalCloseHold(link, false)

	return err
}

// RecordLocalEvent persists one locally owned barrier through the bound
// service.
func (e *NativeCooperativeCloseEndpoint) RecordLocalEvent(ctx context.Context,
	id arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	sink, err := e.stateSink()
	if err != nil {
		return arkchannel.Record{}, err
	}

	return sink.RecordLocalEvent(ctx, id, event)
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

// completeCooperativeClose verifies the proposal against the hub's clean lnd
// state, signs both the source checkpoint and final payout, and stores that
// exact authorization before returning it. The client and Ark operator add
// their signatures through the ordinary OOR protocol.
func completeCooperativeClose(ctx context.Context,
	local *NativeCooperativeCloseEndpoint, id arkchannel.ID,
	terms arkchannel.Terms, source arkchannel.VTXOBinding,
	backing arkchannel.Backing, request arkchannel.CooperativeCloseRequest,
	proposal arkchannel.CooperativeCloseProposal) (
	arkchannel.CooperativeClose, error) {

	if local.party != arkchannel.PartyHub {
		return arkchannel.CooperativeClose{}, fmt.Errorf("only hub " +
			"can complete cooperative close")
	}
	record, err := local.GetChannel(ctx, id)
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
	if err := local.ValidateCooperativeCloseOutputs(proposal); err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	template, err := arkchannel.NewCooperativeCloseTemplate(
		terms, source, request, proposal.ClientBalance,
		proposal.HubBalance, proposal.CommitmentHeight,
	)
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	hubSig, err := local.SignHubCooperativeClose(
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
	arkDesc, err := template.ArkSignDescriptor(
		terms, arkchannel.PartyHub, local.keyDesc,
	)
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	hubArkSig, err := local.signer.SignOutputRaw(
		template.ArkTransaction(), arkDesc,
	)
	if err != nil {
		return arkchannel.CooperativeClose{}, err
	}
	settlement, err := template.Complete(
		terms, source, request, hubSig, hubArkSig,
	)
	if err != nil {
		return arkchannel.CooperativeClose{}, fmt.Errorf("complete "+
			"cooperative close: %w", err)
	}
	_, err = local.RecordLocalEvent(
		ctx, id, &arkchannel.CooperativeCloseSigned{
			Close: settlement,
		},
	)
	if err != nil {
		return arkchannel.CooperativeClose{}, fmt.Errorf("store "+
			"complete cooperative close at hub: %w", err)
	}

	return settlement, nil
}

// validateCleanChannelState binds one relative lnd view to Ark channel terms.
func validateCleanChannelState(party arkchannel.Party, terms arkchannel.Terms,
	backing arkchannel.Backing, state CleanChannelState) error {

	if state.ChannelPoint != backing.ChannelPoint ||
		state.Capacity != terms.Capacity {
		return fmt.Errorf("clean lnd state does not match Ark channel")
	}
	if state.LocalInitiator != (terms.FundingInitiator() == party) {
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

// validateCooperativeCloseState rejects a signed settlement whose role-stable
// balance split no longer matches the endpoint's clean lnd commitment.
func validateCooperativeCloseState(party arkchannel.Party,
	state CleanChannelState,
	proposal arkchannel.CooperativeCloseProposal) error {

	clientBalance, hubBalance := mapCleanBalances(party, state)
	if proposal.CommitmentHeight != state.CommitmentHeight ||
		proposal.ClientBalance != clientBalance ||
		proposal.HubBalance != hubBalance {
		return fmt.Errorf("cooperative close proposal does not match " +
			"local clean lnd state")
	}

	return nil
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
	switch snapshot.Phase {
	case arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned,
		arkchannel.PhaseCoopClosePublished:

	default:
		return fmt.Errorf("local channel cannot quiesce close from %s",
			snapshot.Phase)
	}

	return nil
}

// decodeCloseProposal decodes the already validated unsigned checkpoint PSBT.
func decodeCloseProposal(proposal arkchannel.CooperativeCloseProposal) (
	*wire.MsgTx, error) {

	packet, err := psbtutil.Parse(proposal.Transaction)
	if err != nil {
		return nil, fmt.Errorf("decode cooperative close proposal: %w",
			err)
	}

	return packet.UnsignedTx, nil
}

// cooperativeCloseProposalsEqual compares every field that identifies one
// exact unsigned OOR checkpoint authorization.
func cooperativeCloseProposalsEqual(
	a, b arkchannel.CooperativeCloseProposal) bool {

	return a.CommitmentHeight == b.CommitmentHeight &&
		a.ClientBalance == b.ClientBalance &&
		a.HubBalance == b.HubBalance &&
		a.ClientOutput == b.ClientOutput &&
		a.HubOutput == b.HubOutput &&
		bytes.Equal(a.Transaction, b.Transaction)
}
