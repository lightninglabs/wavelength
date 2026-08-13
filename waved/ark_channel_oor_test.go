package waved

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/vhtlcrecovery"
	"github.com/stretchr/testify/require"
)

// TestValidateArkChannelClaimRecovery binds channel promotion to the exact
// dormant receive-claim job that already protects the fallback vHTLC.
func TestValidateArkChannelClaimRecovery(t *testing.T) {
	t.Parallel()

	paymentHash := [32]byte{1, 2, 3}
	outpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			4,
			5,
			6,
		}, Index: 7,
	}
	terms := arkchannel.Terms{PaymentHash: paymentHash}
	source := ArkChannelClaimSource{
		RecoveryID: "recovery-id", Outpoint: outpoint.String(),
		Amount: btcutil.Amount(43_000),
	}
	job := vhtlcrecovery.RecoveryJob{
		ID: source.RecoveryID, SwapID: append(
			[]byte(nil), paymentHash[:]...,
		),
		Direction:    vhtlcrecovery.DirectionReceive,
		Action:       vhtlcrecovery.ActionClaim,
		State:        vhtlcrecovery.StateArmed,
		VTXOOutpoint: outpoint, VTXOAmountSat: int64(source.Amount),
		PreimageHash: append([]byte(nil), paymentHash[:]...),
	}
	require.NoError(
		t, validateArkChannelClaimRecovery(job, terms, source),
	)

	wrongState := job
	wrongState.State = vhtlcrecovery.StateUnrollStarted
	require.ErrorContains(
		t, validateArkChannelClaimRecovery(wrongState, terms, source),
		"recovery state",
	)

	wrongHash := job
	wrongHash.PreimageHash = make([]byte, 32)
	require.ErrorContains(
		t, validateArkChannelClaimRecovery(wrongHash, terms, source),
		"payment hash",
	)

	wrongSource := source
	wrongSource.Amount++
	require.ErrorContains(
		t, validateArkChannelClaimRecovery(job, terms, wrongSource),
		"amount",
	)
}
