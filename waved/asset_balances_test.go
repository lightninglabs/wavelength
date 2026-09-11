package waved

import (
	"math"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/google/uuid"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// assetHolding creates a locally persisted asset VTXO with a composed script.
func assetHolding(t *testing.T, id byte, amount uint64) *vtxo.Descriptor {
	t.Helper()
	d := newRefreshEstimateVTXO(t, id, 1000, 900)
	root := chainhash.Hash{id}
	d.TaprootAssetRoot = &root
	d.TaprootAssetRef = tapsdk.
		AssetRefFromAssetID(tapsdk.AssetID{id}).
		String()
	d.TaprootAssetAmount = amount
	d.TaprootAssetSealedPackage = []byte{1}
	var err error
	d.PkScript, err = d.EffectivePkScript()
	require.NoError(t, err)

	return d
}

// TestLiveAssetBalances separates assets from carrier sats and Bitcoin, and
// excludes pending/terminal holdings without truncating uint64 quantities.
func TestLiveAssetBalances(t *testing.T) {
	t.Parallel()
	first := assetHolding(t, 1, math.MaxUint64)
	second := assetHolding(t, 2, 23)
	more := assetHolding(t, 2, 17)
	pending := assetHolding(t, 1, 1)
	pending.Status = vtxo.VTXOStatusSpending
	bitcoin := newRefreshEstimateVTXO(t, 3, 5000, 900)
	holdings := []*vtxo.Descriptor{second, bitcoin, pending, first, more}
	balances, err := liveAssetBalances(holdings)
	require.NoError(t, err)
	require.Len(t, balances, 2)
	require.Less(t, balances[0].AssetRef, balances[1].AssetRef)
	byRef := map[string]*waverpc.AssetBalance{}
	for _, balance := range balances {
		byRef[balance.AssetRef] = balance
	}
	require.Equal(
		t, uint64(math.MaxUint64), byRef[first.TaprootAssetRef].Amount,
	)
	require.EqualValues(t, 1000, byRef[first.TaprootAssetRef].CarrierSat)
	require.EqualValues(t, 40, byRef[second.TaprootAssetRef].Amount)
	require.EqualValues(t, 2000, byRef[second.TaprootAssetRef].CarrierSat)
	require.EqualValues(t, 5000, vtxo.SumSpendableBalance(holdings))
	encoded, err := protojson.Marshal(
		&waverpc.GetBalanceResponse{
			AssetBalances: balances,
		},
	)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"18446744073709551615"`)
}

// TestLiveAssetBalanceErrors rejects corrupt records and arithmetic overflow.
func TestLiveAssetBalanceErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*vtxo.Descriptor)
	}{
		{
			"root",
			func(d *vtxo.Descriptor) {
				d.TaprootAssetRoot = nil
			},
		},
		{
			"reference",
			func(d *vtxo.Descriptor) {
				d.TaprootAssetRef = ""
			},
		},
		{
			"encoding",
			func(d *vtxo.Descriptor) {
				d.TaprootAssetRef = "invalid"
			},
		},
		{
			"amount",
			func(d *vtxo.Descriptor) {
				d.TaprootAssetAmount = 0
			},
		},
		{
			"carrier",
			func(d *vtxo.Descriptor) {
				d.Amount = -1
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d := assetHolding(t, 1, 1)
			test.mutate(d)
			_, err := liveAssetBalances([]*vtxo.Descriptor{d})
			require.Error(t, err)
		})
	}
	_, err := liveAssetBalances([]*vtxo.Descriptor{
		assetHolding(t, 1, math.MaxUint64), assetHolding(t, 1, 1),
	})
	require.ErrorContains(t, err, "overflow")
	carrier := assetHolding(t, 1, 1)
	carrier.Amount = math.MaxInt64
	_, err = liveAssetBalances(
		[]*vtxo.Descriptor{carrier, assetHolding(t, 1, 1)},
	)
	require.ErrorContains(t, err, "overflow")
}

// TestListAssetVTXOs preserves metadata across SQL reload and pending rounds,
// applying the same reference filter to both sources.
func TestListAssetVTXOs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	stored := assetHolding(t, 1, math.MaxUint64)
	store := newListVTXOsStore(t)
	require.NoError(t, store.SaveVTXO(ctx, stored))
	require.NoError(t, store.SaveVTXO(ctx, assetHolding(t, 2, 100)))
	require.NoError(
		t,
		store.SaveVTXO(
			ctx, newRefreshEstimateVTXO(t, 3, 1000, 900),
		),
	)
	roundID := round.RoundID(uuid.New())
	request := types.VTXORequest{
		Amount:      2000,
		OwnerKey:    localOwnerKey(t),
		AssetRef:    stored.TaprootAssetRef,
		AssetAmount: 42,
	}
	state := &round.InputSigSentState{
		RoundID: roundID,
		Intents: round.Intents{
			VTXOs: []types.VTXORequest{
				request,
			},
		},
	}
	system := newRoundActorWithStates(t, map[string]round.FSMStateInfo{
		roundID.String(): {RoundID: roundID, State: state},
	})
	defer func() {
		require.NoError(t, system.Shutdown(ctx))
	}()
	server := newListVTXOsServer(store, system)
	req := &waverpc.ListVTXOsRequest{
		AssetRef: stored.TaprootAssetRef,
		Statuses: []waverpc.VTXOStatus{
			waverpc.VTXOStatus_VTXO_STATUS_LIVE,
			waverpc.VTXOStatus_VTXO_STATUS_PENDING_ROUND,
		},
	}
	result, err := server.ListVTXOs(ctx, req)
	require.NoError(t, err)
	require.Len(t, result.Vtxos, 2)
	require.EqualValues(t, 42, result.Vtxos[0].AssetAmount)
	require.Equal(t, stored.TaprootAssetRef, result.Vtxos[0].AssetRef)
	require.Equal(t, uint64(math.MaxUint64), result.Vtxos[1].AssetAmount)
	require.Len(t, result.Vtxos[1].AssetRoot, 64)
	req.MinAmountSat = 1500
	result, err = server.ListVTXOs(ctx, req)
	require.NoError(t, err)
	require.Len(t, result.Vtxos, 1)
	require.EqualValues(t, 42, result.Vtxos[0].AssetAmount)
	for _, invalid := range []string{
		"invalid",
		strings.ToUpper(stored.TaprootAssetRef),
	} {
		req.AssetRef = invalid
		_, err := server.ListVTXOs(ctx, req)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
}
