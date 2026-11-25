package waved

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/internal/expiryfixture"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestArkChannelRecoveryAuthenticatesFunding rejects structurally resolvable
// but unauthorized funding and requires confirmation evidence from our chain
// source rather than the peer's expiry claim.
func TestArkChannelRecoveryAuthenticatesFunding(t *testing.T) {
	t.Parallel()
	type confirmation = chainsource.TxConfirmation

	for _, scenario := range []string{
		"valid", "wrong value", "wrong script", "unconfirmed",
		"wrong anchor",
	} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			candidate, commitment := expiryfixture.Round(
				t, 100_000, []byte{0x51}, 200, 100, 73,
			)
			ancestry, err := vtxo.AncestryFromRPC(
				candidate.AncestryPaths,
			)
			require.NoError(t, err)
			ancestry[0].CommitmentHeight = 1_000_000
			var target chainhash.Hash
			copy(target[:], candidate.Outpoint.Txid)
			desc := &vtxo.Descriptor{
				Amount:      btcutil.Amount(candidate.ValueSat),
				PkScript:    candidate.PkScript,
				Ancestry:    ancestry,
				BatchExpiry: 1_000_000,
			}
			desc.Outpoint.Hash = target
			desc.Outpoint.Index = candidate.Outpoint.Vout
			if scenario == "wrong value" {
				desc.Amount++
			}
			if scenario == "wrong script" {
				desc.PkScript = []byte{0x52}
			}
			lookups, cancellations := 0, 0
			backend := &laterHeightBackend{
				register: func(context.Context, *chainhash.Hash,
					[]byte, uint32, uint32, bool) (
					*chainsource.ConfRegistration, error) {

					lookups++
					if scenario == "unconfirmed" {
						return nil, fmt.Errorf(
							"not confirmed")
					}
					confirmed := commitment.Copy()
					if scenario == "wrong anchor" {
						confirmed.TxOut[0].Value++
					}
					ch := make(chan *confirmation, 1)
					ch <- &chainsource.TxConfirmation{
						Tx: confirmed, BlockHeight: 100,
					}

					return &chainsource.ConfRegistration{
						Confirmed: ch,
						Cancel: func() {
							cancellations++
						},
					}, nil
				},
			}
			err = validateArkChannelRecoveryProof(
				t.Context(), desc, arkchannel.RecoveryPackage{},
				backend,
			)
			if scenario == "valid" {
				require.NoError(t, err)
				require.Equal(t, int32(300), desc.BatchExpiry)
				require.Equal(
					t, int32(100),
					desc.Ancestry[0].CommitmentHeight,
				)
				require.Equal(t, 1, cancellations)

				return
			}
			require.Error(t, err)
			switch scenario {
			case "wrong value", "wrong script":
				require.Zero(t, lookups)
			}
		})
	}
}

// TestArkChannelRecoveryWindow rejects real transaction dependencies that
// cannot materialize within the allowance used by native HTLC publication.
func TestArkChannelRecoveryWindow(t *testing.T) {
	t.Parallel()
	timeFlag := uint32(wire.SequenceLockTimeIsSeconds)
	for _, scenario := range []string{
		"exact", "short", "relative lock", "absolute lock", "time lock",
	} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			root := wire.NewMsgTx(2)
			root.AddTxIn(
				&wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{7},
					},
					Sequence: wire.MaxTxInSequenceNum,
				},
			)
			root.AddTxOut(
				&wire.TxOut{
					Value:    100,
					PkScript: []byte{0x51},
				},
			)
			target := wire.NewMsgTx(2)
			target.AddTxIn(
				&wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash: root.TxHash(),
					},
					Sequence: 1,
				},
			)
			target.AddTxOut(root.TxOut[0])
			allowance := uint32(13)
			switch scenario {
			case "short":
				allowance--

			case "relative lock":
				target.TxIn[0].Sequence = 20

			case "absolute lock":
				target.LockTime = 200

			case "time lock":
				target.TxIn[0].Sequence |= timeFlag
			}
			proof, err := recovery.NewProof(
				wire.OutPoint{
					Hash: target.TxHash(),
				},
				10, &recovery.Node{
					Tx: root,
				}, &recovery.Node{
					Tx: target,
				},
			)
			require.NoError(t, err)
			err = validateArkChannelRecoveryWindow(
				proof, allowance, 100,
			)
			if scenario == "exact" {
				require.NoError(t, err)

				return
			}
			require.Error(t, err)
		})
	}
}
