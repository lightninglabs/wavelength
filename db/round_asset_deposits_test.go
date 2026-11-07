package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// confirmTestAssetDeposit stores a confirmed deposit whose output script
// composes the boarding policy of fresh keys with a commitment leaf.
func confirmTestAssetDeposit(t *testing.T, store *AssetDepositStore,
	requestID string) *AssetDeposit {

	t.Helper()

	ctx := t.Context()
	_, operator := btcec.PrivKeyFromBytes([]byte{0x02})
	_, owner := btcec.PrivKeyFromBytes([]byte{0x03})
	leafHash := chainhash.Hash{0x0e}
	template, err := arkscript.EncodeStandardVTXOTemplate(
		owner, operator, 144,
	)
	require.NoError(t, err)
	pkScript, _, _, err := arkscript.ComposedBoardingScript(
		template, leafHash,
	)
	require.NoError(t, err)

	_, err = store.CreateAssetDeposit(ctx, &AssetDepositRequest{
		RequestID: requestID,
		AssetRef: tapsdk.AssetRefFromAssetID(
			tapsdk.AssetID{0x11},
		).String(),
		AssetAmount:     600,
		CarrierValueSat: 4_000,
		TargetConf:      6,
		MaxFeeSat:       5_000,
		OperatorKey:     operator,
		ExitDelay:       144,
		FundingProofs:   [][]byte{{0x01}},
	})
	require.NoError(t, err)

	output := &AssetDepositOutput{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x07,
			},
			Index: 1,
		},
		ValueSat:       4_000,
		FeeSat:         250,
		PkScript:       pkScript,
		PolicyTemplate: template,
		OwnerKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 42,
				Index:  9,
			},
			PubKey: owner,
		},
		Digest: [32]byte{
			0x0d,
		},
		ScriptKey:        owner.SerializeCompressed(),
		TaprootAssetRoot: leafHash,
		OPTrueWitness: [][]byte{
			{
				txscript.OP_TRUE,
			},
			{
				0xc0,
			},
		},
	}
	require.NoError(
		t, store.MarkAssetDepositPublished(ctx, requestID, output),
	)
	require.NoError(
		t,
		store.MarkAssetDepositConfirmed(
			ctx, requestID, []byte("proof"), 120,
		),
	)

	deposit, err := store.GetAssetDeposit(ctx, requestID)
	require.NoError(t, err)

	return deposit
}

// TestRoundAssetDepositCheckpoint checks that a round checkpointed with an
// asset deposit restores it with its input signature, holds the deposit until
// the round fails, and then releases it.
func TestRoundAssetDepositCheckpoint(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	roundStore, baseDB := newRoundStoreForTest(t)
	depositStore := &AssetDepositStore{
		TransactionExecutor: NewTransactionExecutor(
			baseDB, func(tx *sql.Tx) *sqlc.Queries {
				return baseDB.WithTx(tx)
			}, btclog.Disabled,
		),
		now: time.Now,
	}
	deposit := confirmTestAssetDeposit(t, depositStore, "alice-1")

	boarding, err := deposit.RoundBoarding()
	require.NoError(t, err)
	intent, err := round.NewAssetBoardingIntent(
		boarding, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	signer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	sig, err := schnorr.Sign(signer, chainhash.HashB([]byte("input")))
	require.NoError(t, err)
	inputSig := &types.BoardingInputSignature{
		InputIndex:      2,
		Outpoint:        intent.Outpoint,
		ClientSignature: sig,
	}

	roundID := testRoundIDDB("asset-deposit-round")
	testRound := createTestRound(t, roundID)
	testRound.Intents.Boarding = []round.BoardingIntent{intent}
	state := &round.InputSigSentState{
		RoundID:     roundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
		InputSigs: []*types.BoardingInputSignature{
			inputSig,
		},
	}
	testRound.CommitmentTx.WhenSome(func(packet *psbt.Packet) {
		state.CommitmentTx = packet
	})
	require.NoError(t, roundStore.CommitState(ctx, testRound, state))

	spender, err := depositStore.AssetDepositRound(ctx, "alice-1")
	require.NoError(t, err)
	require.Equal(t, roundID.String(), spender)

	restored, restoredState, err := roundStore.FetchState(ctx, roundID)
	require.NoError(t, err)
	require.Len(t, restored.Intents.Boarding, 1)
	got := restored.Intents.Boarding[0]
	require.Equal(t, intent.Outpoint, got.Outpoint)
	require.Equal(t, intent.Request.AssetRef, got.Request.AssetRef)
	require.Equal(t, intent.Request.AssetProof, got.Request.AssetProof)
	require.NotNil(t, got.AuthSpend)

	sigState, ok := restoredState.(*round.InputSigSentState)
	require.True(t, ok)
	require.Len(t, sigState.InputSigs, 1)
	require.Equal(t, 2, sigState.InputSigs[0].InputIndex)
	require.True(t, sigState.InputSigs[0].ClientSignature.IsEqual(sig))

	active, err := roundStore.ListActiveRounds(ctx)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Len(t, active[0].Intents.Boarding, 1)

	require.NoError(t, roundStore.FailRound(ctx, roundID))
	spender, err = depositStore.AssetDepositRound(ctx, "alice-1")
	require.NoError(t, err)
	require.Empty(t, spender)
}

// TestRoundAssetDepositRequiresSignature checks that a checkpoint refuses an
// asset deposit without its input signature.
func TestRoundAssetDepositRequiresSignature(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	roundStore, baseDB := newRoundStoreForTest(t)
	depositStore := &AssetDepositStore{
		TransactionExecutor: NewTransactionExecutor(
			baseDB, func(tx *sql.Tx) *sqlc.Queries {
				return baseDB.WithTx(tx)
			}, btclog.Disabled,
		),
		now: time.Now,
	}
	deposit := confirmTestAssetDeposit(t, depositStore, "alice-1")
	boarding, err := deposit.RoundBoarding()
	require.NoError(t, err)
	intent, err := round.NewAssetBoardingIntent(
		boarding, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	roundID := testRoundIDDB("unsigned-asset-round")
	testRound := createTestRound(t, roundID)
	testRound.Intents.Boarding = []round.BoardingIntent{intent}
	state := &round.InputSigSentState{
		RoundID:     roundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
		InputSigs: []*types.BoardingInputSignature{
			nil,
		},
	}
	require.Error(t, roundStore.CommitState(ctx, testRound, state))

	spender, err := depositStore.AssetDepositRound(ctx, "alice-1")
	require.NoError(t, err)
	require.Empty(t, spender)
}
