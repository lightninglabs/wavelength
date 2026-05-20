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
	"strings"
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

	// defaultStressMaxUnrolls is the default random unroll budget.
	defaultStressMaxUnrolls = 5

	// defaultStressMaxRestarts is the default restart/crash disruption
	// budget.
	defaultStressMaxRestarts = 5

	// defaultStressMaxReorgs is the default chain reorg disruption budget.
	defaultStressMaxReorgs = 0

	// defaultStressConcurrency is the default number of concurrent stress
	// operations.
	defaultStressConcurrency = 4

	// defaultStressDuration is the default maximum stress runtime.
	defaultStressDuration = 10 * time.Minute

	// defaultStressPaymentLiquidityTimeout bounds how long a payment worker
	// waits for live, unreserved sender VTXOs before recording a skip.
	defaultStressPaymentLiquidityTimeout = 10 * time.Second

	// defaultStressUnrollTimeout bounds one random unroll attempt. The
	// background miner supplies block progress, so this is a job-level
	// watchdog rather than a block-driving cadence.
	defaultStressUnrollTimeout = 15 * time.Minute

	// defaultStressTraceDuration keeps optional runtime traces short enough
	// for the Go trace browser to load comfortably during stress runs.
	defaultStressTraceDuration = time.Minute

	// defaultStressMinPayment is the default smallest OOR payment amount.
	defaultStressMinPayment = int64(1_000)

	// defaultStressMaxPayment is the default largest OOR payment amount.
	defaultStressMaxPayment = int64(50_000)

	// defaultStressBoardAmount is boarded into each client at bootstrap.
	defaultStressBoardAmount = int64(250_000)

	// defaultStressBoardVTXOs is the default number of VTXOs each client
	// receives from bootstrap boarding.
	defaultStressBoardVTXOs = 1

	// defaultStressReorgDepth is the default number of active-chain blocks
	// disconnected by one stress reorg.
	defaultStressReorgDepth = 2

	// defaultStressReorgNewBlocks is the default number of replacement
	// branch blocks mined by one stress reorg.
	defaultStressReorgNewBlocks = 3

	// defaultStressReorgMinInterval is the default minimum delay between
	// reserved stress reorg events.
	defaultStressReorgMinInterval = 30 * time.Second

	// defaultStressMineIntervalMin is the shortest default delay between
	// background miner blocks. Set --mine-interval-min=0 to disable the
	// background miner and return to event-driven mining only.
	defaultStressMineIntervalMin = 2 * time.Second

	// defaultStressMineIntervalMax is the longest default delay between
	// background miner blocks.
	defaultStressMineIntervalMax = 10 * time.Second

	// stressReorgRecoveryTimeout bounds the best-effort rollback path used
	// after invalidateblock succeeds but replacement mining fails.
	stressReorgRecoveryTimeout = 30 * time.Second

	// minSatsPerBoardedVTXO rejects fanout shapes that would create tiny
	// VTXOs and fail later with less useful daemon-side dust errors.
	minSatsPerBoardedVTXO = int64(500)

	// stressSummaryName is the final machine-readable summary artifact.
	stressSummaryName = "summary.json"

	// stressSummaryTopLine is the visible terminal summary banner opener.
	stressSummaryTopLine = "========== ARKTEST STRESS SUMMARY =========="

	// stressSummaryBottomLine is the terminal summary banner closer.
	stressSummaryBottomLine = "============================================"

	// stressSenderScanTerminalLimit bounds per-client scan rows printed for
	// one skipped payment. The full scan is always written to events.jsonl.
	stressSenderScanTerminalLimit = 12

	// stressLiveVTXOCacheTTL keeps sender selection from hammering
	// each in-process client daemon with repeated ListVTXOs calls
	// while preserving a short enough view that reservations and
	// invalidations remain useful.
	stressLiveVTXOCacheTTL = 250 * time.Millisecond

	// stressPaymentLiquidityPollInterval is the cadence for retrying sender
	// selection while a payment worker is waiting for spendable liquidity.
	stressPaymentLiquidityPollInterval = 250 * time.Millisecond

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
	operatorDB       string
	lndImage         string
	trace            bool
	traceFile        string
	traceDuration    time.Duration
	cpuProfile       bool
	cpuProfileFile   string
	blockProfile     bool
	blockProfileFile string
	mutexProfile     bool
	mutexProfileFile string
	operatorFunds    int64
	clientLNDFunds   int64
	clientCount      int
	maxPayments      int
	maxRounds        int
	maxUnrolls       int
	maxRestarts      int
	maxReorgs        int
	reorgDepth       int
	reorgNewBlocks   int
	reorgMinInterval time.Duration
	mineIntervalMin  time.Duration
	mineIntervalMax  time.Duration
	concurrency      int
	duration         time.Duration
	liquidityTimeout time.Duration
	paymentMinDelay  time.Duration
	unrollTimeout    time.Duration
	seed             int64
	minPayment       int64
	maxPayment       int64
	boardAmount      int64
	boardVTXOs       int
	logStdout        bool
	operatorRestarts bool
	clientRestarts   bool
	clientCrashes    bool
}

// stressSummary is written to summary.json when a stress run completes.
type stressSummary struct {
	Seed               int64          `json:"seed"`
	StartedAt          string         `json:"started_at"`
	CompletedAt        string         `json:"completed_at"`
	DurationMS         int64          `json:"duration_ms"`
	ArtifactsDir       string         `json:"artifacts_dir"`
	Clients            int            `json:"clients"`
	OperatorDB         string         `json:"operator_db"`
	BoardAmountSat     int64          `json:"board_amount_sat"`
	BoardVTXOs         int            `json:"board_vtxos_per_client"`
	HarnessResult      string         `json:"harness_result"`
	WorkloadResult     string         `json:"workload_result"`
	InvariantsResult   string         `json:"invariants_result"`
	RecoveryResult     string         `json:"recovery_result"`
	ExpectedFailures   int            `json:"expected_failures"`
	UnexpectedFailures int            `json:"unexpected_failures"`
	FailureClasses     map[string]int `json:"failure_classes,omitempty"`
	RecoveryFailures   []string       `json:"recovery_failures,omitempty"`
	PaymentsAttempted  int            `json:"payments_attempted"`
	PaymentsSettled    int            `json:"payments_settled"`
	PaymentsFailed     int            `json:"payments_failed"`
	PaymentsSkipped    int            `json:"payments_skipped"`
	Skips              map[string]int `json:"payment_skip_classes,omitempty"` //nolint:ll
	PaymentSuccessPct  float64        `json:"payment_success_pct"`
	PaymentAvgMS       int64          `json:"payment_avg_ms"`
	PaymentP50MS       int64          `json:"payment_p50_ms"`
	PaymentP95MS       int64          `json:"payment_p95_ms"`
	PaymentMaxMS       int64          `json:"payment_max_ms"`
	PaymentThroughput  float64        `json:"payment_throughput_per_sec"`
	LiquidityWaits     int            `json:"liquidity_waits"`
	LiquidityTimeouts  int            `json:"liquidity_wait_timeouts"`
	LiquidityWaitAvgMS int64          `json:"liquidity_wait_avg_ms"`
	LiquidityWaitP50MS int64          `json:"liquidity_wait_p50_ms"`
	LiquidityWaitP95MS int64          `json:"liquidity_wait_p95_ms"`
	LiquidityWaitMaxMS int64          `json:"liquidity_wait_max_ms"`
	PaymentMinDelayMS  int64          `json:"payment_min_interval_ms"`
	RoundsTriggered    int            `json:"rounds_triggered"`
	RoundsConfirmed    int            `json:"rounds_confirmed"`
	RoundsFailed       int            `json:"rounds_failed"`
	UnrollsAttempted   int            `json:"unrolls_attempted"`
	UnrollsCompleted   int            `json:"unrolls_completed"`
	UnrollsFailed      int            `json:"unrolls_failed"`
	UnrollsSkipped     int            `json:"unrolls_skipped"`
	UnrollAvgMS        int64          `json:"unroll_avg_ms"`
	UnrollP50MS        int64          `json:"unroll_p50_ms"`
	UnrollP95MS        int64          `json:"unroll_p95_ms"`
	UnrollMaxMS        int64          `json:"unroll_max_ms"`
	ReorgsTriggered    int            `json:"reorgs_triggered"`
	ReorgsCompleted    int            `json:"reorgs_completed"`
	ReorgsFailed       int            `json:"reorgs_failed"`
	BackgroundBlocks   int            `json:"background_blocks_mined"`
	BackgroundMineMin  int64          `json:"background_mine_min_ms,omitempty"` //nolint:ll
	BackgroundMineMax  int64          `json:"background_mine_max_ms,omitempty"` //nolint:ll
	ClientRestarts     int            `json:"client_restarts"`
	ClientCrashes      int            `json:"client_crashes"`
	OperatorRestarts   int            `json:"operator_restarts"`
	Concurrency        int            `json:"concurrency"`
	TraceFile          string         `json:"trace_file,omitempty"`
	CPUProfileFile     string         `json:"cpu_profile_file,omitempty"`
	BlockProfileFile   string         `json:"block_profile_file,omitempty"`
	MutexProfileFile   string         `json:"mutex_profile_file,omitempty"`
}

// stress result values written to summary.json.
const (
	stressResultPass               = "pass"
	stressResultFail               = "fail"
	stressResultExpectedFailures   = "expected_failures"
	stressResultUnexpectedFailures = "unexpected_failures"
)

// stressFailureClass is a stable class for expected/unexpected outcome policy.
type stressFailureClass string

const (
	failureClassUnexpected        stressFailureClass = "unexpected"
	failureClassClientUnavailable stressFailureClass = "client_unavailable"
	failureClassConnectionClosing stressFailureClass = "connection_closing"
	failureClassConnectionRefused stressFailureClass = "connection_refused"
	failureClassGracefulStop      stressFailureClass = "graceful_stop"
	failureClassDeadlineExceeded  stressFailureClass = "deadline_exceeded"
	failureClassDustChange        stressFailureClass = "dust_change"
	failureClassInsufficientFunds stressFailureClass = "insufficient_funds"
	failureClassNoFundedSender    stressFailureClass = "no_funded_sender"
	failureClassNoLiveVTXOs       stressFailureClass = "no_live_vtxos"
	failureClassNoRegistered      stressFailureClass = "no_registered_" +
		"clients"
	failureClassRoundTimeout   stressFailureClass = "round_timeout"
	failureClassFailedRound    stressFailureClass = "failed_round"
	failureClassPendingForfeit stressFailureClass = "pending_forfeit"
	failureClassReorgFailed    stressFailureClass = "reorg_failed"
)

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
	paymentReserved  map[string]map[string]int64
	unrollReserved   map[string]map[string]int64
	paymentJobs      int
	unrollJobs       int
	liveVTXOMu       sync.Mutex
	liveVTXOCache    map[string]liveVTXOCacheEntry
	roundMu          sync.Mutex
	operatorMu       sync.Mutex
	chainMu          sync.Mutex
	names            []string
	started          time.Time
	diagnostics      *stressDiagnostics
	diagnosticPaths  stressDiagnosticPaths
	summary          stressSummary
	paymentLatencies []time.Duration
	unrollLatencies  []time.Duration
	liquidityWaits   []time.Duration
	workloadDeadline time.Time
	nextPaymentAt    time.Time
	nextReorgAt      time.Time
	minerCancel      context.CancelFunc
	minerWG          sync.WaitGroup
}

// stressReorgResult describes one chain reorg injected by the stress runner.
type stressReorgResult struct {
	OldTip       clientharness.BlockHeader
	ForkPoint    clientharness.BlockHeader
	Disconnected []clientharness.BlockHeader
	Connected    []clientharness.BlockHeader
}

// liveVTXOCacheEntry is a short-lived stress-runner snapshot of a client's
// live VTXOs.
type liveVTXOCacheEntry struct {
	fetchedAt time.Time
	vtxos     []*daemonrpc.VTXO
}

var stressCfg stressConfig

// newStressCmd creates the random workload runner.
func newStressCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stress",
		Short: "Run a sparse-log random arktest workload",
		Long: "Starts one local Ark topology, boards a client set, " +
			"then concurrently performs random OOR payments, " +
			"round refreshes, unilateral exits, graceful " +
			"restarts, and client crash/recover cycles while " +
			"the background miner advances blocks.",
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
		&stressCfg.operatorDB, "operator-db", "sqlite",
		"operator database backend: sqlite or postgres",
	)
	f.StringVar(
		&stressCfg.lndImage, "lnd-image", "",
		"override the default LND docker image",
	)
	f.BoolVar(
		&stressCfg.trace, "trace", false,
		"capture a Go runtime trace into the stress artifacts",
	)
	f.StringVar(
		&stressCfg.traceFile, "trace-file", "",
		"runtime trace output path; relative to the run dir",
	)
	f.DurationVar(
		&stressCfg.traceDuration, "trace-duration",
		defaultStressTraceDuration,
		"stop runtime trace after this duration; zero traces until end",
	)
	f.BoolVar(
		&stressCfg.cpuProfile, "cpu-profile", true,
		"capture a CPU profile into the stress artifacts",
	)
	f.StringVar(
		&stressCfg.cpuProfileFile, "cpu-profile-file", "",
		"CPU profile output path; relative paths are under the run dir",
	)
	f.BoolVar(
		&stressCfg.blockProfile, "block-profile", true,
		"write a sampled block profile at the end of the stress run",
	)
	f.StringVar(
		&stressCfg.blockProfileFile, "block-profile-file", "",
		"block profile output path; relative to the run dir",
	)
	f.BoolVar(
		&stressCfg.mutexProfile, "mutex-profile", true,
		"write a sampled mutex profile at the end of the stress run",
	)
	f.StringVar(
		&stressCfg.mutexProfileFile, "mutex-profile-file", "",
		"mutex profile output path; relative to the run dir",
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
		defaultStressMaxPayments, "maximum random OOR payment attempts",
	)
	f.IntVar(
		&stressCfg.maxRounds, "max-rounds", defaultStressMaxRounds,
		"maximum random refresh rounds after bootstrap",
	)
	f.IntVar(
		&stressCfg.maxUnrolls, "max-unrolls", defaultStressMaxUnrolls,
		"maximum random unilateral exit attempts",
	)
	f.IntVar(
		&stressCfg.maxRestarts, "max-restarts",
		defaultStressMaxRestarts,
		"maximum restart/crash disruption events",
	)
	f.IntVar(
		&stressCfg.maxReorgs, "max-reorgs", defaultStressMaxReorgs,
		"maximum chain reorg disruption events",
	)
	f.IntVar(
		&stressCfg.reorgDepth, "reorg-depth", defaultStressReorgDepth,
		"number of active-chain blocks to disconnect per reorg",
	)
	f.IntVar(
		&stressCfg.reorgNewBlocks, "reorg-new-blocks",
		defaultStressReorgNewBlocks,
		"number of replacement-branch blocks to mine per reorg",
	)
	f.DurationVar(
		&stressCfg.reorgMinInterval, "reorg-min-interval",
		defaultStressReorgMinInterval,
		"minimum delay between chain reorg disruption events",
	)
	f.DurationVar(
		&stressCfg.mineIntervalMin, "mine-interval-min",
		defaultStressMineIntervalMin, "minimum delay between "+
			"background-mined blocks; zero disables background "+
			"mining",
	)
	f.DurationVar(
		&stressCfg.mineIntervalMax, "mine-interval-max",
		defaultStressMineIntervalMax, "maximum delay between "+
			"background-mined blocks; if zero, uses "+
			"--mine-interval-min",
	)
	f.IntVar(
		&stressCfg.concurrency, "concurrency", defaultStressConcurrency,
		"maximum concurrent random workload operations",
	)
	f.DurationVar(
		&stressCfg.duration, "duration", defaultStressDuration,
		"maximum wall-clock runtime",
	)
	f.DurationVar(
		&stressCfg.liquidityTimeout, "payment-liquidity-timeout",
		defaultStressPaymentLiquidityTimeout, "maximum time a "+
			"payment worker waits for live sender liquidity; "+
			"zero records no-funded-sender skips immediately",
	)
	f.DurationVar(
		&stressCfg.paymentMinDelay, "payment-min-interval", 0,
		"minimum delay between scheduled payment jobs; zero disables "+
			"payment pacing",
	)
	f.DurationVar(
		&stressCfg.unrollTimeout, "unroll-timeout",
		defaultStressUnrollTimeout,
		"maximum time to wait for one random unroll to complete",
	)
	f.Int64Var(
		&stressCfg.seed, "seed", 0,
		"workload seed; zero chooses the current time",
	)
	f.Int64Var(
		&stressCfg.minPayment, "min-payment", defaultStressMinPayment,
		"minimum random OOR payment amount in sats",
	)
	f.Int64Var(
		&stressCfg.maxPayment, "max-payment", defaultStressMaxPayment,
		"maximum random OOR payment amount in sats",
	)
	f.Int64Var(
		&stressCfg.boardAmount, "board-amount",
		defaultStressBoardAmount,
		"satoshis boarded into each client before stress starts",
	)
	f.IntVar(
		&stressCfg.boardVTXOs, "board-vtxos-per-client",
		defaultStressBoardVTXOs,
		"number of VTXOs each client's boarded balance fans into",
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

	runner.startBackgroundMiner()
	runner.bootstrapBoarding()
	runner.runWorkload()
	runner.checkRecovery()
	runner.writeSummary()
}

// normalizeStressConfig validates stress flags and fills derived defaults.
func normalizeStressConfig(t *testing.T, cfg stressConfig) stressConfig {
	t.Helper()

	if cfg.clientCount < 2 {
		t.Fatalf("--clients must be at least 2")
	}
	if cfg.operatorDB != "sqlite" && cfg.operatorDB != "postgres" {
		t.Fatalf("--operator-db must be sqlite or postgres")
	}
	if cfg.maxPayments < 0 || cfg.maxRounds < 0 || cfg.maxUnrolls < 0 ||
		cfg.maxRestarts < 0 || cfg.maxReorgs < 0 {

		t.Fatalf("stress budgets must be non-negative")
	}
	if cfg.reorgDepth < 0 {
		t.Fatalf("--reorg-depth must be non-negative")
	} else if cfg.reorgDepth == 0 {
		cfg.reorgDepth = defaultStressReorgDepth
	}
	if cfg.reorgNewBlocks < 0 {
		t.Fatalf("--reorg-new-blocks must be non-negative")
	} else if cfg.reorgNewBlocks == 0 {
		cfg.reorgNewBlocks = defaultStressReorgNewBlocks
	}
	if cfg.reorgNewBlocks <= cfg.reorgDepth {
		t.Fatalf("--reorg-new-blocks must be greater than " +
			"--reorg-depth")
	}
	if cfg.reorgMinInterval < 0 {
		t.Fatalf("--reorg-min-interval must be non-negative")
	}
	if cfg.mineIntervalMin < 0 {
		t.Fatalf("--mine-interval-min must be non-negative")
	}
	if cfg.mineIntervalMax < 0 {
		t.Fatalf("--mine-interval-max must be non-negative")
	}
	switch {
	case cfg.mineIntervalMin == 0:
		cfg.mineIntervalMax = 0

	case cfg.mineIntervalMax == 0:
		cfg.mineIntervalMax = cfg.mineIntervalMin

	case cfg.mineIntervalMax < cfg.mineIntervalMin:
		t.Fatalf("--mine-interval-max must be greater than or equal " +
			"to --mine-interval-min")
	}
	if cfg.concurrency <= 0 {
		t.Fatalf("--concurrency must be positive")
	}
	if cfg.duration <= 0 {
		t.Fatalf("--duration must be positive")
	}
	if cfg.liquidityTimeout < 0 {
		t.Fatalf("--payment-liquidity-timeout must be non-negative")
	}
	if cfg.paymentMinDelay < 0 {
		t.Fatalf("--payment-min-interval must be non-negative")
	}
	if cfg.unrollTimeout < 0 {
		t.Fatalf("--unroll-timeout must be non-negative")
	} else if cfg.unrollTimeout == 0 {
		cfg.unrollTimeout = defaultStressUnrollTimeout
	}
	if cfg.traceDuration < 0 {
		t.Fatalf("--trace-duration must be non-negative")
	}
	if cfg.boardAmount <= 0 {
		t.Fatalf("--board-amount must be positive")
	}
	if cfg.boardVTXOs <= 0 {
		t.Fatalf("--board-vtxos-per-client must be positive")
	}
	if uint64(cfg.boardVTXOs) > math.MaxUint32 {
		t.Fatalf("--board-vtxos-per-client exceeds uint32 max")
	}
	if int64(cfg.boardVTXOs)*minSatsPerBoardedVTXO > cfg.boardAmount {
		t.Fatalf("--board-vtxos-per-client is too large for "+
			"--board-amount: need at least %d sat per VTXO",
			minSatsPerBoardedVTXO)
	}
	if cfg.minPayment <= 0 || cfg.maxPayment < cfg.minPayment {
		t.Fatalf("invalid payment range")
	}
	if cfg.seed == 0 {
		cfg.seed = time.Now().UnixNano()
	}
	if cfg.traceFile != "" {
		cfg.trace = true
	}
	if cfg.cpuProfileFile != "" {
		cfg.cpuProfile = true
	}
	if cfg.blockProfileFile != "" {
		cfg.blockProfile = true
	}
	if cfg.mutexProfileFile != "" {
		cfg.mutexProfile = true
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
		t:               t,
		cfg:             cfg,
		rng:             rand.New(rand.NewSource(cfg.seed)),
		clients:         clients,
		clientLocks:     clientLocks,
		paymentReserved: make(map[string]map[string]int64, len(names)),
		unrollReserved:  make(map[string]map[string]int64, len(names)),
		liveVTXOCache: make(
			map[string]liveVTXOCacheEntry, len(names),
		),
		names: names,
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
		OperatorDBBackend:      r.cfg.operatorDB,
		ClientDaemonWalletType: r.cfg.clientWallet,
		OperatorDebugLevel:     "debug",
		ClientDebugLevel:       "debug",
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
			"operator_db": r.cfg.operatorDB,
			"payment_min_interval_ms": r.cfg.paymentMinDelay.
				Milliseconds(),
			"group":     r.cfg.groupName,
			"artifacts": artifactsAbs,
		})

	r.startDiagnostics()

	if r.cfg.operatorFunds > 0 {
		r.events.Printf("fund", map[string]any{
			"amount_sat": r.cfg.operatorFunds,
		},
			"funding operator lnd amount=%d", r.cfg.operatorFunds)
		r.h.Harness.FundOperatorLND(
			btcutil.Amount(r.cfg.operatorFunds),
		)
		r.events.Printf("fund", map[string]any{
			"amount_sat": r.cfg.operatorFunds,
		},
			"operator lnd funded amount=%d", r.cfg.operatorFunds)
	}

	for _, name := range r.names {
		r.startClient(name)
	}

	if err := r.saveCurrentState(); err != nil {
		r.t.Fatalf("save state: %v", err)
	}

	r.events.Printf("ready", map[string]any{
		"run_dir":     r.state.RunDir,
		"clients":     r.names,
		"operator_db": r.cfg.operatorDB,
	},
		"arktest stress ready clients=%d operator_db=%s "+
			"artifacts=%s seed=%d",
		len(r.names), r.cfg.operatorDB, r.state.RunDir, r.cfg.seed)
}

// stop tears down the live topology and closes the sparse event artifact.
func (r *stressRunner) stop() {
	r.stopBackgroundMiner()
	r.stopDiagnostics("teardown")
	if r.h != nil {
		r.h.Stop()
	}
	if r.events != nil {
		_ = r.events.Close()
	}
}

// startBackgroundMiner starts an optional chain clock for stress runs. It
// begins after the topology and state file are ready, before bootstrap
// boarding, so unroll/recovery flows see steady block progress from the start
// of the useful test workload.
func (r *stressRunner) startBackgroundMiner() {
	if r.cfg.mineIntervalMin == 0 {
		r.events.Print(
			"background_miner", "background miner disabled", nil,
		)

		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.minerCancel = cancel
	r.minerWG.Add(1)

	r.events.Printf("background_miner", map[string]any{
		"min_interval_ms": r.cfg.mineIntervalMin.Milliseconds(),
		"max_interval_ms": r.cfg.mineIntervalMax.Milliseconds(),
	},
		"background miner started interval=%s..%s",
		r.cfg.mineIntervalMin, r.cfg.mineIntervalMax)

	go func() {
		defer r.minerWG.Done()

		for {
			delay := r.nextBackgroundMineDelay()
			timer := time.NewTimer(delay)

			select {
			case <-ctx.Done():
				timer.Stop()

				return

			case <-timer.C:
			}

			r.backgroundMineBlock(ctx, delay)
		}
	}()
}

// stopBackgroundMiner stops the optional chain clock before the harness tears
// down bitcoind and dependent services.
func (r *stressRunner) stopBackgroundMiner() {
	if r.minerCancel == nil {
		return
	}

	r.minerCancel()
	r.minerWG.Wait()
	r.minerCancel = nil
}

// nextBackgroundMineDelay returns the next variable delay between background
// blocks. A fixed interval is used when min == max.
func (r *stressRunner) nextBackgroundMineDelay() time.Duration {
	minDelay := r.cfg.mineIntervalMin
	maxDelay := r.cfg.mineIntervalMax
	if maxDelay <= minDelay {
		return minDelay
	}

	spread := maxDelay - minDelay

	r.mu.Lock()
	jitter := time.Duration(r.rng.Int63n(int64(spread) + 1))
	r.mu.Unlock()

	return minDelay + jitter
}

// backgroundMineBlock mines one block under the shared chain mutation lock.
func (r *stressRunner) backgroundMineBlock(ctx context.Context,
	delay time.Duration) {

	if ctx.Err() != nil {
		return
	}

	start := time.Now()
	r.chainMu.Lock()
	defer r.chainMu.Unlock()

	if ctx.Err() != nil {
		return
	}

	r.h.Harness.Generate(1)
	count := r.recordBackgroundBlockMined()
	r.events.Printf("background_mine", map[string]any{
		"blocks":      1,
		"count":       count,
		"interval_ms": delay.Milliseconds(),
		"latency_ms":  time.Since(start).Milliseconds(),
	},
		"background mined block count=%d interval=%s latency=%s", count,
		delay.Round(time.Millisecond),
		time.Since(start).Round(time.Millisecond))
}

// recordBackgroundBlockMined increments the background-mined block counter.
func (r *stressRunner) recordBackgroundBlockMined() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.summary.BackgroundBlocks++

	return r.summary.BackgroundBlocks
}

// mineStressBlocks mines blocks under the shared chain mutation lock.
func (r *stressRunner) mineStressBlocks(blocks int) {
	r.chainMu.Lock()
	defer r.chainMu.Unlock()

	r.h.Harness.Generate(blocks)
}

// startClient starts one client daemon and records its state.
func (r *stressRunner) startClient(name string) {
	r.t.Helper()

	r.events.Printf("client_start", map[string]any{
		"client": name,
		"wallet": r.cfg.clientWallet,
	},
		"starting client %s wallet=%s", name, r.cfg.clientWallet)
	client := r.h.StartClientDaemon(name)
	r.setClient(name, client)
	r.events.Printf("client_ready", map[string]any{
		"client": name,
		"rpc":    client.RPCAddr,
	},
		"client %s ready rpc=%s", name, client.RPCAddr)

	if r.cfg.clientLNDFunds > 0 {
		r.events.Printf("fund", map[string]any{
			"client":     name,
			"amount_sat": r.cfg.clientLNDFunds,
		},
			"funding client %s lnd wallet amount=%d", name,
			r.cfg.clientLNDFunds)
		r.h.FundClientWallet(
			client, btcutil.Amount(r.cfg.clientLNDFunds),
		)
		r.events.Printf("fund", map[string]any{
			"client":     name,
			"amount_sat": r.cfg.clientLNDFunds,
		},
			"client %s lnd wallet funded amount=%d", name,
			r.cfg.clientLNDFunds)
	}
}

// setClient records the active harness handle and persisted state for a client.
func (r *stressRunner) setClient(name string,
	client *darepoharness.ClientDaemonHarness) {

	r.mu.Lock()
	r.clients[name] = client
	r.recordClientStateLocked(name, client)
	r.mu.Unlock()

	r.invalidateLiveVTXOs(name)
}

// getClient returns the current harness handle for a client.
func (r *stressRunner) getClient(
	name string) *darepoharness.ClientDaemonHarness {

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.clients[name]
}

// clientRPC returns the current daemon RPC client for workload operations.
func (r *stressRunner) clientRPC(name string) (daemonrpc.DaemonServiceClient,
	error) {

	client := r.getClient(name)
	if client == nil || client.RPCClient == nil {
		return nil, fmt.Errorf("client %s daemon unavailable", name)
	}

	return client.RPCClient, nil
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

// bootstrapBoarding funds each stress client and submits one Board request per
// client, optionally fanning the balance into multiple VTXOs.
func (r *stressRunner) bootstrapBoarding() {
	ctx, cancel := r.contextWithTimeout(5 * time.Minute)
	defer cancel()

	r.events.Printf("bootstrap", nil,
		"boarding %d clients amount=%d vtxos_per_client=%d",
		len(r.names), r.cfg.boardAmount, r.cfg.boardVTXOs)

	for _, name := range r.names {
		client := r.getClient(name)
		r.events.Printf("bootstrap", map[string]any{
			"client": name,
		},
			"client %s requesting boarding address", name)
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
		},
			"funding client %s boarding address amount=%d", name,
			r.cfg.boardAmount)
		r.h.Harness.Faucet(
			addr.Address, btcutil.Amount(r.cfg.boardAmount),
		)
		r.events.Printf("bootstrap", map[string]any{
			"client":     name,
			"address":    addr.Address,
			"amount_sat": r.cfg.boardAmount,
		},
			"client %s boarding address funded", name)
	}
	r.events.Printf("bootstrap", map[string]any{
		"blocks": 6,
	},
		"mining boarding confirmations blocks=%d", 6)
	r.mineStressBlocks(6)
	r.events.Printf("bootstrap", map[string]any{
		"blocks": 6,
	},
		"boarding confirmations mined blocks=%d", 6)

	for _, name := range r.names {
		r.events.Printf("bootstrap", map[string]any{
			"client":            name,
			"target_vtxo_count": r.cfg.boardVTXOs,
		},
			"client %s submitting board request "+
				"target_vtxo_count=%d",
			name, r.cfg.boardVTXOs)
		_, err := r.getClient(name).RPCClient.Board(
			ctx, &daemonrpc.BoardRequest{
				TargetVtxoCount: uint32(r.cfg.boardVTXOs),
			},
		)
		if err != nil {
			r.t.Fatalf("%s Board: %v", name, err)
		}

		if err := r.waitClientRoundAtLeast(
			name, daemonrpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY,
			stressRoundWaitTimeout,
		); err != nil {

			r.t.Fatalf("wait for %s bootstrap intent: %v", name,
				err)
		}
		state := daemonrpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY.
			String()
		r.events.Printf("bootstrap", map[string]any{
			"client": name,
			"state":  state,
		},
			"client %s board intent ready", name)

		r.getClient(name).TriggerRoundRegistration()
		r.events.Printf("bootstrap", map[string]any{
			"client": name,
		},
			"client %s triggered round registration", name)
	}

	r.events.Print(
		"bootstrap", "waiting for all clients to send round "+
			"registration", nil,
	)
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
	},
		"bootstrap batch triggered round=%s", resp.RoundId)
	if err := r.confirmRound(resp.RoundId); err != nil {
		r.t.Fatalf("confirm bootstrap round: %v", err)
	}

	if err := r.waitAllVTXOBalances(); err != nil {
		r.t.Fatalf("%v", err)
	}
	r.events.Printf("round", map[string]any{
		"round_id": resp.RoundId,
	},
		"bootstrap round confirmed round=%s", resp.RoundId)
}

// runWorkload runs random events until all configured budgets are exhausted or
// the duration limit is reached.
func (r *stressRunner) runWorkload() {
	deadline := time.Now().Add(r.cfg.duration)
	r.workloadDeadline = deadline
	sem := make(chan struct{}, r.cfg.concurrency)
	var wg sync.WaitGroup
	activeJobs := 0

	for time.Now().Before(deadline) {
		hasBudget, hasActiveJobs := r.workloadState(&activeJobs)
		if !hasBudget {
			if !hasActiveJobs {
				break
			}

			time.Sleep(25 * time.Millisecond)

			continue
		}

		sem <- struct{}{}
		if !time.Now().Before(deadline) {
			<-sem
			break
		}

		job, ok := r.reserveNextJob()
		if !ok {
			<-sem
			time.Sleep(r.schedulableRetryDelay())
			continue
		}

		r.mu.Lock()
		activeJobs++
		r.mu.Unlock()
		wg.Add(1)
		go func(job stressJob) {
			defer wg.Done()
			defer func() {
				r.mu.Lock()
				activeJobs--
				r.mu.Unlock()
				<-sem
			}()

			r.runJob(job)
		}(job)

		r.sleepBetweenJobs()
	}

	wg.Wait()
}

// workloadState returns whether any event budget or worker remains. Both reads
// share r.mu so a worker cannot release payment budget and finish between a
// stale budget read and the active-worker check.
func (r *stressRunner) workloadState(activeJobs *int) (bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.hasBudgetLocked(), *activeJobs > 0
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
	return r.eventBudgetRemainingLocked(stressEventPayment) ||
		r.eventBudgetRemainingLocked(stressEventRound) ||
		r.eventBudgetRemainingLocked(stressEventUnroll) ||
		r.eventBudgetRemainingLocked(stressEventReorg) ||
		r.eventBudgetRemainingLocked(stressEventClientRestart) ||
		r.eventBudgetRemainingLocked(stressEventClientCrash) ||
		r.eventBudgetRemainingLocked(stressEventOperatorRestart)
}

// hasSchedulableWorkLocked returns true while at least one random event can be
// reserved immediately. The caller must hold r.mu.
func (r *stressRunner) hasSchedulableWorkLocked() bool {
	return r.eventAllowedLocked(stressEventPayment) ||
		r.eventAllowedLocked(stressEventRound) ||
		r.eventAllowedLocked(stressEventUnroll) ||
		r.eventAllowedLocked(stressEventReorg) ||
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

	// stressEventUnroll triggers and waits for one random unilateral exit.
	stressEventUnroll

	// stressEventReorg disconnects chain blocks and mines a longer branch.
	stressEventReorg

	// stressEventClientRestart gracefully restarts one client.
	stressEventClientRestart

	// stressEventClientCrash crashes and recovers one client.
	stressEventClientCrash

	// stressEventOperatorRestart gracefully restarts the operator.
	stressEventOperatorRestart
)

// stressJob is one reserved unit of asynchronous stress work.
type stressJob struct {
	event stressEvent
}

// reserveNextJob chooses a weighted event that still has budget and reserves
// the corresponding attempt counter before the worker starts.
func (r *stressRunner) reserveNextJob() (stressJob, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.hasSchedulableWorkLocked() {
		return stressJob{}, false
	}

	for {
		roll := r.rng.Intn(100)
		var evt stressEvent
		switch {
		case roll < 60:
			evt = stressEventPayment

		case roll < 72:
			evt = stressEventRound

		case roll < 82:
			evt = stressEventUnroll

		case roll < 88:
			evt = stressEventReorg

		case roll < 94:
			evt = stressEventClientRestart

		case roll < 99:
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
			r.paymentJobs++
			if r.cfg.paymentMinDelay > 0 {
				r.nextPaymentAt = time.Now().Add(
					r.cfg.paymentMinDelay,
				)
			}

		case stressEventRound:
			r.summary.RoundsTriggered++

		case stressEventUnroll:
			r.unrollJobs++

		case stressEventReorg:
			r.summary.ReorgsTriggered++
			r.nextReorgAt = time.Now().Add(r.cfg.reorgMinInterval)

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
		r.randomPayment()

	case stressEventRound:
		r.randomRefreshRound()

	case stressEventUnroll:
		r.randomUnroll()

	case stressEventReorg:
		r.randomReorg()

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

// eventBudgetRemainingLocked returns true if an event type has remaining
// budget. The caller must hold r.mu.
func (r *stressRunner) eventBudgetRemainingLocked(evt stressEvent) bool {
	switch evt {
	case stressEventPayment:
		return r.summary.PaymentsAttempted+r.paymentJobs <
			r.cfg.maxPayments

	case stressEventRound:
		return r.summary.RoundsTriggered < r.cfg.maxRounds

	case stressEventUnroll:
		return r.summary.UnrollsAttempted+r.unrollJobs <
			r.cfg.maxUnrolls

	case stressEventReorg:
		return r.summary.ReorgsTriggered < r.cfg.maxReorgs

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

// eventAllowedLocked returns true if an event type can be reserved now. The
// caller must hold r.mu.
func (r *stressRunner) eventAllowedLocked(evt stressEvent) bool {
	if !r.eventBudgetRemainingLocked(evt) {
		return false
	}

	if evt == stressEventPayment && r.cfg.paymentMinDelay > 0 &&
		!r.nextPaymentAt.IsZero() {

		return !time.Now().Before(r.nextPaymentAt)
	}

	if evt != stressEventReorg || r.cfg.reorgMinInterval == 0 {
		return true
	}

	return r.nextReorgAt.IsZero() || !time.Now().Before(r.nextReorgAt)
}

// schedulableRetryDelay returns a short wait before retrying future work.
func (r *stressRunner) schedulableRetryDelay() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	delay := 250 * time.Millisecond
	if r.eventBudgetRemainingLocked(stressEventPayment) &&
		!r.nextPaymentAt.IsZero() {

		if until := time.Until(r.nextPaymentAt); until > 0 &&
			until < delay {

			delay = until
		}
	}
	if !r.eventBudgetRemainingLocked(stressEventReorg) ||
		r.nextReorgAt.IsZero() {
		return delay
	}

	until := time.Until(r.nextReorgAt)
	if until > 0 && until < delay {
		return until
	}

	return delay
}

// randomPayment sends a random OOR amount from one funded client to another.
func (r *stressRunner) randomPayment() {
	reservation, stats, wait, polls, ok := r.waitPaymentReservation()
	if !ok {
		r.releasePaymentJob()
		r.paymentSkipped(stats, wait, polls)

		return
	}
	if polls > 1 {
		r.recordLiquidityWait(wait, false)
		waitMsg := "payment liquidity recovered wait=%s " +
			"polls=%d sender=%s"
		r.events.Printf("payment_liquidity_wait", map[string]any{
			"wait_ms": wait.Milliseconds(),
			"polls":   polls,
			"sender":  reservation.Sender,
			"amount":  reservation.Amount,
		},
			waitMsg, wait.Round(time.Millisecond), polls,
			reservation.Sender)
	}
	defer r.releasePaymentReservation(
		reservation.Sender, reservation.Outpoints,
	)

	paymentID, ok := r.reservePaymentAttempt()
	if !ok {
		r.events.Printf("payment_budget_exhausted", map[string]any{
			"wait_ms": wait.Milliseconds(),
			"polls":   polls,
			"sender":  reservation.Sender,
			"amount":  reservation.Amount,
		},
			"payment budget exhausted after liquidity wait=%s "+
				"polls=%d sender=%s",
			wait.Round(time.Millisecond), polls, reservation.Sender)

		return
	}

	sender := reservation.Sender
	receiver := r.randomReceiver(sender)
	defer r.invalidateLiveVTXOs(sender, receiver)

	senderRPC, err := r.clientRPC(sender)
	if err != nil {
		r.paymentFailed(paymentID, "sender rpc", err)

		return
	}
	receiverRPC, err := r.clientRPC(receiver)
	if err != nil {
		r.paymentFailed(paymentID, "receiver rpc", err)

		return
	}

	liveBalance, err := r.liveVTXOBalance(sender)
	if err != nil {
		r.paymentFailed(paymentID, "sender live vtxos", err)

		return
	}
	if liveBalance < reservation.Amount {
		err := fmt.Errorf("insufficient funds: %s has %d live sats, "+
			"need reserved amount %d", sender, liveBalance,
			reservation.Amount)
		r.paymentFailed(
			paymentID, "sender live vtxos", err,
		)

		return
	}

	r.events.Printf("payment", map[string]any{
		"id":                    paymentID,
		"sender":                sender,
		"receiver":              receiver,
		"amount":                reservation.Amount,
		"sender_reserved_vtxos": reservation.Outpoints,
		"sender_live_balance":   liveBalance,
		"sender_reserved_total": reservation.ReservedTotal,
	},
		"payment %d %s -> %s amount=%d", paymentID, sender, receiver,
		reservation.Amount)

	ctx, cancel := r.shortContext()
	defer cancel()

	var recv *daemonrpc.NewReceiveScriptResponse
	stressTraceRegion(ctx, "arktest.payment.receive_script", func() {
		recv, err = receiverRPC.NewReceiveScript(
			ctx, &daemonrpc.NewReceiveScriptRequest{
				Label: fmt.Sprintf("stress-%d", paymentID),
			},
		)
	})
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
	var resp *daemonrpc.SendOORResponse
	stressTraceRegion(ctx, "arktest.payment.send_oor", func() {
		resp, err = senderRPC.SendOOR(
			ctx, &daemonrpc.SendOORRequest{
				Recipient: &daemonrpc.Output{
					Destination: &daemonrpc.Output_Pubkey{
						Pubkey: pubkey,
					},
					AmountSat: reservation.Amount,
				},
			},
		)
	})
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
	},
		"payment %d settled latency=%s session=%s", paymentID,
		latency.Round(time.Millisecond), resp.SessionId)
}

// releasePaymentJob releases a scheduler-reserved payment slot without
// consuming one of the caller's requested payment attempts.
func (r *stressRunner) releasePaymentJob() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.paymentJobs > 0 {
		r.paymentJobs--
	}
}

// reservePaymentAttempt converts a scheduler payment slot into a real payment
// attempt after the runner has found an eligible sender.
func (r *stressRunner) reservePaymentAttempt() (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.paymentJobs > 0 {
		r.paymentJobs--
	}
	if r.summary.PaymentsAttempted >= r.cfg.maxPayments {
		return 0, false
	}

	r.summary.PaymentsAttempted++

	return r.summary.PaymentsAttempted, true
}

// paymentSkipped records a scheduler retry that could not find an eligible
// sender. Skips are not payment failures because no OOR attempt was made.
func (r *stressRunner) paymentSkipped(stats senderSelectionStats,
	wait time.Duration, polls int) {

	if polls > 1 {
		r.recordLiquidityWait(wait, true)
	}

	class := failureClassNoFundedSender
	id := r.recordPaymentSkipped(class)
	fields := stats.fields()
	fields["id"] = id
	fields["class"] = class
	fields["expected"] = true
	fields["wait_ms"] = wait.Milliseconds()
	fields["polls"] = polls
	r.events.Printf("payment_skip", fields,
		"payment skip %d: no funded sender after wait=%s "+
			"polls=%d\n	checked=%d rpc_failed=%d below_min=%d "+
			"candidates=%d\n	max_live=%d total_live=%d "+
			"reserved=%d max_available=%d total_available=%d "+
			"min_payment=%d\n	scan:\n%s",
		id, wait.Round(time.Millisecond), polls, stats.ClientsChecked,
		stats.RPCFailed, stats.BelowMin, stats.Candidates,
		stats.MaxLiveBalance, stats.TotalLiveBalance,
		stats.TotalReserved, stats.MaxAvailable, stats.TotalAvailable,
		stats.MinPayment,
		stats.scanBlock(stressSenderScanTerminalLimit))
}

// recordPaymentSkipped records a scheduler skip class and returns its stable
// skip sequence number.
func (r *stressRunner) recordPaymentSkipped(class stressFailureClass) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.summary.PaymentsSkipped++
	if r.summary.Skips == nil {
		r.summary.Skips = make(map[string]int)
	}
	r.summary.Skips[string(class)]++

	return r.summary.PaymentsSkipped
}

// recordLiquidityWait records time spent waiting for a payment sender to regain
// live, unreserved VTXO liquidity. This is scheduler backpressure, not OOR
// protocol latency.
func (r *stressRunner) recordLiquidityWait(wait time.Duration, timedOut bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.summary.LiquidityWaits++
	if timedOut {
		r.summary.LiquidityTimeouts++
	}
	r.liquidityWaits = append(r.liquidityWaits, wait)
}

// paymentFailed records a failed payment event and increments the summary.
func (r *stressRunner) paymentFailed(id int, phase string, err error) {
	class := r.classifyFailure(err)
	expected := r.failureExpected(class)
	r.incrementPaymentFailed(class, expected)
	r.events.Printf("payment_failed", map[string]any{
		"id":       id,
		"phase":    phase,
		"class":    class,
		"expected": expected,
		"error":    err.Error(),
	},
		"payment %d failed phase=%s err=%v", id, phase, err)
}

// incrementPaymentFailed increments the failed payment counter.
func (r *stressRunner) incrementPaymentFailed(class stressFailureClass,
	expected bool) {

	r.mu.Lock()
	defer r.mu.Unlock()

	r.summary.PaymentsFailed++
	r.recordWorkloadFailureLocked(class, expected)
}

// recordPaymentSettled records a successful payment latency.
func (r *stressRunner) recordPaymentSettled(latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.summary.PaymentsSettled++
	r.paymentLatencies = append(r.paymentLatencies, latency)
}

// randomUnroll triggers and waits for one random unilateral exit.
func (r *stressRunner) randomUnroll() {
	reservation, ok := r.randomUnrollReservation()
	if !ok {
		r.releaseUnrollJob()
		r.unrollSkipped()

		return
	}
	defer r.releaseUnrollReservation(
		reservation.Client, reservation.Outpoint,
	)
	defer r.invalidateLiveVTXOs(reservation.Client)

	unrollID, ok := r.reserveUnrollAttempt()
	if !ok {
		r.events.Printf("unroll_budget_exhausted", map[string]any{
			"client":   reservation.Client,
			"outpoint": reservation.Outpoint,
			"amount":   reservation.Amount,
		},
			"unroll budget exhausted after candidate client=%s "+
				"outpoint=%s",
			reservation.Client, reservation.Outpoint)

		return
	}

	clientRPC, err := r.clientRPC(reservation.Client)
	if err != nil {
		r.unrollFailed(unrollID, "client rpc", err)

		return
	}

	r.events.Printf("unroll", map[string]any{
		"id":             unrollID,
		"client":         reservation.Client,
		"outpoint":       reservation.Outpoint,
		"amount":         reservation.Amount,
		"chain_depth":    reservation.ChainDepth,
		"reserved_prior": reservation.ReservedPrior,
		"available":      reservation.Available,
	},
		"unroll %d client=%s outpoint=%s amount=%d chain_depth=%d",
		unrollID, reservation.Client, reservation.Outpoint,
		reservation.Amount, reservation.ChainDepth)

	start := time.Now()
	ctx, cancel := r.shortContext()
	var resp *daemonrpc.UnrollResponse
	stressTraceRegion(ctx, "arktest.unroll.start", func() {
		resp, err = clientRPC.Unroll(
			ctx, &daemonrpc.UnrollRequest{
				Outpoint: reservation.Outpoint,
			},
		)
	})
	cancel()
	if err != nil {
		r.unrollFailed(unrollID, "unroll rpc", err)

		return
	}
	if !resp.Created {
		err := fmt.Errorf("unroll already existed for %s",
			reservation.Outpoint)
		r.unrollFailed(unrollID, "unroll rpc", err)

		return
	}

	status, err := r.waitUnrollCompletion(
		unrollID, clientRPC, reservation.Outpoint,
	)
	if err != nil {
		r.unrollFailed(unrollID, "unroll completion", err)

		return
	}
	if status.SweepTxid == "" {
		err := fmt.Errorf("completed unroll %s missing sweep txid",
			reservation.Outpoint)
		r.unrollFailed(unrollID, "unroll status", err)

		return
	}

	latency := time.Since(start)
	r.recordUnrollCompleted(latency)
	r.events.Printf("unroll_completed", map[string]any{
		"id":         unrollID,
		"client":     reservation.Client,
		"outpoint":   reservation.Outpoint,
		"sweep_txid": status.SweepTxid,
		"latency_ms": latency.Milliseconds(),
	},
		"unroll %d completed client=%s outpoint=%s latency=%s "+
			"sweep_txid=%s",
		unrollID, reservation.Client, reservation.Outpoint,
		latency.Round(time.Millisecond), status.SweepTxid)
}

// releaseUnrollJob releases a scheduler-reserved unroll slot without consuming
// one of the caller's requested unroll attempts.
func (r *stressRunner) releaseUnrollJob() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.unrollJobs > 0 {
		r.unrollJobs--
	}
}

// reserveUnrollAttempt converts a scheduler slot into a real unroll attempt
// after the runner has found an eligible live VTXO.
func (r *stressRunner) reserveUnrollAttempt() (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.unrollJobs > 0 {
		r.unrollJobs--
	}
	if r.summary.UnrollsAttempted >= r.cfg.maxUnrolls {
		return 0, false
	}

	r.summary.UnrollsAttempted++

	return r.summary.UnrollsAttempted, true
}

// unrollSkipped records a scheduler retry that found no live unroll candidate.
func (r *stressRunner) unrollSkipped() {
	id := r.recordUnrollSkipped()
	r.events.Printf("unroll_skip", map[string]any{
		"id":       id,
		"class":    failureClassNoLiveVTXOs,
		"expected": true,
	},
		"unroll skip %d: no live unreserved vtxo", id)
}

// recordUnrollSkipped increments the unroll skip counter.
func (r *stressRunner) recordUnrollSkipped() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.summary.UnrollsSkipped++

	return r.summary.UnrollsSkipped
}

// unrollFailed records a failed unroll event and increments the summary.
func (r *stressRunner) unrollFailed(id int, phase string, err error) {
	class := r.classifyFailure(err)
	expected := r.failureExpected(class)
	r.incrementUnrollFailed(class, expected)
	r.events.Printf("unroll_failed", map[string]any{
		"id":       id,
		"phase":    phase,
		"class":    class,
		"expected": expected,
		"error":    err.Error(),
	},
		"unroll %d failed phase=%s err=%v", id, phase, err)
}

// incrementUnrollFailed increments the failed unroll counter.
func (r *stressRunner) incrementUnrollFailed(class stressFailureClass,
	expected bool) {

	r.mu.Lock()
	defer r.mu.Unlock()

	r.summary.UnrollsFailed++
	r.recordWorkloadFailureLocked(class, expected)
}

// recordUnrollCompleted records a successful unroll latency.
func (r *stressRunner) recordUnrollCompleted(latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.summary.UnrollsCompleted++
	r.unrollLatencies = append(r.unrollLatencies, latency)
}

// waitUnrollCompletion polls daemon job state until the unroll completes.
func (r *stressRunner) waitUnrollCompletion(id int,
	client daemonrpc.DaemonServiceClient, outpoint string) (
	*daemonrpc.GetUnrollStatusResponse, error) {

	completedStatus := daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED
	failedStatus := daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_FAILED

	ctx, cancel := r.contextWithTimeout(r.cfg.unrollTimeout)
	defer cancel()

	ticker := time.NewTicker(stressRoundPollInterval)
	defer ticker.Stop()

	var lastStatus daemonrpc.UnrollJobStatus
	var lastErr error
	loggedStatus := false
	loggedNotFound := false
	for {
		var resp *daemonrpc.GetUnrollStatusResponse
		var err error
		stressTraceRegion(ctx, "arktest.unroll.status", func() {
			resp, err = client.GetUnrollStatus(
				ctx, &daemonrpc.GetUnrollStatusRequest{
					Outpoint: outpoint,
				},
			)
		})
		switch {
		case err != nil:
			lastErr = err

		case resp != nil && resp.Found:
			statusText := resp.Status.String()
			if !loggedStatus || resp.Status != lastStatus {
				loggedStatus = true
				lastStatus = resp.Status
				r.events.Printf("unroll_status",
					map[string]any{
						"id":       id,
						"outpoint": outpoint,
						"status":   statusText,
					},
					"unroll %d status=%s outpoint=%s", id,
					resp.Status, outpoint)
			}

			switch resp.Status {
			case completedStatus:
				return resp, nil

			case failedStatus:
				return nil, fmt.Errorf("unroll job failed: %s",
					resp.LastError)
			}

		case resp != nil && !resp.Found && !loggedNotFound:
			loggedNotFound = true
			r.events.Printf("unroll_not_found", map[string]any{
				"id":       id,
				"outpoint": outpoint,
			},
				"unroll %d not yet found outpoint=%s", id,
				outpoint)
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("unroll deadline exceeded "+
				"after %s outpoint=%s last_status=%s "+
				"last_err=%v: %w", r.cfg.unrollTimeout,
				outpoint, lastStatus, lastErr, ctx.Err())

		case <-ticker.C:
		}
	}
}

// unrollReservation records one runner-side VTXO reserved for unroll.
type unrollReservation struct {
	Client        string
	Outpoint      string
	Amount        int64
	ChainDepth    uint32
	LiveBalance   int64
	Available     int64
	ReservedPrior int64
}

// randomUnrollReservation chooses a client and reserves one live VTXO.
func (r *stressRunner) randomUnrollReservation() (unrollReservation, bool) {
	for _, name := range r.shuffledClientNames() {
		vtxos, err := r.liveVTXOs(name)
		if err != nil {
			class := r.classifyFailure(err)
			expected := r.failureExpected(class)
			r.recordUnexpectedProbeFailure(class, expected)
			probeMsg := "unroll probe failed client=%s err=%v"
			r.events.Printf("unroll_probe_failed", map[string]any{
				"client":   name,
				"class":    class,
				"expected": expected,
				"error":    err.Error(),
			},
				probeMsg, name, err)

			continue
		}

		reservation, ok := r.reserveUnrollVTXO(name, vtxos)
		if ok {
			return reservation, true
		}
	}

	return unrollReservation{}, false
}

// reserveUnrollVTXO reserves one live VTXO, preferring OOR-derived VTXOs.
func (r *stressRunner) reserveUnrollVTXO(name string, vtxos []*daemonrpc.VTXO) (
	unrollReservation, bool) {

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.unrollReserved == nil {
		r.unrollReserved = make(map[string]map[string]int64)
	}

	liveBalance := sumVTXOs(vtxos)
	reserved := r.totalReservedVTXOsLocked(name)
	availableVTXOs := make([]*daemonrpc.VTXO, 0, len(vtxos))
	for _, vtxo := range vtxos {
		if vtxo == nil || vtxo.Outpoint == "" {
			continue
		}
		if r.outpointReservedLocked(name, vtxo.Outpoint) {
			continue
		}
		availableVTXOs = append(availableVTXOs, vtxo)
	}
	available := sumVTXOs(availableVTXOs)

	reservation := unrollReservation{
		Client:        name,
		LiveBalance:   liveBalance,
		Available:     available,
		ReservedPrior: reserved,
	}
	if len(availableVTXOs) == 0 {
		return reservation, false
	}

	preferred := make([]*daemonrpc.VTXO, 0, len(availableVTXOs))
	for _, vtxo := range availableVTXOs {
		if vtxo.ChainDepth > 0 {
			preferred = append(preferred, vtxo)
		}
	}
	candidates := availableVTXOs
	if len(preferred) > 0 {
		candidates = preferred
	}
	target := candidates[r.rng.Intn(len(candidates))]

	if r.unrollReserved[name] == nil {
		r.unrollReserved[name] = make(map[string]int64)
	}
	r.unrollReserved[name][target.Outpoint] = target.AmountSat

	reservation.Outpoint = target.Outpoint
	reservation.Amount = target.AmountSat
	reservation.ChainDepth = target.ChainDepth

	return reservation, true
}

// releaseUnrollReservation releases a runner-side unroll reservation.
func (r *stressRunner) releaseUnrollReservation(name, outpoint string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.unrollReserved[name], outpoint)
	if len(r.unrollReserved[name]) == 0 {
		delete(r.unrollReserved, name)
	}
}

// senderSelectionClient records one client's sender-selection scan result.
type senderSelectionClient struct {
	Name        string             `json:"name"`
	Status      string             `json:"status"`
	LiveBalance int64              `json:"live_balance_sat"`
	LiveVTXOs   int                `json:"live_vtxos"`
	Reserved    int64              `json:"reserved_sat"`
	Available   int64              `json:"available_sat"`
	Amount      int64              `json:"reserved_amount_sat"`
	Class       stressFailureClass `json:"class,omitempty"`
	Expected    bool               `json:"expected"`
	Error       string             `json:"error,omitempty"`
}

// senderSelectionStats summarizes why payment sender selection succeeded or
// failed.
type senderSelectionStats struct {
	ClientsChecked   int                     `json:"clients_checked"`
	RPCFailed        int                     `json:"rpc_failed"`
	BelowMin         int                     `json:"below_min"`
	Candidates       int                     `json:"candidates"`
	MaxLiveBalance   int64                   `json:"max_live_balance_sat"`
	TotalLiveBalance int64                   `json:"total_live_balance_sat"`
	MaxAvailable     int64                   `json:"max_available_sat"`
	TotalAvailable   int64                   `json:"total_available_sat"`
	TotalReserved    int64                   `json:"total_reserved_sat"`
	MinPayment       int64                   `json:"min_payment_sat"`
	Clients          []senderSelectionClient `json:"clients"`
}

// fields returns a structured field map for event logging.
func (s senderSelectionStats) fields() map[string]any {
	return map[string]any{
		"clients_checked":        s.ClientsChecked,
		"rpc_failed":             s.RPCFailed,
		"below_min":              s.BelowMin,
		"candidates":             s.Candidates,
		"max_live_balance_sat":   s.MaxLiveBalance,
		"total_live_balance_sat": s.TotalLiveBalance,
		"max_available_sat":      s.MaxAvailable,
		"total_available_sat":    s.TotalAvailable,
		"total_reserved_sat":     s.TotalReserved,
		"min_payment_sat":        s.MinPayment,
		"client_scan":            s.scanSummary(0),
		"clients":                s.Clients,
	}
}

// scanSummary returns a compact per-client sender scan for terminal output.
func (s senderSelectionStats) scanSummary(limit int) string {
	if len(s.Clients) == 0 {
		return "none"
	}
	if limit <= 0 || limit > len(s.Clients) {
		limit = len(s.Clients)
	}

	parts := make([]string, 0, limit+1)
	for _, client := range s.Clients[:limit] {
		switch client.Status {
		case "rpc_failed":
			parts = append(
				parts, fmt.Sprintf("%s:rpc_failed/%s",
					client.Name, client.Class),
			)

		default:
			parts = append(
				parts, fmt.Sprintf("%s:%s/live=%d/reserved=%"+
					"d/available=%d/vtxos=%d", client.Name,
					client.Status, client.LiveBalance,
					client.Reserved, client.Available,
					client.LiveVTXOs),
			)
		}
	}
	if remaining := len(s.Clients) - limit; remaining > 0 {
		parts = append(parts, fmt.Sprintf("+%d_more", remaining))
	}

	return strings.Join(parts, ",")
}

// scanBlock returns a readable multi-line scan for terminal output.
func (s senderSelectionStats) scanBlock(limit int) string {
	if len(s.Clients) == 0 {
		return "\t\tnone"
	}
	if limit <= 0 || limit > len(s.Clients) {
		limit = len(s.Clients)
	}

	lines := make([]string, 0, limit+1)
	for _, client := range s.Clients[:limit] {
		switch client.Status {
		case "rpc_failed":
			lines = append(
				lines, fmt.Sprintf("		%s "+
					"status=rpc_failed class=%s "+
					"expected=%v", client.Name,
					client.Class, client.Expected),
			)

		default:
			lines = append(
				lines, fmt.Sprintf("		%s status=%s "+
					"live=%d reserved=%d available=%d "+
					"vtxos=%d", client.Name, client.Status,
					client.LiveBalance, client.Reserved,
					client.Available, client.LiveVTXOs),
			)
		}
	}
	if remaining := len(s.Clients) - limit; remaining > 0 {
		lines = append(
			lines, fmt.Sprintf("		+%d more (see "+
				"events.jsonl)", remaining),
		)
	}

	return strings.Join(lines, "\n")
}

// paymentReservation records runner-side VTXOs reserved for one payment.
type paymentReservation struct {
	Sender        string
	Amount        int64
	Outpoints     []string
	LiveBalance   int64
	Available     int64
	ReservedPrior int64
	ReservedTotal int64
}

// waitPaymentReservation waits for live, unreserved sender liquidity before
// giving up on one scheduler payment slot. A skip after this wait means
// liquidity stayed unavailable, while a recovered wait means the worker applied
// backpressure until change/incoming VTXOs became selectable again.
func (r *stressRunner) waitPaymentReservation() (paymentReservation,
	senderSelectionStats, time.Duration, int, bool) {

	started := time.Now()
	deadline := started.Add(r.cfg.liquidityTimeout)
	if !r.workloadDeadline.IsZero() && r.workloadDeadline.Before(deadline) {
		deadline = r.workloadDeadline
	}

	var stats senderSelectionStats
	polls := 0
	for {
		polls++
		reservation, latest, ok := r.randomPaymentReservation()
		stats = latest
		if ok {
			return reservation, stats, time.Since(started),
				polls, true
		}

		if r.cfg.liquidityTimeout == 0 || !time.Now().Before(deadline) {
			return paymentReservation{}, stats, time.Since(started),
				polls, false
		}

		sleep := stressPaymentLiquidityPollInterval
		if remaining := time.Until(deadline); remaining < sleep {
			sleep = remaining
		}
		if sleep <= 0 {
			return paymentReservation{}, stats, time.Since(started),
				polls, false
		}

		time.Sleep(sleep)
	}
}

// randomPaymentReservation chooses a sender and reserves whole VTXOs. The
// daemon selector reserves VTXOs, not partial amounts, so the runner mirrors
// that unit to avoid queuing impossible same-client payments.
func (r *stressRunner) randomPaymentReservation() (paymentReservation,
	senderSelectionStats, bool) {

	names := r.shuffledClientNames()
	stats := senderSelectionStats{
		MinPayment: r.cfg.minPayment,
		Clients:    make([]senderSelectionClient, 0, len(names)),
	}
	for _, name := range names {
		stats.ClientsChecked++

		vtxos, err := r.liveVTXOs(name)
		if err != nil {
			class := r.classifyFailure(err)
			expected := r.failureExpected(class)
			stats.RPCFailed++
			client := senderSelectionClient{
				Name:     name,
				Status:   "rpc_failed",
				Class:    class,
				Expected: expected,
				Error:    err.Error(),
			}
			stats.Clients = append(stats.Clients, client)
			r.recordUnexpectedProbeFailure(class, expected)
			r.events.Printf("balance_failed", map[string]any{
				"client":   name,
				"class":    class,
				"expected": expected,
				"error":    err.Error(),
			},
				"live vtxo balance failed client=%s err=%v",
				name, err)

			continue
		}

		liveBalance := sumVTXOs(vtxos)
		liveCount := len(vtxos)
		stats.TotalLiveBalance += liveBalance
		if liveBalance > stats.MaxLiveBalance {
			stats.MaxLiveBalance = liveBalance
		}

		reservation, ok := r.reservePaymentVTXOs(name, vtxos)
		stats.TotalReserved += reservation.ReservedPrior
		stats.TotalAvailable += reservation.Available
		if reservation.Available > stats.MaxAvailable {
			stats.MaxAvailable = reservation.Available
		}
		if ok {
			stats.Candidates++
			client := senderSelectionClient{
				Name:        name,
				Status:      "candidate",
				LiveBalance: liveBalance,
				LiveVTXOs:   liveCount,
				Reserved:    reservation.ReservedPrior,
				Available:   reservation.Available,
				Amount:      reservation.Amount,
				Expected:    true,
			}
			stats.Clients = append(stats.Clients, client)

			return reservation, stats, true
		}

		stats.BelowMin++
		client := senderSelectionClient{
			Name:        name,
			Status:      "below_min",
			LiveBalance: liveBalance,
			LiveVTXOs:   liveCount,
			Reserved:    reservation.ReservedPrior,
			Available:   reservation.Available,
			Expected:    true,
		}
		stats.Clients = append(stats.Clients, client)
	}

	return paymentReservation{}, stats, false
}

// reservePaymentVTXOs reserves whole live VTXOs against a sender's latest
// observed set. It prevents arktest workers from overbooking the same VTXO
// while still leaving daemon-side VTXO selection as the source of truth.
func (r *stressRunner) reservePaymentVTXOs(name string,
	vtxos []*daemonrpc.VTXO) (paymentReservation, bool) {

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.paymentReserved == nil {
		r.paymentReserved = make(map[string]map[string]int64)
	}

	reserved := r.totalReservedVTXOsLocked(name)
	liveBalance := sumVTXOs(vtxos)
	availableVTXOs := make([]*daemonrpc.VTXO, 0, len(vtxos))
	for _, vtxo := range vtxos {
		if vtxo == nil || vtxo.Outpoint == "" {
			continue
		}
		if r.outpointReservedLocked(name, vtxo.Outpoint) {
			continue
		}
		availableVTXOs = append(availableVTXOs, vtxo)
	}
	available := sumVTXOs(availableVTXOs)

	reservation := paymentReservation{
		Sender:        name,
		LiveBalance:   liveBalance,
		Available:     available,
		ReservedPrior: reserved,
		ReservedTotal: reserved,
	}
	if available < r.cfg.minPayment {
		return reservation, false
	}

	maxAmount := minInt64(r.cfg.maxPayment, available)
	amount := r.cfg.minPayment
	if maxAmount > r.cfg.minPayment {
		amount += r.rng.Int63n(maxAmount - r.cfg.minPayment + 1)
	}

	if r.paymentReserved[name] == nil {
		r.paymentReserved[name] = make(map[string]int64)
	}

	var selectedBalance int64
	for _, vtxo := range availableVTXOs {
		r.paymentReserved[name][vtxo.Outpoint] = vtxo.AmountSat
		reservation.Outpoints = append(
			reservation.Outpoints, vtxo.Outpoint,
		)
		selectedBalance += vtxo.AmountSat
		if selectedBalance >= amount {
			break
		}
	}

	reservation.Amount = amount
	reservation.ReservedTotal = sumReservedVTXOs(r.paymentReserved[name])

	return reservation, true
}

// releasePaymentReservation releases a runner-side payment reservation.
func (r *stressRunner) releasePaymentReservation(name string,
	outpoints []string) {

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, outpoint := range outpoints {
		delete(r.paymentReserved[name], outpoint)
	}
	if len(r.paymentReserved[name]) == 0 {
		delete(r.paymentReserved, name)
	}
}

// outpointReservedLocked reports whether any stress worker reserved an
// outpoint. The caller must hold r.mu.
func (r *stressRunner) outpointReservedLocked(name, outpoint string) bool {
	if _, ok := r.paymentReserved[name][outpoint]; ok {
		return true
	}
	if _, ok := r.unrollReserved[name][outpoint]; ok {
		return true
	}

	return false
}

// totalReservedVTXOsLocked returns all runner-side reservations for a client.
// The caller must hold r.mu.
func (r *stressRunner) totalReservedVTXOsLocked(name string) int64 {
	return sumReservedVTXOs(r.paymentReserved[name]) +
		sumReservedVTXOs(r.unrollReserved[name])
}

// sumVTXOs returns the total amount of the supplied VTXOs.
func sumVTXOs(vtxos []*daemonrpc.VTXO) int64 {
	var sum int64
	for _, vtxo := range vtxos {
		if vtxo == nil {
			continue
		}
		sum += vtxo.AmountSat
	}

	return sum
}

// sumReservedVTXOs returns the total amount of runner-reserved VTXOs.
func sumReservedVTXOs(vtxos map[string]int64) int64 {
	var sum int64
	for _, amount := range vtxos {
		sum += amount
	}

	return sum
}

// recordUnexpectedProbeFailure records unexpected sender-selection probe
// failures without incrementing payment failure counters. Expected probe
// failures remain event detail and are summarized by the final payment skip.
func (r *stressRunner) recordUnexpectedProbeFailure(class stressFailureClass,
	expected bool) {

	if expected {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.recordWorkloadFailureLocked(class, expected)
}

// classifyFailure maps an error into a stable stress failure class.
func (r *stressRunner) classifyFailure(err error) stressFailureClass {
	if err == nil {
		return failureClassUnexpected
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "client daemon unavailable"):
		return failureClassClientUnavailable

	case strings.Contains(msg, "connection is closing") ||
		strings.Contains(msg, "code = canceled"):
		return failureClassConnectionClosing

	case strings.Contains(msg, "connection refused"):
		return failureClassConnectionRefused

	case strings.Contains(msg, "graceful_stop") ||
		strings.Contains(msg, "error reading from server: eof"):
		return failureClassGracefulStop

	case strings.Contains(msg, "deadlineexceeded") ||
		strings.Contains(msg, "deadline exceeded"):
		return failureClassDeadlineExceeded

	case strings.Contains(msg, "below dust"):
		return failureClassDustChange

	case strings.Contains(msg, "insufficient funds") ||
		strings.Contains(msg, "insufficient wallet utxos") ||
		strings.Contains(msg, "no confirmed wallet utxos") ||
		(strings.Contains(msg, "has ") &&
			strings.Contains(msg, "sats") &&
			strings.Contains(msg, "need at least")):
		return failureClassInsufficientFunds

	case strings.Contains(msg, "no live vtxos"):
		return failureClassNoLiveVTXOs

	case strings.Contains(msg, "no registered clients"):
		return failureClassNoRegistered

	case strings.Contains(msg, "timed out waiting"):
		return failureClassRoundTimeout

	case strings.Contains(msg, "failed round"):
		return failureClassFailedRound

	case strings.Contains(msg, "cannot accept pending forfeit"):
		return failureClassPendingForfeit

	case strings.Contains(msg, "reorg"):
		return failureClassReorgFailed

	default:
		return failureClassUnexpected
	}
}

// failureExpected reports whether a failure class is expected for this stress
// profile.
func (r *stressRunner) failureExpected(class stressFailureClass) bool {
	switch class {
	case failureClassDustChange, failureClassInsufficientFunds,
		failureClassNoFundedSender, failureClassNoLiveVTXOs,
		failureClassNoRegistered, failureClassPendingForfeit:
		return true

	case failureClassClientUnavailable, failureClassConnectionClosing,
		failureClassConnectionRefused, failureClassGracefulStop:
		return r.lifecycleDisruptionsEnabled()

	case failureClassDeadlineExceeded:
		return r.reorgDisruptionsEnabled() ||
			r.operatorDisruptionsEnabled() ||
			r.unrollDisruptionsEnabled()

	case failureClassRoundTimeout, failureClassFailedRound:
		operatorDisruption := r.operatorDisruptionsEnabled()

		return operatorDisruption || r.reorgDisruptionsEnabled()

	case failureClassReorgFailed:
		return r.reorgDisruptionsEnabled()

	default:
		return false
	}
}

// lifecycleDisruptionsEnabled returns true when this profile can intentionally
// tear down client/operator RPC connections during workload execution.
func (r *stressRunner) lifecycleDisruptionsEnabled() bool {
	if r.cfg.maxRestarts <= 0 {
		return false
	}

	return r.cfg.clientRestarts || r.cfg.clientCrashes ||
		r.cfg.operatorRestarts
}

// operatorDisruptionsEnabled returns true when an in-flight round can be
// interrupted by an intentional operator restart.
func (r *stressRunner) operatorDisruptionsEnabled() bool {
	return r.cfg.maxRestarts > 0 && r.cfg.operatorRestarts
}

// unrollDisruptionsEnabled returns true when this profile can intentionally
// run long-lived unroll actors that wait for chain progress.
func (r *stressRunner) unrollDisruptionsEnabled() bool {
	return r.cfg.maxUnrolls > 0
}

// reorgDisruptionsEnabled returns true when this profile can intentionally
// invalidate recently mined blocks while workload RPCs are in flight.
func (r *stressRunner) reorgDisruptionsEnabled() bool {
	return r.cfg.maxReorgs > 0
}

// recordWorkloadFailureLocked records one expected or unexpected workload
// failure. The caller must hold r.mu.
func (r *stressRunner) recordWorkloadFailureLocked(class stressFailureClass,
	expected bool) {

	if r.summary.FailureClasses == nil {
		r.summary.FailureClasses = make(map[string]int)
	}
	r.summary.FailureClasses[string(class)]++

	if expected {
		r.summary.ExpectedFailures++
	} else {
		r.summary.UnexpectedFailures++
	}
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
func (r *stressRunner) clientBalance(name string) (
	*daemonrpc.GetBalanceResponse, error) {

	ctx, cancel := r.shortContext()
	defer cancel()

	clientRPC, err := r.clientRPC(name)
	if err != nil {
		return nil, err
	}

	return clientRPC.GetBalance(
		ctx, &daemonrpc.GetBalanceRequest{},
	)
}

// liveVTXOs returns the client's currently spendable VTXOs. Stress sender
// selection can ask every client for live VTXOs many times per second, so this
// path uses a very short in-process cache to keep the harness from becoming the
// dominant daemon load.
func (r *stressRunner) liveVTXOs(name string) ([]*daemonrpc.VTXO, error) {
	if vtxos, ok := r.cachedLiveVTXOs(name); ok {
		return vtxos, nil
	}

	return r.refreshLiveVTXOs(name)
}

// cachedLiveVTXOs returns a live VTXO snapshot if the short stress-runner cache
// still considers it fresh.
func (r *stressRunner) cachedLiveVTXOs(name string) ([]*daemonrpc.VTXO, bool) {
	r.liveVTXOMu.Lock()
	defer r.liveVTXOMu.Unlock()

	entry, ok := r.liveVTXOCache[name]
	if !ok {
		return nil, false
	}
	if time.Since(entry.fetchedAt) > stressLiveVTXOCacheTTL {
		delete(r.liveVTXOCache, name)

		return nil, false
	}

	return cloneVTXOs(entry.vtxos), true
}

// refreshLiveVTXOs fetches the client's currently spendable VTXOs from the
// daemon and stores the short-lived snapshot for later sender scans.
func (r *stressRunner) refreshLiveVTXOs(name string) ([]*daemonrpc.VTXO,
	error) {

	ctx, cancel := r.shortContext()
	defer cancel()

	clientRPC, err := r.clientRPC(name)
	if err != nil {
		return nil, err
	}
	resp, err := clientRPC.ListVTXOs(
		ctx, &daemonrpc.ListVTXOsRequest{
			StatusFilter: daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
		},
	)
	if err != nil {
		return nil, err
	}

	cached := cloneVTXOs(resp.Vtxos)
	r.liveVTXOMu.Lock()
	if r.liveVTXOCache == nil {
		r.liveVTXOCache = make(map[string]liveVTXOCacheEntry)
	}
	r.liveVTXOCache[name] = liveVTXOCacheEntry{
		fetchedAt: time.Now(),
		vtxos:     cached,
	}
	r.liveVTXOMu.Unlock()

	return cloneVTXOs(cached), nil
}

// invalidateLiveVTXOs drops cached VTXO snapshots for the named clients.
func (r *stressRunner) invalidateLiveVTXOs(names ...string) {
	r.liveVTXOMu.Lock()
	defer r.liveVTXOMu.Unlock()

	for _, name := range names {
		delete(r.liveVTXOCache, name)
	}
}

// cloneVTXOs copies a VTXO slice so callers cannot mutate cached slice
// structure.
func cloneVTXOs(vtxos []*daemonrpc.VTXO) []*daemonrpc.VTXO {
	if len(vtxos) == 0 {
		return nil
	}

	return append([]*daemonrpc.VTXO(nil), vtxos...)
}

// liveVTXOBalance returns the sum of the client's currently spendable VTXOs.
func (r *stressRunner) liveVTXOBalance(name string) (int64, error) {
	balance, _, err := r.liveVTXOStats(name)

	return balance, err
}

// liveVTXOStats returns the sum and count of the client's spendable VTXOs.
func (r *stressRunner) liveVTXOStats(name string) (int64, int, error) {
	vtxos, err := r.liveVTXOs(name)
	if err != nil {
		return 0, 0, err
	}

	var balance int64
	for _, vtxo := range vtxos {
		balance += vtxo.AmountSat
	}

	return balance, len(vtxos), nil
}

// liveVTXOOutpoints returns the client's currently spendable VTXO outpoints.
func (r *stressRunner) liveVTXOOutpoints(name string) ([]string, error) {
	vtxos, err := r.refreshLiveVTXOs(name)
	if err != nil {
		return nil, err
	}

	outpoints := make([]string, 0, len(vtxos))
	for _, vtxo := range vtxos {
		if vtxo == nil || vtxo.Outpoint == "" {
			continue
		}
		if r.outpointReserved(name, vtxo.Outpoint) {
			continue
		}
		outpoints = append(outpoints, vtxo.Outpoint)
	}

	return outpoints, nil
}

// outpointReserved reports whether any stress worker currently owns an
// outpoint reservation.
func (r *stressRunner) outpointReserved(name, outpoint string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.outpointReservedLocked(name, outpoint)
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

// randomRefreshRound queues a refresh for a random client and confirms a round.
func (r *stressRunner) randomRefreshRound() {
	name := r.names[r.randIntn(len(r.names))]
	r.roundMu.Lock()
	defer r.roundMu.Unlock()

	r.events.Printf("round", map[string]any{
		"client": name,
	},
		"refresh round requested client=%s", name)

	ctx, cancel := r.shortContext()
	defer cancel()

	clientRPC, err := r.clientRPC(name)
	if err != nil {
		r.recordRoundFailedf("round_failed", err, map[string]any{
			"client": name,
		}, "refresh round failed client=%s err=%v", name, err)

		return
	}
	outpoints, err := r.liveVTXOOutpoints(name)
	if err != nil {
		r.recordRoundFailedf("round_failed", err, map[string]any{
			"client": name,
		}, "list live vtxos failed client=%s err=%v", name, err)

		return
	}
	if len(outpoints) == 0 {
		err := fmt.Errorf("refresh round skipped client=%s no "+
			"live vtxos", name)
		r.recordRoundFailedf("round_failed", err, map[string]any{
			"client": name,
		}, "refresh round skipped for client %s: no live vtxos",
			name)

		return
	}

	_, err = clientRPC.RefreshVTXOs(
		ctx, &daemonrpc.RefreshVTXOsRequest{
			Selection: &daemonrpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: outpoints,
				},
			},
		},
	)
	if err != nil {
		r.recordRoundFailedf("round_failed", err, map[string]any{
			"client": name,
		}, "refresh round failed client=%s err=%v", name, err)

		return
	}
	r.invalidateLiveVTXOs(name)

	if err := r.waitClientRoundAtLeast(
		name, daemonrpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY,
		stressRoundWaitTimeout,
	); err != nil {

		r.recordRoundFailedf("round_failed", err, map[string]any{
			"client": name,
		}, "refresh pending wait failed client=%s err=%v",
			name, err)

		return
	}

	unlockClient := r.lockClients(name)
	client := r.getClient(name)
	if client == nil {
		unlockClient()
		err := fmt.Errorf("client %s daemon unavailable", name)
		r.recordRoundFailedf("round_failed", err, map[string]any{
			"client": name,
		}, "trigger registration failed client=%s unavailable", name)

		return
	}
	client.TriggerRoundRegistration()
	unlockClient()
	if err := r.waitClientRoundAtLeast(
		name, daemonrpc.RoundState_ROUND_STATE_REGISTRATION_SENT,
		stressRoundWaitTimeout,
	); err != nil {

		r.recordRoundFailedf("round_failed", err, map[string]any{
			"client": name,
		}, "refresh registration wait failed client=%s err=%v",
			name, err)

		return
	}
	time.Sleep(stressRegistrationSettleDelay)

	resp, err := r.h.ArkAdminClient.TriggerBatch(
		ctx, &adminrpc.TriggerBatchRequest{},
	)
	if err != nil {
		r.recordRoundFailedf("round_failed", err, map[string]any{
			"client": name,
		}, "trigger batch failed client=%s err=%v", name, err)

		return
	}

	if err := r.confirmRound(resp.RoundId); err != nil {
		r.recordRoundFailedf("round_failed", err, map[string]any{
			"client": name,
			"round":  resp.RoundId,
		}, "refresh round confirmation failed client=%s round=%s "+
			"err=%v", name, resp.RoundId, err)

		return
	}

	r.recordRoundConfirmed()
	r.events.Printf("round_confirmed", map[string]any{
		"client": name,
		"round":  resp.RoundId,
	},
		"refresh round confirmed client=%s round=%s", name,
		resp.RoundId)
}

// randomReorg injects a deterministic chain reorg while workload workers run.
func (r *stressRunner) randomReorg() {
	start := time.Now()
	r.events.Printf("reorg", map[string]any{
		"depth":      r.cfg.reorgDepth,
		"new_blocks": r.cfg.reorgNewBlocks,
	},
		"reorg requested depth=%d new_blocks=%d", r.cfg.reorgDepth,
		r.cfg.reorgNewBlocks)

	ctx, cancel := r.contextWithTimeout(stressRoundWaitTimeout)
	defer cancel()

	result, err := r.applyStressReorgWithWorkloadPaused(ctx)
	if err == nil {
		targetHeight := uint32(reorgTip(result).Height)
		err = r.waitReorgConvergence(ctx, targetHeight)
	}
	if err != nil {
		r.recordReorgFailedf("reorg_failed", err, map[string]any{
			"depth":      r.cfg.reorgDepth,
			"new_blocks": r.cfg.reorgNewBlocks,
		}, "reorg failed depth=%d new_blocks=%d err=%v",
			r.cfg.reorgDepth, r.cfg.reorgNewBlocks, err)

		return
	}

	r.recordReorgCompleted()
	fields := reorgResultFields(result)
	fields["latency_ms"] = time.Since(start).Milliseconds()
	r.events.Printf("reorg_confirmed", fields,
		"reorg confirmed depth=%d old_tip=%d:%s new_tip=%d:%s "+
			"latency=%s",
		len(result.Disconnected), result.OldTip.Height,
		result.OldTip.Hash, reorgTip(result).Height,
		reorgTip(result).Hash,
		time.Since(start).Round(time.Millisecond))
}

// applyStressReorgWithWorkloadPaused applies the bitcoind chain mutation while
// operations that assume stable client/operator state are paused.
func (r *stressRunner) applyStressReorgWithWorkloadPaused(ctx context.Context) (
	stressReorgResult, error) {

	r.operatorMu.Lock()
	defer r.operatorMu.Unlock()
	r.roundMu.Lock()
	defer r.roundMu.Unlock()
	r.chainMu.Lock()
	defer r.chainMu.Unlock()
	unlockClients := r.lockAllClients()
	defer unlockClients()

	return r.applyStressReorg(ctx)
}

// applyStressReorg invalidates recent active-chain blocks and mines a longer
// replacement branch. The caller is responsible for waiting for services to
// converge on the new tip.
func (r *stressRunner) applyStressReorg(ctx context.Context) (stressReorgResult,
	error) {

	depth := r.cfg.reorgDepth
	newBlocks := r.cfg.reorgNewBlocks
	oldTip, err := r.bitcoindBestBlockHeader(ctx)
	if err != nil {
		return stressReorgResult{}, fmt.Errorf("reorg old tip: %w", err)
	}
	if oldTip.Height < int64(depth) {
		return stressReorgResult{}, fmt.Errorf("reorg depth %d "+
			"exceeds height %d", depth, oldTip.Height)
	}

	forkHeight := oldTip.Height - int64(depth)
	forkPoint, err := r.bitcoindBlockHeaderByHeight(ctx, forkHeight)
	if err != nil {
		return stressReorgResult{}, fmt.Errorf("reorg fork point: %w",
			err)
	}

	disconnected := make([]clientharness.BlockHeader, 0, depth)
	for height := forkHeight + 1; height <= oldTip.Height; height++ {
		header, err := r.bitcoindBlockHeaderByHeight(ctx, height)
		if err != nil {
			return stressReorgResult{}, fmt.Errorf("reorg "+
				"disconnected height %d: %w", height, err)
		}

		disconnected = append(disconnected, header)
	}

	invalidateHash := disconnected[0].Hash
	if err := callBitcoindRPC(
		ctx, r.state, "invalidateblock", []any{invalidateHash}, nil,
	); err != nil {
		return stressReorgResult{}, fmt.Errorf("reorg invalidate "+
			"%s: %w", invalidateHash, err)
	}
	if err := r.waitBitcoindHeight(ctx, forkHeight); err != nil {
		return stressReorgResult{}, r.recoverInvalidatedBlock(
			invalidateHash, oldTip.Height, err,
		)
	}

	connected, err := r.bitcoindGenerateHeaders(ctx, newBlocks)
	if err != nil {
		return stressReorgResult{}, r.recoverInvalidatedBlock(
			invalidateHash, oldTip.Height,
			fmt.Errorf("reorg mine: %w", err),
		)
	}
	newTip := connected[len(connected)-1]
	if newTip.Hash == oldTip.Hash {
		return stressReorgResult{}, fmt.Errorf("reorg did not replace "+
			"old tip %s", oldTip.Hash)
	}

	result := stressReorgResult{
		OldTip:       oldTip,
		ForkPoint:    forkPoint,
		Disconnected: disconnected,
		Connected:    connected,
	}

	return result, nil
}

// recoverInvalidatedBlock attempts to restore the original chain if a reorg
// fails after invalidateblock has succeeded.
func (r *stressRunner) recoverInvalidatedBlock(invalidateHash string,
	oldHeight int64, cause error) error {

	recoveryCtx, cancel := context.WithTimeout(
		context.Background(), stressReorgRecoveryTimeout,
	)
	defer cancel()

	err := callBitcoindRPC(
		recoveryCtx, r.state, "reconsiderblock", []any{invalidateHash},
		nil,
	)
	if err != nil {
		return fmt.Errorf("%w; reorg recovery reconsider %s failed: %w",
			cause, invalidateHash, err)
	}
	if err := r.waitBitcoindHeight(recoveryCtx, oldHeight); err != nil {
		return fmt.Errorf("%w; reorg recovery wait old height %d "+
			"failed: %w", cause, oldHeight, err)
	}

	return cause
}

// bitcoindBestBlockHeader returns the active chain tip header.
func (r *stressRunner) bitcoindBestBlockHeader(ctx context.Context) (
	clientharness.BlockHeader, error) {

	var height uint32
	if err := callBitcoindRPC(
		ctx, r.state, "getblockcount", nil, &height,
	); err != nil {
		return clientharness.BlockHeader{}, err
	}

	return r.bitcoindBlockHeaderByHeight(ctx, int64(height))
}

// bitcoindBlockHeaderByHeight returns the verbose header for a block height.
func (r *stressRunner) bitcoindBlockHeaderByHeight(ctx context.Context,
	height int64) (clientharness.BlockHeader, error) {

	var hash string
	if err := callBitcoindRPC(
		ctx, r.state, "getblockhash", []any{height}, &hash,
	); err != nil {
		return clientharness.BlockHeader{}, err
	}

	return r.bitcoindBlockHeader(ctx, hash)
}

// bitcoindBlockHeader returns the verbose header for a block hash.
func (r *stressRunner) bitcoindBlockHeader(ctx context.Context, hash string) (
	clientharness.BlockHeader, error) {

	var header clientharness.BlockHeader
	if err := callBitcoindRPC(
		ctx, r.state, "getblockheader", []any{hash, true}, &header,
	); err != nil {
		return clientharness.BlockHeader{}, err
	}

	return header, nil
}

// bitcoindGenerateHeaders mines blocks and returns their verbose headers.
func (r *stressRunner) bitcoindGenerateHeaders(ctx context.Context,
	blocks int) ([]clientharness.BlockHeader, error) {

	var address string
	if err := callBitcoindRPC(
		ctx, r.state, "getnewaddress", nil, &address,
	); err != nil {
		return nil, err
	}

	var hashes []string
	if err := callBitcoindRPC(
		ctx, r.state, "generatetoaddress", []any{blocks, address},
		&hashes,
	); err != nil {
		return nil, err
	}

	headers := make([]clientharness.BlockHeader, 0, len(hashes))
	for _, hash := range hashes {
		header, err := r.bitcoindBlockHeader(ctx, hash)
		if err != nil {
			return nil, err
		}
		headers = append(headers, header)
	}

	return headers, nil
}

// waitBitcoindHeight waits for bitcoind's active chain height.
func (r *stressRunner) waitBitcoindHeight(ctx context.Context,
	height int64) error {

	ticker := time.NewTicker(stressRoundPollInterval)
	defer ticker.Stop()

	for {
		var current uint32
		err := callBitcoindRPC(
			ctx, r.state, "getblockcount", nil, &current,
		)
		if err == nil && int64(current) == height {
			return nil
		}

		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("reorg wait bitcoind height "+
					"%d: %w", height, err)
			}

			return fmt.Errorf("reorg timed out waiting for "+
				"bitcoind height %d", height)

		case <-ticker.C:
		}
	}
}

// waitReorgConvergence waits for the operator and clients to report the new
// chain height after a reorg.
func (r *stressRunner) waitReorgConvergence(ctx context.Context,
	targetHeight uint32) error {

	ticker := time.NewTicker(stressRoundPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		ok, err := r.reorgConverged(ctx, targetHeight)
		if ok {
			return nil
		}
		if err != nil {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("reorg convergence: %w",
					lastErr)
			}

			return fmt.Errorf("reorg timed out waiting for "+
				"height %d", targetHeight)

		case <-ticker.C:
		}
	}
}

// reorgConverged checks whether every stress-visible service has caught up.
func (r *stressRunner) reorgConverged(ctx context.Context,
	targetHeight uint32) (bool, error) {

	info, err := r.h.ArkAdminClient.Info(ctx, &adminrpc.InfoRequest{})
	if err != nil {
		return false, fmt.Errorf("operator info: %w", err)
	}
	if info.GetBlockHeight() < targetHeight {
		return false, nil
	}

	for _, name := range r.names {
		clientRPC, err := r.clientRPC(name)
		if err != nil {
			return false, err
		}

		resp, err := clientRPC.GetInfo(
			ctx, &daemonrpc.GetInfoRequest{},
		)
		if err != nil {
			return false, fmt.Errorf("%s getinfo: %w", name, err)
		}
		if resp.GetBlockHeight() < targetHeight {
			return false, nil
		}
	}

	return true, nil
}

// reorgResultFields returns structured event fields for a completed reorg.
func reorgResultFields(result stressReorgResult) map[string]any {
	return map[string]any{
		"old_tip_height":      result.OldTip.Height,
		"old_tip_hash":        result.OldTip.Hash,
		"fork_height":         result.ForkPoint.Height,
		"fork_hash":           result.ForkPoint.Hash,
		"new_tip_height":      reorgTip(result).Height,
		"new_tip_hash":        reorgTip(result).Hash,
		"disconnected_blocks": blockHeaderHashes(result.Disconnected),
		"connected_blocks":    blockHeaderHashes(result.Connected),
		"disconnected_count":  len(result.Disconnected),
		"connected_count":     len(result.Connected),
		"replacement_is_longer": len(result.Connected) >
			len(result.Disconnected),
	}
}

// blockHeaderHashes returns header hashes in height order.
func blockHeaderHashes(headers []clientharness.BlockHeader) []string {
	hashes := make([]string, 0, len(headers))
	for _, header := range headers {
		hashes = append(hashes, header.Hash)
	}

	return hashes
}

// reorgTip returns the replacement-branch tip.
func reorgTip(result stressReorgResult) clientharness.BlockHeader {
	if len(result.Connected) == 0 {
		return clientharness.BlockHeader{}
	}

	return result.Connected[len(result.Connected)-1]
}

// recordReorgCompleted increments the successful reorg counter.
func (r *stressRunner) recordReorgCompleted() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.summary.ReorgsCompleted++
}

// recordReorgFailedf records one failed reorg attempt in the event log and
// summary counters.
func (r *stressRunner) recordReorgFailedf(kind string, err error,
	fields map[string]any, format string, args ...any) {

	class := r.classifyFailure(err)
	expected := r.failureExpected(class)
	r.mu.Lock()
	r.summary.ReorgsFailed++
	r.recordWorkloadFailureLocked(class, expected)
	r.mu.Unlock()

	if fields == nil {
		fields = make(map[string]any)
	}
	fields["class"] = class
	fields["expected"] = expected
	if err != nil {
		fields["error"] = err.Error()
	}

	r.events.Printf(kind, fields, format, args...)
}

// recordRoundConfirmed increments the successful refresh-round counter.
func (r *stressRunner) recordRoundConfirmed() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.summary.RoundsConfirmed++
}

// recordRoundFailedf records one failed refresh-round attempt in the event log
// and summary counters.
func (r *stressRunner) recordRoundFailedf(kind string, err error,
	fields map[string]any, format string, args ...any) {

	class := r.classifyFailure(err)
	expected := r.failureExpected(class)
	r.mu.Lock()
	r.summary.RoundsFailed++
	r.recordWorkloadFailureLocked(class, expected)
	r.mu.Unlock()

	if fields == nil {
		fields = make(map[string]any)
	}
	fields["class"] = class
	fields["expected"] = expected
	if err != nil {
		fields["error"] = err.Error()
	}

	r.events.Printf(kind, fields, format, args...)
}

// randomClientRestart gracefully restarts one client daemon.
func (r *stressRunner) randomClientRestart() {
	name := r.names[r.randIntn(len(r.names))]
	unlock := r.lockClients(name)
	defer unlock()

	r.events.Printf("client_restart", map[string]any{
		"client": name,
	},
		"client restarting client=%s", name)

	start := time.Now()
	client := r.h.RestartClientDaemon(name)
	r.setClient(name, client)
	if err := r.saveCurrentState(); err != nil {
		r.t.Fatalf("save state after client restart: %v", err)
	}

	r.events.Printf("client_ready", map[string]any{
		"client":     name,
		"latency_ms": time.Since(start).Milliseconds(),
	},
		"client ready client=%s latency=%s", name,
		time.Since(start).Round(time.Millisecond))
}

// randomClientCrash crashes and recovers one client daemon.
func (r *stressRunner) randomClientCrash() {
	name := r.names[r.randIntn(len(r.names))]
	unlock := r.lockClients(name)
	defer unlock()

	r.events.Printf("client_crash", map[string]any{
		"client": name,
	},
		"client crashing client=%s", name)

	start := time.Now()
	client := r.h.CrashClientDaemon(name)
	r.setClient(name, client)
	if err := r.saveCurrentState(); err != nil {
		r.t.Fatalf("save state after client crash: %v", err)
	}

	r.events.Printf("client_recovered", map[string]any{
		"client":     name,
		"latency_ms": time.Since(start).Milliseconds(),
	},
		"client recovered client=%s latency=%s", name,
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
	},
		"operator ready latency=%s rpc=%s",
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
		r.mineStressBlocks(stressRoundMineDepth)
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
	timeout time.Duration, statuses ...adminrpc.RoundStatus) (
	adminrpc.RoundStatus, error) {

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
func (r *stressRunner) adminRoundStatus(roundID string) (adminrpc.RoundStatus,
	bool, error) {

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
func (r *stressRunner) waitAllClientsRoundAtLeast(minState daemonrpc.RoundState,
	timeout time.Duration) error {

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

	return fmt.Errorf("timed out waiting for all clients in round state "+
		"at least %s", minState)
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

	return fmt.Errorf("timed out waiting for %s in round state %s", name,
		minState)
}

// clientHasRoundAtLeast reports whether a client has any local round at or
// beyond the requested state.
func (r *stressRunner) clientHasRoundAtLeast(name string,
	minState daemonrpc.RoundState) (bool, error) {

	ctx, cancel := r.shortContext()
	defer cancel()

	clientRPC, err := r.clientRPC(name)
	if err != nil {
		return false, err
	}
	resp, err := clientRPC.ListRounds(
		ctx, &daemonrpc.ListRoundsRequest{},
	)
	if err != nil {
		return false, err
	}

	for _, round := range resp.Rounds {
		if round.State == daemonrpc.RoundState_ROUND_STATE_FAILED {
			return false, fmt.Errorf("%s has failed round %s", name,
				round.RoundId)
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

// checkRecovery probes the topology after all workload workers have drained.
func (r *stressRunner) checkRecovery() {
	var failures []string

	ctx, cancel := r.shortContext()
	if _, err := r.h.ArkAdminClient.Info(
		ctx, &adminrpc.InfoRequest{},
	); err != nil {

		failures = append(
			failures, fmt.Sprintf("operator info failed: %v", err),
		)
	}
	cancel()

	for _, name := range r.names {
		clientRPC, err := r.clientRPC(name)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}

		ctx, cancel := r.shortContext()
		_, balanceErr := clientRPC.GetBalance(
			ctx, &daemonrpc.GetBalanceRequest{},
		)
		cancel()
		if balanceErr != nil {
			failures = append(
				failures,
				fmt.Sprintf("%s balance failed: %v", name,
					balanceErr),
			)

			continue
		}

		ctx, cancel = r.shortContext()
		filter := daemonrpc.VTXOStatus_VTXO_STATUS_LIVE
		_, listErr := clientRPC.ListVTXOs(
			ctx, &daemonrpc.ListVTXOsRequest{
				StatusFilter: filter,
			},
		)
		cancel()
		if listErr != nil {
			failures = append(
				failures,
				fmt.Sprintf("%s list vtxos failed: %v", name,
					listErr),
			)
		}
	}

	r.mu.Lock()
	r.summary.RecoveryFailures = append(
		r.summary.RecoveryFailures, failures...,
	)
	r.mu.Unlock()

	if len(failures) == 0 {
		r.events.Print("recovery", "recovery check passed", nil)

		return
	}

	r.events.Printf("recovery_failed", map[string]any{
		"failures": failures,
	},
		"recovery check failed failures=%d", len(failures))
}

// writeSummary writes summary.json and emits the final sparse summary event.
func (r *stressRunner) writeSummary() {
	r.stopDiagnostics("summary")

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
	summary.OperatorDB = r.cfg.operatorDB
	summary.BoardAmountSat = r.cfg.boardAmount
	summary.BoardVTXOs = r.cfg.boardVTXOs
	summary.Concurrency = r.cfg.concurrency
	if r.cfg.mineIntervalMin > 0 {
		summary.BackgroundMineMin = r.cfg.mineIntervalMin.Milliseconds()
		summary.BackgroundMineMax = r.cfg.mineIntervalMax.Milliseconds()
	}
	summary.TraceFile = r.diagnosticPaths.TraceFile
	summary.CPUProfileFile = r.diagnosticPaths.CPUProfileFile
	summary.BlockProfileFile = r.diagnosticPaths.BlockProfileFile
	summary.MutexProfileFile = r.diagnosticPaths.MutexProfileFile
	summary.HarnessResult = stressResultPass
	summary.RecoveryResult = stressResultPass
	if len(summary.RecoveryFailures) > 0 {
		summary.RecoveryResult = stressResultFail
	}
	summary.WorkloadResult = stressResultPass
	if summary.UnexpectedFailures > 0 {
		summary.WorkloadResult = stressResultUnexpectedFailures
	} else if summary.ExpectedFailures > 0 {
		summary.WorkloadResult = stressResultExpectedFailures
	}
	summary.InvariantsResult = stressResultPass
	if summary.UnexpectedFailures > 0 ||
		summary.RecoveryResult != stressResultPass {

		summary.InvariantsResult = stressResultFail
	}

	if summary.FailureClasses != nil {
		failureClasses := make(
			map[string]int, len(summary.FailureClasses),
		)
		for class, count := range summary.FailureClasses {
			failureClasses[class] = count
		}
		summary.FailureClasses = failureClasses
	}
	if summary.Skips != nil {
		skipClasses := make(
			map[string]int, len(summary.Skips),
		)
		for class, count := range summary.Skips {
			skipClasses[class] = count
		}
		summary.Skips = skipClasses
	}
	summary.RecoveryFailures = append(
		[]string(nil), summary.RecoveryFailures...,
	)

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

	unrollLatencies := append([]time.Duration(nil), r.unrollLatencies...)
	if len(unrollLatencies) > 0 {
		sort.Slice(unrollLatencies, func(i, j int) bool {
			return unrollLatencies[i] < unrollLatencies[j]
		})

		var total time.Duration
		for _, latency := range unrollLatencies {
			total += latency
		}

		summary.UnrollAvgMS = (total /
			time.Duration(len(unrollLatencies))).Milliseconds()
		summary.UnrollP50MS = percentileDuration(
			unrollLatencies, 50,
		).Milliseconds()
		summary.UnrollP95MS = percentileDuration(
			unrollLatencies, 95,
		).Milliseconds()
		maxLatency := unrollLatencies[len(unrollLatencies)-1]
		summary.UnrollMaxMS = maxLatency.Milliseconds()
	}

	waitLatencies := append([]time.Duration(nil), r.liquidityWaits...)
	if len(waitLatencies) > 0 {
		sort.Slice(waitLatencies, func(i, j int) bool {
			return waitLatencies[i] < waitLatencies[j]
		})

		var total time.Duration
		for _, latency := range waitLatencies {
			total += latency
		}

		summary.LiquidityWaitAvgMS = (total /
			time.Duration(len(waitLatencies))).Milliseconds()
		summary.LiquidityWaitP50MS = percentileDuration(
			waitLatencies, 50,
		).Milliseconds()
		summary.LiquidityWaitP95MS = percentileDuration(
			waitLatencies, 95,
		).Milliseconds()
		maxLatency := waitLatencies[len(waitLatencies)-1]
		summary.LiquidityWaitMaxMS = maxLatency.Milliseconds()
	}
	summary.PaymentMinDelayMS = r.cfg.paymentMinDelay.Milliseconds()

	// Persist the derived fields so summary.json and any later readers of
	// r.summary observe the same final snapshot.
	r.summary = summary

	return summary
}

// printFinalSummary emits a prominent human-readable stress summary.
func (r *stressRunner) printFinalSummary(path string, summary stressSummary) {
	r.events.BlankLine()
	r.events.Print("stress_summary", stressSummaryTopLine, nil)
	r.events.Printf("stress_summary", map[string]any{
		"summary":    path,
		"harness":    summary.HarnessResult,
		"workload":   summary.WorkloadResult,
		"invariants": summary.InvariantsResult,
		"recovery":   summary.RecoveryResult,
	},
		"HARNESS=%s WORKLOAD=%s INVARIANTS=%s RECOVERY=%s",
		strings.ToUpper(summary.HarnessResult),
		strings.ToUpper(summary.WorkloadResult),
		strings.ToUpper(summary.InvariantsResult),
		strings.ToUpper(summary.RecoveryResult))
	r.events.Printf("stress_summary", map[string]any{
		"attempted":  summary.PaymentsAttempted,
		"settled":    summary.PaymentsSettled,
		"failed":     summary.PaymentsFailed,
		"skipped":    summary.PaymentsSkipped,
		"expected":   summary.ExpectedFailures,
		"unexpected": summary.UnexpectedFailures,
		"success":    summary.PaymentSuccessPct,
	},
		"payments settled=%d/%d failed=%d skipped=%d expected=%d "+
			"unexpected=%d success=%.1f%%",
		summary.PaymentsSettled, summary.PaymentsAttempted,
		summary.PaymentsFailed, summary.PaymentsSkipped,
		summary.ExpectedFailures, summary.UnexpectedFailures,
		summary.PaymentSuccessPct)
	if len(summary.FailureClasses) > 0 {
		r.events.Printf("stress_summary", map[string]any{
			"failure_classes": summary.FailureClasses,
		},
			"failure classes: %s", formatFailureClasses(
				summary.FailureClasses,
			))
	}
	if len(summary.Skips) > 0 {
		r.events.Printf("stress_summary", map[string]any{
			"skip_classes": summary.Skips,
		},
			"payment skip classes: %s", formatFailureClasses(
				summary.Skips,
			))
	}
	if len(summary.RecoveryFailures) > 0 {
		r.events.Printf("stress_summary", map[string]any{
			"failures": summary.RecoveryFailures,
		},
			"recovery failures=%d", len(summary.RecoveryFailures))
	}
	r.events.Printf("stress_summary", map[string]any{
		"avg_ms": summary.PaymentAvgMS,
		"p50_ms": summary.PaymentP50MS,
		"p95_ms": summary.PaymentP95MS,
		"max_ms": summary.PaymentMaxMS,
	},
		"payment latency avg=%dms p50=%dms p95=%dms max=%dms",
		summary.PaymentAvgMS, summary.PaymentP50MS,
		summary.PaymentP95MS, summary.PaymentMaxMS)
	r.events.Printf("stress_summary", map[string]any{
		"attempted": summary.UnrollsAttempted,
		"completed": summary.UnrollsCompleted,
		"failed":    summary.UnrollsFailed,
		"skipped":   summary.UnrollsSkipped,
		"avg_ms":    summary.UnrollAvgMS,
		"p50_ms":    summary.UnrollP50MS,
		"p95_ms":    summary.UnrollP95MS,
		"max_ms":    summary.UnrollMaxMS,
	},
		"unrolls completed=%d/%d failed=%d skipped=%d avg=%dms "+
			"p50=%dms p95=%dms max=%dms",
		summary.UnrollsCompleted, summary.UnrollsAttempted,
		summary.UnrollsFailed, summary.UnrollsSkipped,
		summary.UnrollAvgMS, summary.UnrollP50MS, summary.UnrollP95MS,
		summary.UnrollMaxMS)
	if summary.LiquidityWaits > 0 {
		r.events.Printf("stress_summary", map[string]any{
			"waits":      summary.LiquidityWaits,
			"timeouts":   summary.LiquidityTimeouts,
			"avg_ms":     summary.LiquidityWaitAvgMS,
			"p50_ms":     summary.LiquidityWaitP50MS,
			"p95_ms":     summary.LiquidityWaitP95MS,
			"max_ms":     summary.LiquidityWaitMaxMS,
			"timeout_ms": r.cfg.liquidityTimeout.Milliseconds(),
		},
			"liquidity wait count=%d timeouts=%d avg=%dms "+
				"p50=%dms p95=%dms max=%dms timeout=%s",
			summary.LiquidityWaits, summary.LiquidityTimeouts,
			summary.LiquidityWaitAvgMS, summary.LiquidityWaitP50MS,
			summary.LiquidityWaitP95MS, summary.LiquidityWaitMaxMS,
			r.cfg.liquidityTimeout)
	}
	if summary.PaymentMinDelayMS > 0 {
		r.events.Printf("stress_summary", map[string]any{
			"payment_min_interval_ms": summary.PaymentMinDelayMS,
		},
			"payment pacing min_interval=%s",
			r.cfg.paymentMinDelay)
	}
	r.events.Printf("stress_summary", map[string]any{
		"throughput_per_sec": summary.PaymentThroughput,
		"duration_ms":        summary.DurationMS,
		"concurrency":        summary.Concurrency,
	},
		"throughput %.2f settled payments/sec duration=%s "+
			"concurrency=%d",
		summary.PaymentThroughput,
		time.Duration(summary.DurationMS)*time.Millisecond,
		summary.Concurrency)
	r.events.Printf("stress_summary", map[string]any{
		"rounds":            summary.RoundsTriggered,
		"round_confirmed":   summary.RoundsConfirmed,
		"round_failures":    summary.RoundsFailed,
		"reorgs":            summary.ReorgsTriggered,
		"reorg_completed":   summary.ReorgsCompleted,
		"reorg_failures":    summary.ReorgsFailed,
		"client_restarts":   summary.ClientRestarts,
		"client_crashes":    summary.ClientCrashes,
		"operator_restarts": summary.OperatorRestarts,
	},
		"rounds confirmed=%d/%d failed=%d client_restarts=%d "+
			"client_crashes=%d operator_restarts=%d reorgs=%d/%d "+
			"failed=%d",
		summary.RoundsConfirmed, summary.RoundsTriggered,
		summary.RoundsFailed, summary.ClientRestarts,
		summary.ClientCrashes, summary.OperatorRestarts,
		summary.ReorgsCompleted, summary.ReorgsTriggered,
		summary.ReorgsFailed)
	r.printDiagnosticsSummary(summary)
	r.printArtifactSummary(path)
	r.events.Print("stress_summary", stressSummaryBottomLine, nil)
	r.events.BlankLine()
}

// printDiagnosticsSummary emits direct paths to optional Go runtime artifacts.
func (r *stressRunner) printDiagnosticsSummary(summary stressSummary) {
	noTrace := summary.TraceFile == ""
	noCPUProfile := summary.CPUProfileFile == ""
	noBlockProfile := summary.BlockProfileFile == ""
	noMutexProfile := summary.MutexProfileFile == ""
	if noTrace && noCPUProfile && noBlockProfile && noMutexProfile {
		return
	}

	r.events.Print("stress_summary", "diagnostics:", nil)
	if summary.TraceFile != "" {
		r.events.Printf("stress_summary", map[string]any{
			"trace_file": summary.TraceFile,
		},
			"  trace_file=%s", summary.TraceFile)
	}
	if summary.CPUProfileFile != "" {
		r.events.Printf("stress_summary", map[string]any{
			"cpu_profile": summary.CPUProfileFile,
		},
			"  cpu_profile=%s", summary.CPUProfileFile)
	}
	if summary.BlockProfileFile != "" {
		r.events.Printf("stress_summary", map[string]any{
			"block_profile": summary.BlockProfileFile,
		},
			"  block_profile=%s", summary.BlockProfileFile)
	}
	if summary.MutexProfileFile != "" {
		r.events.Printf("stress_summary", map[string]any{
			"mutex_profile": summary.MutexProfileFile,
		},
			"  mutex_profile=%s", summary.MutexProfileFile)
	}
	if summary.TraceFile != "" {
		r.events.Print(
			"stress_summary", "  trace_scope=arktest+in-process-"+
				"operator+clients", nil,
		)
	}
	if summary.BlockProfileFile != "" || summary.MutexProfileFile != "" {
		r.events.Printf("stress_summary", map[string]any{
			"block_rate_ns":  stressBlockProfileRate,
			"mutex_fraction": stressMutexProfileFraction,
		},
			"  profile_sampling=block_rate_ns=%d mutex_fraction=%d",
			stressBlockProfileRate, stressMutexProfileFraction)
	}

	commands := stressDiagnosticCommands(summary)
	if len(commands) == 0 {
		return
	}

	r.events.Print("stress_summary", "diagnostic commands:", nil)
	for _, command := range commands {
		r.events.Printf("stress_summary", map[string]any{
			"command": command,
		},
			"  %s", command)
	}
}

// stressDiagnosticCommands returns browser commands for diagnostics artifacts.
func stressDiagnosticCommands(summary stressSummary) []string {
	var commands []string
	if summary.TraceFile != "" {
		commands = append(
			commands, fmt.Sprintf("go tool trace %s",
				summary.TraceFile),
		)
	}
	if summary.CPUProfileFile != "" {
		commands = append(
			commands, fmt.Sprintf("go tool pprof -http=:0 "+
				"./arktest %s", summary.CPUProfileFile),
		)
	}
	if summary.BlockProfileFile != "" {
		commands = append(
			commands, fmt.Sprintf("go tool pprof -http=:0 "+
				"./arktest %s", summary.BlockProfileFile),
		)
	}
	if summary.MutexProfileFile != "" {
		commands = append(
			commands, fmt.Sprintf("go tool pprof -http=:0 "+
				"./arktest %s", summary.MutexProfileFile),
		)
	}

	return commands
}

// printArtifactSummary emits direct paths to the main stress run artifacts.
func (r *stressRunner) printArtifactSummary(summaryPath string) {
	runDir := r.state.RunDir
	eventsPath := filepath.Join(runDir, defaultEventLogName)
	harnessLog := filepath.Join(runDir, "harness.log")
	operatorLog := filepath.Join(runDir, "arkd", "arkd.log")
	operatorLNDLog := lndLogPath(filepath.Join(runDir, "lnd"))
	bitcoindLog := filepath.Join(
		runDir, "bitcoind", "regtest", "debug.log",
	)

	r.events.Print("stress_summary", "artifacts:", nil)
	r.events.Printf("stress_summary", map[string]any{
		"run_dir": runDir,
	},
		"  run_dir=%s", runDir)
	r.events.Printf("stress_summary", map[string]any{
		"events_jsonl": eventsPath,
	},
		"  events_jsonl=%s", eventsPath)
	r.events.Printf("stress_summary", map[string]any{
		"summary_json": summaryPath,
	},
		"  summary_json=%s", summaryPath)
	r.events.Printf("stress_summary", map[string]any{
		"harness_log": harnessLog,
	},
		"  harness_log=%s", harnessLog)
	r.events.Printf("stress_summary", map[string]any{
		"operator_log": operatorLog,
	},
		"  operator_log=%s", operatorLog)
	r.events.Printf("stress_summary", map[string]any{
		"operator_lnd_log": operatorLNDLog,
	},
		"  operator_lnd_log=%s", operatorLNDLog)
	r.events.Printf("stress_summary", map[string]any{
		"bitcoind_log": bitcoindLog,
	},
		"  bitcoind_log=%s", bitcoindLog)
	r.events.Print(
		"stress_summary", "client logs: run `arktest logs` to list "+
			"component targets", nil,
	)
}

// formatFailureClasses returns a stable human-readable failure class list.
func formatFailureClasses(classes map[string]int) string {
	keys := make([]string, 0, len(classes))
	for class := range classes {
		keys = append(keys, class)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, class := range keys {
		part := fmt.Sprintf("%s=%d", class, classes[class])
		parts = append(parts, part)
	}

	return strings.Join(parts, " ")
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
func (r *stressRunner) contextWithTimeout(timeout time.Duration) (
	context.Context, context.CancelFunc) {

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
