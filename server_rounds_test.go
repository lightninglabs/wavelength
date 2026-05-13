package darepo

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestMaxConnectorBroadcastTxs verifies the worst-case connector
// broadcast count the fraud-response startup gate sizes the exit delay
// against. Expected values track tree.Tree.Depth() for the shape
// rounds/fsm_transitions.go produces from these (maxConnectors, radix)
// pairs: the leaf transaction itself is one of the broadcasts, so a
// single-leaf tree costs one tx and the count is ceil(log_radix(N))+1
// for N >= 2.
func TestMaxConnectorBroadcastTxs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		maxConnectors uint32
		radix         uint32
		want          uint32
	}{{
		name: "zero max", maxConnectors: 0, radix: 2, want: 0,
	}, {
		// One leaf is itself one broadcast tx that spends the
		// commitment-tx connector output directly. The prior helper
		// returned 0 here, which undercounted the chain by one.
		name: "one leaf", maxConnectors: 1, radix: 2, want: 1,
	}, {
		// Root + leaf = two broadcasts.
		name:          "two leaves binary",
		maxConnectors: 2, radix: 2, want: 2,
	}, {
		// ceil(log_2(32)) = 5 branch tiers, plus the leaf tier.
		name:          "32 leaves binary",
		maxConnectors: 32, radix: 2, want: 6,
	}, {
		name:          "33 leaves binary",
		maxConnectors: 33, radix: 2, want: 7,
	}, {
		// ceil(log_4(256)) = 4 branch tiers, plus the leaf tier.
		name:          "256 leaves quaternary",
		maxConnectors: 256, radix: 4, want: 5,
	}, {
		name: "bad radix 0", maxConnectors: 32, radix: 0, want: 0,
	}, {
		name: "bad radix 1", maxConnectors: 32, radix: 1, want: 0,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := maxConnectorBroadcastTxs(
				tc.maxConnectors, tc.radix,
			)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestCheckFraudResponseSafetyMargin verifies that the fraud-response
// startup gate sizes the connector path against ConnectorTreeRadix
// rather than the (unrelated) VTXO TreeRadix.
//
// Regression test for darepo#374: a configuration where TreeRadix is
// larger than ConnectorTreeRadix would, under the buggy call, look safe
// (the gate computed an over-generous depth using TreeRadix) while the
// real connector tree — built with ConnectorTreeRadix — is taller and
// races the client's CSV exit.
func TestCheckFraudResponseSafetyMargin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string

		// terms drives the gate. VTXOExitDelay is what the gate is
		// trying to validate against the connector broadcast count.
		terms *batch.Terms

		// wantErr is true when the gate must reject the configuration.
		wantErr bool
	}{{
		// Baseline: a generous VTXOExitDelay easily clears the
		// connector path plus the 6-block safety margin. With
		// maxConns=32, radix=4 the broadcast count is
		// ceil(log_4(32))+1 = 4, so minExitDelay = 4+6 = 10.
		name: "exit delay clears connector depth",
		terms: &batch.Terms{
			MaxConnectorsPerTree: 32,
			TreeRadix:            2,
			ConnectorTreeRadix:   4,
			VTXOExitDelay:        144,
		},
		wantErr: false,
	}, {
		// Exit delay sits exactly on the boundary (minExitDelay=10);
		// the gate uses strict greater-than so this must be rejected.
		name: "exit delay equals connector depth plus margin",
		terms: &batch.Terms{
			MaxConnectorsPerTree: 32,
			TreeRadix:            2,
			ConnectorTreeRadix:   4,
			VTXOExitDelay:        10,
		},
		wantErr: true,
	}, {
		// Issue #374 regression: TreeRadix > ConnectorTreeRadix.
		// Under the buggy code the gate consumed TreeRadix=32 against
		// MaxConnectors=32 → depth 1 + margin 6 = minExitDelay 7, so
		// VTXOExitDelay=11 would pass. The real connector tree has
		// radix 2 and 32 leaves → broadcast count 6, so minExitDelay
		// = 12 and 11 must fail. This case catches that regression
		// precisely.
		name: "tree radix masks deeper connector tree",
		terms: &batch.Terms{
			MaxConnectorsPerTree: 32,
			TreeRadix:            32,
			ConnectorTreeRadix:   2,
			VTXOExitDelay:        11,
		},
		wantErr: true,
	}, {
		// Off-by-one regression for the post-review depth+1 fix.
		// With maxConns=32, radix=2 the prior helper returned 5
		// (branch tiers only), accepting VTXOExitDelay=12 because
		// 5+6=11 and 12>11. The leaf tx is a broadcast too, so the
		// true count is 6 and minExitDelay is 12 — VTXOExitDelay=12
		// must now fail.
		name: "depth off by one catches leaf broadcast",
		terms: &batch.Terms{
			MaxConnectorsPerTree: 32,
			TreeRadix:            2,
			ConnectorTreeRadix:   2,
			VTXOExitDelay:        12,
		},
		wantErr: true,
	}, {
		// Same shape as the previous case but with a VTXOExitDelay
		// large enough to clear the real connector depth (>12). The
		// fix must still accept genuinely-safe configs.
		name: "tree radix masks deeper connector tree but exit ok",
		terms: &batch.Terms{
			MaxConnectorsPerTree: 32,
			TreeRadix:            32,
			ConnectorTreeRadix:   2,
			VTXOExitDelay:        13,
		},
		wantErr: false,
	}, {
		// Defensive: a ConnectorTreeRadix below 2 is now an
		// unconditional reject because rounds/fsm_transitions.go and
		// client/lib/tree/batch.go fail the build at finalize time
		// for any radix < 2. The earlier gate only flagged radix < 2
		// when MaxConnectorsPerTree > 1, which would have let a
		// MaxConnectorsPerTree==1 config boot and then crash the
		// first non-trivial forfeit round.
		name: "degenerate connector radix rejected",
		terms: &batch.Terms{
			MaxConnectorsPerTree: 32,
			TreeRadix:            2,
			ConnectorTreeRadix:   1,
			VTXOExitDelay:        144,
		},
		wantErr: true,
	}, {
		// Direct regression test for the reviewer's first concern:
		// MaxConnectorsPerTree==1 with ConnectorTreeRadix==0 must
		// still be rejected even though no multi-leaf tree gets
		// built — the builder rejects radix<2 unconditionally, so
		// the gate must too.
		name: "radix zero rejected even when no tree",
		terms: &batch.Terms{
			MaxConnectorsPerTree: 1,
			TreeRadix:            2,
			ConnectorTreeRadix:   0,
			VTXOExitDelay:        144,
		},
		wantErr: true,
	}, {
		// Same as above with ConnectorTreeRadix==1.
		name: "radix one rejected even when no tree",
		terms: &batch.Terms{
			MaxConnectorsPerTree: 1,
			TreeRadix:            2,
			ConnectorTreeRadix:   1,
			VTXOExitDelay:        144,
		},
		wantErr: true,
	}, {
		// Single-leaf tree with a valid radix is accepted when the
		// exit delay clears (1 broadcast tx + 6 safety = 7); 144 is
		// well above.
		name: "single leaf valid radix accepted",
		terms: &batch.Terms{
			MaxConnectorsPerTree: 1,
			TreeRadix:            2,
			ConnectorTreeRadix:   2,
			VTXOExitDelay:        144,
		},
		wantErr: false,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := checkFraudResponseSafetyMargin(tc.terms)
			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
		})
	}
}

// TestRoundsTermsFromConfigIncludesConnectorDustAmount verifies the
// operator's round terms carry the configured connector dust amount.
func TestRoundsTermsFromConfigIncludesConnectorDustAmount(t *testing.T) {
	t.Parallel()

	cfg := DefaultRoundsConfig()
	cfg.ConnectorDustAmount = 777

	terms := roundsTermsFromConfig(cfg)

	require.Equal(
		t, btcutil.Amount(cfg.ConnectorDustAmount),
		terms.ConnectorDustAmount,
	)
}

// TestRestoreConfirmedBatchWatchesRegistersConfirmedTrees verifies startup
// restoration re-registers persisted confirmed round trees with BatchWatcher.
func TestRestoreConfirmedBatchWatchesRegistersConfirmedTrees(t *testing.T) {
	t.Parallel()

	sqlStore := db.NewTestDB(t)
	store := newTestIndexerStore(sqlStore.BaseDB)
	roundStore := store.NewRoundStore()
	ctx := t.Context()

	round := testConfirmedWatchRound(t)
	const confirmationHeight = int32(321)
	err := roundStore.PersistRound(ctx, round)
	require.NoError(t, err)

	err = roundStore.MarkRoundConfirmed(
		ctx, round.RoundID, confirmationHeight, chainhash.Hash{0x02},
	)
	require.NoError(t, err)

	watcher := &captureBatchWatcherRef{}
	server := &Server{
		batchWatcherRef: watcher,
		log:             btclog.Disabled,
	}

	err = server.restoreConfirmedBatchWatches(ctx, roundStore)
	require.NoError(t, err)

	require.Len(t, watcher.registers, 1)

	req := watcher.registers[0]
	require.Equal(
		t,
		batchwatcher.BatchIDForRoundOutput(
			uuid.UUID(round.RoundID), 0,
		),
		req.BatchID,
	)
	require.Equal(t, uint32(confirmationHeight), req.ConfirmationHeight)
	require.Equal(
		t, uint32(confirmationHeight)+round.CSVDelay, req.ExpiryHeight,
	)
	require.NotNil(t, req.Tree)
	require.Equal(
		t, round.VTXOTrees[0].BatchOutpoint, req.Tree.BatchOutpoint,
	)
}

// captureBatchWatcherRef records RegisterBatchRequest messages for tests.
type captureBatchWatcherRef struct {
	registers []*batchwatcher.RegisterBatchRequest
}

// batchWatcherFuture aliases the verbose batch watcher actor future type.
type batchWatcherFuture = actor.Future[batchwatcher.BatchWatcherResp]

// ID returns the stable actor ID for the capture reference.
func (c *captureBatchWatcherRef) ID() string {
	return "capture-batch-watcher"
}

// Tell records register-batch requests and rejects other messages.
func (c *captureBatchWatcherRef) Tell(_ context.Context,
	msg batchwatcher.BatchWatcherMsg) error {

	req, ok := msg.(*batchwatcher.RegisterBatchRequest)
	if !ok {
		return fmt.Errorf("unexpected batch watcher message %T", msg)
	}

	c.registers = append(c.registers, req)

	return nil
}

// Ask returns an unsupported-operation response for the capture reference.
func (c *captureBatchWatcherRef) Ask(_ context.Context,
	msg batchwatcher.BatchWatcherMsg) batchWatcherFuture {

	promise := actor.NewPromise[batchwatcher.BatchWatcherResp]()
	_ = promise.Complete(
		fn.Err[batchwatcher.BatchWatcherResp](
			fmt.Errorf("unexpected batch watcher ask %T", msg),
		),
	)

	return promise.Future()
}

// testConfirmedWatchRound returns a minimal persisted round with one VTXO tree.
func testConfirmedWatchRound(t *testing.T) *rounds.Round {
	t.Helper()

	roundID := rounds.RoundID(uuid.New())
	finalTx := wire.NewMsgTx(2)
	finalTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash: chainhash.Hash{0x01},
		},
	})
	finalTx.AddTxOut(&wire.TxOut{
		Value:    100_000,
		PkScript: []byte{0x51},
	})

	batchOutpoint := wire.OutPoint{
		Hash: finalTx.TxHash(),
	}

	sweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const csvDelay = uint32(144)
	sweepRoot := testSweepTaproot(t, sweepKey.PubKey(), csvDelay)
	vtxoTree := &tree.Tree{
		BatchOutpoint: batchOutpoint,
		BatchOutput:   finalTx.TxOut[0],
		Root: &tree.Node{
			Input: batchOutpoint,
			Outputs: []*wire.TxOut{
				{
					Value: 100_000,
					PkScript: []byte{
						0x51,
					},
				},
			},
			CoSigners: []*btcec.PublicKey{
				sweepKey.PubKey(),
			},
			Children: make(map[uint32]*tree.Node),
			Amount:   btcutil.Amount(100_000),
		},
		SweepTapscriptRoot: sweepRoot,
	}

	return &rounds.Round{
		RoundID: roundID,
		FinalTx: finalTx,
		VTXOTrees: map[int]*tree.Tree{
			0: vtxoTree,
		},
		ConnectorDescriptors: []*rounds.ConnectorTreeDescriptor{},
		ForfeitInfos: make(
			map[wire.OutPoint]*rounds.ForfeitInfo,
		),
		ClientRegistrations: make(
			map[rounds.ClientID]*rounds.ClientRegistration,
		),
		SweepKey: sweepKey.PubKey(),
		CSVDelay: csvDelay,
	}
}

// testSweepTaproot returns the sweep tapscript root for a test VTXO tree.
func testSweepTaproot(t *testing.T, sweepKey *btcec.PublicKey,
	csvDelay uint32) []byte {

	t.Helper()

	sweepTapLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		sweepKey, csvDelay,
	)
	require.NoError(t, err)

	sweepTapRoot := sweepTapLeaf.TapHash()

	return sweepTapRoot[:]
}
