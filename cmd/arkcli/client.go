package main

import (
	"crypto/tls"
	"fmt"

	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// getAdminClient establishes a gRPC connection to the admin server and
// returns an OperatorAdminClient. The caller is responsible for closing
// the returned connection.
func getAdminClient(cmd *cobra.Command) (adminrpc.OperatorAdminClient,
	*grpc.ClientConn, error) {

	rpcServer, _ := cmd.Flags().GetString("rpcserver")
	noTLS, _ := cmd.Flags().GetBool("no-tls")
	tlsCertPath, _ := cmd.Flags().GetString("tlscertpath")

	var opts []grpc.DialOption

	switch {
	case noTLS:
		opts = append(
			opts,
			grpc.WithTransportCredentials(
				insecure.NewCredentials(),
			),
		)

	case tlsCertPath != "":
		creds, err := credentials.NewClientTLSFromFile(
			tlsCertPath, "",
		)
		if err != nil {
			return nil, nil, fmt.Errorf("unable to load TLS "+
				"cert: %w", err)
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
		return nil, nil, fmt.Errorf("unable to connect to admin "+
			"server at %s: %w", rpcServer, err)
	}

	client := adminrpc.NewOperatorAdminClient(conn)

	return client, conn, nil
}
