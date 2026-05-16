//go:build !js

package walletdk

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func connectGRPC(ctx context.Context, cfg ConnectConfig) (*Client, error) {
	dialOpts := append([]grpc.DialOption(nil), cfg.DialOptions...)
	if len(dialOpts) == 0 {
		creds := insecure.NewCredentials()
		dialOpts = append(
			dialOpts, grpc.WithTransportCredentials(creds),
		)
	}

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
