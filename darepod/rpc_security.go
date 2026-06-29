package darepod

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/rpcauth"
	"google.golang.org/grpc"
)

// localRPCServerOptions returns TLS and macaroon options for the daemon RPC
// server.
func (s *Server) localRPCServerOptions() (*rpcauth.Service, []grpc.ServerOption,
	error) {

	var (
		authService       *rpcauth.Service
		serverOptions     []grpc.ServerOption
		unaryInterceptors = append(
			[]grpc.UnaryServerInterceptor(nil),
			s.cfg.UnaryServerInterceptors...,
		)
	)

	if !s.cfg.RPC.NoTLS {
		err := rpcauth.EnsureTLSCert(
			s.cfg.RPC.TLSCertPath, s.cfg.RPC.TLSKeyPath, "darepod",
		)
		if err != nil {
			return nil, nil, fmt.Errorf("ensure rpc tls cert: %w",
				err)
		}

		creds, err := rpcauth.ServerTLSCredentials(
			s.cfg.RPC.TLSCertPath, s.cfg.RPC.TLSKeyPath,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("load rpc tls cert: %w",
				err)
		}

		serverOptions = append(serverOptions, grpc.Creds(creds))
	}

	if !s.cfg.RPC.NoMacaroons {
		var err error
		authService, err = rpcauth.NewService(
			s.cfg.rpcMacaroonDBPath(), s.cfg.RPC.MacaroonPath,
			"darepod", darepodDefaultMacaroonOps(), nil,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("load rpc macaroon "+
				"auth: %w", err)
		}

		unaryInterceptors = append(
			unaryInterceptors, authService.UnaryServerInterceptor(),
		)
		serverOptions = append(
			serverOptions,
			grpc.ChainStreamInterceptor(
				authService.StreamServerInterceptor(),
			),
		)
	}
	if len(unaryInterceptors) > 0 {
		serverOptions = append(
			serverOptions,
			grpc.ChainUnaryInterceptor(unaryInterceptors...),
		)
	}

	return authService, serverOptions, nil
}
