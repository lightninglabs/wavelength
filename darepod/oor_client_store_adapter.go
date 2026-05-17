//nolint:ll
package darepod

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	clientdb "github.com/lightninglabs/darepo-client/db"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/oor"
)

const (
	oorClientDirectionOutgoing = "outgoing"
	oorClientDirectionIncoming = "incoming"

	oorClientStateAwaitingArkSignatures        = "awaiting_ark_signatures"
	oorClientStateAwaitingSubmitAccepted       = "awaiting_submit_accepted"
	oorClientStateAwaitingCheckpointSignatures = "awaiting_checkpoint_signatures"
	oorClientStateAwaitingFinalizeAccepted     = "awaiting_finalize_accepted"
	oorClientStateAwaitingLocalVTXOUpdate      = "awaiting_local_vtxo_update"
	oorClientStateCompleted                    = "completed"
	oorClientStateFailed                       = "failed"
	oorClientStateReceiveResolving             = "receive_resolving"
	oorClientStateReceiveNotified              = "receive_notified"
	oorClientStateReceiveAwaitingAck           = "receive_awaiting_ack"
	oorClientStateReceiveCompleted             = "receive_completed"

	oorClientArkPhaseUnsigned         = "unsigned"
	oorClientArkPhaseArkSigned        = "ark_signed"
	oorClientArkPhaseAccepted         = "accepted"
	oorClientArkPhaseFinalizedContext = "finalized_context"

	oorClientCheckpointPhaseUnsigned  = "unsigned"
	oorClientCheckpointPhaseCosigned  = "cosigned"
	oorClientCheckpointPhaseFinalized = "finalized"
)

type oorClientSQLSessionStore struct {
	store          *clientdb.OORClientStoreDB
	limits         oor.ReceiveLimits
	enqueueEffects bool
}

// NewOORClientSQLSessionStore adapts the client OOR SQL store to the
// coordinator's session-store boundary.
func NewOORClientSQLSessionStore(store *clientdb.OORClientStoreDB,
	limits oor.ReceiveLimits,
	enqueueEffects bool) oor.OORClientSessionStore {

	return &oorClientSQLSessionStore{
		store:          store,
		limits:         limits,
		enqueueEffects: enqueueEffects,
	}
}

var _ oor.OORClientSessionStore = (*oorClientSQLSessionStore)(nil)
var _ oor.OORClientEffectStore = (*oorClientSQLSessionStore)(nil)
var _ oor.OORClientIncomingEffectStore = (*oorClientSQLSessionStore)(nil)

func (s *oorClientSQLSessionStore) LoadActiveSessions(ctx context.Context) (
	[]oor.StoredClientSession, error) {

	rows, err := s.store.ListActiveSessions(ctx)
	if err != nil {
		return nil, err
	}

	sessions := make([]oor.StoredClientSession, 0, len(rows))
	for i := range rows {
		session, err := s.loadStoredSession(ctx, rows[i])
		if err != nil {
			return nil, err
		}

		sessions = append(sessions, session)
	}

	return sessions, nil
}

func (s *oorClientSQLSessionStore) FindOutgoingByIdempotencyKey(
	ctx context.Context, idempotencyKey string) (oor.SessionID, bool,
	error) {

	row, found, err := s.store.FindOutgoingByIdempotencyKey(
		ctx, idempotencyKey,
	)
	if err != nil || !found {
		return oor.SessionID{}, false, err
	}

	sessionID, err := oor.SessionIDFromBytes(row.SessionID)
	if err != nil {
		return oor.SessionID{}, false, err
	}

	return sessionID, true, nil
}

func (s *oorClientSQLSessionStore) ClaimDueOORClientEffects(ctx context.Context,
	owner string, limit int, lease time.Duration) ([]oor.OORClientEffect,
	error) {

	rows, err := s.store.ClaimDueEffects(ctx, owner, limit, lease)
	if err != nil {
		return nil, err
	}

	effects := make([]oor.OORClientEffect, 0, len(rows))
	for i := range rows {
		effect, err := oorEffectFromRow(rows[i])
		if err != nil {
			return nil, err
		}

		effects = append(effects, effect)
	}

	return effects, nil
}

func (s *oorClientSQLSessionStore) MarkOORClientEffectDone(ctx context.Context,
	id, claimToken string) error {

	return s.store.MarkEffectDone(ctx, id, claimToken)
}

func (s *oorClientSQLSessionStore) ReleaseOORClientEffectForRetry(
	ctx context.Context, id, claimToken string, retryAfter time.Duration,
	failure error) error {

	return s.store.ReleaseEffectForRetry(
		ctx, id, claimToken, retryAfter, failure,
	)
}

func (s *oorClientSQLSessionStore) ReleaseExpiredOORClientEffectClaims(
	ctx context.Context) error {

	return s.store.ReleaseExpiredEffectClaims(ctx)
}

func (s *oorClientSQLSessionStore) SaveIncomingMetadataEffect(
	ctx context.Context, sessionID oor.SessionID,
	matches []oor.IncomingMetadataMatch) error {

	sessionIDBytes := oor.SessionIDBytes(sessionID)
	metadata := make([]clientdb.OORClientIncomingMetadata, 0, len(matches))
	for i := range matches {
		raw, err := oor.EncodeIncomingMetadataMatch(matches[i])
		if err != nil {
			return err
		}

		row := clientdb.OORClientIncomingMetadata{
			SessionID:   sessionIDBytes,
			OutputIndex: int32(matches[i].OutputIndex),
			RoundID:     []byte(matches[i].Metadata.RoundID),
			ChainDepth: sql.NullInt32{
				Int32: int32(matches[i].Metadata.ChainDepth),
				Valid: true,
			},
			BatchExpiry: sql.NullInt32{
				Int32: matches[i].Metadata.BatchExpiry,
				Valid: true,
			},
			MetadataBlob: raw,
		}
		if matches[i].Metadata.OperatorKey != nil {
			row.OperatorPubkey = matches[i].Metadata.OperatorKey.
				SerializeCompressed()
		}

		metadata = append(metadata, row)
	}

	effectID := oor.OORClientEffectID(
		sessionID, oor.OORClientEffectMaterializeIncomingVTXOs,
	)

	return s.store.SaveIncomingMetadataEffect(
		ctx, metadata, clientdb.OORClientEffectInsert{
			ID:             effectID,
			SessionID:      sessionIDBytes,
			Direction:      oorClientDirectionIncoming,
			EffectType:     oor.OORClientEffectMaterializeIncomingVTXOs,
			IdempotencyKey: effectID,
		},
	)
}

func (s *oorClientSQLSessionStore) BuildMaterializeIncomingVTXOsRequest(
	ctx context.Context, sessionID oor.SessionID) (
	*oor.MaterializeIncomingVTXOsRequest, error) {

	sessionBytes := oor.SessionIDBytes(sessionID)
	row, found, err := s.store.GetSession(ctx, sessionBytes)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("incoming session %s not found",
			sessionID)
	}

	snapshot, err := s.loadIncomingSnapshot(ctx, row)
	if err != nil {
		return nil, err
	}

	state, err := oor.IncomingStateFromSnapshot(snapshot)
	if err != nil {
		return nil, err
	}

	notified, ok := state.(*oor.ReceiveNotified)
	if !ok {
		return nil, fmt.Errorf("incoming session %s is %T, want "+
			"*oor.ReceiveNotified", sessionID, state)
	}

	recipients, err := oor.ExtractArkRecipients(notified.ArkPSBT)
	if err != nil {
		return nil, err
	}

	rows, err := s.store.ListIncomingMetadata(ctx, sessionBytes)
	if err != nil {
		return nil, err
	}

	matches := make([]oor.IncomingMetadataMatch, 0, len(rows))
	for i := range rows {
		if len(rows[i].MetadataBlob) == 0 {
			return nil, fmt.Errorf("incoming metadata row %d for "+
				"session %s missing metadata blob",
				rows[i].OutputIndex, sessionID)
		}

		match, err := oor.DecodeIncomingMetadataMatchWithLimits(
			rows[i].MetadataBlob, s.limits,
		)
		if err != nil {
			return nil, err
		}

		matches = append(matches, match)
	}

	return &oor.MaterializeIncomingVTXOsRequest{
		SessionID:            sessionID,
		ArkPSBT:              notified.ArkPSBT,
		FinalCheckpointPSBTs: notified.FinalCheckpointPSBTs,
		Recipients:           recipients,
		MetadataMatches:      matches,
		AncestorPackages:     notified.AncestorPackages,
	}, nil
}

func (s *oorClientSQLSessionStore) SaveOutgoingSession(ctx context.Context,
	snapshot *oor.OutgoingSnapshot) error {

	if snapshot == nil {
		return fmt.Errorf("outgoing snapshot must be provided")
	}

	sessionID := oor.SessionIDBytes(snapshot.SessionID)
	state, err := outgoingStateForPhase(snapshot.Phase)
	if err != nil {
		return err
	}

	bundle := clientdb.OORClientSessionBundle{
		Session: clientdb.OORClientSession{
			SessionID:      sessionID,
			Direction:      oorClientDirectionOutgoing,
			State:          state,
			IdempotencyKey: nullString(snapshot.IdempotencyKey),
			RetryAfter:     nullDuration(snapshot.RetryAfter),
			RetryReason:    nullString(snapshot.FailReason),
			FailReason:     failReasonForOutgoing(snapshot),
			CompletedAt:    completedAtForOutgoing(snapshot),
		},
	}
	if s.enqueueEffects {
		appendSessionEffect(
			&bundle, snapshot.SessionID, oorClientDirectionOutgoing,
		)
	}

	for i := range snapshot.TransferInputSnapshots {
		row, err := inputRowFromSnapshot(
			sessionID, int32(i), snapshot.TransferInputSnapshots[i],
		)
		if err != nil {
			return err
		}

		bundle.Inputs = append(bundle.Inputs, row)
	}

	for i := range snapshot.Recipients {
		recipient := snapshot.Recipients[i]
		bundle.Recipients = append(
			bundle.Recipients, clientdb.OORClientRecipient{
				SessionID:   sessionID,
				OutputIndex: int32(i),
				PkScript:    recipient.PkScript,
				ValueSat:    int64(recipient.Value),
				VtxoPolicyTemplate: recipient.
					VTXOPolicyTemplate,
			},
		)
	}

	arkPhase := outgoingArkArtifactPhase(snapshot.Phase)
	if arkPhase != "" && len(snapshot.ArkPSBT) != 0 {
		bundle.ArkArtifacts = append(
			bundle.ArkArtifacts, clientdb.OORClientArkArtifact{
				SessionID: sessionID,
				Phase:     arkPhase,
				ArkPSBT:   snapshot.ArkPSBT,
			},
		)
	}

	checkpointPhase := outgoingCheckpointPhase(snapshot.Phase)
	for i := range snapshot.CheckpointPSBTs {
		bundle.Checkpoints = append(
			bundle.Checkpoints, clientdb.OORClientCheckpoint{
				SessionID:       sessionID,
				CheckpointIndex: int32(i),
				Phase:           checkpointPhase,
				CheckpointPSBT:  snapshot.CheckpointPSBTs[i],
			},
		)
	}

	return s.store.UpsertSessionBundle(ctx, bundle)
}

func (s *oorClientSQLSessionStore) SaveIncomingSession(ctx context.Context,
	snapshot *oor.IncomingSnapshot) error {

	if snapshot == nil {
		return fmt.Errorf("incoming snapshot must be provided")
	}

	sessionID := oor.SessionIDBytes(snapshot.SessionID)
	state, err := incomingStateForPhase(snapshot.Phase)
	if err != nil {
		return err
	}

	bundle := clientdb.OORClientSessionBundle{
		Session: clientdb.OORClientSession{
			SessionID:   sessionID,
			Direction:   oorClientDirectionIncoming,
			State:       state,
			FailReason:  failReasonForIncoming(snapshot),
			CompletedAt: completedAtForIncoming(snapshot),
		},
	}
	if s.enqueueEffects {
		appendSessionEffect(
			&bundle, snapshot.SessionID, oorClientDirectionIncoming,
		)
	}

	if snapshot.Phase == oor.IncomingPhaseResolvePending {
		bundle.IncomingHint = &clientdb.OORClientIncomingHint{
			SessionID:         sessionID,
			RecipientPkScript: snapshot.RecipientPkScript,
			RecipientEventID:  int64(snapshot.RecipientEventID),
		}

		return s.store.UpsertSessionBundle(ctx, bundle)
	}

	if len(snapshot.ArkPSBT) != 0 {
		bundle.ArkArtifacts = append(
			bundle.ArkArtifacts, clientdb.OORClientArkArtifact{
				SessionID: sessionID,
				Phase:     oorClientArkPhaseFinalizedContext,
				ArkPSBT:   snapshot.ArkPSBT,
			},
		)
	}

	for i := range snapshot.CheckpointPSBTs {
		bundle.Checkpoints = append(
			bundle.Checkpoints, clientdb.OORClientCheckpoint{
				SessionID:       sessionID,
				CheckpointIndex: int32(i),
				Phase:           oorClientCheckpointPhaseFinalized,
				CheckpointPSBT:  snapshot.CheckpointPSBTs[i],
			},
		)
	}

	return s.store.UpsertSessionBundle(ctx, bundle)
}

func (s *oorClientSQLSessionStore) SavePendingIncomingHint(
	ctx context.Context, req *oor.ResolveIncomingTransferRequest) error {

	if req == nil {
		return nil
	}

	return s.store.UpsertIncomingHint(ctx, clientdb.OORClientIncomingHint{
		SessionID:         oor.SessionIDBytes(req.SessionID),
		RecipientPkScript: req.RecipientPkScript,
		RecipientEventID:  int64(req.RecipientEventID),
	})
}

func (s *oorClientSQLSessionStore) loadStoredSession(ctx context.Context,
	row clientdb.OORClientSession) (oor.StoredClientSession, error) {

	switch row.Direction {
	case oorClientDirectionOutgoing:
		snapshot, err := s.loadOutgoingSnapshot(ctx, row)
		if err != nil {
			return oor.StoredClientSession{}, err
		}

		return oor.StoredClientSession{
			Direction: oor.SessionDirectionOutgoing,
			Outgoing:  snapshot,
		}, nil

	case oorClientDirectionIncoming:
		snapshot, err := s.loadIncomingSnapshot(ctx, row)
		if err != nil {
			return oor.StoredClientSession{}, err
		}

		return oor.StoredClientSession{
			Direction: oor.SessionDirectionIncoming,
			Incoming:  snapshot,
		}, nil

	default:
		return oor.StoredClientSession{}, fmt.Errorf("unknown OOR "+
			"client session direction: %s", row.Direction)
	}
}

func (s *oorClientSQLSessionStore) loadOutgoingSnapshot(ctx context.Context,
	row clientdb.OORClientSession) (*oor.OutgoingSnapshot, error) {

	sessionID, err := oor.SessionIDFromBytes(row.SessionID)
	if err != nil {
		return nil, err
	}

	phase, err := outgoingPhaseForState(row.State)
	if err != nil {
		return nil, err
	}

	inputRows, err := s.store.ListInputs(ctx, row.SessionID)
	if err != nil {
		return nil, err
	}

	inputs := make([]*oor.TransferInputSnapshot, 0, len(inputRows))
	for i := range inputRows {
		input, err := inputSnapshotFromRow(inputRows[i])
		if err != nil {
			return nil, err
		}

		inputs = append(inputs, input)
	}

	recipients, err := s.loadRecipients(ctx, row.SessionID)
	if err != nil {
		return nil, err
	}

	ark, err := s.loadArkPSBT(
		ctx, row.SessionID, outgoingArkArtifactPhase(phase),
	)
	if err != nil {
		return nil, err
	}

	checkpoints, err := s.loadCheckpoints(
		ctx, row.SessionID, outgoingCheckpointPhase(phase),
	)
	if err != nil {
		return nil, err
	}

	return &oor.OutgoingSnapshot{
		Version:                4,
		SessionID:              sessionID,
		Phase:                  phase,
		ArkPSBT:                ark,
		CheckpointPSBTs:        checkpoints,
		TransferInputSnapshots: inputs,
		Recipients:             recipients,
		RetryAfter:             durationFromNull(row.RetryAfter),
		FailReason: stringFromNull(
			row.RetryReason, row.FailReason,
		),
		IdempotencyKey: stringFromNull(row.IdempotencyKey),
	}, nil
}

func (s *oorClientSQLSessionStore) loadIncomingSnapshot(ctx context.Context,
	row clientdb.OORClientSession) (*oor.IncomingSnapshot, error) {

	sessionID, err := oor.SessionIDFromBytes(row.SessionID)
	if err != nil {
		return nil, err
	}

	phase, err := incomingPhaseForState(row.State)
	if err != nil {
		return nil, err
	}

	snapshot := &oor.IncomingSnapshot{
		Version:    1,
		SessionID:  sessionID,
		Phase:      phase,
		FailReason: stringFromNull(row.FailReason),
	}

	if hint, found, err := s.store.GetIncomingHint(
		ctx, row.SessionID,
	); err != nil {
		return nil, err
	} else if found {
		snapshot.RecipientPkScript = hint.RecipientPkScript
		snapshot.RecipientEventID = uint64(hint.RecipientEventID)
	}

	if phase == oor.IncomingPhaseResolvePending ||
		phase == oor.IncomingPhaseAckPending ||
		phase == oor.IncomingPhaseCompleted ||
		phase == oor.IncomingPhaseFailed {
		return snapshot, nil
	}

	snapshot.ArkPSBT, err = s.loadArkPSBT(
		ctx, row.SessionID, oorClientArkPhaseFinalizedContext,
	)
	if err != nil {
		return nil, err
	}

	snapshot.CheckpointPSBTs, err = s.loadCheckpoints(
		ctx, row.SessionID, oorClientCheckpointPhaseFinalized,
	)
	if err != nil {
		return nil, err
	}

	return snapshot, nil
}

func (s *oorClientSQLSessionStore) loadRecipients(ctx context.Context,
	sessionID []byte) ([]oortx.RecipientOutput, error) {

	rows, err := s.store.ListRecipients(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	recipients := make([]oortx.RecipientOutput, 0, len(rows))
	for i := range rows {
		recipients = append(recipients, oortx.RecipientOutput{
			PkScript:           rows[i].PkScript,
			Value:              btcutilAmount(rows[i].ValueSat),
			VTXOPolicyTemplate: rows[i].VtxoPolicyTemplate,
		})
	}

	return recipients, nil
}

func (s *oorClientSQLSessionStore) loadArkPSBT(ctx context.Context,
	sessionID []byte, phase string) ([]byte, error) {

	if phase == "" {
		return nil, nil
	}

	artifact, err := s.store.LoadArkArtifact(ctx, sessionID, phase)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}

		return nil, err
	}

	return artifact.ArkPSBT, nil
}

func (s *oorClientSQLSessionStore) loadCheckpoints(
	ctx context.Context, sessionID []byte, phase string) ([][]byte, error) {

	if phase == "" {
		return nil, nil
	}

	rows, err := s.store.ListCheckpointsByPhase(ctx, sessionID, phase)
	if err != nil {
		return nil, err
	}

	checkpoints := make([][]byte, 0, len(rows))
	for i := range rows {
		checkpoints = append(checkpoints, rows[i].CheckpointPSBT)
	}

	return checkpoints, nil
}

func inputRowFromSnapshot(sessionID []byte, idx int32,
	snapshot *oor.TransferInputSnapshot) (clientdb.OORClientInput, error) {

	if snapshot == nil {
		return clientdb.OORClientInput{}, fmt.Errorf("transfer input " +
			"snapshot must be provided")
	}

	conditionWitness, err := json.Marshal(snapshot.ConditionWitness)
	if err != nil {
		return clientdb.OORClientInput{}, err
	}

	return clientdb.OORClientInput{
		SessionID:          sessionID,
		InputIndex:         idx,
		OutpointHash:       snapshot.Outpoint.Hash.CloneBytes(),
		OutpointIndex:      int32(snapshot.Outpoint.Index),
		AmountSat:          snapshot.AmountSat,
		PkScript:           snapshot.PkScript,
		ClientKeyFamily:    snapshot.ClientKeyFamily,
		ClientKeyIndex:     int32(snapshot.ClientKeyIndex),
		ClientPubKey:       snapshot.ClientPubKey,
		OperatorPubKey:     snapshot.OperatorPubKey,
		ExitDelay:          int32(snapshot.ExitDelay),
		VtxoPolicyTemplate: snapshot.VTXOPolicyTemplate,
		OwnerLeafScript:    snapshot.OwnerLeafScript,
		OwnerLeafPolicy:    snapshot.OwnerLeafPolicy,
		SpendWitnessScript: snapshot.SpendWitnessScript,
		SpendControlBlock:  snapshot.SpendControlBlock,
		ConditionWitness:   conditionWitness,
		RequiredSequence: sql.NullInt32{
			Int32: int32(snapshot.RequiredSequence),
			Valid: true,
		},
		RequiredLocktime: sql.NullInt32{
			Int32: int32(snapshot.RequiredLockTime),
			Valid: true,
		},
	}, nil
}

func inputSnapshotFromRow(row clientdb.OORClientInput) (
	*oor.TransferInputSnapshot, error) {

	var outpointHash chainhash.Hash
	if len(row.OutpointHash) != chainhash.HashSize {
		return nil, fmt.Errorf("outpoint hash must be %d bytes",
			chainhash.HashSize)
	}
	copy(outpointHash[:], row.OutpointHash)

	var conditionWitness [][]byte
	if len(row.ConditionWitness) != 0 {
		if err := json.Unmarshal(
			row.ConditionWitness, &conditionWitness,
		); err != nil {
			return nil, err
		}
	}

	return &oor.TransferInputSnapshot{
		Outpoint: wire.OutPoint{
			Hash:  outpointHash,
			Index: uint32(row.OutpointIndex),
		},
		AmountSat:          row.AmountSat,
		ClientKeyFamily:    row.ClientKeyFamily,
		ClientKeyIndex:     uint32(row.ClientKeyIndex),
		ClientPubKey:       row.ClientPubKey,
		OperatorPubKey:     row.OperatorPubKey,
		ExitDelay:          uint32(row.ExitDelay),
		OwnerLeafScript:    row.OwnerLeafScript,
		OwnerLeafPolicy:    row.OwnerLeafPolicy,
		VTXOPolicyTemplate: row.VtxoPolicyTemplate,
		PkScript:           row.PkScript,
		SpendWitnessScript: row.SpendWitnessScript,
		SpendControlBlock:  row.SpendControlBlock,
		ConditionWitness:   conditionWitness,
		RequiredSequence:   uint32(row.RequiredSequence.Int32),
		RequiredLockTime:   uint32(row.RequiredLocktime.Int32),
	}, nil
}

func oorEffectFromRow(row clientdb.OORClientEffect) (oor.OORClientEffect,
	error) {

	sessionID, err := oor.SessionIDFromBytes(row.SessionID)
	if err != nil {
		return oor.OORClientEffect{}, err
	}

	direction, err := oorDirectionFromRow(row.Direction)
	if err != nil {
		return oor.OORClientEffect{}, err
	}

	return oor.OORClientEffect{
		ID:         row.ID,
		SessionID:  sessionID,
		Direction:  direction,
		EffectType: row.EffectType,
		ClaimToken: row.ClaimToken.String,
		Attempts:   row.Attempts,
	}, nil
}

func oorDirectionFromRow(direction string) (oor.SessionDirection, error) {
	switch direction {
	case oorClientDirectionOutgoing:
		return oor.SessionDirectionOutgoing, nil

	case oorClientDirectionIncoming:
		return oor.SessionDirectionIncoming, nil

	default:
		return oor.SessionDirectionAll, fmt.Errorf("unknown OOR "+
			"client effect direction: %s", direction)
	}
}

func outgoingStateForPhase(phase oor.OutgoingPhase) (string, error) {
	switch phase {
	case oor.OutgoingPhaseArkSignRequested:
		return oorClientStateAwaitingArkSignatures, nil

	case oor.OutgoingPhaseSubmitSent:
		return oorClientStateAwaitingSubmitAccepted, nil

	case oor.OutgoingPhaseCoSigned:
		return oorClientStateAwaitingCheckpointSignatures, nil

	case oor.OutgoingPhaseFinalizeSent:
		return oorClientStateAwaitingFinalizeAccepted, nil

	case oor.OutgoingPhaseLocalVTXOUpdate:
		return oorClientStateAwaitingLocalVTXOUpdate, nil

	case oor.OutgoingPhaseCompleted:
		return oorClientStateCompleted, nil

	case oor.OutgoingPhaseFailed:
		return oorClientStateFailed, nil

	default:
		return "", fmt.Errorf("unknown outgoing phase: %s", phase)
	}
}

func outgoingPhaseForState(state string) (oor.OutgoingPhase, error) {
	switch state {
	case oorClientStateAwaitingArkSignatures:
		return oor.OutgoingPhaseArkSignRequested, nil

	case oorClientStateAwaitingSubmitAccepted:
		return oor.OutgoingPhaseSubmitSent, nil

	case oorClientStateAwaitingCheckpointSignatures:
		return oor.OutgoingPhaseCoSigned, nil

	case oorClientStateAwaitingFinalizeAccepted:
		return oor.OutgoingPhaseFinalizeSent, nil

	case oorClientStateAwaitingLocalVTXOUpdate:
		return oor.OutgoingPhaseLocalVTXOUpdate, nil

	case oorClientStateCompleted:
		return oor.OutgoingPhaseCompleted, nil

	case oorClientStateFailed:
		return oor.OutgoingPhaseFailed, nil

	default:
		return "", fmt.Errorf("unknown outgoing session state: %s",
			state)
	}
}

func appendSessionEffect(bundle *clientdb.OORClientSessionBundle,
	sessionID oor.SessionID, direction string) {

	if bundle == nil {
		return
	}

	effectType := effectTypeForStoredState(bundle.Session.State)
	if effectType == "" {
		return
	}

	bundle.Effects = append(bundle.Effects, clientdb.OORClientEffectInsert{
		ID:             oor.OORClientEffectID(sessionID, effectType),
		SessionID:      bundle.Session.SessionID,
		Direction:      direction,
		EffectType:     effectType,
		IdempotencyKey: oor.OORClientEffectID(sessionID, effectType),
	})
}

func effectTypeForStoredState(state string) string {
	switch state {
	case oorClientStateAwaitingArkSignatures:
		return oor.OORClientEffectRequestArkSignatures

	case oorClientStateAwaitingSubmitAccepted:
		return oor.OORClientEffectSendSubmitPackage

	case oorClientStateAwaitingCheckpointSignatures:
		return oor.OORClientEffectRequestCheckpointSignatures

	case oorClientStateAwaitingFinalizeAccepted:
		return oor.OORClientEffectSendFinalizePackage

	case oorClientStateAwaitingLocalVTXOUpdate:
		return oor.OORClientEffectMarkInputsSpent

	case oorClientStateReceiveResolving:
		return oor.OORClientEffectQueryIncomingTransfer

	case oorClientStateReceiveNotified:
		return oor.OORClientEffectQueryIncomingMetadata

	case oorClientStateReceiveAwaitingAck:
		return oor.OORClientEffectSendIncomingAck

	default:
		return ""
	}
}

func incomingStateForPhase(phase oor.IncomingPhase) (string, error) {
	switch phase {
	case oor.IncomingPhaseResolvePending:
		return oorClientStateReceiveResolving, nil

	case oor.IncomingPhaseMaterializePending:
		return oorClientStateReceiveNotified, nil

	case oor.IncomingPhaseAckPending:
		return oorClientStateReceiveAwaitingAck, nil

	case oor.IncomingPhaseCompleted:
		return oorClientStateReceiveCompleted, nil

	case oor.IncomingPhaseFailed:
		return oorClientStateFailed, nil

	default:
		return "", fmt.Errorf("unknown incoming phase: %s", phase)
	}
}

func incomingPhaseForState(state string) (oor.IncomingPhase, error) {
	switch state {
	case oorClientStateReceiveResolving:
		return oor.IncomingPhaseResolvePending, nil

	case oorClientStateReceiveNotified:
		return oor.IncomingPhaseMaterializePending, nil

	case oorClientStateReceiveAwaitingAck:
		return oor.IncomingPhaseAckPending, nil

	case oorClientStateReceiveCompleted:
		return oor.IncomingPhaseCompleted, nil

	case oorClientStateFailed:
		return oor.IncomingPhaseFailed, nil

	default:
		return "", fmt.Errorf("unknown incoming session state: %s",
			state)
	}
}

func outgoingArkArtifactPhase(phase oor.OutgoingPhase) string {
	switch phase {
	case oor.OutgoingPhaseArkSignRequested:
		return oorClientArkPhaseUnsigned

	case oor.OutgoingPhaseSubmitSent:
		return oorClientArkPhaseArkSigned

	case oor.OutgoingPhaseCoSigned, oor.OutgoingPhaseFinalizeSent:
		return oorClientArkPhaseAccepted

	default:
		return ""
	}
}

func outgoingCheckpointPhase(phase oor.OutgoingPhase) string {
	switch phase {
	case oor.OutgoingPhaseArkSignRequested, oor.OutgoingPhaseSubmitSent:
		return oorClientCheckpointPhaseUnsigned

	case oor.OutgoingPhaseCoSigned:
		return oorClientCheckpointPhaseCosigned

	case oor.OutgoingPhaseFinalizeSent:
		return oorClientCheckpointPhaseFinalized

	default:
		return ""
	}
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func nullDuration(value time.Duration) sql.NullInt64 {
	return sql.NullInt64{Int64: int64(value), Valid: value != 0}
}

func durationFromNull(value sql.NullInt64) time.Duration {
	if !value.Valid {
		return 0
	}

	return time.Duration(value.Int64)
}

func stringFromNull(values ...sql.NullString) string {
	for i := range values {
		if values[i].Valid {
			return values[i].String
		}
	}

	return ""
}

func failReasonForOutgoing(snapshot *oor.OutgoingSnapshot) sql.NullString {
	if snapshot.Phase != oor.OutgoingPhaseFailed {
		return sql.NullString{}
	}

	return nullString(snapshot.FailReason)
}

func failReasonForIncoming(snapshot *oor.IncomingSnapshot) sql.NullString {
	if snapshot.Phase != oor.IncomingPhaseFailed {
		return sql.NullString{}
	}

	return nullString(snapshot.FailReason)
}

func completedAtForOutgoing(snapshot *oor.OutgoingSnapshot) sql.NullInt64 {
	if snapshot.Phase != oor.OutgoingPhaseCompleted &&
		snapshot.Phase != oor.OutgoingPhaseFailed {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: time.Now().Unix(), Valid: true}
}

func completedAtForIncoming(snapshot *oor.IncomingSnapshot) sql.NullInt64 {
	if snapshot.Phase != oor.IncomingPhaseCompleted &&
		snapshot.Phase != oor.IncomingPhaseFailed {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: time.Now().Unix(), Valid: true}
}

func btcutilAmount(value int64) btcutil.Amount {
	return btcutil.Amount(value)
}
