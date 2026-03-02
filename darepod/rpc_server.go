package darepod

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/round"
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
// including version, network, and wallet connection state.
func (r *RPCServer) GetInfo(ctx context.Context,
	_ *daemonrpc.GetInfoRequest) (*daemonrpc.GetInfoResponse, error) {

	resp := &daemonrpc.GetInfoResponse{
		Version: build.Version(),
		Commit:  build.CommitHash,
		Network: r.server.cfg.Network,
	}

	// Populate wallet fields if the provider is initialized.
	if r.server.walletProvider != nil {
		resp.LndIdentityPubkey = r.server.walletProvider.NodePubkey()

		// Fetch the current best block height from the
		// chain backend via the wallet provider.
		chainBackend := r.server.walletProvider.ChainBackend()
		if chainBackend != nil {
			height, _, err := chainBackend.BestBlock(ctx)
			if err != nil {
				log.WarnS(ctx,
					"Unable to fetch block height",
					err)
			} else {
				resp.BlockHeight = uint32(height)
			}
		}
	}

	// Populate server info if operator terms have been fetched.
	if r.server.operatorTerms != nil {
		terms := r.server.operatorTerms

		si := &daemonrpc.ServerInfo{
			OperatorPubkey:    terms.PubKey.SerializeCompressed(),
			BoardingExitDelay: terms.BoardingExitDelay,
			VtxoExitDelay:     terms.VTXOExitDelay,
			ForfeitScript:     terms.ForfeitScript,
			SweepDelay:        terms.SweepDelay,
			DustLimit:         uint64(terms.DustLimit),
			MinBoardingAmount: uint64(terms.MinBoardingAmount),
			MaxBoardingAmount: uint64(terms.MaxBoardingAmount),
			FeeRate:           uint64(terms.FeeRate),
			MinConfirmations:  terms.MinConfirmations,
		}

		if terms.SweepKey != nil {
			si.SweepKey = terms.SweepKey.SerializeCompressed()
		}

		resp.ServerInfo = si
	}

	return resp, nil
}

// RequestRoundOutputs registers desired output amounts for the next
// round by sending a RegisterVTXORequestsRequest to the round actor.
func (r *RPCServer) RequestRoundOutputs(ctx context.Context,
	req *daemonrpc.RequestRoundOutputsRequest,
) (*daemonrpc.RequestRoundOutputsResponse, error) {

	amounts := make([]btcutil.Amount, len(req.Amounts))
	for i, a := range req.Amounts {
		amounts[i] = btcutil.Amount(a)
	}

	msg := &round.RegisterVTXORequestsRequest{
		Amounts: amounts,
	}

	roundKey := round.NewServiceKey()
	future := roundKey.Ref(r.server.actorSystem).Ask(ctx, msg)
	result := future.Await(ctx)
	if result.IsErr() {
		return nil, fmt.Errorf(
			"register vtxo requests: %w", result.Err(),
		)
	}

	return &daemonrpc.RequestRoundOutputsResponse{}, nil
}

// JoinRound tells the round actor to join the current round by
// sending a RegistrationRequested event via ServerMessageNotification.
func (r *RPCServer) JoinRound(ctx context.Context,
	_ *daemonrpc.JoinRoundRequest,
) (*daemonrpc.JoinRoundResponse, error) {

	msg := &round.ServerMessageNotification{
		Message: &round.RegistrationRequested{},
	}

	roundKey := round.NewServiceKey()
	future := roundKey.Ref(r.server.actorSystem).Ask(ctx, msg)
	result := future.Await(ctx)
	if result.IsErr() {
		return nil, fmt.Errorf(
			"join round: %w", result.Err(),
		)
	}

	return &daemonrpc.JoinRoundResponse{}, nil
}

// CompletedRoundID returns the most recently completed round ID by
// querying the round store for active rounds.
func (r *RPCServer) CompletedRoundID(ctx context.Context,
	_ *daemonrpc.CompletedRoundIDRequest,
) (*daemonrpc.CompletedRoundIDResponse, error) {

	if r.server.roundStore == nil {
		return nil, fmt.Errorf("round store not initialized")
	}

	rounds, err := r.server.roundStore.ListActiveRounds(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"list active rounds: %w", err,
		)
	}

	// Find the most recent round that has a confirmation.
	var latestRoundID string
	for _, rnd := range rounds {
		if rnd.ConfInfo.IsSome() {
			latestRoundID = rnd.RoundID.String()
		}
	}

	return &daemonrpc.CompletedRoundIDResponse{
		RoundId: latestRoundID,
	}, nil
}

// ListVTXOs returns all VTXOs currently known to the daemon.
func (r *RPCServer) ListVTXOs(ctx context.Context,
	_ *daemonrpc.ListVTXOsRequest,
) (*daemonrpc.ListVTXOsResponse, error) {

	if r.server.vtxoStore == nil {
		return nil, fmt.Errorf("vtxo store not initialized")
	}

	vtxos, err := r.server.vtxoStore.ListVTXOs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list vtxos: %w", err)
	}

	protoVTXOs := make([]*daemonrpc.VTXOInfo, len(vtxos))
	for i, v := range vtxos {
		var roundID string
		v.RoundID.WhenSome(
			func(id round.RoundID) {
				roundID = id.String()
			},
		)

		protoVTXOs[i] = &daemonrpc.VTXOInfo{
			OutpointHash:   v.Outpoint.Hash[:],
			OutpointIndex:  v.Outpoint.Index,
			Amount:         int64(v.Amount),
			RoundId:        roundID,
			RelativeExpiry: v.Expiry,
		}
	}

	return &daemonrpc.ListVTXOsResponse{
		Vtxos: protoVTXOs,
	}, nil
}

// GetBalance returns the total live VTXO balance by summing all
// VTXOs from the VTXO store.
func (r *RPCServer) GetBalance(ctx context.Context,
	_ *daemonrpc.GetBalanceRequest,
) (*daemonrpc.GetBalanceResponse, error) {

	if r.server.vtxoStore == nil {
		return nil, fmt.Errorf("vtxo store not initialized")
	}

	vtxos, err := r.server.vtxoStore.ListVTXOs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list vtxos: %w", err)
	}

	var total btcutil.Amount
	for _, v := range vtxos {
		total += v.Amount
	}

	return &daemonrpc.GetBalanceResponse{
		Balance: int64(total),
	}, nil
}

// GetOnChainBalance returns the confirmed and unconfirmed on-chain
// wallet balance via the wallet provider.
func (r *RPCServer) GetOnChainBalance(ctx context.Context,
	_ *daemonrpc.GetOnChainBalanceRequest,
) (*daemonrpc.GetOnChainBalanceResponse, error) {

	if r.server.walletProvider == nil {
		return nil, fmt.Errorf(
			"wallet provider not initialized",
		)
	}

	confirmed, unconfirmed, err := r.server.walletProvider.OnChainBalance(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"get on-chain balance: %w", err,
		)
	}

	return &daemonrpc.GetOnChainBalanceResponse{
		Confirmed:   int64(confirmed),
		Unconfirmed: int64(unconfirmed),
	}, nil
}

// GetNewAddress generates a new on-chain receiving address via the
// wallet provider.
func (r *RPCServer) GetNewAddress(ctx context.Context,
	_ *daemonrpc.GetNewAddressRequest,
) (*daemonrpc.GetNewAddressResponse, error) {

	if r.server.walletProvider == nil {
		return nil, fmt.Errorf(
			"wallet provider not initialized",
		)
	}

	addr, err := r.server.walletProvider.NewAddress(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"get new address: %w", err,
		)
	}

	return &daemonrpc.GetNewAddressResponse{
		Address: addr.String(),
	}, nil
}
