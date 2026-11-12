package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/bip322"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newTestAssetBoarding returns a deposit whose script composes the standard
// boarding policy of the given keys with a fixed commitment leaf.
func newTestAssetBoarding(t *testing.T,
	owner, operator *btcec.PublicKey) *AssetBoarding {

	t.Helper()

	const exitDelay = 144
	leafHash := [32]byte{0x44, 0x55}
	template, err := arkscript.EncodeStandardVTXOTemplate(
		owner, operator, exitDelay,
	)
	require.NoError(t, err)
	pkScript, _, _, err := arkscript.ComposedBoardingScript(
		template, leafHash,
	)
	require.NoError(t, err)

	return &AssetBoarding{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x0a,
			},
			Index: 1,
		},
		Value:    4_000,
		PkScript: pkScript,
		OwnerKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 42,
				Index:  3,
			},
			PubKey: owner,
		},
		OperatorKey: operator,
		ExitDelay:   exitDelay,
		ConfHeight:  210,
		AssetRef: tapsdk.AssetRefFromAssetID(
			tapsdk.AssetID{0x11},
		).String(),
		AssetAmount: 600,
		AssetDigest: [32]byte{
			0x33,
		},
		AssetProof: []byte{
			0x01,
			0x02,
		},
		CommitmentLeafHash: leafHash,
		AssetWitness: [][]byte{
			{
				txscript.OP_TRUE,
			},
			{
				0xc0,
			},
		},
	}
}

// TestNewAssetBoardingIntent checks that a deposit becomes a boarding intent
// only when its boarding policy and commitment leaf reproduce its script.
func TestNewAssetBoardingIntent(t *testing.T) {
	t.Parallel()

	_, owner := btcec.PrivKeyFromBytes([]byte{0x01})
	_, operator := btcec.PrivKeyFromBytes([]byte{0x02})
	params := &chaincfg.RegressionNetParams
	deposit := newTestAssetBoarding(t, owner, operator)

	intent, err := NewAssetBoardingIntent(deposit, params)
	require.NoError(t, err)

	pkScript, err := txscript.PayToAddrScript(intent.Address.Address)
	require.NoError(t, err)
	require.Equal(t, deposit.PkScript, pkScript)
	require.Equal(t, deposit.Value, intent.ChainInfo.Amount)
	require.Equal(t, deposit.Outpoint, *intent.Request.Outpoint)
	require.Equal(t, deposit.AssetRef, intent.Request.AssetRef)
	require.Equal(t, deposit.AssetAmount, intent.Request.AssetAmount)
	require.Equal(
		t, deposit.CommitmentLeafHash[:],
		intent.Request.AssetCommitmentLeafHash,
	)
	require.NoError(t, intent.AuthSpend.VerifyBindsToPkScript(pkScript))

	other := *deposit
	other.CommitmentLeafHash = [32]byte{0x66}
	_, err = NewAssetBoardingIntent(&other, params)
	require.ErrorContains(t, err, "do not reproduce")

	noAsset := *deposit
	noAsset.AssetRef = ""
	noAsset.AssetAmount = 0
	_, err = NewAssetBoardingIntent(&noAsset, params)
	require.Error(t, err)
}

// TestBuildJoinRoundAuthAssetDeposit checks that join-round authorization
// proves an asset deposit through its composed exit spend, and that the
// plain boarding spend does not.
func TestBuildJoinRoundAuthAssetDeposit(t *testing.T) {
	t.Parallel()

	f := newJoinAuthTestFixture(t)
	deposit := newTestAssetBoarding(
		t, f.clientPrivKey.PubKey(), f.operatorPrivKey.PubKey(),
	)
	intent, err := NewAssetBoardingIntent(
		deposit, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	vtxoReqs := []types.VTXORequest{
		f.newVTXORequest(t, 2_000),
	}
	proofPrevOuts := map[wire.OutPoint]*wire.TxOut{
		deposit.Outpoint: {
			Value:    int64(deposit.Value),
			PkScript: deposit.PkScript,
		},
	}
	build := func(intent BoardingIntent) *types.JoinRoundAuth {
		auth, err := buildJoinRoundAuth(
			f.ctx, f.env, f.identifierKeyDesc(), Intents{
				Boarding: []BoardingIntent{intent},
				VTXOs:    vtxoReqs,
			}, vtxoReqs, nil, nil,
		)
		require.NoError(t, err)

		return auth
	}

	f.verifyAuth(t, build(intent), proofPrevOuts)

	plain := intent
	plain.AuthSpend = nil
	result := f.validateAuth(t, build(plain), proofPrevOuts)
	require.NotEqual(t, bip322.VerificationStateValid, result.State)
}

// TestHandleAssetBoarding checks that an asset deposit joins a round of its
// own with an asset VTXO and a BTC change VTXO, leaving queued Bitcoin
// boarding inputs in their round, and that the deposit is refused while a
// round holds it or another join is in flight.
func TestHandleAssetBoarding(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())

	for i := range 2 {
		h.wallet.On(
			"DeriveNextKey", mock.Anything,
			types.VTXOOwnerKeyFamily,
		).Return(&keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: types.VTXOOwnerKeyFamily,
				Index:  uint32(i),
			},
			PubKey: h.clientPubKey,
		}, nil).Once()
	}

	// A confirmed Bitcoin boarding input waits in an assembling round
	// for the user to board it.
	btcBoarding := h.newTestBoardingIntent()
	h.sendWalletConfirmation(btcBoarding)

	_, operator := btcec.PrivKeyFromBytes([]byte{0x02})
	deposit := newTestAssetBoarding(t, h.clientPubKey, operator)
	intent, err := NewAssetBoardingIntent(
		deposit, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	msg := &RegisterAssetBoardingRequest{
		Boarding:        intent,
		AssetVTXOAmount: 1_000,
		ChangeAmount:    2_000,
	}
	result := h.receive(msg)
	require.True(t, result.IsOk(), "%v", result.Err())
	require.Len(t, h.serverMessages(), 1)

	var (
		joined   *IntentSentState
		assembly *PendingRoundAssembly
	)
	for _, info := range h.queryState() {
		switch s := info.State.(type) {
		case *IntentSentState:
			joined = s

		case *PendingRoundAssembly:
			assembly = s
		}
	}
	require.NotNil(t, assembly)
	require.Len(t, assembly.Boarding, 1)
	require.Equal(t, btcBoarding.Outpoint, assembly.Boarding[0].Outpoint)

	require.NotNil(t, joined)
	require.Len(t, joined.Intents.Boarding, 1)
	require.Equal(t, deposit.Outpoint, joined.Intents.Boarding[0].Outpoint)
	require.NotNil(t, joined.Intents.Boarding[0].AuthSpend)
	require.Len(t, joined.Intents.VTXOs, 2)

	asset, change := joined.Intents.VTXOs[0], joined.Intents.VTXOs[1]
	require.Equal(t, btcutil.Amount(1_000), asset.Amount)
	require.Equal(t, deposit.AssetRef, asset.AssetRef)
	require.Equal(t, deposit.AssetAmount, asset.AssetAmount)
	require.True(t, asset.FixedAmount)
	require.False(t, asset.IsChange)
	require.Equal(t, types.VTXOOriginRoundTransfer, asset.Origin)
	require.Equal(t, btcutil.Amount(2_000), change.Amount)
	require.Empty(t, change.AssetRef)
	require.True(t, change.IsChange)
	require.Equal(t, types.VTXOOriginRoundTransfer, change.Origin)

	result = h.receive(msg)
	require.ErrorIs(t, result.Err(), ErrBoardingInFlight)

	// A second deposit would replace the first deposit's join.
	other := *deposit
	other.Outpoint.Index = 2
	otherIntent, err := NewAssetBoardingIntent(
		&other, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	otherMsg := &RegisterAssetBoardingRequest{
		Boarding:        otherIntent,
		AssetVTXOAmount: 1_000,
		ChangeAmount:    2_000,
	}
	result = h.receive(otherMsg)
	require.ErrorIs(t, result.Err(), ErrRoundBusy)

	// A round whose state cannot be read fails the check with neither
	// error.
	for _, roundFSM := range h.actor.rounds {
		roundFSM.FSM.Stop()
	}
	result = h.receive(otherMsg)
	require.Error(t, result.Err())
	require.NotErrorIs(t, result.Err(), ErrBoardingInFlight)
	require.NotErrorIs(t, result.Err(), ErrRoundBusy)
	require.Len(t, h.serverMessages(), 1)
}

// TestReserveAssetDepositClaim checks that a deposit a round holds cannot be
// reserved for a claim, and that a reserved deposit cannot board.
func TestReserveAssetDepositClaim(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())

	for i := range 2 {
		h.wallet.On(
			"DeriveNextKey", mock.Anything,
			types.VTXOOwnerKeyFamily,
		).Return(&keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: types.VTXOOwnerKeyFamily,
				Index:  uint32(i),
			},
			PubKey: h.clientPubKey,
		}, nil).Once()
	}

	_, operator := btcec.PrivKeyFromBytes([]byte{0x02})
	boarded := newTestAssetBoarding(t, h.clientPubKey, operator)
	boardedIntent, err := NewAssetBoardingIntent(
		boarded, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	result := h.receive(&RegisterAssetBoardingRequest{
		Boarding:        boardedIntent,
		AssetVTXOAmount: 1_000,
		ChangeAmount:    2_000,
	})
	require.True(t, result.IsOk(), "%v", result.Err())

	result = h.receive(&ReserveAssetDepositClaimRequest{
		Outpoint: boarded.Outpoint,
	})
	require.ErrorIs(t, result.Err(), ErrBoardingInFlight)

	// A deposit no round holds is reserved even while another round is
	// joining, and then refused for boarding.
	claimed := *boarded
	claimed.Outpoint.Index = 2
	result = h.receive(&ReserveAssetDepositClaimRequest{
		Outpoint: claimed.Outpoint,
	})
	require.True(t, result.IsOk(), "%v", result.Err())

	claimedIntent, err := NewAssetBoardingIntent(
		&claimed, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	result = h.receive(&RegisterAssetBoardingRequest{
		Boarding:        claimedIntent,
		AssetVTXOAmount: 1_000,
		ChangeAmount:    2_000,
	})
	require.ErrorIs(t, result.Err(), ErrDepositClaimed)
	require.Len(t, h.serverMessages(), 1)
}
