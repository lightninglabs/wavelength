package lnruntime

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	lndfunding "github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// NativeFundingBackend is the narrow native lnd surface required by channel
// negotiation. It keeps the coordinator independent of lnd's internal wallet
// and funding-manager types.
type NativeFundingBackend interface {
	OpenChannel(FundingOpenRequest) (*FundingFlow, error)

	ExpectedFundingOutput(lndfunding.PendingChanID) (*wire.TxOut, error)

	RegisterBacking(VirtualFunding) error

	FinalizeBacking(lndfunding.PendingChanID, *psbt.Packet,
		VirtualFunding) error

	FundingFinalized(context.Context, arkchannel.Terms,
		arkchannel.Backing) (bool, error)

	ChannelActive(context.Context, arkchannel.Terms,
		arkchannel.Backing) (bool, error)

	CancelBacking(lndfunding.PendingChanID, *wire.OutPoint) error
}

// FundingCounterparty is the application transport boundary used for the
// small amount of Ark-specific coordination outside the ordinary BOLT stream.
type FundingCounterparty interface {
	SignBacking(context.Context, arkchannel.ID, arkchannel.Terms,
		arkchannel.VTXOBinding, *psbt.Packet) (input.Signature, error)

	InstallBacking(context.Context, arkchannel.ID, arkchannel.Terms,
		arkchannel.VTXOBinding, arkchannel.Backing) error

	FundingFinalized(context.Context, arkchannel.Terms,
		arkchannel.Backing) (bool, error)

	ChannelActive(context.Context, arkchannel.Terms,
		arkchannel.Backing) (bool, error)

	ApplyChannelEvent(context.Context, arkchannel.ID,
		arkchannel.Event) (arkchannel.Record, error)
}

// RecoveryCounterparty extends the funding transport with installation of the
// endpoint-neutral source package. Local lnd funding endpoints deliberately do
// not implement this application-level archive operation.
type RecoveryCounterparty interface {
	FundingCounterparty

	InstallRecoveryPackage(context.Context, arkchannel.ID, arkchannel.Terms,
		arkchannel.VTXOBinding, arkchannel.RecoveryPackage) error
}

// RecoveryExportCounterparty exposes the funder's finalized source package to
// the lnd opener when Ark funding is owned by the remote endpoint.
type RecoveryExportCounterparty interface {
	FundingCounterparty

	ExportRecoveryPackage(context.Context,
		arkchannel.ID) (arkchannel.RecoveryPackage, error)
}

// ChannelRecoveryManager exports a finalized funder package and installs the
// endpoint-local recovery-only descriptor, OOR artifacts, and ancestry
// watches. Installation must be idempotent for an identical package.
type ChannelRecoveryManager interface {
	ExportRecoveryPackage(context.Context, arkchannel.ID, arkchannel.Terms,
		arkchannel.VTXOBinding) (arkchannel.RecoveryPackage, error)

	InstallRecoveryPackage(context.Context, arkchannel.ID, arkchannel.Terms,
		arkchannel.VTXOBinding, arkchannel.RecoveryPackage) error
}

// NativeFundingEndpoint validates and signs one endpoint's view using its own
// lnd reservation and materialization key.
type NativeFundingEndpoint struct {
	party   arkchannel.Party
	funding NativeFundingBackend
	signer  input.Signer
	keyDesc keychain.KeyDescriptor

	mu   sync.RWMutex
	sink fundingEventSink
}

// fundingEventSink distinguishes local lnd observations from authenticated
// peer barriers before either is admitted to the durable channel FSM.
type fundingEventSink interface {
	arkchannel.ChannelEventSink

	ApplyPeerEvent(context.Context, arkchannel.ID,
		arkchannel.Event) (arkchannel.Record, error)

	RecordLocalEvent(context.Context, arkchannel.ID,
		arkchannel.Event) (arkchannel.Record, error)

	ResumeChannelAction(context.Context,
		arkchannel.ID) (arkchannel.Record, error)
}

// NewNativeFundingEndpoint constructs one local or remotely adapted endpoint.
func NewNativeFundingEndpoint(party arkchannel.Party,
	funding NativeFundingBackend, signer input.Signer,
	keyDesc keychain.KeyDescriptor) (*NativeFundingEndpoint, error) {

	if party != arkchannel.PartyClient && party != arkchannel.PartyHub {
		return nil, fmt.Errorf("channel funding party is required")
	}
	if funding == nil {
		return nil, fmt.Errorf("native funding backend is required")
	}
	if signer == nil {
		return nil, fmt.Errorf("channel backing signer is required")
	}
	if keyDesc.PubKey == nil {
		return nil, fmt.Errorf("channel backing key is required")
	}

	return &NativeFundingEndpoint{
		party:   party,
		funding: funding,
		signer:  signer,
		keyDesc: keyDesc,
	}, nil
}

// BindChannelEventSink attaches the endpoint to its durable channel service.
func (e *NativeFundingEndpoint) BindChannelEventSink(
	sink arkchannel.ChannelEventSink) error {

	if sink == nil {
		return fmt.Errorf("channel event sink is required")
	}
	fundingSink, ok := sink.(fundingEventSink)
	if !ok {
		return fmt.Errorf("channel event sink cannot record peer " +
			"events")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sink != nil {
		return nil
	}
	e.sink = fundingSink

	return nil
}

// SignBacking independently reconstructs the backing template and verifies
// that it pays this endpoint's exact native lnd reservation before signing.
func (e *NativeFundingEndpoint) SignBacking(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms, source arkchannel.VTXOBinding,
	packet *psbt.Packet) (input.Signature, error) {

	if err := validateFundingRequest(ctx, id, terms, source); err != nil {
		return nil, err
	}
	packetCopy, err := cloneFundingPSBT(packet)
	if err != nil {
		return nil, err
	}
	template, err := arkchannel.NewBackingTemplate(
		packetCopy, terms, source,
	)
	if err != nil {
		return nil, err
	}
	expected, err := e.funding.ExpectedFundingOutput(
		terms.PendingChannelID,
	)
	if err != nil {
		return nil, err
	}
	if err := template.ValidateFundingOutput(expected); err != nil {
		return nil, err
	}
	desc, err := template.SignDescriptor(terms, e.party, e.keyDesc)
	if err != nil {
		return nil, err
	}

	return e.signer.SignOutputRaw(template.Packet().UnsignedTx, desc)
}

// InstallBacking registers the immutable signed transaction before lnd starts
// watching it, then records the same fact in the durable channel FSM.
func (e *NativeFundingEndpoint) InstallBacking(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms, source arkchannel.VTXOBinding,
	backing arkchannel.Backing) error {

	if err := validateFundingRequest(ctx, id, terms, source); err != nil {
		return err
	}
	if err := backing.Validate(terms, source); err != nil {
		return err
	}
	funding, err := virtualFundingFromBacking(terms, backing)
	if err != nil {
		return err
	}
	if err := e.funding.RegisterBacking(funding); err != nil {
		return err
	}
	event := &arkchannel.BackingSigned{
		Backing: backing,
	}
	if terms.Funder == e.party {
		_, err = e.applyLocalEvent(ctx, id, event)
	} else {
		_, err = e.ApplyChannelEvent(ctx, id, event)
	}

	return err
}

// FundingFinalized reports the native lnd pending-open durability barrier.
func (e *NativeFundingEndpoint) FundingFinalized(ctx context.Context,
	terms arkchannel.Terms, backing arkchannel.Backing) (bool, error) {

	ready, err := e.funding.FundingFinalized(ctx, terms, backing)
	if err != nil || !ready {
		return ready, err
	}
	_, err = e.recordLocalEvent(ctx, terms.ID, &arkchannel.FundingFinalized{
		Party: e.party,
	})

	return err == nil, err
}

// ChannelActive reports whether native lnd moved the exact channel out of its
// pending state after virtual confirmation.
func (e *NativeFundingEndpoint) ChannelActive(ctx context.Context,
	terms arkchannel.Terms, backing arkchannel.Backing) (bool, error) {

	active, err := e.funding.ChannelActive(ctx, terms, backing)
	if err != nil || !active {
		return active, err
	}
	_, err = e.recordLocalEvent(ctx, terms.ID, &arkchannel.ChannelActive{
		ChannelPointHash:  backing.ChannelPoint.Hash,
		ChannelPointIndex: backing.ChannelPoint.Index,
	})

	return err == nil, err
}

// ApplyChannelEvent records a peer-observed fact in this endpoint's durable
// channel FSM.
func (e *NativeFundingEndpoint) ApplyChannelEvent(ctx context.Context,
	id arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	e.mu.RLock()
	sink := e.sink
	e.mu.RUnlock()
	if sink == nil {
		return arkchannel.Record{}, fmt.Errorf("channel event sink " +
			"is not bound")
	}

	return sink.ApplyPeerEvent(ctx, id, event)
}

// applyLocalEvent records evidence produced by this endpoint's native lnd
// subsystems.
func (e *NativeFundingEndpoint) applyLocalEvent(ctx context.Context,
	id arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	e.mu.RLock()
	sink := e.sink
	e.mu.RUnlock()
	if sink == nil {
		return arkchannel.Record{}, fmt.Errorf("channel event sink " +
			"is not bound")
	}

	return sink.ApplyLocalEvent(ctx, id, event)
}

// recordLocalEvent persists local native-lnd evidence without recursively
// dispatching an action already being executed by the channel service.
func (e *NativeFundingEndpoint) recordLocalEvent(ctx context.Context,
	id arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	e.mu.RLock()
	sink := e.sink
	e.mu.RUnlock()
	if sink == nil {
		return arkchannel.Record{}, fmt.Errorf("channel event sink " +
			"is not bound")
	}

	return sink.RecordLocalEvent(ctx, id, event)
}

// cloneFundingPSBT isolates endpoint validation from caller mutation.
func cloneFundingPSBT(packet *psbt.Packet) (*psbt.Packet, error) {
	if packet == nil {
		return nil, fmt.Errorf("lnd funding PSBT is required")
	}
	var encoded bytes.Buffer
	if err := packet.Serialize(&encoded); err != nil {
		return nil, fmt.Errorf("serialize lnd funding PSBT: %w", err)
	}

	return psbt.NewFromRawBytes(bytes.NewReader(encoded.Bytes()), false)
}

// validateFundingRequest checks immutable cross-system facts before invoking
// either lnd or a remote endpoint.
func validateFundingRequest(ctx context.Context, id arkchannel.ID,
	terms arkchannel.Terms, source arkchannel.VTXOBinding) error {

	select {
	case <-ctx.Done():
		return ctx.Err()

	default:
	}
	if id != terms.ID {
		return fmt.Errorf("channel ID does not match funding terms")
	}
	if err := terms.Validate(); err != nil {
		return err
	}

	return source.Validate(terms)
}
