//go:build itest

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/spf13/cobra"
)

const defaultFaucetAmountSat = defaultBoardAmountSat

type faucetResponse struct {
	Address      string   `json:"address"`
	AmountSat    int64    `json:"amount_sat"`
	TxID         string   `json:"txid"`
	MinedBlocks  int      `json:"mined_blocks"`
	MinerAddress string   `json:"miner_address"`
	BlockHashes  []string `json:"block_hashes"`
}

// newFaucetCmd creates the generic regtest faucet subcommand.
func newFaucetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "faucet <address> [amount-sat]",
		Short: "Send regtest coins to any address",
		Long: "Sends regtest coins from arktest's bitcoind " +
			"wallet to the provided address, then mines 6 " +
			"blocks so the output is confirmed. The amount " +
			"is expressed in satoshis and " +
			"defaults to the same amount used by `arktest board`.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			address := args[0]

			amount := defaultFaucetAmountSat
			if len(args) == 2 {
				parsed, err := strconv.ParseInt(args[1], 10, 64)
				if err != nil {
					return fmt.Errorf("parse amount: %w",
						err)
				}
				amount = parsed
			}

			if amount <= 0 {
				return fmt.Errorf("amount must be positive")
			}

			state, err := loadState()
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(
				context.Background(), 60*time.Second,
			)
			defer cancel()

			resp, err := faucetAddress(ctx, state, address, amount)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")

			return enc.Encode(resp)
		},
	}
}

// faucetAddress sends sats to address and confirms the faucet transaction.
func faucetAddress(ctx context.Context, state *harnessState, address string,
	amount int64) (*faucetResponse, error) {

	amt := btcutil.Amount(amount).ToBTC()

	var txid string
	if err := callBitcoindRPC(
		ctx, state, "sendtoaddress", []any{address, amt}, &txid,
	); err != nil {
		return nil, fmt.Errorf("sendtoaddress: %w", err)
	}

	var miner string
	if err := callBitcoindRPC(
		ctx, state, "getnewaddress", nil, &miner,
	); err != nil {
		return nil, fmt.Errorf("getnewaddress: %w", err)
	}

	var hashes []string
	if err := callBitcoindRPC(
		ctx, state, "generatetoaddress", []any{6, miner}, &hashes,
	); err != nil {
		return nil, fmt.Errorf("mine: %w", err)
	}

	return &faucetResponse{
		Address:      address,
		AmountSat:    amount,
		TxID:         txid,
		MinedBlocks:  len(hashes),
		MinerAddress: miner,
		BlockHashes:  hashes,
	}, nil
}
