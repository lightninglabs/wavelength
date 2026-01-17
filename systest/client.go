//go:build systest

package systest

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/db"
	clientharness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo-client/lib/types"
	clientlnd "github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/lndbackend"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/stretchr/testify/require"
)

// TestClient represents a client participating in rounds for e2e tests. It
// uses REAL actors wired up with real LND backends and SQLite persistence.
// Only the ServerConn is a bridge routing messages to the in-process server.
type TestClient struct {
	// harness is a reference to the parent E2E harness.
	harness *E2EHarness

	// clientID is the unique identifier for this client.
	clientID clientconn.ClientID

	// lndInstance is this client's dedicated LND instance. Each client
	// has its own LND for proper wallet isolation.
	lndInstance *clientharness.LndInstance

	// lndServices provides easy access to the client's LND services.
	lndServices *lndclient.LndServices

	// serverConn is the bridge connection to the server for C→S messages.
	serverConn *BridgeServerConn

	// serverConnRef is the actor reference for the server connection bridge.
	serverConnRef actor.ActorRef[serverconn.ServerConnMsg, serverconn.ServerConnResp]

	// chainBackend is the LND-backed chain backend.
	chainBackend *chainbackends.LNDBackend

	// chainSourceRef is the reference to the chain source actor.
	chainSourceRef actor.ActorRef[chainsource.ChainSourceMsg, chainsource.ChainSourceResp]

	// walletActor is the boarding wallet actor (Ark).
	walletActor *wallet.Ark

	// walletRef is the actor reference for the wallet.
	walletRef actor.ActorRef[wallet.WalletMsg, wallet.WalletResp]

	// roundActor is the client-side round actor.
	roundActor *round.RoundClientActor

	// roundRef is the actor reference for the round client actor.
	roundRef actor.ActorRef[round.ClientMsg, round.ClientResp]

	// sqlDB is the per-client SQLite database.
	sqlDB *db.SqliteStore

	// dbPath is the path to the SQLite database file. We track this
	// separately because SqliteStore doesn't expose its path and we need
	// it for restart testing.
	dbPath string

	// roundStore provides round persistence (implements both RoundStore and
	// VTXOStore interfaces).
	roundStore *db.RoundPersistenceStore

	// boardingStore provides boarding address persistence.
	boardingStore *db.BoardingWalletStore

	// clientKeyDesc is this client's identity key from LND.
	clientKeyDesc *keychain.KeyDescriptor

	// vtxoObserver receives VTXOCreatedNotification events from the round
	// actor, enabling event-based detection of round completion.
	vtxoObserver *VTXOObserver

	// eventSub is a long-lived subscriber to bridge events for this client.
	// It receives all server->client events, allowing event-driven waiting
	// for round progress (e.g., RoundJoined, BoardingFailed, etc.).
	eventSub *subscribe.Client

	// mu protects the mutable state fields below.
	mu sync.Mutex

	// pendingRound is the current round the client is participating in.
	pendingRound *pendingRoundState
}

// pendingRoundState tracks state during an active round.
type pendingRoundState struct {
	// roundID is the ID of the round.
	roundID round.RoundID

	// completed indicates the round has finished.
	completed bool

	// failed indicates the round failed.
	failed bool

	// failReason captures the failure reason if failed.
	failReason string
}

// testClientOpts contains options for creating a test client.
type testClientOpts struct {
	lndInstance *clientharness.LndInstance
	dbPath      string
	actorSuffix string // Suffix for actor IDs to differentiate restart actors.
}

// NewTestClient creates a new test client connected to the E2E harness.
// This creates REAL actors with REAL LND backends and SQLite persistence.
// Only the ServerConn is a bridge routing messages to the in-process server.
func NewTestClient(h *E2EHarness) *TestClient {
	h.mu.Lock()
	h.clientCounter++
	clientNum := h.clientCounter
	h.mu.Unlock()

	// Start dedicated LND instance for this client.
	clientLNDName := fmt.Sprintf("client-%d", clientNum)
	lndInstance := h.StartClientLND(clientLNDName)

	// Create database path in temp directory.
	dbPath := fmt.Sprintf("%s/client-%d.db", h.t.TempDir(), clientNum)

	return newTestClientInternal(h, testClientOpts{
		lndInstance: lndInstance,
		dbPath:      dbPath,
		actorSuffix: fmt.Sprintf("-%d", clientNum),
	})
}

// NewTestClientWithExistingDB creates a new test client reusing an existing
// LND instance and database. This simulates a client restart where the process
// terminates and restarts with persisted state. The new client will:
// 1. Load persisted state from the existing database
// 2. Re-register for chain confirmations via a new ChainSourceActor
// 3. Resume any in-progress round operations
//
// This function is used by RestartClient() to implement restart testing.
func NewTestClientWithExistingDB(
	h *E2EHarness, lndInstance *clientharness.LndInstance,
	existingDBPath string,
) *TestClient {

	h.mu.Lock()
	h.clientCounter++
	clientNum := h.clientCounter
	h.mu.Unlock()

	return newTestClientInternal(h, testClientOpts{
		lndInstance: lndInstance,
		dbPath:      existingDBPath,
		actorSuffix: fmt.Sprintf("-restart-%d", clientNum),
	})
}

// newTestClientInternal creates a test client with the given options. This is
// the internal implementation shared by NewTestClient and
// NewTestClientWithExistingDB.
func newTestClientInternal(h *E2EHarness, opts testClientOpts) *TestClient {
	ctx := h.ctx
	t := h.t
	lndServices := opts.lndInstance.Client

	// Open or create the database.
	sqlDB := db.NewTestDBHandleFromPath(t, opts.dbPath)

	// Derive client identity key from LND.
	clientKeyDesc, err := lndServices.WalletKit.DeriveNextKey(
		ctx, int32(keychain.KeyFamilyNodeKey),
	)
	require.NoError(t, err, "failed to derive client identity key")

	clientID := ClientIDFromPubKey(clientKeyDesc.PubKey.SerializeCompressed())

	// Create ChainBackend using client's LND.
	chainBackend := chainbackends.NewLNDBackendFromLndClient(
		chainbackends.LNDBackendFromLndClientConfig{
			LND: lndServices,
		}.WithLogger(h.SubLogger(chainbackends.LndClientSubsystem)),
	)

	// Create and spawn ChainSourceActor.
	chainSourceActor := chainsource.NewChainSourceActor(
		chainsource.ChainSourceConfig{
			Backend: chainBackend,
			System:  h.actorSystem,
		}.WithLogger(h.SubLogger(chainsource.Subsystem)),
	)
	chainSourceActorID := fmt.Sprintf("chain-source%s", opts.actorSuffix)
	chainSourceRef := chainsource.ChainSourceKey.Spawn(
		h.actorSystem, chainSourceActorID, chainSourceActor,
	)

	// Create BoardingStore.
	boardingDB := db.NewTransactionExecutor(
		sqlDB,
		func(tx *sql.Tx) db.BoardingStore {
			return sqlDB.WithTx(tx)
		},
		h.SubLogger("BWDB"),
	)
	boardingStore := db.NewBoardingWalletStore(
		boardingDB, &chaincfg.RegressionNetParams,
		clock.NewDefaultClock(),
	)

	// Create BoardingBackend using client's LND.
	boardingBackend := clientlnd.NewBoardingBackend(lndServices.WalletKit)

	// Create and spawn WalletActor (Ark).
	walletActor := wallet.NewArk(
		boardingBackend,
		boardingStore,
		chainSourceRef,
		h.SubLogger(wallet.Subsystem),
	)
	walletActorID := fmt.Sprintf("wallet%s", opts.actorSuffix)
	walletKey := actor.NewServiceKey[wallet.WalletMsg, wallet.WalletResp](
		walletActorID,
	)
	walletRef := walletKey.Spawn(h.actorSystem, walletActorID, walletActor)

	// Start wallet actor (registers for block epoch notifications).
	err = walletActor.Start(ctx, walletRef)
	require.NoError(t, err, "failed to start wallet actor")

	// Create RoundStore.
	roundDB := db.NewTransactionExecutor(
		sqlDB,
		func(tx *sql.Tx) db.RoundStore {
			return sqlDB.WithTx(tx)
		},
		h.SubLogger("RNDB"),
	)
	roundStore := db.NewRoundPersistenceStore(
		roundDB, &chaincfg.RegressionNetParams, clock.NewDefaultClock(),
	)

	// Create per-client C→S bridge.
	serverConn := NewBridgeServerConn(clientID, h.roundsActor, h.transcript)
	serverConnActorID := fmt.Sprintf("bridge-server%s", opts.actorSuffix)
	serverConnKey := actor.NewServiceKey[serverconn.ServerConnMsg, serverconn.ServerConnResp](
		serverConnActorID,
	)
	serverConnRef := serverConnKey.Spawn(
		h.actorSystem, serverConnActorID, serverConn,
	)

	// Create ClientWallet (LndWalletController implements input.Signer).
	clientWallet := lndbackend.NewLndWalletController(
		lndServices.WalletKit, lndServices.Signer,
	)

	// Build operator terms for client.
	operatorTerms := &types.OperatorTerms{
		PubKey:            h.operatorKeyDesc.PubKey,
		BoardingExitDelay: h.terms.BoardingExitDelay,
		VTXOExitDelay:     h.terms.VTXOExitDelay,
		SweepDelay:        h.terms.SweepDelay,
		SweepKey:          h.terms.SweepKey.PubKey,
		MinConfirmations:  uint32(h.terms.MinBoardingConfirmations),
	}

	// Create and spawn VTXOObserver.
	vtxoObserver := NewVTXOObserver()
	vtxoObserverActorID := fmt.Sprintf("vtxo-observer%s", opts.actorSuffix)
	vtxoObserverKey := actor.NewServiceKey[VTXOObserverMsg, VTXOObserverResp](
		vtxoObserverActorID,
	)
	vtxoObserverRef := vtxoObserverKey.Spawn(
		h.actorSystem, vtxoObserverActorID, vtxoObserver,
	)

	vtxoManagerRef := actor.NewMapInputRef(
		vtxoObserverRef,
		func(m actor.Message) VTXOObserverMsg {
			return m.(VTXOObserverMsg)
		},
	)

	// Create RoundClientActor config.
	//
	// MaxOperatorFee is set to a generous 100,000 sats to avoid test
	// brittleness when calculating VTXO amounts. This is the difference
	// between total input (boarding) amounts and total output (VTXO)
	// amounts that the client is willing to accept.
	const maxOperatorFee = btcutil.Amount(100_000)

	roundCfg := &round.RoundClientConfig{
		Name:           string(clientID),
		Logger:         h.SubLogger(round.Subsystem),
		Wallet:         clientWallet,
		RoundStore:     roundStore,
		VTXOStore:      roundStore,
		OperatorTerms:  operatorTerms,
		ServerConn:     serverConnRef,
		ChainSource:    chainSourceRef,
		WalletActor:    walletRef,
		ChainParams:    &chaincfg.RegressionNetParams,
		VTXOManager:    vtxoManagerRef,
		MaxOperatorFee: maxOperatorFee,
	}

	// Create and spawn RoundClientActor.
	roundActorResult := round.NewRoundClientActor(roundCfg)
	roundActorInner := roundActorResult.UnwrapOrFail(t)

	roundActorID := fmt.Sprintf("round-client%s", opts.actorSuffix)
	roundKey := actor.NewServiceKey[round.ClientMsg, round.ClientResp](
		roundActorID,
	)
	roundRef := roundKey.Spawn(h.actorSystem, roundActorID, roundActorInner)

	// Set SelfRef after spawning.
	roundCfg.SelfRef = roundRef

	// Start round actor (registers with wallet for boarding confirmations).
	err = roundActorInner.Start(ctx)
	require.NoError(t, err, "failed to start round actor")

	// Register with S→C bridge.
	h.bridge.RegisterClient(clientID, roundRef)

	// Subscribe to bridge events for this client.
	eventSub, err := h.bridge.Subscribe(clientID)
	require.NoError(t, err, "failed to subscribe to bridge events")

	client := &TestClient{
		harness:        h,
		clientID:       clientID,
		lndInstance:    opts.lndInstance,
		lndServices:    lndServices,
		serverConn:     serverConn,
		serverConnRef:  serverConnRef,
		chainBackend:   chainBackend,
		chainSourceRef: chainSourceRef,
		walletActor:    walletActor,
		walletRef:      walletRef,
		roundActor:     roundActorInner,
		roundRef:       roundRef,
		sqlDB:          sqlDB,
		dbPath:         opts.dbPath,
		roundStore:     roundStore,
		boardingStore:  boardingStore,
		clientKeyDesc:  clientKeyDesc,
		vtxoObserver:   vtxoObserver,
		eventSub:       eventSub,
	}

	// Register the client with the harness.
	h.RegisterClient(client)

	return client
}

// ClientID returns the client's unique identifier.
func (c *TestClient) ClientID() clientconn.ClientID {
	return c.clientID
}

// LND returns the client's dedicated LND services.
func (c *TestClient) LND() *lndclient.LndServices {
	return c.lndServices
}

// CreateBoardingAddress creates a new boarding address using the wallet actor.
// Returns the address details for funding.
func (c *TestClient) CreateBoardingAddress(
	exitDelay uint32) (*wallet.CreateBoardingAddressResponse, error) {

	ctx := c.harness.ctx

	req := &wallet.CreateBoardingAddressRequest{
		OperatorKey: c.harness.operatorKeyDesc.PubKey,
		ExitDelay:   exitDelay,
	}

	future := c.walletRef.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		return nil, result.Err()
	}

	resp, _ := result.Unpack()

	return resp.(*wallet.CreateBoardingAddressResponse), nil
}

// JoinRound initiates joining a round with the specified boarding inputs.
// This is a simplified helper that creates a join request with the given
// boarding inputs and sends it to the server via the bridge.
func (c *TestClient) JoinRound(ctx context.Context,
	boardingInputs []*types.BoardingRequest) error {

	// Build the join request using the client's round package types.
	req := &round.JoinRoundRequest{
		BoardingRequests: make([]types.BoardingRequest, 0, len(boardingInputs)),
	}

	for _, input := range boardingInputs {
		req.BoardingRequests = append(req.BoardingRequests, *input)
	}

	// Send via the bridge using serverconn message types.
	sendReq := &serverconn.SendClientEventRequest{
		Message: req,
	}
	result := c.serverConn.Receive(ctx, sendReq)

	if result.IsErr() {
		return result.Err()
	}

	// Track pending round.
	c.mu.Lock()
	c.pendingRound = &pendingRoundState{}
	c.mu.Unlock()

	return nil
}

// WaitForRoundComplete waits for the current round to complete or fail.
// Round completion is detected via the VTXOObserver which receives
// VTXOCreatedNotification events when the FSM reaches ConfirmedState.
//
// This method uses the pre-existing bridge event subscription to detect the
// RoundJoined event (which contains the round ID), then waits for VTXO
// creation for that specific round.
func (c *TestClient) WaitForRoundComplete(timeout time.Duration) error {
	// Wait for RoundJoined event from the pre-existing subscription.
	// The subscription was created when the TestClient was created, so
	// we won't miss any events.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var targetRoundID round.RoundID
	for {
		select {
		case update := <-c.eventSub.Updates():
			event, ok := update.(round.ClientEvent)
			if !ok {
				continue
			}

			if rj, ok := event.(*round.RoundJoined); ok {
				targetRoundID = rj.RoundID
				goto gotRoundID
			}

		case <-c.eventSub.Quit():
			return fmt.Errorf("event subscription closed")

		case <-timer.C:
			return fmt.Errorf("timeout waiting for RoundJoined event")
		}
	}

gotRoundID:
	return c.WaitForRound(targetRoundID, timeout)
}

// WaitForRound waits for a specific round to complete or fail. This is useful
// for subsequent rounds where VTXOs from previous rounds already exist.
func (c *TestClient) WaitForRound(targetRoundID round.RoundID,
	timeout time.Duration) error {

	ctx := c.harness.ctx

	// Check if round already failed before waiting.
	c.mu.Lock()
	if c.pendingRound != nil && c.pendingRound.failed {
		reason := c.pendingRound.failReason
		c.mu.Unlock()

		return fmt.Errorf("round failed: %s", reason)
	}
	c.mu.Unlock()

	// hasVTXOForRound checks if a VTXO for the target round exists.
	hasVTXOForRound := func() bool {
		vtxos, err := c.roundStore.ListVTXOs(ctx)
		if err != nil {
			return false
		}

		for _, vtxo := range vtxos {
			if !vtxo.RoundID.IsSome() {
				continue
			}

			if vtxo.RoundID.UnsafeFromSome() == targetRoundID {
				return true
			}
		}

		return false
	}

	// Check if VTXO for this round already exists (in case we missed the
	// notification).
	if hasVTXOForRound() {
		return nil
	}

	// Wait for VTXOCreatedNotification from the observer or timeout.
	deadline := time.After(timeout)
	for {
		select {
		case notification := <-c.vtxoObserver.NotifyChan():
			if notification == nil || len(notification.VTXOs) == 0 {
				continue
			}

			// Check if any VTXO in the notification is for our
			// target round.
			for _, vtxo := range notification.VTXOs {
				if !vtxo.RoundID.IsSome() {
					continue
				}

				if vtxo.RoundID.UnsafeFromSome() == targetRoundID {
					return nil
				}
			}

			// Notification was for a different round, keep waiting.

		case <-deadline:
			// Check DB one more time before returning timeout.
			if hasVTXOForRound() {
				return nil
			}

			return fmt.Errorf(
				"timeout waiting for round %s completion",
				targetRoundID,
			)

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ListVTXOs queries the VTXO store for all VTXOs. This uses the real database.
func (c *TestClient) ListVTXOs(ctx context.Context) ([]*round.ClientVTXO, error) {
	return c.roundStore.ListVTXOs(ctx)
}

// AssertConfirmedRoundCountFromDB asserts the expected number of confirmed
// rounds are persisted in the client's real SQLite database.
// This polls for up to 5 seconds to handle async round finalization.
func (c *TestClient) AssertConfirmedRoundCountFromDB(expected int) {
	ctx := c.harness.ctx
	t := c.harness.t

	err := wait.Predicate(func() bool {
		rounds, err := c.roundStore.ListConfirmedRounds(ctx)
		if err != nil {
			return false
		}

		return len(rounds) == expected
	}, 5*time.Second)

	require.NoError(t, err,
		"client %s confirmed round count mismatch: expected %d",
		c.clientID, expected)
}

// AssertVTXOCountFromDB asserts the expected number of VTXOs are persisted
// in the client's real SQLite database.
func (c *TestClient) AssertVTXOCountFromDB(expected int) {
	ctx := c.harness.ctx
	vtxos, err := c.roundStore.ListVTXOs(ctx)
	require.NoError(c.harness.t, err, "failed to list VTXOs from DB")
	require.Equal(c.harness.t, expected, len(vtxos),
		"client %s VTXO count mismatch in database", c.clientID)
}

// AssertVTXOProperties verifies all VTXOs in the database have valid
// properties based on the boarding input and operator terms.
func (c *TestClient) AssertVTXOProperties() {
	ctx := c.harness.ctx
	t := c.harness.t

	vtxos, err := c.roundStore.ListVTXOs(ctx)
	require.NoError(t, err, "failed to list VTXOs from DB")

	terms := c.harness.Terms()

	for i, vtxo := range vtxos {
		// Amount must be non-zero.
		require.NotZero(t, vtxo.Amount,
			"VTXO %d should have non-zero amount", i)

		// Expiry should match the operator terms.
		require.Equal(t, terms.BoardingExitDelay, vtxo.Expiry,
			"VTXO %d expiry should match terms", i)

		// ClientKey must be present. Note: The VTXO's client key comes from
		// the boarding address creation (BoardingKeyFamily), not from the
		// client's identity key (KeyFamilyNodeKey), so we just verify it
		// exists and is valid.
		require.NotNil(t, vtxo.ClientKey.PubKey,
			"VTXO %d should have client key", i)

		// OperatorKey must match the server's operator key.
		require.NotNil(t, vtxo.OperatorKey,
			"VTXO %d should have operator key", i)
		require.True(t,
			vtxo.OperatorKey.IsEqual(c.harness.operatorKeyDesc.PubKey),
			"VTXO %d operator key should match server operator", i)

		// TreePath is required for unilateral exit.
		require.NotNil(t, vtxo.TreePath,
			"VTXO %d should have tree path for unilateral exit", i)

		// RoundID must be set.
		require.True(t, vtxo.RoundID.IsSome(),
			"VTXO %d should have round ID", i)
	}
}

// AssertVTXOMatchesBoardingInput verifies that a VTXO's properties match the
// original boarding input. This is a detailed verification that the round
// correctly processed the boarding input.
//
// Parameters:
// - vtxoIdx: index of the VTXO to check (0-based).
// - expectedAmount: the amount funded to the boarding address.
// - tolerance: maximum difference allowed between expected and actual amount
// (to account for fees taken during VTXO tree construction).
func (c *TestClient) AssertVTXOMatchesBoardingInput(
	vtxoIdx int, expectedAmount btcutil.Amount, tolerance btcutil.Amount) {

	ctx := c.harness.ctx
	t := c.harness.t

	vtxos, err := c.roundStore.ListVTXOs(ctx)
	require.NoError(t, err, "failed to list VTXOs from DB")
	require.Greater(t, len(vtxos), vtxoIdx,
		"VTXO index %d out of range (have %d VTXOs)", vtxoIdx, len(vtxos))

	vtxo := vtxos[vtxoIdx]

	// Amount should be close to the boarding amount (minus fees).
	diff := expectedAmount - vtxo.Amount
	if diff < 0 {
		diff = -diff
	}
	require.LessOrEqual(t, diff, tolerance,
		"VTXO amount %d differs from expected %d by %d (tolerance: %d)",
		vtxo.Amount, expectedAmount, diff, tolerance)

	// Verify the client key exists. Note: The client key comes from
	// BoardingKeyFamily, not the identity key, so we just check presence.
	require.NotNil(t, vtxo.ClientKey.PubKey,
		"VTXO should have a client key")
}

// GetLastCompletedRoundID returns the most recent confirmed round ID from the
// client's database. This is useful for verifying subsequent rounds. The
// method polls for up to 5 seconds to handle async round finalization.
func (c *TestClient) GetLastCompletedRoundID() (round.RoundID, error) {
	ctx := c.harness.ctx

	var result round.RoundID
	err := wait.Predicate(func() bool {
		rounds, err := c.roundStore.ListConfirmedRounds(ctx)
		if err != nil || len(rounds) == 0 {
			return false
		}

		// ListConfirmedRounds orders by creation_time DESC, so index 0
		// is the most recent round.
		result = rounds[0].RoundID
		return true
	}, 5*time.Second)

	if err != nil {
		return round.RoundID{}, fmt.Errorf("no confirmed rounds found "+
			"in database after polling: %w", err)
	}

	return result, nil
}

// TotalVTXOValue returns the sum of all unspent VTXO amounts in the client's
// database. This is useful for verifying total balance after multiple rounds.
func (c *TestClient) TotalVTXOValue() (btcutil.Amount, error) {
	ctx := c.harness.ctx
	vtxos, err := c.roundStore.ListVTXOs(ctx)
	if err != nil {
		return 0, err
	}

	var total btcutil.Amount
	for _, v := range vtxos {
		total += v.Amount
	}

	return total, nil
}

// AssertRoundConfirmed verifies that a round has been confirmed on-chain with
// valid confirmation info (block height).
func (c *TestClient) AssertRoundConfirmed(roundID round.RoundID) {
	ctx := c.harness.ctx
	t := c.harness.t

	r, _, err := c.roundStore.FetchState(ctx, roundID)
	require.NoError(t, err, "failed to fetch round %s", roundID)
	require.NotNil(t, r, "round %s should exist", roundID)

	require.True(t, r.ConfInfo.IsSome(),
		"round %s should have confirmation info", roundID)

	confInfo := r.ConfInfo.UnwrapOr(round.ConfInfo{})
	require.Greater(t, confInfo.Height, int32(0),
		"round %s should have positive confirmation height", roundID)
}

// AssertVTXOsForRound verifies that the expected number of VTXOs were created
// for a specific round and validates their properties.
func (c *TestClient) AssertVTXOsForRound(
	roundID round.RoundID, expectedCount int) {

	ctx := c.harness.ctx
	t := c.harness.t

	vtxos, err := c.roundStore.ListVTXOs(ctx)
	require.NoError(t, err, "failed to list VTXOs")

	var roundVTXOs []*round.ClientVTXO
	for _, v := range vtxos {
		if v.RoundID.IsSome() {
			id := v.RoundID.UnwrapOr(round.RoundID{})
			if id == roundID {
				roundVTXOs = append(roundVTXOs, v)
			}
		}
	}

	require.Len(t, roundVTXOs, expectedCount,
		"expected %d VTXOs for round %s, got %d",
		expectedCount, roundID, len(roundVTXOs))
}

// CreateBoardingRequest creates a boarding request for the given UTXO details.
// This is a helper to build properly formatted boarding requests for testing.
func CreateBoardingRequest(outpoint *wire.OutPoint, amount btcutil.Amount,
	exitDelay uint32) *types.BoardingRequest {

	return &types.BoardingRequest{
		Outpoint:  outpoint,
		ExitDelay: exitDelay,
		// Note: ClientKey, OperatorKey, and TxProof would be populated in
		// a full implementation. For tests, these may be nil or mocked.
	}
}

// WaitForFSMState waits for the client's primary FSM to reach the specified
// state. This is used to wait for boarding confirmation (PendingRoundAssembly).
func (c *TestClient) WaitForFSMState(targetState string, timeout time.Duration) error {
	ctx := c.harness.ctx
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		// Query the FSM state.
		stateReq := &round.GetClientStateRequest{}
		future := c.roundRef.Ask(ctx, stateReq)
		result := future.Await(ctx)
		if result.IsErr() {
			return fmt.Errorf("failed to get FSM state: %w", result.Err())
		}

		resp, _ := result.Unpack()
		stateResp, ok := resp.(*round.GetClientStateResponse)
		if !ok {
			return fmt.Errorf("unexpected response type: %T", resp)
		}

		// Check all FSM states. The State field is a ClientState interface,
		// so we check its type name. FSM keys are either temp keys
		// (e.g., "temp:uuid") or round IDs.
		for _, fsmState := range stateResp.States {
			stateName := fmt.Sprintf("%T", fsmState.State)
			// Remove package prefix if present (e.g.,
			// "*round.PendingRoundAssembly").
			if idx := len("*round."); len(stateName) > idx &&
				stateName[:idx] == "*round." {

				stateName = stateName[idx:]
			}
			if stateName == targetState {
				return nil
			}
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for FSM state %s", targetState)
}

// WaitForBoardingConfirmation waits for the wallet to detect the boarding
// UTXO confirmation, which transitions the FSM to PendingRoundAssembly state.
func (c *TestClient) WaitForBoardingConfirmation(timeout time.Duration) error {
	return c.WaitForFSMState("PendingRoundAssembly", timeout)
}

// RegisterVTXORequests sends VTXO request amounts to the round actor. This must
// be called before TriggerRegistration to specify the VTXOs the client wants to
// receive. The amounts should not exceed the total boarding input value minus
// operator fees.
func (c *TestClient) RegisterVTXORequests(ctx context.Context,
	amounts []btcutil.Amount) error {

	msg := &round.RegisterVTXORequestsRequest{
		Amounts: amounts,
	}

	future := c.roundRef.Ask(ctx, msg)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("VTXO registration failed: %w", result.Err())
	}

	respVal, _ := result.Unpack()
	resp, ok := respVal.(*round.RegisterVTXORequestsResponse)
	if !ok {
		return fmt.Errorf("unexpected response type: %T", respVal)
	}
	if !resp.Success {
		return fmt.Errorf("VTXO registration failed: %s", resp.Error)
	}

	return nil
}

// TriggerRegistration sends a RegistrationRequested event to the round actor,
// triggering the FSM to transition from PendingRoundAssembly to
// RegistrationSent state and emit a JoinRoundRequest.
func (c *TestClient) TriggerRegistration(ctx context.Context) error {
	// The round actor processes RegistrationRequested via ServerMessageNotification.
	// This is how client-initiated events are routed to the FSM.
	msg := &round.ServerMessageNotification{
		Message: &round.RegistrationRequested{},
	}

	future := c.roundRef.Ask(ctx, msg)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("failed to trigger registration: %w", result.Err())
	}

	return nil
}

// Stop gracefully shuts down the client's actors and subscriptions. This is
// used for restart testing to simulate client process termination. The database
// is left intact so a new client can recover state on restart.
func (c *TestClient) Stop() {
	// Cancel event subscription to stop receiving server events.
	if c.eventSub != nil {
		c.eventSub.Cancel()
	}

	// Unregister from bridge to stop message routing to this client.
	c.harness.bridge.UnregisterClient(c.clientID)

	// Note: Actors are tied to the actor system and will be cleaned up
	// when the harness stops. For restart testing, we leave the database
	// intact so the new client can recover state.
}

// DBPath returns the path to the client's SQLite database file. This is used
// when creating a new client instance that should reuse the existing database
// for restart testing.
func (c *TestClient) DBPath() string {
	return c.dbPath
}

// LNDInstance returns the client's dedicated LND instance. This is used when
// creating a new client instance that should reuse the existing LND for restart
// testing, ensuring the same wallet and keys are available.
func (c *TestClient) LNDInstance() *clientharness.LndInstance {
	return c.lndInstance
}
