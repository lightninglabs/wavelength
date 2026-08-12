// Package unrollbridge materializes Ark-backed channel points through the
// ordinary VTXO unroller with the signed channel backing as its final spend.
package unrollbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/unroll"
)

const (
	// ExitPolicyKind identifies a materialized channel backing transaction
	// in the durable unroll registry.
	ExitPolicyKind unroll.ExitPolicyKind = "ark_channel_backing"

	// CooperativeCloseExitPolicyKind identifies a fully signed direct spend
	// of the channel-policy VTXO.
	CooperativeCloseExitPolicyKind = unroll.ExitPolicyKind(
		"ark_channel_cooperative_close",
	)

	defaultPollInterval = 100 * time.Millisecond
)

// Controller starts and observes channel materialization jobs.
type Controller struct {
	registry       actor.ActorRef[unroll.RegistryMsg, unroll.RegistryResp]
	sourcePreparer SourcePreparer

	pollInterval time.Duration

	mu   sync.RWMutex
	sink arkchannel.ChannelEventSink
}

// SourcePreparer makes the channel-policy VTXO and its finalized OOR package
// available to the common unroller without adding it to wallet inventory.
type SourcePreparer interface {
	EnsureChannelSource(context.Context, arkchannel.ID, arkchannel.Terms,
		arkchannel.VTXOBinding, unroll.ExitPolicyKind) error
}

// NewController constructs a channel materializer over the common unroll
// registry.
func NewController(
	registry actor.ActorRef[unroll.RegistryMsg, unroll.RegistryResp],
	sourcePreparer SourcePreparer) (*Controller, error) {

	if registry == nil {
		return nil, fmt.Errorf("unroll registry is required")
	}
	if sourcePreparer == nil {
		return nil, fmt.Errorf("channel source preparer is required")
	}

	return &Controller{
		registry:       registry,
		sourcePreparer: sourcePreparer,
		pollInterval:   defaultPollInterval,
	}, nil
}

// BindChannelEventSink attaches materialization observations to the durable
// channel service.
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

// MaterializeChannel asks the ordinary VTXO unroller to publish all OOR
// ancestry and use the already signed backing transaction as its final spend.
func (c *Controller) MaterializeChannel(ctx context.Context, id arkchannel.ID,
	terms arkchannel.Terms, source arkchannel.VTXOBinding,
	backing arkchannel.Backing) error {

	if err := c.publishFinalSpend(
		ctx, id, terms, source, ExitPolicyKind,
		backing.ChannelPoint.Hash, false,
	); err != nil {
		return err
	}

	return c.apply(ctx, id, &arkchannel.BackingPublished{
		TxID: backing.ChannelPoint.Hash,
	})
}

// SettleCooperativeClose publishes the Ark ancestry and exact direct VTXO
// settlement, waiting for confirmation before lnd archives the channel.
func (c *Controller) SettleCooperativeClose(ctx context.Context,
	id arkchannel.ID, terms arkchannel.Terms, source arkchannel.VTXOBinding,
	settlement arkchannel.CooperativeClose) error {

	return c.publishFinalSpend(
		ctx, id, terms, source, CooperativeCloseExitPolicyKind,
		settlement.TxID, true,
	)
}

// publishFinalSpend runs one durable unroll job and verifies its final spend.
func (c *Controller) publishFinalSpend(ctx context.Context, id arkchannel.ID,
	terms arkchannel.Terms, source arkchannel.VTXOBinding,
	policyKind unroll.ExitPolicyKind, expectedTxID chainhash.Hash,
	waitForConfirmation bool) error {

	if err := c.sourcePreparer.EnsureChannelSource(
		ctx, id, terms, source, policyKind,
	); err != nil {
		return fmt.Errorf("prepare channel unroll source: %w", err)
	}

	result := c.registry.Ask(actor.WithoutTx(ctx),
		&unroll.EnsureUnrollRequest{
			Outpoint:       source.OutPoint,
			Trigger:        unroll.TriggerManual,
			ExitPolicyKind: policyKind,
			ExitPolicyRef:  hex.EncodeToString(id[:]),
		},
	).Await(ctx)
	response, err := result.Unpack()
	if err != nil {
		return fmt.Errorf("start channel materialization: %w", err)
	}
	if _, ok := response.(*unroll.EnsureUnrollResp); !ok {
		return fmt.Errorf("unexpected unroll response %T", response)
	}

	for {
		statusResult := c.registry.Ask(actor.WithoutTx(ctx),
			&unroll.GetStatusRequest{Outpoint: source.OutPoint},
		).Await(ctx)
		statusResponse, err := statusResult.Unpack()
		if err != nil {
			return fmt.Errorf("read channel materialization: %w",
				err)
		}
		status, ok := statusResponse.(*unroll.GetStatusResp)
		if !ok {
			return fmt.Errorf("unexpected unroll status %T",
				statusResponse)
		}
		if !status.Found {
			return fmt.Errorf("channel materialization job " +
				"disappeared")
		}
		if status.Phase == unroll.PhaseFailed {
			return fmt.Errorf("channel materialization failed: %s",
				status.FailReason)
		}
		if status.SweepTxid != nil {
			if *status.SweepTxid != expectedTxID {
				return fmt.Errorf("unroller published a " +
					"different Ark channel transaction")
			}
			if !waitForConfirmation ||
				status.Phase == unroll.PhaseCompleted {
				return nil
			}
		}
		if status.Phase == unroll.PhaseCompleted {
			return fmt.Errorf("completed channel unroll has no " +
				"final txid")
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

// apply records a materialization observation through the bound service.
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

// Resolver reconstructs channel final-spend policies from the durable channel
// FSM instead of persisting another copy in the unroller.
type Resolver struct {
	Channels arkchannel.Store
}

// SupportsKind reports support for channel backing spends.
func (r Resolver) SupportsKind(kind unroll.ExitPolicyKind) bool {
	return kind == ExitPolicyKind || kind == CooperativeCloseExitPolicyKind
}

// ResolveExitSpendPolicy loads and validates the exact signed backing named by
// the durable channel ID.
func (r Resolver) ResolveExitSpendPolicy(ctx context.Context,
	req unroll.ExitSpendPolicyRequest) (unroll.ExitSpendPolicy, error) {

	if r.Channels == nil {
		return nil, fmt.Errorf("Ark channel store is required")
	}
	if !r.SupportsKind(req.Kind) {
		return nil, fmt.Errorf("unsupported channel exit policy %q",
			req.Kind)
	}
	rawID, err := hex.DecodeString(req.Ref)
	if err != nil || len(rawID) != len(arkchannel.ID{}) {
		return nil, fmt.Errorf("invalid Ark channel policy reference")
	}
	var id arkchannel.ID
	copy(id[:], rawID)
	record, err := r.Channels.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	snapshot := record.Snapshot
	if snapshot.Source == nil || snapshot.Backing == nil {
		return nil, fmt.Errorf("Ark channel is missing " +
			"materialization artifacts")
	}
	if !snapshot.OORFinalized {
		return nil, fmt.Errorf("Ark channel OOR is not finalized")
	}
	switch req.Kind {
	case ExitPolicyKind:
		if err := snapshot.Backing.Validate(
			snapshot.Terms, *snapshot.Source,
		); err != nil {
			return nil, err
		}

		return &channelExitPolicy{
			terms:   snapshot.Terms,
			source:  snapshot.Source.Clone(),
			backing: snapshot.Backing.Clone(),
		}, nil

	case CooperativeCloseExitPolicyKind:
		if snapshot.CooperativeCloseRequest == nil ||
			snapshot.CooperativeClose == nil {
			return nil, fmt.Errorf("Ark channel is missing " +
				"cooperative close artifacts")
		}
		if err := snapshot.CooperativeClose.Validate(
			snapshot.Terms, *snapshot.Source,
			*snapshot.CooperativeCloseRequest,
		); err != nil {
			return nil, err
		}

		return &cooperativeCloseExitPolicy{
			terms:      snapshot.Terms,
			source:     snapshot.Source.Clone(),
			request:    snapshot.CooperativeCloseRequest.Clone(),
			settlement: snapshot.CooperativeClose.Clone(),
		}, nil

	default:
		return nil, fmt.Errorf("unsupported channel exit policy %q",
			req.Kind)
	}
}

// channelExitPolicy returns the immutable funding transaction as unroll's
// final spend after lineage publication and CSV maturity.
type channelExitPolicy struct {
	terms   arkchannel.Terms
	source  arkchannel.VTXOBinding
	backing arkchannel.Backing
}

// Kind returns the durable channel backing policy kind.
func (p *channelExitPolicy) Kind() unroll.ExitPolicyKind {
	return ExitPolicyKind
}

// CSVDelay returns the delay encoded by the channel materialization leaf.
func (p *channelExitPolicy) CSVDelay() uint32 {
	return p.terms.VTXO.ChannelDelay
}

// RequiredLockTime returns the backing transaction's absolute locktime.
func (p *channelExitPolicy) RequiredLockTime() uint32 {
	tx, err := p.transaction()
	if err != nil {
		return 0
	}

	return tx.LockTime
}

// ValidateTarget binds the materialized output to the channel source.
func (p *channelExitPolicy) ValidateTarget(target *wire.TxOut) error {
	if target == nil {
		return fmt.Errorf("materialized channel VTXO is required")
	}
	if target.Value != int64(p.source.Amount) ||
		!bytes.Equal(target.PkScript, p.source.PkScript) {
		return fmt.Errorf("materialized output does not match " +
			"channel VTXO")
	}

	return nil
}

// BuildSpendTx returns the fully signed transaction negotiated before the OOR
// transfer crossed its point of no return.
func (p *channelExitPolicy) BuildSpendTx(_ context.Context,
	req unroll.ExitSpendRequest) (*wire.MsgTx, error) {

	if req.TargetOutpoint != p.source.OutPoint {
		return nil, fmt.Errorf("materialized outpoint does not match " +
			"channel VTXO")
	}
	if err := p.ValidateTarget(req.TargetOutput); err != nil {
		return nil, err
	}
	if err := p.backing.Validate(p.terms, p.source); err != nil {
		return nil, err
	}

	return p.transaction()
}

// transaction decodes an isolated copy of the signed backing transaction.
func (p *channelExitPolicy) transaction() (*wire.MsgTx, error) {
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(
		bytes.NewReader(p.backing.Transaction),
	); err != nil {
		return nil, fmt.Errorf("decode channel backing transaction: %w",
			err)
	}

	return tx, nil
}

// cooperativeCloseExitPolicy returns the signed immediate three-party VTXO
// settlement after Ark ancestry reaches the chain.
type cooperativeCloseExitPolicy struct {
	terms      arkchannel.Terms
	source     arkchannel.VTXOBinding
	request    arkchannel.CooperativeCloseRequest
	settlement arkchannel.CooperativeClose
}

// Kind returns the durable cooperative-close policy kind.
func (p *cooperativeCloseExitPolicy) Kind() unroll.ExitPolicyKind {
	return CooperativeCloseExitPolicyKind
}

// CSVDelay reports that the immediate cooperative policy has no relative
// timelock.
func (*cooperativeCloseExitPolicy) CSVDelay() uint32 {
	return 0
}

// RequiredLockTime returns the signed settlement's absolute locktime.
func (p *cooperativeCloseExitPolicy) RequiredLockTime() uint32 {
	tx, err := p.transaction()
	if err != nil {
		return 0
	}

	return tx.LockTime
}

// ValidateTarget binds the settlement to the exact channel-policy VTXO.
func (p *cooperativeCloseExitPolicy) ValidateTarget(target *wire.TxOut) error {
	if target == nil {
		return fmt.Errorf("materialized channel VTXO is required")
	}
	if target.Value != int64(p.source.Amount) ||
		!bytes.Equal(target.PkScript, p.source.PkScript) {
		return fmt.Errorf("materialized output does not match " +
			"channel VTXO")
	}

	return nil
}

// BuildSpendTx returns the already signed and script-verified direct close.
func (p *cooperativeCloseExitPolicy) BuildSpendTx(_ context.Context,
	req unroll.ExitSpendRequest) (*wire.MsgTx, error) {

	if req.TargetOutpoint != p.source.OutPoint {
		return nil, fmt.Errorf("materialized outpoint does not match " +
			"channel VTXO")
	}
	if err := p.ValidateTarget(req.TargetOutput); err != nil {
		return nil, err
	}
	if err := p.settlement.Validate(
		p.terms, p.source, p.request,
	); err != nil {
		return nil, err
	}

	return p.transaction()
}

// transaction decodes an isolated direct settlement transaction.
func (p *cooperativeCloseExitPolicy) transaction() (*wire.MsgTx, error) {
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(
		bytes.NewReader(p.settlement.Transaction),
	); err != nil {
		return nil, fmt.Errorf("decode cooperative close "+
			"transaction: %w", err)
	}

	return tx, nil
}

var _ arkchannel.ChannelMaterializer = (*Controller)(nil)
var _ arkchannel.ChannelEventSinkBinder = (*Controller)(nil)
var _ unroll.ExitSpendPolicyResolver = Resolver{}
var _ unroll.ResolverKindSupport = Resolver{}
var _ unroll.ExitSpendPolicy = (*channelExitPolicy)(nil)
var _ unroll.ExitSpendPolicy = (*cooperativeCloseExitPolicy)(nil)
