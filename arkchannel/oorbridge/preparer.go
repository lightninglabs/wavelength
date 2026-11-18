package oorbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/oor"
)

// PrepareRequest contains the funder's selected VTXOs and the operator policy
// needed to create one channel-policy output through the ordinary OOR actor.
type PrepareRequest struct {
	Terms            arkchannel.Terms
	CheckpointPolicy arkscript.CheckpointPolicy
	Inputs           []oor.TransferInput
	BackingFee       btcutil.Amount
	ChangeOutput     *oortx.RecipientOutput
}

// PrepareResult reports the exact channel-policy output and whether another
// replay already owned the idempotency key. A caller that selected fresh VTXOs
// must release them when Existing is true.
type PrepareResult struct {
	Binding  arkchannel.VTXOBinding
	Existing bool
}

// PreparationStatus classifies the durable admission state for one channel
// idempotency key.
type PreparationStatus uint8

const (
	// PreparationAbsent proves no accepted or durable active session owns
	// the channel key. A fresh wallet selection is safe after old locks are
	// released.
	PreparationAbsent PreparationStatus = iota

	// PreparationPending means the registry accepted the request but its
	// child has not committed a durable session row yet.
	PreparationPending

	// PreparationPrepared means the exact pre-signing binding is
	// recoverable.
	PreparationPrepared

	// PreparationAccepted means the keyed session advanced beyond
	// preparation. Its inputs must never be unlocked as an unaccepted
	// request.
	PreparationAccepted
)

// PreparationLookup is the authoritative keyed admission result.
type PreparationLookup struct {
	Status         PreparationStatus
	Binding        arkchannel.VTXOBinding
	InputOutpoints []wire.OutPoint
}

// PrepareChannel durably reserves the selected VTXOs and constructs the exact
// OOR output used by lnd negotiation. It does not release any signature or
// network side effect.
func (c *Controller) PrepareChannel(ctx context.Context, req PrepareRequest) (
	PrepareResult, error) {

	if err := req.Terms.Validate(); err != nil {
		return PrepareResult{}, err
	}
	if req.CheckpointPolicy.OperatorKey == nil {
		return PrepareResult{}, fmt.Errorf("OOR checkpoint operator " +
			"key is required")
	}
	if req.CheckpointPolicy.CSVDelay == 0 {
		return PrepareResult{}, fmt.Errorf("OOR checkpoint delay is " +
			"required")
	}
	if len(req.Inputs) == 0 {
		return PrepareResult{}, fmt.Errorf("at least one OOR funding " +
			"input is required")
	}
	if err := validateBackingAmount(req.Terms, req.BackingFee); err != nil {
		return PrepareResult{}, err
	}

	policy, pkScript, err := req.Terms.VTXO.Artifacts()
	if err != nil {
		return PrepareResult{}, err
	}
	amount := req.Terms.Capacity + req.BackingFee
	recipient := oortx.RecipientOutput{
		PkScript:           pkScript,
		Value:              amount,
		VTXOPolicyTemplate: policy,
	}
	recipients := []oortx.RecipientOutput{recipient}
	if req.ChangeOutput != nil {
		change := *req.ChangeOutput
		change.PkScript = append([]byte(nil), change.PkScript...)
		change.VTXOPolicyTemplate = append(
			[]byte(nil), change.VTXOPolicyTemplate...,
		)
		if change.Value <= 0 || len(change.PkScript) == 0 ||
			len(change.VTXOPolicyTemplate) == 0 {
			return PrepareResult{}, fmt.Errorf("complete OOR " +
				"change output is required")
		}
		recipients = append(recipients, change)
	}
	idempotencyKey := channelIdempotencyKey(req.Terms.ID)
	actorCtx := actor.WithoutTx(context.WithoutCancel(ctx))
	result := c.ref.Ask(actorCtx, &oor.StartTransferRequest{
		Policy:         req.CheckpointPolicy,
		Inputs:         req.Inputs,
		Recipients:     recipients,
		IdempotencyKey: idempotencyKey,
		PrepareOnly:    true,
	}).Await(ctx)
	response, err := result.Unpack()
	if err != nil {
		if !preparationAwaitCanceled(ctx, err) {
			return PrepareResult{}, fmt.Errorf("prepare channel "+
				"OOR transfer: %w", err)
		}

		return PrepareResult{}, ambiguousPreparationError(
			fmt.Errorf("prepare channel OOR transfer: %w", err),
		)
	}
	started, ok := response.(*oor.StartTransferResponse)
	if !ok {
		return PrepareResult{}, ambiguousPreparationError(
			fmt.Errorf("unexpected OOR prepare response %T",
				response),
		)
	}
	if started.SessionID == (oor.SessionID{}) {
		return PrepareResult{}, ambiguousPreparationError(
			fmt.Errorf("prepared OOR session ID is empty"),
		)
	}
	state, err := c.state(ctx, [32]byte(started.SessionID))
	if err != nil {
		return PrepareResult{}, ambiguousPreparationError(err)
	}
	prepared, ok := state.(*oor.Prepared)
	if !ok {
		return PrepareResult{}, ambiguousPreparationError(
			fmt.Errorf("OOR session is %T, expected prepared",
				state),
		)
	}
	binding, err := c.preparedChannelBinding(
		ctx, req.Terms, req.BackingFee, [32]byte(started.SessionID),
		prepared,
	)
	if err != nil {
		return PrepareResult{}, ambiguousPreparationError(err)
	}

	return PrepareResult{
		Binding: binding, Existing: started.Existing,
	}, nil
}

// preparationAwaitCanceled distinguishes an interrupted caller wait from an
// actor result that definitively rejected admission.
func preparationAwaitCanceled(ctx context.Context, err error) bool {
	if ctx.Err() == nil {
		return false
	}

	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// ambiguousPreparationError prevents a caller from declaring the channel
// failed after the durable OOR actor may already own its selected inputs.
func ambiguousPreparationError(err error) error {
	return fmt.Errorf("%w: %w", arkchannel.ErrOORPreparationAmbiguous, err)
}

// LookupPreparedChannel reconstructs an interrupted channel binding from the
// OOR actor's durable idempotency-key winner without selecting more VTXOs.
func (c *Controller) LookupPreparedChannel(ctx context.Context,
	terms arkchannel.Terms, backingFee btcutil.Amount) (
	arkchannel.VTXOBinding, bool, error) {

	lookup, err := c.LookupChannelPreparation(ctx, terms, backingFee)
	if err != nil {
		return arkchannel.VTXOBinding{}, false, err
	}
	switch lookup.Status {
	case PreparationAbsent:
		return arkchannel.VTXOBinding{}, false, nil

	case PreparationPrepared:
		return lookup.Binding, true, nil

	case PreparationPending, PreparationAccepted:
		return arkchannel.VTXOBinding{}, false,
			ambiguousPreparationError(
				fmt.Errorf("channel OOR admission is %s",
					lookup.Status),
			)

	default:
		return arkchannel.VTXOBinding{}, false, fmt.Errorf("unknown "+
			"channel OOR preparation status %d", lookup.Status)
	}
}

// String returns the stable diagnostic name of a preparation status.
func (s PreparationStatus) String() string {
	switch s {
	case PreparationAbsent:
		return "absent"

	case PreparationPending:
		return "pending"

	case PreparationPrepared:
		return "prepared"

	case PreparationAccepted:
		return "accepted"

	default:
		return "unknown"
	}
}

// LookupChannelPreparation classifies one keyed admission without selecting
// inputs or starting another OOR transfer.
func (c *Controller) LookupChannelPreparation(ctx context.Context,
	terms arkchannel.Terms, backingFee btcutil.Amount) (PreparationLookup,
	error) {

	if err := terms.Validate(); err != nil {
		return PreparationLookup{}, err
	}
	if err := validateBackingAmount(terms, backingFee); err != nil {
		return PreparationLookup{}, err
	}
	result := c.ref.Ask(actor.WithoutTx(ctx), &oor.LookupTransferRequest{
		IdempotencyKey: channelIdempotencyKey(terms.ID),
	}).Await(ctx)
	response, err := result.Unpack()
	if err != nil {
		return PreparationLookup{}, fmt.Errorf("lookup prepared "+
			"channel OOR: %w", err)
	}
	lookup, ok := response.(*oor.LookupTransferResponse)
	if !ok {
		return PreparationLookup{}, fmt.Errorf("unexpected OOR lookup "+
			"response %T", response)
	}
	if !lookup.Found && !lookup.Pending {
		return PreparationLookup{Status: PreparationAbsent}, nil
	}
	if lookup.SessionID == (oor.SessionID{}) {
		return PreparationLookup{}, fmt.Errorf("prepared OOR lookup " +
			"returned an empty session ID")
	}
	if lookup.Pending {
		return PreparationLookup{Status: PreparationPending}, nil
	}
	state, err := c.state(ctx, [32]byte(lookup.SessionID))
	if err != nil {
		return PreparationLookup{}, err
	}
	prepared, ok := state.(*oor.Prepared)
	if !ok {
		if failed, ok := state.(*oor.Failed); ok && failed.PrePONR {
			return PreparationLookup{Status: PreparationAbsent}, nil
		}

		return PreparationLookup{
			Status:         PreparationAccepted,
			InputOutpoints: sessionInputOutpoints(state),
		}, nil
	}
	binding, err := c.preparedChannelBinding(
		ctx, terms, backingFee, [32]byte(lookup.SessionID), prepared,
	)
	if err != nil {
		return PreparationLookup{}, err
	}

	return PreparationLookup{
		Status:         PreparationPrepared,
		Binding:        binding,
		InputOutpoints: oor.InputOutpoints(prepared.TransferInputs),
	}, nil
}

// sessionInputOutpoints returns the selected inputs retained by outgoing OOR
// states. Terminal success intentionally returns nil because its wallet update
// already owns the source lifecycle.
func sessionInputOutpoints(state oor.SessionState) []wire.OutPoint {
	var inputs []oor.TransferInput
	switch state := state.(type) {
	case *oor.Prepared:
		inputs = state.TransferInputs

	case *oor.AwaitingArkSignatures:
		inputs = state.TransferInputs

	case *oor.AwaitingSubmitAccepted:
		inputs = state.TransferInputs

	case *oor.AwaitingCheckpointSignatures:
		inputs = state.TransferInputs

	case *oor.AwaitingFinalizeAccepted:
		inputs = state.TransferInputs

	case *oor.AwaitingLocalVTXOUpdate:
		inputs = state.TransferInputs
	}

	return oor.InputOutpoints(inputs)
}

// preparedChannelBinding derives and validates the exact target output from a
// durable prepared OOR state.
func (c *Controller) preparedChannelBinding(ctx context.Context,
	terms arkchannel.Terms, backingFee btcutil.Amount, sessionID [32]byte,
	prepared *oor.Prepared) (arkchannel.VTXOBinding, error) {

	policy, pkScript, err := terms.VTXO.Artifacts()
	if err != nil {
		return arkchannel.VTXOBinding{}, err
	}
	recipient := oortx.RecipientOutput{
		PkScript: pkScript, Value: terms.Capacity + backingFee,
		VTXOPolicyTemplate: policy,
	}
	outpoint, err := oortx.RecipientOutPoint(
		chainhash.Hash(sessionID), prepared.RecipientOutputs, recipient,
	)
	if err != nil {
		return arkchannel.VTXOBinding{}, fmt.Errorf("resolve prepared "+
			"channel output: %w", err)
	}
	var arkTransaction bytes.Buffer
	if err := prepared.ArkPSBT.UnsignedTx.Serialize(
		&arkTransaction,
	); err != nil {
		return arkchannel.VTXOBinding{}, fmt.Errorf("serialize "+
			"prepared Ark transaction: %w", err)
	}
	binding := arkchannel.VTXOBinding{
		OORSessionID: sessionID, OutPoint: outpoint,
		Amount: recipient.Value, ArkTransaction: arkTransaction.Bytes(),
		PolicyTemplate: policy, PkScript: pkScript,
	}
	if err := c.ValidatePreparedOOR(ctx, terms, binding); err != nil {
		return arkchannel.VTXOBinding{}, fmt.Errorf("validate "+
			"prepared channel OOR: %w", err)
	}

	return binding, nil
}

// validateBackingAmount rejects arithmetic overflow before building outputs.
func validateBackingAmount(terms arkchannel.Terms,
	backingFee btcutil.Amount) error {

	if backingFee <= 0 || terms.Capacity >
		btcutil.Amount(math.MaxInt64)-backingFee {
		return fmt.Errorf("invalid channel backing amount")
	}

	return nil
}

// channelIdempotencyKey maps one immutable channel ID to its OOR session.
func channelIdempotencyKey(id arkchannel.ID) string {
	return "ark-channel:" + hex.EncodeToString(id[:])
}
