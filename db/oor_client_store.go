//nolint:ll
package db

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

// OORClientStoreDB persists the client OOR runtime tables.
type OORClientStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]

	clk clock.Clock
}

// NewOORClientStore creates a client OOR runtime store from the shared DB.
func NewOORClientStore(store *Store, clk clock.Clock) *OORClientStoreDB {
	if clk == nil {
		clk = clock.NewDefaultClock()
	}

	txExec := NewTransactionExecutor(
		store.BaseDB(),
		func(tx *sql.Tx) *sqlc.Queries {
			return store.Queries().WithTx(tx)
		},
		store.log,
	)

	return &OORClientStoreDB{
		TransactionExecutor: txExec,
		clk:                 clk,
	}
}

type OORClientSession struct {
	SessionID      []byte
	Direction      string
	State          string
	IdempotencyKey sql.NullString
	RetryAfter     sql.NullInt64
	RetryReason    sql.NullString
	FailReason     sql.NullString
	CompletedAt    sql.NullInt64
}

type OORClientInput struct {
	SessionID          []byte
	InputIndex         int32
	OutpointHash       []byte
	OutpointIndex      int32
	AmountSat          int64
	PkScript           []byte
	ClientKeyFamily    int32
	ClientKeyIndex     int32
	ClientPubKey       []byte
	OperatorPubKey     []byte
	ExitDelay          int32
	VtxoPolicyTemplate []byte
	OwnerLeafScript    []byte
	OwnerLeafPolicy    []byte
	SpendWitnessScript []byte
	SpendControlBlock  []byte
	ConditionWitness   []byte
	RequiredSequence   sql.NullInt32
	RequiredLocktime   sql.NullInt32
}

type OORClientRecipient struct {
	SessionID          []byte
	OutputIndex        int32
	PkScript           []byte
	ValueSat           int64
	VtxoPolicyTemplate []byte
}

type OORClientIncomingHint struct {
	SessionID         []byte
	RecipientPkScript []byte
	RecipientEventID  int64
}

type OORClientIncomingMetadata struct {
	SessionID      []byte
	OutputIndex    int32
	RoundID        []byte
	ChainDepth     sql.NullInt32
	BatchExpiry    sql.NullInt32
	OperatorPubkey []byte
	AncestryBlob   []byte
	MetadataBlob   []byte
}

type OORClientArkArtifact struct {
	SessionID []byte
	Phase     string
	ArkPSBT   []byte
}

type OORClientCheckpoint struct {
	SessionID       []byte
	CheckpointIndex int32
	Phase           string
	CheckpointPSBT  []byte
}

type OORClientEffectInsert struct {
	ID             string
	SessionID      []byte
	Direction      string
	EffectType     string
	IdempotencyKey string
	MaxAttempts    int32
}

type OORClientEffect struct {
	ID             string
	SessionID      []byte
	Direction      string
	EffectType     string
	IdempotencyKey string
	ClaimToken     sql.NullString
	Attempts       int32
}

type OORClientSessionBundle struct {
	Session      OORClientSession
	Inputs       []OORClientInput
	Recipients   []OORClientRecipient
	ArkArtifacts []OORClientArkArtifact
	Checkpoints  []OORClientCheckpoint
	Metadata     []OORClientIncomingMetadata
	Effects      []OORClientEffectInsert
	IncomingHint *OORClientIncomingHint
}

func (s *OORClientStoreDB) UpsertSession(ctx context.Context,
	session OORClientSession) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.UpsertOORClientSession(
			ctx, sqlc.UpsertOORClientSessionParams{
				SessionID:      session.SessionID,
				Direction:      session.Direction,
				State:          session.State,
				IdempotencyKey: session.IdempotencyKey,
				RetryAfter:     session.RetryAfter,
				RetryReason:    session.RetryReason,
				FailReason:     session.FailReason,
				CreatedAt:      now,
				UpdatedAt:      now,
				CompletedAt:    session.CompletedAt,
			},
		)
	})
}

func (s *OORClientStoreDB) UpsertSessionBundle(ctx context.Context,
	bundle OORClientSessionBundle) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		err := q.UpsertOORClientSession(
			ctx, sqlc.UpsertOORClientSessionParams{
				SessionID:      bundle.Session.SessionID,
				Direction:      bundle.Session.Direction,
				State:          bundle.Session.State,
				IdempotencyKey: bundle.Session.IdempotencyKey,
				RetryAfter:     bundle.Session.RetryAfter,
				RetryReason:    bundle.Session.RetryReason,
				FailReason:     bundle.Session.FailReason,
				CreatedAt:      now,
				UpdatedAt:      now,
				CompletedAt:    bundle.Session.CompletedAt,
			},
		)
		if err != nil {
			return err
		}

		for i := range bundle.Inputs {
			input := bundle.Inputs[i]
			err := q.UpsertOORClientInput(
				ctx, sqlc.UpsertOORClientInputParams{
					SessionID:          input.SessionID,
					InputIndex:         input.InputIndex,
					OutpointHash:       input.OutpointHash,
					OutpointIndex:      input.OutpointIndex,
					AmountSat:          input.AmountSat,
					PkScript:           input.PkScript,
					ClientKeyFamily:    input.ClientKeyFamily,
					ClientKeyIndex:     input.ClientKeyIndex,
					ClientPubKey:       input.ClientPubKey,
					OperatorPubKey:     input.OperatorPubKey,
					ExitDelay:          input.ExitDelay,
					VtxoPolicyTemplate: input.VtxoPolicyTemplate,
					OwnerLeafScript:    input.OwnerLeafScript,
					OwnerLeafPolicy:    input.OwnerLeafPolicy,
					SpendWitnessScript: input.SpendWitnessScript,
					SpendControlBlock:  input.SpendControlBlock,
					ConditionWitness:   input.ConditionWitness,
					RequiredSequence:   input.RequiredSequence,
					RequiredLocktime:   input.RequiredLocktime,
				},
			)
			if err != nil {
				return err
			}
		}

		for i := range bundle.Recipients {
			recipient := bundle.Recipients[i]
			err := q.UpsertOORClientRecipient(
				ctx, sqlc.UpsertOORClientRecipientParams{
					SessionID:          recipient.SessionID,
					OutputIndex:        recipient.OutputIndex,
					PkScript:           recipient.PkScript,
					ValueSat:           recipient.ValueSat,
					VtxoPolicyTemplate: recipient.VtxoPolicyTemplate,
				},
			)
			if err != nil {
				return err
			}
		}

		for i := range bundle.ArkArtifacts {
			artifact := bundle.ArkArtifacts[i]
			err := q.UpsertOORClientArkArtifact(
				ctx, sqlc.UpsertOORClientArkArtifactParams{
					SessionID: artifact.SessionID,
					Phase:     artifact.Phase,
					ArkPsbt:   artifact.ArkPSBT,
					CreatedAt: now,
					UpdatedAt: now,
				},
			)
			if err != nil {
				return err
			}
		}

		for i := range bundle.Checkpoints {
			checkpoint := bundle.Checkpoints[i]
			err := q.UpsertOORClientCheckpoint(
				ctx, sqlc.UpsertOORClientCheckpointParams{
					SessionID:       checkpoint.SessionID,
					CheckpointIndex: checkpoint.CheckpointIndex,
					Phase:           checkpoint.Phase,
					CheckpointPsbt:  checkpoint.CheckpointPSBT,
					CreatedAt:       now,
					UpdatedAt:       now,
				},
			)
			if err != nil {
				return err
			}
		}

		for i := range bundle.Metadata {
			metadata := bundle.Metadata[i]
			err := q.UpsertOORClientIncomingMetadata(
				ctx, sqlc.UpsertOORClientIncomingMetadataParams{
					SessionID:      metadata.SessionID,
					OutputIndex:    metadata.OutputIndex,
					RoundID:        metadata.RoundID,
					ChainDepth:     metadata.ChainDepth,
					BatchExpiry:    metadata.BatchExpiry,
					OperatorPubkey: metadata.OperatorPubkey,
					AncestryBlob:   metadata.AncestryBlob,
					MetadataBlob:   metadata.MetadataBlob,
					CreatedAt:      now,
					UpdatedAt:      now,
				},
			)
			if err != nil {
				return err
			}
		}

		for i := range bundle.Effects {
			effect := bundle.Effects[i]
			if effect.ID == "" {
				effect.ID = effect.IdempotencyKey
			}
			if effect.ID == "" {
				effect.ID = uuid.NewString()
			}
			if effect.MaxAttempts == 0 {
				effect.MaxAttempts = 10
			}

			err := q.InsertOORClientEffect(
				ctx, sqlc.InsertOORClientEffectParams{
					ID:             effect.ID,
					SessionID:      effect.SessionID,
					Direction:      effect.Direction,
					EffectType:     effect.EffectType,
					IdempotencyKey: effect.IdempotencyKey,
					MaxAttempts:    effect.MaxAttempts,
					NextAttemptAt:  now,
					CreatedAt:      now,
				},
			)
			if err != nil {
				return err
			}
		}

		if bundle.IncomingHint == nil {
			return nil
		}

		return q.UpsertOORClientIncomingHint(
			ctx, sqlc.UpsertOORClientIncomingHintParams{
				SessionID: bundle.IncomingHint.SessionID,
				RecipientPkScript: bundle.IncomingHint.
					RecipientPkScript,
				RecipientEventID: bundle.IncomingHint.
					RecipientEventID,
				CreatedAt: now,
				UpdatedAt: now,
			},
		)
	})
}

func (s *OORClientStoreDB) ListActiveSessions(ctx context.Context) (
	[]OORClientSession, error) {

	var sessions []OORClientSession
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.ListActiveOORClientSessions(ctx)
		if err != nil {
			return err
		}

		sessions = make([]OORClientSession, 0, len(rows))
		for _, row := range rows {
			sessions = append(
				sessions, oorClientSessionFromRow(row),
			)
		}

		return nil
	})

	return sessions, err
}

func (s *OORClientStoreDB) ListSessions(ctx context.Context) (
	[]OORClientSession, error) {

	var sessions []OORClientSession
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.ListOORClientSessions(ctx)
		if err != nil {
			return err
		}

		sessions = make([]OORClientSession, 0, len(rows))
		for _, row := range rows {
			sessions = append(
				sessions, oorClientSessionFromRow(row),
			)
		}

		return nil
	})

	return sessions, err
}

func (s *OORClientStoreDB) GetSession(ctx context.Context, sessionID []byte) (
	OORClientSession, bool, error) {

	var session OORClientSession
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		row, err := q.GetOORClientSession(ctx, sessionID)
		if err != nil {
			return err
		}

		session = oorClientSessionFromRow(row)

		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return OORClientSession{}, false, nil
	}

	return session, err == nil, err
}

func (s *OORClientStoreDB) FindOutgoingByIdempotencyKey(ctx context.Context,
	idempotencyKey string) (OORClientSession, bool, error) {

	if idempotencyKey == "" {
		return OORClientSession{}, false, nil
	}

	var session OORClientSession
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		row, err := q.FindOORClientOutgoingSessionByIdempotencyKey(
			ctx, sql.NullString{
				String: idempotencyKey,
				Valid:  true,
			},
		)
		if err != nil {
			return err
		}

		session = oorClientSessionFromRow(row)

		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return OORClientSession{}, false, nil
	}

	return session, err == nil, err
}

func (s *OORClientStoreDB) UpsertInput(ctx context.Context,
	input OORClientInput) error {

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.UpsertOORClientInput(
			ctx, sqlc.UpsertOORClientInputParams{
				SessionID:          input.SessionID,
				InputIndex:         input.InputIndex,
				OutpointHash:       input.OutpointHash,
				OutpointIndex:      input.OutpointIndex,
				AmountSat:          input.AmountSat,
				PkScript:           input.PkScript,
				ClientKeyFamily:    input.ClientKeyFamily,
				ClientKeyIndex:     input.ClientKeyIndex,
				ClientPubKey:       input.ClientPubKey,
				OperatorPubKey:     input.OperatorPubKey,
				ExitDelay:          input.ExitDelay,
				VtxoPolicyTemplate: input.VtxoPolicyTemplate,
				OwnerLeafScript:    input.OwnerLeafScript,
				OwnerLeafPolicy:    input.OwnerLeafPolicy,
				SpendWitnessScript: input.SpendWitnessScript,
				SpendControlBlock:  input.SpendControlBlock,
				ConditionWitness:   input.ConditionWitness,
				RequiredSequence:   input.RequiredSequence,
				RequiredLocktime:   input.RequiredLocktime,
			},
		)
	})
}

func (s *OORClientStoreDB) ListInputs(ctx context.Context, sessionID []byte) (
	[]OORClientInput, error) {

	var inputs []OORClientInput
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.ListOORClientInputs(ctx, sessionID)
		if err != nil {
			return err
		}

		inputs = make([]OORClientInput, 0, len(rows))
		for _, row := range rows {
			inputs = append(inputs, oorClientInputFromRow(row))
		}

		return nil
	})

	return inputs, err
}

func (s *OORClientStoreDB) UpsertRecipient(ctx context.Context,
	recipient OORClientRecipient) error {

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.UpsertOORClientRecipient(
			ctx, sqlc.UpsertOORClientRecipientParams{
				SessionID:          recipient.SessionID,
				OutputIndex:        recipient.OutputIndex,
				PkScript:           recipient.PkScript,
				ValueSat:           recipient.ValueSat,
				VtxoPolicyTemplate: recipient.VtxoPolicyTemplate,
			},
		)
	})
}

func (s *OORClientStoreDB) ListRecipients(ctx context.Context,
	sessionID []byte) ([]OORClientRecipient, error) {

	var recipients []OORClientRecipient
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.ListOORClientRecipients(ctx, sessionID)
		if err != nil {
			return err
		}

		recipients = make([]OORClientRecipient, 0, len(rows))
		for _, row := range rows {
			recipients = append(
				recipients, oorClientRecipientFromRow(row),
			)
		}

		return nil
	})

	return recipients, err
}

func (s *OORClientStoreDB) UpsertIncomingHint(ctx context.Context,
	hint OORClientIncomingHint) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.UpsertOORClientIncomingHint(
			ctx, sqlc.UpsertOORClientIncomingHintParams{
				SessionID:         hint.SessionID,
				RecipientPkScript: hint.RecipientPkScript,
				RecipientEventID:  hint.RecipientEventID,
				CreatedAt:         now,
				UpdatedAt:         now,
			},
		)
	})
}

func (s *OORClientStoreDB) GetIncomingHint(ctx context.Context,
	sessionID []byte) (OORClientIncomingHint, bool, error) {

	var hint OORClientIncomingHint
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		row, err := q.GetOORClientIncomingHint(ctx, sessionID)
		if err != nil {
			return err
		}

		hint = oorClientIncomingHintFromRow(row)

		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return OORClientIncomingHint{}, false, nil
	}

	return hint, err == nil, err
}

func (s *OORClientStoreDB) UpsertIncomingMetadata(ctx context.Context,
	metadata OORClientIncomingMetadata) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.UpsertOORClientIncomingMetadata(
			ctx, sqlc.UpsertOORClientIncomingMetadataParams{
				SessionID:      metadata.SessionID,
				OutputIndex:    metadata.OutputIndex,
				RoundID:        metadata.RoundID,
				ChainDepth:     metadata.ChainDepth,
				BatchExpiry:    metadata.BatchExpiry,
				OperatorPubkey: metadata.OperatorPubkey,
				AncestryBlob:   metadata.AncestryBlob,
				MetadataBlob:   metadata.MetadataBlob,
				CreatedAt:      now,
				UpdatedAt:      now,
			},
		)
	})
}

func (s *OORClientStoreDB) SaveIncomingMetadataEffect(ctx context.Context,
	metadata []OORClientIncomingMetadata,
	effect OORClientEffectInsert) error {

	now := s.clk.Now().Unix()
	if effect.ID == "" {
		effect.ID = effect.IdempotencyKey
	}
	if effect.ID == "" {
		effect.ID = uuid.NewString()
	}
	if effect.MaxAttempts == 0 {
		effect.MaxAttempts = 10
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		for i := range metadata {
			row := metadata[i]
			err := q.UpsertOORClientIncomingMetadata(
				ctx, sqlc.UpsertOORClientIncomingMetadataParams{
					SessionID:      row.SessionID,
					OutputIndex:    row.OutputIndex,
					RoundID:        row.RoundID,
					ChainDepth:     row.ChainDepth,
					BatchExpiry:    row.BatchExpiry,
					OperatorPubkey: row.OperatorPubkey,
					AncestryBlob:   row.AncestryBlob,
					MetadataBlob:   row.MetadataBlob,
					CreatedAt:      now,
					UpdatedAt:      now,
				},
			)
			if err != nil {
				return err
			}
		}

		return q.InsertOORClientEffect(
			ctx, sqlc.InsertOORClientEffectParams{
				ID:             effect.ID,
				SessionID:      effect.SessionID,
				Direction:      effect.Direction,
				EffectType:     effect.EffectType,
				IdempotencyKey: effect.IdempotencyKey,
				MaxAttempts:    effect.MaxAttempts,
				NextAttemptAt:  now,
				CreatedAt:      now,
			},
		)
	})
}

func (s *OORClientStoreDB) ListIncomingMetadata(ctx context.Context,
	sessionID []byte) ([]OORClientIncomingMetadata, error) {

	var metadata []OORClientIncomingMetadata
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.ListOORClientIncomingMetadata(ctx, sessionID)
		if err != nil {
			return err
		}

		metadata = make([]OORClientIncomingMetadata, 0, len(rows))
		for i := range rows {
			metadata = append(
				metadata,
				oorClientIncomingMetadataFromRow(rows[i]),
			)
		}

		return nil
	})

	return metadata, err
}

func (s *OORClientStoreDB) SaveArkArtifact(ctx context.Context,
	artifact OORClientArkArtifact) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.UpsertOORClientArkArtifact(
			ctx, sqlc.UpsertOORClientArkArtifactParams{
				SessionID: artifact.SessionID,
				Phase:     artifact.Phase,
				ArkPsbt:   artifact.ArkPSBT,
				CreatedAt: now,
				UpdatedAt: now,
			},
		)
	})
}

func (s *OORClientStoreDB) LoadArkArtifact(ctx context.Context,
	sessionID []byte, phase string) (OORClientArkArtifact, error) {

	var artifact OORClientArkArtifact
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		row, err := q.GetOORClientArkArtifact(
			ctx, sqlc.GetOORClientArkArtifactParams{
				SessionID: sessionID,
				Phase:     phase,
			},
		)
		if err != nil {
			return err
		}

		artifact = oorClientArkArtifactFromRow(row)

		return nil
	})

	return artifact, err
}

func (s *OORClientStoreDB) ListArkArtifacts(ctx context.Context,
	sessionID []byte) ([]OORClientArkArtifact, error) {

	var artifacts []OORClientArkArtifact
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.ListOORClientArkArtifacts(ctx, sessionID)
		if err != nil {
			return err
		}

		artifacts = make([]OORClientArkArtifact, 0, len(rows))
		for _, row := range rows {
			artifacts = append(
				artifacts, oorClientArkArtifactFromRow(row),
			)
		}

		return nil
	})

	return artifacts, err
}

func (s *OORClientStoreDB) SaveCheckpoint(ctx context.Context,
	checkpoint OORClientCheckpoint) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.UpsertOORClientCheckpoint(
			ctx, sqlc.UpsertOORClientCheckpointParams{
				SessionID:       checkpoint.SessionID,
				CheckpointIndex: checkpoint.CheckpointIndex,
				Phase:           checkpoint.Phase,
				CheckpointPsbt:  checkpoint.CheckpointPSBT,
				CreatedAt:       now,
				UpdatedAt:       now,
			},
		)
	})
}

func (s *OORClientStoreDB) LoadCheckpoint(ctx context.Context, sessionID []byte,
	checkpointIndex int32, phase string) (OORClientCheckpoint, error) {

	var checkpoint OORClientCheckpoint
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		row, err := q.GetOORClientCheckpoint(
			ctx, sqlc.GetOORClientCheckpointParams{
				SessionID:       sessionID,
				CheckpointIndex: checkpointIndex,
				Phase:           phase,
			},
		)
		if err != nil {
			return err
		}

		checkpoint = oorClientCheckpointFromRow(row)

		return nil
	})

	return checkpoint, err
}

func (s *OORClientStoreDB) ListCheckpointsByPhase(ctx context.Context,
	sessionID []byte, phase string) ([]OORClientCheckpoint, error) {

	var checkpoints []OORClientCheckpoint
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.ListOORClientCheckpointsByPhase(
			ctx, sqlc.ListOORClientCheckpointsByPhaseParams{
				SessionID: sessionID,
				Phase:     phase,
			},
		)
		if err != nil {
			return err
		}

		checkpoints = make([]OORClientCheckpoint, 0, len(rows))
		for _, row := range rows {
			checkpoints = append(
				checkpoints, oorClientCheckpointFromRow(row),
			)
		}

		return nil
	})

	return checkpoints, err
}

func (s *OORClientStoreDB) ListCheckpoints(ctx context.Context,
	sessionID []byte) ([]OORClientCheckpoint, error) {

	var checkpoints []OORClientCheckpoint
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.ListOORClientCheckpoints(ctx, sessionID)
		if err != nil {
			return err
		}

		checkpoints = make([]OORClientCheckpoint, 0, len(rows))
		for _, row := range rows {
			checkpoints = append(
				checkpoints, oorClientCheckpointFromRow(row),
			)
		}

		return nil
	})

	return checkpoints, err
}

func (s *OORClientStoreDB) InsertEffect(ctx context.Context,
	effect OORClientEffectInsert) error {

	now := s.clk.Now().Unix()
	if effect.ID == "" {
		effect.ID = effect.IdempotencyKey
	}
	if effect.ID == "" {
		effect.ID = uuid.NewString()
	}
	if effect.MaxAttempts == 0 {
		effect.MaxAttempts = 10
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.InsertOORClientEffect(
			ctx, sqlc.InsertOORClientEffectParams{
				ID:             effect.ID,
				SessionID:      effect.SessionID,
				Direction:      effect.Direction,
				EffectType:     effect.EffectType,
				IdempotencyKey: effect.IdempotencyKey,
				MaxAttempts:    effect.MaxAttempts,
				NextAttemptAt:  now,
				CreatedAt:      now,
			},
		)
	})
}

func (s *OORClientStoreDB) ClaimDueEffects(ctx context.Context, owner string,
	limit int, lease time.Duration) ([]OORClientEffect, error) {

	if limit <= 0 {
		return nil, nil
	}

	now := s.clk.Now().Unix()
	claimUntil := s.clk.Now().Add(lease).Unix()
	claimed := make([]OORClientEffect, 0, limit)

	err := s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		ids, err := q.ListDueOORClientEffectIDs(
			ctx, sqlc.ListDueOORClientEffectIDsParams{
				NextAttemptAt: now,
				Limit:         int32(limit),
			},
		)
		if err != nil {
			return err
		}

		for _, id := range ids {
			token := uuid.NewString()
			row, err := q.ClaimOORClientEffect(
				ctx, sqlc.ClaimOORClientEffectParams{
					ID: id,
					ClaimOwner: sql.NullString{
						String: owner,
						Valid:  true,
					},
					ClaimToken: sql.NullString{
						String: token,
						Valid:  true,
					},
					ClaimUntil: sql.NullInt64{
						Int64: claimUntil,
						Valid: true,
					},
					UpdatedAt: now,
				},
			)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}

			claimed = append(claimed, oorClientEffectFromRow(row))
		}

		return nil
	})

	return claimed, err
}

func (s *OORClientStoreDB) MarkEffectDone(ctx context.Context, id,
	claimToken string) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.MarkOORClientEffectDone(
			ctx, sqlc.MarkOORClientEffectDoneParams{
				ID: id,
				ClaimToken: sql.NullString{
					String: claimToken,
					Valid:  true,
				},
				DoneAt: sql.NullInt64{
					Int64: now,
					Valid: true,
				},
			},
		)
	})
}

func (s *OORClientStoreDB) ReleaseEffectForRetry(ctx context.Context, id,
	claimToken string, retryAfter time.Duration, failure error) error {

	now := s.clk.Now()
	errText := ""
	if failure != nil {
		errText = failure.Error()
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.ReleaseOORClientEffectForRetry(
			ctx, sqlc.ReleaseOORClientEffectForRetryParams{
				ID: id,
				ClaimToken: sql.NullString{
					String: claimToken,
					Valid:  true,
				},
				NextAttemptAt: now.Add(retryAfter).Unix(),
				UpdatedAt:     now.Unix(),
				LastError: sql.NullString{
					String: errText,
					Valid:  errText != "",
				},
			},
		)
	})
}

func (s *OORClientStoreDB) ReleaseExpiredEffectClaims(
	ctx context.Context) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.ReleaseExpiredOORClientEffectClaims(
			ctx, sqlc.ReleaseExpiredOORClientEffectClaimsParams{
				ClaimUntil: sql.NullInt64{
					Int64: now,
					Valid: true,
				},
				UpdatedAt: now,
			},
		)
	})
}

func oorClientSessionFromRow(row sqlc.OorClientSession) OORClientSession {
	return OORClientSession{
		SessionID:      row.SessionID,
		Direction:      row.Direction,
		State:          row.State,
		IdempotencyKey: row.IdempotencyKey,
		RetryAfter:     row.RetryAfter,
		RetryReason:    row.RetryReason,
		FailReason:     row.FailReason,
		CompletedAt:    row.CompletedAt,
	}
}

func oorClientInputFromRow(row sqlc.OorClientInput) OORClientInput {
	return OORClientInput{
		SessionID:          row.SessionID,
		InputIndex:         row.InputIndex,
		OutpointHash:       row.OutpointHash,
		OutpointIndex:      row.OutpointIndex,
		AmountSat:          row.AmountSat,
		PkScript:           row.PkScript,
		ClientKeyFamily:    row.ClientKeyFamily,
		ClientKeyIndex:     row.ClientKeyIndex,
		ClientPubKey:       row.ClientPubKey,
		OperatorPubKey:     row.OperatorPubKey,
		ExitDelay:          row.ExitDelay,
		VtxoPolicyTemplate: row.VtxoPolicyTemplate,
		OwnerLeafScript:    row.OwnerLeafScript,
		OwnerLeafPolicy:    row.OwnerLeafPolicy,
		SpendWitnessScript: row.SpendWitnessScript,
		SpendControlBlock:  row.SpendControlBlock,
		ConditionWitness:   row.ConditionWitness,
		RequiredSequence:   row.RequiredSequence,
		RequiredLocktime:   row.RequiredLocktime,
	}
}

func oorClientRecipientFromRow(row sqlc.OorClientRecipient) OORClientRecipient {
	return OORClientRecipient{
		SessionID:          row.SessionID,
		OutputIndex:        row.OutputIndex,
		PkScript:           row.PkScript,
		ValueSat:           row.ValueSat,
		VtxoPolicyTemplate: row.VtxoPolicyTemplate,
	}
}

func oorClientIncomingHintFromRow(
	row sqlc.OorClientIncomingHint) OORClientIncomingHint {

	return OORClientIncomingHint{
		SessionID:         row.SessionID,
		RecipientPkScript: row.RecipientPkScript,
		RecipientEventID:  row.RecipientEventID,
	}
}

func oorClientIncomingMetadataFromRow(
	row sqlc.OorClientIncomingMetadatum) OORClientIncomingMetadata {

	return OORClientIncomingMetadata{
		SessionID:      row.SessionID,
		OutputIndex:    row.OutputIndex,
		RoundID:        row.RoundID,
		ChainDepth:     row.ChainDepth,
		BatchExpiry:    row.BatchExpiry,
		OperatorPubkey: row.OperatorPubkey,
		AncestryBlob:   row.AncestryBlob,
		MetadataBlob:   row.MetadataBlob,
	}
}

func oorClientArkArtifactFromRow(
	row sqlc.OorClientArkArtifact) OORClientArkArtifact {

	return OORClientArkArtifact{
		SessionID: row.SessionID,
		Phase:     row.Phase,
		ArkPSBT:   row.ArkPsbt,
	}
}

func oorClientCheckpointFromRow(
	row sqlc.OorClientCheckpoint) OORClientCheckpoint {

	return OORClientCheckpoint{
		SessionID:       row.SessionID,
		CheckpointIndex: row.CheckpointIndex,
		Phase:           row.Phase,
		CheckpointPSBT:  row.CheckpointPsbt,
	}
}

func oorClientEffectFromRow(row sqlc.OorClientEffect) OORClientEffect {
	return OORClientEffect{
		ID:             row.ID,
		SessionID:      row.SessionID,
		Direction:      row.Direction,
		EffectType:     row.EffectType,
		IdempotencyKey: row.IdempotencyKey,
		ClaimToken:     row.ClaimToken,
		Attempts:       row.Attempts,
	}
}
