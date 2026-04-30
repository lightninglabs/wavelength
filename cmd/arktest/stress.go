//go:build itest

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	clientharness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	darepoharness "github.com/lightninglabs/darepo/harness"
	"github.com/spf13/cobra"
)

const (
	// defaultStressClients is the default number of stress clients.
	defaultStressClients = 5

	// defaultStressMaxPayments is the default random OOR payment budget.
	defaultStressMaxPayments = 50

	// defaultStressMaxRounds is the default random refresh round budget.
	defaultStressMaxRounds = 5

	// defaultStressMaxRestarts is the default restart/crash disruption
	// budget.
	defaultStressMaxRestarts = 5

	// defaultStressConcurrency is the default number of concurrent stress
	// operations.
	defaultStressConcurrency = 4

	// defaultStressDuration is the default maximum stress runtime.
	defaultStressDuration = 10 * time.Minute

	// defaultStressMinPayment is the default smallest OOR payment amount.
	defaultStressMinPayment = int64(1_000)

	// defaultStressMaxPayment is the default largest OOR payment amount.
	defaultStressMaxPayment = int64(50_000)

	// defaultStressBoardAmount is boarded into each client at bootstrap.
	defaultStressBoardAmount = int64(250_000)

	// stressSummaryName is the final machine-readable summary artifact.
	stressSummaryName = "summary.json"

	// stressSummaryTopLine is the visible terminal summary banner opener.
	stressSummaryTopLine = "========== ARKTEST STRESS SUMMARY =========="

	// stressSummaryBottomLine is the terminal summary banner closer.
	stressSummaryBottomLine = "============================================"

	// stressRoundMineDepth is the number of blocks mined after a round
	// transaction is known to be broadcast.
	stressRoundMineDepth = 6
)

var (
	// stressRoundWaitTimeout bounds waits for client/operator round state
	// transitions during bootstrap and refresh events.
	stressRoundWaitTimeout = 2 * time.Minute

	// stressRoundPollInterval is the polling cadence for stress round
	// readiness checks.
	stressRoundPollInterval = 500 * time.Millisecond

	// stressRegistrationSettleDelay lets outbound registration messages
	// cross the durable mailbox before the operator is asked to seal.
	stressRegistrationSettleDelay = time.Second
)

// stressConfig holds the flags that shape a random arktest stress run.
type stressConfig struct {
	artifactsDir     string
	groupName        string
	clientWallet     string
	lndImage         string
	operatorFunds    int64
	clientLNDFunds   int64
	clientCount      int
	maxPayments      int
	maxRounds        int
	maxRestarts      int
	concurrency      int
	duration         time.Duration
	seed             int64
	minPayment       int64
	maxPayment       int64
	boardAmount      int64
	logStdout        bool
	operatorRestarts bool
	clientRestarts   bool
	clientCrashes    bool
}

// stressSummary is written to summary.json when a stress run completes.
type stressSummary struct {
	Seed              int64   `json:"seed"`
	StartedAt         string  `json:"started_at"`
	CompletedAt       string  `json:"completed_at"`
	DurationMS        int64   `json:"duration_ms"`
	ArtifactsDir      string  `json:"artifacts_dir"`
	Clients           int     `json:"clients"`
	PaymentsAttempted int     `json:"payments_attempted"`
	PaymentsSettled   int     `json:"payments_settled"`
	PaymentsFailed    int     `json:"payments_failed"`
	PaymentSuccessPct float64 `json:"payment_success_pct"`
	PaymentAvgMS      int64   `json:"payment_avg_ms"`
	PaymentP50MS      int64   `json:"payment_p50_ms"`
	PaymentP95MS      int64   `json:"payment_p95_ms"`
	PaymentMaxMS      int64   `json:"payment_max_ms"`
	PaymentThroughput float64 `json:"payment_throughput_per_sec"`
	RoundsTriggered   int     `json:"rounds_triggered"`
	RoundsConfirmed   int     `json:"rounds_confirmed"`
	RoundsFailed      int     `json:"rounds_failed"`
	ClientRestarts    int     `json:"client_restarts"`
	ClientCrashes     int     `json:"client_crashes"`
	OperatorRestarts  int     `json:"operator_restarts"`
	Concurrency       int     `json:"concurrency"`
}

// stressRunner owns the live harness references and counters for one stress
// command invocation.
type stressRunner struct {
	t                *testing.T
	cfg              stressConfig
	h                *darepoharness.ArkHarness
	state            *harnessState
	events           *eventLog
	mu               sync.Mutex
	rng              *rand.Rand
	clients          map[string]*darepoharness.ClientDaemonHarness
	clientLocks      map[string]*sync.Mutex
	roundMu          sync.Mutex
	operatorMu       sync.Mutex
	names            []string
	started          time.Time
	summary          stressSummary
	paymentLatencies []time.Duration
}

var stressCfg stressConfig

// newStressCmd creates the random workload runner.
func newStressCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stress",
		Short: "Run a sparse-log random arktest workload",
		Long: "Starts one local Ark topology, boards a client set, " +
			"then concurrently performs random OOR payments, " +
			"round refreshes, graceful restarts, and client " +
			"crash/recover cycles until one of the configured " +
			"budgets is exhausted.",
		Run: runStress,
	}

	f := cmd.Flags()
	f.StringVar(
		&stressCfg.artifactsDir, "artifacts-dir", defaultArtifactsDir,
		"directory for harness artifacts (logs, summaries, etc.)",
	)
	f.StringVar(
		&stressCfg.groupName, "group-name", defaultGroupName,
		"Docker container/network naming group",
	)
	f.StringVar(
		&stressCfg.clientWallet, "client-wallet", defaultClientWallet,
		"client daemon wallet backend: lnd, lwwallet, or btcwallet",
	)
	f.StringVar(
		&stressCfg.lndImage, "lnd-image", "",
		"override the default LND docker image",
	)
	f.Int64Var(
		&stressCfg.operatorFunds, "operator-funds",
		int64(defaultOperatorFunds),
		"satoshis sent to the operator LND wallet for round txs",
	)
	f.Int64Var(
		&stressCfg.clientLNDFunds, "client-lnd-funds",
		int64(defaultClientLNDFunds),
		"satoshis sent to each client's LND wallet for CPFP fee bumps",
	)
	f.IntVar(
		&stressCfg.clientCount, "clients", defaultStressClients,
		"number of clients to create",
	)
	f.IntVar(
		&stressCfg.maxPayments, "max-payments",
		defaultStressMaxPayments,
		"maximum random OOR payment attempts",
	)
	f.IntVar(
		&stressCfg.maxRounds, "max-rounds", defaultStressMaxRounds,
		"maximum random refresh rounds after bootstrap",
	)
	f.IntVar(
		&stressCfg.maxRestarts, "max-restarts",
		defaultStressMaxRestarts,
		"maximum restart/crash disruption events",
	)
	f.IntVar(
		&stressCfg.concurrency, "concurrency",
		defaultStressConcurrency,
		"maximum concurrent random workload operations",
	)
	f.DurationVar(
		&stressCfg.duration, "duration", defaultStressDuration,
		"maximum wall-clock runtime",
	)
	f.Int64Var(
		&stressCfg.seed, "seed", 0,
		"workload seed; zero chooses the current time",
	)
	f.Int64Var(
		&stressCfg.minPayment, "min-payment",
		defaultStressMinPayment,
		"minimum random OOR payment amount in sats",
	)
	f.Int64Var(
		&stressCfg.maxPayment, "max-payment",
		defaultStressMaxPayment,
		"maximum random OOR payment amount in sats",
	)
	f.Int64Var(
		&stressCfg.boardAmount, "board-amount",
		defaultStressBoardAmount,
		"satoshis boarded into each client before stress starts",
	)
	f.BoolVar(
		&stressCfg.logStdout, "logstdout", false,
		"also print harness/operator logs to stdout",
	)
	f.BoolVar(
		&stressCfg.operatorRestarts, "operator-restarts", true,
		"allow graceful operator restarts",
	)
	f.BoolVar(
		&stressCfg.clientRestarts, "client-restarts", true,
		"allow graceful client restarts",
	)
	f.BoolVar(
		&stressCfg.clientCrashes, "client-crashes", true,
		"allow client crash/recover events",
	)

	return cmd
}

// runStress hides Cobra flags from testing.Main and runs the stress harness.
func runStress(_ *cobra.Command, _ []string) {
	os.Args = []string{os.Args[0]}

	testing.Main(
		regexp.MatchString,
		[]testing.InternalTest{{
			Name: "ArktestStress",
			F:    runStressHarness,
		}},
		nil, nil,
	)
}

// runStressHarness builds a single topology and runs the random workload.
func runStressHarness(t *testing.T) {
	cfg := normalizeStressConfig(t, stressCfg)
	runner := newStressRunner(t, cfg)

	runner.start()
	defer runner.stop()

	runner.bootstrapBoarding()
	runner.runWorkload()
	runner.writeSummary()
}

// normalizeStressConfig validates stress flags and fills derived defaults.
func normalizeStressConfig(t *testing.T, cfg stressConfig) stressConfig {
	t.Helper()

	if cfg.clientCount < 2 {
		t.Fatalf("--clients must be at least 2")
	}
	if cfg.maxPayments < 0 || cfg.maxRounds < 0 || cfg.maxRestarts < 0 {
		t.Fatalf("stress budgets must be non-negative")
	}
	if cfg.concurrency <= 0 {
		t.Fatalf("--concurrency must be positive")
	}
	if cfg.duration <= 0 {
		t.Fatalf("--duration must be positive")
	}
	if cfg.boardAmount <= 0 {
		t.Fatalf("--board-amount must be positive")
	}
	if cfg.minPayment <= 0 || cfg.maxPayment < cfg.minPayment {
		t.Fatalf("invalid payment range")
	}
	if cfg.seed == 0 {
		cfg.seed = time.Now().UnixNano()
	}

	return cfg
}

// newStressRunner constructs a runner with a deterministic workload RNG.
func newStressRunner(t *testing.T, cfg stressConfig) *stressRunner {
	names := stressClientNames(cfg.clientCount)
	clientLocks := make(map[string]*sync.Mutex, len(names))
	clients := make(map[string]*darepoharness.ClientDaemonHarness)
	for _, name := range names {
		clientLocks[name] = &sync.Mutex{}
	}

	return &stressRunner{
		t:           t,
		cfg:         cfg,
		rng:         rand.New(rand.NewSource(cfg.seed)),
		clients:     clients,
		clientLocks: clientLocks,
		names:       names,
	}
}

// start boots the topology and persists a state file usable by other commands.
func (r *stressRunner) start() {
	r.t.Helper()

	requireMkdir(r.t, dataDir)
	artifactsAbs, err := filepath.Abs(r.cfg.artifactsDir)
	if err != nil {
		r.t.Fatalf("resolve artifacts dir: %v", err)
	}
	requireMkdir(r.t, artifactsAbs)

	defaults := clientharness.DefaultOptions()
	clientOpts := &defaults
	clientOpts.ArtifactsBaseDir = artifactsAbs
	clientOpts.GroupName = r.cfg.groupName
	clientOpts.HarnessLogStdOut = r.cfg.logStdout
	clientOpts.ArkdLogStdOut = r.cfg.logStdout
	if r.cfg.lndImage != "" {
		clientOpts.LNDImage = r.cfg.lndImage
	}

	hopts := &darepoharness.ArkHarnessOptions{
		ClientOptions:          clientOpts,
		ClientDaemonWalletType: r.cfg.clientWallet,
	}
	applyDaemonLogStdout(hopts, r.cfg.logStdout)

	r.h = darepoharness.NewArkHarness(r.t, hopts)
	r.h.Start()

	r.state = buildBaseState(r.h, artifactsAbs)
	r.state.Clients = make(map[string]*arkClientState)
	r.state.ClientLNDs = make(map[string]*lndState)

	events, err := newEventLog(
		os.Stdout, filepath.Join(r.state.RunDir, defaultEventLogName),
	)
	if err != nil {
		r.t.Fatalf("open event log: %v", err)
	}
	r.events = events
	r.started = time.Now()

	r.events.Print("stress_start", "arktest stress starting",
		map[string]any{
			"seed":        r.cfg.seed,
			"clients":     r.cfg.clientCount,
			"concurrency": r.cfg.concurrency,
			"wallet":      r.cfg.clientWallet,
			"group":       r.cfg.groupName,
			"artifacts":   artifactsAbs,
		})

	if r.cfg.operatorFunds > 0 {
		r.events.Printf("fund", map[string]any{
			"amount_sat": r.cfg.operatorFunds,
		}, "funding operator lnd amount=%d", r.cfg.operatorFunds)
		r.h.Harness.FundOperatorLND(
			btcutil.Amount(r.cfg.operatorFunds),
		)
		r.events.Printf("fund", map[string]any{
			"amount_sat": r.cfg.operatorFunds,
		}, "operator lnd funded amount=%d", r.cfg.operatorFunds)
	}

	for _, name := range r.names {
		r.startClient(name)
	}

	if err := r.saveCurrentState(); err != nil {
		r.t.Fatalf("save state: %v", err)
	}

	r.events.Printf("ready", map[string]any{
		"run_dir": r.state.RunDir,
		"clients": r.names,
	}, "arktest stress ready clients=%d artifacts=%s seed=%d",
		len(r.names), r.state.RunDir, r.cfg.seed)
}

// stop tears down the live topology and closes the sparse event artifact.
func (r *stressRunner) stop() {
	if r.h != nil {
		r.h.Stop()
	}
	if r.events != nil {
		_ = r.events.Close()
	}
}

// startClient starts one client daemon and records its state.
func (r *stressRunner) startClient(name string) {
	r.t.Helper()

	r.events.Printf("client_start", map[string]any{
		"client": name,
		"wallet": r.cfg.clientWallet,
	}, "starting client %s wallet=%s", name, r.cfg.clientWallet)
	client := r.h.StartClientDaemon(name)
	r.setClient(name, client)
	r.events.Printf("client_ready", map[string]any{
		"client": name,
		"rpc":    client.RPCAddr,
	}, "client %s ready rpc=%s", name, client.RPCAddr)

	if r.cfg.clientLNDFunds > 0 {
		r.events.Printf("fund", map[string]any{
			"client":     name,
			"amount_sat": r.cfg.clientLNDFunds,
		}, "funding client %s lnd wallet amount=%d",
			name, r.cfg.clientLNDFunds)
		r.h.FundClientWallet(
			client, btcutil.Amount(r.cfg.clientLNDFunds),
		)
		r.events.Printf("fund", map[string]any{
			"client":     name,
			"amount_sat": r.cfg.clientLNDFunds,
		}, "client %s lnd wallet funded amount=%d",
			name, r.cfg.clientLNDFunds)
	}
}

// setClient records the active harness handle and persisted state for a client.
func (r *stressRunner) setClient(name string,
	client *darepoharness.ClientDaemonHarness) {

	r.mu.Lock()
	defer r.mu.Unlock()

	r.clients[name] = client
	r.recordClientStateLocked(name, client)
}

// getClient returns the current harness handle for a client.
func (r *stressRunner) getClient(
	name string) *darepoharness.ClientDaemonHarness {

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.clients[name]
}

// recordClientStateLocked records a live client daemon in the persisted state.
// The caller must hold r.mu.
func (r *stressRunner) recordClientStateLocked(name string,
	client *darepoharness.ClientDaemonHarness) {

	r.state.Clients[name] = &arkClientState{
		Name:    name,
		RPCAddr: client.RPCAddr,
		DataDir: client.DataDir,
		Wallet:  r.cfg.clientWallet,
	}

	lndBackend := darepoharness.ClientWalletBackendLND
	if r.cfg.clientWallet != lndBackend {
		return
	}

	lnd := r.h.Harness.GetAdditionalLND(name)
	if lnd == nil {
		return
	}

	r.state.ClientLNDs[name] = &lndState{
		Name:          lnd.Name,
		GRPCAddr:      "127.0.0.1:" + lnd.GRPCPort,
		TLSCertPath:   lnd.TLSCert,
		MacaroonPath:  lnd.Macaroon,
		DataDir:       lnd.DataDir,
		ContainerName: lnd.ContainerName,
	}
}

// saveCurrentState writes the persisted arktest state while holding the runner
// mutex that protects state mutations.
func (r *stressRunner) saveCurrentState() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return saveState(r.state)
}

// lockClients locks lifecycle mutations for the named clients in stable order
// and returns an unlock function. Normal client RPC workload operations do not
// take these locks; they intentionally overlap to exercise daemon concurrency.
func (r *stressRunner) lockClients(names ...string) func() {
	unique := make(map[string]struct{}, len(names))
	ordered := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := unique[name]; ok {
			continue
		}
		unique[name] = struct{}{}
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)

	for _, name := range ordered {
		r.clientLocks[name].Lock()
	}

	return func() {
		for i := len(ordered) - 1; i >= 0; i-- {
			r.clientLocks[ordered[i]].Unlock()
		}
	}
}

// lockAllClients locks lifecycle mutations for every stress client in stable
// order and returns an unlock function.
func (r *stressRunner) lockAllClients() func() {
	return r.lockClients(r.names...)
}

// bootstrapBoarding funds and boards every stress client into one round.
func (r *stressRunner) bootstrapBoarding() {
	ctx, cancel := r.contextWithTimeout(5 * time.Minute)
	defer cancel()

	r.events.Printf("bootstrap", nil,
		"boarding %d clients amount=%d", len(r.names),
		r.cfg.boardAmount)

	for _, name := range r.names {
		client := r.getClient(name)
		r.events.Printf("bootstrap", map[string]any{
			"client": name,
		}, "client %s requesting boarding address", name)
		addr, err := client.RPCClient.NewAddress(
			ctx, &daemonrpc.NewAddressRequest{},
		)
		if err != nil {
			r.t.Fatalf("%s NewAddress: %v", name, err)
		}
		r.events.Printf("bootstrap", map[string]any{
			"client":     name,
			"address":    addr.Address,
			"amount_sat": r.cfg.boardAmount,
		}, "funding client %s boarding address amount=%d",
			name, r.cfg.boardAmount)
		r.h.Harness.Faucet(
			addr.Address, btcutil.Amount(r.cfg.boardAmount),
		)
		r.events.Printf("bootstrap", map[string]any{
			"client":     name,
			"address":    addr.Address,
			"amount_sat": r.cfg.boardAmount,
		}, "client %s boarding address funded", name)
	}
	r.events.Printf("bootstrap", map[string]any{
		"blocks": 6,
	}, "mining boarding confirmations blocks=%d", 6)
	r.h.Harness.Generate(6)
	r.events.Printf("bootstrap", map[string]any{
		"blocks": 6,
	}, "boarding confirmations mined blocks=%d", 6)

	for _, name := range r.names {
		r.events.Printf("bootstrap", map[string]any{
			"client": name,
		}, "client %s submitting board request", name)
		_, err := r.getClient(name).RPCClient.Board(
			ctx, &daemonrpc.BoardRequest{},
		)
		if err != nil {
			r.t.Fatalf("%s Board: %v", name, err)
		}

		if err := r.waitClientRoundAtLeast(
			name, daemonrpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY,
			stressRoundWaitTimeout,
		); err != nil {
			r.t.Fatalf("wait for %s bootstrap intent: %v",
				name, err)
		}
		state := daemonrpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY.
			String()
		r.events.Printf("bootstrap", map[string]any{
			"client": name,
			"state":  state,
		}, "client %s board intent ready", name)

		r.getClient(name).TriggerRoundRegistration()
		r.events.Printf("bootstrap", map[string]any{
			"client": name,
		}, "client %s triggered round registration", name)
	}

	r.events.Print("bootstrap",
		"waiting for all clients to send round registration", nil)
	if err := r.waitAllClientsRoundAtLeast(
		daemonrpc.RoundState_ROUND_STATE_REGISTRATION_SENT,
		stressRoundWaitTimeout,
	); err != nil {
		r.t.Fatalf("wait for bootstrap intents: %v", err)
	}
	r.events.Print("bootstrap", "all clients sent round registration", nil)
	time.Sleep(stressRegistrationSettleDelay)

	r.events.Print("bootstrap", "triggering bootstrap batch", nil)
	resp, err := r.h.ArkAdminClient.TriggerBatch(
		ctx, &adminrpc.TriggerBatchRequest{},
	)
	if err != nil {
		r.t.Fatalf("bootstrap trigger batch: %v", err)
	}
	r.events.Printf("bootstrap", map[string]any{
		"round_id": resp.RoundId,
	}, "bootstrap batch triggered round=%s", resp.RoundId)
	if err := r.confirmRound(resp.RoundId); err != nil {
		r.t.Fatalf("confirm bootstrap round: %v", err)
	}

	if err := r.waitAllVTXOBalances(); err != nil {
		r.t.Fatalf("%v", err)
	}
	r.events.Printf("round", map[string]any{
		"round_id": resp.RoundId,
	}, "bootstrap round confirmed round=%s", resp.RoundId)
}

// runWorkload runs random events until all configured budgets are exhausted or
// the duration limit is reached.
func (r *stressRunner) runWorkload() {
	deadline := time.Now().Add(r.cfg.duration)
	sem := make(chan struct{}, r.cfg.concurrency)
	var wg sync.WaitGroup

	for time.Now().Before(deadline) && r.hasBudget() {
		sem <- struct{}{}
		if !time.Now().Before(deadline) {
			<-sem
			break
		}

		job, ok := r.reserveNextJob()
		if !ok {
			<-sem
			break
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			r.runJob(job)
		}()

		r.sleepBetweenJobs()
	}

	wg.Wait()
}

// hasBudget returns true while at least one random event budget remains.
func (r *stressRunner) hasBudget() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.hasBudgetLocked()
}

// hasBudgetLocked returns true while at least one random event budget remains.
// The caller must hold r.mu.
func (r *stressRunner) hasBudgetLocked() bool {
	return r.eventAllowedLocked(stressEventPayment) ||
		r.eventAllowedLocked(stressEventRound) ||
		r.eventAllowedLocked(stressEventClientRestart) ||
		r.eventAllowedLocked(stressEventClientCrash) ||
		r.eventAllowedLocked(stressEventOperatorRestart)
}

// totalRestartsLocked returns the number of disruption events consumed. The
// caller must hold r.mu.
func (r *stressRunner) totalRestartsLocked() int {
	return r.summary.ClientRestarts + r.summary.ClientCrashes +
		r.summary.OperatorRestarts
}

// stressEvent identifies one random workload event type.
type stressEvent int

const (
	// stressEventPayment sends one random OOR payment.
	stressEventPayment stressEvent = iota

	// stressEventRound queues and confirms one refresh round.
	stressEventRound

	// stressEventClientRestart gracefully restarts one client.
	stressEventClientRestart

	// stressEventClientCrash crashes and recovers one client.
	stressEventClientCrash

	// stressEventOperatorRestart gracefully restarts the operator.
	stressEventOperatorRestart
)

// stressJob is one reserved unit of asynchronous stress work.
type stressJob struct {
	event     stressEvent
	paymentID int
}

// reserveNextJob chooses a weighted event that still has budget and reserves
// the corresponding attempt counter before the worker starts.
func (r *stressRunner) reserveNextJob() (stressJob, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.hasBudgetLocked() {
		return stressJob{}, false
	}

	for {
		roll := r.rng.Intn(100)
		var evt stressEvent
		switch {
		case roll < 60:
			evt = stressEventPayment
		case roll < 75:
			evt = stressEventRound
		case roll < 86:
			evt = stressEventClientRestart
		case roll < 96:
			evt = stressEventClientCrash
		default:
			evt = stressEventOperatorRestart
		}

		if !r.eventAllowedLocked(evt) {
			continue
		}

		job := stressJob{event: evt}
		switch evt {
		case stressEventPayment:
			r.summary.PaymentsAttempted++
			job.paymentID = r.summary.PaymentsAttempted

		case stressEventRound:
			r.summary.RoundsTriggered++

		case stressEventClientRestart:
			r.summary.ClientRestarts++

		case stressEventClientCrash:
			r.summary.ClientCrashes++

		case stressEventOperatorRestart:
			r.summary.OperatorRestarts++
		}

		return job, true
	}
}

// runJob executes one reserved stress job.
func (r *stressRunner) runJob(job stressJob) {
	switch job.event {
	case stressEventPayment:
		r.randomPayment(job.paymentID)

	case stressEventRound:
		r.randomRefreshRound()

	case stressEventClientRestart:
		r.randomClientRestart()

	case stressEventClientCrash:
		r.randomClientCrash()

	case stressEventOperatorRestart:
		r.operatorRestart()
	}
}

// sleepBetweenJobs adds deterministic jitter between scheduled jobs.
func (r *stressRunner) sleepBetweenJobs() {
	delay := time.Duration(25+r.randIntn(125)) * time.Millisecond
	time.Sleep(delay)
}

// randIntn returns a pseudo-random int from the runner RNG.
func (r *stressRunner) randIntn(n int) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.rng.Intn(n)
}

// randInt63n returns a pseudo-random int64 from the runner RNG.
func (r *stressRunner) randInt63n(n int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.rng.Int63n(n)
}

// eventAllowedLocked returns true if an event type has remaining budget. The
// caller must hold r.mu.
func (r *stressRunner) eventAllowedLocked(evt stressEvent) bool {
	switch evt {
	case stressEventPayment:
		return r.summary.PaymentsAttempted < r.cfg.maxPayments

	case stressEventRound:
		return r.summary.RoundsTriggered < r.cfg.maxRounds

	case stressEventClientRestart:
		return r.cfg.clientRestarts && r.totalRestartsLocked() <
			r.cfg.maxRestarts

	case stressEventClientCrash:
		return r.cfg.clientCrashes && r.totalRestartsLocked() <
			r.cfg.maxRestarts

	case stressEventOperatorRestart:
		return r.cfg.operatorRestarts && r.totalRestartsLocked() <
			r.cfg.maxRestarts

	default:
		return false
	}
}

// randomPayment sends a random OOR amount from one funded client to another.
func (r *stressRunner) randomPayment(paymentID int) {
	sender, _, ok := r.randomFundedSender()
	if !ok {
		r.events.Print("payment_skip",
			"payment skipped: no funded sender", nil)
		r.incrementPaymentFailed()

		return
	}

	receiver := r.randomReceiver(sender)
	senderClient := r.getClient(sender)
	receiverClient := r.getClient(receiver)

	liveBalance, err := r.liveVTXOBalance(sender)
	if err != nil {
		r.paymentFailed(paymentID, "sender live vtxos", err)
		return
	}
	if liveBalance < r.cfg.minPayment {
		err := fmt.Errorf(
			"%s has %d sats, need at least %d", sender,
			liveBalance, r.cfg.minPayment,
		)
		r.paymentFailed(
			paymentID, "sender live vtxos", err,
		)

		return
	}

	maxAmount := minInt64(r.cfg.maxPayment, liveBalance)
	amount := r.randomAmount(r.cfg.minPayment, maxAmount)

	idKey := fmt.Sprintf("arktest-stress-%d-%d", r.cfg.seed, paymentID)

	r.events.Printf("payment", map[string]any{
		"id":       paymentID,
		"sender":   sender,
		"receiver": receiver,
		"amount":   amount,
	}, "payment %d %s -> %s amount=%d",
		paymentID, sender, receiver, amount)

	ctx, cancel := r.shortContext()
	defer cancel()

	recv, err := receiverClient.RPCClient.NewReceiveScript(
		ctx, &daemonrpc.NewReceiveScriptRequest{
			Label: fmt.Sprintf("stress-%d", paymentID),
		},
	)
	if err != nil {
		r.paymentFailed(paymentID, "receive script", err)
		return
	}

	pubkey, err := hex.DecodeString(recv.PubkeyXonlyHex)
	if err != nil {
		r.paymentFailed(paymentID, "decode pubkey", err)
		return
	}

	start := time.Now()
	resp, err := senderClient.RPCClient.SendOOR(
		ctx, &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: pubkey,
				},
				AmountSat: amount,
			},
			IdempotencyKey: idKey,
		},
	)
	if err != nil {
		r.paymentFailed(paymentID, "send oor", err)
		return
	}

	latency := time.Since(start)
	r.recordPaymentSettled(latency)
	r.events.Printf("payment_settled", map[string]any{
		"id":         paymentID,
		"session_id": resp.SessionId,
		"latency_ms": latency.Milliseconds(),
	}, "payment %d settled latency=%s session=%s",
		paymentID, latency.Round(time.Millisecond), resp.SessionId)
}

// paymentFailed records a failed payment event and increments the summary.
func (r *stressRunner) paymentFailed(id int, phase string, err error) {
	r.incrementPaymentFailed()
	r.events.Printf("payment_failed", map[string]any{
		"id":    id,
		"phase": phase,
		"error": err.Error(),
	}, "payment %d failed phase=%s err=%v", id, phase, err)
}

// incrementPaymentFailed increments the failed payment counter.
func (r *stressRunner) incrementPaymentFailed() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.summary.PaymentsFailed++
}

// recordPaymentSettled records a successful payment latency.
func (r *stressRunner) recordPaymentSettled(latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.summary.PaymentsSettled++
	r.paymentLatencies = append(r.paymentLatencies, latency)
}

// randomFundedSender chooses a sender with enough live VTXO balance.
func (r *stressRunner) randomFundedSender() (
	string, int64, bool) {

	names := r.shuffledClientNames()
	for _, name := range names {
		liveBalance, err := r.liveVTXOBalance(name)
		if err != nil {
			r.events.Printf("balance_failed", map[string]any{
				"client": name,
				"error":  err.Error(),
			}, "live vtxo balance failed client=%s err=%v",
				name, err)

			continue
		}

		if liveBalance >= r.cfg.minPayment {
			return name, liveBalance, true
		}
	}

	return "", 0, false
}

// shuffledClientNames returns the stress client names in deterministic random
// order.
func (r *stressRunner) shuffledClientNames() []string {
	names := append([]string(nil), r.names...)
	r.mu.Lock()
	defer r.mu.Unlock()

	r.rng.Shuffle(len(names), func(i, j int) {
		names[i], names[j] = names[j], names[i]
	})

	return names
}

// clientBalance queries a client's confirmed Ark balance.
func (r *stressRunner) clientBalance(
	name string) (*daemonrpc.GetBalanceResponse, error) {

	ctx, cancel := r.shortContext()
	defer cancel()

	client := r.getClient(name)

	return client.RPCClient.GetBalance(
		ctx, &daemonrpc.GetBalanceRequest{},
	)
}

// liveVTXOs returns the client's currently spendable VTXOs.
func (r *stressRunner) liveVTXOs(name string) ([]*daemonrpc.VTXO, error) {
	ctx, cancel := r.shortContext()
	defer cancel()

	client := r.getClient(name)
	resp, err := client.RPCClient.ListVTXOs(
		ctx, &daemonrpc.ListVTXOsRequest{
			StatusFilter: daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
		},
	)
	if err != nil {
		return nil, err
	}

	return resp.Vtxos, nil
}

// liveVTXOBalance returns the sum of the client's currently spendable VTXOs.
func (r *stressRunner) liveVTXOBalance(name string) (int64, error) {
	vtxos, err := r.liveVTXOs(name)
	if err != nil {
		return 0, err
	}

	var balance int64
	for _, vtxo := range vtxos {
		balance += vtxo.AmountSat
	}

	return balance, nil
}

// liveVTXOOutpoints returns the client's currently spendable VTXO outpoints.
func (r *stressRunner) liveVTXOOutpoints(name string) ([]string, error) {
	vtxos, err := r.liveVTXOs(name)
	if err != nil {
		return nil, err
	}

	outpoints := make([]string, 0, len(vtxos))
	for _, vtxo := range vtxos {
		outpoints = append(outpoints, vtxo.Outpoint)
	}

	return outpoints, nil
}

// randomReceiver chooses a receiver different from sender.
func (r *stressRunner) randomReceiver(sender string) string {
	for {
		receiver := r.names[r.randIntn(len(r.names))]
		if receiver != sender {
			return receiver
		}
	}
}

// randomAmount chooses a random amount in [minAmount, maxAmount].
func (r *stressRunner) randomAmount(minAmount, maxAmount int64) int64 {
	if maxAmount <= minAmount {
		return minAmount
	}

	return minAmount + r.randInt63n(maxAmount-minAmount+1)
}

// randomRefreshRound queues a refresh for a random client and confirms a round.
func (r *stressRunner) randomRefreshRound() {
	name := r.names[r.randIntn(len(r.names))]
	r.roundMu.Lock()
	defer r.roundMu.Unlock()

	r.events.Printf("round", map[string]any{
		"client": name,
	}, "refresh round requested client=%s", name)

	ctx, cancel := r.shortContext()
	defer cancel()

	client := r.getClient(name)
	outpoints, err := r.liveVTXOOutpoints(name)
	if err != nil {
		r.recordRoundFailedf("round_failed", map[string]any{
			"client": name,
			"error":  err.Error(),
		}, "list live vtxos failed client=%s err=%v", name, err)

		return
	}
	if len(outpoints) == 0 {
		r.recordRoundFailedf("round_failed", map[string]any{
			"client": name,
		}, "refresh round skipped client=%s no live vtxos", name)

		return
	}

	_, err = client.RPCClient.RefreshVTXOs(
		ctx, &daemonrpc.RefreshVTXOsRequest{
			Selection: &daemonrpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: outpoints,
				},
			},
		},
	)
	if err != nil {
		r.recordRoundFailedf("round_failed", map[string]any{
			"client": name,
			"error":  err.Error(),
		}, "refresh round failed client=%s err=%v", name, err)

		return
	}

	if err := r.waitClientRoundAtLeast(
		name, daemonrpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY,
		stressRoundWaitTimeout,
	); err != nil {
		r.recordRoundFailedf("round_failed", map[string]any{
			"client": name,
			"error":  err.Error(),
		}, "refresh pending wait failed client=%s err=%v",
			name, err)

		return
	}

	r.getClient(name).TriggerRoundRegistration()
	if err := r.waitClientRoundAtLeast(
		name, daemonrpc.RoundState_ROUND_STATE_REGISTRATION_SENT,
		stressRoundWaitTimeout,
	); err != nil {
		r.recordRoundFailedf("round_failed", map[string]any{
			"client": name,
			"error":  err.Error(),
		}, "refresh registration wait failed client=%s err=%v",
			name, err)

		return
	}
	time.Sleep(stressRegistrationSettleDelay)

	resp, err := r.h.ArkAdminClient.TriggerBatch(
		ctx, &adminrpc.TriggerBatchRequest{},
	)
	if err != nil {
		r.recordRoundFailedf("round_failed", map[string]any{
			"client": name,
			"error":  err.Error(),
		}, "trigger batch failed client=%s err=%v", name, err)

		return
	}

	if err := r.confirmRound(resp.RoundId); err != nil {
		r.recordRoundFailedf("round_failed", map[string]any{
			"client": name,
			"round":  resp.RoundId,
			"error":  err.Error(),
		}, "refresh round confirmation failed client=%s round=%s "+
			"err=%v", name, resp.RoundId, err)

		return
	}

	r.recordRoundConfirmed()
	r.events.Printf("round_confirmed", map[string]any{
		"client": name,
		"round":  resp.RoundId,
	}, "refresh round confirmed client=%s round=%s", name,
		resp.RoundId)
}

// recordRoundConfirmed increments the successful refresh-round counter.
func (r *stressRunner) recordRoundConfirmed() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.summary.RoundsConfirmed++
}

// recordRoundFailedf records one failed refresh-round attempt in the event log
// and summary counters.
func (r *stressRunner) recordRoundFailedf(kind string, fields map[string]any,
	format string, args ...any) {

	r.mu.Lock()
	r.summary.RoundsFailed++
	r.mu.Unlock()

	r.events.Printf(kind, fields, format, args...)
}

// randomClientRestart gracefully restarts one client daemon.
func (r *stressRunner) randomClientRestart() {
	name := r.names[r.randIntn(len(r.names))]
	unlock := r.lockClients(name)
	defer unlock()

	r.events.Printf("client_restart", map[string]any{
		"client": name,
	}, "client restarting client=%s", name)

	start := time.Now()
	client := r.h.RestartClientDaemon(name)
	r.setClient(name, client)
	if err := r.saveCurrentState(); err != nil {
		r.t.Fatalf("save state after client restart: %v", err)
	}

	r.events.Printf("client_ready", map[string]any{
		"client":     name,
		"latency_ms": time.Since(start).Milliseconds(),
	}, "client ready client=%s latency=%s", name,
		time.Since(start).Round(time.Millisecond))
}

// randomClientCrash crashes and recovers one client daemon.
func (r *stressRunner) randomClientCrash() {
	name := r.names[r.randIntn(len(r.names))]
	unlock := r.lockClients(name)
	defer unlock()

	r.events.Printf("client_crash", map[string]any{
		"client": name,
	}, "client crashing client=%s", name)

	start := time.Now()
	client := r.h.CrashClientDaemon(name)
	r.setClient(name, client)
	if err := r.saveCurrentState(); err != nil {
		r.t.Fatalf("save state after client crash: %v", err)
	}

	r.events.Printf("client_recovered", map[string]any{
		"client":     name,
		"latency_ms": time.Since(start).Milliseconds(),
	}, "client recovered client=%s latency=%s", name,
		time.Since(start).Round(time.Millisecond))
}

// operatorRestart gracefully restarts arkd and then restarts every client so
// they connect to the fresh operator RPC address.
func (r *stressRunner) operatorRestart() {
	r.operatorMu.Lock()
	defer r.operatorMu.Unlock()
	r.roundMu.Lock()
	defer r.roundMu.Unlock()
	unlockClients := r.lockAllClients()
	defer unlockClients()

	r.events.Print("operator_restart", "operator restarting", nil)

	start := time.Now()
	r.h.RestartArkd()
	r.mu.Lock()
	r.state.ArkAdminAddr = r.h.ArkAdminAddr
	r.state.ArkRPCAddr = r.h.ArkRPCAddr
	r.mu.Unlock()

	for _, name := range r.names {
		client := r.h.RestartClientDaemon(name)
		r.setClient(name, client)
	}

	if err := r.saveCurrentState(); err != nil {
		r.t.Fatalf("save state after operator restart: %v", err)
	}

	r.events.Printf("operator_ready", map[string]any{
		"latency_ms": time.Since(start).Milliseconds(),
		"ark_admin":  r.state.ArkAdminAddr,
		"ark_rpc":    r.state.ArkRPCAddr,
	}, "operator ready latency=%s rpc=%s",
		time.Since(start).Round(time.Millisecond), r.state.ArkRPCAddr)
}

// confirmRound waits until a triggered operator round is broadcast, mines the
// confirmation blocks, and then waits until admin state reports confirmation.
func (r *stressRunner) confirmRound(roundID string) error {
	status, err := r.waitAdminRoundStatus(
		roundID, stressRoundWaitTimeout,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
		adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
	)
	if err != nil {
		return err
	}

	if status != adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED {
		r.h.Harness.Generate(stressRoundMineDepth)
	}

	_, err = r.waitAdminRoundStatus(
		roundID, stressRoundWaitTimeout,
		adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
	)

	return err
}

// waitAdminRoundStatus waits until ListRounds reports one of the requested
// statuses for the given round ID.
func (r *stressRunner) waitAdminRoundStatus(roundID string,
	timeout time.Duration,
	statuses ...adminrpc.RoundStatus) (adminrpc.RoundStatus, error) {

	want := make(map[adminrpc.RoundStatus]struct{}, len(statuses))
	for _, status := range statuses {
		want[status] = struct{}{}
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, found, err := r.adminRoundStatus(roundID)
		if err != nil {
			return adminrpc.RoundStatus_ROUND_STATUS_UNSPECIFIED,
				err
		}

		if found {
			if status == adminrpc.RoundStatus_ROUND_STATUS_FAILED {
				return status, fmt.Errorf("round %s failed",
					roundID)
			}

			if _, ok := want[status]; ok {
				return status, nil
			}
		}

		time.Sleep(stressRoundPollInterval)
	}

	return adminrpc.RoundStatus_ROUND_STATUS_UNSPECIFIED,
		fmt.Errorf("timed out waiting for round %s statuses %v",
			roundID, statuses)
}

// adminRoundStatus returns the admin status for a known round, if it is listed
// by the operator.
func (r *stressRunner) adminRoundStatus(
	roundID string) (adminrpc.RoundStatus, bool, error) {

	ctx, cancel := r.shortContext()
	defer cancel()

	resp, err := r.h.ArkAdminClient.ListRounds(
		ctx, &adminrpc.ListRoundsRequest{
			Limit: 100,
		},
	)
	if err != nil {
		return adminrpc.RoundStatus_ROUND_STATUS_UNSPECIFIED,
			false, err
	}

	for _, round := range resp.Rounds {
		if round.Id == roundID {
			return round.Status, true, nil
		}
	}

	return adminrpc.RoundStatus_ROUND_STATUS_UNSPECIFIED, false, nil
}

// waitAllClientsRoundAtLeast waits until every stress client reports at least
// one local round at or beyond the requested state.
func (r *stressRunner) waitAllClientsRoundAtLeast(
	minState daemonrpc.RoundState, timeout time.Duration) error {

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allReady := true
		for _, name := range r.names {
			ready, err := r.clientHasRoundAtLeast(name, minState)
			if err != nil {
				return err
			}

			if !ready {
				allReady = false
				break
			}
		}

		if allReady {
			return nil
		}

		time.Sleep(stressRoundPollInterval)
	}

	return fmt.Errorf("timed out waiting for all clients in round "+
		"state at least %s", minState)
}

// waitClientRoundAtLeast waits until a single stress client reports at least
// one local round at or beyond the requested state.
func (r *stressRunner) waitClientRoundAtLeast(name string,
	minState daemonrpc.RoundState, timeout time.Duration) error {

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ready, err := r.clientHasRoundAtLeast(name, minState)
		if err != nil {
			return err
		}

		if ready {
			return nil
		}

		time.Sleep(stressRoundPollInterval)
	}

	return fmt.Errorf("timed out waiting for %s in round state %s",
		name, minState)
}

// clientHasRoundAtLeast reports whether a client has any local round at or
// beyond the requested state.
func (r *stressRunner) clientHasRoundAtLeast(name string,
	minState daemonrpc.RoundState) (bool, error) {

	ctx, cancel := r.shortContext()
	defer cancel()

	client := r.getClient(name)
	resp, err := client.RPCClient.ListRounds(
		ctx, &daemonrpc.ListRoundsRequest{},
	)
	if err != nil {
		return false, err
	}

	for _, round := range resp.Rounds {
		if round.State == daemonrpc.RoundState_ROUND_STATE_FAILED {
			return false, fmt.Errorf("%s has failed round %s",
				name, round.RoundId)
		}

		if roundStateAtLeast(round.State, minState) {
			return true, nil
		}
	}

	return false, nil
}

// roundStateAtLeast reports whether a round has reached minimum in the normal
// client round state progression.
func roundStateAtLeast(current, minimum daemonrpc.RoundState) bool {
	currentRank := roundStateRank(current)
	minimumRank := roundStateRank(minimum)

	return currentRank >= 0 && minimumRank >= 0 &&
		currentRank >= minimumRank
}

// roundStateRank maps client round states onto their lifecycle order. The
// protobuf enum values are mostly ordered, but QUOTE_RECEIVED was added later
// and has a higher numeric value than later lifecycle states.
func roundStateRank(state daemonrpc.RoundState) int {
	switch state {
	case daemonrpc.RoundState_ROUND_STATE_IDLE:
		return 0
	case daemonrpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY:
		return 1
	case daemonrpc.RoundState_ROUND_STATE_REGISTRATION_SENT:
		return 2
	case daemonrpc.RoundState_ROUND_STATE_QUOTE_RECEIVED:
		return 3
	case daemonrpc.RoundState_ROUND_STATE_JOINED:
		return 4
	case daemonrpc.RoundState_ROUND_STATE_COMMITMENT_RECEIVED:
		return 5
	case daemonrpc.RoundState_ROUND_STATE_COMMITMENT_VALIDATED:
		return 6
	case daemonrpc.RoundState_ROUND_STATE_FORFEIT_COLLECTING:
		return 7
	case daemonrpc.RoundState_ROUND_STATE_NONCES_SENT:
		return 8
	case daemonrpc.RoundState_ROUND_STATE_NONCES_AGGREGATED:
		return 9
	case daemonrpc.RoundState_ROUND_STATE_PARTIAL_SIGS_SENT:
		return 10
	case daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT:
		return 11
	case daemonrpc.RoundState_ROUND_STATE_CONFIRMED:
		return 12
	default:
		return -1
	}
}

// waitAllVTXOBalances waits for every bootstrapped client to see VTXO balance.
func (r *stressRunner) waitAllVTXOBalances() error {
	deadline := time.Now().Add(stressRoundWaitTimeout)
	for time.Now().Before(deadline) {
		allReady := true
		for _, name := range r.names {
			balance, err := r.clientBalance(name)
			if err != nil || balance.VtxoBalanceSat <= 0 {
				allReady = false
				break
			}
		}
		if allReady {
			return nil
		}
		time.Sleep(stressRoundPollInterval)
	}

	return fmt.Errorf("timed out waiting for bootstrapped VTXO balances")
}

// writeSummary writes summary.json and emits the final sparse summary event.
func (r *stressRunner) writeSummary() {
	completed := time.Now()
	summary := r.finalSummary(completed)

	path := filepath.Join(r.state.RunDir, stressSummaryName)
	buf, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		r.t.Fatalf("marshal summary: %v", err)
	}
	if err := os.WriteFile(path, append(buf, '\n'), 0o600); err != nil {
		r.t.Fatalf("write summary: %v", err)
	}

	r.printFinalSummary(path, summary)
}

// finalSummary returns a snapshot with derived latency and throughput fields.
func (r *stressRunner) finalSummary(completed time.Time) stressSummary {
	r.mu.Lock()
	defer r.mu.Unlock()

	summary := r.summary
	summary.Seed = r.cfg.seed
	summary.StartedAt = r.started.UTC().Format(time.RFC3339)
	summary.CompletedAt = completed.UTC().Format(time.RFC3339)
	summary.DurationMS = completed.Sub(r.started).Milliseconds()
	summary.ArtifactsDir = r.state.RunDir
	summary.Clients = len(r.names)
	summary.Concurrency = r.cfg.concurrency

	if summary.PaymentsAttempted > 0 {
		settled := float64(summary.PaymentsSettled)
		attempted := float64(summary.PaymentsAttempted)
		summary.PaymentSuccessPct = 100 * settled / attempted
	}

	durationSeconds := completed.Sub(r.started).Seconds()
	if durationSeconds > 0 {
		summary.PaymentThroughput = float64(summary.PaymentsSettled) /
			durationSeconds
	}

	latencies := append([]time.Duration(nil), r.paymentLatencies...)
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool {
			return latencies[i] < latencies[j]
		})

		var total time.Duration
		for _, latency := range latencies {
			total += latency
		}

		summary.PaymentAvgMS = (total / time.Duration(len(latencies))).
			Milliseconds()
		summary.PaymentP50MS = percentileDuration(latencies, 50).
			Milliseconds()
		summary.PaymentP95MS = percentileDuration(latencies, 95).
			Milliseconds()
		maxLatency := latencies[len(latencies)-1]
		summary.PaymentMaxMS = maxLatency.Milliseconds()
	}

	r.summary = summary

	return summary
}

// printFinalSummary emits a prominent human-readable stress summary.
func (r *stressRunner) printFinalSummary(path string, summary stressSummary) {
	verdict := "PASS"
	if summary.PaymentsFailed > 0 || summary.RoundsFailed > 0 {
		verdict = "FAILURES"
	}

	r.events.Print("stress_summary", stressSummaryTopLine, nil)
	r.events.Printf("stress_summary", map[string]any{
		"summary": path,
		"verdict": verdict,
	}, "RESULT=%s artifacts=%s", verdict, r.state.RunDir)
	r.events.Printf("stress_summary", map[string]any{
		"attempted": summary.PaymentsAttempted,
		"settled":   summary.PaymentsSettled,
		"failed":    summary.PaymentsFailed,
		"success":   summary.PaymentSuccessPct,
	}, "payments settled=%d/%d failed=%d success=%.1f%%",
		summary.PaymentsSettled, summary.PaymentsAttempted,
		summary.PaymentsFailed, summary.PaymentSuccessPct)
	r.events.Printf("stress_summary", map[string]any{
		"avg_ms": summary.PaymentAvgMS,
		"p50_ms": summary.PaymentP50MS,
		"p95_ms": summary.PaymentP95MS,
		"max_ms": summary.PaymentMaxMS,
	}, "payment latency avg=%dms p50=%dms p95=%dms max=%dms",
		summary.PaymentAvgMS, summary.PaymentP50MS,
		summary.PaymentP95MS, summary.PaymentMaxMS)
	r.events.Printf("stress_summary", map[string]any{
		"throughput_per_sec": summary.PaymentThroughput,
		"duration_ms":        summary.DurationMS,
		"concurrency":        summary.Concurrency,
	}, "throughput %.2f settled payments/sec duration=%s concurrency=%d",
		summary.PaymentThroughput,
		time.Duration(summary.DurationMS)*time.Millisecond,
		summary.Concurrency)
	r.events.Printf("stress_summary", map[string]any{
		"rounds":            summary.RoundsTriggered,
		"round_confirmed":   summary.RoundsConfirmed,
		"round_failures":    summary.RoundsFailed,
		"client_restarts":   summary.ClientRestarts,
		"client_crashes":    summary.ClientCrashes,
		"operator_restarts": summary.OperatorRestarts,
	}, "rounds confirmed=%d/%d failed=%d client_restarts=%d "+
		"client_crashes=%d operator_restarts=%d",
		summary.RoundsConfirmed, summary.RoundsTriggered,
		summary.RoundsFailed,
		summary.ClientRestarts, summary.ClientCrashes,
		summary.OperatorRestarts)
	r.events.Print("stress_summary", stressSummaryBottomLine, nil)
}

// percentileDuration returns the nearest-rank percentile from a sorted
// duration slice.
func percentileDuration(sorted []time.Duration, pct int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}

	idx := (pct*len(sorted) + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}

	return sorted[idx-1]
}

// shortContext returns a bounded context and its cancel function.
func (r *stressRunner) shortContext() (context.Context, context.CancelFunc) {
	return r.contextWithTimeout(30 * time.Second)
}

// contextWithTimeout returns a context bundle with the requested timeout.
func (r *stressRunner) contextWithTimeout(
	timeout time.Duration) (context.Context, context.CancelFunc) {

	return context.WithTimeout(context.Background(), timeout)
}

// stressClientNames returns stable zero-padded names for stress clients.
func stressClientNames(count int) []string {
	width := int(math.Log10(float64(count))) + 1
	if width < 2 {
		width = 2
	}

	names := make([]string, count)
	for i := range names {
		names[i] = fmt.Sprintf("client%0*d", width, i+1)
	}

	sort.Strings(names)

	return names
}

// minInt64 returns the smaller of two int64 values.
func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}

	return b
}

// requireMkdir creates a directory or fails the active test.
func requireMkdir(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}
