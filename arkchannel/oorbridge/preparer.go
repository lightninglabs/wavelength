package oorbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
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

// PrepareChannel durably reserves the selected VTXOs and constructs the exact
// OOR output used by lnd negotiation. It does not release any signature or
// network side effect.
func (c *Controller) PrepareChannel(ctx context.Context, req PrepareRequest) (
	arkchannel.VTXOBinding, error) {

	if err := req.Terms.Validate(); err != nil {
		return arkchannel.VTXOBinding{}, err
	}
	if req.CheckpointPolicy.OperatorKey == nil {
		return arkchannel.VTXOBinding{}, fmt.Errorf("OOR checkpoint " +
			"operator key is required")
	}
	if req.CheckpointPolicy.CSVDelay == 0 {
		return arkchannel.VTXOBinding{}, fmt.Errorf("OOR checkpoint " +
			"delay is required")
	}
	if len(req.Inputs) == 0 {
		return arkchannel.VTXOBinding{}, fmt.Errorf("at least one " +
			"OOR funding input is required")
	}
	if req.BackingFee <= 0 {
		return arkchannel.VTXOBinding{}, fmt.Errorf("channel backing " +
			"fee must be positive")
	}

	policy, pkScript, err := req.Terms.VTXO.Artifacts()
	if err != nil {
		return arkchannel.VTXOBinding{}, err
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
			return arkchannel.VTXOBinding{}, fmt.Errorf(
				"complete OOR change output is required")
		}
		recipients = append(recipients, change)
	}
	idempotencyKey := "ark-channel:" + hex.EncodeToString(
		req.Terms.ID[:],
	)
	result := c.ref.Ask(actor.WithoutTx(ctx), &oor.StartTransferRequest{
		Policy:         req.CheckpointPolicy,
		Inputs:         req.Inputs,
		Recipients:     recipients,
		IdempotencyKey: idempotencyKey,
		PrepareOnly:    true,
	}).Await(ctx)
	response, err := result.Unpack()
	if err != nil {
		return arkchannel.VTXOBinding{}, fmt.Errorf("prepare channel "+
			"OOR transfer: %w", err)
	}
	started, ok := response.(*oor.StartTransferResponse)
	if !ok {
		return arkchannel.VTXOBinding{}, fmt.Errorf("unexpected OOR "+
			"prepare response %T", response)
	}
	if started.SessionID == (oor.SessionID{}) {
		return arkchannel.VTXOBinding{}, fmt.Errorf("prepared OOR " +
			"session ID is empty")
	}
	state, err := c.state(ctx, [32]byte(started.SessionID))
	if err != nil {
		return arkchannel.VTXOBinding{}, err
	}
	prepared, ok := state.(*oor.Prepared)
	if !ok {
		return arkchannel.VTXOBinding{}, fmt.Errorf("OOR session is "+
			"%T, expected prepared", state)
	}
	var arkTransaction bytes.Buffer
	if err := prepared.ArkPSBT.UnsignedTx.Serialize(
		&arkTransaction,
	); err != nil {
		return arkchannel.VTXOBinding{}, fmt.Errorf("serialize "+
			"prepared Ark transaction: %w", err)
	}

	outpoint, err := oortx.RecipientOutPoint(
		chainhash.Hash(started.SessionID), recipients, recipient,
	)
	if err != nil {
		return arkchannel.VTXOBinding{}, fmt.Errorf("resolve prepared "+
			"channel output: %w", err)
	}
	binding := arkchannel.VTXOBinding{
		OORSessionID:   [32]byte(started.SessionID),
		OutPoint:       outpoint,
		Amount:         amount,
		ArkTransaction: arkTransaction.Bytes(),
		PolicyTemplate: policy,
		PkScript:       pkScript,
	}
	if err := c.ValidatePreparedOOR(ctx, req.Terms, binding); err != nil {
		return arkchannel.VTXOBinding{}, fmt.Errorf("validate "+
			"prepared channel OOR: %w", err)
	}

	return binding, nil
}
