package waved

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/internal/expiryfixture"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// laterHeightBackend supplies confirmation evidence without replacing the
// real indexer validation or database transaction exercised by the test.
type laterHeightBackend struct {
	heightOnlyChainBackend
	register func(context.Context, *chainhash.Hash, []byte, uint32,
		uint32, bool) (*chainsource.ConfRegistration, error)
}

// RegisterConf delegates to the test's bounded confirmation scenario.
func (b *laterHeightBackend) RegisterConf(ctx context.Context,
	txid *chainhash.Hash, script []byte, confs, hint uint32,
	includeBlock bool) (*chainsource.ConfRegistration, error) {

	return b.register(ctx, txid, script, confs, hint, includeBlock)
}

// TestRepairLegacyCommitmentHeightsIsolatesConfirmationWait proves an
// unconfirmable first target leaves the pass alive to repair the next target.
// Above-tip claims must not register a watch, and silent watches must be
// cancelled on a shorter deadline without changing the pending descriptor.
func TestRepairLegacyCommitmentHeightsIsolatesConfirmationWait(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"above tip", "silent watch"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			s, rpc, descs := newCommitmentRepairServer(
				t, 2,
				clock.NewTestClock(
					time.Now(),
				),
			)
			// The live target is visited before the exiting target.
			first := descs[0]
			commitmentID := first.Ancestry[0].CommitmentTxID
			before, err := s.vtxoStore.GetVTXO(
				t.Context(), first.Outpoint,
			)
			require.NoError(t, err)
			candidate := rpc.byScript[string(first.PkScript)]
			candidate.AncestryPaths[0].CommitmentHeight = 326
			tip := int32(500)
			if scenario == "above tip" {
				tip = 325
			}
			var calls, cleanups int
			s.chainBackend = &laterHeightBackend{
				heightOnlyChainBackend: heightOnlyChainBackend{
					height: tip,
				},
				register: func(_ context.Context,
					txid *chainhash.Hash, _ []byte, _,
					_ uint32, _ bool) (
					*chainsource.ConfRegistration, error) {

					calls++
					require.Equal(t, commitmentID, *txid)
					type conf = chainsource.TxConfirmation

					return &chainsource.ConfRegistration{
						Confirmed: make(chan *conf),
						Cancel: func() {
							cleanups++
						},
					}, nil
				},
			}
			ctx, cancel := context.WithTimeout(
				t.Context(), 5*time.Second,
			)
			defer cancel()
			result, err := s.
				repairLegacyCommitmentHeightsWithTimeout(
					ctx, fastCommitmentRepairPage,
					20*time.Millisecond,
				)
			require.Error(t, err)
			require.NoError(
				t, ctx.Err(),
				"first target exhausted pass",
			)
			require.Equal(t, 2, result.candidates)
			require.Equal(t, 1, result.completed)
			if scenario == "above tip" {
				require.ErrorContains(
					t, err, "above local best height",
				)
				require.Zero(t, calls)
			} else {
				require.ErrorIs(
					t, err, context.DeadlineExceeded,
				)
				require.Equal(t, 1, calls)
			}
			require.Equal(t, calls, cleanups)
			unchanged, err := s.vtxoStore.GetVTXO(
				t.Context(), first.Outpoint,
			)
			require.NoError(t, err)
			require.Equal(t, before, unchanged)
			second, err := s.vtxoStore.GetVTXO(
				t.Context(), descs[1].Outpoint,
			)
			require.NoError(t, err)
			require.Equal(
				t, int32(321),
				second.Ancestry[0].CommitmentHeight,
			)
		})
	}
}

// TestRepairLegacyCommitmentHeightsLaterConfirmation reproduces a commitment
// confirming five blocks after the stored creation height. Only matching local
// chain evidence permits the repair; failed evidence preserves every field and
// a later pass can recover without a manual database edit.
func TestRepairLegacyCommitmentHeightsLaterConfirmation(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{
		"later confirmation", "wrong height", "wrong transaction",
		"lookup failure", "cancelled lookup",
	} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			s, rpc, descs := newCommitmentRepairServer(
				t, 1,
				clock.NewTestClock(
					time.Now(),
				),
			)
			desc := descs[0]
			before, err := s.vtxoStore.GetVTXO(
				t.Context(), desc.Outpoint,
			)
			require.NoError(t, err)
			candidate := rpc.byScript[string(desc.PkScript)]
			candidate.AncestryPaths[0].CommitmentHeight = 326
			_, commitment := expiryfixture.Round(
				t, 10000, desc.PkScript, 50, 326, 1,
			)
			require.Equal(
				t, desc.Ancestry[0].CommitmentTxID,
				commitment.TxHash(),
			)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls, cleanups int
			mode := scenario
			s.chainBackend = &laterHeightBackend{
				heightOnlyChainBackend: heightOnlyChainBackend{
					height: 500,
				},
				register: func(_ context.Context,
					txid *chainhash.Hash, script []byte,
					confs, hint uint32, includeBlock bool) (
					*chainsource.ConfRegistration, error) {

					calls++
					require.Equal(
						t, commitment.TxHash(), *txid,
					)
					require.Equal(
						t, desc.Ancestry[0].TreePath.
							BatchOutput.PkScript,
						script,
					)
					require.Equal(t, uint32(1), confs)
					require.Equal(t, uint32(321), hint)
					require.False(t, includeBlock)
					if mode == "lookup failure" {
						return nil, fmt.Errorf(
							"backend unavailable")
					}
					type conf = chainsource.TxConfirmation
					confirmed := make(chan *conf, 1)
					evidence := &conf{
						Tx:          commitment,
						BlockHeight: 326,
					}
					switch mode {
					case "wrong height":
						evidence.BlockHeight = 320

					case "wrong transaction":
						evidence.Tx = commitment.Copy()
						evidence.Tx.LockTime++

					case "cancelled lookup":
						cancel()
					}
					if mode != "cancelled lookup" {
						confirmed <- evidence
					}

					return &chainsource.ConfRegistration{
						Confirmed: confirmed,
						Cancel: func() {
							cleanups++
						},
					}, nil
				},
			}
			result, err := s.repairLegacyCommitmentHeights(
				ctx, fastCommitmentRepairPage,
			)
			require.Equal(t, 1, calls)
			if scenario == "lookup failure" {
				require.Zero(t, cleanups)
			} else {
				require.Equal(t, 1, cleanups)
			}
			if scenario != "later confirmation" {
				require.Error(t, err)
				require.Zero(t, result.completed)
				unchanged, getErr := s.vtxoStore.GetVTXO(
					t.Context(), desc.Outpoint,
				)
				require.NoError(t, getErr)
				require.Equal(t, before, unchanged)

				mode = "later confirmation"
				result, err = s.repairLegacyCommitmentHeights(
					t.Context(), fastCommitmentRepairPage,
				)
			}
			require.NoError(t, err)
			require.Equal(t, 1, result.completed)
			stored, err := s.vtxoStore.GetVTXO(
				t.Context(), desc.Outpoint,
			)
			require.NoError(t, err)
			require.Equal(
				t, int32(326),
				stored.Ancestry[0].CommitmentHeight,
			)
			stored.Ancestry[0].CommitmentHeight = 0
			require.Equal(t, before, stored)

			// A replay does not query either remote dependency
			// again.
			previousCalls := calls
			result, err = s.repairLegacyCommitmentHeights(
				t.Context(), fastCommitmentRepairPage,
			)
			require.NoError(t, err)
			require.Zero(t, result.candidates)
			require.Equal(t, previousCalls, calls)
		})
	}
}
