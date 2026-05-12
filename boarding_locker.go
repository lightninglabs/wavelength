package darepo

import (
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/rounds"
)

// inMemoryBoardingLocker is a simple in-memory implementation of
// rounds.BoardingInputLocker that prevents the same boarding input
// from being used in multiple concurrent rounds. Locks are held in
// memory and lost on restart, which is acceptable because round
// state is also reset on restart.
type inMemoryBoardingLocker struct {
	mu    sync.RWMutex
	locks map[wire.OutPoint]rounds.RoundID
}

// newInMemoryBoardingLocker creates a new in-memory boarding input
// locker.
func newInMemoryBoardingLocker() *inMemoryBoardingLocker {
	return &inMemoryBoardingLocker{
		locks: make(map[wire.OutPoint]rounds.RoundID),
	}
}

// Lock attempts to lock a boarding input for the specified round.
// Returns an error if the input is already locked by another round.
func (l *inMemoryBoardingLocker) Lock(_ context.Context,
	outpoint *wire.OutPoint, roundID rounds.RoundID) error {

	l.mu.Lock()
	defer l.mu.Unlock()

	if existing, ok := l.locks[*outpoint]; ok {
		if existing != roundID {
			return fmt.Errorf("boarding input %v already locked "+
				"by round %v", outpoint, existing)
		}

		return nil
	}

	l.locks[*outpoint] = roundID

	return nil
}

// Unlock releases the lock on a boarding input for the specified
// round. Only the round that locked the input can unlock it.
func (l *inMemoryBoardingLocker) Unlock(_ context.Context,
	outpoint *wire.OutPoint, roundID rounds.RoundID) error {

	l.mu.Lock()
	defer l.mu.Unlock()

	existing, ok := l.locks[*outpoint]
	if !ok {
		return nil
	}

	if existing != roundID {
		return fmt.Errorf("boarding input %v locked by round "+
			"%v, not %v", outpoint, existing, roundID)
	}

	delete(l.locks, *outpoint)

	return nil
}

// IsLocked checks if an input is locked and returns the locking
// round ID if so.
func (l *inMemoryBoardingLocker) IsLocked(_ context.Context,
	outpoint *wire.OutPoint) (bool, rounds.RoundID, error) {

	l.mu.RLock()
	defer l.mu.RUnlock()

	roundID, ok := l.locks[*outpoint]
	if !ok {
		return false, rounds.RoundID{}, nil
	}

	return true, roundID, nil
}

// Compile-time check that inMemoryBoardingLocker implements the
// rounds.BoardingInputLocker interface.
var _ rounds.BoardingInputLocker = (*inMemoryBoardingLocker)(nil)
