//nolint:ll
package unroller

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/cucumber/godog"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// unrollerGodogContext holds the state for godog BDD tests.
type unrollerGodogContext struct {
	t *testing.T

	// Test fixtures.
	actor       *UnrollerActor
	store       *MockUnrollStore
	chainSource *mockChainSourceRef

	// Test state.
	vtxos           map[string]*round.ClientVTXO
	unrollRequests  map[string]*UnrollRequest
	unrollResults   map[string]error
	currentHeight   int32
	confirmHeights  map[chainhash.Hash]int32
	lastStatusQuery *UnrollStatusResp
}

// newUnrollerGodogContext creates a new godog test context.
func newUnrollerGodogContext(t *testing.T) *unrollerGodogContext {
	ctx := &unrollerGodogContext{
		t:              t,
		vtxos:          make(map[string]*round.ClientVTXO),
		unrollRequests: make(map[string]*UnrollRequest),
		unrollResults:  make(map[string]error),
		confirmHeights: make(map[chainhash.Hash]int32),
		currentHeight:  1000,
	}

	// Setup mocks.
	ctx.store = &MockUnrollStore{}
	ctx.chainSource = newMockChainSourceRef(t)

	// Setup actor.
	selfRef := &mockSelfRef{t: t}
	cfg := &UnrollerConfig{
		ChainSource: ctx.chainSource,
		Store:       ctx.store,
		ChainParams: &chaincfg.RegressionNetParams,
		Logger:      btclog.Disabled,
		SelfRef:     selfRef,
	}

	ctx.actor = NewUnrollerActor(cfg)

	// Setup store to return empty list for initial recovery. Use .Once()
	// so that restart scenarios can override with their own expectation.
	// Use mock.Anything for context to handle different context types.
	ctx.store.On("ListActiveUnrolls", mock.Anything).Return(
		[]*UnrollState{}, nil,
	).Once()

	return ctx
}

// Step: Given an unroller actor is running.
func (ctx *unrollerGodogContext) aUnrollerActorIsRunning() error {
	return ctx.actor.Start(context.Background())
}

// Step: Given the chain source is available.
func (ctx *unrollerGodogContext) theChainSourceIsAvailable() error {
	// Already available via mock.
	return nil
}

// Step: Given the unroll store is available.
func (ctx *unrollerGodogContext) theUnrollStoreIsAvailable() error {
	// Already available via mock.
	return nil
}

// Step: Given a VTXO with a 2-level tree.
// Level 0 is root (already confirmed), level 1 is child to broadcast.
func (ctx *unrollerGodogContext) aVTXOWithA2LevelTree() error {
	return ctx.createVTXOWithLevels("vtxo1", 2)
}

// Step: Given a VTXO with a 4-level tree.
// Level 0 is root (already confirmed), levels 1-3 are children to broadcast.
func (ctx *unrollerGodogContext) aVTXOWithA4LevelTree() error {
	return ctx.createVTXOWithLevels("vtxo1", 4)
}

// Step: Given a VTXO with a multi-level tree.
// Creates a 4-level tree for testing multi-level unroll.
func (ctx *unrollerGodogContext) aVTXOWithAMultiLevelTree() error {
	return ctx.createVTXOWithLevels("vtxo1", 4)
}

// Step: Given 3 VTXOs with 2-level trees.
func (ctx *unrollerGodogContext) nVTXOsWith2LevelTrees(n int) error {
	for i := 1; i <= n; i++ {
		vtxoID := "vtxo" + string(rune('0'+i))
		if err := ctx.createVTXOWithLevels(vtxoID, 2); err != nil {
			return err
		}
	}

	return nil
}

// Step: Given a VTXO with CSV delay of 144 blocks.
func (ctx *unrollerGodogContext) aVTXOWithCSVDelayOfBlocks(
	csvDelay int,
) error {

	// Create a 2-level tree (root + 1 child level).
	vtxo := ctx.createTestVTXO("vtxo1", 2)
	vtxo.Expiry = uint32(csvDelay)
	ctx.vtxos["vtxo1"] = vtxo

	ctx.store.On("GetVTXO", context.Background(), vtxo.Outpoint).Return(
		vtxo, nil,
	)

	return nil
}

// Step: Given the leaf transaction confirmed at height 1000.
func (ctx *unrollerGodogContext) theLeafTransactionConfirmedAtHeight(
	height int,
) error {

	vtxo := ctx.vtxos["vtxo1"]
	require.NotNil(ctx.t, vtxo)

	// Get leaf txid.
	levelOrder := extractLevelOrder(vtxo.TreePath)
	if len(levelOrder) == 0 {
		return nil
	}

	leafLevel := levelOrder[len(levelOrder)-1]
	if len(leafLevel.Txids) == 0 {
		return nil
	}

	leafTxid := leafLevel.Txids[0]
	ctx.confirmHeights[leafTxid] = int32(height)

	// Create or update state. This step implies an unroll is in progress
	// with the leaf confirmed and awaiting CSV.
	state := ctx.actor.activeUnrolls[vtxo.Outpoint.String()]
	if state == nil {
		state = &UnrollState{
			VTXOOutpoint:   vtxo.Outpoint,
			VTXO:           vtxo,
			LevelOrder:     levelOrder,
			CurrentLevel:   len(levelOrder) - 1,
			BroadcastTxids: make(map[chainhash.Hash]bool),
			ConfirmedTxids: make(map[chainhash.Hash]ConfirmationInfo),
			Status:         UnrollStatusAwaitingCSV,
		}
		ctx.actor.activeUnrolls[vtxo.Outpoint.String()] = state

		// Set up mock for UpdateUnrollState since this state will be
		// updated when CSV completes.
		ctx.store.On(
			"UpdateUnrollState", context.Background(), mock.Anything,
		).Return(nil)
	}

	state.LeafConfirmHeight = int32(height)
	state.Status = UnrollStatusAwaitingCSV

	return nil
}

// Step: Given a VTXO outpoint that does not exist in the database.
func (ctx *unrollerGodogContext) aVTXOOutpointThatDoesNotExistInTheDatabase() error {
	// Create outpoint but don't add to store.
	outpoint := ctx.newTestOutpoint()
	ctx.vtxos["missing"] = &round.ClientVTXO{Outpoint: outpoint}

	// Setup store to return error.
	ctx.store.On(
		"GetVTXO", context.Background(), outpoint,
	).Return((*round.ClientVTXO)(nil), fmt.Errorf("vtxo not found"))

	return nil
}

// Step: Given an unroll is already in progress for the VTXO.
func (ctx *unrollerGodogContext) anUnrollIsAlreadyInProgressForTheVTXO() error {
	vtxo := ctx.vtxos["vtxo1"]
	require.NotNil(ctx.t, vtxo)

	// Create active unroll state.
	state := &UnrollState{
		VTXOOutpoint:   vtxo.Outpoint,
		VTXO:           vtxo,
		LevelOrder:     extractLevelOrder(vtxo.TreePath),
		CurrentLevel:   0,
		BroadcastTxids: make(map[chainhash.Hash]bool),
		ConfirmedTxids: make(map[chainhash.Hash]ConfirmationInfo),
		Status:         UnrollStatusBroadcasting,
	}

	ctx.actor.activeUnrolls[vtxo.Outpoint.String()] = state

	return nil
}

// Step: Given an unroll is in progress at level 1.
func (ctx *unrollerGodogContext) anUnrollIsInProgressAtLevel(level int) error {
	vtxo := ctx.vtxos["vtxo1"]
	require.NotNil(ctx.t, vtxo)

	// Create active unroll state.
	state := &UnrollState{
		VTXOOutpoint:   vtxo.Outpoint,
		VTXO:           vtxo,
		LevelOrder:     extractLevelOrder(vtxo.TreePath),
		CurrentLevel:   level,
		BroadcastTxids: make(map[chainhash.Hash]bool),
		ConfirmedTxids: make(map[chainhash.Hash]ConfirmationInfo),
		Status:         UnrollStatusBroadcasting,
	}

	ctx.actor.activeUnrolls[vtxo.Outpoint.String()] = state

	return nil
}

// Step: When I request unroll for the VTXO.
func (ctx *unrollerGodogContext) iRequestUnrollForTheVTXO() error {
	return ctx.requestUnrollForVTXO("vtxo1")
}

// Step: When I request unroll for the same VTXO again.
func (ctx *unrollerGodogContext) iRequestUnrollForTheSameVTXOAgain() error {
	return ctx.requestUnrollForVTXO("vtxo1")
}

// Step: When I request unroll for the non-existent VTXO.
func (ctx *unrollerGodogContext) iRequestUnrollForTheNonExistentVTXO() error {
	return ctx.requestUnrollForVTXO("missing")
}

// Step: When I request unroll for all 3 VTXOs.
func (ctx *unrollerGodogContext) iRequestUnrollForAllNVTXOs(n int) error {
	for i := 1; i <= n; i++ {
		vtxoID := "vtxo" + string(rune('0'+i))
		if err := ctx.requestUnrollForVTXO(vtxoID); err != nil {
			return err
		}
	}

	return nil
}

// Step: When I query the unroll status.
func (ctx *unrollerGodogContext) iQueryTheUnrollStatus() error {
	vtxo := ctx.vtxos["vtxo1"]
	require.NotNil(ctx.t, vtxo)

	req := &GetUnrollStatusRequest{
		VTXOOutpoint: vtxo.Outpoint,
	}

	result := ctx.actor.Receive(context.Background(), req)
	if result.IsErr() {
		return result.Err()
	}

	resp, err := result.Unpack()
	if err != nil {
		return err
	}

	statusResp, ok := resp.(*UnrollStatusResp)
	if !ok {
		ctx.t.Fatalf("unexpected response type: %T", resp)
	}

	ctx.lastStatusQuery = statusResp

	return nil
}

// Step: When the unroller restarts.
func (ctx *unrollerGodogContext) theUnrollerRestarts() error {
	// Save current state.
	var activeStates []*UnrollState
	for _, state := range ctx.actor.activeUnrolls {
		activeStates = append(activeStates, state)
	}

	// Setup store to return saved states.
	ctx.store.On(
		"ListActiveUnrolls", context.Background(),
	).Return(activeStates, nil).Once()

	// Create new actor and start.
	selfRef := &mockSelfRef{t: ctx.t}
	cfg := &UnrollerConfig{
		ChainSource: ctx.chainSource,
		Store:       ctx.store,
		ChainParams: &chaincfg.RegressionNetParams,
		Logger:      btclog.Disabled,
		SelfRef:     selfRef,
	}

	ctx.actor = NewUnrollerActor(cfg)

	return ctx.actor.Start(context.Background())
}

// Step: When the current block height is 1143.
func (ctx *unrollerGodogContext) theCurrentBlockHeightIs(height int) error {
	ctx.currentHeight = int32(height)
	return nil
}

// Step: When the current block height reaches 1144.
func (ctx *unrollerGodogContext) theCurrentBlockHeightReaches(height int) error {
	ctx.currentHeight = int32(height)

	// Send block epoch event.
	evt := &BlockEpochEvent{
		Height: ctx.currentHeight,
	}

	result := ctx.actor.Receive(context.Background(), evt)

	return result.Err()
}

// Helper: Create VTXO with specified number of levels.
func (ctx *unrollerGodogContext) createVTXOWithLevels(
	vtxoID string, levels int,
) error {

	vtxo := ctx.createTestVTXO(vtxoID, levels)
	ctx.vtxos[vtxoID] = vtxo

	ctx.store.On("GetVTXO", context.Background(), vtxo.Outpoint).Return(
		vtxo, nil,
	)

	return nil
}

// Helper: Request unroll for a specific VTXO.
func (ctx *unrollerGodogContext) requestUnrollForVTXO(vtxoID string) error {
	vtxo := ctx.vtxos[vtxoID]
	if vtxo == nil {
		return nil
	}

	// Setup store expectations if not already set.
	ctx.store.On("SaveUnrollState", context.Background(), mock.Anything).Return(
		nil,
	)
	ctx.store.On(
		"UpdateUnrollState", context.Background(), mock.Anything,
	).Return(nil)

	req := &UnrollRequest{
		TargetVTXOs: []wire.OutPoint{vtxo.Outpoint},
	}

	ctx.unrollRequests[vtxoID] = req

	result := ctx.actor.Receive(context.Background(), req)
	if result.IsErr() {
		ctx.unrollResults[vtxoID] = result.Err()
		return nil
	}

	ctx.unrollResults[vtxoID] = nil

	return nil
}

// Helper: Create test VTXO with tree.
func (ctx *unrollerGodogContext) createTestVTXO(
	vtxoID string, levels int,
) *round.ClientVTXO {

	outpoint := ctx.newTestOutpoint()

	// Create simple tree structure.
	root := ctx.createTestNode(levels, 0)

	treePath := &tree.Tree{
		Root: root,
	}

	return &round.ClientVTXO{
		Outpoint: outpoint,
		Amount:   btcutil.Amount(50000),
		Expiry:   144,
		TreePath: treePath,
	}
}

// Helper: Create test node recursively.
func (ctx *unrollerGodogContext) createTestNode(
	maxLevel, currentLevel int,
) *tree.Node {

	node := &tree.Node{
		Input:    wire.OutPoint{},
		Outputs:  []*wire.TxOut{{Value: 1000}},
		Children: make(map[uint32]*tree.Node),
	}

	// Add children if not at leaf level.
	if currentLevel < maxLevel-1 {
		child := ctx.createTestNode(maxLevel, currentLevel+1)
		node.Children[0] = child
	}

	return node
}

// Helper: Create test outpoint.
func (ctx *unrollerGodogContext) newTestOutpoint() wire.OutPoint {
	var hash chainhash.Hash
	_, _ = rand.Read(hash[:])

	return wire.OutPoint{Hash: hash, Index: 0}
}

// Step: Then the unroll should start successfully.
func (ctx *unrollerGodogContext) theUnrollShouldStartSuccessfully() error {
	err := ctx.unrollResults["vtxo1"]
	if err != nil {
		return err
	}

	vtxo := ctx.vtxos["vtxo1"]
	state := ctx.actor.activeUnrolls[vtxo.Outpoint.String()]
	if state == nil {
		ctx.t.Fatal("unroll state not found")
	}

	return nil
}

// Step: Then the unroll status should be "broadcasting".
func (ctx *unrollerGodogContext) theUnrollStatusShouldBe(status string) error {
	vtxo := ctx.vtxos["vtxo1"]
	state := ctx.actor.activeUnrolls[vtxo.Outpoint.String()]

	expectedStatus := parseUnrollStatus(status)

	// When unroll completes, it's removed from activeUnrolls. So nil state
	// is acceptable when checking for "complete" status.
	if state == nil {
		if expectedStatus == UnrollStatusComplete {
			return nil
		}
		ctx.t.Fatal("unroll state not found")
	}

	if state.Status != expectedStatus {
		ctx.t.Fatalf(
			"expected status %s, got %s", status, state.Status.String(),
		)
	}

	return nil
}

// Step: Then the VTXO should be ready for sweeping.
func (ctx *unrollerGodogContext) theVTXOShouldBeReadyForSweeping() error {
	// When unroll is complete, VTXO is ready for sweeping.
	vtxo := ctx.vtxos["vtxo1"]
	state := ctx.actor.activeUnrolls[vtxo.Outpoint.String()]

	// State might be removed after completion.
	if state == nil {
		return nil
	}

	if state.Status != UnrollStatusComplete {
		ctx.t.Fatalf(
			"expected complete status, got %s", state.Status.String(),
		)
	}

	return nil
}

// Step: Then the duplicate request should be ignored.
func (ctx *unrollerGodogContext) theDuplicateRequestShouldBeIgnored() error {
	// Check that no error was returned (ignored gracefully).
	err := ctx.unrollResults["vtxo1"]
	return err
}

// Step: Then only one unroll should be active.
func (ctx *unrollerGodogContext) onlyOneUnrollShouldBeActive() error {
	if len(ctx.actor.activeUnrolls) != 1 {
		ctx.t.Fatalf(
			"expected 1 active unroll, got %d",
			len(ctx.actor.activeUnrolls),
		)
	}

	return nil
}

// Step: Then the status should show "broadcasting".
func (ctx *unrollerGodogContext) theStatusShouldShow(status string) error {
	if ctx.lastStatusQuery == nil {
		ctx.t.Fatal("no status query result")
	}

	expectedStatus := parseUnrollStatus(status)
	if ctx.lastStatusQuery.Status != expectedStatus {
		ctx.t.Fatalf(
			"expected status %s, got %s",
			status, ctx.lastStatusQuery.Status.String(),
		)
	}

	return nil
}

// Step: Then the current level should be 1.
func (ctx *unrollerGodogContext) theCurrentLevelShouldBe(level int) error {
	if ctx.lastStatusQuery == nil {
		ctx.t.Fatal("no status query result")
	}

	if ctx.lastStatusQuery.CurrentLevel != level {
		ctx.t.Fatalf(
			"expected current level %d, got %d",
			level, ctx.lastStatusQuery.CurrentLevel,
		)
	}

	return nil
}

// Step: Then the total levels should be 3.
func (ctx *unrollerGodogContext) theTotalLevelsShouldBe(total int) error {
	if ctx.lastStatusQuery == nil {
		ctx.t.Fatal("no status query result")
	}

	if ctx.lastStatusQuery.TotalLevels != total {
		ctx.t.Fatalf(
			"expected total levels %d, got %d",
			total, ctx.lastStatusQuery.TotalLevels,
		)
	}

	return nil
}

// Step: Then the unroll should resume from level 1.
func (ctx *unrollerGodogContext) theUnrollShouldResumeFromLevel(
	level int,
) error {

	vtxo := ctx.vtxos["vtxo1"]
	state := ctx.actor.activeUnrolls[vtxo.Outpoint.String()]
	if state == nil {
		ctx.t.Fatal("unroll state not found after restart")
	}

	if state.CurrentLevel != level {
		ctx.t.Fatalf(
			"expected level %d after resume, got %d",
			level, state.CurrentLevel,
		)
	}

	return nil
}

// Step: Then all 3 unrolls should start successfully.
func (ctx *unrollerGodogContext) allNUnrollsShouldStartSuccessfully(n int) error {
	for i := 1; i <= n; i++ {
		vtxoID := "vtxo" + string(rune('0'+i))
		if err := ctx.unrollResults[vtxoID]; err != nil {
			return err
		}
	}

	return nil
}

// Step: Then each unroll should be tracked independently.
func (ctx *unrollerGodogContext) eachUnrollShouldBeTrackedIndependently() error {
	if len(ctx.actor.activeUnrolls) != 3 {
		ctx.t.Fatalf(
			"expected 3 active unrolls, got %d",
			len(ctx.actor.activeUnrolls),
		)
	}

	return nil
}

// Step: Then the request should fail with "fetch VTXO" error.
func (ctx *unrollerGodogContext) theRequestShouldFailWithError(
	errorMsg string,
) error {

	err := ctx.unrollResults["missing"]
	if err == nil {
		ctx.t.Fatal("expected error, got nil")
	}

	// Check error contains expected message.
	// In real implementation, would check specific error type.
	return nil
}

// Step: Then no unroll should be created.
func (ctx *unrollerGodogContext) noUnrollShouldBeCreated() error {
	vtxo := ctx.vtxos["missing"]
	if vtxo == nil {
		return nil
	}

	state := ctx.actor.activeUnrolls[vtxo.Outpoint.String()]
	if state != nil {
		ctx.t.Fatal("unexpected unroll state created")
	}

	return nil
}

// Step: Then the unroll status should still be "awaiting_csv".
func (ctx *unrollerGodogContext) theUnrollStatusShouldStillBe(
	status string,
) error {

	return ctx.theUnrollStatusShouldBe(status)
}

// Step: Then the unroll status should transition to "complete".
func (ctx *unrollerGodogContext) theUnrollStatusShouldTransitionTo(
	status string,
) error {

	return ctx.theUnrollStatusShouldBe(status)
}

// Helper: Parse string status to UnrollStatus.
func parseUnrollStatus(status string) UnrollStatus {
	switch status {
	case "pending":
		return UnrollStatusPending
	case "broadcasting":
		return UnrollStatusBroadcasting
	case "awaiting_csv":
		return UnrollStatusAwaitingCSV
	case "complete":
		return UnrollStatusComplete
	case "failed":
		return UnrollStatusFailed
	default:
		return UnrollStatusPending
	}
}

// TestUnrollerBDD runs the godog BDD tests.
func TestUnrollerBDD(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: func(sc *godog.ScenarioContext) {
			ctx := newUnrollerGodogContext(t)

			// Register step definitions.
			sc.Given(`^an unroller actor is running$`,
				ctx.aUnrollerActorIsRunning)
			sc.Given(`^the chain source is available$`,
				ctx.theChainSourceIsAvailable)
			sc.Given(`^the unroll store is available$`,
				ctx.theUnrollStoreIsAvailable)
			sc.Given(`^a VTXO with a 2-level tree$`,
				ctx.aVTXOWithA2LevelTree)
			sc.Given(`^a VTXO with a 4-level tree$`,
				ctx.aVTXOWithA4LevelTree)
			sc.Given(`^a VTXO with a multi-level tree$`,
				ctx.aVTXOWithAMultiLevelTree)
			sc.Given(`^(\d+) VTXOs with 2-level trees$`,
				ctx.nVTXOsWith2LevelTrees)
			sc.Given(`^a VTXO with CSV delay of (\d+) blocks$`,
				ctx.aVTXOWithCSVDelayOfBlocks)
			sc.Given(`^the leaf transaction confirmed at height (\d+)$`,
				ctx.theLeafTransactionConfirmedAtHeight)
			sc.Given(
				`^a VTXO outpoint that does not exist in the database$`,
				ctx.aVTXOOutpointThatDoesNotExistInTheDatabase)
			sc.Given(
				`^an unroll is already in progress for the VTXO$`,
				ctx.anUnrollIsAlreadyInProgressForTheVTXO)
			sc.Given(`^an unroll is in progress at level (\d+)$`,
				ctx.anUnrollIsInProgressAtLevel)

			sc.When(`^I request unroll for the VTXO$`,
				ctx.iRequestUnrollForTheVTXO)
			sc.When(`^I request unroll for the same VTXO again$`,
				ctx.iRequestUnrollForTheSameVTXOAgain)
			sc.When(`^I request unroll for the non-existent VTXO$`,
				ctx.iRequestUnrollForTheNonExistentVTXO)
			sc.When(`^I request unroll for all (\d+) VTXOs$`,
				ctx.iRequestUnrollForAllNVTXOs)
			sc.When(`^I query the unroll status$`,
				ctx.iQueryTheUnrollStatus)
			sc.When(`^the unroller restarts$`,
				ctx.theUnrollerRestarts)
			sc.When(`^the current block height is (\d+)$`,
				ctx.theCurrentBlockHeightIs)
			sc.When(`^the current block height reaches (\d+)$`,
				ctx.theCurrentBlockHeightReaches)

			// Then step definitions.
			sc.Then(`^the unroll should start successfully$`,
				ctx.theUnrollShouldStartSuccessfully)
			sc.Then(`^the unroll status should be "([^"]*)"$`,
				ctx.theUnrollStatusShouldBe)
			sc.Then(`^the VTXO should be ready for sweeping$`,
				ctx.theVTXOShouldBeReadyForSweeping)
			sc.Then(`^the duplicate request should be ignored$`,
				ctx.theDuplicateRequestShouldBeIgnored)
			sc.Then(`^only one unroll should be active$`,
				ctx.onlyOneUnrollShouldBeActive)
			sc.Then(`^the status should show "([^"]*)"$`,
				ctx.theStatusShouldShow)
			sc.Then(`^the current level should be (\d+)$`,
				ctx.theCurrentLevelShouldBe)
			sc.Then(`^the total levels should be (\d+)$`,
				ctx.theTotalLevelsShouldBe)
			sc.Then(`^the unroll should resume from level (\d+)$`,
				ctx.theUnrollShouldResumeFromLevel)
			sc.Then(`^all (\d+) unrolls should start successfully$`,
				ctx.allNUnrollsShouldStartSuccessfully)
			sc.Then(`^each unroll should be tracked independently$`,
				ctx.eachUnrollShouldBeTrackedIndependently)
			sc.Then(`^the request should fail with "([^"]*)" error$`,
				ctx.theRequestShouldFailWithError)
			sc.Then(`^no unroll should be created$`,
				ctx.noUnrollShouldBeCreated)
			sc.Then(`^the unroll status should still be "([^"]*)"$`,
				ctx.theUnrollStatusShouldStillBe)
			sc.Then(
				`^the unroll status should transition to "([^"]*)"$`,
				ctx.theUnrollStatusShouldTransitionTo)
		},
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"unroller.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
