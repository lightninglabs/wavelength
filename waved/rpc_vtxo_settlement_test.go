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
// only for a FORFEITED descriptor carrying a settlement. Internal checkpoint
// data on a FORFEITING descriptor must not widen the public RPC contract.
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

	t.Run("forfeiting checkpoint stays private", func(t *testing.T) {
		t.Parallel()

		desc := base(vtxo.VTXOStatusForfeiting)
		desc.Settlement = fn.Some(vtxo.Settlement{
			TxID: settlementTxid,
		})

		got := descriptorToProto(desc)
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
