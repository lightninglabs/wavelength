package waved

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/vhtlcrecovery"
	"github.com/lightninglabs/wavelength/vhtlcrecovery/coordinator"
	"github.com/lightninglabs/wavelength/vhtlcrecovery/unrollpolicy"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

//nolint:ll // Generated daemon RPC enum names are intentionally long.
const (
	rpcDirectionPay         = waverpc.VHTLCRecoveryDirection_VHTLC_RECOVERY_DIRECTION_PAY
	rpcDirectionReceive     = waverpc.VHTLCRecoveryDirection_VHTLC_RECOVERY_DIRECTION_RECEIVE
	rpcDirectionServerIn    = waverpc.VHTLCRecoveryDirection_VHTLC_RECOVERY_DIRECTION_SERVER_IN
	rpcDirectionServerOut   = waverpc.VHTLCRecoveryDirection_VHTLC_RECOVERY_DIRECTION_SERVER_OUT
	rpcDirectionUnspecified = waverpc.VHTLCRecoveryDirection_VHTLC_RECOVERY_DIRECTION_UNSPECIFIED

	rpcActionClaim       = waverpc.VHTLCRecoveryAction_VHTLC_RECOVERY_ACTION_CLAIM
	rpcActionRefund      = waverpc.VHTLCRecoveryAction_VHTLC_RECOVERY_ACTION_REFUND_WITHOUT_RECEIVER
	rpcActionUnspecified = waverpc.VHTLCRecoveryAction_VHTLC_RECOVERY_ACTION_UNSPECIFIED

	rpcStateArmed          = waverpc.VHTLCRecoveryState_VHTLC_RECOVERY_STATE_ARMED
	rpcStateUnrollStarted  = waverpc.VHTLCRecoveryState_VHTLC_RECOVERY_STATE_UNROLL_STARTED
	rpcStateWaitingTarget  = waverpc.VHTLCRecoveryState_VHTLC_RECOVERY_STATE_WAITING_FOR_TARGET
	rpcStateWaitingCSV     = waverpc.VHTLCRecoveryState_VHTLC_RECOVERY_STATE_WAITING_FOR_CSV
	rpcStateBuildingExit   = waverpc.VHTLCRecoveryState_VHTLC_RECOVERY_STATE_BUILDING_EXIT_SPEND
	rpcStateExitBuilt      = waverpc.VHTLCRecoveryState_VHTLC_RECOVERY_STATE_EXIT_SPEND_BUILT
	rpcStateSubmittingExit = waverpc.VHTLCRecoveryState_VHTLC_RECOVERY_STATE_SUBMITTING_EXIT_SPEND
	rpcStatePendingConfirm = waverpc.VHTLCRecoveryState_VHTLC_RECOVERY_STATE_EXIT_SPEND_PENDING_CONFIRMATION
	rpcStateCompleted      = waverpc.VHTLCRecoveryState_VHTLC_RECOVERY_STATE_COMPLETED
	rpcStateCancelled      = waverpc.VHTLCRecoveryState_VHTLC_RECOVERY_STATE_CANCELLED
	rpcStateFailed         = waverpc.VHTLCRecoveryState_VHTLC_RECOVERY_STATE_FAILED
	rpcStateUnspecified    = waverpc.VHTLCRecoveryState_VHTLC_RECOVERY_STATE_UNSPECIFIED
)

// ArmVHTLCRecovery persists a dormant vHTLC on-chain recovery job. The job
// remains armed until a later EscalateVHTLCRecovery call starts unroll.
func (r *RPCServer) ArmVHTLCRecovery(ctx context.Context,
	req *waverpc.ArmVHTLCRecoveryRequest) (
	*waverpc.ArmVHTLCRecoveryResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	service, err := r.requireVHTLCRecovery()
	if err != nil {
		return nil, err
	}
	if req.RequestId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "request_id "+
			"is required")
	}

	job, err := recoveryJobFromProto(req)
	if err != nil {
		return nil, err
	}

	stored, created, err := service.ArmRecovery(ctx, job)
	if err != nil {
		return nil, recoveryErrorToStatus(err)
	}

	return &waverpc.ArmVHTLCRecoveryResponse{
		RecoveryId: stored.ID,
		Created:    created,
		Status: recoveryStatusToProto(&coordinator.RecoveryStatus{
			Job: *stored,
		}),
	}, nil
}

// EscalateVHTLCRecovery starts or resumes the unroll job for a previously
// armed vHTLC recovery.
func (r *RPCServer) EscalateVHTLCRecovery(ctx context.Context,
	req *waverpc.EscalateVHTLCRecoveryRequest) (
	*waverpc.EscalateVHTLCRecoveryResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	service, err := r.requireVHTLCRecovery()
	if err != nil {
		return nil, err
	}
	if req.RecoveryId == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"recovery_id is required")
	}

	recoveryStatus, err := service.EscalateRecovery(
		ctx, req.RecoveryId, req.Reason,
		cloneRPCBytes(req.ClaimPreimage),
	)
	if err != nil {
		return nil, recoveryErrorToStatus(err)
	}

	return &waverpc.EscalateVHTLCRecoveryResponse{
		Status: recoveryStatusToProto(recoveryStatus),
	}, nil
}

// CancelVHTLCRecovery records that cooperative settlement or operator action
// made the recovery job unnecessary.
func (r *RPCServer) CancelVHTLCRecovery(ctx context.Context,
	req *waverpc.CancelVHTLCRecoveryRequest) (
	*waverpc.CancelVHTLCRecoveryResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	service, err := r.requireVHTLCRecovery()
	if err != nil {
		return nil, err
	}
	if req.RecoveryId == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"recovery_id is required")
	}

	cooperativeTxid, err := txidBytesFromString(req.CooperativeTxid)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"cooperative_txid: %v", err)
	}

	recoveryStatus, err := service.CancelRecovery(
		ctx, req.RecoveryId, req.Reason, cooperativeTxid,
	)
	if err != nil {
		if errors.Is(err, db.ErrVHTLCRecoveryJobNotFound) {
			return &waverpc.CancelVHTLCRecoveryResponse{}, nil
		}

		return nil, recoveryErrorToStatus(err)
	}

	return &waverpc.CancelVHTLCRecoveryResponse{
		Status: recoveryStatusToProto(recoveryStatus),
	}, nil
}

// GetVHTLCRecoveryStatus returns the durable recovery row joined with current
// unroll status when the job has escalated.
func (r *RPCServer) GetVHTLCRecoveryStatus(ctx context.Context,
	req *waverpc.GetVHTLCRecoveryStatusRequest) (
	*waverpc.GetVHTLCRecoveryStatusResponse, error) {

	service, err := r.requireVHTLCRecovery()
	if err != nil {
		return nil, err
	}
	if req.RecoveryId == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"recovery_id is required")
	}

	recoveryStatus, err := service.GetRecoveryStatus(ctx, req.RecoveryId)
	if err != nil {
		if errors.Is(err, db.ErrVHTLCRecoveryJobNotFound) {
			return &waverpc.GetVHTLCRecoveryStatusResponse{}, nil
		}

		return nil, recoveryErrorToStatus(err)
	}

	return &waverpc.GetVHTLCRecoveryStatusResponse{
		Found:  true,
		Status: recoveryStatusToProto(recoveryStatus),
	}, nil
}

// ListVHTLCRecoveries returns durable recovery rows for operator inspection.
func (r *RPCServer) ListVHTLCRecoveries(ctx context.Context,
	req *waverpc.ListVHTLCRecoveriesRequest) (
	*waverpc.ListVHTLCRecoveriesResponse, error) {

	service, err := r.requireVHTLCRecovery()
	if err != nil {
		return nil, err
	}

	statuses, err := service.ListRecoveryStatuses(ctx)
	if err != nil {
		return nil, recoveryErrorToStatus(err)
	}

	resp := &waverpc.ListVHTLCRecoveriesResponse{
		Statuses: make(
			[]*waverpc.VHTLCRecoveryStatus, 0, len(statuses),
		),
	}
	for i := range statuses {
		status := recoveryStatusToProto(&statuses[i])
		if !req.IncludeTerminal &&
			recoveryStateIsTerminal(status.GetState()) {

			continue
		}

		resp.Statuses = append(resp.Statuses, status)
	}

	return resp, nil
}

// requireVHTLCRecovery returns the daemon recovery service or an Unavailable
// status when wallet-dependent actors have not started yet.
func (r *RPCServer) requireVHTLCRecovery() (*coordinator.Service, error) {
	if r.server.vhtlcRecovery == nil {
		return nil, status.Errorf(codes.Unavailable, "vhtlc recovery "+
			"service not initialized")
	}

	return r.server.vhtlcRecovery, nil
}

// RegisterVHTLCRecoveryPreimageResolver wires the optional swap-owned preimage
// source used by vHTLC claim recovery. The swapruntime subserver calls this
// after it opens the daemon-owned swap store; until then claim recovery fails
// closed while refund-without-receiver recovery remains usable.
func (r *RPCServer) RegisterVHTLCRecoveryPreimageResolver(
	resolver unrollpolicy.PreimageResolver) error {

	if r == nil {
		return status.Errorf(codes.Unavailable, "vhtlc recovery "+
			"preimage registry not initialized")
	}
	if r.server == nil {
		return status.Errorf(codes.Unavailable, "vhtlc recovery "+
			"preimage registry not initialized")
	}
	if r.server.vhtlcPreimages == nil {
		return status.Errorf(codes.Unavailable, "vhtlc recovery "+
			"preimage registry not initialized")
	}

	r.server.vhtlcPreimages.SetResolver(resolver)

	return nil
}

// recoveryJobFromProto converts a public arm request into the durable recovery
// row accepted by the service layer.
func recoveryJobFromProto(req *waverpc.ArmVHTLCRecoveryRequest) (
	vhtlcrecovery.RecoveryJob, error) {

	direction, err := recoveryDirectionFromProto(req.Direction)
	if err != nil {
		return vhtlcrecovery.RecoveryJob{}, err
	}

	action, err := recoveryActionFromProto(req.Action)
	if err != nil {
		return vhtlcrecovery.RecoveryJob{}, err
	}

	outpoint, err := parseOutpointString(req.VtxoOutpoint)
	if err != nil {
		return vhtlcrecovery.RecoveryJob{}, fmt.Errorf("invalid "+
			"vtxo_outpoint: %w", err)
	}

	return vhtlcrecovery.RecoveryJob{
		RequestID:     req.RequestId,
		SwapID:        cloneRPCBytes(req.SwapId),
		Direction:     direction,
		Action:        action,
		VTXOOutpoint:  outpoint,
		VTXOAmountSat: req.VtxoAmountSat,
		SenderPubkey: cloneRPCBytes(
			req.SenderPubkey,
		),
		ReceiverPubkey: cloneRPCBytes(
			req.ReceiverPubkey,
		),
		ServerPubkey: cloneRPCBytes(
			req.ServerPubkey,
		),
		RefundLocktime:                       req.RefundLocktime,
		UnilateralClaimDelay:                 req.UnilateralClaimDelay,
		UnilateralRefundDelay:                req.UnilateralRefundDelay,
		UnilateralRefundWithoutReceiverDelay: req.UnilateralRefundWithoutReceiverDelay, //nolint:ll
		PreimageHash: cloneRPCBytes(
			req.PreimageHash,
		),
		SignerKeyFamily: req.SignerKeyFamily,
		SignerKeyIndex:  req.SignerKeyIndex,
		DestinationScript: cloneRPCBytes(
			req.DestinationScript,
		),
		MaxFeeRateSatPerKWeight: req.MaxFeeRateSatPerKw,
	}, nil
}

// recoveryDirectionFromProto maps the public direction enum to the durable SQL
// value.
func recoveryDirectionFromProto(direction waverpc.VHTLCRecoveryDirection) (
	string, error) {

	switch direction {
	case rpcDirectionPay:
		return vhtlcrecovery.DirectionPay, nil

	case rpcDirectionReceive:
		return vhtlcrecovery.DirectionReceive, nil

	case rpcDirectionServerIn:
		return vhtlcrecovery.DirectionServerIn, nil

	case rpcDirectionServerOut:
		return vhtlcrecovery.DirectionServerOut, nil

	default:
		return "", fmt.Errorf("direction is required")
	}
}

// recoveryDirectionToProto maps the durable SQL value to the public direction
// enum.
func recoveryDirectionToProto(direction string) waverpc.VHTLCRecoveryDirection {
	switch direction {
	case vhtlcrecovery.DirectionPay:
		return rpcDirectionPay

	case vhtlcrecovery.DirectionReceive:
		return rpcDirectionReceive

	case vhtlcrecovery.DirectionServerIn:
		return rpcDirectionServerIn

	case vhtlcrecovery.DirectionServerOut:
		return rpcDirectionServerOut

	default:
		return rpcDirectionUnspecified
	}
}

// recoveryActionFromProto maps the public action enum to the durable SQL value.
func recoveryActionFromProto(action waverpc.VHTLCRecoveryAction) (string,
	error) {

	switch action {
	case rpcActionClaim:
		return vhtlcrecovery.ActionClaim, nil

	case rpcActionRefund:
		return vhtlcrecovery.ActionRefundWithoutReceiver, nil

	default:
		return "", fmt.Errorf("action is required")
	}
}

// recoveryActionToProto maps the durable SQL value to the public action enum.
func recoveryActionToProto(action string) waverpc.VHTLCRecoveryAction {
	switch action {
	case vhtlcrecovery.ActionClaim:
		return rpcActionClaim

	case vhtlcrecovery.ActionRefundWithoutReceiver:
		return rpcActionRefund

	default:
		return rpcActionUnspecified
	}
}

// recoveryStateToProto maps the durable SQL state to the public state enum.
func recoveryStateToProto(state string) waverpc.VHTLCRecoveryState {
	switch state {
	case vhtlcrecovery.StateArmed:
		return rpcStateArmed

	case vhtlcrecovery.StateUnrollStarted:
		return rpcStateUnrollStarted

	case vhtlcrecovery.StateWaitingForTarget:
		return rpcStateWaitingTarget

	case vhtlcrecovery.StateWaitingForCSV:
		return rpcStateWaitingCSV

	case vhtlcrecovery.StateBuildingExitSpend:
		return rpcStateBuildingExit

	case vhtlcrecovery.StateExitSpendBuilt:
		return rpcStateExitBuilt

	case vhtlcrecovery.StateSubmittingExitSpend:
		return rpcStateSubmittingExit

	case vhtlcrecovery.StateExitSpendPendingConfirmation:
		return rpcStatePendingConfirm

	case vhtlcrecovery.StateCompleted:
		return rpcStateCompleted

	case vhtlcrecovery.StateCancelled:
		return rpcStateCancelled

	case vhtlcrecovery.StateFailed:
		return rpcStateFailed

	default:
		return rpcStateUnspecified
	}
}

// recoveryStateIsTerminal reports whether the RPC state no longer needs
// operator action unless a failed row is being investigated.
func recoveryStateIsTerminal(state waverpc.VHTLCRecoveryState) bool {
	switch state {
	case rpcStateCompleted, rpcStateCancelled, rpcStateFailed:
		return true

	default:
		return false
	}
}

// recoveryStatusToProto converts the joined service status into the daemon RPC
// response shape.
func recoveryStatusToProto(
	status *coordinator.RecoveryStatus) *waverpc.VHTLCRecoveryStatus {

	if status == nil {
		return nil
	}

	job := status.Job
	lastError := job.LastError
	if lastError == "" {
		lastError = status.UnrollFailure
	}

	return &waverpc.VHTLCRecoveryStatus{
		RecoveryId:            job.ID,
		RequestId:             job.RequestID,
		SwapId:                cloneRPCBytes(job.SwapID),
		Direction:             recoveryDirectionToProto(job.Direction),
		Action:                recoveryActionToProto(job.Action),
		State:                 recoveryStateToProto(job.State),
		VtxoOutpoint:          job.VTXOOutpoint.String(),
		VtxoAmountSat:         job.VTXOAmountSat,
		ExitPolicyKind:        job.ExitPolicyKind,
		RefundLocktime:        job.RefundLocktime,
		UnilateralClaimDelay:  job.UnilateralClaimDelay,
		UnilateralRefundDelay: job.UnilateralRefundDelay,
		UnilateralRefundWithoutReceiverDelay: job.
			UnilateralRefundWithoutReceiverDelay,
		UnrollFound:     status.UnrollFound,
		UnrollStatus:    unrollPhaseToProto(status.UnrollPhase),
		SweepTxid:       hashString(status.UnrollSweep),
		ExitTxid:        txidStringFromBytes(job.ExitTxid),
		CooperativeTxid: txidStringFromBytes(job.CooperativeTxid),
		LastError:       lastError,
		CancelReason:    job.CancelReason,
		CreatedAtUnix:   job.CreatedAt.Unix(),
		UpdatedAtUnix:   job.UpdatedAt.Unix(),
		ArmedAtUnix:     unixPtr(job.ArmedAt),
		EscalatedAtUnix: unixPtr(job.EscalatedAt),
		TerminalAtUnix:  unixPtr(job.TerminalAt),
	}
}

// recoveryErrorToStatus maps known service/store errors to stable gRPC codes.
func recoveryErrorToStatus(err error) error {
	switch {
	case errors.Is(err, db.ErrVHTLCRecoveryJobNotFound):
		return status.Errorf(codes.NotFound, "%v", err)

	case errors.Is(err, db.ErrVHTLCRecoveryIdempotencyConflict):
		return status.Errorf(codes.AlreadyExists, "%v", err)

	case errors.Is(err, db.ErrVHTLCRecoveryCannotEscalate):
		return status.Errorf(codes.FailedPrecondition, "%v", err)

	default:
		return status.Errorf(codes.Internal, "%v", err)
	}
}

// txidBytesFromString parses a canonical txid string into the byte layout used
// by the database row. Empty strings are allowed for optional fields.
func txidBytesFromString(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}

	hash, err := chainhash.NewHashFromStr(value)
	if err != nil {
		return nil, err
	}

	return cloneRPCBytes(hash[:]), nil
}

// txidStringFromBytes renders a stored txid byte slice in Bitcoin's canonical
// display order.
func txidStringFromBytes(value []byte) string {
	if len(value) != chainhash.HashSize {
		return ""
	}

	var hash chainhash.Hash
	copy(hash[:], value)

	return hash.String()
}

// hashString renders an optional chainhash pointer.
func hashString(hash *chainhash.Hash) string {
	if hash == nil {
		return ""
	}

	return hash.String()
}

// unixPtr returns the unix timestamp for an optional UTC time pointer.
func unixPtr(value *time.Time) int64 {
	if value == nil {
		return 0
	}

	return value.Unix()
}

// cloneRPCBytes returns an owned copy of bytes carried through RPC messages.
func cloneRPCBytes(value []byte) []byte {
	return append([]byte(nil), value...)
}
