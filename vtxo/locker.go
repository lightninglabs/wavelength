package vtxo

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// LockOwner identifies the subsystem and session that owns a VTXO lock.
//
// This is a string rather than a structured type so it can be stored and
// compared cheaply, while still avoiding collisions between subsystems by
// convention (e.g. "round:<uuid>", "oor:<txid>").
type LockOwner string

const (
	// LockOwnerRoundPrefix is the prefix used for round-owned locks.
	LockOwnerRoundPrefix = "round:"

	// LockOwnerOORPrefix is the prefix used for OOR-session-owned locks.
	LockOwnerOORPrefix = "oor:"
)

// RoundLockOwner returns the LockOwner used to lock VTXOs for a server round.
func RoundLockOwner(roundID string) LockOwner {
	return LockOwner(LockOwnerRoundPrefix + roundID)
}

// OORLockOwner returns the LockOwner used to lock VTXOs for an OOR session.
func OORLockOwner(sessionID string) LockOwner {
	return LockOwner(LockOwnerOORPrefix + sessionID)
}

// Locker provides mutual exclusion for VTXO outpoints across subsystems.
//
// The default implementation is in-memory (best-effort; cleared on restart).
// A DB-backed implementation also exists (see db.VTXOLockerDB) which persists
// lock state across restarts.
type Locker interface {
	// LockMany attempts to lock all outpoints for the owner atomically.
	//
	// If any outpoint is already locked by a different owner, this returns
	// an error and does not modify any lock state.
	LockMany(ctx context.Context, outpoints []wire.OutPoint,
		owner LockOwner) error

	// UnlockMany releases locks held by owner.
	//
	// This is idempotent: unlocking an outpoint that is not locked is a
	// no-op, and unlocking an outpoint already locked by owner is always
	// allowed.
	//
	// If an outpoint is locked by a different owner, this returns an error
	// and leaves lock state unchanged.
	UnlockMany(ctx context.Context, outpoints []wire.OutPoint,
		owner LockOwner) error
}

// LeaseLocker is an optional extension that supports lock expiries.
//
// Implementations should treat an expired lock as released.
type LeaseLocker interface {
	// LockManyWithExpiry attempts to lock outpoints until the expiry time.
	//
	// A zero expiry means the lock does not expire.
	LockManyWithExpiry(ctx context.Context, outpoints []wire.OutPoint,
		owner LockOwner, expiresAt time.Time) error
}

// ErrLocked is returned when attempting to lock an outpoint that is already
// locked by another owner.
type ErrLocked struct {
	Outpoint wire.OutPoint
	Owner    LockOwner
}

// Error returns the error message.
func (e *ErrLocked) Error() string {
	return fmt.Sprintf("outpoint %v locked by %s", e.Outpoint, e.Owner)
}

// ErrNotOwner is returned when attempting to unlock an outpoint that is locked
// by another owner.
type ErrNotOwner struct {
	Outpoint wire.OutPoint
	Owner    LockOwner
	Attempt  LockOwner
}

// Error returns the error message.
func (e *ErrNotOwner) Error() string {
	return fmt.Sprintf("outpoint %v locked by %s (attempt %s)",
		e.Outpoint, e.Owner, e.Attempt)
}

// InMemoryLocker is a simple in-memory Locker implementation intended for
// unit tests and early in-process development.
type InMemoryLocker struct {
	mu sync.Mutex

	clk clock.Clock
	log btclog.Logger

	locks  map[wire.OutPoint]LockOwner
	leases map[wire.OutPoint]time.Time
}

// NewInMemoryLocker creates a new empty in-memory locker.
func NewInMemoryLocker(
	log ...fn.Option[btclog.Logger]) *InMemoryLocker {

	return NewInMemoryLockerWithClock(
		clock.NewDefaultClock(), log...,
	)
}

// NewInMemoryLockerWithClock creates a new empty in-memory locker using the
// provided clock.
func NewInMemoryLockerWithClock(clk clock.Clock,
	log ...fn.Option[btclog.Logger]) *InMemoryLocker {

	if clk == nil {
		clk = clock.NewDefaultClock()
	}

	logger := btclog.Disabled
	if len(log) > 0 {
		logger = log[0].UnwrapOr(btclog.Disabled)
	}

	return &InMemoryLocker{
		clk:    clk,
		log:    logger,
		locks:  make(map[wire.OutPoint]LockOwner),
		leases: make(map[wire.OutPoint]time.Time),
	}
}

// evictExpired removes any expired leases.
func (l *InMemoryLocker) evictExpired(now time.Time) {
	// NOTE: This is currently an O(n) scan over all leases. If the VTXO set
	// grows large, consider tracking expiries in a min-heap (or running a
	// periodic sweeper) so eviction work can be bounded.
	for op, expiresAt := range l.leases {
		if expiresAt.IsZero() {
			continue
		}

		if now.After(expiresAt) {
			delete(l.leases, op)
			delete(l.locks, op)
		}
	}
}

// LockMany attempts to lock all outpoints for the owner atomically.
func (l *InMemoryLocker) LockMany(_ context.Context, outpoints []wire.OutPoint,
	owner LockOwner) error {

	return l.lockMany(outpoints, owner, time.Time{})
}

// LockManyWithExpiry attempts to lock outpoints until the expiry time.
func (l *InMemoryLocker) LockManyWithExpiry(_ context.Context,
	outpoints []wire.OutPoint, owner LockOwner,
	expiresAt time.Time) error {

	return l.lockMany(outpoints, owner, expiresAt)
}

func (l *InMemoryLocker) lockMany(outpoints []wire.OutPoint, owner LockOwner,
	expiresAt time.Time) error {

	if len(outpoints) == 0 {
		return nil
	}

	if owner == "" {
		return fmt.Errorf("owner must be provided")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.evictExpired(l.clk.Now())

	for _, op := range outpoints {
		existing, locked := l.locks[op]
		if !locked {
			continue
		}

		if existing == owner {
			continue
		}

		return &ErrLocked{
			Outpoint: op,
			Owner:    existing,
		}
	}

	for _, op := range outpoints {
		l.locks[op] = owner
		if expiresAt.IsZero() {
			delete(l.leases, op)
			continue
		}

		l.leases[op] = expiresAt
	}

	ctx := context.Background()
	l.log.DebugS(ctx, "Acquired VTXO locks",
		slog.Int("count", len(outpoints)),
		slog.String("owner", string(owner)))

	return nil
}

// UnlockMany releases locks held by owner.
func (l *InMemoryLocker) UnlockMany(_ context.Context,
	outpoints []wire.OutPoint, owner LockOwner) error {

	if len(outpoints) == 0 {
		return nil
	}

	if owner == "" {
		return fmt.Errorf("owner must be provided")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.evictExpired(l.clk.Now())

	for _, op := range outpoints {
		existing, locked := l.locks[op]
		if !locked {
			continue
		}

		if existing != owner {
			return &ErrNotOwner{
				Outpoint: op,
				Owner:    existing,
				Attempt:  owner,
			}
		}
	}

	for _, op := range outpoints {
		existing, locked := l.locks[op]
		if !locked {
			continue
		}

		if existing == owner {
			delete(l.locks, op)
			delete(l.leases, op)
		}
	}

	ctx := context.Background()
	l.log.DebugS(ctx, "Released VTXO locks",
		slog.Int("count", len(outpoints)),
		slog.String("owner", string(owner)))

	return nil
}

var _ Locker = (*InMemoryLocker)(nil)
var _ LeaseLocker = (*InMemoryLocker)(nil)
