//go:build systest

package systest

import (
	"context"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/stretchr/testify/mock"
)

// MockRoundStore implements rounds.RoundStore using testify/mock for test
// assertions. Only storage interfaces are mocked on the server side to assert
// proper calls while using real wallet, chain source, and signers.
type MockRoundStore struct {
	mock.Mock
}

// PersistRound implements rounds.RoundStore.
func (m *MockRoundStore) PersistRound(ctx context.Context,
	round *rounds.Round) error {

	args := m.Called(ctx, round)

	return args.Error(0)
}

// MarkRoundConfirmed implements rounds.RoundStore.
func (m *MockRoundStore) MarkRoundConfirmed(ctx context.Context,
	roundID rounds.RoundID, blockHeight int32,
	blockHash chainhash.Hash) error {

	args := m.Called(ctx, roundID, blockHeight, blockHash)

	return args.Error(0)
}

// LoadPendingRounds implements rounds.RoundStore.
func (m *MockRoundStore) LoadPendingRounds(ctx context.Context) (
	[]*rounds.Round, error) {

	args := m.Called(ctx)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]*rounds.Round), args.Error(1)
}

// SetupDefaultExpectations configures common expectations for a successful
// round. Call this in test setup to allow basic operations to succeed.
func (m *MockRoundStore) SetupDefaultExpectations() {
	m.On("LoadPendingRounds", mock.Anything).Return(nil, nil)
	m.On("PersistRound", mock.Anything, mock.Anything).Return(nil)
	m.On(
		"MarkRoundConfirmed", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything,
	).Return(nil)
}

// Compile-time check that MockRoundStore implements rounds.RoundStore.
var _ rounds.RoundStore = (*MockRoundStore)(nil)

// MockBoardingLocker implements rounds.BoardingInputLocker using testify/mock
// for test assertions. This allows tests to verify that boarding inputs are
// properly locked during registration and unlocked on round completion/failure.
type MockBoardingLocker struct {
	mock.Mock
}

// Lock implements rounds.BoardingInputLocker.
func (m *MockBoardingLocker) Lock(ctx context.Context, outpoint *wire.OutPoint,
	roundID rounds.RoundID) error {

	args := m.Called(ctx, outpoint, roundID)

	return args.Error(0)
}

// Unlock implements rounds.BoardingInputLocker.
func (m *MockBoardingLocker) Unlock(ctx context.Context,
	outpoint *wire.OutPoint, roundID rounds.RoundID) error {

	args := m.Called(ctx, outpoint, roundID)

	return args.Error(0)
}

// IsLocked implements rounds.BoardingInputLocker.
func (m *MockBoardingLocker) IsLocked(ctx context.Context,
	outpoint *wire.OutPoint) (bool, rounds.RoundID, error) {

	args := m.Called(ctx, outpoint)

	return args.Bool(0), args.Get(1).(rounds.RoundID), args.Error(2)
}

// SetupDefaultExpectations configures common expectations for successful
// locking operations. Call this in test setup to allow basic operations.
func (m *MockBoardingLocker) SetupDefaultExpectations() {
	m.On("Lock", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	m.On("Unlock", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	m.On("IsLocked", mock.Anything, mock.Anything).Return(
		false, rounds.RoundID{}, nil,
	)
}

// Compile-time check that MockBoardingLocker implements
// rounds.BoardingInputLocker.
var _ rounds.BoardingInputLocker = (*MockBoardingLocker)(nil)

// MockVTXOStore implements rounds.VTXOStore using testify/mock for test
// assertions. This allows tests to verify that VTXOs are properly persisted
// after round completion and marked live after confirmation.
type MockVTXOStore struct {
	mock.Mock
}

// PersistVTXOs implements rounds.VTXOStore.
func (m *MockVTXOStore) PersistVTXOs(ctx context.Context,
	vtxos []*rounds.VTXO) error {

	args := m.Called(ctx, vtxos)

	return args.Error(0)
}

// MarkVTXOsLive implements rounds.VTXOStore.
func (m *MockVTXOStore) MarkVTXOsLive(ctx context.Context,
	roundID rounds.RoundID) error {

	args := m.Called(ctx, roundID)

	return args.Error(0)
}

// MarkVTXOForfeit implements rounds.VTXOStore.
func (m *MockVTXOStore) MarkVTXOForfeit(ctx context.Context,
	outpoint wire.OutPoint, info *rounds.ForfeitInfo) error {

	args := m.Called(ctx, outpoint, info)

	return args.Error(0)
}

// GetVTXO implements rounds.VTXOStore.
func (m *MockVTXOStore) GetVTXO(ctx context.Context,
	outpoint wire.OutPoint) (*rounds.VTXO, error) {

	args := m.Called(ctx, outpoint)

	if vtxo := args.Get(0); vtxo != nil {
		return vtxo.(*rounds.VTXO), args.Error(1)
	}

	return nil, args.Error(1)
}

// LockVTXO implements rounds.VTXOStore.
func (m *MockVTXOStore) LockVTXO(ctx context.Context, roundID rounds.RoundID,
	outpoints ...wire.OutPoint) error {

	args := m.Called(ctx, roundID, outpoints)

	return args.Error(0)
}

// UnlockVTXO implements rounds.VTXOStore.
func (m *MockVTXOStore) UnlockVTXO(ctx context.Context,
	roundID rounds.RoundID, outpoints ...wire.OutPoint) error {

	args := m.Called(ctx, roundID, outpoints)

	return args.Error(0)
}

// GetForfeitInfo implements rounds.VTXOStore.
func (m *MockVTXOStore) GetForfeitInfo(ctx context.Context,
	outpoint wire.OutPoint) (*rounds.ForfeitInfo, error) {

	args := m.Called(ctx, outpoint)

	if info := args.Get(0); info != nil {
		return info.(*rounds.ForfeitInfo), args.Error(1)
	}

	return nil, args.Error(1)
}

// UnlockStaleVTXOs implements rounds.VTXOStore.
func (m *MockVTXOStore) UnlockStaleVTXOs(ctx context.Context,
	activeRoundIDs []rounds.RoundID) error {

	args := m.Called(ctx, activeRoundIDs)

	return args.Error(0)
}

// SetupDefaultExpectations configures common expectations for successful VTXO
// persistence operations.
func (m *MockVTXOStore) SetupDefaultExpectations() {
	m.On("PersistVTXOs", mock.Anything, mock.Anything).Return(nil)
	m.On("MarkVTXOsLive", mock.Anything, mock.Anything).Return(nil)
	m.On("MarkVTXOForfeit", mock.Anything, mock.Anything,
		mock.Anything).Return(nil)
	m.On("GetVTXO", mock.Anything, mock.Anything).Return(nil, nil)
	m.On("LockVTXO", mock.Anything, mock.Anything,
		mock.Anything).Return(nil)
	m.On("UnlockVTXO", mock.Anything, mock.Anything,
		mock.Anything).Return(nil)
}

// Compile-time check that MockVTXOStore implements rounds.VTXOStore.
var _ rounds.VTXOStore = (*MockVTXOStore)(nil)

// MemoryVTXOStore is an in-memory implementation of rounds.VTXOStore that
// actually stores and retrieves VTXOs. Unlike MockVTXOStore which just returns
// nil for GetVTXO, this store maintains state across rounds, which is required
// for the forfeit flow where round N+1 needs to look up VTXOs created in round
// N.
type MemoryVTXOStore struct {
	// vtxos maps outpoint string to VTXO.
	vtxos map[string]*rounds.VTXO

	// lockedBy maps outpoint string to the round that locked it.
	lockedBy map[string]rounds.RoundID

	// byRoundID maps round ID to list of outpoint strings for that round.
	byRoundID map[rounds.RoundID][]string

	// forfeitInfos maps outpoint string to forfeit metadata.
	forfeitInfos map[string]*rounds.ForfeitInfo
}

// NewMemoryVTXOStore creates a new in-memory VTXO store.
func NewMemoryVTXOStore() *MemoryVTXOStore {
	return &MemoryVTXOStore{
		vtxos:        make(map[string]*rounds.VTXO),
		lockedBy:     make(map[string]rounds.RoundID),
		byRoundID:    make(map[rounds.RoundID][]string),
		forfeitInfos: make(map[string]*rounds.ForfeitInfo),
	}
}

// PersistVTXOs implements rounds.VTXOStore.
func (m *MemoryVTXOStore) PersistVTXOs(ctx context.Context,
	vtxos []*rounds.VTXO) error {

	for _, vtxo := range vtxos {
		key := vtxo.Outpoint.String()
		m.vtxos[key] = vtxo
		m.byRoundID[vtxo.RoundID] = append(
			m.byRoundID[vtxo.RoundID], key,
		)
	}

	return nil
}

// MarkVTXOsLive implements rounds.VTXOStore.
func (m *MemoryVTXOStore) MarkVTXOsLive(ctx context.Context,
	roundID rounds.RoundID) error {

	outpoints := m.byRoundID[roundID]
	for _, op := range outpoints {
		if vtxo, exists := m.vtxos[op]; exists {
			vtxo.Status = rounds.VTXOStatusLive
		}
	}

	return nil
}

// MarkVTXOForfeit implements rounds.VTXOStore.
func (m *MemoryVTXOStore) MarkVTXOForfeit(ctx context.Context,
	outpoint wire.OutPoint, info *rounds.ForfeitInfo) error {

	key := outpoint.String()
	if vtxo, exists := m.vtxos[key]; exists {
		vtxo.Status = rounds.VTXOStatusForfeited
	}

	// Store forfeit metadata.
	if info != nil {
		m.forfeitInfos[key] = info
	}

	return nil
}

// GetVTXO implements rounds.VTXOStore.
func (m *MemoryVTXOStore) GetVTXO(ctx context.Context,
	outpoint wire.OutPoint) (*rounds.VTXO, error) {

	key := outpoint.String()
	if vtxo, exists := m.vtxos[key]; exists {
		return vtxo, nil
	}

	return nil, nil
}

// GetForfeitInfo implements rounds.VTXOStore.
func (m *MemoryVTXOStore) GetForfeitInfo(ctx context.Context,
	outpoint wire.OutPoint) (*rounds.ForfeitInfo, error) {

	key := outpoint.String()
	if info, exists := m.forfeitInfos[key]; exists {
		return info, nil
	}

	return nil, nil
}

// LockVTXO implements rounds.VTXOStore.
func (m *MemoryVTXOStore) LockVTXO(ctx context.Context, roundID rounds.RoundID,
	outpoints ...wire.OutPoint) error {

	for _, op := range outpoints {
		key := op.String()
		m.lockedBy[key] = roundID
	}

	return nil
}

// UnlockVTXO implements rounds.VTXOStore.
func (m *MemoryVTXOStore) UnlockVTXO(ctx context.Context,
	roundID rounds.RoundID, outpoints ...wire.OutPoint) error {

	for _, op := range outpoints {
		key := op.String()
		if lockedRound, exists := m.lockedBy[key]; exists {
			if lockedRound == roundID {
				delete(m.lockedBy, key)
			}
		}
	}

	return nil
}

// UnlockStaleVTXOs implements rounds.VTXOStore.
func (m *MemoryVTXOStore) UnlockStaleVTXOs(ctx context.Context,
	activeRoundIDs []rounds.RoundID) error {

	// Build a map of active round IDs for fast lookup.
	activeRounds := make(map[rounds.RoundID]bool)
	for _, id := range activeRoundIDs {
		activeRounds[id] = true
	}

	// Unlock VTXOs locked by rounds not in the active list.
	toUnlock := []string{}
	for key, lockedRound := range m.lockedBy {
		if !activeRounds[lockedRound] {
			toUnlock = append(toUnlock, key)
		}
	}

	for _, key := range toUnlock {
		delete(m.lockedBy, key)
	}

	return nil
}

// Compile-time check that MemoryVTXOStore implements rounds.VTXOStore.
var _ rounds.VTXOStore = (*MemoryVTXOStore)(nil)
