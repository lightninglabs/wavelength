package round

import (
	"testing"

	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
)

// TestBuildClientAssetVTXORejectsMissingState prevents incomplete restart
// evidence from being persisted as an ordinary Bitcoin leaf.
func TestBuildClientAssetVTXORejectsMissingState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*boundAssetVTXOFixture)
	}{
		{
			"missing context",
			func(f *boundAssetVTXOFixture) {
				f.tree.AssetContext = nil
			},
		},
		{
			"missing package",
			func(f *boundAssetVTXOFixture) {
				f.tree.AssetContext.SetSealedPackage(
					f.tree.Root.Input, nil,
				)
			},
		},
		{
			"wrong reference",
			func(f *boundAssetVTXOFixture) {
				f.request.AssetRef = "other"
			},
		},
		{
			"wrong units",
			func(f *boundAssetVTXOFixture) {
				f.request.AssetAmount++
			},
		},
		{
			"Bitcoin request",
			func(f *boundAssetVTXOFixture) {
				f.request.AssetRef = ""
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newTestHarness(t)
			fixture := newBoundAssetVTXOFixture(t, h)
			fixture.tree.AssetContext.SetSealedPackage(
				fixture.tree.Root.Input, fixture.sealedPackage,
			)
			test.mutate(fixture)
			signerKey := NewSignerKey(
				fixture.request.SigningKey.PubKey,
			)
			vtxos, err := buildClientVTXOs(
				t.Context(), nil, Intents{
					VTXOs: []types.VTXORequest{
						fixture.request,
					},
				}, map[SignerKey]*tree.Tree{
					signerKey: fixture.tree,
				},
				RoundID{1},
			)
			require.Error(t, err)
			require.Empty(t, vtxos)
		})
	}
}
