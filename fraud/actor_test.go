package fraud

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	checkpointtx "github.com/lightninglabs/darepo-client/lib/tx/checkpoint"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestNewActorRejectsMissingRequiredFields verifies that NewActor
// surfaces a misconfigured fraud Config at construction time rather
// than silently dropping notifications until the first runtime touch.
func TestNewActorRejectsMissingRequiredFields(t *testing.T) {
	t.Parallel()

	_, _, operatorKey, signer, sweepInfo := makeCheckpointSweepFixture(t)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey,
		CSVDelay:    10,
	}

	makeBaseConfig := func() Config {
		return Config{
			TxConfirmRef: &recordingTxConfirmRef{},
			CheckpointPlanner: &CheckpointPlanner{
				VTXOStore: &fakeVTXOStore{},
				CheckpointLookup: &fakeCheckpointLookup{
					tx:    sweepInfo.CheckpointTx,
					found: true,
				},
				CheckpointSweepStore: &fakeCheckpointSweepStore{
					info:  sweepInfo,
					found: true,
				},
				CheckpointPolicy: policy,
			},
			CheckpointSweepStore: &fakeCheckpointSweepStore{
				info:  sweepInfo,
				found: true,
			},
			CheckpointPolicy: policy,
			OperatorKey:      operatorKey,
			Signer:           signer,
			NewSweepPkScript: func(context.Context) ([]byte,
				error) {

				return []byte{0x51}, nil
			},
		}
	}

	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{{
		name:   "missing TxConfirmRef",
		mutate: func(c *Config) { c.TxConfirmRef = nil },
		want:   "TxConfirmRef",
	}, {
		name:   "missing CheckpointPlanner",
		mutate: func(c *Config) { c.CheckpointPlanner = nil },
		want:   "CheckpointPlanner",
	}, {
		name:   "missing CheckpointSweepStore",
		mutate: func(c *Config) { c.CheckpointSweepStore = nil },
		want:   "CheckpointSweepStore",
	}, {
		name:   "missing NewSweepPkScript",
		mutate: func(c *Config) { c.NewSweepPkScript = nil },
		want:   "NewSweepPkScript",
	}, {
		name:   "missing Signer",
		mutate: func(c *Config) { c.Signer = nil },
		want:   "Signer",
	}, {
		name: "missing OperatorKey.PubKey",
		mutate: func(c *Config) {
			c.OperatorKey = keychain.KeyDescriptor{}
		},
		want: "OperatorKey",
	}, {
		name: "missing CheckpointPolicy.OperatorKey",
		mutate: func(c *Config) {
			c.CheckpointPolicy.OperatorKey = nil
		},
		want: "CheckpointPolicy.OperatorKey",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := makeBaseConfig()
			tc.mutate(&cfg)

			actor, err := NewActor(cfg)
			require.Error(t, err)
			require.ErrorContains(t, err, tc.want)
			require.Nil(t, actor)
		})
	}

	// Sanity: the unmutated base config must construct cleanly.
	a, err := NewActor(makeBaseConfig())
	require.NoError(t, err)
	require.NotNil(t, a)
}

func TestCheckpointPlannerIgnoresUnknownVTXO(t *testing.T) {
	t.Parallel()

	planner := &CheckpointPlanner{
		VTXOStore:        &fakeVTXOStore{},
		CheckpointLookup: &fakeCheckpointLookup{},
	}

	plan, actionable, err := planner.PlanCheckpoint(
		t.Context(), onChainNotification(testOutpoint(1)),
	)
	require.NoError(t, err)
	require.False(t, actionable)
	require.Nil(t, plan)
}

func TestCheckpointPlannerIgnoresNonSpentVTXO(t *testing.T) {
	t.Parallel()

	input := testOutpoint(2)
	planner := &CheckpointPlanner{
		VTXOStore: &fakeVTXOStore{
			records: map[wire.OutPoint]*batchwatcher.RecoveryVTXO{
				input: {
					Outpoint: input,
					Status:   batchwatcher.VTXOStatusLive,
				},
			},
		},
		CheckpointLookup: &fakeCheckpointLookup{},
	}

	plan, actionable, err := planner.PlanCheckpoint(
		t.Context(), onChainNotification(input),
	)
	require.NoError(t, err)
	require.False(t, actionable)
	require.Nil(t, plan)
}

func TestCheckpointPlannerErrorsWhenSpentCheckpointMissing(t *testing.T) {
	t.Parallel()

	input := testOutpoint(3)
	planner := &CheckpointPlanner{
		VTXOStore: spentVTXOStore(input),
		CheckpointLookup: &fakeCheckpointLookup{
			found: false,
		},
	}

	_, actionable, err := planner.PlanCheckpoint(
		t.Context(), onChainNotification(input),
	)
	require.True(t, actionable)
	require.ErrorContains(t, err, "no finalized checkpoint")
}

func TestCheckpointPlannerReturnsStoredCheckpointTx(t *testing.T) {
	t.Parallel()

	input, _, _, _, sweepInfo := makeCheckpointSweepFixture(t)
	checkpointTx := sweepInfo.CheckpointTx
	planner := &CheckpointPlanner{
		VTXOStore: spentVTXOStore(input),
		CheckpointLookup: &fakeCheckpointLookup{
			tx:    checkpointTx,
			found: true,
		},
	}

	plan, actionable, err := planner.PlanCheckpoint(
		t.Context(), onChainNotification(input),
	)
	require.NoError(t, err)
	require.True(t, actionable)
	require.Same(t, checkpointTx, plan.CheckpointTx)
}

// TestCheckpointPlannerReturnsStoredForfeitTx verifies that a forfeited VTXO
// leaf resolves to its stored forfeit transaction.
func TestCheckpointPlannerReturnsStoredForfeitTx(t *testing.T) {
	t.Parallel()

	input := testOutpoint(4)
	forfeitTx := testForfeitTx(input, 24_000, []byte{0x51})

	forfeitedStatus := batchwatcher.VTXOStatusForfeited
	planner := &CheckpointPlanner{
		VTXOStore: &fakeVTXOStore{
			records: map[wire.OutPoint]*batchwatcher.RecoveryVTXO{
				input: {
					Outpoint: input,
					Status:   forfeitedStatus,
				},
			},
		},
		CheckpointLookup: &fakeCheckpointLookup{},
		ForfeitLookup: &fakeForfeitLookup{
			plans: map[wire.OutPoint]*ResponsePlan{
				input: {ResponseTx: forfeitTx},
			},
		},
	}

	plan, actionable, err := planner.PlanCheckpoint(
		t.Context(), onChainNotification(input),
	)
	require.NoError(t, err)
	require.True(t, actionable)
	require.Same(t, forfeitTx, plan.ForfeitPlan.ResponseTx)
	require.Nil(t, plan.CheckpointTx)
}

func TestActorSubmitsCheckpointWithExpectedLabel(t *testing.T) {
	t.Parallel()

	input, policy, operatorKey, signer, sweepInfo :=
		makeCheckpointSweepFixture(t)
	txConfirmRef := &recordingTxConfirmRef{}
	actor := newCheckpointActor(
		t, input, policy, operatorKey, signer, sweepInfo,
		txConfirmRef,
	)

	result := actor.Receive(t.Context(), onChainNotification(input))
	require.NoError(t, result.Err())

	require.Len(t, txConfirmRef.ensureReqs, 1)
	require.Equal(t, CheckpointLabel, txConfirmRef.ensureReqs[0].Label)
	require.Equal(t, sweepInfo.CheckpointTx.TxHash(),
		txConfirmRef.ensureReqs[0].Tx.TxHash())
}

// TestActorSubmitsForfeitedOnChainResponse verifies that a forfeited VTXO
// leaf appearing on-chain submits the stored forfeit transaction.
func TestActorSubmitsForfeitedOnChainResponse(t *testing.T) {
	t.Parallel()

	input := testOutpoint(5)
	forfeitTx := testForfeitTx(input, 24_000, []byte{0x51})

	forfeitedStatus := batchwatcher.VTXOStatusForfeited
	txConfirmRef := &recordingTxConfirmRef{}
	actor := newOnChainPlannerActor(t, &CheckpointPlanner{
		VTXOStore: &fakeVTXOStore{
			records: map[wire.OutPoint]*batchwatcher.RecoveryVTXO{
				input: {
					Outpoint: input,
					Status:   forfeitedStatus,
				},
			},
		},
		CheckpointLookup: &fakeCheckpointLookup{},
		ForfeitLookup: &fakeForfeitLookup{
			plans: map[wire.OutPoint]*ResponsePlan{
				input: {ResponseTx: forfeitTx},
			},
		},
	}, txConfirmRef)

	result := actor.Receive(t.Context(), onChainNotification(input))
	require.NoError(t, result.Err())
	require.Len(t, txConfirmRef.ensureReqs, 1)
	require.Equal(t, ForfeitLabel, txConfirmRef.ensureReqs[0].Label)
	require.Equal(
		t, forfeitTx.TxHash(), txConfirmRef.ensureReqs[0].Tx.TxHash(),
	)
}

// TestActorDedupsRepeatedForfeitedNotification verifies that a repeated
// VTXOOnChainNotification for a forfeited VTXO does not re-broadcast the
// stored forfeit transaction.
func TestActorDedupsRepeatedForfeitedNotification(t *testing.T) {
	t.Parallel()

	input := testOutpoint(8)
	forfeitTx := testForfeitTx(input, 24_000, []byte{0x51})

	forfeitedStatus := batchwatcher.VTXOStatusForfeited
	txConfirmRef := &recordingTxConfirmRef{}
	actor := newOnChainPlannerActor(t, &CheckpointPlanner{
		VTXOStore: &fakeVTXOStore{
			records: map[wire.OutPoint]*batchwatcher.RecoveryVTXO{
				input: {
					Outpoint: input,
					Status:   forfeitedStatus,
				},
			},
		},
		CheckpointLookup: &fakeCheckpointLookup{},
		ForfeitLookup: &fakeForfeitLookup{
			plans: map[wire.OutPoint]*ResponsePlan{
				input: {ResponseTx: forfeitTx},
			},
		},
	}, txConfirmRef)

	result := actor.Receive(t.Context(), onChainNotification(input))
	require.NoError(t, result.Err())

	result = actor.Receive(t.Context(), onChainNotification(input))
	require.NoError(t, result.Err())

	require.Len(t, txConfirmRef.ensureReqs, 1)
}

func TestActorDedupsRepeatedCheckpointNotification(t *testing.T) {
	t.Parallel()

	input, policy, operatorKey, signer, sweepInfo :=
		makeCheckpointSweepFixture(t)
	txConfirmRef := &recordingTxConfirmRef{}
	actor := newCheckpointActor(
		t, input, policy, operatorKey, signer, sweepInfo,
		txConfirmRef,
	)

	result := actor.Receive(t.Context(), onChainNotification(input))
	require.NoError(t, result.Err())

	result = actor.Receive(t.Context(), onChainNotification(input))
	require.NoError(t, result.Err())

	require.Len(t, txConfirmRef.ensureReqs, 1)
}

func TestCheckpointSweepBuilderCreatesValidCSVScriptPathSpend(t *testing.T) {
	t.Parallel()

	_, policy, operatorKey, signer, sweepInfo :=
		makeCheckpointSweepFixture(t)

	sweepTx, err := BuildCheckpointTimeoutSweep(
		t.Context(), &CheckpointSweepRequest{
			Info:          sweepInfo,
			Policy:        policy,
			OperatorKey:   operatorKey,
			Signer:        signer,
			SweepPkScript: []byte{0x51},
		},
	)
	require.NoError(t, err)
	require.NotNil(t, sweepTx)

	checkpointTxid := sweepInfo.CheckpointTx.TxHash()
	require.Equal(t, wire.OutPoint{
		Hash:  checkpointTxid,
		Index: 0,
	}, sweepTx.TxIn[0].PreviousOutPoint)
	require.Equal(t, policy.CSVDelay, sweepTx.TxIn[0].Sequence)
	require.Len(t, sweepTx.TxIn[0].Witness, 3)
	require.Equal(t, int32(arktx.TxVersion), sweepTx.Version)
	require.False(t, arktx.IsAnchorOutput(sweepTx.TxOut[0]))
	require.True(t, arktx.IsAnchorOutput(sweepTx.TxOut[1]))
}

func TestForfeitSweepBuilderCreatesValidBIP86KeySpend(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey := keychain.KeyDescriptor{
		PubKey: operatorPriv.PubKey(),
	}
	signer := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorPriv}, nil,
	)

	forfeitScript, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(operatorPriv.PubKey()),
	)
	require.NoError(t, err)

	forfeitTx := wire.NewMsgTx(arktx.TxVersion)
	forfeitedOutpoint := testOutpoint(0xf0)
	forfeitTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: forfeitedOutpoint,
	})
	forfeitTx.AddTxOut(&wire.TxOut{
		Value:    24_000,
		PkScript: forfeitScript,
	})

	sweepTx, err := BuildForfeitOutputSweep(
		t.Context(), &ForfeitSweepRequest{
			ForfeitTx:       forfeitTx,
			ForfeitOutpoint: forfeitedOutpoint,
			OperatorKey:     operatorKey,
			Signer:          signer,
			SweepPkScript:   []byte{0x51},
		},
	)
	require.NoError(t, err)
	require.NotNil(t, sweepTx)

	require.Equal(t, wire.OutPoint{
		Hash:  forfeitTx.TxHash(),
		Index: 0,
	}, sweepTx.TxIn[0].PreviousOutPoint)
	require.Equal(t, wire.MaxTxInSequenceNum, sweepTx.TxIn[0].Sequence)
	require.Len(t, sweepTx.TxIn[0].Witness, 1)
	require.Equal(t, int32(arktx.TxVersion), sweepTx.Version)
	require.False(t, arktx.IsAnchorOutput(sweepTx.TxOut[0]))
	require.True(t, arktx.IsAnchorOutput(sweepTx.TxOut[1]))
}

func TestForfeitSweepBuilderRejectsWrongForfeitOutpoint(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey := keychain.KeyDescriptor{
		PubKey: operatorPriv.PubKey(),
	}
	signer := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorPriv}, nil,
	)

	forfeitScript, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(operatorPriv.PubKey()),
	)
	require.NoError(t, err)

	forfeitedOutpoint := testOutpoint(0xf1)
	forfeitTx := wire.NewMsgTx(arktx.TxVersion)
	forfeitTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: forfeitedOutpoint,
	})
	forfeitTx.AddTxOut(&wire.TxOut{
		Value:    24_000,
		PkScript: forfeitScript,
	})

	_, err = BuildForfeitOutputSweep(
		t.Context(), &ForfeitSweepRequest{
			ForfeitTx:       forfeitTx,
			ForfeitOutpoint: testOutpoint(0xf2),
			OperatorKey:     operatorKey,
			Signer:          signer,
			SweepPkScript:   []byte{0x51},
		},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "forfeit tx input spends")
}

func TestActorSweepsForfeitPenaltyAfterResponseConfirms(t *testing.T) {
	t.Parallel()

	input := testOutpoint(9)
	forfeitTx := testForfeitTx(input, 24_000, []byte{0x51})
	sweepTx := testSweepTx(forfeitTx.TxHash())

	forfeitedStatus := batchwatcher.VTXOStatusForfeited
	txConfirmRef := &recordingTxConfirmRef{}
	actor := newOnChainPlannerActor(t, &CheckpointPlanner{
		VTXOStore: &fakeVTXOStore{
			records: map[wire.OutPoint]*batchwatcher.RecoveryVTXO{
				input: {
					Outpoint: input,
					Status:   forfeitedStatus,
				},
			},
		},
		CheckpointLookup: &fakeCheckpointLookup{},
		ForfeitLookup: &fakeForfeitLookup{
			plans: map[wire.OutPoint]*ResponsePlan{
				input: {ResponseTx: forfeitTx},
			},
		},
	}, txConfirmRef)
	actor.cfg.BuildForfeitSweep = func(_ context.Context,
		req *ForfeitSweepRequest) (*wire.MsgTx, error) {

		require.Same(t, forfeitTx, req.ForfeitTx)

		return sweepTx, nil
	}

	result := actor.Receive(t.Context(), onChainNotification(input))
	require.NoError(t, result.Err())
	require.Len(t, txConfirmRef.ensureReqs, 1)

	result = actor.Receive(t.Context(), &txconfirm.TxConfirmed{
		Txid:        forfeitTx.TxHash(),
		BlockHeight: 101,
	})
	require.NoError(t, result.Err())

	require.Len(t, txConfirmRef.ensureReqs, 2)
	require.Equal(t, ForfeitSweepLabel, txConfirmRef.ensureReqs[1].Label)
	require.Equal(
		t, sweepTx.TxHash(), txConfirmRef.ensureReqs[1].Tx.TxHash(),
	)
	require.Equal(t, forfeitJobState{
		phase: forfeitPhaseSweep,
		txid:  sweepTx.TxHash(),
	}, actor.forfeitsByOutpoint[input])

	result = actor.Receive(t.Context(), &txconfirm.TxConfirmed{
		Txid:        sweepTx.TxHash(),
		BlockHeight: 102,
	})
	require.NoError(t, result.Err())
	require.NotContains(t, actor.forfeitsByOutpoint, input)
}

func TestSweepWaitsUntilCSVMaturity(t *testing.T) {
	t.Parallel()

	input, policy, operatorKey, signer, sweepInfo :=
		makeCheckpointSweepFixture(t)
	txConfirmRef := &recordingTxConfirmRef{}
	sweepTx := testSweepTx(sweepInfo.CheckpointTx.TxHash())

	actor := newCheckpointActor(
		t, input, policy, operatorKey, signer, sweepInfo,
		txConfirmRef,
	)
	actor.cfg.BuildSweep = func(context.Context,
		*CheckpointSweepRequest) (*wire.MsgTx, error) {

		return sweepTx, nil
	}

	result := actor.Receive(t.Context(), onChainNotification(input))
	require.NoError(t, result.Err())
	require.Len(t, txConfirmRef.ensureReqs, 1)

	checkpointOutpoint := wire.OutPoint{
		Hash:  sweepInfo.CheckpointTx.TxHash(),
		Index: 0,
	}
	result = actor.Receive(t.Context(),
		&batchwatcher.CheckpointSweepNotification{
			InputOutpoint:      input,
			CheckpointOutpoint: checkpointOutpoint,
			MaturityHeight:     110,
		},
	)
	require.NoError(t, result.Err())
	require.Len(t, txConfirmRef.ensureReqs, 2)
	require.Equal(
		t, CheckpointSweepLabel, txConfirmRef.ensureReqs[1].Label,
	)
	require.Equal(
		t, sweepTx.TxHash(), txConfirmRef.ensureReqs[1].Tx.TxHash(),
	)
}

// TestActorIgnoresDuplicateUnexpectedSpendForKnownCheckpoint verifies that a
// trailing UnexpectedSpendNotification for a checkpoint we already submitted
// from VTXOOnChainNotification is not re-submitted to txconfirm.
func TestActorIgnoresDuplicateUnexpectedSpendForKnownCheckpoint(t *testing.T) {
	t.Parallel()

	input, policy, operatorKey, signer, sweepInfo :=
		makeCheckpointSweepFixture(t)
	txConfirmRef := &recordingTxConfirmRef{}
	actor := newCheckpointActor(
		t, input, policy, operatorKey, signer, sweepInfo,
		txConfirmRef,
	)

	result := actor.Receive(t.Context(), onChainNotification(input))
	require.NoError(t, result.Err())
	require.Len(t, txConfirmRef.ensureReqs, 1)

	spentLeaf := batchwatcher.SpendClassificationSpentLeaf
	result = actor.Receive(t.Context(),
		&batchwatcher.UnexpectedSpendNotification{
			TrackedOutput: &batchwatcher.Output{
				Outpoint: input,
				TxOut: &wire.TxOut{
					Value:    25_000,
					PkScript: []byte{0x51},
				},
			},
			Classification: spentLeaf,
			ResponseTxID:   sweepInfo.CheckpointTx.TxHash(),
			ResponseTx:     sweepInfo.CheckpointTx,
		},
	)
	require.NoError(t, result.Err())
	require.Len(t, txConfirmRef.ensureReqs, 1)
}

// TestCheckpointRetriesAfterTxConfirmAskFails verifies that when the
// initial txconfirm.Ask for a checkpoint broadcast fails synchronously,
// fraud does NOT permanently mark the input as deduped. A subsequent
// VTXOOnChainNotification (after batchwatcher's retry, say) can therefore
// re-attempt the checkpoint submission and succeed. Symmetric coverage to
// TestCheckpointSweepRetriesAfterTxConfirmAskFails for the sweep path.
func TestCheckpointRetriesAfterTxConfirmAskFails(t *testing.T) {
	t.Parallel()

	input, policy, operatorKey, signer, sweepInfo :=
		makeCheckpointSweepFixture(t)
	txConfirmRef := &recordingTxConfirmRef{}
	actor := newCheckpointActor(
		t, input, policy, operatorKey, signer, sweepInfo,
		txConfirmRef,
	)

	// Make txconfirm fail the first Ask synchronously.
	txConfirmRef.failNext = fmt.Errorf("simulated txconfirm failure")

	// First attempt: must surface the txconfirm error and must NOT
	// register the txid in checkpointsByTxid (otherwise the retry below
	// would be silently deduped). The failNext mock swallows the request
	// before recording it, so ensureReqs stays empty on failure.
	result := actor.Receive(t.Context(), onChainNotification(input))
	require.Error(t, result.Err(),
		"first attempt must surface the txconfirm error")
	require.Len(t, txConfirmRef.ensureReqs, 0,
		"failed attempt must not record an ensure req")

	// Second attempt: same notification, no failure injected. The
	// checkpoint must now actually submit, proving the dedup index was
	// not poisoned by the failed attempt.
	result = actor.Receive(t.Context(), onChainNotification(input))
	require.NoError(t, result.Err(),
		"retry must succeed once the transient error clears")
	require.Len(t, txConfirmRef.ensureReqs, 1,
		"retry must reach txconfirm once the transient error clears")
	require.Equal(t, CheckpointLabel, txConfirmRef.ensureReqs[0].Label)
}

// TestActorSubmitsForfeitedLeafResponse verifies that forfeited-leaf fraud
// notifications submit the stored forfeit transaction to txconfirm.
func TestActorSubmitsForfeitedLeafResponse(t *testing.T) {
	t.Parallel()

	input, policy, operatorKey, signer, sweepInfo :=
		makeCheckpointSweepFixture(t)
	txConfirmRef := &recordingTxConfirmRef{}
	actor := newCheckpointActor(
		t, input, policy, operatorKey, signer, sweepInfo,
		txConfirmRef,
	)

	forfeitTx := testForfeitTx(
		testOutpoint(7), 24_000, []byte{0x51},
	)

	result := actor.Receive(t.Context(),
		&batchwatcher.UnexpectedSpendNotification{
			TrackedOutput: &batchwatcher.Output{
				Outpoint: testOutpoint(7),
				TxOut: &wire.TxOut{
					Value:    25_000,
					PkScript: []byte{0x51},
				},
			},
			Classification: batchwatcher.
				SpendClassificationForfeitedLeaf,
			ResponseTxID: forfeitTx.TxHash(),
			ResponseTx:   forfeitTx,
		},
	)
	require.NoError(t, result.Err())
	require.Len(t, txConfirmRef.ensureReqs, 1)
	require.Equal(
		t, "fraud-forfeited_leaf", txConfirmRef.ensureReqs[0].Label,
	)
	require.Equal(
		t, forfeitTx.TxHash(), txConfirmRef.ensureReqs[0].Tx.TxHash(),
	)
}

// TestActorFansOutSharedConnectorAncestorConfirmation verifies that two
// forfeit responses with overlapping connector tree ancestors both advance
// when the shared ancestor confirms. Multiple forfeits from the same batch
// commonly share a connector tree prefix, so a single TxConfirmed for that
// shared ancestor must fan out to every dependent forfeit's chain — not
// just the most recently submitted one. Without the fan-out, the second
// submitNextTxn for the shared txid would have overwritten the first job's
// pending entry, stranding the first forfeit's chain at the shared
// ancestor and never broadcasting its forfeit transaction.
//
// Covers issue #247 "Connector Spend Race Handling": multiple forfeits
// sharing connector tree prefix.
func TestActorFansOutSharedConnectorAncestorConfirmation(t *testing.T) {
	t.Parallel()

	_, policy, operatorKey, signer, sweepInfo :=
		makeCheckpointSweepFixture(t)

	// Two forfeits, two distinct VTXO outpoints, but both forfeit
	// transactions consume the same connector ancestor. This is the
	// shared-prefix shape that arises whenever multiple leaves under
	// the same connector branch are forfeited in the same batch.
	outpointA := testOutpoint(0xa1)
	outpointB := testOutpoint(0xb1)

	sharedAncestor := wire.NewMsgTx(int32(arktx.TxVersion))
	sharedAncestor.AddTxIn(&wire.TxIn{
		PreviousOutPoint: testOutpoint(0xff),
	})
	sharedAncestor.AddTxOut(&wire.TxOut{
		Value: 660, PkScript: []byte{0x51},
	})

	forfeitA := testForfeitTx(outpointA, 24_000, []byte{0x51})
	forfeitB := testForfeitTx(outpointB, 24_000, []byte{0x52})

	planA := &ResponsePlan{
		Ancestors:  []*wire.MsgTx{sharedAncestor},
		ResponseTx: forfeitA,
		Label:      ForfeitLabel,
	}
	planB := &ResponsePlan{
		Ancestors:  []*wire.MsgTx{sharedAncestor},
		ResponseTx: forfeitB,
		Label:      ForfeitLabel,
	}

	txConfirmRef := &recordingTxConfirmRef{}
	a := newCheckpointActor(
		t, outpointA, policy, operatorKey, signer, sweepInfo,
		txConfirmRef,
	)
	a.cfg.BuildForfeitSweep = func(_ context.Context,
		req *ForfeitSweepRequest) (*wire.MsgTx, error) {

		return testSweepTx(req.ForfeitTx.TxHash()), nil
	}

	require.NoError(t, a.ensureForfeit(t.Context(), outpointA, planA))
	require.NoError(t, a.ensureForfeit(t.Context(), outpointB, planB))

	// Both jobs registered their interest in the shared ancestor.
	require.Len(t, a.pending[sharedAncestor.TxHash()], 2,
		"both jobs must be queued on the shared ancestor txid; "+
			"otherwise the second ensureForfeit overwrote the "+
			"first and stranded its chain")
	require.Len(t, txConfirmRef.ensureReqs, 2)

	// One TxConfirmed for the shared ancestor must advance BOTH jobs.
	// Each job submits its own forfeit tx (forfeitA / forfeitB).
	result := a.Receive(t.Context(), &txconfirm.TxConfirmed{
		Txid:        sharedAncestor.TxHash(),
		BlockHeight: 100,
	})
	require.NoError(t, result.Err())

	require.Len(t, txConfirmRef.ensureReqs, 4,
		"shared ancestor confirmation must fan out to both "+
			"forfeits; total ensure requests = 2 ancestors + "+
			"2 forfeit txs")

	submittedTxids := make(map[chainhash.Hash]bool)
	for _, req := range txConfirmRef.ensureReqs {
		submittedTxids[req.Tx.TxHash()] = true
	}
	require.Contains(t, submittedTxids, forfeitA.TxHash(),
		"forfeit A must have been submitted")
	require.Contains(t, submittedTxids, forfeitB.TxHash(),
		"forfeit B must have been submitted")
}

// TestActorCoalescesDuplicateTerminalForfeitSweeps verifies that the
// VTXO-on-chain and legacy unexpected-spend paths cannot both build a penalty
// sweep after the same stored forfeit transaction confirms.
func TestActorCoalescesDuplicateTerminalForfeitSweeps(t *testing.T) {
	t.Parallel()

	input, policy, operatorKey, signer, sweepInfo :=
		makeCheckpointSweepFixture(t)
	txConfirmRef := &recordingTxConfirmRef{}
	a := newCheckpointActor(
		t, input, policy, operatorKey, signer, sweepInfo,
		txConfirmRef,
	)

	forfeitedOutpoint := testOutpoint(0xd1)
	forfeitTx := testForfeitTx(forfeitedOutpoint, 24_000, []byte{0x51})
	sweepTx := testSweepTx(forfeitTx.TxHash())

	var sweepBuilds int
	a.cfg.BuildForfeitSweep = func(_ context.Context,
		req *ForfeitSweepRequest) (*wire.MsgTx, error) {

		require.Same(t, forfeitTx, req.ForfeitTx)
		sweepBuilds++

		return sweepTx, nil
	}

	require.NoError(t, a.ensureForfeit(
		t.Context(), forfeitedOutpoint,
		&ResponsePlan{
			ResponseTx: forfeitTx,
			Label:      ForfeitLabel,
		},
	))

	result := a.Receive(t.Context(),
		&batchwatcher.UnexpectedSpendNotification{
			TrackedOutput: &batchwatcher.Output{
				Outpoint: forfeitedOutpoint,
				TxOut: &wire.TxOut{
					Value:    25_000,
					PkScript: []byte{0x51},
				},
			},
			Classification: batchwatcher.
				SpendClassificationForfeitedLeaf,
			ResponseTxID: forfeitTx.TxHash(),
			ResponseTx:   forfeitTx,
		},
	)
	require.NoError(t, result.Err())
	require.Len(t, a.pending[forfeitTx.TxHash()], 2)
	require.Len(t, txConfirmRef.ensureReqs, 2)

	result = a.Receive(t.Context(), &txconfirm.TxConfirmed{
		Txid:        forfeitTx.TxHash(),
		BlockHeight: 101,
	})
	require.NoError(t, result.Err())
	require.Equal(t, 1, sweepBuilds)
	require.Len(t, txConfirmRef.ensureReqs, 3)
	require.Equal(t, ForfeitSweepLabel, txConfirmRef.ensureReqs[2].Label)
	require.Equal(
		t, sweepTx.TxHash(), txConfirmRef.ensureReqs[2].Tx.TxHash(),
	)
	require.Equal(t, forfeitJobState{
		phase: forfeitPhaseSweep,
		txid:  sweepTx.TxHash(),
	}, a.forfeitsByOutpoint[forfeitedOutpoint])
}

// TestForfeitResponseClearsDedupAfterIntermediateAskFailure verifies that a
// synchronous txconfirm failure after the first ancestor confirms does not
// leave the forfeited outpoint permanently deduped.
func TestForfeitResponseClearsDedupAfterIntermediateAskFailure(t *testing.T) {
	t.Parallel()

	_, policy, operatorKey, signer, sweepInfo :=
		makeCheckpointSweepFixture(t)

	forfeitedOutpoint := testOutpoint(0xc1)
	ancestor := wire.NewMsgTx(int32(arktx.TxVersion))
	ancestor.AddTxIn(&wire.TxIn{
		PreviousOutPoint: testOutpoint(0xfc),
	})
	ancestor.AddTxOut(&wire.TxOut{
		Value:    660,
		PkScript: []byte{0x51},
	})
	forfeitTx := testForfeitTx(forfeitedOutpoint, 24_000, []byte{0x51})
	plan := &ResponsePlan{
		Ancestors:  []*wire.MsgTx{ancestor},
		ResponseTx: forfeitTx,
		Label:      ForfeitLabel,
	}

	txConfirmRef := &recordingTxConfirmRef{}
	a := newCheckpointActor(
		t, forfeitedOutpoint, policy, operatorKey, signer, sweepInfo,
		txConfirmRef,
	)

	require.NoError(
		t, a.ensureForfeit(t.Context(), forfeitedOutpoint, plan),
	)
	require.Equal(t, forfeitJobState{
		phase: forfeitPhaseResponse,
		txid:  forfeitTx.TxHash(),
	}, a.forfeitsByOutpoint[forfeitedOutpoint])

	txConfirmRef.failNext = fmt.Errorf("simulated txconfirm failure")
	result := a.Receive(t.Context(), &txconfirm.TxConfirmed{
		Txid:        ancestor.TxHash(),
		BlockHeight: 100,
	})
	require.Error(t, result.Err())
	require.NotContains(t, a.forfeitsByOutpoint, forfeitedOutpoint)
	require.Len(t, txConfirmRef.ensureReqs, 1,
		"failed follow-up must not record another ensure req")

	require.NoError(
		t, a.ensureForfeit(t.Context(), forfeitedOutpoint, plan),
	)
	require.Len(t, txConfirmRef.ensureReqs, 2,
		"retry must reach txconfirm after the stale dedup entry clears")
}

// TestActorPopulatesForfeitDedupOnUnexpectedSpend verifies that the legacy
// UnexpectedSpend ForfeitedLeaf path also populates forfeitsByOutpoint so a
// later VTXOOnChainNotification dedup-skips correctly. The legacy path
// itself does not early-return on a dedup hit because its redundant
// submission of the forfeit tx is what kicks txconfirm into observing
// already-mined confirmations after a daemon restart, but the map write
// keeps the cross-path invariant from the H-3 review finding.
func TestActorPopulatesForfeitDedupOnUnexpectedSpend(t *testing.T) {
	t.Parallel()

	input, policy, operatorKey, signer, sweepInfo :=
		makeCheckpointSweepFixture(t)
	txConfirmRef := &recordingTxConfirmRef{}
	a := newCheckpointActor(
		t, input, policy, operatorKey, signer, sweepInfo,
		txConfirmRef,
	)

	forfeitedOutpoint := testOutpoint(7)
	forfeitTx := testForfeitTx(forfeitedOutpoint, 24_000, []byte{0x51})

	notif := &batchwatcher.UnexpectedSpendNotification{
		TrackedOutput: &batchwatcher.Output{
			Outpoint: forfeitedOutpoint,
			TxOut: &wire.TxOut{
				Value:    25_000,
				PkScript: []byte{0x51},
			},
		},
		Classification: batchwatcher.
			SpendClassificationForfeitedLeaf,
		ResponseTxID: forfeitTx.TxHash(),
		ResponseTx:   forfeitTx,
	}

	result := a.Receive(t.Context(), notif)
	require.NoError(t, result.Err())

	require.Equal(t, forfeitJobState{
		phase: forfeitPhaseResponse,
		txid:  forfeitTx.TxHash(),
	}, a.forfeitsByOutpoint[forfeitedOutpoint],
		"legacy path must populate forfeitsByOutpoint after submit")

	// A subsequent on-chain VTXOOnChainNotification for the same outpoint
	// must dedup-skip via the entry the legacy path just wrote.
	a.forfeitsByOutpoint[forfeitedOutpoint] = forfeitJobState{
		phase: forfeitPhaseResponse,
		txid:  forfeitTx.TxHash(),
	}
	require.NoError(t, a.ensureForfeit(
		t.Context(), forfeitedOutpoint,
		&ResponsePlan{ResponseTx: forfeitTx},
	))
	require.Len(t, txConfirmRef.ensureReqs, 1,
		"VTXOOnChain path must skip when dedup map already populated")
}

// TestCheckpointSweepRetriesAfterTxConfirmAskFails verifies that when the
// initial txconfirm.Ask for a checkpoint sweep fails synchronously, fraud
// does NOT permanently mark the output as in-flight. A subsequent
// CheckpointSweepNotification (after batchwatcher's retry interval, say)
// can therefore re-attempt the sweep submission and succeed.
func TestCheckpointSweepRetriesAfterTxConfirmAskFails(t *testing.T) {
	t.Parallel()

	input, policy, operatorKey, signer, sweepInfo :=
		makeCheckpointSweepFixture(t)
	txConfirmRef := &recordingTxConfirmRef{}
	sweepTx := testSweepTx(sweepInfo.CheckpointTx.TxHash())

	a := newCheckpointActor(
		t, input, policy, operatorKey, signer, sweepInfo,
		txConfirmRef,
	)
	a.cfg.BuildSweep = func(context.Context,
		*CheckpointSweepRequest) (*wire.MsgTx, error) {

		return sweepTx, nil
	}

	// First ensure the checkpoint itself broadcasts so the actor's
	// state mirrors the post-checkpoint-confirm path that batchwatcher
	// would drive in production.
	result := a.Receive(t.Context(), onChainNotification(input))
	require.NoError(t, result.Err())
	require.Len(t, txConfirmRef.ensureReqs, 1)

	// Make txconfirm fail the next Ask synchronously, simulating a
	// transient backend error.
	txConfirmRef.failNext = fmt.Errorf("simulated txconfirm failure")

	checkpointOutpoint := wire.OutPoint{
		Hash:  sweepInfo.CheckpointTx.TxHash(),
		Index: 0,
	}
	notif := &batchwatcher.CheckpointSweepNotification{
		InputOutpoint:      input,
		CheckpointOutpoint: checkpointOutpoint,
		MaturityHeight:     110,
	}

	// First sweep attempt fails; recording shows no new ensure req
	// landed, and the actor must NOT have marked the output as
	// in-flight (otherwise the retry below would be silently deduped).
	result = a.Receive(t.Context(), notif)
	require.Error(t, result.Err(),
		"first attempt must surface the txconfirm error")
	require.Len(t, txConfirmRef.ensureReqs, 1,
		"failed attempt must not record a sweep ensure req")

	// Second attempt — same notification, no failure injected. Sweep
	// must now actually submit.
	result = a.Receive(t.Context(), notif)
	require.NoError(t, result.Err())
	require.Len(t, txConfirmRef.ensureReqs, 2,
		"retry must reach txconfirm once the transient error clears")
	require.Equal(
		t, CheckpointSweepLabel, txConfirmRef.ensureReqs[1].Label,
	)
}

// TestCheckpointTxFailedClearsDedup verifies that a terminal txconfirm
// failure for the checkpoint stage clears the per-txid dedup entry, so a
// subsequent VTXOOnChainNotification for the same input is allowed to
// re-submit. Without this clear, a single async failure permanently
// silences future notifications for that input within the daemon
// session.
func TestCheckpointTxFailedClearsDedup(t *testing.T) {
	t.Parallel()

	input, policy, operatorKey, signer, sweepInfo :=
		makeCheckpointSweepFixture(t)
	txConfirmRef := &recordingTxConfirmRef{}
	a := newCheckpointActor(
		t, input, policy, operatorKey, signer, sweepInfo,
		txConfirmRef,
	)

	// First submission lands and registers the dedup entry.
	result := a.Receive(t.Context(), onChainNotification(input))
	require.NoError(t, result.Err())
	require.Len(t, txConfirmRef.ensureReqs, 1)

	// Deliver a terminal TxFailed for the checkpoint txid. After this
	// the dedup entry must be gone so a re-notification can retry.
	checkpointTxid := sweepInfo.CheckpointTx.TxHash()
	result = a.Receive(t.Context(), &txconfirm.TxFailed{
		Txid:   checkpointTxid,
		Reason: "simulated terminal failure",
	})
	require.NoError(t, result.Err())

	// Re-deliver the same on-chain notification. The actor must
	// re-submit because the previous failed attempt was cleared.
	result = a.Receive(t.Context(), onChainNotification(input))
	require.NoError(t, result.Err())
	require.Len(t, txConfirmRef.ensureReqs, 2,
		"a TxFailed for the checkpoint stage must clear the dedup "+
			"index so a re-notification can retry")
}

// newCheckpointActor builds a fraud actor wired to fake checkpoint stores.
func newCheckpointActor(t *testing.T, input wire.OutPoint,
	policy arkscript.CheckpointPolicy, operatorKey keychain.KeyDescriptor,
	signer input.Signer, sweepInfo *CheckpointSweepInfo,
	txConfirmRef *recordingTxConfirmRef) *Actor {

	t.Helper()

	a, err := NewActor(Config{
		TxConfirmRef: txConfirmRef,
		CheckpointPlanner: &CheckpointPlanner{
			VTXOStore: spentVTXOStore(input),
			CheckpointLookup: &fakeCheckpointLookup{
				tx:    sweepInfo.CheckpointTx,
				found: true,
			},
			ForfeitLookup: &fakeForfeitLookup{},
			CheckpointSweepStore: &fakeCheckpointSweepStore{
				info:  sweepInfo,
				found: true,
			},
			CheckpointPolicy: policy,
		},
		CheckpointSweepStore: &fakeCheckpointSweepStore{
			info:  sweepInfo,
			found: true,
		},
		CheckpointPolicy: policy,
		OperatorKey:      operatorKey,
		Signer:           signer,
		NewSweepPkScript: func(context.Context) ([]byte, error) {
			return []byte{0x51}, nil
		},
	})
	require.NoError(t, err)

	notifRef := actor.NewChannelTellOnlyRef[txconfirm.Notification](
		"txconfirm-notify", 10,
	)
	a.SetNotificationRef(notifRef)

	return a
}

// newOnChainPlannerActor builds a fraud actor wired to a specific on-chain
// planner.
func newOnChainPlannerActor(t *testing.T, planner *CheckpointPlanner,
	txConfirmRef *recordingTxConfirmRef) *Actor {

	t.Helper()

	_, policy, operatorKey, signer, sweepInfo :=
		makeCheckpointSweepFixture(t)

	a, err := NewActor(Config{
		TxConfirmRef:      txConfirmRef,
		CheckpointPlanner: planner,
		CheckpointSweepStore: &fakeCheckpointSweepStore{
			info:  sweepInfo,
			found: true,
		},
		CheckpointPolicy: policy,
		OperatorKey:      operatorKey,
		Signer:           signer,
		NewSweepPkScript: func(context.Context) ([]byte, error) {
			return []byte{0x51}, nil
		},
	})
	require.NoError(t, err)

	notifRef := actor.NewChannelTellOnlyRef[txconfirm.Notification](
		"txconfirm-notify", 10,
	)
	a.SetNotificationRef(notifRef)

	return a
}

// makeCheckpointSweepFixture returns a finalized checkpoint and signing
// material for sweep tests.
func makeCheckpointSweepFixture(t *testing.T) (wire.OutPoint,
	arkscript.CheckpointPolicy, keychain.KeyDescriptor, input.Signer,
	*CheckpointSweepInfo) {

	t.Helper()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inputOutpoint := testOutpoint(99)
	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorPriv.PubKey(),
		CSVDelay:    10,
	}

	ownerLeaf, err := (&arkscript.Multisig{
		Keys: []*btcec.PublicKey{
			ownerPriv.PubKey(), operatorPriv.PubKey(),
		},
	}).Script()
	require.NoError(t, err)

	artifact, err := checkpointtx.BuildPSBT(
		policy, checkpointtx.Input{
			SpentVTXO: checkpointtx.SpentVTXORef{
				Outpoint: inputOutpoint,
				Output: &wire.TxOut{
					Value:    25_000,
					PkScript: []byte{0x51},
				},
			},
			OwnerLeafScript: ownerLeaf,
		},
	)
	require.NoError(t, err)

	checkpointTx := artifact.PSBT.UnsignedTx.Copy()
	sweepInfo := &CheckpointSweepInfo{
		InputOutpoint:         inputOutpoint,
		CheckpointTx:          checkpointTx,
		CheckpointOutputIndex: 0,
		CheckpointOutput:      checkpointTx.TxOut[0],
		TapTreeEncoded:        artifact.TapTreeEncoded,
	}

	operatorKey := keychain.KeyDescriptor{
		PubKey: operatorPriv.PubKey(),
	}
	signer := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorPriv}, nil,
	)

	return inputOutpoint, policy, operatorKey, signer, sweepInfo
}

// spentVTXOStore returns a fake VTXO store with one spent record.
func spentVTXOStore(outpoint wire.OutPoint) *fakeVTXOStore {
	return &fakeVTXOStore{
		records: map[wire.OutPoint]*batchwatcher.RecoveryVTXO{
			outpoint: {
				Outpoint: outpoint,
				Status:   batchwatcher.VTXOStatusSpent,
			},
		},
	}
}

// onChainNotification builds the batchwatcher notification under test.
func onChainNotification(
	outpoint wire.OutPoint) *batchwatcher.VTXOOnChainNotification {

	return &batchwatcher.VTXOOnChainNotification{
		VTXOOutpoint: outpoint,
		VTXOOutput: &wire.TxOut{
			Value:    25_000,
			PkScript: []byte{0x51},
		},
	}
}

// testSweepTx returns a minimal sweep-like transaction for actor tests.
func testSweepTx(checkpointTxid chainhash.Hash) *wire.MsgTx {
	tx := wire.NewMsgTx(arktx.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointTxid,
			Index: 0,
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    1_000,
		PkScript: []byte{0x51},
	})

	return tx
}

// TestValidateForfeitPlanRejectsMalformedShape exercises each rejection
// branch of validateForfeitPlan. The pre-broadcast bind keeps a tampered or
// malformed persisted forfeit tx from racing the operator's claim of the
// VTXO before the sweep guard ever fires.
func TestValidateForfeitPlanRejectsMalformedShape(t *testing.T) {
	t.Parallel()

	input := testOutpoint(0xfe)

	tests := []struct {
		name    string
		mutate  func(*wire.MsgTx)
		wantSub string
	}{{
		name:    "wrong version",
		mutate:  func(tx *wire.MsgTx) { tx.Version = 2 },
		wantSub: "version",
	}, {
		name:    "wrong input count",
		mutate:  func(tx *wire.MsgTx) { tx.TxIn = tx.TxIn[:1] },
		wantSub: "inputs, want 2",
	}, {
		name: "wrong input 0 prevout",
		mutate: func(tx *wire.MsgTx) {
			tx.TxIn[0].PreviousOutPoint = testOutpoint(0xff)
		},
		wantSub: "spends",
	}, {
		name:    "wrong output count",
		mutate:  func(tx *wire.MsgTx) { tx.TxOut = tx.TxOut[:1] },
		wantSub: "outputs, want 2",
	}, {
		name:    "non-positive penalty value",
		mutate:  func(tx *wire.MsgTx) { tx.TxOut[0].Value = 0 },
		wantSub: "not positive",
	}, {
		name:    "empty penalty pkScript",
		mutate:  func(tx *wire.MsgTx) { tx.TxOut[0].PkScript = nil },
		wantSub: "pkScript is empty",
	}, {
		name: "non-anchor at output 1",
		mutate: func(tx *wire.MsgTx) {
			tx.TxOut[1] = &wire.TxOut{
				Value: 1, PkScript: []byte{0x51},
			}
		},
		wantSub: "anchor",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tx := testForfeitTx(input, 24_000, []byte{0x51})
			tc.mutate(tx)

			err := validateForfeitPlan(input, tx)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

// TestValidateForfeitPlanAcceptsCanonicalShape verifies the canonical shape
// produced by testForfeitTx passes the strengthened validator.
func TestValidateForfeitPlanAcceptsCanonicalShape(t *testing.T) {
	t.Parallel()

	input := testOutpoint(0xee)
	tx := testForfeitTx(input, 24_000, []byte{0x51})

	require.NoError(t, validateForfeitPlan(input, tx))
}

// testForfeitTx constructs a canonical-shape forfeit tx for tests:
// version 3 (TRUC), 2 inputs (the forfeited VTXO + a placeholder connector
// input), 2 outputs (penalty + ephemeral anchor). Tests that exercise
// validateForfeitPlan must produce a forfeit tx in this shape.
func testForfeitTx(input wire.OutPoint, value int64,
	pkScript []byte) *wire.MsgTx {

	tx := wire.NewMsgTx(int32(arktx.TxVersion))
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: input})
	tx.AddTxIn(&wire.TxIn{})
	tx.AddTxOut(&wire.TxOut{Value: value, PkScript: pkScript})
	tx.AddTxOut(arkscript.AnchorOutput())

	return tx
}

// testOutpoint returns a deterministic outpoint for test maps.
func testOutpoint(seed byte) wire.OutPoint {
	var hash chainhash.Hash
	hash[0] = seed

	return wire.OutPoint{
		Hash:  hash,
		Index: uint32(seed),
	}
}

type fakeVTXOStore struct {
	records map[wire.OutPoint]*batchwatcher.RecoveryVTXO
}

// GetVTXO returns a fake persisted VTXO by outpoint.
func (s *fakeVTXOStore) GetVTXO(_ context.Context,
	outpoint wire.OutPoint) (*batchwatcher.RecoveryVTXO, error) {

	return s.records[outpoint], nil
}

type fakeCheckpointLookup struct {
	tx    *wire.MsgTx
	found bool
	err   error
}

// LoadCheckpointTxByInput returns the configured checkpoint lookup result.
func (l *fakeCheckpointLookup) LoadCheckpointTxByInput(
	context.Context, wire.OutPoint) (*wire.MsgTx, bool, error) {

	return l.tx, l.found, l.err
}

type fakeForfeitLookup struct {
	plans map[wire.OutPoint]*ResponsePlan
	err   error
}

// PlanForfeit returns a fake persisted forfeit plan by outpoint.
func (l *fakeForfeitLookup) PlanForfeit(_ context.Context,
	outpoint wire.OutPoint) (*ResponsePlan, error) {

	if l.err != nil {
		return nil, l.err
	}

	return l.plans[outpoint], nil
}

type fakeCheckpointSweepStore struct {
	info  *CheckpointSweepInfo
	found bool
	err   error
}

// LoadCheckpointSweepInfoByInput returns the configured sweep lookup result.
func (s *fakeCheckpointSweepStore) LoadCheckpointSweepInfoByInput(
	context.Context, wire.OutPoint) (*CheckpointSweepInfo, bool, error) {

	return s.info, s.found, s.err
}

type recordingTxConfirmRef struct {
	ensureReqs []*txconfirm.EnsureConfirmedReq

	// failNext, when non-nil, causes the next Ask to fail with this
	// error and clears itself. Used by failure-recovery tests to make
	// txconfirm reject the first attempt and accept the retry.
	failNext error
}

// ID returns the fake txconfirm actor identifier.
func (r *recordingTxConfirmRef) ID() string {
	return "recording-txconfirm"
}

// Tell accepts fire-and-forget txconfirm messages for interface parity.
func (r *recordingTxConfirmRef) Tell(context.Context, txconfirm.Msg) error {
	return nil
}

// Ask records txconfirm ensure requests and completes them immediately.
// When failNext is set, the next Ask returns that error without recording
// the request, then resets so subsequent calls succeed.
func (r *recordingTxConfirmRef) Ask(_ context.Context,
	msg txconfirm.Msg) actor.Future[txconfirm.Resp] {

	promise := actor.NewPromise[txconfirm.Resp]()

	req, ok := msg.(*txconfirm.EnsureConfirmedReq)
	if !ok {
		promise.Complete(fn.Err[txconfirm.Resp](
			fmt.Errorf("unexpected txconfirm msg %T", msg),
		))

		return promise.Future()
	}

	if r.failNext != nil {
		err := r.failNext
		r.failNext = nil
		promise.Complete(fn.Err[txconfirm.Resp](err))

		return promise.Future()
	}

	r.ensureReqs = append(r.ensureReqs, req)
	promise.Complete(fn.Ok[txconfirm.Resp](
		&txconfirm.EnsureConfirmedResp{
			Txid:    req.Tx.TxHash(),
			State:   txconfirm.TxStateAwaitingConfirmation,
			Created: true,
		},
	))

	return promise.Future()
}
