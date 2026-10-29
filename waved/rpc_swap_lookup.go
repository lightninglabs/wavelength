package waved

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetIndexedVTXOByPkScript queries the authoritative indexer for the first
// VTXO matching the given script and status filter.
func (r *RPCServer) GetIndexedVTXOByPkScript(ctx context.Context,
	req *waverpc.GetIndexedVTXOByPkScriptRequest) (
	*waverpc.GetIndexedVTXOByPkScriptResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	return r.getIndexedVTXOByPkScript(ctx, req, 0)
}

// getIndexedVTXOByPkScript shares policy authorization, proof validation, and
// bounded confirmation lookup between indexed existence and expiry reads.
func (r *RPCServer) getIndexedVTXOByPkScript(ctx context.Context,
	req *waverpc.GetIndexedVTXOByPkScriptRequest, currentHeight int32) (
	*waverpc.GetIndexedVTXOByPkScriptResponse, error) {

	if req == nil || len(req.PkScript) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "missing pk_script",
		)
	}

	// Wallet unlock precedes mailbox and actor startup. Require their
	// published result before reading the indexer or authenticator fields;
	// the channel close also orders their initialization against this RPC.
	select {
	case <-r.server.walletServicesReady:
		if err := r.server.walletServicesReadyErr; err != nil {
			return nil, status.Errorf(codes.Unavailable, "wallet "+
				"services failed to start: %v", err)
		}

	default:
		return nil, status.Error(
			codes.Unavailable, "wallet services are starting",
		)
	}

	if r.server.indexer == nil {
		return nil, status.Error(
			codes.Internal, "indexer client not initialized",
		)
	}

	queryClient := r.server.indexer
	policyAuthorized := len(req.PolicyTemplate) != 0
	var policy *arkscript.PolicyTemplate
	var exitDelay uint32
	if policyAuthorized {
		var err error
		policy, err = arkscript.DecodePolicyTemplate(
			req.PolicyTemplate,
		)
		if err != nil || !policy.MatchesPkScript(req.PkScript) {
			return nil, status.Error(
				codes.InvalidArgument,
				"policy does not match pk_script",
			)
		}
		// Standard outputs use the persisted receive-script signer. Its
		// owner may differ from the daemon identity. Custom queries use
		// the identity participant, as in the existing policy API.
		exitDelay = indexedPolicyExitDelay(policy)
		if exitDelay == 0 {
			factory, err := r.server.indexerProofSignerFactory()
			if err != nil {
				return nil, status.Error(
					codes.Unavailable, err.Error(),
				)
			}
			key := r.server.loadClientKeyDesc()
			if key.PubKey == nil {
				return nil, status.Error(
					codes.Unavailable,
					"identity key not initialized",
				)
			}
			queryClient = queryClient.WithSigner(factory(key))
		}
	}

	var statusFilter []arkrpc.VTXOStatus
	if len(req.StatusFilter) > 0 {
		var err error
		statusFilter, err = indexedExpiryStatusFilter(req.StatusFilter)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid status filter: %v", err)
		}
	}

	resp, err := queryClient.ListVTXOsByScriptsTaproot(
		ctx,
		[]indexer.TaprootScriptScope{{
			PkScript: append([]byte(nil), req.PkScript...),
			PolicyTemplate: append(
				[]byte(nil), req.PolicyTemplate...,
			),
		}},
		nil, 1, statusFilter,
	)
	if err != nil {
		code := status.Code(err)
		if code == codes.Unknown {
			code = codes.Unavailable
		}

		return nil, status.Errorf(code, "indexer query failed: %v", err)
	}

	vtxos := vtxo.FlattenListVTXOsByScriptsResponse(resp)
	if len(vtxos) == 0 {
		return &waverpc.GetIndexedVTXOByPkScriptResponse{
			PolicyAuthorized: policyAuthorized,
		}, nil
	}
	if !bytes.Equal(vtxos[0].GetPkScript(), req.PkScript) {
		return nil, status.Error(
			codes.DataLoss,
			"indexed vtxo script does not match query",
		)
	}
	verified, err := authenticateIndexedExpiry(
		ctx, vtxos[0], r.server.expiryAuthenticator,
	)
	if err != nil {
		if errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) {
			return nil, status.FromContextError(err).Err()
		}
		code := status.Code(err)
		if code == codes.Unknown {
			code = codes.Unavailable
		}
		if errors.Is(err, vtxo.ErrInvalidBatchExpiryEvidence) {
			code = codes.DataLoss
		}

		return nil, status.Errorf(code, "authenticate indexed vtxo "+
			"expiry: %v", err)
	}
	verified.exitDelay = exitDelay

	if currentHeight == 0 {
		currentHeight, err = r.currentBlockHeight(ctx)
		if err != nil {
			currentHeight = 0
		}
	}

	vtxo, err := indexedVTXOToProto(
		vtxos[0], verified, currentHeight, r.server.vtxoExpiryConfig(),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "convert indexed "+
			"vtxo: %v", err)
	}

	return &waverpc.GetIndexedVTXOByPkScriptResponse{
		Vtxo:             vtxo,
		PolicyAuthorized: policyAuthorized,
	}, nil
}

// GetIndexedOORSessionByTxid queries the authoritative indexer for one OOR
// session using a spent script proof and deterministic session txid.
func (r *RPCServer) GetIndexedOORSessionByTxid(ctx context.Context,
	req *waverpc.GetIndexedOORSessionByTxidRequest) (
	*waverpc.GetIndexedOORSessionByTxidResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	switch {
	case req == nil:
		return nil, status.Error(
			codes.InvalidArgument, "request is required",
		)

	case len(req.PkScript) == 0:
		return nil, status.Error(
			codes.InvalidArgument, "missing pk_script",
		)

	case len(req.SessionTxid) != chainhash.HashSize:
		return nil, status.Error(
			codes.InvalidArgument, "session_txid must be 32 bytes",
		)
	}

	if r.server.indexer == nil {
		return nil, status.Error(
			codes.Internal, "indexer client not initialized",
		)
	}

	resp, err := r.server.indexer.GetOORSessionByTxidTaproot(
		ctx, req.PkScript, req.SessionTxid,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "indexer query "+
			"failed: %v", err)
	}

	return &waverpc.GetIndexedOORSessionByTxidResponse{
		ArkPsbt: append([]byte(nil), resp.GetArkPsbt()...),
		CheckpointPsbts: deepCopyByteSlices(
			resp.GetCheckpointPsbts(),
		),
	}, nil
}

// daemonStatusToIndexerStatus converts waverpc VTXO status enums to their
// arkrpc counterparts.
func daemonStatusToIndexerStatus(status waverpc.VTXOStatus) (arkrpc.VTXOStatus,
	error) {

	switch status {
	case waverpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED:
		return arkrpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED, nil

	case waverpc.VTXOStatus_VTXO_STATUS_LIVE:
		return arkrpc.VTXOStatus_VTXO_STATUS_LIVE, nil

	case waverpc.VTXOStatus_VTXO_STATUS_PENDING_ROUND:
		return arkrpc.VTXOStatus_VTXO_STATUS_UNCONFIRMED, nil

	case waverpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT:
		return arkrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT, nil

	case waverpc.VTXOStatus_VTXO_STATUS_FORFEITING:
		return arkrpc.VTXOStatus_VTXO_STATUS_FORFEITING, nil

	case waverpc.VTXOStatus_VTXO_STATUS_FORFEITED:
		return arkrpc.VTXOStatus_VTXO_STATUS_FORFEITED, nil

	case waverpc.VTXOStatus_VTXO_STATUS_SPENT:
		return arkrpc.VTXOStatus_VTXO_STATUS_SPENT, nil

	case waverpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT:
		return arkrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT, nil

	case waverpc.VTXOStatus_VTXO_STATUS_FAILED:
		return arkrpc.VTXOStatus_VTXO_STATUS_FAILED, nil

	case waverpc.VTXOStatus_VTXO_STATUS_SPENDING:
		return 0, fmt.Errorf("indexer does not expose spending status")

	default:
		return 0, fmt.Errorf("unknown VTXO status: %v", status)
	}
}

// authenticatedIndexedExpiry contains the timing facts proved by signed
// ancestry. A positive batchExpiry additionally requires local confirmation;
// zero preserves the output lookup without claiming a usable deadline.
type authenticatedIndexedExpiry struct {
	batchExpiry int32
	treeDepth   uint32
	chainDepth  uint32
	exitDelay   uint32
}

// indexedPolicyExitDelay returns the script-bound VTXO exit delay when the
// caller supplied a matching standard policy. Without one, zero means that
// indexed expiry posture cannot be classified.
func indexedPolicyExitDelay(policy *arkscript.PolicyTemplate) uint32 {
	if policy == nil {
		return 0
	}
	params, err := arkscript.DecodeStandardVTXOParams(policy)
	if err == nil {
		return params.ExitDelay
	}

	// A matching nonstandard policy still authorizes the lookup, but it
	// does not supply a standard exit delay for expiry classification.
	return 0
}

// authenticateIndexedExpiry binds the indexed output to its signed ancestry,
// then attempts a bounded local confirmation lookup. Valid targets survive
// missing confirmation with a zero deadline; malformed evidence is an error.
func authenticateIndexedExpiry(ctx context.Context, indexed *arkrpc.VTXO,
	authenticate oor.IncomingExpiryAuthenticator) (
	authenticatedIndexedExpiry, error) {

	if err := ctx.Err(); err != nil {
		return authenticatedIndexedExpiry{}, err
	}
	ancestry, err := vtxo.IndexedAncestryFromRPC(indexed)
	if err != nil {
		return authenticatedIndexedExpiry{}, fmt.Errorf("validate "+
			"indexed ancestry: %w", err)
	}
	if authenticate == nil {
		return authenticatedIndexedExpiry{}, status.Error(
			codes.Internal, "expiry authenticator not initialized",
		)
	}
	verified := authenticatedIndexedExpiry{}
	for _, fragment := range ancestry {
		verified.treeDepth = max(
			verified.treeDepth, fragment.TreeDepth,
		)
	}
	// Each OOR package contributes at most one sequential checkpoint hop.
	// Counting all packages may overstate parallel paths, which only
	// shrinks the available payment window. The reported chain depth may
	// increase this reserve but cannot decrease it.
	verified.chainDepth = max(
		indexed.GetChainDepth(),
		uint32(
			len(
				indexed.GetAncestryPackages(),
			),
		),
	)

	// An explicitly unconfirmed candidate needs no confirmation watch.
	// A zero height hint alone can also describe a confirmed legacy output,
	// so live candidates still get the bounded local lookup below.
	if indexed.GetStatus() == arkrpc.VTXOStatus_VTXO_STATUS_UNCONFIRMED {
		return verified, nil
	}
	lookupCtx, cancel := context.WithTimeout(
		ctx, indexedExpiryAuthenticationTimeout,
	)
	defer cancel()
	evidence, err := authenticate(lookupCtx, ancestry)
	if err != nil {
		if ctx.Err() != nil {
			return authenticatedIndexedExpiry{}, ctx.Err()
		}
		if errors.Is(err, vtxo.ErrInvalidBatchExpiryEvidence) {
			return authenticatedIndexedExpiry{}, err
		}

		// A valid signed target remains observable during confirmation
		// lag or a chain-backend outage. No deadline is claimed.
		return verified, nil
	}
	if evidence.BatchExpiry > 0 {
		verified.batchExpiry = evidence.BatchExpiry
	}

	return verified, nil
}

// indexedExpiryAuthenticationTimeout bounds chain work for a read. Durable
// acceptance paths retain their separate confirmation-wait budget.
const indexedExpiryAuthenticationTimeout = 2 * time.Second

// indexedVTXOToProto converts a proved indexed VTXO into the daemon's
// simplified VTXO response shape.
func indexedVTXOToProto(vtxo *arkrpc.VTXO, verified authenticatedIndexedExpiry,
	currentHeight int32, cfg *vtxo.ExpiryConfig) (*waverpc.VTXO, error) {

	if vtxo == nil {
		return nil, fmt.Errorf("vtxo must be provided")
	}

	outpoint := vtxo.GetOutpoint()
	if outpoint == nil {
		return nil, fmt.Errorf("indexer vtxo missing outpoint")
	}

	txid, err := chainhash.NewHash(outpoint.GetTxid())
	if err != nil {
		return nil, fmt.Errorf("parse outpoint txid: %w", err)
	}

	status, err := indexerStatusToDaemonStatus(vtxo.GetStatus())
	if err != nil {
		return nil, err
	}

	commitmentTxid := ""
	if len(vtxo.GetCommitmentTxid()) > 0 {
		commitmentHash, err := chainhash.NewHash(
			vtxo.GetCommitmentTxid(),
		)
		if err != nil {
			return nil, fmt.Errorf("parse commitment txid: %w", err)
		}

		commitmentTxid = commitmentHash.String()
	}

	spentByTxid := ""
	if len(vtxo.GetSpentByTxid()) > 0 {
		spentHash, err := chainhash.NewHash(vtxo.GetSpentByTxid())
		if err != nil {
			return nil, fmt.Errorf("parse spent_by_txid: %w", err)
		}

		spentByTxid = spentHash.String()
	}

	// The indexer's relative expiry describes the batch, and is not proof
	// of this output's unilateral exit delay. Only a policy template that
	// matches the output script can supply that delay for classification.
	var expiryInfo *waverpc.VTXOExpiryInfo
	if verified.exitDelay > 0 {
		expiryInfo = expiryInfoFromTiming(
			verified.batchExpiry, verified.exitDelay,
			verified.treeDepth, verified.chainDepth, currentHeight,
			cfg,
		)
	} else {
		expiryInfo = &waverpc.VTXOExpiryInfo{
			Status: waverpc.
				VTXOExpiryStatus_VTXO_EXPIRY_STATUS_UNKNOWN,
			CurrentHeight: currentHeight,
			BatchExpiry:   verified.batchExpiry,
			MaxTreeDepth:  verified.treeDepth,
			ChainDepth:    verified.chainDepth,
		}
		if currentHeight > 0 && verified.batchExpiry > 0 {
			expiryInfo.BlocksRemaining = verified.batchExpiry -
				currentHeight
		}
	}

	return &waverpc.VTXO{
		Outpoint:       fmt.Sprintf("%s:%d", txid, outpoint.GetVout()),
		AmountSat:      int64(vtxo.GetValueSat()),
		Status:         status,
		BatchExpiry:    verified.batchExpiry,
		RoundId:        vtxo.GetRoundId(),
		CreatedHeight:  vtxo.GetCreatedHeight(),
		RelativeExpiry: verified.exitDelay,
		PkScript:       fmt.Sprintf("%x", vtxo.GetPkScript()),
		CommitmentTxid: commitmentTxid,
		ChainDepth:     verified.chainDepth,
		OorFinalCheckpointPsbts: deepCopyByteSlices(
			vtxo.GetOorFinalCheckpointPsbts(),
		),
		SpentByTxid: spentByTxid,
		ExpiryInfo:  expiryInfo,
	}, nil
}

// indexerStatusToDaemonStatus converts arkrpc VTXO status enums to waverpc.
func indexerStatusToDaemonStatus(status arkrpc.VTXOStatus) (waverpc.VTXOStatus,
	error) {

	switch status {
	case arkrpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED:
		return waverpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_LIVE:
		return waverpc.VTXOStatus_VTXO_STATUS_LIVE, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_UNCONFIRMED:
		return waverpc.VTXOStatus_VTXO_STATUS_PENDING_ROUND, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT:
		return waverpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_FORFEITING:
		return waverpc.VTXOStatus_VTXO_STATUS_FORFEITING, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_FORFEITED:
		return waverpc.VTXOStatus_VTXO_STATUS_FORFEITED, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_SPENT:
		return waverpc.VTXOStatus_VTXO_STATUS_SPENT, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT:
		return waverpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_FAILED:
		return waverpc.VTXOStatus_VTXO_STATUS_FAILED, nil

	default:
		return 0, fmt.Errorf("unknown indexer VTXO status: %v", status)
	}
}

// deepCopyByteSlices clones each byte slice in src.
func deepCopyByteSlices(src [][]byte) [][]byte {
	out := make([][]byte, 0, len(src))
	for i := range src {
		out = append(out, append([]byte(nil), src[i]...))
	}

	return out
}
