package unrollpolicy

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/unroll"
	"github.com/lightninglabs/darepo-client/virtualchannel"
	"github.com/lightninglabs/darepo-client/vtxo"
)

const (
	// VirtualChannelBackingExitPolicyKind identifies a virtual-channel
	// conflict materialization job. The final unroll spend is the
	// cooperative VTXO 2-of-2 backing transaction, not the timeout sweep.
	VirtualChannelBackingExitPolicyKind unroll.ExitPolicyKind = "virtual_channel_backing"
)

// ChannelLoader loads virtual-channel registrations by stable id.
type ChannelLoader interface {
	// GetVirtualChannel loads a virtual channel by its stable darepo id.
	GetVirtualChannel(ctx context.Context,
		id virtualchannel.ID) (*virtualchannel.Channel, error)
}

// ExitSpendPolicyResolver resolves virtual-channel backing policies for
// generic VTXO unroll jobs.
type ExitSpendPolicyResolver struct {
	Channels ChannelLoader
}

// SupportsKind reports whether this resolver handles kind.
func (r ExitSpendPolicyResolver) SupportsKind(kind unroll.ExitPolicyKind) bool {
	return kind == VirtualChannelBackingExitPolicyKind
}

// ResolveExitSpendPolicy reconstructs the cooperative backing policy from the
// persisted virtual-channel id.
func (r ExitSpendPolicyResolver) ResolveExitSpendPolicy(ctx context.Context,
	req unroll.ExitSpendPolicyRequest) (unroll.ExitSpendPolicy, error) {

	if req.Kind != VirtualChannelBackingExitPolicyKind {
		return nil, fmt.Errorf("unknown virtual channel exit policy "+
			"kind: %s", req.Kind)
	}
	if r.Channels == nil {
		return nil, fmt.Errorf("virtual channel loader must be " +
			"provided")
	}

	id, err := decodeVirtualChannelID(req.Ref)
	if err != nil {
		return nil, err
	}

	channel, err := r.Channels.GetVirtualChannel(ctx, id)
	if err != nil {
		return nil, err
	}

	return NewBackingExitSpendPolicy(channel, req.StandardDescriptor)
}

// BackingExitSpendPolicy returns the signed VTXO-to-channel-point transaction
// as the unroll actor's final spend.
type BackingExitSpendPolicy struct {
	channel *virtualchannel.Channel
	desc    *vtxo.Descriptor
}

// NewBackingExitSpendPolicy creates a cooperative virtual-channel backing
// exit policy.
func NewBackingExitSpendPolicy(channel *virtualchannel.Channel,
	desc *vtxo.Descriptor) (*BackingExitSpendPolicy, error) {

	policy := &BackingExitSpendPolicy{
		channel: channel,
		desc:    desc,
	}
	if err := policy.validateRegistration(); err != nil {
		return nil, err
	}

	return policy, nil
}

// Kind returns the durable policy kind.
func (p *BackingExitSpendPolicy) Kind() unroll.ExitPolicyKind {
	return VirtualChannelBackingExitPolicyKind
}

// CSVDelay returns zero because the cooperative VTXO spend path has no
// relative timelock.
func (p *BackingExitSpendPolicy) CSVDelay() uint32 {
	return 0
}

// RequiredLockTime returns zero because the cooperative VTXO spend path has no
// absolute timelock.
func (p *BackingExitSpendPolicy) RequiredLockTime() uint32 {
	return 0
}

// DirectBroadcast reports that the LND funding transaction must be relayed
// exactly as registered instead of going through Ark anchor CPFP handling.
func (p *BackingExitSpendPolicy) DirectBroadcast() bool {
	return true
}

// ValidateTarget verifies the materialized VTXO output is the exact output
// consumed by the signed virtual-channel backing transaction.
func (p *BackingExitSpendPolicy) ValidateTarget(target *wire.TxOut) error {
	if err := p.validateRegistration(); err != nil {
		return err
	}
	if target == nil {
		return fmt.Errorf("target output must be provided")
	}

	backing := p.channel.BackingVTXOs[0]
	if target.Value != int64(backing.Amount) {
		return fmt.Errorf("target value %d does not match virtual "+
			"channel backing amount %d", target.Value,
			backing.Amount)
	}
	if target.Value <= 0 {
		return fmt.Errorf("target output value must be positive")
	}
	if p.desc != nil && len(p.desc.PkScript) > 0 &&
		!bytes.Equal(target.PkScript, p.desc.PkScript) {
		return fmt.Errorf("target output pkscript does not match " +
			"descriptor pkscript")
	}

	return nil
}

// BuildSpendTx returns the already-signed cooperative backing transaction.
func (p *BackingExitSpendPolicy) BuildSpendTx(ctx context.Context,
	req unroll.ExitSpendRequest) (*wire.MsgTx, error) {

	_ = ctx

	if err := p.ValidateTarget(req.TargetOutput); err != nil {
		return nil, err
	}
	if req.TargetOutpoint != p.channel.BackingVTXOs[0].OutPoint {
		return nil, fmt.Errorf("target outpoint %v does not match "+
			"virtual channel backing outpoint %v",
			req.TargetOutpoint, p.channel.BackingVTXOs[0].OutPoint)
	}

	return cloneTx(p.channel.BackingTx)
}

// validateRegistration verifies the persisted registration is compatible with
// the current per-target unroll actor.
func (p *BackingExitSpendPolicy) validateRegistration() error {
	if p == nil || p.channel == nil {
		return fmt.Errorf("virtual channel must be provided")
	}
	channel := p.channel
	if channel.BackingTx == nil {
		return fmt.Errorf("virtual channel backing tx is nil")
	}
	if len(channel.BackingVTXOs) != 1 {
		return fmt.Errorf("virtual channel conflict materialization "+
			"requires exactly one backing VTXO, got %d",
			len(channel.BackingVTXOs))
	}
	if len(channel.BackingTx.TxIn) != 1 {
		return fmt.Errorf("virtual channel backing tx must have one "+
			"input for per-target unroll, got %d",
			len(channel.BackingTx.TxIn))
	}
	if channel.ChannelPoint.Index != 0 {
		return fmt.Errorf("virtual channel backing output index %d is "+
			"unsupported by unroll confirmation tracking",
			channel.ChannelPoint.Index)
	}

	backing := channel.BackingVTXOs[0]
	txIn := channel.BackingTx.TxIn[0]
	if txIn.PreviousOutPoint != backing.OutPoint {
		return fmt.Errorf("virtual channel backing tx spends %v, "+
			"expected %v", txIn.PreviousOutPoint, backing.OutPoint)
	}
	if len(txIn.Witness) == 0 {
		return fmt.Errorf("virtual channel backing tx input has no " +
			"witness")
	}

	txid := channel.BackingTx.TxHash()
	if txid != channel.ChannelPoint.Hash {
		return fmt.Errorf("virtual channel backing txid %v does not "+
			"match channel point %v", txid,
			channel.ChannelPoint.Hash)
	}
	if len(channel.BackingTx.TxOut) == 0 {
		return fmt.Errorf("virtual channel backing tx has no outputs")
	}

	funding := channel.BackingTx.TxOut[0]
	if funding.Value != int64(channel.Capacity) {
		return fmt.Errorf("virtual channel funding output value %d "+
			"does not match capacity %d", funding.Value,
			channel.Capacity)
	}
	if funding.Value <= 0 {
		return fmt.Errorf("virtual channel funding output value must " +
			"be positive")
	}
	if len(funding.PkScript) == 0 {
		return fmt.Errorf("virtual channel funding output script is " +
			"empty")
	}

	return nil
}

// decodeVirtualChannelID decodes the durable hex policy ref into an id.
func decodeVirtualChannelID(ref string) (virtualchannel.ID, error) {
	var id virtualchannel.ID
	if ref == "" {
		return id, fmt.Errorf("virtual channel policy ref is required")
	}

	raw, err := hex.DecodeString(ref)
	if err != nil {
		return id, fmt.Errorf("decode virtual channel policy ref: %w",
			err)
	}
	if len(raw) != len(id) {
		return id, fmt.Errorf("virtual channel policy ref has %d "+
			"bytes, expected %d", len(raw), len(id))
	}
	copy(id[:], raw)

	return id, nil
}

// EncodeVirtualChannelID encodes a virtual-channel id as a durable policy ref.
func EncodeVirtualChannelID(id virtualchannel.ID) string {
	return hex.EncodeToString(id[:])
}

// cloneTx returns a deep copy of tx, including witness data.
func cloneTx(tx *wire.MsgTx) (*wire.MsgTx, error) {
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("serialize backing tx: %w", err)
	}

	clone := wire.NewMsgTx(tx.Version)
	if err := clone.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		return nil, fmt.Errorf("deserialize backing tx: %w", err)
	}

	return clone, nil
}

// Compile-time checks.
var _ unroll.ExitSpendPolicyResolver = ExitSpendPolicyResolver{}
var _ unroll.ResolverKindSupport = ExitSpendPolicyResolver{}
var _ unroll.ExitSpendPolicy = (*BackingExitSpendPolicy)(nil)
var _ unroll.DirectBroadcastExitSpendPolicy = (*BackingExitSpendPolicy)(nil)
