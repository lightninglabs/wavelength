package unroll

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestAssetForeignTargetSpendTerminalizes verifies that asset sweep safety
// does not prevent a foreign target spend from failing the real child actor
// and reaching the registry's terminal persistence and manager feedback paths.
func TestAssetForeignTargetSpendTerminalizes(t *testing.T) {
	tests := []struct {
		name        string
		trigger     StartTrigger
		recoverable bool
	}{
		{
			name:    "materializing",
			trigger: TriggerManual,
		},
		{
			name:        "deferredCheckpoint",
			trigger:     TriggerFraudSpend,
			recoverable: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proof := buildOORProof(t)
			target := proof.TargetOutpoint()
			desc := testDescriptor(t, target, proof.CSVDelay())
			assetRoot := chainhash.Hash{1}
			desc.TaprootAssetRoot = &assetRoot

			observer := &captureExitObserver{}
			observerRef := actor.TellOnlyRef[vtxo.ManagerMsg](
				observer,
			)
			store := newMemRegistryStore()
			checkpoints := newMemCheckpointStore()
			txconfirmRef := &fakeTxConfirmRef{}
			chainSource := &fakeChainSourceRef{}
			registry := newRegistryHarnessWithSpawn(
				t, RegistryConfig{
					Store:         store,
					DeliveryStore: checkpoints,
					ProofAssembler: &mockProofAssembler{
						proof: proof,
					},
					VTXOStore: &mockVTXOStore{
						desc: desc,
					},
					TxConfirmRef:     txconfirmRef,
					ChainSource:      chainSource,
					Wallet:           &fakeSweepWallet{},
					VTXOExitObserver: fn.Some(observerRef),
				},
			)
			t.Cleanup(registry.Stop)

			_, err := registry.Ref().Ask(
				t.Context(), &EnsureUnrollRequest{
					Outpoint: target,
					Trigger:  test.trigger,
				},
			).Await(t.Context()).Unpack()
			require.NoError(t, err)
			require.Contains(
				t, chainSource.spendRegistrations(), target,
			)

			checkpoint := mustDecodeCheckpoint(
				t, checkpoints, actorIDForTarget(target),
			)
			require.True(t, checkpoint.Started)
			require.Equal(
				t, StandardVTXOTimeoutExitPolicyKind,
				checkpoint.ExitPolicyKind,
			)
			if test.recoverable {
				// Deferral supplies a real active exit with no
				// local broadcasts, so failure must reach the
				// manager.
				require.Len(
					t, checkpoint.DeferredCheckpoints, 1,
				)
				require.Zero(t, txconfirmRef.requestCount())
			} else {
				require.Equal(t, 1, txconfirmRef.requestCount())
			}

			externalTxid := chainhash.Hash{0xee}
			chainSource.emitSpendForOutpoint(
				t, target, externalTxid, 101,
			)
			require.Eventually(t, func() bool {
				record, err := store.GetRecord(
					t.Context(), target,
				)
				require.NoError(t, err)

				if record == nil {
					return false
				}

				return record.Phase == PhaseFailed
			}, testTimeout, 10*time.Millisecond)

			record, err := store.GetRecord(t.Context(), target)
			require.NoError(t, err)
			require.Contains(
				t, record.FailReason, "spent externally",
			)
			require.Contains(t, record.FailReason, target.String())
			require.Contains(
				t, record.FailReason, externalTxid.String(),
			)
			require.Contains(t, record.FailReason, "height 101")
			require.Equal(
				t, test.recoverable, record.RecoverableFailure,
			)
			require.False(t, record.ConflictedFailure)

			checkpoint = mustDecodeCheckpoint(
				t, checkpoints, actorIDForTarget(target),
			)
			require.Equal(t, record.FailReason, checkpoint.Fail)
			require.Nil(t, checkpoint.SweepTx)
			if !test.recoverable {
				require.Empty(t, observer.notifications())

				return
			}

			require.Eventually(t, func() bool {
				return len(observer.notifications()) == 1
			}, testTimeout, 10*time.Millisecond)
			note := observer.notifications()[0]
			require.Equal(t, target, note.Outpoint)
			require.Equal(
				t, vtxo.ExitOutcomeRecoverable, note.Outcome,
			)
			require.Equal(t, record.FailReason, note.Reason)
			require.Equal(
				t, actormsg.ExitPolicyKind(
					StandardVTXOTimeoutExitPolicyKind,
				),
				note.ExitPolicyKind,
			)
		})
	}
}
