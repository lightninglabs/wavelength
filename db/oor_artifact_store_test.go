package db

import (
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newOORArtifactStoreForTest creates an artifact store backed by a fresh test
// database and transactional query adapter.
func newOORArtifactStoreForTest(t *testing.T) *OORArtifactPersistenceStore {
	t.Helper()

	db := NewTestDB(t)

	artifactDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) OORArtifactStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	clk := clock.NewDefaultClock()

	return NewOORArtifactPersistenceStore(artifactDB, clk)
}

// TestOORArtifactStoreGetPackageForOutpoint verifies that a persisted
// outpoint binding resolves to a full package bundle and preserves the matched
// binding identity.
func TestOORArtifactStoreGetPackageForOutpoint(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newOORArtifactStoreForTest(t)

	sessionID, arkPSBT, checkpoints, recipientOutpoint, recipientPkScript,
		valueSat, _ := buildTestOORPackage(t, 0x11)

	err := store.UpsertPackage(ctx, OORPackageDirectionIncoming,
		sessionID, arkPSBT, checkpoints)
	require.NoError(t, err)

	err = store.UpsertBinding(ctx, recipientOutpoint, sessionID, 0,
		OORPackageLinkKindCreatedOutput, recipientPkScript, &valueSat)
	require.NoError(t, err)

	pkg, err := store.GetPackageForOutpoint(ctx, recipientOutpoint)
	require.NoError(t, err)
	require.NotNil(t, pkg)
	require.Equal(t, sessionID, pkg.SessionID)
	require.Equal(t, OORPackageDirectionIncoming, pkg.Direction)
	require.Len(t, pkg.FinalCheckpointPSBTs, 1)
	require.NotNil(t, pkg.MatchedOutpointBinding)
	require.Equal(t, OORPackageLinkKindCreatedOutput,
		pkg.MatchedOutpointBinding.LinkKind)
	require.EqualValues(t, recipientOutpoint.Index,
		pkg.MatchedOutpointBinding.Outpoint.Index)
	require.Equal(t, recipientOutpoint.Hash,
		pkg.MatchedOutpointBinding.Outpoint.Hash)
}

// TestOORArtifactStoreListReceivedAndSentPackages verifies that package
// listings are correctly filtered by direction while still returning full
// bundle data.
func TestOORArtifactStoreListReceivedAndSentPackages(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newOORArtifactStoreForTest(t)

	incomingSession, incomingArk, incomingCheckpoints,
		incomingOutpoint, incomingScript, incomingValue, _ :=
		buildTestOORPackage(t, 0x21)

	err := store.UpsertPackage(ctx, OORPackageDirectionIncoming,
		incomingSession, incomingArk, incomingCheckpoints)
	require.NoError(t, err)

	err = store.UpsertBinding(ctx, incomingOutpoint, incomingSession, 0,
		OORPackageLinkKindCreatedOutput, incomingScript, &incomingValue)
	require.NoError(t, err)

	outgoingSession, outgoingArk, outgoingCheckpoints, _, _, _,
		consumedInput := buildTestOORPackage(t, 0x31)

	err = store.UpsertPackage(ctx, OORPackageDirectionOutgoing,
		outgoingSession, outgoingArk, outgoingCheckpoints)
	require.NoError(t, err)

	err = store.UpsertBinding(ctx, consumedInput, outgoingSession, 0,
		OORPackageLinkKindConsumedInput, nil, nil)
	require.NoError(t, err)

	all, err := store.ListPackages(ctx, nil)
	require.NoError(t, err)
	require.Len(t, all, 2)

	received, err := store.ListReceivedPackages(ctx)
	require.NoError(t, err)
	require.Len(t, received, 1)
	require.Equal(t, OORPackageDirectionIncoming, received[0].Direction)

	sent, err := store.ListSentPackages(ctx)
	require.NoError(t, err)
	require.Len(t, sent, 1)
	require.Equal(t, OORPackageDirectionOutgoing, sent[0].Direction)
}

// TestOORArtifactStoreGetPackageForOutpointPrefersCreatedBinding verifies that
// outpoint lookups return the created-output package when both created and
// consumed bindings exist for the same outpoint.
func TestOORArtifactStoreGetPackageForOutpointPrefersCreatedBinding(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store := newOORArtifactStoreForTest(t)

	externalInput := wire.OutPoint{
		Hash:  chainhash.Hash{0x63, 0xaa},
		Index: 0,
	}

	parentSession, parentArk, parentCheckpoints, parentOutpoint,
		parentScript, parentValue, _ := buildTestOORPackageWithInput(
		t, 0x63, externalInput,
	)

	err := store.UpsertPackage(ctx, OORPackageDirectionIncoming,
		parentSession, parentArk, parentCheckpoints)
	require.NoError(t, err)

	err = store.UpsertBinding(ctx, parentOutpoint, parentSession, 0,
		OORPackageLinkKindCreatedOutput, parentScript, &parentValue)
	require.NoError(t, err)

	childSession, childArk, childCheckpoints, _, _, _, _ :=
		buildTestOORPackageWithInput(t, 0x64, parentOutpoint)
	err = store.UpsertPackage(ctx, OORPackageDirectionOutgoing,
		childSession, childArk, childCheckpoints)
	require.NoError(t, err)

	err = store.UpsertBinding(ctx, parentOutpoint, childSession, 0,
		OORPackageLinkKindConsumedInput, nil, nil)
	require.NoError(t, err)

	pkg, err := store.GetPackageForOutpoint(ctx, parentOutpoint)
	require.NoError(t, err)
	require.NotNil(t, pkg)
	require.Equal(t, parentSession, pkg.SessionID)
	require.NotNil(t, pkg.MatchedOutpointBinding)
	require.Equal(t, OORPackageLinkKindCreatedOutput,
		pkg.MatchedOutpointBinding.LinkKind)
}

// buildTestOORPackage constructs a deterministic incoming-style OOR package
// fixture and returns the primary outpoints used by storage tests.
func buildTestOORPackage(t *testing.T, seed byte) (chainhash.Hash, *psbt.Packet,
	[]*psbt.Packet, wire.OutPoint, []byte, int64, wire.OutPoint) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{seed, 0xaa},
		Index: 0,
	}

	spentPkScript, err := txscript.PayToTaprootScript(operatorKey.PubKey())
	require.NoError(t, err)

	inputValue := btcutil.Amount(10_000)
	checkpointInput := oortx.CheckpointInput{
		SpentVTXO: oortx.SpentVTXORef{
			Outpoint: inputOutpoint,
			Output: &wire.TxOut{
				Value:    int64(inputValue),
				PkScript: spentPkScript,
			},
		},
		OwnerLeafScript: []byte{0x51},
	}

	checkpointRes, err := oortx.BuildCheckpointPSBT(policy, checkpointInput)
	require.NoError(t, err)

	recipientTapKey, err := scripts.VTXOTapKey(
		recipientKey.PubKey(), operatorKey.PubKey(), 10,
	)
	require.NoError(t, err)

	recipientPkScript, err := txscript.PayToTaprootScript(recipientTapKey)
	require.NoError(t, err)

	checkpointTxID := checkpointRes.PSBT.UnsignedTx.TxHash()
	checkpointOutput := checkpointRes.PSBT.UnsignedTx.TxOut[0]

	arkPSBT, err := oortx.BuildArkPSBT(
		[]oortx.CheckpointOutput{
			{
				Txid:           checkpointTxID,
				Output:         checkpointOutput,
				TapTreeEncoded: checkpointRes.TapTreeEncoded,
			},
		},
		[]oortx.RecipientOutput{
			{
				PkScript: recipientPkScript,
				Value:    inputValue,
			},
		},
	)
	require.NoError(t, err)

	sessionID := arkPSBT.UnsignedTx.TxHash()
	recipientOutpoint := wire.OutPoint{
		Hash:  sessionID,
		Index: 0,
	}

	return sessionID, arkPSBT, []*psbt.Packet{checkpointRes.PSBT},
		recipientOutpoint, recipientPkScript, int64(inputValue),
		inputOutpoint
}
