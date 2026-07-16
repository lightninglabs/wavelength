package virtualchannel

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/stretchr/testify/require"
)

func TestReceiveChannelTransitionRequiresConfirmation(t *testing.T) {
	require.NoError(
		t, ValidateTransition(
			KindReceiveChannel, StatusRequested,
			StatusRoundRequested,
		),
	)
	require.NoError(
		t, ValidateTransition(
			KindReceiveChannel, StatusRoundRequested,
			StatusFundingBound,
		),
	)
	require.NoError(
		t, ValidateTransition(
			KindReceiveChannel, StatusLNDNegotiating,
			StatusFundingVerified,
		),
	)
	require.NoError(
		t, ValidateTransition(
			KindReceiveChannel, StatusFundingVerified,
			StatusBackingArmed,
		),
	)
	require.Error(
		t, ValidateTransition(
			KindReceiveChannel, StatusLNDNegotiating,
			StatusBackingArmed,
		),
	)
	require.Error(
		t, ValidateTransition(
			KindReceiveChannel, StatusBackingArmed, StatusActive,
		),
	)
	require.NoError(
		t, ValidateTransition(
			KindReceiveChannel, StatusBackingArmed,
			StatusRoundConfirmed,
		),
	)
	require.NoError(
		t, ValidateTransition(
			KindReceiveChannel, StatusRoundConfirmed, StatusActive,
		),
	)
	require.NoError(
		t, ValidateTransition(
			KindReceiveChannel, StatusBackingArmed, StatusClosing,
		),
	)
	require.Error(
		t, ValidateTransition(
			KindReceiveChannel, StatusRoundConfirmed, StatusFailed,
		),
	)
	require.Error(
		t, ValidateTransition(
			KindReceiveChannel, StatusBackingArmed, StatusFailed,
		),
	)
}

func TestPromotedVTXOSkipsRoundConfirmation(t *testing.T) {
	require.NoError(
		t, ValidateTransition(
			KindPromoteVTXO, StatusBackingArmed, StatusActive,
		),
	)
	require.Error(
		t, ValidateTransition(
			KindPromoteVTXO, StatusBackingArmed,
			StatusRoundConfirmed,
		),
	)
	require.Error(
		t, ValidateTransition(
			KindPromoteVTXO, StatusBackingArmed, StatusFailed,
		),
	)
}

func TestPublishedFundingRemainsRoutable(t *testing.T) {
	require.NoError(
		t, ValidateTransition(
			KindReceiveChannel, StatusActive,
			StatusFundingPublished,
		),
	)
	require.NoError(
		t, ValidateTransition(
			KindReceiveChannel, StatusMaterializing,
			StatusFundingPublished,
		),
	)
	require.NoError(
		t, ValidateTransition(
			KindReceiveChannel, StatusFundingPublished,
			StatusClosing,
		),
	)
	require.True(t, IsRoutableStatus(StatusActive))
	require.True(t, IsRoutableStatus(StatusFundingPublished))
	require.False(t, IsRoutableStatus(StatusMaterializing))
	require.True(t, HasArmedBacking(StatusFundingPublished))
}

func TestReceiveChannelStartsOperatorOwned(t *testing.T) {
	require.NoError(
		t, ValidateInitialBalances(
			KindReceiveChannel, RoleOperator, 100_000, 100_000, 0,
		),
	)
	require.NoError(
		t, ValidateInitialBalances(
			KindReceiveChannel, RoleClient, 100_000, 0, 100_000,
		),
	)
	require.ErrorContains(
		t, ValidateInitialBalances(
			KindReceiveChannel, RoleClient, 100_000, 1, 99_999,
		),
		"fully owned by the operator",
	)
	require.ErrorContains(
		t, ValidateInitialBalances(
			KindReceiveChannel, RoleOperator, 100_000, 99_999, 1,
		),
		"fully owned by the operator",
	)
}

func TestInitialBalancesRejectExcess(t *testing.T) {
	require.ErrorContains(
		t, ValidateInitialBalances(
			KindPromoteVTXO, RoleClient, btcutil.MaxSatoshi,
			btcutil.MaxSatoshi, btcutil.MaxSatoshi,
		),
		"exceed capacity",
	)
}

func TestInitialBalancesRejectMoneySupplyOverflow(t *testing.T) {
	require.ErrorContains(
		t, ValidateInitialBalances(
			KindPromoteVTXO, RoleClient, btcutil.MaxSatoshi+1, 1, 0,
		),
		"money supply",
	)
}
