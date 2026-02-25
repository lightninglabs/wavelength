package darepod

import (
	"context"

	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/daemonrpc"
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
		Version: build.Version(),
		Commit:  build.CommitHash,
		Network: r.server.cfg.Network,
	}

	// Populate lnd fields if connected.
	if r.server.lnd != nil {
		resp.LndIdentityPubkey = r.server.lnd.NodePubkey.String()
		resp.LndAlias = r.server.lnd.NodeAlias

		// Fetch the current best block height from the chain
		// backend via lnd's ChainKit interface.
		_, height, err := r.server.lnd.ChainKit.GetBestBlock(ctx)
		if err != nil {
			log.WarnS(ctx, "Unable to fetch block height", err)
		} else {
			resp.BlockHeight = uint32(height)
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

	// TODO(roasbeef): populate server connection status from runtime.

	return resp, nil
}
