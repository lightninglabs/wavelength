package unrollpolicy

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/unroll"
	"github.com/lightninglabs/darepo-client/virtualchannel"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/stretchr/testify/require"
)

// TestBackingExitSpendPolicyReturnsSignedBackingTx verifies that the policy
// hands unroll the persisted cooperative backing spend after validating the
// materialized target.
func TestBackingExitSpendPolicyReturnsSignedBackingTx(t *testing.T) {
	t.Parallel()

	channel, targetOutput := testVirtualChannel(t)
	policy, err := NewBackingExitSpendPolicy(
		channel, &vtxo.Descriptor{
			PkScript: targetOutput.PkScript,
		},
	)
	require.NoError(t, err)

	spendTx, err := policy.BuildSpendTx(
		t.Context(), unroll.ExitSpendRequest{
			TargetOutpoint: channel.BackingVTXOs[0].OutPoint,
			TargetOutput:   targetOutput,
		},
	)
	require.NoError(t, err)
	require.Equal(t, channel.BackingTx.TxHash(), spendTx.TxHash())
	require.Equal(
		t, channel.BackingTx.TxIn[0].Witness, spendTx.TxIn[0].Witness,
	)

	spendTx.TxOut[0].Value--
	require.NotEqual(
		t, channel.BackingTx.TxOut[0].Value, spendTx.TxOut[0].Value,
	)
}

// TestBackingExitSpendPolicyRejectsMultiVTXO records the current per-target
// unroll limitation so multi-input virtual backing transactions fail closed.
func TestBackingExitSpendPolicyRejectsMultiVTXO(t *testing.T) {
	t.Parallel()

	channel, _ := testVirtualChannel(t)
	channel.BackingVTXOs = append(channel.BackingVTXOs,
		virtualchannel.BackingVTXO{
			OutPoint: wire.OutPoint{
				Hash:  chainhash.HashH([]byte("second")),
				Index: 1,
			},
			Amount: 1,
		},
	)

	_, err := NewBackingExitSpendPolicy(channel, nil)
	require.ErrorContains(t, err, "exactly one backing VTXO")
}

// TestExitSpendPolicyResolverLoadsChannel verifies durable hex refs are used
// to reconstruct virtual-channel backing policies after restart.
func TestExitSpendPolicyResolverLoadsChannel(t *testing.T) {
	t.Parallel()

	channel, targetOutput := testVirtualChannel(t)
	loader := &fakeChannelLoader{
		channel: channel,
	}
	resolver := ExitSpendPolicyResolver{
		Channels: loader,
	}

	policy, err := resolver.ResolveExitSpendPolicy(
		t.Context(), unroll.ExitSpendPolicyRequest{
			Kind: VirtualChannelBackingExitPolicyKind,
			Ref:  EncodeVirtualChannelID(channel.ID),
			StandardDescriptor: &vtxo.Descriptor{
				PkScript: targetOutput.PkScript,
			},
		},
	)
	require.NoError(t, err)
	require.True(
		t, resolver.SupportsKind(
			VirtualChannelBackingExitPolicyKind,
		),
	)
	require.Equal(t, VirtualChannelBackingExitPolicyKind, policy.Kind())
	require.Equal(t, channel.ID, loader.id)
}

type fakeChannelLoader struct {
	id      virtualchannel.ID
	channel *virtualchannel.Channel
}

// GetVirtualChannel implements ChannelLoader.
func (l *fakeChannelLoader) GetVirtualChannel(_ context.Context,
	id virtualchannel.ID) (*virtualchannel.Channel, error) {

	l.id = id

	return l.channel, nil
}

// testVirtualChannel creates a valid single-VTXO virtual channel registration
// and matching materialized target output.
func testVirtualChannel(t *testing.T) (*virtualchannel.Channel, *wire.TxOut) {
	t.Helper()

	var id virtualchannel.ID
	for i := range id {
		id[i] = byte(i + 1)
	}

	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("backing-vtxo")),
		Index: 2,
	}
	backingTx := wire.NewMsgTx(2)
	backingTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: outpoint,
		Witness: wire.TxWitness{
			[]byte{0x01},
			[]byte{0x02},
		},
	})
	backingTx.AddTxOut(&wire.TxOut{
		Value:    90_000,
		PkScript: []byte{0x51},
	})

	targetOutput := &wire.TxOut{
		Value: 100_000,
		PkScript: []byte{
			0x51,
			0x20,
		},
	}
	channel := &virtualchannel.Channel{
		Registration: virtualchannel.Registration{
			ID: id,
			ChannelPoint: wire.OutPoint{
				Hash:  backingTx.TxHash(),
				Index: 0,
			},
			Status:    virtualchannel.StatusActive,
			Capacity:  btcutil.Amount(backingTx.TxOut[0].Value),
			BackingTx: backingTx,
			BackingVTXOs: []virtualchannel.BackingVTXO{
				{
					OutPoint: outpoint,
					Amount: btcutil.Amount(
						targetOutput.Value,
					),
				},
			},
		},
	}

	return channel, targetOutput
}
