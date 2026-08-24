// Package oorbridge connects Ark channel actions to the durable OOR actor.
package oorbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
)

const defaultPollInterval = 50 * time.Millisecond

var (
	// ErrOORPostPONRFailure means the OOR source is no longer safe to
	// release, but the channel-policy output has not been proven to exist.
	// The channel must remain at its commit gate for explicit recovery or
	// intervention.
	ErrOORPostPONRFailure = errors.New("channel OOR failed after point " +
		"of no return")
)

// TerminalResult is the authoritative terminal classification of a prepared
// channel OOR. Exactly one of Finalized and Aborted is true.
type TerminalResult struct {
	Finalized bool
	Aborted   bool
	Reason    string
}

// Validate rejects ambiguous or contradictory terminal classifications.
func (r TerminalResult) Validate() error {
	if r.Finalized == r.Aborted {
		return fmt.Errorf("OOR terminal result must be exactly one " +
			"of finalized or aborted")
	}
	if r.Aborted && r.Reason == "" {
		return fmt.Errorf("aborted OOR terminal result requires a " +
			"reason")
	}
	if r.Finalized && r.Reason != "" {
		return fmt.Errorf("finalized OOR terminal result cannot have " +
			"an abort reason")
	}

	return nil
}

// Controller drives the durable OOR actor and reports its terminal result back
// to the channel FSM.
type Controller struct {
	ref actor.ActorRef[oor.OORDurableMsg, oor.ActorResp]

	pollInterval time.Duration

	mu   sync.RWMutex
	sink arkchannel.ChannelEventSink
}

// New resolves the durable OOR registry from an actor system.
func New(system actor.SystemContext) (*Controller, error) {
	if system == nil {
		return nil, fmt.Errorf("actor system is required")
	}

	return NewWithRef(oor.NewServiceKey().Ref(system))
}

// NewWithRef constructs a controller from an explicit actor reference. It is
// useful for runtimes that already resolved the service.
func NewWithRef(ref actor.ActorRef[oor.OORDurableMsg, oor.ActorResp]) (
	*Controller, error) {

	if ref == nil {
		return nil, fmt.Errorf("OOR actor reference is required")
	}

	return &Controller{
		ref:          ref,
		pollInterval: defaultPollInterval,
	}, nil
}

// BindChannelEventSink connects terminal OOR observations to the channel FSM.
func (c *Controller) BindChannelEventSink(
	sink arkchannel.ChannelEventSink) error {

	if sink == nil {
		return fmt.Errorf("channel event sink is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sink != nil {
		return nil
	}
	c.sink = sink

	return nil
}

// ValidatePreparedOOR proves that the bound output exists in the exact
// deterministic OOR package and carries the expected channel policy metadata.
func (c *Controller) ValidatePreparedOOR(ctx context.Context,
	terms arkchannel.Terms, source arkchannel.VTXOBinding) error {

	if err := source.Validate(terms); err != nil {
		return err
	}
	state, err := c.state(ctx, source.OORSessionID)
	if err != nil {
		return err
	}
	prepared, ok := state.(*oor.Prepared)
	if !ok {
		return fmt.Errorf("OOR session is %T, expected prepared", state)
	}
	recipients, err := oor.ExtractArkRecipients(prepared.ArkPSBT)
	if err != nil {
		return fmt.Errorf("extract prepared OOR outputs: %w", err)
	}

	for _, recipient := range recipients {
		if recipient.OutputIndex != source.OutPoint.Index {
			continue
		}
		if recipient.Value != source.Amount ||
			!bytes.Equal(recipient.PkScript, source.PkScript) {
			return fmt.Errorf("prepared OOR output does not " +
				"match binding")
		}
		for _, declared := range prepared.RecipientOutputs {
			if declared.Value != source.Amount ||
				!bytes.Equal(
					declared.PkScript, source.PkScript,
				) {

				continue
			}
			if !bytes.Equal(
				declared.VTXOPolicyTemplate,
				source.PolicyTemplate,
			) {
				return fmt.Errorf("prepared OOR policy does " +
					"not match binding")
			}

			return nil
		}

		return fmt.Errorf("prepared OOR output has no channel policy")
	}

	return fmt.Errorf("prepared OOR output index %d is missing",
		source.OutPoint.Index)
}

// CommitPreparedOOR crosses the OOR signing gate and waits for a durable
// terminal result before advancing the channel FSM.
func (c *Controller) CommitPreparedOOR(ctx context.Context, id arkchannel.ID,
	terms arkchannel.Terms, source arkchannel.VTXOBinding) error {

	result, err := c.CommitPreparedOORResult(ctx, id, terms, source)
	if err != nil {
		return err
	}
	_, err = c.applyTerminalResult(ctx, id, source, result)

	return err
}

// CommitPreparedOORResult commits when the session is still prepared, or
// reconciles a prior/lost control response to one authoritative outcome.
func (c *Controller) CommitPreparedOORResult(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms,
	source arkchannel.VTXOBinding) (TerminalResult, error) {

	if err := source.Validate(terms); err != nil {
		return TerminalResult{}, err
	}

	return c.controlPreparedOOR(
		ctx, source, &oor.CommitPreparedEvent{},
	)
}

// controlPreparedOOR submits a control event only while the OOR is still at
// its prepared gate. Replays after either side won the gate only observe the
// actor's durable terminal state.
func (c *Controller) controlPreparedOOR(ctx context.Context,
	source arkchannel.VTXOBinding, event oor.Event) (TerminalResult,
	error) {

	state, err := c.state(ctx, source.OORSessionID)
	if err != nil {
		return TerminalResult{}, err
	}
	if result, terminal, err := terminalResult(state); err != nil {
		return TerminalResult{}, err
	} else if terminal {
		return result, nil
	}

	if _, prepared := state.(*oor.Prepared); prepared {
		driveErr := c.drive(ctx, source, event)
		if driveErr != nil {
			// A concurrent control event can advance the state
			// before this request observes its response. Re-read
			// before deciding that the control failed.
			state, err = c.state(ctx, source.OORSessionID)
			if err != nil {
				return TerminalResult{}, fmt.Errorf("drive "+
					"OOR session: %v; reconcile state: %w",
					driveErr, err)
			}
			_, stillPrepared := state.(*oor.Prepared)
			if stillPrepared {
				return TerminalResult{}, driveErr
			}
			result, terminal, terminalErr := terminalResult(state)
			if terminalErr != nil {
				return TerminalResult{}, terminalErr
			}
			if terminal {
				return result, nil
			}
		}
	}

	return c.waitForTerminal(ctx, source)
}

// SettleCooperativeClose spends the channel-policy VTXO through its no-delay
// 3-of-3 path in the ordinary durable OOR protocol. The Ark operator signs the
// checkpoint before the local actor adds the client's signature; the hub's
// pre-authorized signature is supplied as external witness material.
func (c *Controller) SettleCooperativeClose(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms, source arkchannel.VTXOBinding,
	request arkchannel.CooperativeCloseRequest,
	settlement arkchannel.CooperativeClose,
	clientKey keychain.KeyDescriptor) error {

	if id != terms.ID {
		return fmt.Errorf("cooperative close channel ID does not match")
	}
	if err := settlement.Validate(terms, source, request); err != nil {
		return err
	}
	template, err := arkchannel.NewCooperativeCloseTemplate(
		terms, source, request, settlement.Proposal.ClientBalance,
		settlement.Proposal.HubBalance,
		settlement.Proposal.CommitmentHeight,
	)
	if err != nil {
		return err
	}
	spec, err := template.OORSpec()
	if err != nil {
		return err
	}
	spendPath, err := arkscript.DecodeSpendPath(spec.SpendPath)
	if err != nil {
		return fmt.Errorf("decode cooperative close spend path: %w",
			err)
	}
	clientPolicyKey, err := btcec.ParsePubKey(
		terms.VTXO.ClientArkKey[:],
	)
	if err != nil {
		return fmt.Errorf("parse client Ark key: %w", err)
	}
	if clientKey.PubKey == nil ||
		!clientKey.PubKey.IsEqual(clientPolicyKey) {
		return fmt.Errorf("cooperative close client key does not match")
	}
	operatorKey, err := btcec.ParsePubKey(terms.VTXO.ArkOperatorKey[:])
	if err != nil {
		return fmt.Errorf("parse Ark operator key: %w", err)
	}
	hubKey, err := btcec.ParsePubKey(terms.VTXO.HubArkKey[:])
	if err != nil {
		return fmt.Errorf("parse hub Ark key: %w", err)
	}
	transferInput := oor.TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint: source.OutPoint, Amount: source.Amount,
			PolicyTemplate: slices.Clone(source.PolicyTemplate),
			PkScript:       slices.Clone(source.PkScript),
			ClientKey:      clientKey,
			OperatorKey:    operatorKey,
			RelativeExpiry: terms.VTXO.MinExitDelay,
		},
		VTXOPolicyTemplate: slices.Clone(source.PolicyTemplate),
		CustomSpend:        spendPath,
		ExternalSignatures: []oor.ExternalTaprootScriptSignature{{
			PubKey: hubKey, WitnessScript: slices.Clone(
				spendPath.SpendInfo.WitnessScript,
			),
			Signature: slices.Clone(settlement.Transaction),
			SigHash:   txscript.SigHashDefault,
		}},
	}
	if err := transferInput.Validate(); err != nil {
		return fmt.Errorf("validate cooperative close OOR input: %w",
			err)
	}

	idempotencyKey := "ark-channel-close:" + hex.EncodeToString(id[:])
	result := c.ref.Ask(actor.WithoutTx(ctx), &oor.StartTransferRequest{
		Policy:         spec.CheckpointPolicy,
		Inputs:         []oor.TransferInput{transferInput},
		Recipients:     spec.Recipients,
		IdempotencyKey: idempotencyKey,
		PrepareOnly:    true,
	}).Await(ctx)
	response, err := result.Unpack()
	if err != nil {
		return fmt.Errorf("start cooperative close OOR: %w", err)
	}
	started, ok := response.(*oor.StartTransferResponse)
	if !ok {
		return fmt.Errorf("unexpected cooperative close OOR "+
			"response %T", response)
	}
	if [32]byte(started.SessionID) != settlement.TxID {
		return fmt.Errorf("cooperative close OOR session ID changed")
	}
	state, err := c.state(ctx, [32]byte(started.SessionID))
	if err != nil {
		return err
	}
	if prepared, ok := state.(*oor.Prepared); ok {
		if len(prepared.CheckpointPSBTs) != 1 {
			return fmt.Errorf("cooperative close OOR has %d "+
				"checkpoints, expected one",
				len(prepared.CheckpointPSBTs))
		}
		checkpoint, err := psbtutil.Serialize(
			prepared.CheckpointPSBTs[0],
		)
		if err != nil {
			return fmt.Errorf("serialize prepared cooperative "+
				"close: %w", err)
		}
		if !bytes.Equal(checkpoint, settlement.Proposal.Transaction) {
			return fmt.Errorf("prepared cooperative close " +
				"checkpoint does not match hub authorization")
		}
		if err := c.driveSession(
			ctx, [32]byte(started.SessionID),
			&oor.CommitPreparedEvent{},
		); err != nil {
			return err
		}
	}

	return c.waitForOORCompletion(ctx, [32]byte(started.SessionID))
}

// AbortPreparedOOR aborts only a pre-PONR transfer and waits until that fact is
// durable before allowing lnd funding cancellation.
func (c *Controller) AbortPreparedOOR(ctx context.Context, id arkchannel.ID,
	terms arkchannel.Terms, source arkchannel.VTXOBinding,
	reason string) error {

	result, err := c.AbortPreparedOORResult(
		ctx, id, terms, source, reason,
	)
	if err != nil {
		return err
	}
	_, err = c.applyTerminalResult(ctx, id, source, result)

	return err
}

// AbortPreparedOORResult aborts only while the session is prepared. If commit
// already won, it waits for and returns the finalized result instead of
// misclassifying the source as releasable.
func (c *Controller) AbortPreparedOORResult(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms, source arkchannel.VTXOBinding,
	reason string) (TerminalResult, error) {

	if err := source.Validate(terms); err != nil {
		return TerminalResult{}, err
	}
	if reason == "" {
		return TerminalResult{}, fmt.Errorf("OOR abort reason is " +
			"required")
	}

	return c.controlPreparedOOR(
		ctx, source, &oor.AbortPreparedEvent{
			Reason: reason,
		},
	)
}

// drive records one durable control event in the exact OOR session.
func (c *Controller) drive(ctx context.Context, source arkchannel.VTXOBinding,
	event oor.Event) error {

	return c.driveSession(ctx, source.OORSessionID, event)
}

// driveSession records one durable control event in the named OOR session.
func (c *Controller) driveSession(ctx context.Context, sessionID [32]byte,
	event oor.Event) error {

	result := c.ref.Ask(actor.WithoutTx(ctx), &oor.DriveEventRequest{
		SessionID: oor.SessionID(sessionID),
		Event:     event,
	}).Await(ctx)
	response, err := result.Unpack()
	if err != nil {
		return fmt.Errorf("drive OOR session: %w", err)
	}
	if _, ok := response.(*oor.DriveEventResponse); !ok {
		return fmt.Errorf("unexpected OOR drive response %T", response)
	}

	return nil
}

// waitForTerminal observes the OOR actor until it can produce an authoritative
// source result without requiring a channel-event sink. A post-PONR failure
// remains unresolved because it proves neither a releasable source nor a
// materialized channel-policy output.
func (c *Controller) waitForTerminal(ctx context.Context,
	source arkchannel.VTXOBinding) (TerminalResult, error) {

	for {
		state, err := c.state(ctx, source.OORSessionID)
		if err != nil {
			return TerminalResult{}, err
		}
		if result, terminal, err := terminalResult(state); err != nil {
			return TerminalResult{}, err
		} else if terminal {
			return result, nil
		}

		timer := time.NewTimer(c.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}

			return TerminalResult{}, ctx.Err()

		case <-timer.C:
		}
	}
}

// terminalResult maps only proven OOR outcomes onto channel source facts.
// Post-PONR failure remains an error so it cannot activate an unfunded channel.
func terminalResult(state oor.SessionState) (TerminalResult, bool, error) {
	switch state := state.(type) {
	case *oor.Completed:
		return TerminalResult{Finalized: true}, true, nil

	case *oor.Failed:
		if state.PrePONR {
			reason := state.Reason
			if reason == "" {
				reason = "OOR failed before point of no return"
			}

			return TerminalResult{
				Aborted: true, Reason: reason,
			}, true, nil
		}

		return TerminalResult{}, true, fmt.Errorf("%w: %s",
			ErrOORPostPONRFailure, state.Reason)

	case oor.ReceiveState:
		// An incoming self-transfer for the same Ark txid can only
		// exist after the operator finalized the outgoing package. Its
		// row may replace the reaped outgoing row before this observer
		// polls it.
		return TerminalResult{Finalized: true}, true, nil

	default:
		return TerminalResult{}, false, nil
	}
}

// applyTerminalResult records exactly one terminal OOR fact in the channel
// sink before returning that same classification to the caller.
func (c *Controller) applyTerminalResult(ctx context.Context, id arkchannel.ID,
	source arkchannel.VTXOBinding, result TerminalResult) (TerminalResult,
	error) {

	if err := result.Validate(); err != nil {
		return TerminalResult{}, err
	}
	var event arkchannel.Event
	if result.Finalized {
		event = &arkchannel.OORFinalized{
			SessionID: source.OORSessionID,
		}
	} else {
		event = &arkchannel.OORAborted{
			SessionID: source.OORSessionID,
			Reason:    result.Reason,
		}
	}
	if err := c.apply(ctx, id, event); err != nil {
		return TerminalResult{}, err
	}

	return result, nil
}

// waitForOORCompletion observes a cooperative-close OOR without emitting the
// channel-creation OORFinalized event used by prepared funding sessions.
func (c *Controller) waitForOORCompletion(ctx context.Context,
	sessionID [32]byte) error {

	for {
		state, err := c.state(ctx, sessionID)
		if err != nil {
			return err
		}

		switch state := state.(type) {
		case *oor.Completed:
			return nil

		case *oor.Failed:
			return fmt.Errorf("cooperative close OOR failed: %s",
				state.Reason)

		case oor.ReceiveState:
			// The sender can also own one replacement output. Its
			// incoming row may replace the completed outgoing row.
			return nil
		}

		timer := time.NewTimer(c.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}

			return ctx.Err()

		case <-timer.C:
		}
	}
}

// state loads the authoritative state through the durable OOR registry.
func (c *Controller) state(ctx context.Context, sessionID [32]byte) (
	oor.SessionState, error) {

	result := c.ref.Ask(actor.WithoutTx(ctx), &oor.GetStateRequest{
		SessionID: oor.SessionID(sessionID),
	}).Await(ctx)
	response, err := result.Unpack()
	if err != nil {
		return nil, fmt.Errorf("get OOR session state: %w", err)
	}
	state, ok := response.(*oor.GetStateResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected OOR state response %T",
			response)
	}
	if state.State == nil {
		return nil, fmt.Errorf("OOR session returned an empty state")
	}

	return state.State, nil
}

// apply records one OOR terminal observation through the bound channel service.
func (c *Controller) apply(ctx context.Context, id arkchannel.ID,
	event arkchannel.Event) error {

	c.mu.RLock()
	sink := c.sink
	c.mu.RUnlock()
	if sink == nil {
		return fmt.Errorf("channel event sink is not bound")
	}

	_, err := sink.Apply(ctx, id, event)

	return err
}

var _ arkchannel.OORTransferController = (*Controller)(nil)
var _ arkchannel.ChannelEventSinkBinder = (*Controller)(nil)
