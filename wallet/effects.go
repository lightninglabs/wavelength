package wallet

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	WalletEffectRecordLedgerSweepFee    = "record_ledger_sweep_fee"
	WalletEffectRecordLedgerUTXOCreated = "record_ledger_utxo_created"
	WalletEffectRecordLedgerUTXOSpent   = "record_ledger_utxo_spent"

	defaultWalletEffectBatchSize = 32
	defaultWalletEffectLease     = 30 * time.Second
	defaultWalletEffectInterval  = time.Second
	defaultWalletEffectRetry     = 5 * time.Second
)

// WalletEffect is a durable wallet-owned side effect. It points at concrete
// wallet facts by txid/outpoint; it does not carry an actor payload.
type WalletEffect struct {
	ID             string
	EffectType     string
	IdempotencyKey string

	OutpointHash   []byte
	OutpointIndex  sql.NullInt32
	Txid           []byte
	AmountSat      sql.NullInt64
	FeeSat         sql.NullInt64
	BlockHeight    sql.NullInt32
	Classification sql.NullString

	ClaimToken sql.NullString
	Attempts   int32
}

// WalletEffectInsert describes one pending wallet effect to persist.
type WalletEffectInsert struct {
	ID             string
	EffectType     string
	IdempotencyKey string

	OutpointHash   []byte
	OutpointIndex  sql.NullInt32
	Txid           []byte
	AmountSat      sql.NullInt64
	FeeSat         sql.NullInt64
	BlockHeight    sql.NullInt32
	Classification sql.NullString

	MaxAttempts int32
}

// WalletEffectStore is the concrete durability surface for wallet effects.
type WalletEffectStore interface {
	InsertWalletEffect(ctx context.Context, effect WalletEffectInsert) error

	ClaimDueWalletEffects(ctx context.Context, owner string, limit int,
		lease time.Duration) ([]WalletEffect, error)

	MarkWalletEffectDone(ctx context.Context, id, claimToken string) error

	ReleaseWalletEffectForRetry(ctx context.Context, id, claimToken string,
		retryAfter time.Duration, failure error) error

	ReleaseExpiredWalletEffectClaims(ctx context.Context) error
}

// BoardingSweepLedgerEffectEmitter marks sweep stores that enqueue ledger
// effects in the same transaction that resolves a boarding sweep.
type BoardingSweepLedgerEffectEmitter interface {
	EmitsBoardingSweepLedgerEffects() bool
}

// WalletEffectWorker drains wallet_effects rows into the existing ledger sink.
// Ledger handlers are idempotent, so a crash after Tell and before mark-done
// safely retries the same effect row.
type WalletEffectWorker struct {
	store WalletEffectStore
	sink  ledger.Sink
	clk   clock.Clock
	log   fn.Option[btclog.Logger]

	owner      string
	batchSize  int
	lease      time.Duration
	interval   time.Duration
	retryDelay time.Duration

	cancel context.CancelFunc
	done   chan struct{}
}

// WalletEffectWorkerConfig configures a wallet effect worker.
type WalletEffectWorkerConfig struct {
	Store WalletEffectStore
	Sink  ledger.Sink
	Clock clock.Clock
	Log   fn.Option[btclog.Logger]

	Owner      string
	BatchSize  int
	Lease      time.Duration
	Interval   time.Duration
	RetryDelay time.Duration
}

// NewWalletEffectWorker creates a worker for wallet-owned durable effects.
func NewWalletEffectWorker(cfg WalletEffectWorkerConfig) *WalletEffectWorker {
	if cfg.Clock == nil {
		cfg.Clock = clock.NewDefaultClock()
	}
	if cfg.Owner == "" {
		cfg.Owner = "wallet-effects-" + uuid.NewString()
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultWalletEffectBatchSize
	}
	if cfg.Lease <= 0 {
		cfg.Lease = defaultWalletEffectLease
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultWalletEffectInterval
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = defaultWalletEffectRetry
	}

	return &WalletEffectWorker{
		store:      cfg.Store,
		sink:       cfg.Sink,
		clk:        cfg.Clock,
		log:        cfg.Log,
		owner:      cfg.Owner,
		batchSize:  cfg.BatchSize,
		lease:      cfg.Lease,
		interval:   cfg.Interval,
		retryDelay: cfg.RetryDelay,
		done:       make(chan struct{}),
	}
}

// Start begins the polling worker.
func (w *WalletEffectWorker) Start(ctx context.Context) error {
	if w.store == nil {
		return fmt.Errorf("wallet effect store must be provided")
	}
	if w.sink == nil {
		return fmt.Errorf("ledger sink must be provided")
	}
	if w.cancel != nil {
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel

	go w.loop(runCtx)

	return nil
}

// Stop stops the polling worker and waits for it to exit.
func (w *WalletEffectWorker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	if w.done != nil {
		<-w.done
	}
}

func (w *WalletEffectWorker) loop(ctx context.Context) {
	defer close(w.done)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			w.logger(ctx).WarnS(
				ctx,
				"wallet effect worker tick failed",
				err,
			)
		}

		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
		}
	}
}

// RunOnce claims and processes one batch of due wallet effects.
func (w *WalletEffectWorker) RunOnce(ctx context.Context) error {
	if err := w.store.ReleaseExpiredWalletEffectClaims(ctx); err != nil {
		return err
	}

	effects, err := w.store.ClaimDueWalletEffects(
		ctx, w.owner, w.batchSize, w.lease,
	)
	if err != nil {
		return err
	}

	for _, effect := range effects {
		if err := w.handleEffect(ctx, effect); err != nil {
			w.logger(ctx).WarnS(ctx, "wallet effect failed",
				err,
				slog.String("effect_id", effect.ID),
				slog.String("effect_type", effect.EffectType),
				slog.Int("attempts", int(effect.Attempts)),
			)

			token := effect.ClaimToken.String
			releaseErr := w.store.ReleaseWalletEffectForRetry(
				ctx, effect.ID, token, w.retryDelay, err,
			)
			if releaseErr != nil {
				return releaseErr
			}

			continue
		}

		if err := w.store.MarkWalletEffectDone(
			ctx, effect.ID, effect.ClaimToken.String,
		); err != nil {
			return err
		}
	}

	return nil
}

func (w *WalletEffectWorker) handleEffect(ctx context.Context,
	effect WalletEffect) error {

	switch effect.EffectType {
	case WalletEffectRecordLedgerSweepFee:
		txid, err := hashFromEffect("txid", effect.Txid)
		if err != nil {
			return err
		}
		if !effect.FeeSat.Valid {
			return fmt.Errorf("wallet effect %s missing fee_sat",
				effect.ID)
		}

		return w.sink.Tell(ctx, &ledger.FeePaidMsg{
			AmountSat:      effect.FeeSat.Int64,
			FeeType:        ledger.FeeTypeOnchainSweep,
			BlockHeight:    uint32(nullInt32(effect.BlockHeight)),
			IdempotencyKey: append([]byte(nil), txid[:]...),
		})

	case WalletEffectRecordLedgerUTXOCreated:
		opHash, err := hashFromEffect(
			"outpoint_hash", effect.OutpointHash,
		)
		if err != nil {
			return err
		}
		if !effect.OutpointIndex.Valid || !effect.AmountSat.Valid ||
			!effect.Classification.Valid {
			return fmt.Errorf("wallet effect %s missing UTXO "+
				"created fields", effect.ID)
		}

		return w.sink.Tell(ctx, &ledger.UTXOCreatedMsg{
			OutpointHash:   opHash,
			OutpointIndex:  uint32(effect.OutpointIndex.Int32),
			AmountSat:      effect.AmountSat.Int64,
			BlockHeight:    uint32(nullInt32(effect.BlockHeight)),
			Classification: effect.Classification.String,
		})

	case WalletEffectRecordLedgerUTXOSpent:
		opHash, err := hashFromEffect(
			"outpoint_hash", effect.OutpointHash,
		)
		if err != nil {
			return err
		}
		if !effect.OutpointIndex.Valid || !effect.AmountSat.Valid ||
			!effect.Classification.Valid {
			return fmt.Errorf("wallet effect %s missing UTXO "+
				"spent fields", effect.ID)
		}

		return w.sink.Tell(ctx, &ledger.UTXOSpentMsg{
			OutpointHash:   opHash,
			OutpointIndex:  uint32(effect.OutpointIndex.Int32),
			AmountSat:      effect.AmountSat.Int64,
			BlockHeight:    uint32(nullInt32(effect.BlockHeight)),
			Classification: effect.Classification.String,
		})

	default:
		return fmt.Errorf("unknown wallet effect type %q",
			effect.EffectType)
	}
}

func (w *WalletEffectWorker) logger(ctx context.Context) btclog.Logger {
	return w.log.UnwrapOr(build.LoggerFromContext(ctx))
}

func nullInt32(v sql.NullInt32) int32 {
	if !v.Valid {
		return 0
	}

	return v.Int32
}

func hashFromEffect(name string, raw []byte) (chainhash.Hash, error) {
	if len(raw) != chainhash.HashSize {
		return chainhash.Hash{}, fmt.Errorf("%s must be %d bytes", name,
			chainhash.HashSize)
	}

	var h chainhash.Hash
	copy(h[:], raw)

	return h, nil
}
