//go:build itest

package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// envPrefix is the per-state-key prefix used in the printed
// environment block. Keep it short and ARKTEST_-namespaced so users
// can grep / unset cleanly.
const envPrefix = "ARKTEST_"

func newAliasesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "aliases",
		Short: "Print shell helpers for each running client",
		Long: "Prints a block of bash that exports endpoint env " +
			"vars and defines per-client wrapper functions " +
			"(e.g. alice-cli, bob-cli, alice-lncli). Source it " +
			"with `eval \"$(arktest aliases)\"`.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			state, err := loadState()
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			printEnv(out, state)
			fmt.Fprintln(out)
			printAliases(out, state)

			return nil
		},
	}
}

// printEnv writes a block of `export VAR=value` lines covering the
// running topology. Subsequent alias bodies use these vars.
func printEnv(out io.Writer, s *harnessState) {
	bin := s.BinDir
	darepocli := filepath.Join(bin, "darepocli")
	arkcli := filepath.Join(bin, "arkcli")

	export(out, "DAREPOCLI", darepocli)
	export(out, "ARKCLI", arkcli)
	export(out, "ARK_ADMIN", s.ArkAdminAddr)
	export(out, "ARK_RPC", s.ArkRPCAddr)
	export(out, "BITCOIND_RPC", s.BitcoindRPC)
	export(out, "BITCOIND_RPC_USER", s.BitcoindRPCUser)
	export(out, "BITCOIND_RPC_PASS", s.BitcoindRPCPass)
	export(out, "BITCOIND_CONTAINER", s.BitcoindContainerName)
	export(out, "ESPLORA_URL", s.EsploraURL)

	names := make([]string, 0, len(s.Clients))
	for n := range s.Clients {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		c := s.Clients[n]
		key := strings.ToUpper(n)
		export(out, key+"_RPC", c.RPCAddr)
		export(out, key+"_DATADIR", c.DataDir)

		if lnd, ok := s.ClientLNDs[n]; ok && lnd != nil {
			export(out, key+"_LND_GRPC", lnd.GRPCAddr)
			export(out, key+"_LND_TLSCERT", lnd.TLSCertPath)
			export(out, key+"_LND_MACAROON", lnd.MacaroonPath)
		}
	}
}

func export(out io.Writer, key, value string) {
	fmt.Fprintf(out, "export %s%s=%q\n", envPrefix, key, value)
}

// printAliases emits one wrapper per client, plus an admin wrapper
// for the operator. The wrappers are functions (not aliases) so
// arguments forward cleanly.
func printAliases(out io.Writer, s *harnessState) {
	fmt.Fprintln(out, `arkcli() {`)
	fmt.Fprintln(out, `  "$ARKTEST_ARKCLI" \`)
	fmt.Fprintln(out, `    --rpcserver="$ARKTEST_ARK_ADMIN" \`)
	fmt.Fprintln(out, `    --no-tls "$@"`)
	fmt.Fprintln(out, `}`)

	// btc wraps bitcoin-cli inside the harness bitcoind container so
	// callers don't have to remember the container name, regtest flag,
	// or RPC credentials. Use it like any bitcoin-cli invocation:
	//   btc getblockcount
	//   btc sendtoaddress bcrt1q... 0.001
	fmt.Fprintln(out)
	fmt.Fprintln(out, `btc() {`)
	fmt.Fprintln(out, `  docker exec "$ARKTEST_BITCOIND_CONTAINER" \`)
	fmt.Fprintln(out, `    bitcoin-cli -regtest \`)
	fmt.Fprintln(out, `    -rpcuser="$ARKTEST_BITCOIND_RPC_USER" \`)
	fmt.Fprintln(out, `    -rpcpassword="$ARKTEST_BITCOIND_RPC_PASS" \`)
	fmt.Fprintln(out, `    "$@"`)
	fmt.Fprintln(out, `}`)

	names := make([]string, 0, len(s.Clients))
	for n := range s.Clients {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		key := strings.ToUpper(n)
		fmt.Fprintln(out)
		fmt.Fprintf(out, "%s-cli() {\n", n)
		fmt.Fprintln(out, `  "$ARKTEST_DAREPOCLI" \`)
		fmt.Fprintf(out, "    --rpcserver=\"$ARKTEST_%s_RPC\" \\\n",
			key)
		fmt.Fprintln(out, `    --no-tls "$@"`)
		fmt.Fprintln(out, `}`)

		if _, ok := s.ClientLNDs[n]; !ok {
			continue
		}

		fmt.Fprintln(out)
		fmt.Fprintf(out, "%s-lncli() {\n", n)
		fmt.Fprintln(out, `  lncli --network=regtest \`)
		fmt.Fprintf(
			out, "    --rpcserver=\"$ARKTEST_%s_LND_GRPC\" \\\n",
			key,
		)
		fmt.Fprintf(
			out, "    --tlscertpath=\"$ARKTEST_%s_LND_TLSCERT\" "+
				"\\\n", key,
		)
		fmt.Fprintf(
			out,
			"    --macaroonpath=\"$ARKTEST_%s_LND_MACAROON\" \\\n",
			key,
		)
		fmt.Fprintln(out, `    "$@"`)
		fmt.Fprintln(out, `}`)
	}
}
