package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightningnetwork/lnd/tlv"
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

// TestDurableLegacyBoardingWithoutAuthRecord uses the exact original record
// identities, omitting record 51 entirely rather than writing an empty value.
// Bitcoin intent ownership survives; an asset disclosure still fails closed.
func TestDurableLegacyBoardingWithoutAuthRecord(t *testing.T) {
	intent := durableBoardingFixture(t)
	intent.AuthSpend = nil
	intent.Request.AssetRef = ""
	intent.Request.AssetAmount = 0
	intent.Request.AssetDigest = nil
	intent.Request.AssetProof = nil
	intent.Request.AssetCommitmentLeafHash = nil
	intent.Request.AssetWitness = nil
	raw, err := encodeDurableBoarding(intent)
	require.NoError(t, err)
	var record durableBoarding
	require.NoError(t, decodeDurableFields(raw, record.records()...))
	legacyRecords := make([]tlv.Record, 0, 25)
	for _, field := range record.records() {
		if field.Type() <= 49 {
			legacyRecords = append(legacyRecords, field)
		}
	}
	legacy, err := encodeDurableFields(legacyRecords...)
	require.NoError(t, err)
	restored, err := decodeDurableBoarding(
		legacy, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.Equal(t, intent.Outpoint, restored.Outpoint)
	require.Equal(t, intent.Address.KeyDesc, restored.Address.KeyDesc)
	require.Nil(t, restored.AuthSpend)
	// Exercise the outer version-one decoder with a nonempty boarding list.
	stateRaw, err := encodeClientState(
		&PendingRoundAssembly{
			Boarding: []BoardingIntent{intent},
		},
	)
	require.NoError(t, err)
	var stateRecord clientStateRecord
	require.NoError(
		t,
		decodeDurableFields(
			stateRaw, stateRecord.records()...,
		),
	)
	var intentsRecord durableIntents
	require.NoError(
		t,
		decodeDurableFields(
			stateRecord.Intents, intentsRecord.records()...,
		),
	)
	intentsRecord.Boarding, err = encodeDurableList(
		[][]byte{legacy}, durableBlob,
	)
	require.NoError(t, err)
	stateRecord.Intents, err = encodeDurableFields(
		intentsRecord.records()...,
	)
	require.NoError(t, err)
	stateRecord.Version = 1
	stateRaw, err = encodeDurableFields(stateRecord.records()...)
	require.NoError(t, err)
	state, lost, err := decodeClientState(
		stateRaw, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.False(t, lost)
	assembly, ok := state.(*PendingRoundAssembly)
	require.True(t, ok)
	require.Equal(t, intent.Outpoint, assembly.Boarding[0].Outpoint)
	record.AssetRef = []byte("asset")
	legacyRecords = record.records()[:25]
	legacy, err = encodeDurableFields(legacyRecords...)
	require.NoError(t, err)
	_, err = decodeDurableBoarding(legacy, &chaincfg.RegressionNetParams)
	require.ErrorContains(t, err, "missing authorization path")
}
