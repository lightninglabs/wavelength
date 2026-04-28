//go:build itest

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

type mineResponse struct {
	Address string   `json:"address"`
	Blocks  int      `json:"blocks"`
	Hashes  []string `json:"hashes"`
}

func newMineCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "mine [blocks]",
		Short: "Mine regtest blocks",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			blocks := 1
			if len(args) == 1 {
				parsed, err := strconv.Atoi(args[0])
				if err != nil {
					return err
				}
				blocks = parsed
			}

			if blocks <= 0 {
				return fmt.Errorf("blocks must be positive")
			}

			state, err := loadState()
			if err != nil {
				return err
			}

			ctx := context.Background()

			var address string
			if err := callBitcoindRPC(
				ctx, state, "getnewaddress", nil, &address,
			); err != nil {
				return err
			}

			var hashes []string
			if err := callBitcoindRPC(
				ctx, state, "generatetoaddress",
				[]any{blocks, address}, &hashes,
			); err != nil {
				return err
			}

			resp := mineResponse{
				Address: address,
				Blocks:  blocks,
				Hashes:  hashes,
			}

			out := cmd.OutOrStdout()
			if jsonOutput {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}

			fmt.Fprintf(
				out, "mined %d block(s) to %s\n", blocks,
				address,
			)

			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON output")

	return cmd
}
