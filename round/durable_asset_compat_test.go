package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/stretchr/testify/require"
)

// TestDurableAssetAuthorizationPath preserves the real composed asset leaf,
// including its control block, so cold restore cannot select a different join
// authorization path from the one proved by the asset boarding constructor.
func TestDurableAssetAuthorizationPath(t *testing.T) {
	owner, _ := btcec.PrivKeyFromBytes([]byte{11})
	operator, _ := btcec.PrivKeyFromBytes([]byte{12})
	deposit := newTestAssetBoarding(t, owner.PubKey(), operator.PubKey())
	intent, err := NewAssetBoardingIntent(
		deposit, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	encoded, err := encodeDurableBoarding(intent)
	require.NoError(t, err)
	restored, err := decodeDurableBoarding(
		encoded, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.NotNil(t, restored.AuthSpend)
	before, err := intent.AuthSpend.Encode()
	require.NoError(t, err)
	after, err := restored.AuthSpend.Encode()
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NoError(
		t, restored.AuthSpend.VerifyBindsToPkScript(deposit.PkScript),
	)
}

// TestDurableLegacyVersionOneState keeps existing Bitcoin checkpoint decoding
// while new writers use version two to prevent old readers dropping AuthSpend.
func TestDurableLegacyVersionOneState(t *testing.T) {
	raw, err := encodeClientState(&Idle{})
	require.NoError(t, err)
	var record clientStateRecord
	require.NoError(t, decodeDurableFields(raw, record.records()...))
	require.EqualValues(t, 2, record.Version)
	record.Version = 1
	legacy, err := encodeDurableFields(record.records()...)
	require.NoError(t, err)
	state, lost, err := decodeClientState(
		legacy, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.False(t, lost)
	require.IsType(t, &Idle{}, state)
	record.Version = 3
	unknown, err := encodeDurableFields(record.records()...)
	require.NoError(t, err)
	_, _, err = decodeClientState(unknown, &chaincfg.RegressionNetParams)
	require.ErrorContains(t, err, "unknown client state version")
}

// TestDurableMissingAssetAuthorizationFailsClosed models a version-one
// prototype snapshot that retained asset disclosure but omitted AuthSpend.
// Restore must refuse it rather than substitute the standard Bitcoin leaf.
func TestDurableMissingAssetAuthorizationFailsClosed(t *testing.T) {
	legacy := durableBoarding{AssetRef: []byte("legacy-asset")}
	raw, err := encodeDurableFields(legacy.records()...)
	require.NoError(t, err)
	_, err = decodeDurableBoarding(raw, &chaincfg.RegressionNetParams)
	require.ErrorContains(t, err, "missing authorization path")
	intent := durableBoardingFixture(t)
	intent.AuthSpend = nil
	_, err = encodeDurableBoarding(intent)
	require.ErrorContains(t, err, "requires authorization path")
}
