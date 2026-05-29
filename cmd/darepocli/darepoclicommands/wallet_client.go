package darepoclicommands

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// errWalletRPCDisabled is the structured error printed when the daemon
// is not built with the walletdkrpc tag and a top-level wallet verb is
// invoked against it. The CLI registers the verbs unconditionally so
// agents always see the same surface; the error message points at the
// build documentation so the operator can rebuild the daemon if needed.
var errWalletRPCDisabled = errors.New("daemon was not built with -tags " +
	"walletdkrpc; rebuild with `make build-walletdkrpc` or see " +
	"docs/walletdkrpc_build.md")

// errOffchainOnchainConflict is the canned error returned when a caller
// passes both --offchain and --onchain on the same invocation.
var errOffchainOnchainConflict = errors.New("--offchain and --onchain are " +
	"mutually exclusive; pick one")

type dryRunDetails struct {
	Invoice *dryRunInvoicePreview `json:"invoice,omitempty"`
}

type dryRunInvoicePreview struct {
	Network       string `json:"network"`
	AmountSat     uint64 `json:"amount_sat"`
	PaymentHash   string `json:"payment_hash"`
	CreatedAtUnix int64  `json:"created_at_unix"`
	ExpiresAtUnix int64  `json:"expires_at_unix"`
	ExpirySeconds int64  `json:"expiry_seconds"`
}

// withWalletClient dials the daemon's WalletService and invokes fn with
// the resulting client. The transport reuses the existing getDaemonConn
// helper so the top-level wallet verbs honor the same global flags
// (--rpcserver, --tlscertpath, --no-tls) as every other darepocli verb.
// gRPC UNIMPLEMENTED is mapped to errWalletRPCDisabled so stub-build
// daemons surface a clear, actionable error.
func withWalletClient(cmd *cobra.Command,
	fn func(walletdkrpc.WalletServiceClient) error) error {

	conn, err := getDaemonConn(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	if err := fn(walletdkrpc.NewWalletServiceClient(conn)); err != nil {
		if status.Code(err) == codes.Unimplemented {
			return errWalletRPCDisabled
		}

		return err
	}

	return nil
}

// withWalletInspectionClient dials the daemon's technical inspection service.
func withWalletInspectionClient(cmd *cobra.Command,
	fn func(walletdkrpc.WalletInspectionServiceClient) error) error {

	conn, err := getDaemonConn(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	client := walletdkrpc.NewWalletInspectionServiceClient(conn)
	if err := fn(client); err != nil {
		if status.Code(err) == codes.Unimplemented {
			return errWalletRPCDisabled
		}

		return err
	}

	return nil
}

// walletProtoMarshal is the canonical proto-JSON marshal config for the
// top-level wallet verbs. It matches printJSON's shape so the output is
// uniform across legacy and new commands.
var walletProtoMarshal = protojson.MarshalOptions{
	Indent:          "  ",
	UseProtoNames:   true,
	EmitUnpopulated: true,
}

// printWalletProto writes a proto message as pretty-printed JSON to
// stdout. Used by the top-level wallet verbs in place of the generic
// printJSON because each verb already holds a typed proto response.
func printWalletProto(v proto.Message) error {
	data, err := walletProtoMarshal.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}

	fmt.Fprintln(os.Stdout, string(data))

	return nil
}

// invalidArgs wraps a client-side validation error in the canonical
// INVALID_ARGS envelope so the structured stderr shape and the exit-
// code-2 mapping both kick in. Returns nil if err is nil so callers
// can use it directly in `return invalidArgs(validator(...))` style.
// All seven top-level wallet verbs route their input-hardening
// rejections through this helper so the envelope an agent sees
// matches what swap.* / ark.* verbs emit for the same failure class.
func invalidArgs(err error) error {
	if err == nil {
		return nil
	}

	return PrintError("INVALID_ARGS", err.Error())
}

// walletDryRunPreview emits a structured preview of the RPC that would
// have been dispatched. The fully-validated request body is included
// so an agent staging a transaction can diff the proto-JSON against
// what it intended. A DRY_RUN_OK printedError is returned so main.go
// exits with code 10 — the agent-cli skill's "dry-run passed" marker —
// without re-printing the envelope.
func walletDryRunPreview(method string, req proto.Message,
	details ...*dryRunDetails) error {

	body, err := walletProtoMarshal.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal dry-run preview: %w", err)
	}

	var detailsRaw []byte
	if len(details) > 0 && details[0] != nil {
		detailsRaw, err = json.Marshal(details[0])
		if err != nil {
			return fmt.Errorf("marshal dry-run details: %w", err)
		}
	}

	// stdout carries the machine-readable preview; main.go's DRY_RUN_OK
	// stderr envelope carries the marker that the dry-run validated.
	fmt.Fprintf(os.Stdout, `{"dry_run":true,"method":%q,`+
		`"validation":"passed",`, method,
	)
	if len(detailsRaw) > 0 {
		fmt.Fprintf(os.Stdout, `"details":%s,`, string(detailsRaw))
	}
	fmt.Fprintf(os.Stdout, `"body":%s}`+"\n", string(body))

	return PrintError(
		"DRY_RUN_OK",
		"dry-run validation passed; no mutating RPC was dispatched",
	)
}

// daemonNetwork reads the daemon's configured Bitcoin network for dry-run
// validation that depends on chain context. It uses GetInfo only; no mutating
// wallet RPC is dispatched.
func daemonNetwork(ctx context.Context, cmd *cobra.Command) (string, error) {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	resp, err := client.GetInfo(ctx, &daemonrpc.GetInfoRequest{})
	if err != nil {
		return "", fmt.Errorf("get daemon network: %w", err)
	}

	network := strings.TrimSpace(resp.GetNetwork())
	if network == "" {
		return "", fmt.Errorf("daemon network is empty")
	}

	return network, nil
}

// dryRunInvoiceDetails decodes one offchain send invoice against the daemon's
// active network and returns the parsed fields a caller should confirm before
// dispatching a real payment.
func dryRunInvoiceDetails(invoice,
	daemonNet string) (*dryRunInvoicePreview, error) {

	network := normalizeDaemonNetwork(daemonNet)
	if hrp, invoiceNet := invoiceHRPNetwork(invoice); invoiceNet != "" &&
		invoiceNet != network {
		return nil, fmt.Errorf("invoice HRP %q is for %s; daemon "+
			"is on %s", hrp, invoiceNet, network)
	}

	params, err := chainParamsForDaemonNetwork(network)
	if err != nil {
		return nil, err
	}

	decoded, err := zpay32.Decode(invoice, params)
	if err != nil {
		return nil, fmt.Errorf("decode invoice: %w", err)
	}
	if decoded.PaymentHash == nil {
		return nil, fmt.Errorf("invoice payment hash is required")
	}
	if decoded.MilliSat == nil {
		return nil, fmt.Errorf("invoice amount is required")
	}

	amountMSat := uint64(*decoded.MilliSat)
	if amountMSat == 0 {
		return nil, fmt.Errorf("invoice amount must be positive")
	}
	if amountMSat%1000 != 0 {
		return nil, fmt.Errorf("invoice amount must be whole satoshis")
	}

	expiry := decoded.Expiry()

	return &dryRunInvoicePreview{
		Network:       network,
		AmountSat:     amountMSat / 1000,
		PaymentHash:   hex.EncodeToString(decoded.PaymentHash[:]),
		CreatedAtUnix: decoded.Timestamp.Unix(),
		ExpiresAtUnix: decoded.Timestamp.Add(expiry).Unix(),
		ExpirySeconds: int64(expiry / time.Second),
	}, nil
}

// normalizeDaemonNetwork maps daemon aliases onto the names used by
// Config.Network and the dry-run preview.
func normalizeDaemonNetwork(network string) string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "testnet3", "testnet4":
		return "testnet"

	default:
		return strings.ToLower(strings.TrimSpace(network))
	}
}

// chainParamsForDaemonNetwork returns the chain params zpay32.Decode needs to
// validate the invoice checksum and HRP against the daemon network.
func chainParamsForDaemonNetwork(network string) (*chaincfg.Params, error) {
	switch normalizeDaemonNetwork(network) {
	case "mainnet":
		return &chaincfg.MainNetParams, nil

	case "testnet":
		return &chaincfg.TestNet3Params, nil

	case "regtest":
		return &chaincfg.RegressionNetParams, nil

	case "simnet":
		return &chaincfg.SimNetParams, nil

	case "signet":
		return &chaincfg.SigNetParams, nil

	default:
		return nil, fmt.Errorf("unknown daemon network %q", network)
	}
}

// invoiceHRPNetwork extracts the BOLT-11 currency HRP and maps it to a daemon
// network name when it is one of the networks the CLI knows about.
func invoiceHRPNetwork(invoice string) (string, string) {
	hrp := strings.ToLower(invoice)
	if idx := strings.IndexByte(hrp, '1'); idx >= 0 {
		hrp = hrp[:idx]
	}

	for _, candidate := range []struct {
		hrp     string
		network string
	}{
		{
			"lnbcrt",
			"regtest",
		},
		{
			"lntbs",
			"signet",
		},
		{
			"lntb",
			"testnet",
		},
		{
			"lnbc",
			"mainnet",
		},
		{
			"lnsb",
			"simnet",
		},
	} {
		if strings.HasPrefix(hrp, candidate.hrp) {
			return candidate.hrp, candidate.network
		}
	}

	return hrp, ""
}

// parseEntryKind maps a user-facing kind string to the proto enum used
// in ListRequest.Kinds.
func parseEntryKind(s string) (walletdkrpc.EntryKind, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "send":
		return walletdkrpc.EntryKind_ENTRY_KIND_SEND, nil

	case "recv", "receive":
		return walletdkrpc.EntryKind_ENTRY_KIND_RECV, nil

	case "deposit":
		return walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT, nil

	case "exit":
		return walletdkrpc.EntryKind_ENTRY_KIND_EXIT, nil

	default:
		return walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
			fmt.Errorf("unknown kind %q (send|recv|deposit|exit)",
				s)
	}
}

// parseListView maps a user-facing view string to the proto enum used in
// ListRequest.View. Empty/default falls back to ACTIVITY.
func parseListView(s string) (walletdkrpc.ListView, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "activity":
		return walletdkrpc.ListView_LIST_VIEW_ACTIVITY, nil

	case "vtxos", "vtxo":
		return walletdkrpc.ListView_LIST_VIEW_VTXOS, nil

	case "onchain", "on-chain":
		return walletdkrpc.ListView_LIST_VIEW_ONCHAIN, nil

	default:
		return walletdkrpc.ListView_LIST_VIEW_UNSPECIFIED,
			fmt.Errorf("unknown view %q (activity|vtxos|onchain)",
				s)
	}
}

// resolveOffchainFlag enforces the --offchain / --onchain invariant: at
// most one may be set, and absence implies offchain (the default for
// send and recv).
func resolveOffchainFlag(cmd *cobra.Command) (bool, error) {
	offchain, _ := cmd.Flags().GetBool("offchain")
	onchain, _ := cmd.Flags().GetBool("onchain")

	switch {
	case offchain && onchain:
		return false, errOffchainOnchainConflict

	case onchain:
		return false, nil

	default:
		// Either --offchain set or neither set: offchain is the
		// default. Two paths converge here so an agent that omits the
		// flag gets the friendliest behaviour.
		return true, nil
	}
}

// validateFreeText rejects ASCII control characters in caller-supplied
// free-text fields (note, memo). Agents sometimes paste pre-encoded
// strings or embed invisible characters; the daemon does its own
// validation but the CLI is the most common entry point and a clear
// rejection here keeps downstream error surface small.
func validateFreeText(name, s string) error {
	if s == "" {
		return nil
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s contains invalid UTF-8", name)
	}
	for i, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains control character at "+
				"byte %d (0x%02x)", name, i, r)
		}
	}

	return nil
}

// validateDestination rejects empty destination strings and obvious
// agent-hallucination patterns (embedded query params or fragments)
// before the daemon ever sees them.
func validateDestination(dest string) error {
	if dest == "" {
		return fmt.Errorf("destination is required")
	}
	if strings.ContainsAny(dest, "?#") {
		return fmt.Errorf("destination contains query/fragment "+
			"characters (%q)", dest)
	}
	if strings.ContainsAny(dest, " \t\n\r") {
		return fmt.Errorf("destination contains whitespace; got %q",
			dest)
	}

	return nil
}

// validateOutpoint enforces the canonical txid:vout shape for exit
// commands. A precise format check up front avoids the daemon emitting a
// generic InvalidArgument when an agent passes an obviously malformed
// string.
func validateOutpoint(s string) error {
	if s == "" {
		return fmt.Errorf("--outpoint is required (txid:vout)")
	}
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return fmt.Errorf("--outpoint must be txid:vout (got %q)", s)
	}
	if len(parts[0]) != 64 {
		return fmt.Errorf("--outpoint txid must be 64 hex chars (got "+
			"%d in %q)", len(parts[0]), s)
	}
	for _, c := range parts[0] {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') ||
			(c >= 'A' && c <= 'F')
		if !isHex {
			return fmt.Errorf("--outpoint txid contains non-hex "+
				"character %q", c)
		}
	}
	if parts[1] == "" {
		return fmt.Errorf("--outpoint vout is empty in %q", s)
	}
	if _, err := strconv.ParseUint(parts[1], 10, 32); err != nil {
		return fmt.Errorf("--outpoint vout must be a non-negative "+
			"uint32 (got %q in %q): %w", parts[1], s, err)
	}

	return nil
}
