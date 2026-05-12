package fraud

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightninglabs/darepo/batchwatcher"
)

// VTXOStore loads the persisted VTXO status needed by checkpoint planning.
type VTXOStore interface {
	// GetVTXO loads the persisted VTXO state for outpoint.
	GetVTXO(context.Context,
		wire.OutPoint) (*batchwatcher.RecoveryVTXO, error)
}

// CheckpointLookup loads finalized OOR checkpoint transactions.
type CheckpointLookup interface {
	// LoadCheckpointTxByInput returns the broadcastable checkpoint
	// transaction that spends input, if one exists.
	LoadCheckpointTxByInput(context.Context, wire.OutPoint) (*wire.MsgTx,
		bool, error)
}

// ForfeitLookup builds finalized forfeit broadcast plans.
type ForfeitLookup interface {
	// PlanForfeit returns the transactions required to confirm the
	// forfeit response for outpoint.
	PlanForfeit(context.Context, wire.OutPoint) (*ResponsePlan, error)
}

// CheckpointSweepStore loads persisted data needed to sweep a checkpoint.
type CheckpointSweepStore interface {
	// LoadCheckpointSweepInfoByInput returns the data needed to sweep the
	// checkpoint output for an OOR input.
	LoadCheckpointSweepInfoByInput(context.Context, wire.OutPoint) (
		*CheckpointSweepInfo, bool, error)
}

// CheckpointPlanner resolves VTXO-on-chain notifications into fraud response
// jobs.
type CheckpointPlanner struct {
	// VTXOStore loads the persisted VTXO row used to decide whether an
	// observed on-chain leaf is in a state that warrants a checkpoint
	// broadcast.
	VTXOStore VTXOStore

	// CheckpointLookup returns the broadcastable finalized checkpoint
	// transaction that previously spent the VTXO input, if one exists.
	CheckpointLookup CheckpointLookup

	// ForfeitLookup returns the finalized forfeit broadcast plan for a
	// forfeited VTXO input.
	ForfeitLookup ForfeitLookup

	// CheckpointSweepStore optionally provides tap tree metadata used to
	// validate that checkpoint output 0 is the expected checkpoint output
	// before the transaction is broadcast.
	CheckpointSweepStore CheckpointSweepStore

	// CheckpointPolicy is required when CheckpointSweepStore is set.
	CheckpointPolicy arkscript.CheckpointPolicy
}

// CheckpointPlan is the response transaction selected for an on-chain VTXO
// leaf notification.
type CheckpointPlan struct {
	// CheckpointTx is the finalized OOR checkpoint that the operator must
	// broadcast to race the client's CSV-timeout exit path.
	CheckpointTx *wire.MsgTx
	ForfeitPlan  *ResponsePlan
}

// PlanCheckpoint returns the stored response transaction for an on-chain
// VTXO leaf.
func (p *CheckpointPlanner) PlanCheckpoint(ctx context.Context,
	msg *batchwatcher.VTXOOnChainNotification) (*CheckpointPlan, bool,
	error) {

	if msg == nil {
		return nil, false, fmt.Errorf("notification is nil")
	}
	if p == nil {
		return nil, false, fmt.Errorf("checkpoint planner is nil")
	}
	if p.VTXOStore == nil {
		return nil, false, fmt.Errorf("vtxo store is nil")
	}
	if p.CheckpointLookup == nil {
		return nil, false, fmt.Errorf("checkpoint lookup is nil")
	}

	vtxo, err := p.VTXOStore.GetVTXO(ctx, msg.VTXOOutpoint)
	if err != nil {
		return nil, false, fmt.Errorf("load vtxo %s: %w",
			msg.VTXOOutpoint, err)
	}
	if vtxo == nil {
		return nil, false, nil
	}
	if vtxo.Status == batchwatcher.VTXOStatusForfeited {
		forfeitPlan, err := p.planForfeit(ctx, msg.VTXOOutpoint)
		if err != nil {
			return nil, true, err
		}

		return &CheckpointPlan{
			ForfeitPlan: forfeitPlan,
		}, true, nil
	}

	checkpointTx, found, err := p.CheckpointLookup.
		LoadCheckpointTxByInput(
			ctx, msg.VTXOOutpoint,
		)
	if err != nil {
		return nil, true, fmt.Errorf("load checkpoint tx: %w", err)
	}
	if !found {
		if vtxo.Status != batchwatcher.VTXOStatusSpent {
			return nil, false, nil
		}

		return nil, true, fmt.Errorf("spent vtxo %s has no finalized "+
			"checkpoint", msg.VTXOOutpoint)
	}

	// Treat the finalized checkpoint as authoritative once it exists. The
	// VTXO status can lag the notification path, but a broadcastable
	// checkpoint for this input means the OOR finalize path completed.
	err = validateCheckpointPlan(msg.VTXOOutpoint, checkpointTx)
	if err != nil {
		return nil, true, err
	}
	if p.CheckpointSweepStore != nil {
		err = p.validateCheckpointOutput(
			ctx, msg.VTXOOutpoint, checkpointTx,
		)
		if err != nil {
			return nil, true, err
		}
	}

	return &CheckpointPlan{
		CheckpointTx: checkpointTx,
	}, true, nil
}

// planForfeit returns the stored forfeit transaction for a forfeited VTXO.
func (p *CheckpointPlanner) planForfeit(ctx context.Context,
	outpoint wire.OutPoint) (*ResponsePlan, error) {

	if p.ForfeitLookup == nil {
		return nil, fmt.Errorf("forfeit lookup is nil")
	}

	plan, err := p.ForfeitLookup.PlanForfeit(ctx, outpoint)
	if err != nil {
		return nil, fmt.Errorf("plan forfeit response: %w", err)
	}
	if plan == nil || plan.ResponseTx == nil {
		return nil, fmt.Errorf("forfeited vtxo %s has no forfeit tx",
			outpoint)
	}

	err = validateForfeitPlan(outpoint, plan.ResponseTx)
	if err != nil {
		return nil, err
	}

	return plan, nil
}

// validateCheckpointOutput binds tx output 0 to the persisted tap tree data.
func (p *CheckpointPlanner) validateCheckpointOutput(ctx context.Context,
	input wire.OutPoint, checkpointTx *wire.MsgTx) error {

	info, found, err := p.CheckpointSweepStore.
		LoadCheckpointSweepInfoByInput(
			ctx, input,
		)
	if err != nil {
		return fmt.Errorf("load checkpoint sweep info: %w", err)
	}
	if !found {
		return fmt.Errorf("checkpoint sweep info missing for %s", input)
	}
	if info.CheckpointTx == nil {
		return fmt.Errorf("checkpoint sweep info missing tx")
	}
	if info.CheckpointTx.TxHash() != checkpointTx.TxHash() {
		return fmt.Errorf("checkpoint sweep info txid %s, want %s",
			info.CheckpointTx.TxHash(), checkpointTx.TxHash())
	}
	if info.CheckpointOutputIndex != 0 {
		return fmt.Errorf("checkpoint sweep info output index "+
			"%d, want 0", info.CheckpointOutputIndex)
	}
	if !txOutEqual(info.CheckpointOutput, checkpointTx.TxOut[0]) {
		return fmt.Errorf("checkpoint sweep info output mismatch")
	}

	spendInfo, err := checkpointTimeoutSpendInfo(
		info, p.CheckpointPolicy,
	)
	if err != nil {
		return fmt.Errorf("checkpoint timeout spend info: %w", err)
	}

	err = (&arkscript.SpendPath{
		SpendInfo: spendInfo,
	}).VerifyBindsToPkScript(checkpointTx.TxOut[0].PkScript)
	if err != nil {
		return fmt.Errorf("checkpoint output tap tree binding: %w", err)
	}

	return nil
}

// validateForfeitPlan enforces the canonical forfeit shape needed by the
// fraud responder before handing the tx to txconfirm. The bind is run
// pre-broadcast so a tampered or malformed persisted forfeit tx (DB
// corruption, partial write, encoder bug) fails fast at the responder
// instead of confirming a malformed tx whose penalty output is
// unsweepable.
func validateForfeitPlan(input wire.OutPoint, forfeitTx *wire.MsgTx) error {
	switch {
	case forfeitTx == nil:
		return fmt.Errorf("forfeit tx is nil")

	case forfeitTx.Version != int32(arktx.TxVersion):
		return fmt.Errorf("forfeit tx version %d, want %d",
			forfeitTx.Version, arktx.TxVersion)

	case len(forfeitTx.TxIn) != 2:
		return fmt.Errorf("forfeit tx has %d inputs, want 2",
			len(forfeitTx.TxIn))

	case forfeitTx.TxIn[0].PreviousOutPoint != input:
		return fmt.Errorf("forfeit input 0 spends %s, want %s",
			forfeitTx.TxIn[0].PreviousOutPoint, input)

	case len(forfeitTx.TxOut) != 2:
		return fmt.Errorf("forfeit tx has %d outputs, want 2",
			len(forfeitTx.TxOut))

	case forfeitTx.TxOut[0] == nil:
		return fmt.Errorf("forfeit penalty output is nil")

	case forfeitTx.TxOut[0].Value <= 0:
		return fmt.Errorf("forfeit penalty value %d not positive",
			forfeitTx.TxOut[0].Value)

	case len(forfeitTx.TxOut[0].PkScript) == 0:
		return fmt.Errorf("forfeit penalty pkScript is empty")

	case !arktx.IsAnchorOutput(forfeitTx.TxOut[1]):
		return fmt.Errorf("forfeit output 1 is not anchor")
	}

	return nil
}

// validateCheckpointPlan enforces the OOR checkpoint shape for Step 1.
func validateCheckpointPlan(input wire.OutPoint,
	checkpointTx *wire.MsgTx) error {

	switch {
	case checkpointTx == nil:
		return fmt.Errorf("checkpoint tx is nil")

	case len(checkpointTx.TxIn) == 0:
		return fmt.Errorf("checkpoint tx has no inputs")

	case checkpointTx.TxIn[0].PreviousOutPoint != input:
		return fmt.Errorf("checkpoint input 0 spends %s, want %s",
			checkpointTx.TxIn[0].PreviousOutPoint, input)

	case len(checkpointTx.TxOut) == 0:
		return fmt.Errorf("checkpoint tx has no outputs")

	case checkpointTx.TxOut[0] == nil:
		return fmt.Errorf("checkpoint output 0 is nil")

	case arktx.IsAnchorOutput(checkpointTx.TxOut[0]):
		return fmt.Errorf("checkpoint output 0 is anchor output")
	}

	return nil
}
