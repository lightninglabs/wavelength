package arkscript

import (
	"bytes"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/wire/v2"
)

const (
	// DefaultChannelReactionWindow separates channel-funding eligibility
	// from funder-refund eligibility by 432 blocks: about three days at
	// 144 blocks per day, not a wall-clock guarantee.
	DefaultChannelReactionWindow uint32 = 432
)

// ChannelVTXOParams defines the key roles and relative delays of a VTXO that
// backs an unpublished Lightning channel point.
type ChannelVTXOParams struct {
	// ClientArkKey authorizes the client's part of an in-Ark spend.
	ClientArkKey *btcec.PublicKey

	// HubArkKey authorizes the hub's part of an in-Ark spend.
	HubArkKey *btcec.PublicKey

	// ArkOperatorKey completes the immediate three-party in-Ark spend.
	ArkOperatorKey *btcec.PublicKey

	// ClientChannelKey signs the delayed channel-funding transaction.
	ClientChannelKey *btcec.PublicKey

	// HubChannelKey cosigns the delayed channel-funding transaction.
	HubChannelKey *btcec.PublicKey

	// FunderKey can refund the original contribution after FunderDelay.
	FunderKey *btcec.PublicKey

	// ChannelDelay gates the two-party channel-funding path in blocks.
	// It preserves time for the Ark operator's checkpoint or forfeit
	// recovery after the VTXO confirms, and must be at least MinExitDelay.
	ChannelDelay uint32

	// FunderDelay gates the refund path in blocks from the same VTXO
	// confirmation. It must be at least ChannelDelay + ReactionWindow,
	// giving the non-funder time to materialize before refund is possible.
	FunderDelay uint32

	// MinExitDelay is the admitting Ark operator's minimum exit delay.
	MinExitDelay uint32

	// ReactionWindow is the minimum block gap between the two delayed
	// paths. Zero selects DefaultChannelReactionWindow. Neither delay
	// extends the source round's absolute batch expiry.
	ReactionWindow uint32
}

// ChannelVTXOPolicy is the compiled three-path policy for an Ark-backed
// Lightning channel.
type ChannelVTXOPolicy struct {
	// Template retains the semantic policy for encoding and validation.
	Template *PolicyTemplate

	// CompiledPolicy provides the canonical tapscripts and spend paths.
	*CompiledPolicy

	// CooperativeClosure is the client-hub-operator 3-of-3 path, with no
	// CSV delay, used for an in-Ark transfer or cooperative refresh.
	CooperativeClosure Node

	// ChannelClosure is the delayed client-hub 2-of-2 path that funds the
	// Lightning channel point when the VTXO is materialized on chain.
	ChannelClosure Node

	// FunderClosure is the longer-delayed unilateral funder refund path.
	FunderClosure Node

	// ChannelDelay is the channel-funding CSV delay from VTXO confirmation.
	ChannelDelay uint32

	// FunderDelay is the refund CSV delay from that same confirmation.
	FunderDelay uint32
}

// NewChannelVTXOPolicy constructs the cooperative, materialization, and
// funder-refund paths while enforcing their relative timing.
func NewChannelVTXOPolicy(params ChannelVTXOParams) (*ChannelVTXOPolicy,
	error) {

	if err := params.validate(); err != nil {
		return nil, err
	}

	cooperativeClosure := &Multisig{
		Keys: []*btcec.PublicKey{
			params.ClientArkKey,
			params.HubArkKey,
			params.ArkOperatorKey,
		},
	}
	channelClosure := &CSV{
		Lock: blockchain.LockTimeToSequence(
			false, params.ChannelDelay,
		),
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{
				params.ClientChannelKey,
				params.HubChannelKey,
			},
		},
	}
	funderClosure := &CSV{
		Lock: blockchain.LockTimeToSequence(
			false, params.FunderDelay,
		),
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{
				params.FunderKey,
			},
		},
	}

	template := &PolicyTemplate{
		Leaves: []LeafTemplate{
			{
				Node: cooperativeClosure,
			},
			{
				Node: channelClosure,
			},
			{
				Node: funderClosure,
			},
		},
	}
	if err := template.ValidateArkPolicy(PolicyValidationOpts{
		OperatorKey:  params.ArkOperatorKey,
		MinExitDelay: params.MinExitDelay,
	}); err != nil {
		return nil, fmt.Errorf("validate channel VTXO policy: %w", err)
	}

	compiled, err := template.Compile()
	if err != nil {
		return nil, fmt.Errorf("compile channel VTXO policy: %w", err)
	}

	return &ChannelVTXOPolicy{
		Template:           template,
		CompiledPolicy:     compiled,
		CooperativeClosure: cooperativeClosure,
		ChannelClosure:     channelClosure,
		FunderClosure:      funderClosure,
		ChannelDelay:       params.ChannelDelay,
		FunderDelay:        params.FunderDelay,
	}, nil
}

// CooperativeSpendPath returns the immediate client-hub-Ark-operator path.
func (p *ChannelVTXOPolicy) CooperativeSpendPath() (*SpendPath, error) {
	return p.SpendPathForNode(p.CooperativeClosure, nil)
}

// ChannelSpendPath returns the delayed two-party materialization path.
func (p *ChannelVTXOPolicy) ChannelSpendPath() (*SpendPath, error) {
	return p.SpendPathForNode(p.ChannelClosure, nil)
}

// FunderSpendPath returns the later unilateral refund path.
func (p *ChannelVTXOPolicy) FunderSpendPath() (*SpendPath, error) {
	return p.SpendPathForNode(p.FunderClosure, nil)
}

// EncodeChannelVTXOArtifacts returns the semantic policy and its canonical
// P2TR output script.
func EncodeChannelVTXOArtifacts(params ChannelVTXOParams) ([]byte, []byte,
	error) {

	policy, err := NewChannelVTXOPolicy(params)
	if err != nil {
		return nil, nil, err
	}

	template, err := policy.Template.Encode()
	if err != nil {
		return nil, nil, err
	}
	pkScript, err := policy.Template.PkScript()
	if err != nil {
		return nil, nil, err
	}

	return template, pkScript, nil
}

// ValidateChannelVTXOPolicy recognizes the canonical three-leaf channel VTXO
// policy using the admitting operator's key and minimum exit delay. It does
// not assign client and hub labels to the two symmetric endpoint key pairs.
func ValidateChannelVTXOPolicy(template *PolicyTemplate,
	operatorKey *btcec.PublicKey, minExitDelay uint32) error {

	if template == nil {
		return fmt.Errorf("channel VTXO template is required")
	}
	if operatorKey == nil {
		return fmt.Errorf("channel VTXO operator key is required")
	}
	if minExitDelay == 0 {
		return fmt.Errorf("channel VTXO minimum exit delay is required")
	}
	if len(template.Leaves) != 3 {
		return fmt.Errorf("channel VTXO policy must contain exactly " +
			"3 leaves")
	}

	var (
		cooperativeKeys []*btcec.PublicKey
		channelKeys     []*btcec.PublicKey
		funderKey       *btcec.PublicKey
		channelDelay    uint32
		funderDelay     uint32
	)
	for i := range template.Leaves {
		switch node := template.Leaves[i].Node.(type) {
		case *Multisig:
			if len(node.Keys) != 3 || cooperativeKeys != nil {
				return fmt.Errorf("channel VTXO leaf %d is "+
					"not the unique 3-of-3 "+
					"cooperative path", i)
			}

			cooperativeKeys = node.Keys

		case *CSV:
			if err := validateCSVLock(node.Lock); err != nil {
				return fmt.Errorf("channel VTXO leaf %d has "+
					"invalid CSV: %w", i, err)
			}

			multisig, ok := node.Inner.(*Multisig)
			if !ok {
				return fmt.Errorf("channel VTXO leaf %d is "+
					"not CSV-gated multisig", i)
			}

			delay := node.Lock & uint32(wire.SequenceLockTimeMask)
			switch len(multisig.Keys) {
			case 2:
				if channelKeys != nil {
					return fmt.Errorf("channel VTXO has " +
						"multiple 2-of-2 " +
						"materialization paths")
				}

				channelKeys = multisig.Keys
				channelDelay = delay

			case 1:
				if funderKey != nil {
					return fmt.Errorf("channel VTXO has " +
						"multiple funder refund paths")
				}

				funderKey = multisig.Keys[0]
				funderDelay = delay

			default:
				return fmt.Errorf("channel VTXO leaf %d has "+
					"%d signing keys", i, len(
					multisig.Keys,
				))
			}

		default:
			return fmt.Errorf("channel VTXO leaf %d has an "+
				"unsupported shape", i)
		}
	}

	if cooperativeKeys == nil || channelKeys == nil || funderKey == nil {
		return fmt.Errorf("channel VTXO policy is missing a required " +
			"path")
	}

	params := ChannelVTXOParams{
		ClientArkKey:     cooperativeKeys[0],
		HubArkKey:        cooperativeKeys[1],
		ArkOperatorKey:   operatorKey,
		ClientChannelKey: channelKeys[0],
		HubChannelKey:    channelKeys[1],
		FunderKey:        funderKey,
		ChannelDelay:     channelDelay,
		FunderDelay:      funderDelay,
		MinExitDelay:     minExitDelay,
	}

	return ValidateChannelVTXOTemplate(template, params)
}

// ValidateChannelVTXOTemplate verifies that an untrusted template has the
// exact key roles and delays expected by a channel intent.
func ValidateChannelVTXOTemplate(template *PolicyTemplate,
	params ChannelVTXOParams) error {

	if template == nil {
		return fmt.Errorf("channel VTXO template is required")
	}

	expected, err := NewChannelVTXOPolicy(params)
	if err != nil {
		return err
	}
	if len(template.Leaves) != len(expected.Template.Leaves) {
		return fmt.Errorf("channel VTXO policy must contain exactly " +
			"3 leaves")
	}

	actualScripts := make(map[string]struct{}, len(template.Leaves))
	for i := range template.Leaves {
		script, err := template.Leaves[i].Script()
		if err != nil {
			return fmt.Errorf("compile channel VTXO leaf %d: %w", i,
				err)
		}

		actualScripts[string(script)] = struct{}{}
	}

	for i := range expected.Template.Leaves {
		script, err := expected.Template.Leaves[i].Script()
		if err != nil {
			return err
		}
		if _, ok := actualScripts[string(script)]; !ok {
			return fmt.Errorf("channel VTXO policy does not " +
				"match expected key roles and delays")
		}
	}

	actualPkScript, err := template.PkScript()
	if err != nil {
		return fmt.Errorf("compile channel VTXO output: %w", err)
	}
	expectedPkScript, err := expected.Template.PkScript()
	if err != nil {
		return fmt.Errorf("compile expected channel VTXO output: %w",
			err)
	}
	if !bytes.Equal(actualPkScript, expectedPkScript) {
		return fmt.Errorf("channel VTXO output does not match " +
			"expected key roles and delays")
	}

	return nil
}

// validate enforces key separation and the funder reaction window.
func (p ChannelVTXOParams) validate() error {
	keys := []struct {
		name string
		key  *btcec.PublicKey
	}{
		{
			name: "client Ark",
			key:  p.ClientArkKey,
		},
		{
			name: "hub Ark",
			key:  p.HubArkKey,
		},
		{
			name: "Ark operator",
			key:  p.ArkOperatorKey,
		},
		{
			name: "client channel",
			key:  p.ClientChannelKey,
		},
		{
			name: "hub channel",
			key:  p.HubChannelKey,
		},
		{
			name: "funder",
			key:  p.FunderKey,
		},
	}
	for _, key := range keys {
		if key.key == nil {
			return fmt.Errorf("channel VTXO %s key is nil",
				key.name)
		}
	}

	if channelKeysEqual(p.ClientArkKey, p.HubArkKey) ||
		channelKeysEqual(p.ClientArkKey, p.ArkOperatorKey) ||
		channelKeysEqual(p.HubArkKey, p.ArkOperatorKey) {
		return fmt.Errorf("channel VTXO cooperative keys must be " +
			"controlled by three distinct parties")
	}
	if channelKeysEqual(p.ClientChannelKey, p.HubChannelKey) {
		return fmt.Errorf("channel VTXO materialization keys must be " +
			"distinct")
	}

	if p.MinExitDelay == 0 {
		return fmt.Errorf("channel VTXO minimum exit delay must be " +
			"non-zero")
	}
	if p.ChannelDelay < p.MinExitDelay {
		return fmt.Errorf("channel delay %d is below Ark minimum %d",
			p.ChannelDelay, p.MinExitDelay)
	}
	if p.ChannelDelay > wire.SequenceLockTimeMask ||
		p.FunderDelay > wire.SequenceLockTimeMask {
		return fmt.Errorf("channel VTXO delays must fit BIP-68 block " +
			"mode")
	}

	reactionWindow := p.ReactionWindow
	if reactionWindow == 0 {
		reactionWindow = DefaultChannelReactionWindow
	}
	if reactionWindow > wire.SequenceLockTimeMask ||
		p.ChannelDelay > math.MaxUint32-reactionWindow {
		return fmt.Errorf("channel VTXO reaction window overflows")
	}
	minimumFunderDelay := p.ChannelDelay + reactionWindow
	if p.FunderDelay < minimumFunderDelay {
		return fmt.Errorf("funder delay %d must be at least %d to "+
			"preserve the reaction window", p.FunderDelay,
			minimumFunderDelay)
	}

	return nil
}

// channelKeysEqual compares public keys by their x-only tapscript encoding.
func channelKeysEqual(a, b *btcec.PublicKey) bool {
	return bytes.Equal(
		schnorr.SerializePubKey(a), schnorr.SerializePubKey(b),
	)
}
