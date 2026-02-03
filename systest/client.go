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
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/db"
	clientharness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/lib/types"
	clientlnd "github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/lndbackend"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
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

	// roundRef is the actor reference for the round client actor. Uses
	// actormsg types so the wallet can find it via service key lookup.
	roundRef actor.ActorRef[actormsg.RoundReceivable, actormsg.RoundActorResp]

	// sqlDB is the per-client SQLite database.
	sqlDB *db.SqliteStore

	// dbPath is the path to the SQLite database file. We track this
	// separately because SqliteStore doesn't expose its path and we need
	// it for restart testing.
	dbPath string

	// roundStore provides round persistence (implements both RoundStore and
	// VTXOStore interfaces).
	roundStore *db.RoundPersistenceStore

	// vtxoStore provides VTXO lifecycle persistence with status tracking.
	vtxoStore *db.VTXOPersistenceStore

	// boardingStore provides boarding address persistence.
	boardingStore *db.BoardingWalletStore

	// clientKeyDesc is this client's identity key from LND.
	clientKeyDesc *keychain.KeyDescriptor

	// vtxoManager is the real VTXO manager that spawns VTXO actors.
	vtxoManager *vtxo.Manager

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

	// Create and spawn WalletActor (Ark). The wallet uses service key lookup
	// to find the round actor when forwarding refresh requests.
	walletActor := wallet.NewArk(
		boardingBackend,
		boardingStore,
		chainSourceRef,
		h.actorSystem,
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

	// Create VTXOStore for VTXO lifecycle management.
	vtxoStore := db.NewVTXOPersistenceStore(roundDB, clock.NewDefaultClock())

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
		ForfeitScript:     h.ForfeitScript(),
	}

	// Create RoundClientActor config with VTXOManager=nil initially.
	// We'll set it after creating the vtxo.Manager (circular dependency).
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
		VTXOManager:    nil, // Set after vtxo.Manager is created.
		MaxOperatorFee: maxOperatorFee,
		ActorSystem:    h.actorSystem,
	}

	// Create and spawn RoundClientActor. The round actor uses actormsg types
	// (RoundReceivable/RoundActorResp) so the wallet can find it via service
	// key lookup without import cycles.
	roundActorResult := round.NewRoundClientActor(roundCfg)
	roundActorInner := roundActorResult.UnwrapOrFail(t)

	roundKey := actormsg.RoundActorServiceKey()
	roundRef := roundKey.Spawn(
		h.actorSystem, actormsg.RoundActorServiceKeyName, roundActorInner,
	)

	// Set SelfRef after spawning. Since roundRef is
	// ActorRef[RoundReceivable, RoundActorResp] and SelfRef expects
	// TellOnlyRef[RoundReceivable], we can use it directly (ActorRef
	// embeds TellOnlyRef).
	roundCfg.SelfRef = roundRef

	// Create real vtxo.Manager with round actor reference. This enables
	// VTXO actors to send RefreshRequest and ForfeitSignature back to the
	// round actor.
	vtxoManagerCfg := &vtxo.ManagerConfig{
		Store:         vtxoStore,
		Wallet:        clientWallet,
		ChainSource:   chainSourceRef,
		ActorSystem:   h.actorSystem,
		ChainParams:   &chaincfg.RegressionNetParams,
		ExpiryConfig:  nil, // Use defaults.
		Logger:        h.SubLogger("VTXO"),
		RoundActor:    roundRef,
		ChainResolver: nil, // No unilateral exit in e2e tests.
	}
	vtxoManagerActor := vtxo.NewManager(vtxoManagerCfg)

	// Spawn the vtxo.Manager as an actor.
	vtxoManagerActorID := fmt.Sprintf("vtxo-manager%s", opts.actorSuffix)
	vtxoManagerKey := actor.NewServiceKey[vtxo.ManagerMsg, vtxo.ManagerResp](
		vtxoManagerActorID,
	)
	vtxoManagerActorRef := vtxoManagerKey.Spawn(
		h.actorSystem, vtxoManagerActorID, vtxoManagerActor,
	)

	// Create a TellOnlyRef for the manager (used for termination notifications).
	vtxoManagerTellRef := actor.NewMapInputRef[vtxo.ManagerMsg, vtxo.ManagerMsg](
		vtxoManagerActorRef,
		func(m vtxo.ManagerMsg) vtxo.ManagerMsg { return m },
	)

	// Start vtxo.Manager (recovers persisted VTXOs).
	err = vtxoManagerActor.Start(ctx, vtxoManagerTellRef)
	require.NoError(t, err, "failed to start vtxo manager")

	// Create VTXOObserver for test notifications.
	vtxoObserver := NewVTXOObserver()
	vtxoObserverActorID := fmt.Sprintf("vtxo-observer%s", opts.actorSuffix)
	vtxoObserverKey := actor.NewServiceKey[VTXOObserverMsg, VTXOObserverResp](
		vtxoObserverActorID,
	)
	vtxoObserverRef := vtxoObserverKey.Spawn(
		h.actorSystem, vtxoObserverActorID, vtxoObserver,
	)

	// Create fan-out ref that forwards to both vtxo.Manager and VTXOObserver.
	// This allows the round actor to send VTXOCreatedNotification to both.
	vtxoManagerMappedRef := actor.NewMapInputRef[actor.Message, vtxo.ManagerMsg](
		vtxoManagerActorRef,
		func(m actor.Message) vtxo.ManagerMsg {
			return m.(vtxo.ManagerMsg)
		},
	)
	vtxoObserverMappedRef := actor.NewMapInputRef[actor.Message, VTXOObserverMsg](
		vtxoObserverRef,
		func(m actor.Message) VTXOObserverMsg {
			return m.(VTXOObserverMsg)
		},
	)
	vtxoFanout := NewVTXOManagerFanout(vtxoManagerMappedRef, vtxoObserverMappedRef)

	// Set VTXOManager on round config now that we have the fan-out.
	roundCfg.VTXOManager = vtxoFanout

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
		vtxoStore:      vtxoStore,
		boardingStore:  boardingStore,
		clientKeyDesc:  clientKeyDesc,
		vtxoManager:    vtxoManagerActor,
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
	// The round actor processes RegistrationRequested via
	// ServerMessageNotification. This is how client-initiated events are
	// routed to the FSM.
	msg := &round.ServerMessageNotification{
		Message: &round.RegistrationRequested{},
	}

	future := c.roundRef.Ask(ctx, msg)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("failed to trigger registration: %w",
			result.Err())
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

// TriggerVTXORefresh sends a RefreshVTXOsRequest to the wallet actor to trigger
// refresh of the specified VTXOs. The wallet forwards this to the round actor,
// which sends TriggerRefreshEvent to each VTXO actor. The VTXO actors emit
// ForfeitRequest to be included in the next round's forfeit flow.
func (c *TestClient) TriggerVTXORefresh(ctx context.Context,
	outpoints []wire.OutPoint) error {

	req := &wallet.RefreshVTXOsRequest{
		TargetOutpoints: outpoints,
		ForceRefresh:    true,
	}

	future := c.walletRef.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("refresh request failed: %w", result.Err())
	}

	resp, _ := result.Unpack()
	refreshResp, ok := resp.(*wallet.RefreshVTXOsResponse)
	if !ok {
		return fmt.Errorf("unexpected response type: %T", resp)
	}

	if len(refreshResp.Errors) > 0 {
		// Collect all error messages.
		var errMsgs []string
		for op, err := range refreshResp.Errors {
			errMsgs = append(errMsgs, fmt.Sprintf("%s: %v", op, err))
		}
		return fmt.Errorf("refresh errors: %v", errMsgs)
	}

	return nil
}

// WaitForVTXOStatus waits for a VTXO to reach the expected status within the
// specified timeout. This polls the database to detect status changes.
func (c *TestClient) WaitForVTXOStatus(outpoint wire.OutPoint,
	expectedStatus vtxo.VTXOStatus, timeout time.Duration) error {

	ctx := c.harness.ctx

	err := wait.Predicate(func() bool {
		desc, err := c.vtxoStore.GetVTXO(ctx, outpoint)
		if err != nil {
			return false
		}

		return desc.Status == expectedStatus
	}, timeout)

	if err != nil {
		// Get current status for better error message.
		desc, getErr := c.vtxoStore.GetVTXO(ctx, outpoint)
		if getErr != nil {
			return fmt.Errorf(
				"timeout waiting for VTXO %s status %s "+
					"(getting current status failed: %v): %w",
				outpoint, expectedStatus, getErr, err,
			)
		}
		return fmt.Errorf(
			"timeout waiting for VTXO %s status %s (current: %s)",
			outpoint, expectedStatus, desc.Status,
		)
	}

	return nil
}

// AssertVTXOStatus asserts that a VTXO has the expected status in the database.
func (c *TestClient) AssertVTXOStatus(outpoint wire.OutPoint,
	expectedStatus vtxo.VTXOStatus) {

	ctx := c.harness.ctx
	t := c.harness.t

	desc, err := c.vtxoStore.GetVTXO(ctx, outpoint)
	require.NoError(t, err, "failed to get VTXO %s", outpoint)
	require.Equal(t, expectedStatus, desc.Status,
		"VTXO %s status mismatch", outpoint)
}

// AssertVTXOReplacement verifies that the old VTXO was replaced by the new
// VTXO. This checks that the old VTXO is in Forfeited status and that both
// VTXOs exist in the database with the expected relationship.
func (c *TestClient) AssertVTXOReplacement(oldOutpoint,
	newOutpoint wire.OutPoint) {

	ctx := c.harness.ctx
	t := c.harness.t

	// Verify old VTXO is forfeited.
	oldDesc, err := c.vtxoStore.GetVTXO(ctx, oldOutpoint)
	require.NoError(t, err, "failed to get old VTXO %s", oldOutpoint)
	require.Equal(t, vtxo.VTXOStatusForfeited, oldDesc.Status,
		"old VTXO should be forfeited")

	// Verify new VTXO exists and is live.
	newDesc, err := c.vtxoStore.GetVTXO(ctx, newOutpoint)
	require.NoError(t, err, "failed to get new VTXO %s", newOutpoint)
	require.Equal(t, vtxo.VTXOStatusLive, newDesc.Status,
		"new VTXO should be live")

	// Verify amounts are similar (new should be slightly less due to fees).
	require.InDelta(t, float64(oldDesc.Amount), float64(newDesc.Amount),
		float64(100_000), // Allow up to 100k sats difference for fees
		"new VTXO amount should be close to old")
}

// GetVTXOByRoundID returns the VTXO created in a specific round. This is useful
// for finding the new VTXO after a refresh operation.
func (c *TestClient) GetVTXOByRoundID(roundID string) (*vtxo.Descriptor, error) {
	ctx := c.harness.ctx

	// List all live VTXOs and find the one from the target round.
	vtxos, err := c.vtxoStore.ListLiveVTXOs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list VTXOs: %w", err)
	}

	for _, desc := range vtxos {
		if desc.RoundID == roundID {
			return desc, nil
		}
	}

	return nil, fmt.Errorf("no VTXO found for round %s", roundID)
}

// GetVTXODescriptor returns the full VTXO descriptor for an outpoint. This
// provides access to all VTXO fields including status and lifecycle data.
func (c *TestClient) GetVTXODescriptor(outpoint wire.OutPoint) (
	*vtxo.Descriptor, error) {

	return c.vtxoStore.GetVTXO(c.harness.ctx, outpoint)
}

// ListLiveVTXODescriptors returns all VTXOs that are not in a terminal state.
// This is useful for finding VTXOs that can be refreshed or spent.
func (c *TestClient) ListLiveVTXODescriptors() ([]*vtxo.Descriptor, error) {
	return c.vtxoStore.ListLiveVTXOs(c.harness.ctx)
}

// TriggerVTXOLeave sends a LeaveVTXOsRequest to the wallet actor to trigger
// leave (offboard) for the specified VTXOs. The destination is an on-chain
// address where the funds will be sent. The flow is:
// wallet -> round actor -> VTXO actor -> round actor (LeaveVTXORequest).
func (c *TestClient) TriggerVTXOLeave(ctx context.Context,
	outpoints []wire.OutPoint, destAddr btcutil.Address) error {

	// Compute total amount from all VTXOs being left.
	var totalAmount btcutil.Amount
	for _, outpoint := range outpoints {
		desc, err := c.vtxoStore.GetVTXO(ctx, outpoint)
		if err != nil {
			return fmt.Errorf("get VTXO %s: %w", outpoint, err)
		}

		totalAmount += desc.Amount
	}

	// Create output script from destination address.
	pkScript, err := txscript.PayToAddrScript(destAddr)
	if err != nil {
		return fmt.Errorf("create pkScript: %w", err)
	}

	// Create the leave output with total VTXO amount. The actual fee
	// will be deducted by the server.
	leaveOutput := &wire.TxOut{
		Value:    int64(totalAmount),
		PkScript: pkScript,
	}

	// Send LeaveVTXOsRequest to wallet actor.
	req := &wallet.LeaveVTXOsRequest{
		TargetOutpoints: outpoints,
		DestOutput:      leaveOutput,
	}

	future := c.walletRef.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("leave request failed: %w", result.Err())
	}

	return nil
}

// GetOnChainBalance returns the client's confirmed on-chain wallet balance.
// This queries the LND wallet for the total confirmed balance.
func (c *TestClient) GetOnChainBalance(ctx context.Context) (
	btcutil.Amount, error) {

	balance, err := c.lndServices.Client.WalletBalance(ctx)
	if err != nil {
		return 0, fmt.Errorf("get wallet balance: %w", err)
	}

	return balance.Confirmed, nil
}

// WaitForOnChainBalance waits for the client's on-chain balance to be at least
// the expected amount. This is useful after leave operations.
func (c *TestClient) WaitForOnChainBalance(ctx context.Context,
	expectedMin btcutil.Amount, timeout time.Duration) error {

	return wait.NoError(func() error {
		balance, err := c.GetOnChainBalance(ctx)
		if err != nil {
			return err
		}

		if balance < expectedMin {
			return fmt.Errorf(
				"balance %d sats < expected %d sats",
				balance, expectedMin,
			)
		}

		return nil
	}, timeout)
}

// GetNewAddress generates a new on-chain address from the client's LND wallet.
// This is used as the destination for leave operations.
func (c *TestClient) GetNewAddress(ctx context.Context) (
	btcutil.Address, error) {

	// Use the WalletKit to get a taproot address.
	addr, err := c.lndServices.WalletKit.NextAddr(
		ctx, "", walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	if err != nil {
		return nil, fmt.Errorf("get new address: %w", err)
	}

	return addr, nil
}
