package main

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// getDaemonClient establishes a gRPC connection to the daemon and
// returns a DaemonServiceClient. The caller is responsible for closing
// the returned connection.
func getDaemonClient(
	cmd *cobra.Command) (daemonrpc.DaemonServiceClient,
	*grpc.ClientConn, error) {

	rpcServer, _ := cmd.Flags().GetString("rpcserver")
	noTLS, _ := cmd.Flags().GetBool("no-tls")
	tlsCertPath, _ := cmd.Flags().GetString("tlscertpath")

	var opts []grpc.DialOption

	switch {
	case noTLS:
		opts = append(opts, grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		))

	case tlsCertPath != "":
		creds, err := credentials.NewClientTLSFromFile(
			tlsCertPath, "",
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"unable to load TLS cert: %w", err)
		}

		opts = append(opts, grpc.WithTransportCredentials(
			creds,
		))

	default:
		// Default to insecure for dev convenience. Production
		// deployments should use --tlscertpath.
		opts = append(opts, grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		))
	}

	conn, err := grpc.NewClient(rpcServer, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"unable to connect to daemon at %s: %w",
			rpcServer, err)
	}

	client := daemonrpc.NewDaemonServiceClient(conn)

	return client, conn, nil
}
