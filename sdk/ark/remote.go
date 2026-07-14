package ark

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// RemoteConfig configures a connection to a standalone waved instance.
type RemoteConfig struct {
	// Address is the gRPC address of the standalone daemon.
	Address string

	// Credentials secures the remote daemon connection. Callers must
	// either provide transport credentials here or opt into
	// AllowInsecure for local development setups.
	Credentials credentials.TransportCredentials

	// AllowInsecure opts into plaintext transport for local development.
	// It is rejected by default so remote daemon access is
	// secure-by-default.
	AllowInsecure bool

	// DialOptions appends extra gRPC dial options after the SDK's transport
	// credential choice, so later options may override the SDK's default
	// transport credentials.
	DialOptions []grpc.DialOption
}

// DialRemote connects the SDK facade to a remote waved gRPC endpoint. The
// context is currently used only as an upfront cancellation signal because
// grpc.NewClient performs dialing asynchronously.
func DialRemote(ctx context.Context, cfg RemoteConfig) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("dial remote daemon: %w", err)
	}

	if cfg.Address == "" {
		return nil, fmt.Errorf("remote address is required")
	}

	creds := cfg.Credentials
	if creds == nil {
		if !cfg.AllowInsecure {
			return nil, fmt.Errorf("remote credentials are " +
				"required unless AllowInsecure is set")
		}

		creds = insecure.NewCredentials()
	}

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}
	dialOpts = append(dialOpts, cfg.DialOptions...)

	conn, err := grpc.NewClient(cfg.Address, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial remote daemon: %w", err)
	}

	return &Client{
		daemon: waverpc.NewDaemonServiceClient(conn),
		waitCh: closedWaitChan(),
		closeFn: func(context.Context) error {
			return conn.Close()
		},
	}, nil
}
