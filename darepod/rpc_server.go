package darepod

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/lwwallet"
	"github.com/lightninglabs/darepo-client/oor"
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

	case daemonrpc.VTXOStatus_VTXO_STATUS_REFRESH_REQUESTED:
		return vtxo.VTXOStatusRefreshRequested, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITING:
		return vtxo.VTXOStatusForfeiting, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED:
		return vtxo.VTXOStatusForfeited, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_SPENT:
		return vtxo.VTXOStatusSpent, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_EXPIRING:
		return vtxo.VTXOStatusExpiring, nil

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

	case vtxo.VTXOStatusRefreshRequested:
		return daemonrpc.VTXOStatus_VTXO_STATUS_REFRESH_REQUESTED

	case vtxo.VTXOStatusForfeiting:
		return daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITING

	case vtxo.VTXOStatusForfeited:
		return daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED

	case vtxo.VTXOStatusSpent:
		return daemonrpc.VTXOStatus_VTXO_STATUS_SPENT

	case vtxo.VTXOStatusExpiring:
		return daemonrpc.VTXOStatus_VTXO_STATUS_EXPIRING

	case vtxo.VTXOStatusFailed:
		return daemonrpc.VTXOStatus_VTXO_STATUS_FAILED

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
	// oneof.
	pkScript, err := r.resolveOutputPkScript(
		req.Recipient,
	)
	if err != nil {
		return nil, err
	}

	// For dry_run, validate inputs and return a preview.
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

	policy := scripts.CheckpointPolicy{
		OperatorKey: terms.PubKey,
		CSVDelay:    terms.VTXOExitDelay,
	}

	// Ask the wallet actor to select and lock VTXOs covering the
	// send amount. This is atomic: selected VTXOs are locked to
	// prevent double-spends until the transfer completes or is
	// cancelled.
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

	locked, ok := selectResp.(*wallet.SelectAndLockVTXOsResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal,
			"unexpected response type: %T", selectResp)
	}

	// Look up full VTXO descriptors from the store and build
	// OOR transfer inputs. This is delegated to a package-level
	// function so a future SDK can call it directly.
	outpoints := make(
		[]wire.OutPoint, 0, len(locked.SelectedVTXOs),
	)
	for _, sv := range locked.SelectedVTXOs {
		outpoints = append(outpoints, sv.Outpoint)
	}

	selectedInputs, err := BuildTransferInputs(
		ctx, r.server.vtxoStore, outpoints,
	)
	if err != nil {
		r.unlockVTXOs(ctx, locked.SelectedVTXOs)

		return nil, status.Errorf(codes.Internal,
			"unable to build transfer inputs: %v", err)
	}

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
		// reused.
		r.unlockVTXOs(ctx, locked.SelectedVTXOs)

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
// given set of selected VTXOs. This is a fire-and-forget operation used
// for cleanup when an OOR transfer fails.
func (r *RPCServer) unlockVTXOs(ctx context.Context,
	vtxos []wallet.SelectedVTXO) {

	if !r.server.walletRef.IsSome() {
		return
	}

	outpoints := make([]wire.OutPoint, 0, len(vtxos))
	for _, sv := range vtxos {
		outpoints = append(outpoints, sv.Outpoint)
	}

	wRef := r.server.walletRef.UnsafeFromSome()
	wRef.Tell(ctx, &wallet.UnlockVTXOsRequest{
		Outpoints: outpoints,
	})
}

// resolveOutputPkScript derives a pkScript from the Output's
// destination oneof. It supports address, raw pubkey, and raw
// pkScript destinations.
func (r *RPCServer) resolveOutputPkScript(
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
