//go:build itest

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const defaultBoardAmountSat = int64(100_000_000)

func newBoardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "board <client> [amount-sat]",
		Short: "Fund a client's boarding address with regtest coins",
		Long: "Asks the named client daemon for a fresh boarding " +
			"address (its NewAddress RPC), faucets the given " +
			"amount, and mines 6 blocks so the boarding output " +
			"is confirmed. The client can then `<name>-cli " +
			"board` to register it into the next round. The " +
			"resulting taproot UTXO is multisig-script-spend " +
			"only — it is not picked up by `selectFeeInput` " +
			"because boarding-typed addresses are tracked " +
			"separately from the LND backing wallet's regular " +
			"key-spend UTXOs.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			amount := defaultBoardAmountSat
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

			client, ok := state.Clients[name]
			if !ok {
				return fmt.Errorf("no client named %q in "+
					"state (running clients: %v)",
					name, clientNames(state))
			}

			ctx, cancel := context.WithTimeout(
				context.Background(), 60*time.Second,
			)
			defer cancel()

			conn, err := grpc.NewClient(
				client.RPCAddr, grpc.WithTransportCredentials(
					insecure.NewCredentials(),
				),
			)
			if err != nil {
				return fmt.Errorf("dial %s: %w",
					client.RPCAddr, err)
			}
			defer func() { _ = conn.Close() }()

			rpc := daemonrpc.NewDaemonServiceClient(conn)
			addrResp, err := rpc.NewAddress(
				ctx, &daemonrpc.NewAddressRequest{},
			)
			if err != nil {
				return fmt.Errorf("NewAddress: %w", err)
			}

			amt := btcutil.Amount(amount).ToBTC()
			if err := callBitcoindRPC(
				ctx, state, "sendtoaddress",
				[]any{addrResp.Address, amt}, nil,
			); err != nil {
				return fmt.Errorf("sendtoaddress: %w", err)
			}

			var miner string
			if err := callBitcoindRPC(
				ctx, state, "getnewaddress", nil, &miner,
			); err != nil {
				return fmt.Errorf("getnewaddress: %w", err)
			}
			var hashes []string
			if err := callBitcoindRPC(
				ctx, state, "generatetoaddress",
				[]any{6, miner}, &hashes,
			); err != nil {
				return fmt.Errorf("mine: %w", err)
			}

			// Persist the boarding details so `arktest info` can
			// surface them and so we have a record of what was
			// faucet'd.
			client.BoardingAddress = addrResp.Address
			client.BoardingAmount = amount
			client.BoardingConfirmed = true

			if err := saveState(state); err != nil {
				return fmt.Errorf("persist boarding "+
					"state: %w", err)
			}

			out := cmd.OutOrStdout()
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")

			return enc.Encode(map[string]any{
				"client":              name,
				"boarding_address":    addrResp.Address,
				"boarding_amount_sat": amount,
			})
		},
	}
}

func clientNames(s *harnessState) []string {
	names := make([]string, 0, len(s.Clients))
	for n := range s.Clients {
		names = append(names, n)
	}

	return names
}
