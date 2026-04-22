package darepod

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/btcwbackend"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightninglabs/darepo-client/lwwallet"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/clock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RPCServer implements the daemon's gRPC DaemonService interface.
type RPCServer struct {
	daemonrpc.UnimplementedDaemonServiceServer

	server *Server

	// customInputLocksMu guards customInputLocks. Together they form
	// a lightweight in-memory mutex on custom OOR input outpoints
	// currently being processed by concurrent SendOOR calls. Without
	// this gate two concurrent callers supplying the same custom
	// input (e.g. the same vHTLC claim outpoint) could each succeed
	// through BuildCustomTransferInputs and end up double-signing
	// the same input. Standard wallet-managed VTXOs are locked via
	// the VTXO manager's reservation flow; custom inputs live
	// outside that managed set, so we dedup them here.
	customInputLocksMu sync.Mutex

	// customInputLocks is the set of custom OOR input outpoints
	// currently reserved by an in-flight SendOOR call.
	customInputLocks map[wire.OutPoint]struct{}
}

// NewRPCServer creates a new RPCServer backed by the given Server.
func NewRPCServer(server *Server) *RPCServer {
	return &RPCServer{
		server:           server,
		customInputLocks: make(map[wire.OutPoint]struct{}),
	}
}

// reserveCustomInputs atomically claims every outpoint in the supplied
// slice. If any outpoint is already reserved by another in-flight call,
// no claims are taken and an error is returned. The returned release
// function reverses the claim and is safe to call on either success or
// failure paths (e.g. via defer).
func (r *RPCServer) reserveCustomInputs(
	outpoints []wire.OutPoint) (func(), error) {

	r.customInputLocksMu.Lock()
	defer r.customInputLocksMu.Unlock()

	// First pass: check for collisions so we don't partially reserve.
	for i := range outpoints {
		if _, taken := r.customInputLocks[outpoints[i]]; taken {
			return nil, fmt.Errorf("custom input %s already "+
				"reserved by another in-flight SendOOR",
				outpoints[i])
		}
	}

	// Second pass: commit the reservations.
	for i := range outpoints {
		r.customInputLocks[outpoints[i]] = struct{}{}
	}

	release := func() {
		r.customInputLocksMu.Lock()
		defer r.customInputLocksMu.Unlock()

		for i := range outpoints {
			delete(r.customInputLocks, outpoints[i])
		}
	}

	return release, nil
}

// GetInfo returns basic information about the running daemon instance,
// including version, network, and lnd connection state.
func (r *RPCServer) GetInfo(ctx context.Context,
	_ *daemonrpc.GetInfoRequest) (*daemonrpc.GetInfoResponse, error) {

	resp := &daemonrpc.GetInfoResponse{
		Version:     build.Version(),
		Commit:      build.CommitHash,
		Network:     r.server.cfg.Network,
		WalletType:  r.server.cfg.Wallet.Type,
		WalletReady: r.server.isWalletReady(),
	}

	// Populate lnd fields if connected.
	r.server.lnd.WhenSome(
		func(lndSvc *lndclient.GrpcLndServices) {
			resp.LndIdentityPubkey =
				lndSvc.NodePubkey.String()
			resp.LndAlias = lndSvc.NodeAlias

			// Fetch the current best block height from
			// the chain backend via lnd's ChainKit
			// interface.
			_, height, err :=
				lndSvc.ChainKit.GetBestBlock(ctx)
			if err != nil {
				r.server.log.WarnS(ctx,
					"Unable to fetch block height",
					err)
			} else {
				resp.BlockHeight = uint32(height)
			}
		},
	)

	// IdentityPubkey is the daemon identity key derived from
	// the active wallet backend. For lnd-backed daemons this
	// intentionally differs from the
	// public node key above and must stay aligned with the indexer proof
	// signer used for script-scoped VTXO queries.
	identityPubkey, err := r.deriveIdentityPubkey(ctx)
	if err != nil {
		r.server.log.WarnS(ctx,
			"Unable to derive daemon identity key", err)
	} else {
		resp.IdentityPubkey = identityPubkey
	}

	// Populate lwwallet fields if the lightweight wallet is active.
	r.server.lwWallet.WhenSome(
		func(w *lwwallet.Wallet) {
			// Get block height from the chain backend.
			if r.server.chainBackend != nil {
				height, _, err :=
					r.server.chainBackend.BestBlock(
						ctx,
					)
				if err != nil {
					r.server.log.WarnS(ctx,
						"Unable to fetch block "+
							"height", err)
				} else {
					resp.BlockHeight = uint32(height)
				}
			}
		},
	)

	// Populate btcwallet fields if the neutrino-backed wallet is
	// active.
	r.server.btcwWallet.WhenSome(
		func(w *btcwbackend.Wallet) {
			// Get block height from the chain backend.
			if r.server.chainBackend != nil {
				height, _, err :=
					r.server.chainBackend.BestBlock(
						ctx,
					)
				if err != nil {
					r.server.log.WarnS(ctx,
						"Unable to fetch block "+
							"height", err)
				} else {
					resp.BlockHeight = uint32(height)
				}
			}
		},
	)

	// TODO(roasbeef): populate server connection status from runtime.

	return resp, nil
}

// requireWalletReady returns a gRPC error if the wallet is not yet
// ready. Callers use this to gate RPCs that need wallet access.
func (r *RPCServer) requireWalletReady() error {
	if !r.server.isWalletReady() {
		return status.Errorf(codes.FailedPrecondition,
			"wallet is not ready (create or unlock first)")
	}

	return nil
}

// GetBalance returns the current balance of the wallet, broken down
// by boarding (on-chain) and VTXO (off-chain) balances.
func (r *RPCServer) GetBalance(ctx context.Context,
	_ *daemonrpc.GetBalanceRequest) (
	*daemonrpc.GetBalanceResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	resp := &daemonrpc.GetBalanceResponse{}

	// Fetch boarding balance from the wallet actor via Ask.
	if r.server.walletRef.IsSome() {
		wRef := r.server.walletRef.UnsafeFromSome()

		balReq := &wallet.GetBoardingBalanceRequest{}
		future := wRef.Ask(ctx, balReq)
		result := future.Await(ctx)

		balResp, err := result.Unpack()
		if err != nil {
			r.server.log.WarnS(ctx,
				"Unable to fetch boarding balance",
				err)
		} else {
			br, ok := balResp.(*wallet.GetBoardingBalanceResponse)
			if ok {
				resp.BoardingConfirmedSat = int64(
					br.TotalBalance,
				)
			}
		}
	}

	// Fetch VTXO balance by summing all live VTXOs using the
	// package-level SumBalance helper.
	if r.server.vtxoStore != nil {
		liveVTXOs, err := r.server.vtxoStore.ListLiveVTXOs(
			ctx,
		)
		if err != nil {
			r.server.log.WarnS(ctx,
				"Unable to fetch VTXO balance", err)
		} else {
			resp.VtxoBalanceSat = int64(
				vtxo.SumBalance(liveVTXOs),
			)
		}
	}

	resp.TotalConfirmedSat = resp.BoardingConfirmedSat +
		resp.VtxoBalanceSat

	return resp, nil
}

// ListVTXOs returns the set of VTXOs known to the wallet, optionally
// filtered by status and minimum amount.
func (r *RPCServer) ListVTXOs(ctx context.Context,
	req *daemonrpc.ListVTXOsRequest) (
	*daemonrpc.ListVTXOsResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if r.server.vtxoStore == nil {
		return nil, status.Errorf(codes.Internal,
			"vtxo store not initialized")
	}

	// Fetch VTXOs from the store. When a specific status filter
	// is provided, query the DB directly for that status so
	// terminal states (spent, forfeited) are reachable. When
	// unspecified, return all non-terminal (live) VTXOs.
	var (
		dbVTXOs []*vtxo.Descriptor
		err     error
	)

	if req.StatusFilter !=
		daemonrpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED {

		domainStatus, sErr := protoStatusToDomain(
			req.StatusFilter,
		)
		if sErr != nil {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"invalid status filter: %v", sErr,
			)
		}

		dbVTXOs, err = r.server.vtxoStore.ListVTXOsByStatus(
			ctx, domainStatus,
		)
	} else {
		dbVTXOs, err = r.server.vtxoStore.ListLiveVTXOs(ctx)
	}

	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to list VTXOs: %v", err)
	}

	// Apply any remaining in-memory filters (min amount) via
	// the package-level FilterDescriptors function so this
	// logic is reusable by a future SDK.
	filterOpts := vtxo.FilterOptions{
		MinAmount: btcutil.Amount(req.MinAmountSat),
	}

	filtered := vtxo.FilterDescriptors(dbVTXOs, filterOpts)

	var packageStore *db.OORArtifactPersistenceStore
	for i := range filtered {
		if filtered[i].Status == vtxo.VTXOStatusSpent ||
			filtered[i].ChainDepth > 0 {

			packageStore = r.newLocalOORArtifactStore()

			break
		}
	}

	// Convert to proto.
	protoVTXOs := make(
		[]*daemonrpc.VTXO, 0, len(filtered),
	)
	for _, v := range filtered {
		protoVTXO := descriptorToProto(v)
		if packageStore != nil &&
			(v.Status == vtxo.VTXOStatusSpent ||
				v.ChainDepth > 0) {

			err := populatePackageCheckpointPSBTs(
				ctx, packageStore, v, protoVTXO,
			)
			if err != nil {
				return nil, status.Errorf(codes.Internal,
					"populate package checkpoint psbts: %v",
					err)
			}
		}

		protoVTXOs = append(protoVTXOs, protoVTXO)
	}

	return &daemonrpc.ListVTXOsResponse{
		Vtxos: protoVTXOs,
	}, nil
}

// protoStatusToDomain converts a proto VTXOStatus enum to the domain
// vtxo.VTXOStatus type for use with vtxo.FilterDescriptors. An error
// is returned for unknown status values to surface proto/domain drift
// early rather than silently defaulting.
func protoStatusToDomain(
	s daemonrpc.VTXOStatus) (vtxo.VTXOStatus, error) {

	switch s {
	case daemonrpc.VTXOStatus_VTXO_STATUS_LIVE:
		return vtxo.VTXOStatusLive, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT:
		return vtxo.VTXOStatusPendingForfeit, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITING:
		return vtxo.VTXOStatusForfeiting, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED:
		return vtxo.VTXOStatusForfeited, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_SPENT:
		return vtxo.VTXOStatusSpent, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT:
		return vtxo.VTXOStatusUnilateralExit, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_FAILED:
		return vtxo.VTXOStatusFailed, nil

	default:
		return 0, fmt.Errorf("unknown VTXO status: %v", s)
	}
}

// vtxoStatusToProto converts a domain VTXOStatus to the proto enum.
func vtxoStatusToProto(s vtxo.VTXOStatus) daemonrpc.VTXOStatus {
	switch s {
	case vtxo.VTXOStatusLive:
		return daemonrpc.VTXOStatus_VTXO_STATUS_LIVE

	case vtxo.VTXOStatusPendingForfeit:
		return daemonrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT

	case vtxo.VTXOStatusForfeiting:
		return daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITING

	case vtxo.VTXOStatusForfeited:
		return daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED

	case vtxo.VTXOStatusSpent:
		return daemonrpc.VTXOStatus_VTXO_STATUS_SPENT

	case vtxo.VTXOStatusUnilateralExit:
		return daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT

	case vtxo.VTXOStatusFailed:
		return daemonrpc.VTXOStatus_VTXO_STATUS_FAILED

	case vtxo.VTXOStatusSpending:
		return daemonrpc.VTXOStatus_VTXO_STATUS_SPENDING

	default:
		return daemonrpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED
	}
}

// descriptorToProto converts a vtxo.Descriptor to the proto VTXO message.
func descriptorToProto(v *vtxo.Descriptor) *daemonrpc.VTXO {
	return &daemonrpc.VTXO{
		Outpoint: fmt.Sprintf(
			"%s:%d", v.Outpoint.Hash, v.Outpoint.Index,
		),
		AmountSat:      int64(v.Amount),
		Status:         vtxoStatusToProto(v.Status),
		BatchExpiry:    v.BatchExpiry,
		RoundId:        v.RoundID,
		CreatedHeight:  v.CreatedHeight,
		RelativeExpiry: v.RelativeExpiry,
		PkScript:       hex.EncodeToString(v.PkScript),
		CommitmentTxid: v.CommitmentTxID.String(),
		ChainDepth:     uint32(v.ChainDepth),
	}
}

// newLocalOORArtifactStore returns the local artifact store used to resolve
// persisted OOR package checkpoints for spent VTXOs.
func (r *RPCServer) newLocalOORArtifactStore() *db.OORArtifactPersistenceStore {
	if r.server.db == nil {
		return nil
	}

	dbStore := db.NewStore(
		r.server.db.DB, r.server.db.Queries,
		r.server.db.Backend(), r.server.log,
	)

	return dbStore.NewOORArtifactStore(clock.NewDefaultClock())
}

// populatePackageCheckpointPSBTs attaches locally persisted finalized
// checkpoint PSBTs to one VTXO response when an OOR package is available for
// its outpoint.
func populatePackageCheckpointPSBTs(ctx context.Context,
	store *db.OORArtifactPersistenceStore, desc *vtxo.Descriptor,
	protoVTXO *daemonrpc.VTXO) error {

	if store == nil || desc == nil || protoVTXO == nil {
		return nil
	}

	bundle, err := store.GetPackageForOutpoint(ctx, desc.Outpoint)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil

	case err != nil:
		return err
	}

	checkpoints := make(
		[][]byte, 0, len(bundle.FinalCheckpointPSBTs),
	)
	for i := range bundle.FinalCheckpointPSBTs {
		raw, err := psbtutil.Serialize(
			bundle.FinalCheckpointPSBTs[i],
		)
		if err != nil {
			return fmt.Errorf(
				"serialize checkpoint %d: %w", i, err,
			)
		}

		checkpoints = append(checkpoints, raw)
	}

	protoVTXO.OorFinalCheckpointPsbts = checkpoints

	return nil
}

// NewAddress generates a new boarding address that can receive
// on-chain funds for use in the Ark protocol.
func (r *RPCServer) NewAddress(ctx context.Context,
	_ *daemonrpc.NewAddressRequest) (
	*daemonrpc.NewAddressResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Internal,
			"wallet actor not initialized")
	}

	wRef := r.server.walletRef.UnsafeFromSome()

	// Fetch operator terms to get the operator key and exit delay
	// needed for the boarding address tapscript.
	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to fetch operator terms: %v", err)
	}

	addrReq := &wallet.CreateBoardingAddressRequest{
		OperatorKey: terms.PubKey,
		ExitDelay:   terms.BoardingExitDelay,
	}
	future := wRef.Ask(ctx, addrReq)
	result := future.Await(ctx)

	addrResp, err := result.Unpack()
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to create boarding address: %v", err)
	}

	resp, ok := addrResp.(*wallet.CreateBoardingAddressResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal,
			"unexpected response type: %T", addrResp)
	}

	return &daemonrpc.NewAddressResponse{
		Address: resp.Address.String(),
	}, nil
}

// RefreshVTXOs queues one or more VTXOs for refresh in the next round.
// This extends their expiry without changing ownership. If the all flag
// is set, every live VTXO is queued for refresh.
func (r *RPCServer) RefreshVTXOs(ctx context.Context,
	req *daemonrpc.RefreshVTXOsRequest) (
	*daemonrpc.RefreshVTXOsResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Internal,
			"wallet actor not initialized")
	}

	wRef := r.server.walletRef.UnsafeFromSome()

	// Build the list of target outpoints. If the "all" selection
	// is set, leave the slice empty so the wallet actor refreshes
	// everything approaching expiry.
	var (
		targets    []wire.OutPoint
		refreshAll bool
	)

	switch sel := req.Selection.(type) {
	case *daemonrpc.RefreshVTXOsRequest_All:
		refreshAll = sel.All

	case *daemonrpc.RefreshVTXOsRequest_Outpoints:
		if sel.Outpoints == nil ||
			len(sel.Outpoints.Outpoints) == 0 {

			return nil, status.Errorf(
				codes.InvalidArgument,
				"outpoints list is empty")
		}

		for _, opStr := range sel.Outpoints.Outpoints {
			op, err := parseOutpointString(opStr)
			if err != nil {
				return nil, status.Errorf(
					codes.InvalidArgument,
					"invalid outpoint %q: %v",
					opStr, err)
			}

			targets = append(targets, op)
		}

	default:
		return nil, status.Errorf(
			codes.InvalidArgument,
			"selection is required (outpoints or all)")
	}

	// For dry_run, validate inputs and return a preview without
	// actually queuing anything.
	if req.DryRun {
		outpointStrs := make([]string, 0, len(targets))
		for _, op := range targets {
			outpointStrs = append(outpointStrs,
				fmt.Sprintf("%s:%d",
					op.Hash, op.Index))
		}

		return &daemonrpc.RefreshVTXOsResponse{
			QueuedOutpoints: outpointStrs,
			Status:          "preview",
		}, nil
	}

	// Send the refresh request to the wallet actor and await its
	// response.
	refreshReq := &wallet.RefreshVTXOsRequest{
		TargetOutpoints: targets,
		ForceRefresh:    true,
	}
	future := wRef.Ask(ctx, refreshReq)
	result := future.Await(ctx)

	refreshResp, err := result.Unpack()
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"refresh request failed: %v", err)
	}

	resp, ok := refreshResp.(*wallet.RefreshVTXOsResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal,
			"unexpected response type: %T", refreshResp)
	}

	// Log any per-outpoint errors but don't fail the overall
	// request.
	for op, opErr := range resp.Errors {
		r.server.log.WarnS(ctx, "VTXO refresh error", opErr,
			"outpoint", op.String())
	}

	// Build the list of outpoints that were successfully queued.
	queued := make([]string, 0, resp.RefreshingCount)
	if refreshAll && r.server.vtxoStore != nil {
		// When refreshing all, list the live VTXOs to
		// report which ones were actually queued.
		liveVTXOs, err := r.server.vtxoStore.ListLiveVTXOs(
			ctx,
		)
		if err == nil {
			for _, v := range liveVTXOs {
				_, hasErr := resp.Errors[v.Outpoint]
				if !hasErr {
					queued = append(queued,
						fmt.Sprintf("%s:%d",
							v.Outpoint.Hash,
							v.Outpoint.Index))
				}
			}
		}
	} else {
		for _, op := range targets {
			if _, hasErr := resp.Errors[op]; !hasErr {
				queued = append(queued,
					fmt.Sprintf("%s:%d",
						op.Hash, op.Index))
			}
		}
	}

	r.server.log.InfoS(ctx, "VTXOs queued for refresh",
		slog.Int("queued_count", len(queued)),
		slog.Int("error_count", len(resp.Errors)))

	return &daemonrpc.RefreshVTXOsResponse{
		QueuedOutpoints: queued,
		Status:          "queued",
	}, nil
}

// Board triggers the client to join the next round with any confirmed
// boarding UTXOs. The RPC delegates the full flow to the wallet actor:
// balance check, VTXO amount computation, and round registration. It
// returns immediately after the wallet accepts the request; use
// ListRounds/WatchRounds to observe round progress.
func (r *RPCServer) Board(ctx context.Context,
	_ *daemonrpc.BoardRequest) (
	*daemonrpc.BoardResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	// Fetch operator terms so the wallet can compute the VTXO
	// output amount after deducting fees.
	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"failed to fetch operator terms: %v", err)
	}

	// Delegate to the wallet actor which handles balance
	// checking, VTXO amount computation, and forwarding the
	// TriggerBoardMsg to the round actor.
	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Internal,
			"wallet actor not initialized")
	}
	wRef := r.server.walletRef.UnsafeFromSome()

	// Quote the dynamic operator fee for the current confirmed
	// boarding balance. The server's validateOperatorFee
	// validates the submitted implicit fee against the schedule-
	// derived TotalFeeSat; without this quote the client would
	// keep paying the legacy flat terms.MinOperatorFee and
	// either silently overpay (when the new schedule is
	// cheaper) or be rejected with ErrOperatorFeeTooLow (when
	// the schedule is more expensive than the legacy flat
	// value).
	//
	// The boarding balance is fetched via the wallet's
	// GetBoardingBalance Ask so the quote is anchored to the
	// exact value the wallet is about to consume. A small TOCTOU
	// window remains between this call and the wallet's own
	// FetchBoardingIntentsByStatus call below, but a new
	// boarding arrival between the two events would only
	// produce an under-quote, which the server still accepts
	// (it only rejects under-quotes vs. the new, larger, amount
	// when that amount breaks the MinViableVTXOPct policy).
	balanceFuture := wRef.Ask(ctx, &wallet.GetBoardingBalanceRequest{})
	balanceResult := balanceFuture.Await(ctx)
	balanceResp, err := balanceResult.Unpack()
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"peek boarding balance: %v", err)
	}
	balance, ok := balanceResp.(*wallet.GetBoardingBalanceResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal,
			"unexpected boarding balance response type: %T",
			balanceResp)
	}

	// Default to the legacy flat fee. We overwrite it with the
	// dynamic quote below when the balance is positive and the
	// operator is reachable; if the quote path fails we log and
	// fall back rather than refusing to board.
	//
	// We pay max(quoted, terms.MinOperatorFee). The legacy flat
	// fee is the operator's stated lower bound and the round
	// FSM's pre-flight check at round/transitions.go enforces it,
	// so paying anything strictly less than terms.MinOperatorFee
	// would have the client reject its own submission before the
	// server saw it. When the dynamic quote is the larger of the
	// two we pay it exactly so the ledger records the true fee.
	operatorFee := terms.MinOperatorFee
	if balance.TotalBalance > 0 {
		quoted, qErr := r.server.quoteOperatorFee(
			ctx, int64(balance.TotalBalance),
			true /* isBoarding */, 0,
		)
		switch {
		case qErr != nil:
			r.server.log.WarnS(ctx,
				"EstimateFee unavailable; falling back "+
					"to legacy flat MinOperatorFee",
				qErr)

		case quoted > terms.MinOperatorFee:
			operatorFee = quoted
		}
	}

	boardReq := &wallet.BoardRequest{
		MinOperatorFee: operatorFee,
		DustLimit:      terms.DustLimit,
	}

	future := wRef.Ask(ctx, boardReq)
	result := future.Await(ctx)

	resp, err := result.Unpack()
	if err != nil {
		// Expected conditions map to a success response with
		// a descriptive status rather than a gRPC error.
		errStr := err.Error()
		switch {
		case strings.Contains(errStr, "no confirmed boarding"),
			strings.Contains(errStr, "no inputs to register"):

			r.server.log.InfoS(ctx,
				"Board skipped: no boarding UTXOs")

			return &daemonrpc.BoardResponse{
				Status: "no_boarding_utxos",
			}, nil

		case strings.Contains(errStr, "too small after"):
			return nil, status.Errorf(
				codes.FailedPrecondition,
				"boarding balance too small: %v", err,
			)
		}

		return nil, status.Errorf(codes.Internal,
			"board failed: %v", err)
	}

	boardResp, ok := resp.(*wallet.BoardResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal,
			"unexpected board response type: %T", resp)
	}

	r.server.log.InfoS(ctx, "Board request accepted",
		btclog.Fmt("boarding_balance", "%v",
			boardResp.BoardingBalance),
		btclog.Fmt("vtxo_amount", "%v",
			boardResp.VTXOAmount))

	return &daemonrpc.BoardResponse{
		Status: "registered",
	}, nil
}

// SendVTXO initiates an in-round directed transfer by forfeiting
// existing VTXOs and creating new recipient VTXOs in the same round.
// Coin selection, reservation, and round registration are handled
// atomically by the wallet actor.
func (r *RPCServer) SendVTXO(ctx context.Context,
	req *daemonrpc.SendVTXORequest) (
	*daemonrpc.SendVTXOResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	// TODO(#241): Tune this cap based on round tree constraints
	// and consider making it configurable.
	const maxRecipients = 256

	if len(req.Recipients) == 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"at least one recipient is required")
	}

	if len(req.Recipients) > maxRecipients {
		return nil, status.Errorf(codes.InvalidArgument,
			"too many recipients: %d (max %d)",
			len(req.Recipients), maxRecipients)
	}

	// Resolve each recipient's pkScript and client pubkey from
	// the proto Output destination.
	recipients := make(
		[]wallet.SendRecipient, 0, len(req.Recipients),
	)
	var totalAmount int64

	for i, out := range req.Recipients {
		if out.GetDestination() == nil {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"recipient %d: destination is "+
					"required", i,
			)
		}

		if out.AmountSat <= 0 ||
			out.AmountSat > int64(btcutil.MaxSatoshi) {

			return nil, status.Errorf(
				codes.InvalidArgument,
				"recipient %d: amount must be "+
					"between 1 and %d",
				i, int64(btcutil.MaxSatoshi),
			)
		}

		// Overflow-safe addition.
		if totalAmount > int64(btcutil.MaxSatoshi)-out.AmountSat {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"total amount overflows max supply")
		}

		pkScript, clientKey, err := r.resolveRecipientOutput(
			out,
		)
		if err != nil {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"recipient %d: %v", i, err,
			)
		}

		recipients = append(recipients, wallet.SendRecipient{
			PkScript:  pkScript,
			Amount:    btcutil.Amount(out.AmountSat),
			ClientKey: clientKey,
		})

		totalAmount += out.AmountSat
	}

	// Fetch operator terms for fee, dust limit, exit delay, and
	// operator key.
	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to fetch operator terms: %v", err)
	}

	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Internal,
			"wallet actor not initialized")
	}

	wRef := r.server.walletRef.UnsafeFromSome()

	// Quote the dynamic operator fee for this send. SendVTXO
	// forfeits existing VTXOs and produces new recipient VTXOs
	// inside a round, so this is a "refresh" from the fee-model
	// perspective (is_boarding=false). remainingBlocks is left
	// at zero on purpose: the server's ComputeForfeitFee applies
	// MinRefreshDeltaBlocks as a floor whenever the request value
	// is below it, so passing 0 yields the conservative max-floor
	// quote. The wallet selects forfeit inputs across an arbitrary
	// set of VTXOs with different remaining horizons; there is no
	// single "actual" remaining-blocks value to pass here, and
	// over-quoting (then over-paying) is safer than under-quoting
	// and being rejected by the server's validateOperatorFee path.
	//
	// Like Board, we pay max(quoted, terms.MinOperatorFee) so the
	// round FSM's pre-flight check at round/transitions.go is
	// satisfied even when the dynamic schedule sits below the
	// legacy floor.
	operatorFee := terms.MinOperatorFee
	if totalAmount > 0 {
		quoted, qErr := r.server.quoteOperatorFee(
			ctx, totalAmount,
			false /* isBoarding */, 0,
		)
		switch {
		case qErr != nil:
			r.server.log.WarnS(ctx,
				"EstimateFee unavailable for SendVTXO; "+
					"falling back to legacy flat "+
					"MinOperatorFee", qErr)

		case quoted > terms.MinOperatorFee:
			operatorFee = quoted
		}
	}

	sendReq := &wallet.SendVTXOsRequest{
		Recipients:    recipients,
		OperatorFee:   operatorFee,
		DustLimit:     terms.DustLimit,
		OperatorKey:   terms.PubKey,
		VTXOExitDelay: terms.VTXOExitDelay,
		DryRun:        req.DryRun,
	}

	future := wRef.Ask(ctx, sendReq)
	result := future.Await(ctx)

	resp, err := result.Unpack()
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"send failed: %v", err)
	}

	sendResp, ok := resp.(*wallet.SendVTXOsResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal,
			"unexpected response type: %T", resp)
	}

	r.server.log.InfoS(ctx, "SendVTXO completed",
		slog.String("status", sendResp.Status),
		slog.Int("selected_count",
			sendResp.SelectedCount),
		slog.Int64("total_selected",
			int64(sendResp.TotalSelected)),
		slog.Int64("change", int64(sendResp.ChangeAmount)))

	return &daemonrpc.SendVTXOResponse{
		Status:          sendResp.Status,
		TotalAmountSat:  totalAmount,
		ChangeAmountSat: int64(sendResp.ChangeAmount),
		SelectedCount:   int32(sendResp.SelectedCount),
	}, nil
}

// SendOOR initiates an out-of-round transfer directly between the
// client and operator, without waiting for a round. The transfer
// completes asynchronously via the OOR protocol.
//
//nolint:funlen
func (r *RPCServer) SendOOR(ctx context.Context,
	req *daemonrpc.SendOORRequest) (
	*daemonrpc.SendOORResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if req.Recipient == nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"recipient is required")
	}

	if req.Recipient.AmountSat <= 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"amount must be positive")
	}

	// Resolve the recipient's pkScript from the destination
	// oneof. Pubkey destinations need operator terms to derive
	// VTXO-compatible taproot outputs, so we pass a lazy fetcher.
	pkScript, err := r.resolveOutputPkScript(ctx, req.Recipient)
	if err != nil {
		return nil, err
	}

	// For dry_run, validate inputs and return a preview without
	// contacting the operator.
	if req.DryRun {
		return &daemonrpc.SendOORResponse{
			Status: "preview",
		}, nil
	}

	if r.server.actorSystem == nil {
		return nil, status.Errorf(codes.Internal,
			"actor system not initialized")
	}

	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Internal,
			"wallet actor not initialized")
	}

	if r.server.vtxoStore == nil {
		return nil, status.Errorf(codes.Internal,
			"VTXO store not initialized")
	}

	// Fetch operator terms for the checkpoint policy.
	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"unable to fetch operator terms: %v", err)
	}

	policy := arkscript.CheckpointPolicy{
		OperatorKey: terms.PubKey,
		CSVDelay:    terms.VTXOExitDelay,
	}

	recipientPolicyTemplate, err := r.resolveOutputPolicyTemplate(
		ctx, req.Recipient, pkScript, terms.PubKey,
		terms.VTXOExitDelay,
	)
	if err != nil {
		return nil, err
	}

	var (
		selectedInputs []oor.TransferInput
		locked         *wallet.SelectAndLockVTXOsResponse
	)

	if len(req.CustomInputs) > 0 {
		// Custom inputs provided — bypass wallet selection and
		// build TransferInputs from the specified VTXOs.
		//
		// Custom inputs typically refer to non-standard VTXOs
		// (e.g. vHTLC claim outpoints) that live outside the
		// wallet's managed set and therefore are not reserved
		// through the VTXO manager's PendingForfeit flow.
		// Without any dedup, two concurrent SendOOR callers
		// supplying the same custom input could each succeed
		// through BuildCustomTransferInputs and double-sign the
		// same outpoint. We gate that race here with an
		// in-memory reservation keyed by outpoint; the
		// reservation is released regardless of success or
		// failure via defer.
		customOutpoints := make(
			[]wire.OutPoint, 0, len(req.CustomInputs),
		)
		for _, ci := range req.CustomInputs {
			op, err := parseOutpointString(ci.Outpoint)
			if err != nil {
				return nil, status.Errorf(
					codes.InvalidArgument,
					"parse custom input outpoint %q: %v",
					ci.Outpoint, err)
			}

			customOutpoints = append(customOutpoints, op)
		}

		release, err := r.reserveCustomInputs(customOutpoints)
		if err != nil {
			return nil, status.Errorf(
				codes.Aborted,
				"custom input double-use: %v", err)
		}
		defer release()

		selectedInputs, err = BuildCustomTransferInputs(
			ctx, r.server.vtxoStore, req.CustomInputs,
			r.server.clientKeyDesc, terms.PubKey,
			terms.VTXOExitDelay,
		)
		if err != nil {
			return nil, status.Errorf(codes.Internal,
				"build custom inputs: %v", err)
		}
	} else {
		// Standard path: select and lock VTXOs from wallet.
		targetAmt := btcutil.Amount(req.Recipient.AmountSat)
		wRef := r.server.walletRef.UnsafeFromSome()

		selectReq := &wallet.SelectAndLockVTXOsRequest{
			TargetAmount: targetAmt,
		}
		selectFuture := wRef.Ask(ctx, selectReq)
		selectResult := selectFuture.Await(ctx)

		selectResp, err := selectResult.Unpack()
		if err != nil {
			return nil, status.Errorf(codes.Internal,
				"VTXO selection failed: %v", err)
		}

		var ok bool
		locked, ok = selectResp.(*wallet.SelectAndLockVTXOsResponse)
		if !ok {
			return nil, status.Errorf(codes.Internal,
				"unexpected response type: %T",
				selectResp)
		}

		outpoints := make(
			[]wire.OutPoint, 0,
			len(locked.SelectedVTXOs),
		)
		for _, sv := range locked.SelectedVTXOs {
			outpoints = append(
				outpoints, sv.Outpoint,
			)
		}

		selectedInputs, err = BuildTransferInputs(
			ctx, r.server.vtxoStore, outpoints,
		)
		if err != nil {
			if unlockErr := r.unlockVTXOs(
				ctx, locked.SelectedVTXOs,
			); unlockErr != nil {
				r.server.log.ErrorS(ctx,
					"Unable to unlock VTXOs",
					unlockErr,
				)
			}

			return nil, status.Errorf(codes.Internal,
				"build transfer inputs: %v", err)
		}
	}

	targetAmt := btcutil.Amount(req.Recipient.AmountSat)

	// Build the recipient output for the OOR transfer.
	recipients := []oortx.RecipientOutput{{
		PkScript:           pkScript,
		Value:              targetAmt,
		VTXOPolicyTemplate: recipientPolicyTemplate,
	}}

	// Resolve the OOR actor via the service key registered in the
	// actor system's receptionist. This avoids holding a direct
	// reference and is the canonical way to interact with
	// service-registered actors.
	oorKey := oor.NewServiceKey()
	oorRef := oorKey.Ref(r.server.actorSystem)

	oorReq := &oor.StartTransferRequest{
		Policy:     policy,
		Inputs:     selectedInputs,
		Recipients: recipients,
	}

	future := oorRef.Ask(ctx, oorReq)
	oorResult := future.Await(ctx)

	oorResp, err := oorResult.Unpack()
	if err != nil {
		// Unlock VTXOs on OOR failure so they can be
		// reused (only for wallet-selected inputs).
		if locked != nil {
			if unlockErr := r.unlockVTXOs(
				ctx, locked.SelectedVTXOs,
			); unlockErr != nil {
				r.server.log.ErrorS(ctx,
					"Unable to unlock VTXOs",
					unlockErr,
				)
			}
		}

		return nil, status.Errorf(codes.Internal,
			"OOR transfer failed: %v", err)
	}

	resp, ok := oorResp.(*oor.StartTransferResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal,
			"unexpected response type: %T", oorResp)
	}

	r.server.log.InfoS(ctx, "OOR transfer submitted",
		slog.String("session_id", resp.SessionID.String()),
		slog.Int64("amount_sat", req.Recipient.AmountSat))

	return &daemonrpc.SendOORResponse{
		Status:    "submitted",
		SessionId: resp.SessionID.String(),
	}, nil
}

// unlockVTXOs sends an UnlockVTXOsRequest to the wallet actor for the
// given set of selected VTXOs. This is used for cleanup when an OOR
// transfer fails.
func (r *RPCServer) unlockVTXOs(ctx context.Context,
	vtxos []wallet.SelectedVTXO) error {

	if !r.server.walletRef.IsSome() {
		return nil
	}

	outpoints := make([]wire.OutPoint, 0, len(vtxos))
	for _, sv := range vtxos {
		outpoints = append(outpoints, sv.Outpoint)
	}

	wRef := r.server.walletRef.UnsafeFromSome()

	return wRef.Tell(ctx, &wallet.UnlockVTXOsRequest{
		Outpoints: outpoints,
	})
}

// resolveRecipientOutput extracts both the pkScript and the client
// public key from an Output proto. The client key is required for
// constructing VTXO descriptors in directed sends, so policy-template
// destinations must decode to the standard Ark VTXO shape with an
// explicit owner key.
func (r *RPCServer) resolveRecipientOutput(
	out *daemonrpc.Output) ([]byte, *btcec.PublicKey, error) {

	switch d := out.Destination.(type) {
	case *daemonrpc.Output_Pubkey:
		if len(d.Pubkey) != schnorr.PubKeyBytesLen {
			return nil, nil, fmt.Errorf(
				"pubkey must be %d bytes, got %d",
				schnorr.PubKeyBytesLen,
				len(d.Pubkey),
			)
		}

		clientKey, err := schnorr.ParsePubKey(d.Pubkey)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"invalid pubkey: %w", err,
			)
		}

		// Derive the BIP-86 taproot pkScript from the
		// x-only pubkey.
		addr, err := btcutil.NewAddressTaproot(
			d.Pubkey, r.server.chainParams,
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"derive taproot address: %w", err,
			)
		}

		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"derive pkScript: %w", err,
			)
		}

		return pkScript, clientKey, nil

	case *daemonrpc.Output_Address:
		addr, err := btcutil.DecodeAddress(
			d.Address, r.server.chainParams,
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"invalid address: %w", err,
			)
		}

		// Only taproot addresses carry the x-only pubkey
		// needed for VTXO construction.
		tapAddr, ok := addr.(*btcutil.AddressTaproot)
		if !ok {
			return nil, nil, fmt.Errorf(
				"directed sends require a taproot "+
					"address, got %T", addr,
			)
		}

		clientKey, err := schnorr.ParsePubKey(
			tapAddr.ScriptAddress(),
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"extract pubkey from address: %w",
				err,
			)
		}

		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"derive pkScript: %w", err,
			)
		}

		return pkScript, clientKey, nil

	case *daemonrpc.Output_PolicyTemplate:
		if len(d.PolicyTemplate) == 0 {
			return nil, nil, fmt.Errorf(
				"policy_template is empty",
			)
		}

		template, err := arkscript.DecodePolicyTemplate(
			d.PolicyTemplate,
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"decode policy_template: %w", err,
			)
		}

		params, err := arkscript.DecodeStandardVTXOParams(
			template,
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"directed sends require a standard "+
					"policy_template: %w",
				err,
			)
		}

		pkScript, err := template.PkScript()
		if err != nil {
			return nil, nil, fmt.Errorf(
				"derive pkScript from policy_template: %w",
				err,
			)
		}

		return pkScript, params.OwnerKey, nil

	default:
		return nil, nil, fmt.Errorf(
			"directed sends require pubkey, taproot "+
				"address, or standard policy_template "+
				"destination, got %T", d,
		)
	}
}

// resolveOutputPolicyTemplate returns the semantic policy template to persist
// for one OOR recipient output when it is known locally.
func (r *RPCServer) resolveOutputPolicyTemplate(_ context.Context,
	out *daemonrpc.Output, pkScript []byte, operatorKey *btcec.PublicKey,
	exitDelay uint32) ([]byte, error) {

	if out == nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"recipient must be provided")
	}

	switch d := out.Destination.(type) {
	case *daemonrpc.Output_PolicyTemplate:
		if len(d.PolicyTemplate) == 0 {
			return nil, status.Errorf(codes.InvalidArgument,
				"policy_template is empty")
		}

		if len(out.VtxoPolicyTemplate) > 0 &&
			!bytes.Equal(
				out.VtxoPolicyTemplate, d.PolicyTemplate,
			) {

			return nil, status.Errorf(codes.InvalidArgument,
				"destination policy_template does not "+
					"match vtxo_policy_template")
		}

		return validateOutputPolicyTemplate(
			pkScript, d.PolicyTemplate, operatorKey,
			exitDelay,
		)

	case nil:
		return nil, status.Errorf(codes.InvalidArgument,
			"recipient destination is required")
	}

	if len(out.VtxoPolicyTemplate) > 0 {
		return validateOutputPolicyTemplate(
			pkScript, out.VtxoPolicyTemplate,
			operatorKey, exitDelay,
		)
	}

	switch d := out.Destination.(type) {
	case *daemonrpc.Output_Pubkey:
		ownerKey, err := schnorr.ParsePubKey(d.Pubkey)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid pubkey: %v", err)
		}

		return encodeStandardRecipientPolicy(
			ownerKey, operatorKey, exitDelay, pkScript,
		)

	default:
		return nil, nil
	}
}

// validateOutputPolicyTemplate verifies one provided semantic policy against
// the derived recipient script, checks structural Ark invariants, and
// returns a defensive copy on success.
//
// The per-leaf exit-delay minimum is intentionally NOT enforced here:
// non-standard recipient policies (e.g. vHTLC) legitimately carry
// protocol-specific unilateral delays that may fall below the operator's
// standard VTXOExitDelay. What IS enforced:
//
//  1. DecodePolicyTemplate — well-formed encoding.
//  2. MatchesPkScript — the semantic policy compiles bit-for-bit to the
//     on-chain output; this is the core binding that prevents quoting
//     one policy on the wire while committing under another.
//  3. ValidatePolicy structural invariants — at least one collab leaf
//     containing the operator, at least one CSV-gated non-operator exit
//     leaf, and no operator-unilateral spend paths anywhere in the tree.
//  4. A non-zero operator exit delay — the zero-delay fail-closed gate
//     inherited from the H-4/H-5 hardening: a zero operator minimum
//     would invite callers to produce policies the operator cannot
//     actually enforce. Standard-shape recipients are checked against
//     their declared exit delay at the server; custom shapes carry
//     their own per-path delays that pkScript binding transitively
//     verifies.
//
// Standard-shape recipients (SendVTXO-directed sends) are pre-filtered by
// DecodeStandardVTXOParams in resolveRecipientOutput before reaching this
// helper, so the weaker structural check here does not relax the
// standard-VTXO contract — it lets the OOR recipient path accept vHTLC
// and other custom shapes, which is the intent of the arkscript policy
// layer.
func validateOutputPolicyTemplate(pkScript,
	policyTemplate []byte, operatorKey *btcec.PublicKey,
	minExitDelay uint32) ([]byte, error) {

	template, err := arkscript.DecodePolicyTemplate(
		policyTemplate,
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"decode policy_template: %v", err)
	}

	if !template.MatchesPkScript(pkScript) {
		return nil, status.Errorf(codes.InvalidArgument,
			"policy_template does not match output script")
	}

	if operatorKey != nil {
		// Fail closed on a zero operator minimum: this value is
		// supplied by the server's operator terms and a zero here
		// indicates an unconfigured or degraded operator rather
		// than a legitimate shape choice.
		if minExitDelay == 0 {
			return nil, status.Errorf(
				codes.FailedPrecondition,
				"operator exit delay must be non-zero",
			)
		}

		nodes := make(
			[]arkscript.Node, len(template.Leaves),
		)
		for i, leaf := range template.Leaves {
			nodes[i] = leaf.Node
		}

		// Structural invariants only — MinExitDelay is left unset
		// so per-leaf CSV values are not forced to meet the
		// operator's standard-VTXO minimum. See the docstring
		// above for why this is safe.
		if err := arkscript.ValidatePolicy(
			nodes, arkscript.PolicyValidationOpts{
				OperatorKey: operatorKey,
			},
		); err != nil {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"policy violates Ark invariants: %v",
				err,
			)
		}
	}

	return append([]byte(nil), policyTemplate...), nil
}

// encodeStandardRecipientPolicy derives the canonical standard Ark VTXO policy
// and verifies it compiles back to the target output script. Fails closed
// when any required term is missing rather than silently returning a nil
// policy: a zero exit delay or a missing operator key would otherwise
// produce a "policyless" VTXO and silently bypass admission validation.
func encodeStandardRecipientPolicy(ownerKey, operatorKey *btcec.PublicKey,
	exitDelay uint32, pkScript []byte) ([]byte, error) {

	switch {
	case ownerKey == nil:
		return nil, status.Errorf(codes.InvalidArgument,
			"owner key must be provided")

	case operatorKey == nil:
		return nil, status.Errorf(codes.FailedPrecondition,
			"operator key must be fetched before encoding "+
				"standard recipient policy")

	case exitDelay == 0:
		return nil, status.Errorf(codes.FailedPrecondition,
			"operator exit delay must be non-zero")
	}

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		ownerKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"encode standard recipient policy: %v", err)
	}

	template, err := arkscript.DecodePolicyTemplate(policyTemplate)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"decode derived recipient policy: %v", err)
	}

	if !template.MatchesPkScript(pkScript) {
		return nil, status.Errorf(codes.Internal,
			"derived recipient policy does not match pk_script")
	}

	return policyTemplate, nil
}

// resolveOutputPkScript derives a pkScript from the Output's destination
// oneof. It supports address, pubkey, and semantic policy destinations. For
// pubkey destinations, operator terms are fetched to derive a VTXO-compatible
// taproot output.
func (r *RPCServer) resolveOutputPkScript(ctx context.Context,
	out *daemonrpc.Output) ([]byte, error) {

	switch d := out.Destination.(type) {
	case *daemonrpc.Output_Address:
		addr, err := btcutil.DecodeAddress(
			d.Address, r.server.chainParams,
		)
		if err != nil {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"invalid address: %v", err,
			)
		}

		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, status.Errorf(
				codes.Internal,
				"unable to derive pkScript: %v",
				err,
			)
		}

		return pkScript, nil

	case *daemonrpc.Output_Pubkey:
		if len(d.Pubkey) == 0 {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"pubkey is empty",
			)
		}

		recipientKey, err := schnorr.ParsePubKey(d.Pubkey)
		if err != nil {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"invalid pubkey: %v", err,
			)
		}

		// For OOR sends, a pubkey destination creates a
		// VTXO-compatible taproot output with the operator's
		// collab key and exit delay (not a simple BIP-86
		// key-path-only output). This ensures the recipient
		// gets a standard Ark VTXO they can spend
		// collaboratively or exit unilaterally.
		terms, err := r.server.fetchOperatorTerms(ctx)
		if err != nil {
			return nil, status.Errorf(
				codes.Internal,
				"fetch operator terms for pubkey "+
					"destination: %v", err,
			)
		}

		pkScript, err := BuildPubKeyVTXOReceiveScript(
			recipientKey, terms.PubKey,
			terms.VTXOExitDelay,
		)
		if err != nil {
			return nil, status.Errorf(
				codes.Internal,
				"unable to derive VTXO receive "+
					"script: %v",
				err,
			)
		}

		return pkScript, nil

	case *daemonrpc.Output_PolicyTemplate:
		if len(d.PolicyTemplate) == 0 {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"policy_template is empty",
			)
		}

		template, err := arkscript.DecodePolicyTemplate(
			d.PolicyTemplate,
		)
		if err != nil {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"decode policy_template: %v", err,
			)
		}

		pkScript, err := template.PkScript()
		if err != nil {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"derive pkScript from policy_template: %v",
				err,
			)
		}

		return pkScript, nil

	default:
		return nil, status.Errorf(
			codes.InvalidArgument,
			"unsupported destination type: %T", d,
		)
	}
}

// parseOutpointString parses a "txid:index" string into a
// wire.OutPoint.
func parseOutpointString(s string) (wire.OutPoint, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return wire.OutPoint{}, fmt.Errorf(
			"expected txid:index format")
	}

	hash, err := chainhash.NewHashFromStr(parts[0])
	if err != nil {
		return wire.OutPoint{}, fmt.Errorf(
			"invalid txid: %w", err)
	}

	idx, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return wire.OutPoint{}, fmt.Errorf(
			"invalid index: %w", err)
	}

	return wire.OutPoint{
		Hash:  *hash,
		Index: uint32(idx),
	}, nil
}

// watchRoundsPollInterval is how often WatchRounds polls the round
// actor for state changes.
const watchRoundsPollInterval = 500 * time.Millisecond

// clientStateToProto maps a round.ClientState to the proto RoundState
// enum value.
func clientStateToProto(
	state round.ClientState) daemonrpc.RoundState {

	switch state.(type) {
	case *round.Idle:
		return daemonrpc.RoundState_ROUND_STATE_IDLE

	case *round.PendingRoundAssembly:
		return daemonrpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY

	case *round.RegistrationSentState:
		return daemonrpc.RoundState_ROUND_STATE_REGISTRATION_SENT

	case *round.RoundJoinedState:
		return daemonrpc.RoundState_ROUND_STATE_JOINED

	case *round.CommitmentTxReceivedState:
		return daemonrpc.RoundState_ROUND_STATE_COMMITMENT_RECEIVED

	case *round.CommitmentTxValidatedState:
		return daemonrpc.RoundState_ROUND_STATE_COMMITMENT_VALIDATED

	case *round.ForfeitSignaturesCollectingState:
		return daemonrpc.RoundState_ROUND_STATE_FORFEIT_COLLECTING

	case *round.NoncesSentState:
		return daemonrpc.RoundState_ROUND_STATE_NONCES_SENT

	case *round.NoncesAggregatedState:
		return daemonrpc.RoundState_ROUND_STATE_NONCES_AGGREGATED

	case *round.PartialSigsSentState:
		return daemonrpc.RoundState_ROUND_STATE_PARTIAL_SIGS_SENT

	case *round.InputSigSentState:
		return daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT

	case *round.ConfirmedState:
		return daemonrpc.RoundState_ROUND_STATE_CONFIRMED

	case *round.ClientFailedState:
		return daemonrpc.RoundState_ROUND_STATE_FAILED

	case *round.RecoveryInitiatedState:
		return daemonrpc.RoundState_ROUND_STATE_RECOVERY

	default:
		return daemonrpc.RoundState_ROUND_STATE_UNKNOWN
	}
}

// queryRoundStates fetches the current FSM states from the round actor
// and converts them to proto RoundInfo messages.
func (r *RPCServer) queryRoundStates(
	ctx context.Context) ([]*daemonrpc.RoundInfo, error) {

	if r.server.actorSystem == nil {
		return nil, status.Errorf(codes.Internal,
			"actor system not initialized")
	}

	roundKey := round.NewServiceKey()
	roundRef := roundKey.Ref(r.server.actorSystem)

	stateMsg := &round.GetClientStateRequest{}
	future := roundRef.Ask(ctx, stateMsg)
	result := future.Await(ctx)

	resp, err := result.Unpack()
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"failed to query round state: %v", err)
	}

	stateResp, ok := resp.(*round.GetClientStateResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal,
			"unexpected state response type: %T", resp)
	}

	rounds := make(
		[]*daemonrpc.RoundInfo, 0, len(stateResp.States),
	)
	for _, info := range stateResp.States {
		roundID := ""
		if !info.IsTemp {
			roundID = info.RoundID.String()
		}

		rounds = append(rounds, &daemonrpc.RoundInfo{
			RoundId: roundID,
			State:   clientStateToProto(info.State),
			IsTemp:  info.IsTemp,
		})
	}

	return rounds, nil
}

// defaultListRoundsPageSize is the page size used when the client
// does not specify one.
const defaultListRoundsPageSize = 100

// dbStatusToProto maps a persisted round status string to the proto enum.
func dbStatusToProto(status string) daemonrpc.RoundState {
	switch status {
	case "input_sig_sent":
		return daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT

	case "confirmed":
		return daemonrpc.RoundState_ROUND_STATE_CONFIRMED

	case "failed":
		return daemonrpc.RoundState_ROUND_STATE_FAILED

	default:
		return daemonrpc.RoundState_ROUND_STATE_UNKNOWN
	}
}

// ListRounds returns round state information split into two categories:
//   - Pending rounds: live FSM instances from the round actor. Always
//     returned (unless persisted_only is set) and do not count against
//     page_size.
//   - Persisted rounds: rounds stored on disk (input_sig_sent, confirmed,
//     etc.) returned with cursor-based SQL pagination. Each persisted
//     round includes VTXOs created in that round.
//
// Pending rounds appear first in the response, followed by persisted rounds.
func (r *RPCServer) ListRounds(ctx context.Context,
	req *daemonrpc.ListRoundsRequest) (
	*daemonrpc.ListRoundsResponse, error) {

	var rounds []*daemonrpc.RoundInfo

	// Always include pending (in-memory) rounds unless the caller
	// explicitly requested persisted-only results.
	if !req.PersistedOnly {
		pending, err := r.queryRoundStates(ctx)
		if err != nil {
			return nil, err
		}

		rounds = append(rounds, pending...)
	}

	// Query persisted rounds from SQL with cursor pagination.
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = defaultListRoundsPageSize
	}

	var nextToken string

	if r.server.roundStore != nil {
		// Request one extra row to detect whether a next page
		// exists.
		dbRounds, err := r.server.roundStore.ListRoundsPaginated(
			ctx, req.PageToken, pageSize+1,
		)
		if err != nil {
			return nil, status.Errorf(codes.Internal,
				"failed to list persisted rounds: %v", err)
		}

		// If we got more rows than pageSize, there's a next
		// page.
		if int32(len(dbRounds)) > pageSize {
			dbRounds = dbRounds[:pageSize]
			nextToken = dbRounds[len(dbRounds)-1].RoundID.String()
		}

		for _, s := range dbRounds {
			info := &daemonrpc.RoundInfo{
				RoundId: s.RoundID.String(),
				State:   dbStatusToProto(s.Status),
				IsTemp:  false,
			}

			// Populate VTXO details for persisted rounds.
			for _, v := range s.VTXOs {
				info.Vtxos = append(
					info.Vtxos,
					&daemonrpc.RoundVTXOInfo{
						Outpoint:  v.Outpoint.String(),
						AmountSat: int64(v.Amount),
					},
				)
			}

			rounds = append(rounds, info)
		}
	}

	return &daemonrpc.ListRoundsResponse{
		Rounds:        rounds,
		NextPageToken: nextToken,
	}, nil
}

// WatchRounds opens a server-streaming connection that pushes round
// state updates as they occur. The stream polls the round actor and
// sends updates whenever a round's state changes.
func (r *RPCServer) WatchRounds(
	_ *daemonrpc.WatchRoundsRequest,
	stream daemonrpc.DaemonService_WatchRoundsServer) error {

	ctx := stream.Context()

	// Track previous state snapshot to detect changes.
	prevStates := make(map[string]daemonrpc.RoundState)

	ticker := time.NewTicker(watchRoundsPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
		}

		rounds, err := r.queryRoundStates(ctx)
		if err != nil {
			r.server.log.WarnS(ctx, "WatchRounds poll failed", err)

			continue
		}

		// Diff against previous snapshot and send updates
		// for changed or new rounds.
		for _, info := range rounds {
			key := info.RoundId
			if info.IsTemp {
				key = "temp:" + key
			}

			prev, known := prevStates[key]
			if known && prev == info.State {
				continue
			}

			prevStates[key] = info.State

			if err := stream.Send(
				&daemonrpc.WatchRoundsResponse{
					Round: info,
				},
			); err != nil {
				return err
			}
		}
	}
}

// rpcMailboxAdapter wraps RPCServer to satisfy
// DaemonServiceMailboxServer. The mailbox transport is unary
// request/response, so server-streaming RPCs like WatchRounds
// return an error indicating they are unsupported over mailbox.
type rpcMailboxAdapter struct {
	*RPCServer
}

// WatchRounds is unsupported over the mailbox transport because it
// is a server-streaming RPC. Callers should use the gRPC transport
// for streaming endpoints.
func (a *rpcMailboxAdapter) WatchRounds(_ context.Context,
	_ *daemonrpc.WatchRoundsRequest) (
	*daemonrpc.WatchRoundsResponse, error) {

	return nil, fmt.Errorf("WatchRounds is a server-streaming " +
		"RPC and is not supported over mailbox transport")
}
