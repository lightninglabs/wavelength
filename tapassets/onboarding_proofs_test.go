package tapassets

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// fakeOnboardingProofClient serves tapd wallet UTXOs, transfers, and proof
// exports keyed by anchor outpoint. Like tapd, it omits anchors under an
// active lease, and lists the records in hidden only to an all-types query.
type fakeOnboardingProofClient struct {
	utxos     map[string]*tapsdk.ManagedUtxo
	hidden    map[tapsdk.Outpoint][]*tapsdk.AssetRecord
	transfers []*tapsdk.AssetTransfer
	exported  []tapsdk.Outpoint
}

func (f *fakeOnboardingProofClient) ListUtxos(_ context.Context,
	req *tapsdk.ListUtxosRequest) (map[string]*tapsdk.ManagedUtxo, error) {

	allTypes := req.ScriptKeyType != nil && req.ScriptKeyType.AllTypes
	utxos := make(map[string]*tapsdk.ManagedUtxo, len(f.utxos))
	for key, utxo := range f.utxos {
		if !req.IncludeLeased &&
			utxo.LeaseExpiryUnix > time.Now().Unix() {

			continue
		}
		listed := *utxo
		if allTypes {
			listed.Assets = append(
				slices.Clone(utxo.Assets),
				f.hidden[utxo.OutPoint]...,
			)
		}
		utxos[key] = &listed
	}

	return utxos, nil
}

func (f *fakeOnboardingProofClient) ListTransfers(_ context.Context,
	req *tapsdk.ListTransfersRequest) ([]*tapsdk.AssetTransfer, error) {

	var transfers []*tapsdk.AssetTransfer
	for _, transfer := range f.transfers {
		if transfer.AnchorTxid == req.AnchorTxid {
			transfers = append(transfers, transfer)
		}
	}

	return transfers, nil
}

func (f *fakeOnboardingProofClient) ExportProof(_ context.Context,
	ref tapsdk.AssetRef, _ tapsdk.PubKey, outpoint *tapsdk.Outpoint) (
	*tapsdk.ProofFile, error) {

	if _, ok := ref.AssetID(); !ok {
		return nil, fmt.Errorf("proof export needs an asset ID")
	}
	f.exported = append(f.exported, *outpoint)

	return &tapsdk.ProofFile{
		RawProofFile: []byte(
			fmt.Sprintf("proof-%x-%d", outpoint.Txid[:2],
				outpoint.Index),
		),
	}, nil
}

// proofTestOutpoint returns a distinct outpoint for fixture index i.
func proofTestOutpoint(i byte) tapsdk.Outpoint {
	return tapsdk.Outpoint{Txid: tapsdk.Hash{i}, Index: uint32(i)}
}

// ownedUtxo returns a tapd anchor holding amount units of one issuance.
func ownedUtxo(i byte, issuance tapsdk.AssetID, ref tapsdk.AssetRef,
	amount uint64) *tapsdk.ManagedUtxo {

	return &tapsdk.ManagedUtxo{
		OutPoint: proofTestOutpoint(i),
		Assets: []*tapsdk.AssetRecord{{
			AssetRef: ref,
			Genesis: tapsdk.IssuanceGenesis{
				IssuanceID: issuance,
			},
			Amount: amount,
		}},
	}
}

// TestSelectOnboardingProofs checks which tapd anchors fund an onboarding.
func TestSelectOnboardingProofs(t *testing.T) {
	t.Parallel()

	first := tapsdk.AssetID{1}
	second := tapsdk.AssetID{2}
	ref := tapsdk.AssetRefFromAssetID(first)
	otherRef := tapsdk.AssetRefFromAssetID(second)

	leased := func(utxo *tapsdk.ManagedUtxo,
		expiry time.Duration) *tapsdk.ManagedUtxo {

		utxo.LeaseOwner = []byte("tapd")
		utxo.LeaseExpiryUnix = time.Now().Add(expiry).Unix()

		return utxo
	}

	tests := []struct {
		name    string
		utxos   []*tapsdk.ManagedUtxo
		hidden  map[tapsdk.Outpoint][]*tapsdk.AssetRecord
		ref     tapsdk.AssetRef
		amount  uint64
		want    []byte
		wantErr string
	}{{
		name: "smallest covering anchor",
		utxos: []*tapsdk.ManagedUtxo{
			ownedUtxo(1, first, ref, 300),
			ownedUtxo(2, first, ref, 700),
			ownedUtxo(3, first, ref, 500),
		},
		ref:    ref,
		amount: 400,
		want: []byte{
			3,
		},
	}, {
		name: "largest anchors accumulate",
		utxos: []*tapsdk.ManagedUtxo{
			ownedUtxo(1, first, ref, 100),
			ownedUtxo(2, first, ref, 300),
			ownedUtxo(3, first, ref, 400),
		},
		ref:    ref,
		amount: 600,
		want: []byte{
			2,
			3,
		},
	}, {
		name: "other assets and shared anchors skipped",
		utxos: []*tapsdk.ManagedUtxo{
			ownedUtxo(1, second, otherRef, 900),
			{
				OutPoint: proofTestOutpoint(2),
				Assets: []*tapsdk.AssetRecord{
					ownedUtxo(0, first, ref, 900).Assets[0],
					ownedUtxo(0, second, otherRef, 1).
						Assets[0],
				},
			},
			ownedUtxo(3, first, ref, 500),
		},
		ref:    ref,
		amount: 500,
		want: []byte{
			3,
		},
	}, {
		name: "anchor sharing a non-BIP-86 record skipped",
		utxos: []*tapsdk.ManagedUtxo{
			ownedUtxo(1, first, ref, 500),
			ownedUtxo(2, first, ref, 900),
		},
		hidden: map[tapsdk.Outpoint][]*tapsdk.AssetRecord{
			proofTestOutpoint(1): ownedUtxo(0, second, otherRef, 1).
				Assets,
		},
		ref:    ref,
		amount: 400,
		want: []byte{
			2,
		},
	}, {
		name: "leased anchor skipped",
		utxos: []*tapsdk.ManagedUtxo{
			leased(ownedUtxo(1, first, ref, 900), time.Hour),
		},
		ref:     ref,
		amount:  500,
		wantErr: "no tapd UTXO holds the asset alone",
	}, {
		name: "expired lease selected",
		utxos: []*tapsdk.ManagedUtxo{
			leased(ownedUtxo(1, first, ref, 900), -time.Hour),
		},
		ref:    ref,
		amount: 500,
		want: []byte{
			1,
		},
	}, {
		name: "insufficient",
		utxos: []*tapsdk.ManagedUtxo{
			ownedUtxo(1, first, ref, 100),
			ownedUtxo(2, first, ref, 200),
		},
		ref:     ref,
		amount:  500,
		wantErr: "one issuance holds at most 300 units, need 500",
	}, {
		name: "zero amount",
		utxos: []*tapsdk.ManagedUtxo{
			ownedUtxo(1, first, ref, 100),
		},
		ref:     ref,
		wantErr: "onboarding amount is required",
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			client := &fakeOnboardingProofClient{
				utxos:  make(map[string]*tapsdk.ManagedUtxo),
				hidden: test.hidden,
			}
			for idx, utxo := range test.utxos {
				client.utxos[fmt.Sprint(idx)] = utxo
			}

			proofs, err := SelectOnboardingProofs(
				t.Context(), client, test.ref.String(),
				test.amount,
			)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)

				return
			}
			require.NoError(t, err)

			want := make([]tapsdk.Outpoint, 0, len(test.want))
			for _, idx := range test.want {
				want = append(want, proofTestOutpoint(idx))
			}
			require.Equal(t, want, client.exported)
			require.Len(t, proofs, len(want))
		})
	}
}

// TestSelectOnboardingProofsOneIssuance checks that grouped tranches never
// fund one onboarding together, and that the input bound holds.
func TestSelectOnboardingProofsOneIssuance(t *testing.T) {
	t.Parallel()

	first, second := tapsdk.AssetID{1}, tapsdk.AssetID{2}

	tranches := []ownedAssetUtxo{
		{
			outpoint:   proofTestOutpoint(1),
			issuanceID: first,
			amount:     300,
		},
		{
			outpoint:   proofTestOutpoint(2),
			issuanceID: second,
			amount:     300,
		},
	}
	_, err := selectOwnedAssetUtxos(tranches, 500)
	require.ErrorContains(t, err, "one issuance holds at most 300 units")

	tranches = append(tranches, ownedAssetUtxo{
		outpoint: proofTestOutpoint(3), issuanceID: second, amount: 200,
	})
	selected, err := selectOwnedAssetUtxos(tranches, 500)
	require.NoError(t, err)
	require.Len(t, selected, 2)
	for _, utxo := range selected {
		require.Equal(t, second, utxo.issuanceID)
	}

	dust := make([]ownedAssetUtxo, 0, MaxTransitionInputs+1)
	for idx := range MaxTransitionInputs + 1 {
		dust = append(dust, ownedAssetUtxo{
			outpoint:   proofTestOutpoint(byte(idx)),
			issuanceID: first,
			amount:     1,
		})
	}
	_, err = selectOwnedAssetUtxos(dust, MaxTransitionInputs)
	require.NoError(t, err)
	_, err = selectOwnedAssetUtxos(dust, MaxTransitionInputs+1)
	require.ErrorIs(t, err, ErrTooManyTransitionInputs)

	// A later issuance that covers the amount with one anchor funds it.
	whole := ownedAssetUtxo{
		outpoint:   proofTestOutpoint(0xff),
		issuanceID: second,
		amount:     MaxTransitionInputs + 1,
	}
	selected, err = selectOwnedAssetUtxos(
		append(dust, whole), MaxTransitionInputs+1,
	)
	require.NoError(t, err)
	require.Equal(t, []ownedAssetUtxo{whole}, selected)
}

// TestExportOnboardedProof checks that the proof of an onboarding output is
// exported only once its transfer confirms.
func TestExportOnboardedProof(t *testing.T) {
	t.Parallel()

	issuance := tapsdk.AssetID{7}
	txid := chainhash.Hash{9}
	outpoint := wire.OutPoint{Hash: txid, Index: 0}
	transfer := &tapsdk.AssetTransfer{
		AnchorTxid: txid.String(),
		Outputs: []tapsdk.TransferOutput{{
			AnchorOutpoint: sdkOutpoint(wire.OutPoint{
				Hash: txid, Index: 1,
			}),
			IssuanceID: issuance,
		}, {
			AnchorOutpoint: sdkOutpoint(outpoint),
			IssuanceID:     issuance,
		}},
	}
	client := &fakeOnboardingProofClient{
		transfers: []*tapsdk.AssetTransfer{
			transfer,
		},
	}

	_, _, err := ExportOnboardedProof(t.Context(), client, outpoint)
	require.ErrorIs(t, err, ErrOnboardedProofPending)
	require.Empty(t, client.exported)

	transfer.AnchorTxBlockHash = [32]byte{1}
	transfer.AnchorTxBlockHeight = 120
	proof, height, err := ExportOnboardedProof(
		t.Context(), client, outpoint,
	)
	require.NoError(t, err)
	require.NotEmpty(t, proof)
	require.EqualValues(t, 120, height)
	require.Equal(
		t, []tapsdk.Outpoint{sdkOutpoint(outpoint)}, client.exported,
	)

	_, _, err = ExportOnboardedProof(
		t.Context(), client, wire.OutPoint{
			Hash: chainhash.Hash{8},
		},
	)
	require.ErrorContains(t, err, "no tapd transfer creates")

	transfer.Outputs = append(transfer.Outputs, tapsdk.TransferOutput{
		AnchorOutpoint: sdkOutpoint(outpoint),
		IssuanceID:     tapsdk.AssetID{8},
	})
	_, _, err = ExportOnboardedProof(t.Context(), client, outpoint)
	require.ErrorContains(t, err, "holds more than one asset")
}
