package chainsource

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// epochChannelSize is the buffer size for block epoch channels. This
	// allows for buffering up to 10 blocks in transit, which should cover
	// normal block arrival patterns without blocking the backend.
	epochChannelSize = 10
)

// ChainSourceActor is a stateless factory actor that provides blockchain
// interface functionality. It handles direct queries (fee estimation, best
// height, mempool testing, transaction broadcasting) and spawns dedicated
// sub-actors for event monitoring (confirmations, spends, blocks).
//
// Each monitoring request spawns a new dedicated actor with a unique service
// key, enabling deterministic cancellation and eliminating shared state.
type ChainSourceActor struct {
	// backend is the blockchain backend used for all chain operations.
	backend ChainBackend

	// system is the actor system for spawning sub-actors.
	system *actor.ActorSystem
}

// NewChainSourceActor creates a new ChainSourceActor instance with the given
// backend and actor system.
func NewChainSourceActor(backend ChainBackend,
	system *actor.ActorSystem) *ChainSourceActor {

	return &ChainSourceActor{
		backend: backend,
		system:  system,
	}
}

// Receive processes incoming messages for the ChainSourceActor. This handles
// direct backend queries and spawns dedicated sub-actors for event monitoring.
func (a *ChainSourceActor) Receive(actorCtx context.Context,
	msg ChainSourceMsg) fn.Result[ChainSourceResp] {

	switch m := msg.(type) {
	case *FeeEstimateRequest:
		return a.handleFeeEstimate(actorCtx, m)

	case *BestHeightRequest:
		return a.handleBestHeight(actorCtx, m)

	case *TestMempoolAcceptRequest:
		return a.handleTestMempoolAccept(actorCtx, m)

	case *BroadcastTxRequest:
		return a.handleBroadcastTx(actorCtx, m)

	case *RegisterConfRequest:
		return a.handleRegisterConf(actorCtx, m)

	case *RegisterSpendRequest:
		return a.handleRegisterSpend(actorCtx, m)

	case *SubscribeBlocksRequest:
		return a.handleSubscribeBlocks(actorCtx, m)

	case *UnregisterConfRequest:
		return a.handleUnregisterConf(actorCtx, m)

	case *UnregisterSpendRequest:
		return a.handleUnregisterSpend(actorCtx, m)

	case *UnsubscribeBlocksRequest:
		return a.handleUnsubscribeBlocks(actorCtx, m)

	default:
		return fn.Err[ChainSourceResp](fmt.Errorf(
			"unknown message type: %T", msg))
	}
}

// handleFeeEstimate processes a fee estimation request by querying the backend
// for the current fee rate for the target confirmation count.
func (a *ChainSourceActor) handleFeeEstimate(ctx context.Context,
	req *FeeEstimateRequest) fn.Result[ChainSourceResp] {

	feeRate, err := a.backend.EstimateFee(ctx, req.TargetConf)
	if err != nil {
		return fn.Err[ChainSourceResp](fmt.Errorf("failed to "+
			"estimate fee: %w", err))
	}

	return fn.Ok[ChainSourceResp](&FeeEstimateResponse{
		SatPerVByte: feeRate,
	})
}

// handleBestHeight processes a best height request by querying the backend for
// the current best block height and hash.
func (a *ChainSourceActor) handleBestHeight(ctx context.Context,
	req *BestHeightRequest) fn.Result[ChainSourceResp] {

	height, hash, err := a.backend.BestBlock(ctx)
	if err != nil {
		return fn.Err[ChainSourceResp](fmt.Errorf("failed to get "+
			"best height: %w", err))
	}

	return fn.Ok[ChainSourceResp](&BestHeightResponse{
		Height: height,
		Hash:   hash,
	})
}

// handleTestMempoolAccept processes a mempool acceptance test request by
// checking if the given transaction would be accepted by the mempool without
// actually broadcasting it.
func (a *ChainSourceActor) handleTestMempoolAccept(ctx context.Context,
	req *TestMempoolAcceptRequest) fn.Result[ChainSourceResp] {

	accepted, reason, err := a.backend.TestMempoolAccept(ctx, req.Tx)
	if err != nil {
		return fn.Err[ChainSourceResp](fmt.Errorf("failed to test "+
			"mempool accept: %w", err))
	}

	return fn.Ok[ChainSourceResp](&TestMempoolAcceptResponse{
		Accepted: accepted,
		Reason:   reason,
	})
}

// handleBroadcastTx processes a transaction broadcast request by submitting
// the transaction to the network via the backend.
func (a *ChainSourceActor) handleBroadcastTx(ctx context.Context,
	req *BroadcastTxRequest) fn.Result[ChainSourceResp] {

	err := a.backend.BroadcastTx(ctx, req.Tx, req.Label)
	if err != nil {
		return fn.Err[ChainSourceResp](fmt.Errorf("failed to "+
			"broadcast transaction: %w", err))
	}

	txHash := req.Tx.TxHash()
	return fn.Ok[ChainSourceResp](&BroadcastTxResponse{
		Txid: txHash,
	})
}

// handleRegisterConf spawns a dedicated ConfActor for this confirmation
// request and sends it the registration message.
func (a *ChainSourceActor) handleRegisterConf(ctx context.Context,
	req *RegisterConfRequest) fn.Result[ChainSourceResp] {

	// Generate unique key component from txid and/or pkScript.
	keyPart, err := txidOrScriptKey(req.Txid, req.PkScript)
	if err != nil {
		return fn.Err[ChainSourceResp](fmt.Errorf(
			"failed to generate service key: %w", err))
	}

	// We'll then use that "key part"  to generate a unique actor ID and
	// also service key.
	actorID := fmt.Sprintf(
		"conf.%s.%s.%d", req.CallerID, keyPart, req.TargetConfs,
	)
	serviceKey := confActorServiceKey(
		req.CallerID, keyPart, req.TargetConfs,
	)

	confActor := NewConfActor(a.backend, context.Background())
	actorRef := serviceKey.Spawn(a.system, actorID, confActor)

	// We block on this as we want to know if the subscription could be
	// created or not, so we can notify the caller.
	return convertSubActorResult(
		actorRef.Ask(ctx, req).Await(ctx), "register confirmation",
	)
}

// handleRegisterSpend spawns a dedicated SpendActor for this spend request
// and sends it the registration message.
func (a *ChainSourceActor) handleRegisterSpend(ctx context.Context,
	req *RegisterSpendRequest) fn.Result[ChainSourceResp] {

	// Generate unique key component from outpoint and/or pkScript.
	keyPart, err := outpointOrScriptKey(req.Outpoint, req.PkScript)
	if err != nil {
		return fn.Err[ChainSourceResp](fmt.Errorf(
			"failed to generate service key: %w", err))
	}

	// Generate unique actor ID and service key from caller ID + request
	// params.
	actorID := fmt.Sprintf("spend.%s.%s", req.CallerID, keyPart)
	serviceKey := spendActorServiceKey(
		req.CallerID, keyPart,
	)

	spendActor := NewSpendActor(a.backend, context.Background())
	actorRef := serviceKey.Spawn(a.system, actorID, spendActor)

	// We block on this as we want to know if the subscription could be
	// created or not, so we can notify the caller.
	return convertSubActorResult(
		actorRef.Ask(ctx, req).Await(ctx), "register spend",
	)
}

// handleSubscribeBlocks spawns a dedicated BlockEpochActor for this block
// subscription and sends it the subscription message.
func (a *ChainSourceActor) handleSubscribeBlocks(ctx context.Context,
	req *SubscribeBlocksRequest) fn.Result[ChainSourceResp] {

	actorID := fmt.Sprintf("epoch.%s", req.CallerID)
	serviceKey := epochActorServiceKey(req.CallerID)

	epochActor := NewBlockEpochActor(a.backend, context.Background())
	actorRef := serviceKey.Spawn(a.system, actorID, epochActor)

	return convertSubActorResult(
		actorRef.Ask(ctx, req).Await(ctx), "subscribe blocks",
	)
}

// handleUnregisterConf cancels a confirmation subscription by finding and
// stopping the dedicated actor.
func (a *ChainSourceActor) handleUnregisterConf(ctx context.Context,
	req *UnregisterConfRequest) fn.Result[ChainSourceResp] {

	// To unregister a confirmation, we reconstruct the service key that
	// created the actor in the first place, so we can find and stop it.
	keyPart, err := txidOrScriptKey(req.Txid, req.PkScript)
	if err != nil {
		return fn.Err[ChainSourceResp](fmt.Errorf(
			"failed to generate service key: %w", err))
	}

	serviceKey := confActorServiceKey(
		req.CallerID, keyPart, req.TargetConfs,
	)

	// Unregister the actor using the service key.
	unregisterByServiceKey(a, serviceKey)

	return fn.Ok[ChainSourceResp](&UnregisterConfResponse{})
}

// handleUnregisterSpend cancels a spend subscription by finding and stopping
// the dedicated actor.
func (a *ChainSourceActor) handleUnregisterSpend(ctx context.Context,
	req *UnregisterSpendRequest) fn.Result[ChainSourceResp] {

	// To unregister a confirmation, we reconstruct the service key that
	// created the actor in the first place, so we can find and stop it.
	keyPart, err := outpointOrScriptKey(req.Outpoint, req.PkScript)
	if err != nil {
		return fn.Err[ChainSourceResp](fmt.Errorf(
			"failed to generate service key: %w", err))
	}

	serviceKey := spendActorServiceKey(
		req.CallerID, keyPart,
	)

	// Unregister the actor using the service key.
	unregisterByServiceKey(a, serviceKey)

	return fn.Ok[ChainSourceResp](&UnregisterSpendResponse{})
}

// handleUnsubscribeBlocks cancels a block subscription by finding and stopping
// the dedicated actor.
func (a *ChainSourceActor) handleUnsubscribeBlocks(ctx context.Context,
	req *UnsubscribeBlocksRequest) fn.Result[ChainSourceResp] {

	// Reconstruct the service key.
	serviceKey := epochActorServiceKey(req.CallerID)

	// Unregister the actor using the service key.
	unregisterByServiceKey(a, serviceKey)

	return fn.Ok[ChainSourceResp](&UnsubscribeBlocksResponse{})
}

// unregisterByServiceKey is a generic helper that finds and unregisters all
// actors registered with the given service key. This eliminates code
// duplication across the three unregister handler methods.
func unregisterByServiceKey[Req, Resp actor.Message](
	a *ChainSourceActor, serviceKey actor.ServiceKey[Req, Resp]) {

	refs := actor.FindInReceptionist(a.system.Receptionist(), serviceKey)
	for _, ref := range refs {
		serviceKey.Unregister(a.system, ref)
	}
}

// convertSubActorResult converts a sub-actor result to a ChainSourceResp.
func convertSubActorResult[T actor.Message](result fn.Result[T],
	op string) fn.Result[ChainSourceResp] {

	if result.IsErr() {
		return fn.Err[ChainSourceResp](fmt.Errorf("%s: %w",
			op, result.Err()))
	}

	resp, err := result.Unpack()
	if err != nil {
		return fn.Err[ChainSourceResp](fmt.Errorf("%s: %w",
			op, err))
	}

	chainResp, ok := any(resp).(ChainSourceResp)
	if !ok {
		return fn.Err[ChainSourceResp](fmt.Errorf(
			"%s: unexpected response type %T", op, resp))
	}

	return fn.Ok(chainResp)
}

// txidOrScriptKey generates a string key component from txid and/or pkScript.
// This is used to construct unique service keys for confirmation actors.
// Both parameters can be specified for more precise matching.
func txidOrScriptKey(txid *chainhash.Hash, pkScript []byte) (string, error) {
	if txid != nil && len(pkScript) > 0 {
		return fmt.Sprintf("%s+script:%x", txid.String(), pkScript), nil
	}
	if txid != nil {
		return txid.String(), nil
	}
	if len(pkScript) > 0 {
		return fmt.Sprintf("script:%x", pkScript), nil
	}

	return "", fmt.Errorf("both txid and pkScript are nil/empty")
}

// outpointOrScriptKey generates a string key component from outpoint and/or
// pkScript. This is used to construct unique service keys for spend actors.
// Both parameters can be specified for more precise matching.
func outpointOrScriptKey(outpoint *wire.OutPoint, pkScript []byte) (string, error) {
	if outpoint != nil && len(pkScript) > 0 {
		return fmt.Sprintf("%s:%d+script:%x",
			outpoint.Hash.String(), outpoint.Index, pkScript), nil
	}
	if outpoint != nil {
		return fmt.Sprintf(
			"%s:%d", outpoint.Hash.String(), outpoint.Index,
		), nil
	}
	if len(pkScript) > 0 {
		return fmt.Sprintf("script:%x", pkScript), nil
	}

	return "", fmt.Errorf("both outpoint and pkScript are nil/empty")
}

// confActorServiceKey constructs a unique service key for a ConfActor based on
// the caller ID and key part. The service key format is:
// "conf.<callerID>.<keyPart>.<targetConfs>"
func confActorServiceKey(callerID string, keyPart string,
	targetConfs uint32) actor.ServiceKey[ConfMsg, ConfResp] {

	keyStr := fmt.Sprintf(
		"conf.%s.%s.%d",
		callerID, keyPart, targetConfs,
	)

	return actor.NewServiceKey[ConfMsg, ConfResp](keyStr)
}

// spendActorServiceKey constructs a unique service key for a SpendActor based
// on the caller ID and key part. The service key format is:
// "spend.<callerID>.<keyPart>"
func spendActorServiceKey(callerID string,
	keyPart string) actor.ServiceKey[SpendMsg, SpendResp] {

	keyStr := fmt.Sprintf("spend.%s.%s", callerID, keyPart)

	return actor.NewServiceKey[SpendMsg, SpendResp](keyStr)
}

// epochActorServiceKey constructs a unique service key for a BlockEpochActor
// based on the caller ID. The service key format is: "epoch.<callerID>"
func epochActorServiceKey(callerID string) actor.ServiceKey[EpochMsg,
	EpochResp] {

	keyStr := fmt.Sprintf("epoch.%s", callerID)

	return actor.NewServiceKey[EpochMsg, EpochResp](keyStr)
}

// ChainSourceKey is the service key for the main ChainSource actor.
var ChainSourceKey = actor.NewServiceKey[ChainSourceMsg, ChainSourceResp](
	"chainsource",
)
