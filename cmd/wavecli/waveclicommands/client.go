package waveclicommands

import (
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lightninglabs/wavelength/rpcauth"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// getDaemonConn establishes a gRPC connection to the daemon. The caller is
// responsible for closing the returned connection.
func getDaemonConn(cmd *cobra.Command) (*grpc.ClientConn, error) {
	rpcServer, err := cliStringFlag(cmd, "rpcserver")
	if err != nil {
		return nil, err
	}
	noTLS, err := cliBoolFlag(cmd, "no-tls")
	if err != nil {
		return nil, err
	}
	tlsCertPath, err := daemonTLSCertPath(cmd)
	if err != nil {
		return nil, err
	}
	macaroonPath, err := daemonMacaroonPath(cmd)
	if err != nil {
		return nil, err
	}
	noMacaroons, err := cliBoolFlag(cmd, "no-macaroons")
	if err != nil {
		return nil, err
	}

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
			return nil, fmt.Errorf("unable to load TLS cert: %w",
				err)
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

	if !noMacaroons && macaroonPath != "" {
		macaroonOpt, err := rpcauth.DialOptionFromFile(
			macaroonPath,
		)
		if err != nil {
			return nil, err
		}

		opts = append(opts, macaroonOpt)
	}

	conn, err := grpc.NewClient(rpcServer, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to daemon at %s: %w",
			rpcServer, err)
	}

	return conn, nil
}

// getDaemonClient establishes a gRPC connection to the daemon and returns a
// DaemonServiceClient. The caller is responsible for closing the returned
// connection.
func getDaemonClient(cmd *cobra.Command) (waverpc.DaemonServiceClient,
	*grpc.ClientConn, error) {

	conn, err := getDaemonConn(cmd)
	if err != nil {
		return nil, nil, err
	}

	client := waverpc.NewDaemonServiceClient(conn)

	return client, conn, nil
}

// daemonTLSCertPath returns the daemon TLS certificate path for wavecli.
func daemonTLSCertPath(cmd *cobra.Command) (string, error) {
	tlsCertPath, err := cliStringFlag(cmd, "tlscertpath")
	if err != nil {
		return "", err
	}
	if tlsCertPath != "" {
		return expandCLIPath(tlsCertPath)
	}

	datadir, err := cliStringFlag(cmd, "datadir")
	if err != nil {
		return "", err
	}
	network, err := cliStringFlag(cmd, "network")
	if err != nil {
		return "", err
	}

	return expandCLIPath(
		filepath.Join(datadir, "data", network, "tls.cert"),
	)
}

// daemonMacaroonPath returns the daemon macaroon path for wavecli.
func daemonMacaroonPath(cmd *cobra.Command) (string, error) {
	macaroonPath, err := cliStringFlag(cmd, "macaroonpath")
	if err != nil {
		return "", err
	}
	if macaroonPath != "" {
		return expandCLIPath(macaroonPath)
	}

	datadir, err := cliStringFlag(cmd, "datadir")
	if err != nil {
		return "", err
	}
	network, err := cliStringFlag(cmd, "network")
	if err != nil {
		return "", err
	}

	return expandCLIPath(
		filepath.Join(datadir, "data", network, "admin.macaroon"),
	)
}

// expandCLIPath expands a leading tilde in CLI path defaults and overrides.
func expandCLIPath(path string) (string, error) {
	switch {
	case path == "":
		return "", nil

	case path == "~":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}

		return home, nil

	case strings.HasPrefix(path, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}

		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil

	default:
		return path, nil
	}
}

// cliStringFlag returns a command flag value, including inherited persistent
// flags from the root command.
func cliStringFlag(cmd *cobra.Command, name string) (string, error) {
	flag := cmd.Flag(name)
	if flag == nil {
		return "", fmt.Errorf("missing CLI flag %q", name)
	}

	return flag.Value.String(), nil
}

// cliBoolFlag returns a command bool flag value, including inherited persistent
// flags from the root command.
func cliBoolFlag(cmd *cobra.Command, name string) (bool, error) {
	flagValue, err := cliStringFlag(cmd, name)
	if err != nil {
		return false, err
	}

	value, err := strconv.ParseBool(flagValue)
	if err != nil {
		return false, fmt.Errorf("parse CLI flag %q: %w", name, err)
	}

	return value, nil
}
