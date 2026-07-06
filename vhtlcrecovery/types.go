package vhtlcrecovery

import (
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
)

const (
	// DirectionPay identifies a payer-side recovery job.
	DirectionPay = "pay"

	// DirectionReceive identifies a receiver-side recovery job.
	DirectionReceive = "receive"

	// DirectionServerIn identifies a server-owned incoming recovery job.
	DirectionServerIn = "server_in"

	// DirectionServerOut identifies a server-owned outgoing recovery job.
	DirectionServerOut = "server_out"
)

const (
	// ActionClaim spends the unilateral claim leaf with a preimage.
	ActionClaim = "claim"

	// ActionRefundWithoutReceiver spends the unilateral refund leaf that
	// does not require receiver cooperation.
	ActionRefundWithoutReceiver = "refund_without_receiver"
)

const (
	// StateArmed means recovery intent is durable but still dormant.
	StateArmed = "armed"

	// StateUnrollStarted means recovery has escalated into unroll.
	StateUnrollStarted = "unroll_started"

	// StateWaitingForTarget means unroll is materializing the target
	// output.
	StateWaitingForTarget = "waiting_for_target"

	// StateWaitingForCSV means the target output exists but CSV is pending.
	StateWaitingForCSV = "waiting_for_csv"

	// StateBuildingExitSpend means the exit transaction is being built.
	StateBuildingExitSpend = "building_exit_spend"

	// StateExitSpendBuilt means the exit transaction bytes are durable.
	StateExitSpendBuilt = "exit_spend_built"

	// StateSubmittingExitSpend means broadcast has been requested.
	StateSubmittingExitSpend = "submitting_exit_spend"

	// StateExitSpendPendingConfirmation means the exit transaction is
	// waiting for final confirmation.
	StateExitSpendPendingConfirmation = "exit_spend_pending_confirmation"

	// StateCompleted means on-chain recovery completed successfully.
	StateCompleted = "completed"

	// StateCancelled means cooperative completion won or recovery was
	// explicitly stopped before terminal chain execution.
	StateCancelled = "cancelled"

	// StateFailed means recovery reached a terminal error.
	StateFailed = "failed"
)

const (
	// ExitPolicyKindClaim identifies the vHTLC unilateral claim leaf.
	ExitPolicyKindClaim = "vhtlc_claim"

	// ExitPolicyKindRefundWithoutReceiver identifies the vHTLC unilateral
	// refund-without-receiver leaf.
	ExitPolicyKindRefundWithoutReceiver = "vhtlc_refund_without_receiver"
)

// ExitPolicyKindForAction maps a recovery action to its unroll exit policy.
func ExitPolicyKindForAction(action string) (string, error) {
	switch action {
	case ActionClaim:
		return ExitPolicyKindClaim, nil

	case ActionRefundWithoutReceiver:
		return ExitPolicyKindRefundWithoutReceiver, nil

	default:
		return "", fmt.Errorf("unknown vhtlc recovery action: %s",
			action)
	}
}

// RecoveryJob is the durable vHTLC recovery control-plane row.
type RecoveryJob struct {
	ID             string
	RequestID      string
	SwapID         []byte
	Direction      string
	Action         string
	State          string
	VTXOOutpoint   wire.OutPoint
	VTXOAmountSat  int64
	SenderPubkey   []byte
	ReceiverPubkey []byte
	ServerPubkey   []byte
	// RefundLocktime stores the vHTLC absolute locktime. It is signed to
	// match sqlc's SQLite integer mapping; policy construction validates it
	// before converting to the unsigned Bitcoin locktime domain.
	RefundLocktime                       int32
	UnilateralClaimDelay                 int32
	UnilateralRefundDelay                int32
	UnilateralRefundWithoutReceiverDelay int32
	// PreimageHash is the vHTLC payment hash. It is safe to log and index.
	PreimageHash []byte
	// ClaimPreimage is populated only for cross-process claim recovery
	// where the daemon cannot resolve swap-owned state through an
	// in-process resolver. The value must never be logged.
	ClaimPreimage           []byte
	SignerKeyFamily         int32
	SignerKeyIndex          int32
	DestinationScript       []byte
	MaxFeeRateSatPerKWeight int32
	UnrollTargetOutpoint    *wire.OutPoint
	ExitPolicyKind          string
	ExitTx                  []byte
	ExitTxid                []byte
	CooperativeTxid         []byte
	LastError               string
	CancelReason            string
	CreatedAt               time.Time
	UpdatedAt               time.Time
	ArmedAt                 *time.Time
	EscalatedAt             *time.Time
	TargetDetectedAt        *time.Time
	ExitTxBuiltAt           *time.Time
	ExitTxBroadcastAt       *time.Time
	TerminalAt              *time.Time
}

// IsTerminal reports whether the job is in a terminal state.
func (j RecoveryJob) IsTerminal() bool {
	return j.State == StateCompleted ||
		j.State == StateCancelled ||
		j.State == StateFailed
}
