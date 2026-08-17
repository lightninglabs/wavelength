package lnruntime

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	lndfunding "github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const defaultFundingPollInterval = 25 * time.Millisecond

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
	sink arkchannel.ChannelEventSink
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
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sink != nil {
		return nil
	}
	e.sink = sink

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
	_, err = e.ApplyChannelEvent(ctx, id, &arkchannel.BackingSigned{
		Backing: backing,
	})

	return err
}

// FundingFinalized reports the native lnd pending-open durability barrier.
func (e *NativeFundingEndpoint) FundingFinalized(ctx context.Context,
	terms arkchannel.Terms, backing arkchannel.Backing) (bool, error) {

	return e.funding.FundingFinalized(ctx, terms, backing)
}

// ChannelActive reports whether native lnd moved the exact channel out of its
// pending state after virtual confirmation.
func (e *NativeFundingEndpoint) ChannelActive(ctx context.Context,
	terms arkchannel.Terms, backing arkchannel.Backing) (bool, error) {

	return e.funding.ChannelActive(ctx, terms, backing)
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

	return sink.Apply(ctx, id, event)
}

// ChannelNegotiator coordinates only the cross-system funding barriers. lnd
// continues to own the BOLT funding exchange and both commitment states.
type ChannelNegotiator struct {
	local    *NativeFundingEndpoint
	remote   FundingCounterparty
	peer     *Peer
	recovery ChannelRecoveryManager

	pollInterval time.Duration
}

// NewChannelNegotiator constructs the funder-side coordinator.
func NewChannelNegotiator(local *NativeFundingEndpoint,
	remote FundingCounterparty, peer *Peer,
	recovery ChannelRecoveryManager) (*ChannelNegotiator, error) {

	if local == nil {
		return nil, fmt.Errorf("local funding endpoint is required")
	}
	if remote == nil {
		return nil, fmt.Errorf("remote funding endpoint is required")
	}
	if peer == nil {
		return nil, fmt.Errorf("native lnd peer is required")
	}
	if recovery == nil {
		return nil, fmt.Errorf("channel recovery manager is required")
	}

	return &ChannelNegotiator{
		local:        local,
		remote:       remote,
		peer:         peer,
		recovery:     recovery,
		pollInterval: defaultFundingPollInterval,
	}, nil
}

// BindChannelEventSink attaches the local endpoint to its service.
func (n *ChannelNegotiator) BindChannelEventSink(
	sink arkchannel.ChannelEventSink) error {

	if err := n.local.BindChannelEventSink(sink); err != nil {
		return err
	}
	if binder, ok := n.recovery.(arkchannel.ChannelEventSinkBinder); ok {
		return binder.BindChannelEventSink(sink)
	}

	return nil
}

// NegotiateChannel runs only on lnd's funding initiator. The Ark funder is also
// the lnd opener, so every channel begins with its entire spendable balance on
// the endpoint that supplied the prepared OOR transfer.
func (n *ChannelNegotiator) NegotiateChannel(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding) error {

	if err := validateFundingRequest(ctx, id, terms, source); err != nil {
		return err
	}
	if terms.FundingInitiator() != n.local.party {
		return nil
	}

	flow, err := n.local.funding.OpenChannel(FundingOpenRequest{
		Peer:             n.peer,
		PendingChannelID: terms.PendingChannelID,
		Capacity:         terms.Capacity,
		PushAmount:       terms.InitialPushAmount(),
	})
	if err != nil {
		return err
	}
	basePacket, err := awaitNegotiatedPSBT(ctx, flow)
	if err != nil {
		return err
	}
	localPacket, err := cloneFundingPSBT(basePacket)
	if err != nil {
		return err
	}
	template, err := arkchannel.NewBackingTemplate(
		localPacket, terms, source,
	)
	if err != nil {
		return err
	}
	expected, err := n.local.funding.ExpectedFundingOutput(
		terms.PendingChannelID,
	)
	if err != nil {
		return err
	}
	if err := template.ValidateFundingOutput(expected); err != nil {
		return err
	}
	localSig, err := n.local.SignBacking(
		ctx, id, terms, source, basePacket,
	)
	if err != nil {
		return err
	}
	remoteSig, err := n.remote.SignBacking(
		ctx, id, terms, source, basePacket,
	)
	if err != nil {
		return err
	}

	var clientSig, hubSig input.Signature
	if n.local.party == arkchannel.PartyClient {
		clientSig, hubSig = localSig, remoteSig
	} else {
		clientSig, hubSig = remoteSig, localSig
	}
	backing, err := template.Complete(
		terms, source, clientSig, hubSig,
	)
	if err != nil {
		return err
	}
	if err := n.remote.InstallBacking(
		ctx, id, terms, source, backing,
	); err != nil {
		return err
	}
	if err := n.local.InstallBacking(
		ctx, id, terms, source, backing,
	); err != nil {
		return err
	}
	funding, err := virtualFundingFromBacking(terms, backing)
	if err != nil {
		return err
	}
	if err := n.local.funding.FinalizeBacking(
		terms.PendingChannelID, template.Packet(), funding,
	); err != nil {
		return err
	}
	if err := n.waitForFundingFinalized(ctx, terms, backing); err != nil {
		return err
	}

	var localRecord arkchannel.Record
	for _, party := range []arkchannel.Party{
		arkchannel.PartyClient, arkchannel.PartyHub,
	} {
		if _, err := n.remote.ApplyChannelEvent(
			ctx, id, &arkchannel.FundingFinalized{
				Party: party,
			},
		); err != nil {
			return err
		}
		localRecord, err = n.local.ApplyChannelEvent(
			ctx, id, &arkchannel.FundingFinalized{
				Party: party,
			},
		)
		if err != nil {
			return err
		}
	}
	if terms.Funder == n.local.party {
		if !localRecord.Snapshot.OORFinalized {
			return fmt.Errorf("funder OOR did not finalize after " +
				"lnd safety barrier")
		}
		if _, err := n.remote.ApplyChannelEvent(
			ctx, id, &arkchannel.OORFinalized{
				SessionID: source.OORSessionID,
			},
		); err != nil {
			return err
		}
	} else {
		// The remote FundingFinalized call above is a synchronous
		// durable barrier. On the Ark funder it does not return until
		// CommitOOR has completed and OORFinalized has been recorded in
		// its channel FSM.
		if _, err := n.local.ApplyChannelEvent(
			ctx, id, &arkchannel.OORFinalized{
				SessionID: source.OORSessionID,
			},
		); err != nil {
			return err
		}
	}
	if err := n.waitForChannelActive(ctx, terms, backing); err != nil {
		return err
	}
	activeEvent := &arkchannel.ChannelActive{
		ChannelPointHash:  backing.ChannelPoint.Hash,
		ChannelPointIndex: backing.ChannelPoint.Index,
	}
	if _, err := n.remote.ApplyChannelEvent(
		ctx, id, activeEvent,
	); err != nil {
		return err
	}
	_, err = n.local.ApplyChannelEvent(ctx, id, activeEvent)

	return err
}

// PrepareChannelRecovery copies the finalized source package to both
// endpoints before recording the activation barrier. The lnd opener drives
// this exchange and fetches the package from the remote endpoint when the hub
// owns the OOR source.
func (n *ChannelNegotiator) PrepareChannelRecovery(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding) error {

	if err := validateFundingRequest(ctx, id, terms, source); err != nil {
		return err
	}
	if terms.FundingInitiator() != n.local.party {
		return nil
	}
	var recovery arkchannel.RecoveryPackage
	var err error
	if terms.Funder == n.local.party {
		recovery, err = n.recovery.ExportRecoveryPackage(
			ctx, id, terms, source,
		)
	} else {
		exporter, ok := n.remote.(RecoveryExportCounterparty)
		if !ok {
			return fmt.Errorf("remote channel recovery export is " +
				"unavailable")
		}
		recovery, err = exporter.ExportRecoveryPackage(ctx, id)
	}
	if err != nil {
		return fmt.Errorf("export channel recovery package: %w", err)
	}
	if err := n.recovery.InstallRecoveryPackage(
		ctx, id, terms, source, recovery,
	); err != nil {
		return fmt.Errorf("install local channel recovery: %w", err)
	}
	remote, ok := n.remote.(RecoveryCounterparty)
	if !ok {
		if terms.Funder == n.local.party {

			// Receive-channel recovery is fetched by the client
			// over its existing authenticated control RPC. The hub
			// installs its own package first, then waits for the
			// client's common FSM event.
			return nil
		}

		return fmt.Errorf("remote channel recovery transport is " +
			"unavailable")
	}
	if err := remote.InstallRecoveryPackage(
		ctx, id, terms, source, recovery,
	); err != nil {
		return fmt.Errorf("install remote channel recovery: %w", err)
	}
	event := &arkchannel.RecoveryPackageInstalled{}
	if _, err := n.remote.ApplyChannelEvent(ctx, id, event); err != nil {
		return err
	}
	_, err = n.local.ApplyChannelEvent(ctx, id, event)

	return err
}

// CancelChannel removes this endpoint's native lnd reservation after its
// funder's prepared OOR session has durably aborted.
func (n *ChannelNegotiator) CancelChannel(ctx context.Context, id arkchannel.ID,
	terms arkchannel.Terms, backing *arkchannel.Backing) error {

	var channelPoint *wire.OutPoint
	if backing != nil {
		channelPoint = &backing.ChannelPoint
	}
	if err := n.local.funding.CancelBacking(
		terms.PendingChannelID, channelPoint,
	); err != nil {
		return err
	}
	_, err := n.local.ApplyChannelEvent(
		ctx, id, &arkchannel.FundingCanceled{},
	)

	return err
}

// waitForFundingFinalized polls the authoritative lnd databases rather than
// relying on an edge-triggered callback from either endpoint.
func (n *ChannelNegotiator) waitForFundingFinalized(ctx context.Context,
	terms arkchannel.Terms, backing arkchannel.Backing) error {

	return n.waitForBoth(ctx, func(ctx context.Context,
		endpoint FundingCounterparty) (bool, error) {

		return endpoint.FundingFinalized(ctx, terms, backing)
	}, func(ctx context.Context) (bool, error) {
		return n.local.FundingFinalized(ctx, terms, backing)
	})
}

// waitForChannelActive waits until virtual confirmation moved both native lnd
// channel records out of pending state.
func (n *ChannelNegotiator) waitForChannelActive(ctx context.Context,
	terms arkchannel.Terms, backing arkchannel.Backing) error {

	return n.waitForBoth(ctx, func(ctx context.Context,
		endpoint FundingCounterparty) (bool, error) {

		return endpoint.ChannelActive(ctx, terms, backing)
	}, func(ctx context.Context) (bool, error) {
		return n.local.ChannelActive(ctx, terms, backing)
	})
}

// waitForBoth evaluates local and remote durable facts until both hold.
func (n *ChannelNegotiator) waitForBoth(ctx context.Context,
	remoteCheck func(context.Context, FundingCounterparty) (bool, error),
	localCheck func(context.Context) (bool, error)) error {

	ticker := time.NewTicker(n.pollInterval)
	defer ticker.Stop()
	for {
		localReady, err := localCheck(ctx)
		if err != nil {
			return err
		}
		remoteReady, err := remoteCheck(ctx, n.remote)
		if err != nil {
			return err
		}
		if localReady && remoteReady {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-ticker.C:
		}
	}
}

// awaitNegotiatedPSBT waits for lnd to finish exchanging channel parameters.
func awaitNegotiatedPSBT(ctx context.Context,
	flow *FundingFlow) (*psbt.Packet, error) {

	for {
		select {
		case update := <-flow.Updates:
			psbtUpdate := update.GetPsbtFund()
			if psbtUpdate == nil {
				continue
			}

			return psbt.NewFromRawBytes(
				bytes.NewReader(psbtUpdate.Psbt), false,
			)

		case err := <-flow.Errors:
			return nil, err

		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
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

var _ arkchannel.FundingNegotiator = (*ChannelNegotiator)(nil)
var _ arkchannel.ChannelEventSinkBinder = (*ChannelNegotiator)(nil)
var _ FundingCounterparty = (*NativeFundingEndpoint)(nil)
