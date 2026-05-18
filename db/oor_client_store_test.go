//nolint:ll
package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

func TestOORClientStoreEffectClaimRetryDone(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newOORClientStoreForTest(t)
	sessionID := []byte("oor-client-effect-session")

	require.NoError(
		t,
		store.UpsertSession(
			ctx, OORClientSession{
				SessionID: sessionID,
				Direction: "outgoing",
				State:     "awaiting_ark_signatures",
				IdempotencyKey: sql.NullString{
					String: "session-effect",
					Valid:  true,
				},
			},
		),
	)
	require.NoError(
		t,
		store.InsertEffect(
			ctx, OORClientEffectInsert{
				ID:             "effect-1",
				SessionID:      sessionID,
				Direction:      "outgoing",
				EffectType:     "request_ark_signatures",
				IdempotencyKey: "effect-1",
			},
		),
	)

	claimed, err := store.ClaimDueEffects(ctx, "worker-a", 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, "effect-1", claimed[0].ID)
	require.True(t, claimed[0].ClaimToken.Valid)

	claimedAgain, err := store.ClaimDueEffects(
		ctx, "worker-b", 10, time.Minute,
	)
	require.NoError(t, err)
	require.Empty(t, claimedAgain)

	require.NoError(
		t,
		store.ReleaseEffectForRetry(
			ctx, claimed[0].ID, claimed[0].ClaimToken.String,
			time.Nanosecond, assertErr("transient"),
		),
	)

	require.Eventually(t, func() bool {
		claimedRetry, err := store.ClaimDueEffects(
			ctx, "worker-a", 10, time.Minute,
		)
		if err != nil || len(claimedRetry) != 1 {
			return false
		}

		err = store.MarkEffectDone(
			ctx, claimedRetry[0].ID,
			claimedRetry[0].ClaimToken.String,
		)

		return err == nil
	}, time.Second, 10*time.Millisecond)

	claimedDone, err := store.ClaimDueEffects(
		ctx, "worker-c", 10, time.Minute,
	)
	require.NoError(t, err)
	require.Empty(t, claimedDone)
}

func TestOORClientStoreArtifactPhaseUpsertLoad(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newOORClientStoreForTest(t)
	sessionID := []byte("oor-client-artifact-session")

	require.NoError(
		t,
		store.UpsertSession(
			ctx, OORClientSession{
				SessionID: sessionID,
				Direction: "incoming",
				State:     "receive_resolving",
				IdempotencyKey: sql.NullString{
					String: "session-artifacts",
					Valid:  true,
				},
			},
		),
	)

	require.NoError(
		t,
		store.SaveArkArtifact(
			ctx, OORClientArkArtifact{
				SessionID: sessionID,
				Phase:     "unsigned",
				ArkPSBT:   []byte("unsigned-ark-psbt"),
			},
		),
	)
	arkArtifact, err := store.LoadArkArtifact(
		ctx, sessionID, "unsigned",
	)
	require.NoError(t, err)
	require.Equal(t, []byte("unsigned-ark-psbt"), arkArtifact.ArkPSBT)

	require.NoError(
		t,
		store.SaveArkArtifact(
			ctx, OORClientArkArtifact{
				SessionID: sessionID,
				Phase:     "unsigned",
				ArkPSBT:   []byte("updated-unsigned-ark-psbt"),
			},
		),
	)
	arkArtifact, err = store.LoadArkArtifact(ctx, sessionID, "unsigned")
	require.NoError(t, err)
	require.Equal(
		t, []byte("updated-unsigned-ark-psbt"), arkArtifact.ArkPSBT,
	)

	require.NoError(
		t,
		store.SaveCheckpoint(
			ctx, OORClientCheckpoint{
				SessionID:       sessionID,
				CheckpointIndex: 1,
				Phase:           "finalized",
				CheckpointPSBT:  []byte("checkpoint-1"),
			},
		),
	)
	require.NoError(
		t,
		store.SaveCheckpoint(
			ctx, OORClientCheckpoint{
				SessionID:       sessionID,
				CheckpointIndex: 2,
				Phase:           "finalized",
				CheckpointPSBT:  []byte("checkpoint-2"),
			},
		),
	)
	require.NoError(
		t,
		store.SaveCheckpoint(
			ctx, OORClientCheckpoint{
				SessionID:       sessionID,
				CheckpointIndex: 1,
				Phase:           "finalized",
				CheckpointPSBT:  []byte("checkpoint-1-updated"),
			},
		),
	)

	checkpoint, err := store.LoadCheckpoint(ctx, sessionID, 1, "finalized")
	require.NoError(t, err)
	require.Equal(
		t, []byte("checkpoint-1-updated"), checkpoint.CheckpointPSBT,
	)

	checkpoints, err := store.ListCheckpointsByPhase(
		ctx, sessionID, "finalized",
	)
	require.NoError(t, err)
	require.Len(t, checkpoints, 2)
	require.Equal(t, int32(1), checkpoints[0].CheckpointIndex)
	require.Equal(t, int32(2), checkpoints[1].CheckpointIndex)
}

func TestOORClientStoreBundleInsertsEffectsAtomically(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newOORClientStoreForTest(t)
	sessionID := []byte("oor-client-bundle-effect-session")

	require.NoError(
		t,
		store.UpsertSessionBundle(
			ctx, OORClientSessionBundle{
				Session: OORClientSession{
					SessionID: sessionID,
					Direction: "outgoing",
					State:     "awaiting_submit_accepted",
					IdempotencyKey: sql.NullString{
						String: "bundle-effect-session",
						Valid:  true,
					},
				},
				ArkArtifacts: []OORClientArkArtifact{
					{
						SessionID: sessionID,
						Phase:     "ark_signed",
						ArkPSBT:   []byte("signed-ark"),
					},
				},
				Effects: []OORClientEffectInsert{
					{
						ID:             "bundle-effect",
						SessionID:      sessionID,
						Direction:      "outgoing",
						EffectType:     "send_submit_package",
						IdempotencyKey: "bundle-effect",
					},
				},
			},
		),
	)

	artifact, err := store.LoadArkArtifact(ctx, sessionID, "ark_signed")
	require.NoError(t, err)
	require.Equal(t, []byte("signed-ark"), artifact.ArkPSBT)

	claimed, err := store.ClaimDueEffects(ctx, "worker-a", 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, "bundle-effect", claimed[0].ID)
	require.Equal(t, "send_submit_package", claimed[0].EffectType)

	require.NoError(
		t,
		store.UpsertSessionBundle(
			ctx, OORClientSessionBundle{
				Session: OORClientSession{
					SessionID: sessionID,
					Direction: "outgoing",
					State:     "awaiting_submit_accepted",
				},
				Effects: []OORClientEffectInsert{
					{
						ID:             "bundle-effect",
						SessionID:      sessionID,
						Direction:      "outgoing",
						EffectType:     "send_submit_package",
						IdempotencyKey: "bundle-effect",
					},
				},
			},
		),
	)

	claimedDuplicate, err := store.ClaimDueEffects(
		ctx, "worker-b", 10, time.Minute,
	)
	require.NoError(t, err)
	require.Empty(t, claimedDuplicate)
}

func TestOORClientStoreSaveIncomingMetadataEffectAtomically(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newOORClientStoreForTest(t)
	sessionID := []byte("oor-client-incoming-metadata-session")

	require.NoError(
		t,
		store.UpsertSession(
			ctx, OORClientSession{
				SessionID: sessionID,
				Direction: "incoming",
				State:     "receive_notified",
				IdempotencyKey: sql.NullString{
					String: "incoming-metadata-session",
					Valid:  true,
				},
			},
		),
	)

	metadata := []OORClientIncomingMetadata{
		{
			SessionID:   sessionID,
			OutputIndex: 1,
			RoundID:     []byte("round-1"),
			ChainDepth: sql.NullInt32{
				Int32: 6,
				Valid: true,
			},
			BatchExpiry: sql.NullInt32{
				Int32: 144,
				Valid: true,
			},
			OperatorPubkey: []byte("operator-key-1"),
			AncestryBlob:   []byte("ancestry-1"),
			MetadataBlob:   []byte("metadata-match-1"),
		},
		{
			SessionID:      sessionID,
			OutputIndex:    0,
			RoundID:        []byte("round-0"),
			OperatorPubkey: []byte("operator-key-0"),
			MetadataBlob:   []byte("metadata-match-0"),
		},
	}

	require.NoError(
		t,
		store.SaveIncomingMetadataEffect(
			ctx, metadata, OORClientEffectInsert{
				ID:             "materialize-incoming",
				SessionID:      sessionID,
				Direction:      "incoming",
				EffectType:     "materialize_incoming_vtxos",
				IdempotencyKey: "materialize-incoming",
			},
		),
	)

	rows, err := store.ListIncomingMetadata(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, int32(0), rows[0].OutputIndex)
	require.Equal(t, []byte("metadata-match-0"), rows[0].MetadataBlob)
	require.Equal(t, int32(1), rows[1].OutputIndex)
	require.Equal(t, []byte("metadata-match-1"), rows[1].MetadataBlob)
	require.Equal(t, []byte("ancestry-1"), rows[1].AncestryBlob)

	claimed, err := store.ClaimDueEffects(ctx, "worker-a", 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, "materialize-incoming", claimed[0].ID)
	require.Equal(t, "materialize_incoming_vtxos", claimed[0].EffectType)

	require.NoError(
		t,
		store.SaveIncomingMetadataEffect(
			ctx, metadata, OORClientEffectInsert{
				ID:             "materialize-incoming",
				SessionID:      sessionID,
				Direction:      "incoming",
				EffectType:     "materialize_incoming_vtxos",
				IdempotencyKey: "materialize-incoming",
			},
		),
	)

	duplicateClaim, err := store.ClaimDueEffects(
		ctx, "worker-b", 10, time.Minute,
	)
	require.NoError(t, err)
	require.Empty(t, duplicateClaim)
}

func newOORClientStoreForTest(t *testing.T) *OORClientStoreDB {
	t.Helper()

	sqlDB := NewTestDB(t)
	store := NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	)

	return NewOORClientStore(store, clock.NewDefaultClock())
}
