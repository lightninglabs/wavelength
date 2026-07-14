package waved

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/rpcauth"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
)

// localRPCServerOptions returns TLS and macaroon options for the daemon RPC
// server.
func (s *Server) localRPCServerOptions(ctx context.Context) (
	*lndclient.MacaroonService, []grpc.ServerOption, error) {

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
			s.cfg.RPC.TLSCertPath, s.cfg.RPC.TLSKeyPath, "waved",
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
				MacaroonLocation: wavedMacaroonLocation,
				MacaroonPath:     s.cfg.RPC.MacaroonPath,
				Checkers: []macaroons.Checker{
					macaroons.IPLockChecker,
					macaroons.IPRangeLockChecker,
				},
				RequiredPerms: wavedRPCPermissions,
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

		if err := bakeReadOnlyMacaroon(
			ctx, authService, s.cfg.RPC.MacaroonPath,
		); err != nil {

			_ = authService.Stop()

			return nil, nil, fmt.Errorf("bake readonly rpc "+
				"macaroon: %w", err)
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

// readOnlyMacaroonFileName is the name of the read-only macaroon baked
// alongside the admin macaroon in the same directory.
const readOnlyMacaroonFileName = "readonly.macaroon"

// bakeReadOnlyMacaroon writes a read-only macaroon next to the admin macaroon
// at adminPath. The read-only macaroon carries the read op for every logical
// entity, so it can invoke every query method but no mutating method. It is
// created only when absent so a previously distributed copy is never
// invalidated; the admin macaroon is enough to bake fresh scoped tokens.
func bakeReadOnlyMacaroon(ctx context.Context,
	authService *lndclient.MacaroonService, adminPath string) error {

	readOnlyPath := filepath.Join(
		filepath.Dir(adminPath), readOnlyMacaroonFileName,
	)
	if _, err := os.Stat(readOnlyPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat readonly macaroon: %w", err)
	}

	idCtx := macaroons.ContextWithRootKeyID(
		ctx, macaroons.DefaultRootKeyID,
	)
	mac, err := authService.NewMacaroon(
		idCtx, macaroons.DefaultRootKeyID,
		wavedReadOnlyPermissions()...,
	)
	if err != nil {
		return fmt.Errorf("new readonly macaroon: %w", err)
	}

	macBytes, err := mac.M().MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshal readonly macaroon: %w", err)
	}

	if err := os.WriteFile(readOnlyPath, macBytes, 0o600); err != nil {
		return fmt.Errorf("write readonly macaroon: %w", err)
	}

	return nil
}
