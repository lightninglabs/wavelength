package waveclicommands

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/spf13/cobra"
	"gopkg.in/macaroon.v2"
)

// bakedMacaroonResult is the command result. A saved macaroon reports only its
// path so the credential is not also copied into terminal history.
type bakedMacaroonResult struct {
	Macaroon string `json:"macaroon,omitempty"`
	Path     string `json:"path,omitempty"`
}

// newBakeMacaroonCmd creates the scoped macaroon baking command.
func newBakeMacaroonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bakemacaroon <entity:action>...",
		Short: "Bake a scoped daemon macaroon",
		Long: "Bakes a daemon macaroon with known entity/action " +
			"permissions or exact " +
			"uri:/package.Service/Method scopes. " +
			"For example: wavecli bakemacaroon " +
			"uri:/waverpc.DaemonService/GetInfo.",
		RunE: bakeMacaroon,
	}

	cmd.Flags().String(
		"save-to", "", "save the binary macaroon to this path",
	)
	cmd.Flags().Uint64(
		"root-key-id", 0, "numeric macaroon root key ID",
	)
	cmd.Flags().Uint64(
		"valid-for", 0, "validity in seconds (zero means no expiry)",
	)

	return cmd
}

// bakeMacaroon validates CLI permission tuples and calls the daemon baker.
func bakeMacaroon(cmd *cobra.Command, args []string) error {
	rawJSON, err := cmd.Flags().GetString("request-json")
	if err != nil {
		return err
	}
	if rawJSON != "" {
		switch {
		case len(args) != 0:
			return invalidArgs(
				fmt.Errorf("positional permissions cannot " +
					"be combined with --request-json"),
			)

		case cmd.Flags().Changed("root-key-id"):
			return invalidArgs(
				fmt.Errorf("--root-key-id cannot be " +
					"combined with --request-json; set " +
					"rootKeyId in JSON"),
			)
		}
	}

	req := &waverpc.BakeMacaroonRequest{}
	err = parseRequest(cmd, req, func() error {
		if len(args) == 0 {
			return invalidArgs(
				fmt.Errorf("specify at least one " +
					"entity:action permission"),
			)
		}

		permissions, err := parseMacaroonPermissions(args)
		if err != nil {
			return invalidArgs(err)
		}

		rootKeyID, err := cmd.Flags().GetUint64("root-key-id")
		if err != nil {
			return err
		}

		req.Permissions = permissions
		req.RootKeyId = rootKeyID

		return nil
	})
	if err != nil {
		return err
	}

	outputPath, outputFile, err := openMacaroonOutput(cmd)
	if err != nil {
		return err
	}
	keepOutput := false
	if outputFile != nil {
		defer func() {
			if keepOutput {
				return
			}

			_ = outputFile.Close()
			_ = os.Remove(outputPath)
		}()
	}

	client, conn, err := getMacaroonClient(cmd)
	if err != nil {
		return err
	}
	if conn != nil {
		defer func() {
			_ = conn.Close()
		}()
	}

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	resp, err := client.BakeMacaroon(ctx, req)
	if err != nil {
		return fmt.Errorf("BakeMacaroon RPC failed: %w", err)
	}

	macBytes, macHex, err := constrainBakedMacaroon(
		cmd, resp.GetMacaroon(),
	)
	if err != nil {
		return err
	}

	result := bakedMacaroonResult{
		Macaroon: macHex,
	}
	if outputFile != nil {
		if _, err := outputFile.Write(macBytes); err != nil {
			return fmt.Errorf("write baked macaroon: %w", err)
		}
		if err := outputFile.Close(); err != nil {
			return fmt.Errorf("close baked macaroon: %w", err)
		}
		keepOutput = true

		result.Macaroon = ""
		result.Path = outputPath
	}

	return printBakedMacaroonResult(cmd, result)
}

// openMacaroonOutput reserves a new private output file before an RPC can
// mint a credential. Exclusive creation prevents accidental replacement of an
// existing file or symlink.
func openMacaroonOutput(cmd *cobra.Command) (string, *os.File, error) {
	savePath, err := cmd.Flags().GetString("save-to")
	if err != nil {
		return "", nil, err
	}
	if savePath == "" {
		return "", nil, nil
	}

	expandedPath, err := expandCLIPath(savePath)
	if err != nil {
		return "", nil, err
	}

	//nolint:gosec // G304: the destination is supplied by the CLI user.
	file, err := os.OpenFile(
		expandedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600,
	)
	if err != nil {
		return "", nil, fmt.Errorf("create baked macaroon: %w", err)
	}

	return expandedPath, file, nil
}

// constrainBakedMacaroon validates the daemon response and applies optional
// client-side restrictions before the credential is displayed or saved.
func constrainBakedMacaroon(cmd *cobra.Command, macHex string) ([]byte, string,
	error) {

	macBytes, err := hex.DecodeString(macHex)
	if err != nil {
		return nil, "", fmt.Errorf("decode baked macaroon: %w", err)
	}
	if len(macBytes) == 0 {
		return nil, "", fmt.Errorf("daemon returned an empty macaroon")
	}

	rawMacaroon := &macaroon.Macaroon{}
	if err := rawMacaroon.UnmarshalBinary(macBytes); err != nil {
		return nil, "", fmt.Errorf("decode baked macaroon: %w", err)
	}

	validFor, err := cmd.Flags().GetUint64("valid-for")
	if err != nil {
		return nil, "", err
	}
	if validFor > math.MaxInt64 {
		return nil, "", invalidArgs(
			fmt.Errorf(
				"--valid-for exceeds %d", int64(math.MaxInt64),
			),
		)
	}
	if validFor != 0 {
		rawMacaroon, err = macaroons.AddConstraints(
			rawMacaroon,
			macaroons.TimeoutConstraint(
				int64(validFor),
			),
		)
		if err != nil {
			return nil, "", fmt.Errorf("constrain baked "+
				"macaroon: %w", err)
		}

		macBytes, err = rawMacaroon.MarshalBinary()
		if err != nil {
			return nil, "", fmt.Errorf("encode baked macaroon: %w",
				err)
		}
	}

	return macBytes, hex.EncodeToString(macBytes), nil
}

// parseMacaroonPermissions parses entity:action tuples without splitting URI
// actions on their path separators.
func parseMacaroonPermissions(args []string) ([]*waverpc.MacaroonPermission,
	error) {

	permissions := make([]*waverpc.MacaroonPermission, 0, len(args))
	for _, arg := range args {
		entity, action, ok := strings.Cut(arg, ":")
		if !ok || entity == "" || action == "" {
			return nil, fmt.Errorf("invalid permission %q: "+
				"expected entity:action", arg)
		}

		permissions = append(permissions, &waverpc.MacaroonPermission{
			Entity: entity,
			Action: action,
		})
	}

	return permissions, nil
}

// printBakedMacaroonResult writes the stable JSON command result.
func printBakedMacaroonResult(cmd *cobra.Command,
	result bakedMacaroonResult) error {

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal macaroon result: %w", err)
	}

	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))

	return err
}
