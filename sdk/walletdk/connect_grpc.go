//go:build !js

package walletdk

import (
	"context"
	"errors"
	"fmt"

	"github.com/lightninglabs/wavelength/rpcauth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// connectGRPC connects to an external daemon over native gRPC.
func connectGRPC(ctx context.Context, cfg ConnectConfig) (*Client, error) {
	dialOpts, err := connectGRPCDialOptions(cfg)
	if err != nil {
		return nil, err
	}

	// Reconstruct errors.Is-able sentinels from the daemon's walletdk
	// ErrorInfo details on every wallet RPC.
	dialOpts = append(
		dialOpts,
		grpc.WithChainUnaryInterceptor(errorReconstructInterceptor),
	)

	conn, err := grpc.NewClient(cfg.Address, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial wallet daemon: %w", err)
	}
	if err := waitForReady(ctx, conn, nil); err != nil {
		closeErr := conn.Close()

		return nil, fmt.Errorf("wait for wallet daemon readiness: %w",
			errors.Join(err, closeErr))
	}

	closeFn := func(context.Context) error {
		return conn.Close()
	}

	return newClient(conn, true, closedWaitChan(), closeFn), nil
}

// connectGRPCDialOptions returns transport and macaroon dial options.
func connectGRPCDialOptions(cfg ConnectConfig) ([]grpc.DialOption, error) {
	var creds credentials.TransportCredentials
	if cfg.Insecure {
		creds = insecure.NewCredentials()
	} else {
		var err error
		creds, err = rpcauth.ClientTLSCredentials(cfg.TLSCertPath)
		if err != nil {
			return nil, err
		}
	}

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	if cfg.MacaroonPath != "" {
		macaroonOpt, err := rpcauth.DialOptionFromFile(
			cfg.MacaroonPath,
		)
		if err != nil {
			return nil, err
		}

		dialOpts = append(dialOpts, macaroonOpt)
	}

	dialOpts = append(dialOpts, cfg.DialOptions...)

	return dialOpts, nil
}
