package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lightninglabs/darepo-client/adminrpc"
	"github.com/urfave/cli/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultRPCAddr = "localhost:8081"
	defaultTimeout = 10 * time.Second
)

func main() {
	app := &cli.App{
		Name:  "arkcli",
		Usage: "Command line client for the Ark operator",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "rpcserver",
				Aliases: []string{"s"},
				Value:   defaultRPCAddr,
				Usage: "The host:port of the Ark admin RPC " +
					"server",
			},
		},
		Commands: []*cli.Command{
			infoCommand,
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

var infoCommand = &cli.Command{
	Name:   "info",
	Usage:  "Get general information about the operator server",
	Action: info,
}

func info(ctx *cli.Context) error {
	// Get RPC server address from flag.
	rpcAddr := ctx.String("rpcserver")

	// Create connection context with timeout.
	connCtx, connCancel := context.WithTimeout(
		context.Background(), defaultTimeout,
	)
	defer connCancel()

	// Connect to the RPC server.
	conn, err := grpc.DialContext(
		connCtx, rpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("unable to connect to RPC server: %w", err)
	}
	defer conn.Close()

	// Create the admin RPC client.
	client := adminrpc.NewOperatorAdminClient(conn)

	// Make the Info request.
	reqCtx, cancel := context.WithTimeout(
		context.Background(), defaultTimeout,
	)
	defer cancel()

	resp, err := client.Info(reqCtx, &adminrpc.InfoRequest{})
	if err != nil {
		return fmt.Errorf("info request failed: %w", err)
	}

	// Print the information.
	fmt.Printf("Ark Operator Server Info:\n")
	fmt.Printf("  Version: %s\n", resp.Version)
	fmt.Printf("  Network: %s\n", resp.Network)
	fmt.Printf("  Pubkey: %s\n", resp.Pubkey)

	return nil
}
