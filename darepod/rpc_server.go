package darepod

import (
	"context"

	"github.com/lightninglabs/darepo-client/build"
	"google.golang.org/grpc"
)

// DaemonServiceServer is the interface for the daemon's own gRPC service.
// This will be replaced by the generated proto interface once we add the
// proto definition.
type DaemonServiceServer interface {
	// GetInfo returns basic information about the running daemon.
	GetInfo(context.Context, *GetInfoRequest) (*GetInfoResponse, error)
}

// GetInfoRequest is the request message for GetInfo.
type GetInfoRequest struct{}

// GetInfoResponse is the response message for GetInfo.
type GetInfoResponse struct {
	// Version is the daemon's semantic version.
	Version string

	// Commit is the short git commit hash of this build.
	Commit string

	// Network is the active bitcoin network.
	Network string

	// LndIdentityPubkey is the identity public key of the connected lnd
	// node.
	LndIdentityPubkey string

	// BlockHeight is the current best block height known to lnd.
	BlockHeight uint32

	// ServerConnected indicates whether the mailbox connection to the
	// ark operator is active.
	ServerConnected bool
}

// RegisterDaemonServiceServer registers the DaemonServiceServer
// implementation with the gRPC server.
func RegisterDaemonServiceServer(s *grpc.Server, srv DaemonServiceServer) {
	// TODO(roasbeef): replace with generated proto registration once
	// daemon.proto is added.
	_ = s
	_ = srv
}

// RPCServer implements the daemon's gRPC interface.
type RPCServer struct {
	server *Server
}

// NewRPCServer creates a new RPCServer backed by the given Server.
func NewRPCServer(server *Server) *RPCServer {
	return &RPCServer{
		server: server,
	}
}

// GetInfo returns basic information about the running daemon instance.
func (r *RPCServer) GetInfo(ctx context.Context,
	_ *GetInfoRequest) (*GetInfoResponse, error) {

	resp := &GetInfoResponse{
		Version: build.Version(),
		Commit:  build.CommitHash,
		Network: r.server.cfg.Network,
	}

	// Populate lnd info if connected.
	if r.server.lnd != nil {
		resp.LndIdentityPubkey = r.server.lnd.NodePubkey.String()
	}

	// TODO(roasbeef): populate block height from lnd chain client.
	// TODO(roasbeef): populate server connection status.

	return resp, nil
}
