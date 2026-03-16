package darepod

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/lwwallet"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RPCServer implements the daemon's gRPC DaemonService interface.
type RPCServer struct {
	daemonrpc.UnimplementedDaemonServiceServer

	server *Server
}

// NewRPCServer creates a new RPCServer backed by the given Server.
func NewRPCServer(server *Server) *RPCServer {
	return &RPCServer{
		server: server,
	}
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
			resp.IdentityPubkey =
				lndSvc.NodePubkey.String()

			// Fetch the current best block height from
			// the chain backend via lnd's ChainKit
			// interface.
			_, height, err :=
				lndSvc.ChainKit.GetBestBlock(ctx)
			if err != nil {
				log.WarnS(ctx,
					"Unable to fetch block height",
					err)
			} else {
				resp.BlockHeight = uint32(height)
			}
		},
	)

	// Populate lwwallet fields if the lightweight wallet is active.
	r.server.lwWallet.WhenSome(
		func(w *lwwallet.Wallet) {
			// Derive the node identity key from the wallet
			// keyring using KeyFamilyNodeKey (family 6,
			// index 0), matching lnd's identity key
			// derivation path. DeriveKey (not
			// DeriveNextKey) ensures a stable identity.
			desc, err := w.DeriveKey(
				ctx, keychain.KeyLocator{
					Family: identityKeyFamily,
					Index:  0,
				},
			)
			if err != nil {
				log.WarnS(ctx,
					"Unable to derive identity key",
					err)
			} else {
				resp.IdentityPubkey = fmt.Sprintf(
					"%x",
					desc.PubKey.SerializeCompressed(),
				)
			}

			// Get block height from the chain backend.
			if r.server.chainBackend != nil {
				height, _, err :=
					r.server.chainBackend.BestBlock(
						ctx,
					)
				if err != nil {
					log.WarnS(ctx,
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
			log.WarnS(ctx,
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
			log.WarnS(ctx,
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

	// Convert to proto.
	protoVTXOs := make(
		[]*daemonrpc.VTXO, 0, len(filtered),
	)
	for _, v := range filtered {
		protoVTXOs = append(protoVTXOs,
			descriptorToProto(v))
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
		log.WarnS(ctx, "VTXO refresh error", opErr,
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

	log.InfoS(ctx, "VTXOs queued for refresh",
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
	boardReq := &wallet.BoardRequest{
		MinOperatorFee: terms.MinOperatorFee,
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

			log.InfoS(ctx,
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

	log.InfoS(ctx, "Board request accepted",
		btclog.Fmt("boarding_balance", "%v",
			boardResp.BoardingBalance),
		btclog.Fmt("vtxo_amount", "%v",
			boardResp.VTXOAmount))

	return &daemonrpc.BoardResponse{
		Status: "registered",
	}, nil
}

// SendVTXO initiates an in-round transfer by submitting a refresh
// request with specific recipient outputs to the round coordinator.
// The transfer completes asynchronously when the next round commits.
func (r *RPCServer) SendVTXO(ctx context.Context,
	req *daemonrpc.SendVTXORequest) (
	*daemonrpc.SendVTXOResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if len(req.Recipients) == 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"at least one recipient is required")
	}

	// Validate recipients and compute total amount.
	var totalAmount int64
	for i, out := range req.Recipients {
		if out.GetDestination() == nil {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"recipient %d: destination is "+
					"required", i)
		}

		if out.AmountSat <= 0 {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"recipient %d: amount must be "+
					"positive", i)
		}

		totalAmount += out.AmountSat
	}

	// For dry_run, validate inputs and return a preview.
	if req.DryRun {
		return &daemonrpc.SendVTXOResponse{
			Status:         "preview",
			TotalAmountSat: totalAmount,
		}, nil
	}

	// TODO(roasbeef): In-round directed sends are not yet
	// implemented. The wallet actor's RefreshVTXOsRequest only
	// supports self-refresh (sending back to self), not directed
	// transfers to external recipients. Once the round protocol
	// supports recipient outputs, this handler should build a
	// proper send request with the validated recipients.
	return nil, status.Errorf(codes.Unimplemented,
		"in-round directed sends are not yet implemented; "+
			"use SendOOR for out-of-round transfers")
}

// SendOOR initiates an out-of-round transfer directly between the
// client and operator, without waiting for a round. The transfer
// completes asynchronously via the OOR protocol.
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

	var (
		selectedInputs []oor.TransferInput
		locked         *wallet.SelectAndLockVTXOsResponse
	)

	if len(req.CustomInputs) > 0 {
		// Custom inputs provided — bypass wallet selection
		// and build TransferInputs from the specified VTXOs.
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
				log.ErrorS(ctx,
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
		PkScript: pkScript,
		Value:    targetAmt,
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
				log.ErrorS(ctx,
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

	log.InfoS(ctx, "OOR transfer submitted",
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

// resolveOutputPkScript derives a pkScript from the Output's
// destination oneof. It supports address, raw pubkey, and raw
// pkScript destinations. For pubkey destinations, operator terms
// are fetched to derive a VTXO-compatible taproot output.
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

	case *daemonrpc.Output_PkScript:
		if len(d.PkScript) == 0 {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"pk_script is empty",
			)
		}

		return d.PkScript, nil

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
			log.WarnS(ctx, "WatchRounds poll failed", err)

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
