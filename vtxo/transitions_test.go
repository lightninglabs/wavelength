package vtxo

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tx"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestStateProperties verifies basic properties of all states.
func TestStateProperties(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	tests := []struct {
		name       string
		state      VTXOState
		isTerminal bool
	}{
		{
			name:       "LiveState",
			state:      &LiveState{VTXO: vtxo},
			isTerminal: false,
		},
		{
			name:       "PendingForfeitState",
			state:      &PendingForfeitState{VTXO: vtxo},
			isTerminal: false,
		},
		{
			name:       "ForfeitingState",
			state:      &ForfeitingState{VTXO: vtxo},
			isTerminal: false,
		},
		{
			name:       "SpendingState",
			state:      &SpendingState{VTXO: vtxo},
			isTerminal: false,
		},
		{
			name:       "SpentState",
			state:      &SpentState{VTXO: vtxo},
			isTerminal: true,
		},
		{
			name:       "ForfeitedState",
			state:      &ForfeitedState{VTXO: vtxo},
			isTerminal: true,
		},
		{
			name:       "UnilateralExitState",
			state:      &UnilateralExitState{VTXO: vtxo},
			isTerminal: true,
		},
		{
			name:       "FailedState",
			state:      &FailedState{VTXO: vtxo, Reason: "test"},
			isTerminal: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.isTerminal, tc.state.IsTerminal())
			require.NotEmpty(t, tc.state.String())
		})
	}
}

// TestLiveStateBlockEpochSafe verifies that LiveState stays live when expiry
// is safe.
func TestLiveStateBlockEpochSafe(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 1000
	vtxo.CreatedHeight = 100

	h.withState(&LiveState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	// Block height well before expiry - should stay in LiveState.
	evt := h.newBlockEpochEvent(200)
	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	assertState[*LiveState](h)
	require.Empty(t, h.outboxMessages, "no messages for safe expiry")
}

// TestLiveStateBlockEpochNeedsRefresh verifies that LiveState transitions to
// PendingForfeitState when approaching expiry threshold.
func TestLiveStateBlockEpochNeedsRefresh(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 1000
	vtxo.CreatedHeight = 100

	// Configure expiry so we're in the refresh window.
	h.withExpiryConfig(&ExpiryConfig{
		RefreshThresholdBlocks:  200,
		CriticalThresholdBlocks: 50,
		TreeDepthMultiplier:     1,
	})

	h.withState(&LiveState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	// Block height within refresh threshold (200 blocks before expiry).
	// BatchExpiry=1000, so at height 850 we're 150 blocks away (< 200).
	evt := h.newBlockEpochEvent(850)

	// Setup mock for status update.
	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint,
		VTXOStatusPendingForfeit,
	).Return(nil)

	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	assertState[*PendingForfeitState](h)

	// Should emit ForfeitRequest.
	assertOutboxContains[*ForfeitRequest](h)
}

// TestPendingForfeitEventFromLiveState verifies that a round-driven pending
// forfeit commit moves the VTXO into PendingForfeitState without emitting a
// round request back out. The round actor already owns the intent package in
// this path, so the VTXO only needs to update its availability state.
func TestPendingForfeitEventFromLiveState(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&LiveState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint,
		VTXOStatusPendingForfeit,
	).Return(nil)

	_, err := h.sendEvent(&round.PendingForfeitEvent{})
	require.NoError(t, err)

	assertState[*PendingForfeitState](h)
	require.Len(t, h.outboxMessages, 1)
	_, ok := h.outboxMessages[0].(*VTXOStatusUpdate)
	require.True(
		t, ok, "expected VTXOStatusUpdate, got %T",
		h.outboxMessages[0],
	)
}

// TestLiveStateBlockEpochCritical verifies that LiveState transitions to
// UnilateralExitState when critically close to expiry.
func TestLiveStateBlockEpochCritical(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 1000
	vtxo.CreatedHeight = 100

	// Configure expiry so we're in critical zone.
	h.withExpiryConfig(&ExpiryConfig{
		RefreshThresholdBlocks:  200,
		CriticalThresholdBlocks: 50,
		TreeDepthMultiplier:     1,
	})

	h.withState(&LiveState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	// Block height within critical threshold (50 blocks before expiry).
	// BatchExpiry=1000, so at height 970 we're 30 blocks away (< 50).
	evt := h.newBlockEpochEvent(970)

	// Setup mock for status update.
	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint,
		VTXOStatusUnilateralExit,
	).Return(nil)

	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	assertState[*UnilateralExitState](h)

	// Should emit ExpiringNotification (pointer type).
	assertOutboxContains[*ExpiringNotification](h)
}

// TestLiveStateForceUnroll verifies that LiveState transitions to
// UnilateralExitState on ForceUnrollEvent, emitting the same outbox as
// the critical expiry path.
func TestLiveStateForceUnroll(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 10000
	vtxo.CreatedHeight = 100

	h.withState(&LiveState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint,
		VTXOStatusUnilateralExit,
	).Return(nil)

	_, err := h.sendEvent(&ForceUnrollEvent{
		Reason: "manual unroll",
	})
	require.NoError(t, err)

	assertState[*UnilateralExitState](h)
	assertOutboxContains[*ExpiringNotification](h)
	assertOutboxContains[*VTXOStatusUpdate](h)
	assertOutboxContains[*VTXOTerminatedNotification](h)
}

// TestForfeitRequestFromLiveState verifies that LiveState transitions to
// ForfeitingState on ForfeitRequest from round actor.
func TestForfeitRequestFromLiveState(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.setupMockWalletForSigning()
	h.withState(&LiveState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	connectorOutpoint := h.newTestOutpoint()
	evt := &round.ForfeitRequestEvent{
		RoundID:               "round-123",
		ConnectorOutpoint:     connectorOutpoint,
		ConnectorPkScript:     []byte{0x51, 0x20},
		ConnectorAmount:       546,
		ServerForfeitPkScript: []byte{0x51, 0x20},
	}

	// Setup mock for status update.
	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint, VTXOStatusForfeiting,
	).Return(nil)

	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	state := assertState[*ForfeitingState](h)
	require.Equal(t, "round-123", state.NewRoundID)
	require.Equal(t, connectorOutpoint, state.ConnectorOutpoint)
}

// TestForfeitRequestFromPendingForfeit verifies that PendingForfeitState
// transitions to ForfeitingState on ForfeitRequest.
func TestForfeitRequestFromPendingForfeit(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.setupMockWalletForSigning()
	h.withState(&PendingForfeitState{
		VTXO:              vtxo,
		RequestedAtHeight: 800,
	})

	connectorOutpoint := h.newTestOutpoint()
	evt := &round.ForfeitRequestEvent{
		RoundID:               "round-456",
		ConnectorOutpoint:     connectorOutpoint,
		ConnectorPkScript:     []byte{0x51, 0x20},
		ConnectorAmount:       546,
		ServerForfeitPkScript: []byte{0x51, 0x20},
	}

	// Setup mock for status update.
	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint, VTXOStatusForfeiting,
	).Return(nil)

	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	state := assertState[*ForfeitingState](h)
	require.Equal(t, "round-456", state.NewRoundID)
}

// TestPendingForfeitCriticalExpiry verifies that PendingForfeitState
// transitions to UnilateralExitState if expiry becomes critical while waiting.
func TestPendingForfeitCriticalExpiry(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 1000

	h.withExpiryConfig(&ExpiryConfig{
		RefreshThresholdBlocks:  200,
		CriticalThresholdBlocks: 50,
		TreeDepthMultiplier:     1,
	})

	h.withState(&PendingForfeitState{
		VTXO:              vtxo,
		RequestedAtHeight: 800,
	})

	// Block height within critical threshold while waiting for refresh.
	evt := h.newBlockEpochEvent(970)

	// Setup mock for status update.
	h.store.On(
		"UpdateVTXOStatus",
		h.ctx,
		vtxo.Outpoint,
		VTXOStatusUnilateralExit,
	).Return(nil)

	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	assertState[*UnilateralExitState](h)
}

// TestForfeitingStateConfirmed verifies that ForfeitingState transitions to
// ForfeitedState on confirmation.
func TestForfeitingStateConfirmed(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&ForfeitingState{
		VTXO:              vtxo,
		NewRoundID:        "round-789",
		ConnectorOutpoint: h.newTestOutpoint(),
	})

	var commitmentTxID chainhash.Hash
	copy(commitmentTxID[:], []byte("commitment-tx-hash"))

	evt := &round.ForfeitConfirmedEvent{
		CommitmentTxID: commitmentTxID,
		BlockHeight:    1100,
	}

	// Setup mock for marking forfeited.
	h.store.On(
		"MarkForfeited", h.ctx, vtxo.Outpoint, commitmentTxID,
	).Return(nil)

	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	state := assertState[*ForfeitedState](h)
	require.Equal(t, "round-789", state.NewRoundID)
	require.Equal(t, commitmentTxID, state.CommitmentTxID)

	// Should emit termination notification.
	assertOutboxContains[*VTXOTerminatedNotification](h)
}

// TestTerminalStatesSelfLoop verifies that terminal states self-loop on events.
func TestTerminalStatesSelfLoop(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	terminalStates := []VTXOState{
		&SpentState{VTXO: vtxo},
		&ForfeitedState{VTXO: vtxo},
		&UnilateralExitState{VTXO: vtxo},
		&FailedState{VTXO: vtxo, Reason: "test"},
	}

	for _, state := range terminalStates {
		t.Run(state.String(), func(t *testing.T) {
			h.withState(state)

			// Send block epoch - should be no-op.
			evt := h.newBlockEpochEvent(500)
			_, err := h.sendEvent(evt)
			require.NoError(t, err)

			// Should still be in same state.
			require.True(t, h.currentState.IsTerminal())
		})
	}
}

// TestExpiryStatusDetermination verifies expiry calculation logic.
// Config: RefreshThreshold=200, CriticalThreshold=50, TreeDepthMultiplier=10
// Given VTXO: BatchExpiry=1000, TreeDepth=3, RelativeExpiry=144
// Calculated thresholds:
//   - Critical = max(50, 3*10+144) = max(50, 174) = 174
//   - Refresh = max(200, 174+0) = 200 (MinRefreshBuffer defaults to 0)
func TestExpiryStatusDetermination(t *testing.T) {
	t.Parallel()

	cfg := &ExpiryConfig{
		RefreshThresholdBlocks:  200,
		CriticalThresholdBlocks: 50,
		TreeDepthMultiplier:     10,
	}

	vtxo := &Descriptor{
		Outpoint:       wire.OutPoint{},
		BatchExpiry:    1000,
		Ancestry:       []Ancestry{{TreeDepth: 3}},
		RelativeExpiry: 144,
	}

	tests := []struct {
		name     string
		height   int32
		expected ExpiryStatus
	}{
		{
			name:     "well before refresh threshold",
			height:   500,
			expected: ExpiryStatusSafe,
		},
		{
			// Height 810: remaining=190 < 200 (refresh) but > 174
			// (critical), so NeedsRefresh.
			name:     "within refresh threshold",
			height:   810,
			expected: ExpiryStatusNeedsRefresh,
		},
		{
			// Height 850: remaining=150 < 174 (critical).
			name:     "within critical threshold",
			height:   850,
			expected: ExpiryStatusCritical,
		},
		{
			name:     "past expiry",
			height:   1001,
			expected: ExpiryStatusExpired,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status := cfg.CheckExpiry(vtxo, tc.height)
			require.Equal(t, tc.expected, status)
		})
	}
}

// TestBlocksUntilExpiry verifies block counting logic.
func TestBlocksUntilExpiry(t *testing.T) {
	t.Parallel()

	vtxo := &Descriptor{
		BatchExpiry: 1000,
	}

	tests := []struct {
		name     string
		height   int32
		expected int32
	}{
		{
			name:     "500 blocks before",
			height:   500,
			expected: 500,
		},
		{
			name:     "at expiry",
			height:   1000,
			expected: 0,
		},
		{
			name:     "past expiry",
			height:   1100,
			expected: -100,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			blocks := BlocksUntilExpiry(vtxo, tc.height)
			require.Equal(t, tc.expected, blocks)
		})
	}
}

// TestForfeitRequestRealSigning verifies that the forfeit flow produces
// valid cryptographic signatures using real signing operations.
func TestForfeitRequestRealSigning(t *testing.T) {
	t.Parallel()

	h := newRealVTXOSigningHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&LiveState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	connectorOutpoint := h.newTestOutpoint()
	connectorOutput := h.newTestConnectorOutput()
	serverForfeitScript := h.newServerForfeitScript()

	evt := &round.ForfeitRequestEvent{
		RoundID:               "round-real-sig-001",
		ConnectorOutpoint:     connectorOutpoint,
		ConnectorPkScript:     connectorOutput.PkScript,
		ConnectorAmount:       connectorOutput.Value,
		ServerForfeitPkScript: serverForfeitScript,
	}

	// Setup mock for status update.
	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint, VTXOStatusForfeiting,
	).Return(nil)

	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	// Verify state transition.
	state := assertStateReal[*ForfeitingState](h)
	require.Equal(t, "round-real-sig-001", state.NewRoundID)
	require.Equal(t, connectorOutpoint, state.ConnectorOutpoint)

	// Get the emitted forfeit signature submission.
	submission := assertOutboxContainsReal[*ForfeitSignatureSubmission](h)

	// Verify the forfeit tx structure is valid.
	err = tx.ValidateForfeitTx(submission.ForfeitTx, tx.ForfeitTxParams{
		VTXOOutpoint:        vtxo.Outpoint,
		ConnectorOutpoint:   connectorOutpoint,
		ServerForfeitScript: serverForfeitScript,
		ExpectedAmount: btcutil.Amount(
			int64(vtxo.Amount) + connectorOutput.Value,
		),
	})
	require.NoError(t, err)

	// Verify the signature is non-nil and serializes to 64 bytes (Schnorr).
	require.NotNil(t, submission.Signature)
	require.Len(
		t, submission.Signature.Serialize(), 64,
		"signature should be 64 bytes",
	)
}

// TestForfeitingStateCriticalExpiry verifies that ForfeitingState transitions
// to UnilateralExitState if critical expiry is reached while waiting for
// forfeit confirmation.
func TestForfeitingStateCriticalExpiry(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 1000

	h.withExpiryConfig(&ExpiryConfig{
		RefreshThresholdBlocks:  200,
		CriticalThresholdBlocks: 50,
		TreeDepthMultiplier:     1,
	})

	forfeitTx := wire.NewMsgTx(2)
	h.withState(&ForfeitingState{
		VTXO:              vtxo,
		NewRoundID:        "round-stalled",
		ConnectorOutpoint: h.newTestOutpoint(),
		ForfeitTxID:       forfeitTx.TxHash(),
		ForfeitTx:         forfeitTx,
	})

	// Block height within critical threshold while waiting for forfeit
	// confirmation. Should escalate to chain resolver.
	evt := h.newBlockEpochEvent(970)

	// Setup mock for status update.
	h.store.On(
		"UpdateVTXOStatus",
		h.ctx,
		vtxo.Outpoint,
		VTXOStatusUnilateralExit,
	).Return(nil)

	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	assertState[*UnilateralExitState](h)

	// Should emit ExpiringNotification and VTXOTerminatedNotification.
	assertOutboxContains[*ExpiringNotification](h)
	assertOutboxContains[*VTXOTerminatedNotification](h)
}

// TestForfeitSignedEventPreservesForfeitTx verifies that handling
// ForfeitSignedEvent preserves the ForfeitTx field for crash recovery.
func TestForfeitSignedEventPreservesForfeitTx(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	// Create a forfeit tx to track.
	forfeitTx := wire.NewMsgTx(2)
	forfeitTx.AddTxIn(&wire.TxIn{})
	forfeitTx.AddTxOut(&wire.TxOut{Value: 1000})

	var newTxID chainhash.Hash
	copy(newTxID[:], []byte("updated-forfeit-txid"))

	h.withState(&ForfeitingState{
		VTXO:              vtxo,
		NewRoundID:        "round-123",
		ConnectorOutpoint: h.newTestOutpoint(),
		ForfeitTxID:       forfeitTx.TxHash(),
		ForfeitTx:         forfeitTx,
	})

	// Send ForfeitSignedEvent with a new txid.
	evt := &ForfeitSignedEvent{
		ForfeitTxID: newTxID,
	}

	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	// Should stay in ForfeitingState.
	state := assertState[*ForfeitingState](h)

	// ForfeitTx should be preserved for crash recovery.
	require.NotNil(t, state.ForfeitTx, "ForfeitTx must be preserved")
	require.Equal(t, forfeitTx, state.ForfeitTx)

	// ForfeitTxID should be updated from the event.
	require.Equal(t, newTxID, state.ForfeitTxID)
}

// TestDetermineRefreshUrgencyWithDynamicThresholds verifies that urgency
// calculation uses dynamic thresholds based on VTXO tree depth and CSV delay.
func TestDetermineRefreshUrgencyWithDynamicThresholds(t *testing.T) {
	t.Parallel()

	cfg := &ExpiryConfig{
		RefreshThresholdBlocks:  200,
		CriticalThresholdBlocks: 50,
		MinRefreshBuffer:        72,
		TreeDepthMultiplier:     10,
	}

	// VTXO with deep tree and large CSV delay.
	// Dynamic critical = max(50, 5*10 + 144) = max(50, 194) = 194
	// Dynamic refresh = max(200, 194 + 72) = 266
	// Note: For deep VTXOs, there's no "elevated" zone because
	// critical (194) > half of refresh (133), so we go straight
	// from normal to critical.
	deepVTXO := &Descriptor{
		Outpoint:       wire.OutPoint{},
		BatchExpiry:    1000,
		Ancestry:       []Ancestry{{TreeDepth: 5}},
		RelativeExpiry: 144,
	}

	// VTXO with shallow tree and small CSV delay.
	// Dynamic critical = max(50, 1*10 + 24) = max(50, 34) = 50
	// Dynamic refresh = max(200, 50 + 72) = 200
	// Here we have elevated zone: blocks in (50, 100].
	shallowVTXO := &Descriptor{
		Outpoint:       wire.OutPoint{},
		BatchExpiry:    1000,
		Ancestry:       []Ancestry{{TreeDepth: 1}},
		RelativeExpiry: 24,
	}

	tests := []struct {
		name     string
		vtxo     *Descriptor
		blocks   int32
		expected RefreshUrgency
	}{
		{
			// Deep VTXO: critical=194, blocks=180 < 194.
			name:     "deep VTXO at critical",
			vtxo:     deepVTXO,
			blocks:   180,
			expected: RefreshUrgencyCritical,
		},
		{
			// For deep VTXO: blocks=200 > critical(194), so not
			// critical. Half of refresh = 133. Since 200 > 133,
			// it's normal (no elevated zone for deep VTXOs).
			name:     "deep VTXO normal near critical",
			vtxo:     deepVTXO,
			blocks:   200,
			expected: RefreshUrgencyNormal,
		},
		{
			// For deep VTXO: blocks=250 > 133 (half of 266).
			name:     "deep VTXO normal well above",
			vtxo:     deepVTXO,
			blocks:   250,
			expected: RefreshUrgencyNormal,
		},
		{
			// Shallow VTXO: critical=50, blocks=40 < 50.
			name:     "shallow VTXO at critical",
			vtxo:     shallowVTXO,
			blocks:   40,
			expected: RefreshUrgencyCritical,
		},
		{
			// Shallow VTXO: refresh=200, half=100.
			// 80 > 50 (not critical) but 80 < 100 (elevated).
			name:     "shallow VTXO elevated",
			vtxo:     shallowVTXO,
			blocks:   80,
			expected: RefreshUrgencyElevated,
		},
		{
			// For shallow VTXO: blocks=150 > 100 (half of 200).
			name:     "shallow VTXO normal",
			vtxo:     shallowVTXO,
			blocks:   150,
			expected: RefreshUrgencyNormal,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			urgency := cfg.DetermineRefreshUrgency(
				tc.vtxo, tc.blocks,
			)
			require.Equal(t, tc.expected, urgency)
		})
	}
}

// TestForfeitConfirmedEventIncludesForfeitTx verifies that the
// ForfeitConfirmedEvent transition includes the ForfeitTx in the status update
// for persistence.
func TestForfeitConfirmedEventIncludesForfeitTx(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	// Create a forfeit tx.
	forfeitTx := wire.NewMsgTx(2)
	forfeitTx.AddTxIn(&wire.TxIn{})
	forfeitTx.AddTxOut(&wire.TxOut{Value: 1000})

	h.withState(&ForfeitingState{
		VTXO:              vtxo,
		NewRoundID:        "round-789",
		ConnectorOutpoint: h.newTestOutpoint(),
		ForfeitTxID:       forfeitTx.TxHash(),
		ForfeitTx:         forfeitTx,
	})

	var commitmentTxID chainhash.Hash
	copy(commitmentTxID[:], []byte("commitment-tx-hash"))

	evt := &ForfeitConfirmedEvent{
		CommitmentTxID: commitmentTxID,
		BlockHeight:    1100,
	}

	// Setup mock for marking forfeited.
	h.store.On(
		"MarkForfeited", h.ctx, vtxo.Outpoint, commitmentTxID,
	).Return(nil)

	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	// Verify the status update includes ForfeitTx.
	var statusUpdate *VTXOStatusUpdate
	for _, msg := range h.outboxMessages {
		if su, ok := msg.(*VTXOStatusUpdate); ok {
			statusUpdate = su
			break
		}
	}
	require.NotNil(t, statusUpdate, "should emit VTXOStatusUpdate")
	require.NotNil(t, statusUpdate.ForfeitTx,
		"VTXOStatusUpdate should include ForfeitTx for persistence")
	require.Equal(t, forfeitTx, statusUpdate.ForfeitTx)
}

// =============================================================================
// Spend reservation and release tests
// =============================================================================

// TestSpendReserveFromLiveState verifies that LiveState transitions to
// SpendingState on SpendReserveEvent.
func TestSpendReserveFromLiveState(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&LiveState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint,
		VTXOStatusSpending,
	).Return(nil)

	_, err := h.sendEvent(&round.SpendReserveEvent{})
	require.NoError(t, err)

	state := assertState[*SpendingState](h)
	require.Equal(t, vtxo, state.VTXO)
	require.Equal(t, int32(100), state.LastCheckedHeight)

	// Should emit VTXOStatusUpdate with Spending status.
	su := assertOutboxContains[*VTXOStatusUpdate](h)
	require.Equal(t, VTXOStatusSpending, su.NewStatus)
}

// TestSpendReserveRejectedFromPendingForfeit verifies that PendingForfeitState
// rejects SpendReserveEvent — a VTXO committed to cooperative consumption
// cannot be claimed for OOR spending.
func TestSpendReserveRejectedFromPendingForfeit(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&PendingForfeitState{
		VTXO:              vtxo,
		RequestedAtHeight: 800,
	})

	_, err := h.sendEvent(&round.SpendReserveEvent{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot reserve for spend")
}

// TestSpendReserveRejectedFromSpending verifies that SpendingState rejects
// duplicate SpendReserveEvent.
func TestSpendReserveRejectedFromSpending(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&SpendingState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	_, err := h.sendEvent(&round.SpendReserveEvent{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "already reserved for spend")
}

// TestSpendReleasedFromSpendingState verifies that SpendingState transitions
// back to LiveState on SpendReleasedEvent.
func TestSpendReleasedFromSpendingState(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&SpendingState{
		VTXO:              vtxo,
		LastCheckedHeight: 200,
	})

	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint, VTXOStatusLive,
	).Return(nil)

	_, err := h.sendEvent(&round.SpendReleasedEvent{})
	require.NoError(t, err)

	state := assertState[*LiveState](h)
	require.Equal(t, vtxo, state.VTXO)

	// LastCheckedHeight should be preserved through the release.
	require.Equal(t, int32(200), state.LastCheckedHeight)

	su := assertOutboxContains[*VTXOStatusUpdate](h)
	require.Equal(t, VTXOStatusLive, su.NewStatus)
}

// TestSpendCompletedFromSpendingState verifies that SpendingState transitions
// to terminal SpentState on SpendCompletedEvent.
func TestSpendCompletedFromSpendingState(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&SpendingState{
		VTXO:              vtxo,
		LastCheckedHeight: 200,
	})

	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint, VTXOStatusSpent,
	).Return(nil)

	_, err := h.sendEvent(&round.SpendCompletedEvent{})
	require.NoError(t, err)

	state := assertState[*SpentState](h)
	require.Equal(t, vtxo, state.VTXO)
	require.True(t, state.IsTerminal())

	// Should emit status update and termination notification.
	su := assertOutboxContains[*VTXOStatusUpdate](h)
	require.Equal(t, VTXOStatusSpent, su.NewStatus)

	tn := assertOutboxContains[*VTXOTerminatedNotification](h)
	require.Equal(t, "Spent", tn.FinalState)
}

// TestSpendingStateCriticalExpiry verifies that SpendingState transitions to
// UnilateralExitState if critical expiry is reached while the VTXO is reserved
// for an OOR spend. Safety must win over the pending spend.
func TestSpendingStateCriticalExpiry(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 1000

	h.withExpiryConfig(&ExpiryConfig{
		RefreshThresholdBlocks:  200,
		CriticalThresholdBlocks: 50,
		TreeDepthMultiplier:     1,
	})

	h.withState(&SpendingState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	// Block height within critical threshold.
	evt := h.newBlockEpochEvent(970)

	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint,
		VTXOStatusUnilateralExit,
	).Return(nil)

	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	assertState[*UnilateralExitState](h)
	assertOutboxContains[*ExpiringNotification](h)
	assertOutboxContains[*VTXOTerminatedNotification](h)
}

// TestSpendingStateSafeBlockEpoch verifies that SpendingState stays in
// SpendingState when expiry is safe and updates LastCheckedHeight.
func TestSpendingStateSafeBlockEpoch(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.BatchExpiry = 1000
	vtxo.CreatedHeight = 100

	h.withState(&SpendingState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	evt := h.newBlockEpochEvent(200)
	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	state := assertState[*SpendingState](h)
	require.Equal(t, int32(200), state.LastCheckedHeight)
	require.Empty(t, h.outboxMessages)
}

// TestPendingForfeitRejectedFromSpending verifies that SpendingState rejects
// PendingForfeitEvent — a VTXO reserved for OOR spend cannot be claimed for
// cooperative forfeit.
func TestPendingForfeitRejectedFromSpending(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&SpendingState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	_, err := h.sendEvent(&round.PendingForfeitEvent{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot accept pending forfeit")
}

// TestForfeitReleasedFromPendingForfeit verifies that PendingForfeitState
// transitions back to LiveState on ForfeitReleasedEvent.
func TestForfeitReleasedFromPendingForfeit(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&PendingForfeitState{
		VTXO:              vtxo,
		RequestedAtHeight: 800,
	})

	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint, VTXOStatusLive,
	).Return(nil)

	_, err := h.sendEvent(&round.ForfeitReleasedEvent{})
	require.NoError(t, err)

	state := assertState[*LiveState](h)
	require.Equal(t, vtxo, state.VTXO)

	su := assertOutboxContains[*VTXOStatusUpdate](h)
	require.Equal(t, VTXOStatusLive, su.NewStatus)
}

// TestSpendingStateResumeStaysInSpending verifies that SpendingState stays in
// SpendingState on ResumeVTXOEvent. The OOR session will resume and later
// release or complete the claim.
func TestSpendingStateResumeStaysInSpending(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&SpendingState{
		VTXO:              vtxo,
		LastCheckedHeight: 200,
	})

	_, err := h.sendEvent(&round.ResumeVTXOEvent{})
	require.NoError(t, err)

	assertState[*SpendingState](h)
	require.Empty(t, h.outboxMessages)
}

// TestSpendingStateFailedEvent verifies that SpendingState transitions to
// FailedState on VTXOFailedEvent.
func TestSpendingStateFailedEvent(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&SpendingState{
		VTXO:              vtxo,
		LastCheckedHeight: 200,
	})

	_, err := h.sendEvent(&round.VTXOFailedEvent{
		Reason:      "test failure",
		Recoverable: false,
	})
	require.NoError(t, err)

	state := assertState[*FailedState](h)
	require.Equal(t, "test failure", state.Reason)
}

// TestSpendingStateForceUnroll verifies that SpendingState escalates to
// UnilateralExitState on ForceUnrollEvent, emitting the same outbox shape
// as the critical-expiry branch so manual and automatic exits converge on
// a single chain resolver seam.
func TestSpendingStateForceUnroll(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&SpendingState{
		VTXO:              vtxo,
		LastCheckedHeight: 200,
	})

	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint,
		VTXOStatusUnilateralExit,
	).Return(nil)

	_, err := h.sendEvent(&ForceUnrollEvent{
		Reason: "manual unroll",
	})
	require.NoError(t, err)

	assertState[*UnilateralExitState](h)
	assertOutboxContains[*ExpiringNotification](h)
	assertOutboxContains[*VTXOStatusUpdate](h)
	assertOutboxContains[*VTXOTerminatedNotification](h)
}

// TestForfeitingStateForceUnroll verifies that ForfeitingState escalates to
// UnilateralExitState on ForceUnrollEvent so an in-flight forfeit does not
// swallow a manual unroll request.
func TestForfeitingStateForceUnroll(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&ForfeitingState{
		VTXO:       vtxo,
		NewRoundID: "round-123",
	})

	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint,
		VTXOStatusUnilateralExit,
	).Return(nil)

	_, err := h.sendEvent(&ForceUnrollEvent{
		Reason: "manual unroll",
	})
	require.NoError(t, err)

	assertState[*UnilateralExitState](h)
	assertOutboxContains[*ExpiringNotification](h)
	assertOutboxContains[*VTXOStatusUpdate](h)
	assertOutboxContains[*VTXOTerminatedNotification](h)
}

// TestForfeitSignatureValidity verifies that forfeit signatures produced by
// the VTXO FSM can actually spend the VTXO output when combined with the
// operator's signature.
func TestForfeitSignatureValidity(t *testing.T) {
	t.Parallel()

	h := newRealVTXOSigningHarness(t)
	vtxo := h.newTestDescriptor()

	h.withState(&LiveState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	connectorOutpoint := h.newTestOutpoint()
	connectorOutput := h.newTestConnectorOutput()
	serverForfeitScript := h.newServerForfeitScript()

	evt := &round.ForfeitRequestEvent{
		RoundID:               "round-verify-001",
		ConnectorOutpoint:     connectorOutpoint,
		ConnectorPkScript:     connectorOutput.PkScript,
		ConnectorAmount:       connectorOutput.Value,
		ServerForfeitPkScript: serverForfeitScript,
	}

	h.store.On(
		"UpdateVTXOStatus", h.ctx, vtxo.Outpoint, VTXOStatusForfeiting,
	).Return(nil)

	_, err := h.sendEvent(evt)
	require.NoError(t, err)

	submission := assertOutboxContainsReal[*ForfeitSignatureSubmission](h)
	forfeitTx := submission.ForfeitTx

	// Build the VTXO output for sighash computation.
	vtxoOutput := &wire.TxOut{
		Value:    int64(vtxo.Amount),
		PkScript: vtxo.PkScript,
	}

	// Create spend contexts for prevout fetcher.
	vtxoCtx := &tx.VTXOSpendContext{
		Outpoint:  vtxo.Outpoint,
		Output:    vtxoOutput,
		TapScript: vtxo.TapScript,
	}
	connectorCtx := &tx.ConnectorSpendContext{
		Outpoint: connectorOutpoint,
		Output:   connectorOutput,
	}

	prevFetcher, err := tx.NewForfeitPrevOutFetcher(vtxoCtx, connectorCtx)
	require.NoError(t, err)

	sigHashes := txscript.NewTxSigHashes(forfeitTx, prevFetcher)

	// Get the spend info for the collaborative path.
	spendInfo, err := arkscript.NewVTXOSpendInfoFromPolicy(
		vtxo.ClientKey.PubKey, vtxo.OperatorKey,
		vtxo.RelativeExpiry, 0,
	)
	require.NoError(t, err)

	// Sign with operator to get second signature.
	operatorKeyDesc := keychain.KeyDescriptor{
		PubKey: h.operatorPubKey,
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamilyMultiSig,
			Index:  0,
		},
	}

	operatorSignDesc, _, err := tx.NewVTXOCollabSignDescriptor(
		vtxoCtx, operatorKeyDesc, tx.ForfeitVTXOInputIndex,
		sigHashes, prevFetcher,
	)
	require.NoError(t, err)

	operatorSig, err := h.operatorSigner.SignOutputRaw(
		forfeitTx, operatorSignDesc,
	)
	require.NoError(t, err)

	// The client signature is already parsed as *schnorr.Signature.
	clientSig := submission.Signature

	// Build complete witness for collaborative spend.
	witness, err := spendInfo.CollabWitness(
		clientSig, operatorSig,
	)
	require.NoError(t, err)

	forfeitTx.TxIn[tx.ForfeitVTXOInputIndex].Witness = witness

	// Verify the VTXO input can be spent using txscript.NewEngine.
	engine, err := txscript.NewEngine(
		vtxoOutput.PkScript, forfeitTx, tx.ForfeitVTXOInputIndex,
		txscript.StandardVerifyFlags, nil, sigHashes,
		vtxoOutput.Value, prevFetcher,
	)
	require.NoError(t, err)

	err = engine.Execute()
	require.NoError(t, err, "VTXO input signature verification failed")
}
