package waved

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestDescriptorToProtoSettlement verifies the settlement message is emitted
// for a FORFEITED descriptor carrying a settlement, and stays nil for a
// descriptor with no settlement (a LIVE VTXO or an old daemon), so absence is
// explicit on the wire and the RPC surface degrades to prior behavior (#924).
func TestDescriptorToProtoSettlement(t *testing.T) {
	t.Parallel()

	settlementTxid := chainhash.HashH([]byte("settlement-commitment-tx"))
	const settlementHeight = int32(812345)

	base := func(status vtxo.VTXOStatus) *vtxo.Descriptor {
		return &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash:  chainhash.HashH([]byte("vtxo")),
				Index: 1,
			},
			Amount: 50_000,
			Status: status,
		}
	}

	t.Run("forfeited with settlement", func(t *testing.T) {
		t.Parallel()

		desc := base(vtxo.VTXOStatusForfeited)
		desc.Settlement = fn.Some(vtxo.Settlement{
			TxID:   settlementTxid,
			Height: settlementHeight,
		})

		got := descriptorToProto(desc)
		require.NotNil(t, got.GetSettlement())
		require.Equal(
			t, settlementTxid.String(),
			got.GetSettlement().GetTxid(),
		)
		require.Equal(
			t, settlementHeight, got.GetSettlement().GetHeight(),
		)
	})

	t.Run("live without settlement", func(t *testing.T) {
		t.Parallel()

		got := descriptorToProto(base(vtxo.VTXOStatusLive))
		require.Nil(t, got.GetSettlement())
	})

	t.Run("forfeited old daemon leaves nil", func(t *testing.T) {
		t.Parallel()

		// A forfeited descriptor whose forfeit round row was absent (or
		// an old daemon) has a None Settlement: the proto message must
		// stay nil rather than emit an all-zero settlement.
		got := descriptorToProto(base(vtxo.VTXOStatusForfeited))
		require.Nil(t, got.GetSettlement())
	})
}

// TestDescriptorToProtoTaprootAsset separates the Bitcoin carrier value from
// the nested SDK-neutral asset quantity and keeps ordinary VTXOs free of an
// asset sub-message.
func TestDescriptorToProtoTaprootAsset(t *testing.T) {
	t.Parallel()

	const carrierSats = 546
	root := chainhash.HashH([]byte("vtxo-asset-root"))
	desc := &vtxo.Descriptor{
		Outpoint: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("asset-vtxo")),
			Index: 2,
		},
		Amount:             carrierSats,
		Status:             vtxo.VTXOStatusLive,
		TaprootAssetRoot:   &root,
		TaprootAssetRef:    "asset:rpc-projection",
		TaprootAssetAmount: ^uint64(0),
	}

	got := descriptorToProto(desc)
	require.EqualValues(t, carrierSats, got.GetAmountSat())
	require.NotNil(t, got.GetTaprootAsset())
	require.Equal(
		t, desc.TaprootAssetRef, got.GetTaprootAsset().GetAssetRef(),
	)
	require.Equal(
		t, desc.TaprootAssetAmount, got.GetTaprootAsset().GetAmount(),
	)
	require.Equal(
		t, root.CloneBytes(), got.GetTaprootAsset().GetCommitmentRoot(),
	)

	ordinary := &vtxo.Descriptor{
		Outpoint: wire.OutPoint{
			Hash: chainhash.HashH([]byte("bitcoin-vtxo")),
		},
		Amount: carrierSats,
		Status: vtxo.VTXOStatusLive,
	}
	require.Nil(t, descriptorToProto(ordinary).GetTaprootAsset())

	// Historical rows written before semantic asset metadata existed still
	// expose their commitment root without inventing an identity or amount.
	legacy := &vtxo.Descriptor{
		Amount:           carrierSats,
		TaprootAssetRoot: &root,
	}
	legacyAsset := descriptorToProto(legacy).GetTaprootAsset()
	require.NotNil(t, legacyAsset)
	require.Empty(t, legacyAsset.GetAssetRef())
	require.Zero(t, legacyAsset.GetAmount())
	require.Equal(t, root.CloneBytes(), legacyAsset.GetCommitmentRoot())
}
