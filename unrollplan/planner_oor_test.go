package unrollplan

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

const (
	oorPlanStartHeight int32  = 100
	oorCSVDelay        uint32 = 5
)

// TestPlanDeepOORChainAlternatesCheckpointAndArk verifies the concrete
// depth-2 OOR recovery sequence. After the round-tree root confirms, each
// checkpoint is ready only before its corresponding Ark child, and the next
// checkpoint waits for that Ark transaction to confirm.
func TestPlanDeepOORChainAlternatesCheckpointAndArk(t *testing.T) {
	fixture := oorChainProofFixture(t, 2)
	planner, proof := newPlannerFixture(t, fixture.proof)
	state := &State{}

	for index, txid := range fixture.broadcastOrder {
		planHeight := oorPlanStartHeight + int32(index)
		snapshot, err := planner.Plan(planHeight, state)
		require.NoError(t, err)
		require.Len(t, snapshot.Ready, 1)
		require.Equal(t, txid, snapshot.Ready[0].Txid)
		require.Equal(
			t, fixture.kindByTxid[txid],
			snapshot.Ready[0].Node.Kind,
		)

		assertOORChildBlockedWhileParentInFlight(
			t, planner, fixture, state, index, planHeight,
		)

		state.ConfirmedTxids = append(state.ConfirmedTxids, txid)
		if txid == proof.TargetOutpoint().Hash {
			state.TargetConfirmHeight = fn.Some(planHeight)
		}
	}

	maturityHeight := state.TargetConfirmHeight.UnwrapOrFail(t) +
		int32(oorCSVDelay)
	snapshot, err := planner.Plan(maturityHeight, state)
	require.NoError(t, err)
	require.True(t, snapshot.AllProofConfirmed)
	require.True(t, snapshot.TargetConfirmed)
	require.True(t, snapshot.NeedSweep)
}

// TestPlanOORChainOrderingRapid checks the same ordering invariant over
// random OOR depths. Every unconfirmed child remains blocked while its parent
// is merely in flight, then becomes ready only after the parent is confirmed.
func TestPlanOORChainOrderingRapid(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		depth := rapid.IntRange(1, 5).Draw(t, "depth")
		fixture := oorChainProofFixture(t, depth)
		planner, err := NewPlanner(fixture.proof)
		require.NoError(t, err)
		proof := fixture.proof
		resumeIndex := rapid.IntRange(
			0, len(fixture.broadcastOrder)-1,
		).Draw(t, "resumeIndex")
		confirmed := append(
			[]chainhash.Hash(nil),
			fixture.broadcastOrder[:resumeIndex]...,
		)
		if rapid.Bool().Draw(t, "reverseConfirmedOrder") {
			reverseHashes(confirmed)
		}
		startHeight := rapid.Int32Range(0, 1_000_000).Draw(
			t, "startHeight",
		)
		state := &State{
			ConfirmedTxids: confirmed,
		}
		numBroadcasts := len(fixture.broadcastOrder)
		targetConfirmHeight := int32(0)

		for index := resumeIndex; index < numBroadcasts; index++ {
			txid := fixture.broadcastOrder[index]
			planHeight := startHeight + int32(index-resumeIndex)
			snapshot, err := planner.Plan(planHeight, state)
			require.NoError(t, err)
			require.Len(t, snapshot.Ready, 1)
			require.Equal(t, txid, snapshot.Ready[0].Txid)
			require.Equal(
				t, fixture.kindByTxid[txid],
				snapshot.Ready[0].Node.Kind,
			)

			assertOORChildBlockedWhileParentInFlight(
				t, planner, fixture, state, index, planHeight,
			)

			state.ConfirmedTxids = append(
				state.ConfirmedTxids, txid,
			)
			if txid == proof.TargetOutpoint().Hash {
				targetConfirmHeight = planHeight
				state.TargetConfirmHeight = fn.Some(planHeight)
			}
		}

		snapshot, err := planner.Plan(
			targetConfirmHeight+int32(oorCSVDelay), state,
		)
		require.NoError(t, err)
		require.True(t, snapshot.AllProofConfirmed)
		require.True(t, snapshot.TargetConfirmed)
		require.True(t, snapshot.NeedSweep)
	})
}

// TestPlanOORChainRejectsOutOfOrderInFlightState verifies the planner refuses
// a malformed durable state that claims an Ark tx is in flight before its
// checkpoint parent has confirmed.
func TestPlanOORChainRejectsOutOfOrderInFlightState(t *testing.T) {
	fixture := oorChainProofFixture(t, 2)
	planner, _ := newPlannerFixture(t, fixture.proof)

	// The generic in-flight parent invariant is covered elsewhere; this
	// pins the same guard to the OOR-specific checkpoint/Ark topology.
	ark1 := fixture.broadcastOrder[2]
	_, err := planner.Plan(100, &State{
		ConfirmedTxids: []chainhash.Hash{
			fixture.broadcastOrder[0],
		},
		InFlightTxids: []chainhash.Hash{ark1},
	})
	require.ErrorContains(t, err, "in-flight tx")
	require.ErrorContains(t, err, "unconfirmed parents")
}

type oorChainFixture struct {
	proof          *recovery.Proof
	broadcastOrder []chainhash.Hash
	kindByTxid     map[chainhash.Hash]recovery.NodeKind
}

func oorChainProofFixture(t require.TestingT, depth int) *oorChainFixture {
	require.GreaterOrEqual(t, depth, 1)

	root := newTx(nil, 1, "oor-root")
	nodes := []*recovery.Node{{
		Kind: recovery.NodeKindTree,
		Tx:   root,
	}}
	order := []chainhash.Hash{root.TxHash()}
	kindByTxid := map[chainhash.Hash]recovery.NodeKind{
		root.TxHash(): recovery.NodeKindTree,
	}

	prevOutpoint := wire.OutPoint{
		Hash:  root.TxHash(),
		Index: 0,
	}
	for hop := 1; hop <= depth; hop++ {
		checkpoint := newTx(
			[]wire.OutPoint{prevOutpoint}, 1,
			fmt.Sprintf("checkpoint-%d", hop),
		)
		checkpointNode := &recovery.Node{
			Kind: recovery.NodeKindCheckpoint,
			Tx:   checkpoint,
		}
		nodes = append(nodes, checkpointNode)

		checkpointTxid := checkpoint.TxHash()
		order = append(order, checkpointTxid)
		kindByTxid[checkpointTxid] = recovery.NodeKindCheckpoint

		ark := newTx([]wire.OutPoint{{
			Hash:  checkpointTxid,
			Index: 0,
		}}, 1, fmt.Sprintf("ark-%d", hop))
		arkNode := &recovery.Node{
			Kind: recovery.NodeKindArk,
			Tx:   ark,
		}
		nodes = append(nodes, arkNode)

		arkTxid := ark.TxHash()
		order = append(order, arkTxid)
		kindByTxid[arkTxid] = recovery.NodeKindArk

		prevOutpoint = wire.OutPoint{
			Hash:  arkTxid,
			Index: 0,
		}
	}

	proof, err := recovery.NewProof(prevOutpoint, oorCSVDelay, nodes...)
	require.NoError(t, err)

	return &oorChainFixture{
		proof:          proof,
		broadcastOrder: order,
		kindByTxid:     kindByTxid,
	}
}

func assertOORChildBlockedWhileParentInFlight(t require.TestingT,
	planner *Planner, fixture *oorChainFixture, state *State, index int,
	height int32) {

	txid := fixture.broadcastOrder[index]
	inFlightState := &State{
		ConfirmedTxids: append(
			[]chainhash.Hash(nil), state.ConfirmedTxids...,
		),
		InFlightTxids: []chainhash.Hash{
			txid,
		},
		TargetConfirmHeight: state.TargetConfirmHeight,
		Sweep:               state.Sweep,
	}
	snapshot, err := planner.Plan(height, inFlightState)
	require.NoError(t, err)
	require.Len(t, snapshot.Ready, 0)
	require.Len(t, snapshot.InFlight, 1)
	require.Equal(t, txid, snapshot.InFlight[0].Txid)

	if index == len(fixture.broadcastOrder)-1 {
		return
	}

	childTxid := fixture.broadcastOrder[index+1]
	for _, blocked := range snapshot.Blocked {
		if blocked.Txid != childTxid {
			continue
		}

		require.Equal(t, []chainhash.Hash{txid},
			blocked.MissingParents)

		return
	}

	require.Failf(
		t, "child not blocked", "child %s was not blocked on parent %s",
		childTxid, txid,
	)
}

func reverseHashes(hashes []chainhash.Hash) {
	for left, right := 0, len(hashes)-1; left < right; {
		hashes[left], hashes[right] = hashes[right], hashes[left]
		left++
		right--
	}
}
