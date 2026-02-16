package batchsweeper

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/cucumber/godog"
	"github.com/cucumber/godog/colors"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	treepkg "github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

const (
	bddGivenWatcherExists = "^a BatchSweeper with watcher reporting " +
		"the batch exists$"
	bddGivenWatcherMissing = "^a BatchSweeper with watcher reporting the " +
		"batch does not exist$"
	bddGivenMatureOutput = "^a BatchSweeper with a mature operator output$"

	bddWhenWatcherNotifiesExpiry = "^BatchWatcher notifies a batch expiry$"

	bddThenWatcherQueried = "^BatchWatcher should be queried for the " +
		"batch tree state$"
	bddThenSweepBroadcast = "^a sweep transaction should be broadcast$"
	bddThenNoError        = "^no BatchSweeper error should occur$"
)

// completedWatcherFuture returns a Future that is already completed with the
// given BatchWatcher response.
func completedWatcherFuture(
	resp batchwatcher.BatchWatcherResp,
) actor.Future[batchwatcher.BatchWatcherResp] {

	promise := actor.NewPromise[batchwatcher.BatchWatcherResp]()
	promise.Complete(fn.Ok(resp))

	return promise.Future()
}

// mockWatcher is a test double for the BatchWatcher actor reference.
type mockWatcher struct {
	mu sync.Mutex

	resp    batchwatcher.BatchWatcherResp
	lastAsk batchwatcher.BatchWatcherMsg
}

// ID returns the ID of the mock watcher.
func (m *mockWatcher) ID() string {
	return "mock-watcher"
}

// Tell is a no-op for this mock.
func (m *mockWatcher) Tell(_ context.Context,
	_ batchwatcher.BatchWatcherMsg) error {

	return nil
}

// Ask captures the request and returns the preconfigured response.
func (m *mockWatcher) Ask(_ context.Context,
	msg batchwatcher.BatchWatcherMsg) batchWatcherFuture {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.lastAsk = msg

	return completedWatcherFuture(m.resp)
}

// LastAsk returns the last Ask message captured by the mock.
func (m *mockWatcher) LastAsk() batchwatcher.BatchWatcherMsg {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.lastAsk
}

// completedChainSourceFuture returns a Future that is already completed with
// the given ChainSource response.
func completedChainSourceFuture(
	resp chainsource.ChainSourceResp,
) actor.Future[chainsource.ChainSourceResp] {

	promise := actor.NewPromise[chainsource.ChainSourceResp]()
	promise.Complete(fn.Ok(resp))

	return promise.Future()
}

// mockChainSource is a test double for the ChainSource actor reference that can
// return static values and capture broadcast requests.
type mockChainSource struct {
	mu sync.Mutex

	bestHeight int32
	feeRate    btcutil.Amount

	lastBroadcast *chainsource.BroadcastTxRequest
}

// ID returns the ID of the mock chain source.
func (m *mockChainSource) ID() string {
	return "mock-chain-source"
}

// Tell is a no-op for this mock.
func (m *mockChainSource) Tell(_ context.Context,
	_ chainsource.ChainSourceMsg) error {

	return nil
}

// Ask handles supported ChainSource queries and records broadcasts.
func (m *mockChainSource) Ask(_ context.Context,
	msg chainsource.ChainSourceMsg) chainSourceFuture {

	m.mu.Lock()
	defer m.mu.Unlock()

	switch req := msg.(type) {
	case *chainsource.BestHeightRequest:
		return completedChainSourceFuture(
			&chainsource.BestHeightResponse{
				Height: m.bestHeight,
			},
		)

	case *chainsource.FeeEstimateRequest:
		return completedChainSourceFuture(
			&chainsource.FeeEstimateResponse{
				SatPerVByte: m.feeRate,
			},
		)

	case *chainsource.BroadcastTxRequest:
		m.lastBroadcast = req

		return completedChainSourceFuture(
			&chainsource.BroadcastTxResponse{},
		)

	default:
		return completedChainSourceFuture(
			&chainsource.BroadcastTxResponse{},
		)
	}
}

// LastBroadcast returns the last captured broadcast request.
func (m *mockChainSource) LastBroadcast() *chainsource.BroadcastTxRequest {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.lastBroadcast
}

// selfRef is a TellOnlyRef that forwards messages into the actor under test.
type selfRef struct {
	actor *Actor

	mu      sync.Mutex
	lastErr error
}

// ID returns the ID of the self reference.
func (r *selfRef) ID() string {
	return "batchsweeper-self"
}

// Tell forwards the message into the actor and records any error.
func (r *selfRef) Tell(ctx context.Context, msg Msg) error {
	result := r.actor.Receive(ctx, msg)
	if result.IsErr() {
		r.mu.Lock()
		r.lastErr = result.Err()
		r.mu.Unlock()
	}

	return nil
}

// LastErr returns the last actor error observed through this ref.
func (r *selfRef) LastErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.lastErr
}

type bddContext struct {
	t *testing.T

	batchID batchwatcher.BatchID

	actor   *Actor
	selfRef *selfRef
	watcher *mockWatcher
	chain   *mockChainSource
	notify  actor.TellOnlyRef[batchwatcher.BatchSweeperMsg]
}

// aBatchSweeperWithWatcherFound sets up a BatchSweeper with a watcher response.
func (bc *bddContext) aBatchSweeperWithWatcherFound(found bool) error {
	bc.batchID = batchwatcher.BatchID(uuid.New())

	bc.chain = &mockChainSource{
		bestHeight: 200,
		feeRate:    btcutil.Amount(1),
	}

	var treeState *batchwatcher.BatchTreeState
	if found {
		treeState = &batchwatcher.BatchTreeState{
			ExistingOutputs: make(
				map[wire.OutPoint]*batchwatcher.Output,
			),
		}
	}

	bc.watcher = &mockWatcher{
		resp: &batchwatcher.GetTreeStateResponse{
			Found:     found,
			TreeState: treeState,
		},
	}

	cfg := &ActorConfig{
		Logger:       btclog.Disabled,
		BatchWatcher: bc.watcher,
		ChainSource:  bc.chain,
		SweepDelay:   10,
		BuildSweepTx: func(_ []*batchwatcher.Output,
			_ btcutil.Amount) (*wire.MsgTx, error) {

			return wire.NewMsgTx(2), nil
		},
	}

	bc.actor = NewActor(cfg)
	bc.selfRef = &selfRef{
		actor: bc.actor,
	}
	cfg.SelfRef = bc.selfRef

	bc.notify = MapBatchWatcherNotification(bc.selfRef)

	return nil
}

// aBatchSweeperWithMatureOutput sets up a BatchSweeper where the watcher
// returns a tree state with one mature operator-controlled output.
func (bc *bddContext) aBatchSweeperWithMatureOutput() error {
	bc.batchID = batchwatcher.BatchID(uuid.New())

	bc.chain = &mockChainSource{
		bestHeight: 200,
		feeRate:    btcutil.Amount(1),
	}

	internalKey, _ := testutils.CreateKey(1)
	node := &treepkg.Node{
		CoSigners: []*btcec.PublicKey{internalKey},
	}

	txOut := wire.NewTxOut(1000, []byte{0x51})

	outpoint := wire.OutPoint{Index: 0}
	treeState := &batchwatcher.BatchTreeState{
		ExistingOutputs: map[wire.OutPoint]*batchwatcher.Output{
			outpoint: {
				Outpoint:        outpoint,
				TxOut:           txOut,
				ConfirmedHeight: 100,
				IsVTXO:          false,
				TreeNode:        node,
				OutputIndex:     0,
			},
		},
	}

	bc.watcher = &mockWatcher{
		resp: &batchwatcher.GetTreeStateResponse{
			Found:     true,
			TreeState: treeState,
		},
	}

	cfg := &ActorConfig{
		Logger:       btclog.Disabled,
		BatchWatcher: bc.watcher,
		ChainSource:  bc.chain,
		SweepDelay:   10,
		BuildSweepTx: func(candidates []*batchwatcher.Output,
			_ btcutil.Amount) (*wire.MsgTx, error) {

			tx := wire.NewMsgTx(2)
			for _, output := range candidates {
				tx.AddTxIn(&wire.TxIn{
					PreviousOutPoint: output.Outpoint,
				})
			}

			tx.AddTxOut(wire.NewTxOut(1, []byte{0x51}))

			return tx, nil
		},
	}

	bc.actor = NewActor(cfg)
	bc.selfRef = &selfRef{
		actor: bc.actor,
	}
	cfg.SelfRef = bc.selfRef

	bc.notify = MapBatchWatcherNotification(bc.selfRef)

	return nil
}

// batchWatcherNotifiesExpiry sends an expiry notification through the adapter.
func (bc *bddContext) batchWatcherNotifiesExpiry() error {
	notification := &batchwatcher.BatchExpiredNotification{
		BatchID:      bc.batchID,
		ExpiryHeight: 123,
	}

	return bc.notify.Tell(context.Background(), notification)
}

// aSweepTransactionShouldBeBroadcast verifies the ChainSource received a
// broadcast request.
func (bc *bddContext) aSweepTransactionShouldBeBroadcast() error {
	broadcast := bc.chain.LastBroadcast()
	if broadcast == nil {
		return fmt.Errorf("expected a broadcast request")
	}

	if broadcast.Tx == nil {
		return fmt.Errorf("broadcast tx is nil")
	}

	if len(broadcast.Tx.TxIn) == 0 {
		return fmt.Errorf("broadcast tx has no inputs")
	}

	return nil
}

// watcherShouldBeQueried verifies the BatchSweeper queried the watcher.
func (bc *bddContext) watcherShouldBeQueried() error {
	ask := bc.watcher.LastAsk()
	req, ok := ask.(*batchwatcher.GetTreeStateRequest)
	if !ok {
		return fmt.Errorf("expected GetTreeStateRequest, got %T", ask)
	}

	if req.BatchID != bc.batchID {
		return fmt.Errorf("wrong batch id: got %s, want %s",
			req.BatchID, bc.batchID)
	}

	return nil
}

// noErrorShouldOccur verifies the actor did not return an error.
func (bc *bddContext) noErrorShouldOccur() error {
	if err := bc.selfRef.LastErr(); err != nil {
		return fmt.Errorf("unexpected error: %w", err)
	}

	return nil
}

// TestFeatures runs the batchsweeper Gherkin scenarios.
func TestFeatures(t *testing.T) {
	opts := godog.Options{
		Format:   "pretty",
		Paths:    []string{"features"},
		TestingT: t,
		Output:   colors.Colored(os.Stdout),
	}

	initFunc := func(ctx *godog.ScenarioContext) {
		var bc *bddContext

		ctx.Before(func(
			gctx context.Context, _ *godog.Scenario,
		) (context.Context, error) {

			bc = &bddContext{t: t}

			return gctx, nil
		})

		ctx.Given(bddGivenWatcherExists,
			func() error {
				return bc.aBatchSweeperWithWatcherFound(true)
			})
		ctx.Given(bddGivenWatcherMissing,
			func() error {
				return bc.aBatchSweeperWithWatcherFound(false)
			})
		ctx.Given(bddGivenMatureOutput,
			func() error {
				return bc.aBatchSweeperWithMatureOutput()
			})
		ctx.When(bddWhenWatcherNotifiesExpiry,
			func() error {
				return bc.batchWatcherNotifiesExpiry()
			})
		ctx.Then(bddThenWatcherQueried,
			func() error {
				return bc.watcherShouldBeQueried()
			})
		ctx.Then(bddThenSweepBroadcast,
			func() error {
				return bc.aSweepTransactionShouldBeBroadcast()
			})
		ctx.Then(bddThenNoError,
			func() error {
				return bc.noErrorShouldOccur()
			})
	}

	status := godog.TestSuite{
		Name:                "batchsweeper",
		ScenarioInitializer: initFunc,
		Options:             &opts,
	}.Run()

	require.Equal(t, 0, status, "BDD tests failed")
}
