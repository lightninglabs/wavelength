package darepoclicommands

import (
	"crypto/tls"
	"fmt"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// getDaemonConn establishes a gRPC connection to the daemon. The caller is
// responsible for closing the returned connection.
func getDaemonConn(cmd *cobra.Command) (*grpc.ClientConn, error) {
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
			return nil, fmt.Errorf(
				"unable to load TLS cert: %w", err)
		}

		opts = append(opts, grpc.WithTransportCredentials(
			creds,
		))

	default:
		// Default to TLS using the system certificate pool.
		// Use --no-tls for local development without TLS.
		creds := credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12,
		})
		opts = append(opts, grpc.WithTransportCredentials(
			creds,
		))
	}

	conn, err := grpc.NewClient(rpcServer, opts...)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to connect to daemon at %s: %w",
			rpcServer, err)
	}

	return conn, nil
}

// getDaemonClient establishes a gRPC connection to the daemon and returns a
// DaemonServiceClient. The caller is responsible for closing the returned
// connection.
func getDaemonClient(
	cmd *cobra.Command) (daemonrpc.DaemonServiceClient,
	*grpc.ClientConn, error) {

	conn, err := getDaemonConn(cmd)
	if err != nil {
		return nil, nil, err
	}

	client := daemonrpc.NewDaemonServiceClient(conn)

	return client, conn, nil
}
