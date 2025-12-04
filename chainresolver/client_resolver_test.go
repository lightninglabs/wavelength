package chainresolver

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog"
	btclogv2 "github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// Test Helpers
// -----------------------------------------------------------------------------

// mockSigner implements the Signer interface for testing.
type mockSigner struct {
	destAddr btcutil.Address
	signErr  error
	destErr  error
}

// SignTimeoutPath implements Signer.
func (m *mockSigner) SignTimeoutPath(ctx context.Context, tx *wire.MsgTx,
	vtxoOutput *wire.TxOut, csvTimeout uint32) (*wire.MsgTx, error) {

	if m.signErr != nil {
		return nil, m.signErr
	}

	// Add fake witnesses.
	for i := range tx.TxIn {
		tx.TxIn[i].Witness = wire.TxWitness{
			make([]byte, 64),
			make([]byte, 32),
		}
	}

	return tx, nil
}

// GetDestinationAddress implements Signer.
func (m *mockSigner) GetDestinationAddress(
	ctx context.Context) (btcutil.Address, error) {

	if m.destErr != nil {
		return nil, m.destErr
	}

	return m.destAddr, nil
}

// mockChainBackend implements chainsource.ChainBackend for testing.
type mockChainBackend struct {
	feeRate    btcutil.Amount
	bestHeight int32
	bestHash   chainhash.Hash

	confChan    chan *chainsource.TxConfirmation
	spendChan   chan *chainsource.SpendDetail
	epochChan   chan *chainsource.BlockEpoch
	epochCancel chan struct{}

	spendRegs    map[wire.OutPoint]bool
	blockRegs    int
	broadcastTxs []*wire.MsgTx
}

// newMockChainBackend creates a new mock chain backend.
func newMockChainBackend() *mockChainBackend {
	return &mockChainBackend{
		feeRate:     1000,
		bestHeight:  100,
		confChan:    make(chan *chainsource.TxConfirmation, 10),
		spendChan:   make(chan *chainsource.SpendDetail, 10),
		epochChan:   make(chan *chainsource.BlockEpoch, 10),
		epochCancel: make(chan struct{}, 10),
		spendRegs:   make(map[wire.OutPoint]bool),
	}
}

// EstimateFee implements ChainBackend.
func (m *mockChainBackend) EstimateFee(ctx context.Context,
	targetConf uint32) (btcutil.Amount, error) {

	return m.feeRate, nil
}

// BestBlock implements ChainBackend.
func (m *mockChainBackend) BestBlock(
	ctx context.Context) (int32, chainhash.Hash, error) {

	return m.bestHeight, m.bestHash, nil
}

// TestMempoolAccept implements ChainBackend.
func (m *mockChainBackend) TestMempoolAccept(ctx context.Context,
	tx *wire.MsgTx) (bool, string, error) {

	return true, "", nil
}

// BroadcastTx implements ChainBackend.
func (m *mockChainBackend) BroadcastTx(ctx context.Context, tx *wire.MsgTx,
	label string) error {

	m.broadcastTxs = append(m.broadcastTxs, tx)

	return nil
}

// RegisterConf implements ChainBackend.
func (m *mockChainBackend) RegisterConf(ctx context.Context,
	txid *chainhash.Hash, pkScript []byte, numConfs uint32,
	heightHint uint32) (*chainsource.ConfRegistration, error) {

	return &chainsource.ConfRegistration{
		Confirmed: m.confChan,
		Cancel:    func() {},
	}, nil
}

// RegisterSpend implements ChainBackend.
func (m *mockChainBackend) RegisterSpend(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte,
	heightHint uint32) (*chainsource.SpendRegistration, error) {

	m.spendRegs[*outpoint] = true

	return &chainsource.SpendRegistration{
		Spend:  m.spendChan,
		Cancel: func() { delete(m.spendRegs, *outpoint) },
	}, nil
}

// RegisterBlocks implements ChainBackend.
func (m *mockChainBackend) RegisterBlocks(
	ctx context.Context) (*chainsource.BlockRegistration, error) {

	m.blockRegs++

	return &chainsource.BlockRegistration{
		Epochs: m.epochChan,
		Cancel: func() {
			m.blockRegs--
			select {
			case m.epochCancel <- struct{}{}:
			default:
			}
		},
	}, nil
}

// Start implements ChainBackend.
func (m *mockChainBackend) Start() error {
	return nil
}

// Stop implements ChainBackend.
func (m *mockChainBackend) Stop() error {
	return nil
}

// testLogger is a simple logger for tests.
type testLogger struct{}

func (testLogger) Tracef(format string, params ...any)    {}
func (testLogger) Debugf(format string, params ...any)    {}
func (testLogger) Infof(format string, params ...any)     {}
func (testLogger) Warnf(format string, params ...any)     {}
func (testLogger) Errorf(format string, params ...any)    {}
func (testLogger) Criticalf(format string, params ...any) {}

func (testLogger) Trace(v ...any)    {}
func (testLogger) Debug(v ...any)    {}
func (testLogger) Info(v ...any)     {}
func (testLogger) Warn(v ...any)     {}
func (testLogger) Error(v ...any)    {}
func (testLogger) Critical(v ...any) {}

func (testLogger) TraceS(_ context.Context, _ string, _ ...any)          {}
func (testLogger) DebugS(_ context.Context, _ string, _ ...any)          {}
func (testLogger) InfoS(_ context.Context, _ string, _ ...any)           {}
func (testLogger) WarnS(_ context.Context, _ string, _ error, _ ...any)  {}
func (testLogger) ErrorS(_ context.Context, _ string, _ error, _ ...any) {}
func (testLogger) CriticalS(_ context.Context, _ string, _ error, _ ...any) {
}
func (l testLogger) Level() btclog.Level                 { return btclog.LevelOff }
func (testLogger) SetLevel(_ btclog.Level)               {}
func (l testLogger) SubSystem(_ string) btclogv2.Logger  { return l }
func (l testLogger) WithPrefix(_ string) btclogv2.Logger { return l }

// setupTestSystem sets up a test actor system with chainsource.
func setupTestSystem(
	t *testing.T,
) (*actor.ActorSystem, actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
], *mockChainBackend) {

	t.Helper()

	backend := newMockChainBackend()
	system := actor.NewActorSystem()

	chainSourceActor := chainsource.NewChainSourceActor(backend, system)
	chainSourceRef := chainsource.ChainSourceKey.Spawn(
		system, "chainsource", chainSourceActor,
	)

	return system, chainSourceRef, backend
}

// -----------------------------------------------------------------------------
// ClientResolverActor Tests
// -----------------------------------------------------------------------------

// TestClientResolverActorMonitorVTXO tests basic VTXO monitoring.
func TestClientResolverActorMonitorVTXO(t *testing.T) {
	t.Parallel()

	system, chainSourceRef, _ := setupTestSystem(t)
	defer func() { _ = system.Shutdown(t.Context()) }()

	signer := &mockSigner{}
	log := testLogger{}

	resolver := NewClientResolverActor(
		chainSourceRef, system, signer, log,
	)
	resolverRef := ClientResolverKey.Spawn(
		system, "client-resolver", resolver,
	)

	vtxoOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("vtxo")),
		Index: 0,
	}
	vtxoOutput := &wire.TxOut{Value: 10000, PkScript: []byte("vtxo_script")}

	req := &MonitorVTXORequest{
		VTXOOutpoint: vtxoOutpoint,
		VTXOOutput:   vtxoOutput,
		TreePath:     nil,
		ExitDelay:    144,
		HeightHint:   100,
		NotifyActor:  fn.None[actor.TellOnlyRef[ClientResolverEvent]](),
	}

	ctx := t.Context()
	future := resolverRef.Ask(ctx, req)
	result := future.Await(ctx)

	require.True(t, result.IsOk(), "expected ok result")

	resp, err := result.Unpack()
	require.NoError(t, err)

	monitorResp, ok := resp.(*MonitorVTXOResponse)
	require.True(t, ok)
	require.Contains(t, monitorResp.MonitorID, "vtxo-monitor")
}

// TestClientResolverActorMonitorVTXODuplicate tests duplicate monitoring.
func TestClientResolverActorMonitorVTXODuplicate(t *testing.T) {
	t.Parallel()

	system, chainSourceRef, _ := setupTestSystem(t)
	defer func() { _ = system.Shutdown(t.Context()) }()

	signer := &mockSigner{}
	log := testLogger{}

	resolver := NewClientResolverActor(
		chainSourceRef, system, signer, log,
	)
	resolverRef := ClientResolverKey.Spawn(
		system, "client-resolver", resolver,
	)

	vtxoOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("vtxo")),
		Index: 0,
	}
	vtxoOutput := &wire.TxOut{Value: 10000, PkScript: []byte("vtxo_script")}

	req := &MonitorVTXORequest{
		VTXOOutpoint: vtxoOutpoint,
		VTXOOutput:   vtxoOutput,
		TreePath:     nil,
		ExitDelay:    144,
		HeightHint:   100,
		NotifyActor:  fn.None[actor.TellOnlyRef[ClientResolverEvent]](),
	}

	ctx := t.Context()

	// First registration should succeed.
	future1 := resolverRef.Ask(ctx, req)
	result1 := future1.Await(ctx)
	require.True(t, result1.IsOk())

	// Second registration should fail.
	future2 := resolverRef.Ask(ctx, req)
	result2 := future2.Await(ctx)
	require.True(t, result2.IsErr())
	require.Contains(t, result2.Err().Error(), "already being monitored")
}

// TestClientResolverActorStopMonitorVTXO tests stopping VTXO monitoring.
func TestClientResolverActorStopMonitorVTXO(t *testing.T) {
	t.Parallel()

	system, chainSourceRef, _ := setupTestSystem(t)
	defer func() { _ = system.Shutdown(t.Context()) }()

	signer := &mockSigner{}
	log := testLogger{}

	resolver := NewClientResolverActor(
		chainSourceRef, system, signer, log,
	)
	resolverRef := ClientResolverKey.Spawn(
		system, "client-resolver", resolver,
	)

	vtxoOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("vtxo")),
		Index: 0,
	}
	vtxoOutput := &wire.TxOut{Value: 10000, PkScript: []byte("vtxo_script")}

	// Start monitoring.
	monitorReq := &MonitorVTXORequest{
		VTXOOutpoint: vtxoOutpoint,
		VTXOOutput:   vtxoOutput,
		TreePath:     nil,
		ExitDelay:    144,
		HeightHint:   100,
		NotifyActor:  fn.None[actor.TellOnlyRef[ClientResolverEvent]](),
	}

	ctx := t.Context()
	future := resolverRef.Ask(ctx, monitorReq)
	result := future.Await(ctx)
	require.True(t, result.IsOk())

	// Stop monitoring.
	stopReq := &StopMonitorVTXORequest{VTXOOutpoint: vtxoOutpoint}
	stopFuture := resolverRef.Ask(ctx, stopReq)
	stopResult := stopFuture.Await(ctx)

	require.True(t, stopResult.IsOk())

	stopResp, err := stopResult.Unpack()
	require.NoError(t, err)

	resp, ok := stopResp.(*StopMonitorVTXOResponse)
	require.True(t, ok)
	require.True(t, resp.Stopped)
}

// TestClientResolverActorStopNonexistentVTXO tests stopping unknown VTXO.
func TestClientResolverActorStopNonexistentVTXO(t *testing.T) {
	t.Parallel()

	system, chainSourceRef, _ := setupTestSystem(t)
	defer func() { _ = system.Shutdown(t.Context()) }()

	signer := &mockSigner{}
	log := testLogger{}

	resolver := NewClientResolverActor(
		chainSourceRef, system, signer, log,
	)
	resolverRef := ClientResolverKey.Spawn(
		system, "client-resolver", resolver,
	)

	ctx := t.Context()

	// Try to stop a VTXO that doesn't exist.
	stopReq := &StopMonitorVTXORequest{
		VTXOOutpoint: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("nonexistent")),
			Index: 0,
		},
	}
	stopFuture := resolverRef.Ask(ctx, stopReq)
	stopResult := stopFuture.Await(ctx)

	require.True(t, stopResult.IsOk())

	stopResp, err := stopResult.Unpack()
	require.NoError(t, err)

	resp, ok := stopResp.(*StopMonitorVTXOResponse)
	require.True(t, ok)
	require.False(t, resp.Stopped)
}

// -----------------------------------------------------------------------------
// Message Type Tests
// -----------------------------------------------------------------------------

// TestClientResolverMessageTypes tests message type implementations.
func TestClientResolverMessageTypes(t *testing.T) {
	t.Parallel()

	// Test ClientResolverMsg implementations.
	var _ ClientResolverMsg = (*MonitorVTXORequest)(nil)
	var _ ClientResolverMsg = (*StopMonitorVTXORequest)(nil)
	var _ ClientResolverMsg = (*InitiateUnrollRequest)(nil)
	var _ ClientResolverMsg = (*RecoverVTXORequest)(nil)
	var _ ClientResolverMsg = (*MonitorBoardingRequest)(nil)
	var _ ClientResolverMsg = (*internalVTXOSpentNotification)(nil)
	var _ ClientResolverMsg = (*internalCSVTimeoutNotification)(nil)

	// Test ClientResolverResp implementations.
	var _ ClientResolverResp = (*MonitorVTXOResponse)(nil)
	var _ ClientResolverResp = (*StopMonitorVTXOResponse)(nil)
	var _ ClientResolverResp = (*InitiateUnrollResponse)(nil)
	var _ ClientResolverResp = (*RecoverVTXOResponse)(nil)
	var _ ClientResolverResp = (*MonitorBoardingResponse)(nil)

	// Test ClientResolverEvent implementations.
	var _ ClientResolverEvent = VTXOSpentEvent{}
	var _ ClientResolverEvent = VTXOConfirmedEvent{}
	var _ ClientResolverEvent = CSVTimeoutReachedEvent{}
	var _ ClientResolverEvent = BoardingDepositEvent{}

	// Test vtxoMonitorMsg implementations.
	var _ vtxoMonitorMsg = (*startVTXOMonitorRequest)(nil)
	var _ vtxoMonitorMsg = (*stopVTXOMonitorRequest)(nil)
	var _ vtxoMonitorMsg = (*internalVTXOSpendEvent)(nil)
	var _ vtxoMonitorMsg = (*internalVTXOBlockEpoch)(nil)
}

// TestClientResolverMessageTypeNames tests MessageType returns correct names.
func TestClientResolverMessageTypeNames(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		msg      actor.Message
		expected string
	}{
		{&MonitorVTXORequest{}, "MonitorVTXORequest"},
		{&MonitorVTXOResponse{}, "MonitorVTXOResponse"},
		{&StopMonitorVTXORequest{}, "StopMonitorVTXORequest"},
		{&StopMonitorVTXOResponse{}, "StopMonitorVTXOResponse"},
		{&InitiateUnrollRequest{}, "InitiateUnrollRequest"},
		{&InitiateUnrollResponse{}, "InitiateUnrollResponse"},
		{&RecoverVTXORequest{}, "RecoverVTXORequest"},
		{&RecoverVTXOResponse{}, "RecoverVTXOResponse"},
		{&MonitorBoardingRequest{}, "MonitorBoardingRequest"},
		{&MonitorBoardingResponse{}, "MonitorBoardingResponse"},
		{VTXOSpentEvent{}, "VTXOSpentEvent"},
		{VTXOConfirmedEvent{}, "VTXOConfirmedEvent"},
		{CSVTimeoutReachedEvent{}, "CSVTimeoutReachedEvent"},
		{BoardingDepositEvent{}, "BoardingDepositEvent"},
		{&startVTXOMonitorRequest{}, "startVTXOMonitorRequest"},
		{&startVTXOMonitorResponse{}, "startVTXOMonitorResponse"},
		{&stopVTXOMonitorRequest{}, "stopVTXOMonitorRequest"},
		{&stopVTXOMonitorResponse{}, "stopVTXOMonitorResponse"},
		{&internalVTXOSpendEvent{}, "internalVTXOSpendEvent"},
		{&internalVTXOBlockEpoch{}, "internalVTXOBlockEpoch"},
		{
			&internalVTXOSpentNotification{},
			"internalVTXOSpentNotification",
		},
		{
			&internalCSVTimeoutNotification{},
			"internalCSVTimeoutNotification",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.expected, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.expected, tc.msg.MessageType())
		})
	}
}

// -----------------------------------------------------------------------------
// VTXOMonitorHandle Tests
// -----------------------------------------------------------------------------

// TestVTXOMonitorHandle tests the VTXOMonitorHandle struct.
func TestVTXOMonitorHandle(t *testing.T) {
	t.Parallel()

	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("vtxo")),
		Index: 0,
	}

	handle := &VTXOMonitorHandle{
		VTXOOutpoint:       outpoint,
		ServiceKeyName:     "vtxo-monitor.test",
		Config:             &MonitorVTXORequest{VTXOOutpoint: outpoint},
		ConfirmationHeight: 100,
		CSVTimeoutHeight:   244,
		TimeoutNotified:    false,
		Spent:              false,
		ExpectedSpend:      false,
	}

	require.Equal(t, outpoint, handle.VTXOOutpoint)
	require.Equal(t, "vtxo-monitor.test", handle.ServiceKeyName)
	require.Equal(t, int32(100), handle.ConfirmationHeight)
	require.Equal(t, int32(244), handle.CSVTimeoutHeight)
	require.False(t, handle.TimeoutNotified)
	require.False(t, handle.Spent)
	require.False(t, handle.ExpectedSpend)
}

// -----------------------------------------------------------------------------
// Event Tests
// -----------------------------------------------------------------------------

// TestVTXOSpentEvent tests the VTXOSpentEvent struct.
func TestVTXOSpentEvent(t *testing.T) {
	t.Parallel()

	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("vtxo")),
		Index: 0,
	}
	spendingTx := wire.NewMsgTx(2)

	event := VTXOSpentEvent{
		VTXOOutpoint:   outpoint,
		SpendingTx:     spendingTx,
		SpendingHeight: 150,
		ExpectedSpend:  false,
	}

	require.Equal(t, outpoint, event.VTXOOutpoint)
	require.Equal(t, spendingTx, event.SpendingTx)
	require.Equal(t, int32(150), event.SpendingHeight)
	require.False(t, event.ExpectedSpend)
}

// TestCSVTimeoutReachedEvent tests the CSVTimeoutReachedEvent struct.
func TestCSVTimeoutReachedEvent(t *testing.T) {
	t.Parallel()

	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("vtxo")),
		Index: 0,
	}

	event := CSVTimeoutReachedEvent{
		VTXOOutpoint:  outpoint,
		TimeoutHeight: 244,
	}

	require.Equal(t, outpoint, event.VTXOOutpoint)
	require.Equal(t, int32(244), event.TimeoutHeight)
}

// TestBoardingDepositEvent tests the BoardingDepositEvent struct.
func TestBoardingDepositEvent(t *testing.T) {
	t.Parallel()

	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("boarding")),
		Index: 0,
	}

	event := BoardingDepositEvent{
		BoardingAddress: nil, // Would be a real address in production.
		Outpoint:        outpoint,
		Amount:          btcutil.Amount(50000),
		Confirmations:   6,
	}

	require.Equal(t, outpoint, event.Outpoint)
	require.Equal(t, btcutil.Amount(50000), event.Amount)
	require.Equal(t, int32(6), event.Confirmations)
}

// -----------------------------------------------------------------------------
// Signer Interface Tests
// -----------------------------------------------------------------------------

// TestMockSignerTimeoutPath tests the mock signer.
func TestMockSignerTimeoutPath(t *testing.T) {
	t.Parallel()

	signer := &mockSigner{}
	tx := wire.NewMsgTx(3)
	tx.AddTxIn(&wire.TxIn{})

	vtxoOutput := &wire.TxOut{Value: 10000, PkScript: []byte("script")}

	signedTx, err := signer.SignTimeoutPath(
		t.Context(), tx, vtxoOutput, 144,
	)
	require.NoError(t, err)
	require.NotNil(t, signedTx)
	require.NotEmpty(t, signedTx.TxIn[0].Witness)
}

// -----------------------------------------------------------------------------
// Integration Tests
// -----------------------------------------------------------------------------

// TestClientResolverWithNotifications tests event notifications.
func TestClientResolverWithNotifications(t *testing.T) {
	t.Parallel()

	system, chainSourceRef, _ := setupTestSystem(t)
	defer func() { _ = system.Shutdown(t.Context()) }()

	signer := &mockSigner{}
	log := testLogger{}

	resolver := NewClientResolverActor(
		chainSourceRef, system, signer, log,
	)
	resolverRef := ClientResolverKey.Spawn(
		system, "client-resolver", resolver,
	)

	// Create notification channel.
	notifyRef := actor.NewChannelTellOnlyRef[ClientResolverEvent](
		"test-notify", 10,
	)

	vtxoOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("vtxo")),
		Index: 0,
	}
	vtxoOutput := &wire.TxOut{Value: 10000, PkScript: []byte("vtxo_script")}

	notifyOpt := fn.Some[actor.TellOnlyRef[ClientResolverEvent]](notifyRef)
	req := &MonitorVTXORequest{
		VTXOOutpoint: vtxoOutpoint,
		VTXOOutput:   vtxoOutput,
		TreePath:     nil,
		ExitDelay:    144,
		HeightHint:   100,
		NotifyActor:  notifyOpt,
	}

	ctx := t.Context()
	future := resolverRef.Ask(ctx, req)
	result := future.Await(ctx)

	require.True(t, result.IsOk())
}
