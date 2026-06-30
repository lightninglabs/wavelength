package darepod

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/rpcauth"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
)

// localRPCServerOptions returns TLS and macaroon options for the daemon RPC
// server.
func (s *Server) localRPCServerOptions() (*lndclient.MacaroonService,
	[]grpc.ServerOption, error) {

	var (
		authService       *lndclient.MacaroonService
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
		dbStore := db.NewStore(
			s.db.DB, s.db.Queries, s.db.Backend(),
			s.subLogger(db.Subsystem),
		)
		rootKeyStore := dbStore.NewMacaroonRootKeyStore()

		if err := os.MkdirAll(
			filepath.Dir(s.cfg.RPC.MacaroonPath), 0o700,
		); err != nil {
			return nil, nil, fmt.Errorf("create rpc "+
				"macaroon dir: %w", err)
		}

		var err error
		authService, err = lndclient.NewMacaroonService(
			&lndclient.MacaroonServiceConfig{
				RootKeyStore:     rootKeyStore,
				MacaroonLocation: darepodMacaroonEntity,
				MacaroonPath:     s.cfg.RPC.MacaroonPath,
				Checkers: []macaroons.Checker{
					macaroons.IPLockChecker,
					macaroons.IPRangeLockChecker,
				},
				RequiredPerms: darepodRPCPermissions,
			},
		)
		if err != nil {
			return nil, nil, fmt.Errorf("load rpc macaroon "+
				"auth: %w", err)
		}
		if err := authService.Start(); err != nil {
			return nil, nil, fmt.Errorf("start rpc macaroon "+
				"auth: %w", err)
		}
		if err := os.Chmod(s.cfg.RPC.MacaroonPath, 0o600); err != nil {
			_ = authService.Stop()

			return nil, nil, fmt.Errorf("chmod rpc macaroon: %w",
				err)
		}

		macaroonUnaryInterceptor, macaroonStreamInterceptor, err :=
			authService.Interceptors()
		if err != nil {
			_ = authService.Stop()

			return nil, nil, fmt.Errorf("create rpc macaroon "+
				"interceptors: %w", err)
		}

		unaryInterceptors = append(
			unaryInterceptors, macaroonUnaryInterceptor,
		)
		serverOptions = append(
			serverOptions, grpc.ChainStreamInterceptor(
				macaroonStreamInterceptor,
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
