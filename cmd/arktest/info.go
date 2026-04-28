//go:build itest

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"
)

type infoResponse struct {
	State      *harnessState `json:"state"`
	BlockCount uint32        `json:"block_count"`
}

func newInfoCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "info",
		Short: "Show the running topology endpoints",
		RunE: func(cmd *cobra.Command, _ []string) error {
			state, err := loadState()
			if err != nil {
				return err
			}

			var blockCount uint32
			if err := callBitcoindRPC(
				context.Background(), state, "getblockcount",
				nil, &blockCount,
			); err != nil {
				return err
			}

			resp := infoResponse{
				State:      state,
				BlockCount: blockCount,
			}

			out := cmd.OutOrStdout()
			if jsonOutput {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}

			printInfo(out, resp)

			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON output")

	return cmd
}

func printInfo(out io.Writer, resp infoResponse) {
	state := resp.State

	fmt.Fprintf(out, "state:          %s\n", state.StateFile)
	fmt.Fprintf(out, "artifacts:      %s\n", state.ArtifactsDir)
	fmt.Fprintf(out, "block height:   %d\n", resp.BlockCount)
	fmt.Fprintf(out, "ark admin:      %s\n", state.ArkAdminAddr)
	fmt.Fprintf(out, "ark rpc:        %s\n", state.ArkRPCAddr)
	fmt.Fprintf(out, "esplora:        %s\n", state.EsploraURL)
	fmt.Fprintf(out, "bitcoind rpc:   %s\n", state.BitcoindRPC)
	fmt.Fprintf(out, "operator lnd:   %s\n", state.OperatorLND.GRPCAddr)

	names := make([]string, 0, len(state.Clients))
	for name := range state.Clients {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		c := state.Clients[name]
		fmt.Fprintf(out, "client %s\n", name)
		fmt.Fprintf(out, "  rpc:          %s\n", c.RPCAddr)
		fmt.Fprintf(out, "  data dir:     %s\n", c.DataDir)
		fmt.Fprintf(out, "  wallet:       %s\n", c.Wallet)
		if c.BoardingAddress != "" {
			fmt.Fprintf(out, "  boarding:     %s (%d sat, "+
				"confirmed=%t)\n", c.BoardingAddress,
				c.BoardingAmount, c.BoardingConfirmed)
		}

		if lnd, ok := state.ClientLNDs[name]; ok && lnd != nil {
			fmt.Fprintf(out, "  lnd grpc:     %s\n", lnd.GRPCAddr)
			fmt.Fprintf(
				out, "  lnd cert:     %s\n", lnd.TLSCertPath,
			)
			fmt.Fprintf(
				out, "  lnd macaroon: %s\n", lnd.MacaroonPath,
			)
		}
	}
}
