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

	// TODO(roasbeef): populate server connection status from runtime.

	return resp, nil
}
