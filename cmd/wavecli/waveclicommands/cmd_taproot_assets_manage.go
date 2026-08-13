package waveclicommands

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
)

// newTaprootAssetBoardCmd creates the boarding-completion command.
func newTaprootAssetBoardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "board",
		Short: "Board a confirmed onboarded output into a round",
		Long: "Complete an onboarded output's path into the next " +
			"asset round. The daemon rebuilds the boarding " +
			"disclosure from the onboarding named by the " +
			"idempotency key, gathers the confirmation and the " +
			"boarded proof itself, and registers the boarding " +
			"with a matching asset VTXO request. Rerun safely: " +
			"an already-boarded output reports already_boarded.",
		Args: cobra.NoArgs,
		RunE: boardTaprootAsset,
	}

	cmd.Flags().String(
		"idempotency-key", "",
		"the key passed to the onboard command",
	)

	return cmd
}

// boardTaprootAsset invokes the boarding-completion RPC.
func boardTaprootAsset(cmd *cobra.Command, _ []string) error {
	key, _ := cmd.Flags().GetString("idempotency-key")
	if key == "" {
		return invalidArgs(fmt.Errorf("--idempotency-key is required"))
	}
	if err := validateFreeText("--idempotency-key", key); err != nil {
		return invalidArgs(err)
	}

	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	response, err := client.BoardTaprootAsset(
		ctx, &waverpc.BoardTaprootAssetRequest{
			IdempotencyKey: key,
		},
	)
	if err != nil {
		return fmt.Errorf("BoardTaprootAsset RPC failed: %w", err)
	}

	return printJSON(response)
}

// newTaprootAssetClaimCmd creates the exit-claim command.
func newTaprootAssetClaimCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claim",
		Short: "Claim an exited asset VTXO into the tapd wallet",
		Long: "Claim an exited asset VTXO once its unrolled anchor " +
			"has matured past the exit delay. The daemon gathers " +
			"the lineage confirmations itself and spends the " +
			"exit path into a fresh tapd-owned anchor, making " +
			"the units ordinary tapd wallet balance.",
		Args: cobra.NoArgs,
		RunE: claimTaprootAssetVTXO,
	}

	flags := cmd.Flags()
	flags.String("outpoint", "", "the exited asset VTXO (txid:vout)")
	flags.Int64(
		"fee-sat", 0,
		"miner fee paid out of the carrier value (zero estimates one)",
	)

	return cmd
}

// claimTaprootAssetVTXO invokes the exit-claim RPC.
func claimTaprootAssetVTXO(cmd *cobra.Command, _ []string) error {
	outpoint, _ := cmd.Flags().GetString("outpoint")
	feeSat, _ := cmd.Flags().GetInt64("fee-sat")
	if err := validateOutpoint(outpoint); err != nil {
		return invalidArgs(err)
	}
	if feeSat < 0 {
		return invalidArgs(fmt.Errorf("--fee-sat must not be negative"))
	}

	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	response, err := client.ClaimTaprootAssetVTXO(
		ctx, &waverpc.ClaimTaprootAssetVTXORequest{
			Outpoint: outpoint,
			FeeSat:   feeSat,
		},
	)
	if err != nil {
		return fmt.Errorf("ClaimTaprootAssetVTXO RPC failed: %w", err)
	}

	return printJSON(response)
}

// newTaprootAssetListCmd creates the asset VTXO listing command.
func newTaprootAssetListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List asset-bearing VTXOs",
		Long: "List VTXOs carrying Taproot Assets. With --asset-ref " +
			"the daemon filters to one asset; without it every " +
			"asset-bearing VTXO is shown.",
		Args: cobra.NoArgs,
		RunE: listTaprootAssetVTXOs,
	}

	flags := cmd.Flags()
	flags.String("asset-ref", "", "restrict to one asset reference")
	flags.String(
		"status", "",
		"restrict to one VTXO status (e.g. VTXO_STATUS_LIVE)",
	)

	return cmd
}

// listTaprootAssetVTXOs lists VTXOs and keeps the asset-bearing subset.
func listTaprootAssetVTXOs(cmd *cobra.Command, _ []string) error {
	assetRef, _ := cmd.Flags().GetString("asset-ref")
	statusName, _ := cmd.Flags().GetString("status")

	request := &waverpc.ListVTXOsRequest{
		AssetRef:               assetRef,
		ExcludeCheckpointPsbts: true,
	}
	if statusName != "" {
		value, ok := waverpc.VTXOStatus_value[statusName]
		if !ok {
			return invalidArgs(
				fmt.Errorf("unknown --status %q", statusName),
			)
		}
		request.StatusFilter = waverpc.VTXOStatus(value)
	}

	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	response, err := client.ListVTXOs(ctx, request)
	if err != nil {
		return fmt.Errorf("ListVTXOs RPC failed: %w", err)
	}

	// Without an explicit reference the command still only reports
	// asset-bearing VTXOs; plain Bitcoin VTXOs have their own listing.
	if assetRef == "" {
		assetVTXOs := make([]*waverpc.VTXO, 0, len(response.Vtxos))
		for _, v := range response.Vtxos {
			if v.GetTaprootAsset() != nil {
				assetVTXOs = append(assetVTXOs, v)
			}
		}
		response.Vtxos = assetVTXOs
	}

	return printJSON(response)
}

// newTaprootAssetBalanceCmd creates the per-asset balance command.
func newTaprootAssetBalanceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "balance",
		Short: "Show per-asset VTXO balances",
		Long: "Show the wallet's Taproot Asset holdings broken down " +
			"by asset reference, in asset units. The carrier " +
			"satoshis stay part of the Bitcoin balance.",
		Args: cobra.NoArgs,
		RunE: taprootAssetBalance,
	}
}

// taprootAssetBalance renders the asset section of the daemon balance.
func taprootAssetBalance(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	response, err := client.GetBalance(
		ctx, &waverpc.GetBalanceRequest{},
	)
	if err != nil {
		return fmt.Errorf("GetBalance RPC failed: %w", err)
	}

	return printJSONFields(response, []string{"taproot_assets"})
}

// newTaprootAssetSendCmd creates the asset OOR send wrapper.
func newTaprootAssetSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send",
		Short: "Send asset units out of round",
		Long: "Send Taproot Asset units to another Wavelength user " +
			"out of round. The daemon spends one asset VTXO; " +
			"when --amount is below the VTXO's full holding, the " +
			"change rides on a minimal Bitcoin carrier and the " +
			"rest of the selected Bitcoin VTXO returns as plain " +
			"change. The recipient key comes from the " +
			"recipient's 'ark oor receive'. Without --outpoint " +
			"the smallest sufficient asset VTXO is selected.",
		Args: cobra.NoArgs,
		RunE: sendTaprootAsset,
	}

	flags := cmd.Flags()
	flags.String("asset-ref", "", "asset reference to send")
	flags.Uint64("amount", 0, "asset units to send")
	flags.String(
		"recipient-pubkey", "", "recipient receive pubkey (hex)",
	)
	flags.String(
		"outpoint", "",
		"asset VTXO to spend (txid:vout; empty selects one)",
	)
	flags.String(
		"idempotency-key", "", "stable caller-generated retry key",
	)
	flags.Uint64(
		"change-carrier-sat", 0, "Bitcoin value carrying the asset "+
			"change of a partial send (zero uses the operator "+
			"minimum)",
	)

	return cmd
}

// sendTaprootAsset resolves the input VTXO, then submits the asset OOR
// intent.
func sendTaprootAsset(cmd *cobra.Command, _ []string) error {
	assetRef, _ := cmd.Flags().GetString("asset-ref")
	amount, _ := cmd.Flags().GetUint64("amount")
	recipientPubkey, _ := cmd.Flags().GetString("recipient-pubkey")
	outpoint, _ := cmd.Flags().GetString("outpoint")
	idempotencyKey, _ := cmd.Flags().GetString("idempotency-key")
	changeCarrierSat, _ := cmd.Flags().GetUint64("change-carrier-sat")

	switch {
	case assetRef == "":
		return invalidArgs(fmt.Errorf("--asset-ref is required"))

	case amount == 0:
		return invalidArgs(fmt.Errorf("--amount must be positive"))

	case recipientPubkey == "":
		return invalidArgs(fmt.Errorf("--recipient-pubkey is required"))

	case idempotencyKey == "":
		return invalidArgs(fmt.Errorf("--idempotency-key is required"))
	}
	if err := validateFreeText(
		"--idempotency-key", idempotencyKey,
	); err != nil {
		return invalidArgs(err)
	}
	if outpoint != "" {
		if err := validateOutpoint(outpoint); err != nil {
			return invalidArgs(err)
		}
	}

	recipientKey, err := hex.DecodeString(recipientPubkey)
	if err != nil {
		return invalidArgs(
			fmt.Errorf("decode --recipient-pubkey: %w", err),
		)
	}

	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := rpcContext(cmd)
	defer cancel()

	input, err := selectAssetSendInput(
		ctx, client, assetRef, amount, outpoint,
	)
	if err != nil {
		return err
	}

	intent := &waverpc.TaprootAssetOORIntent{
		AssetRef:               assetRef,
		AssetAmount:            input.GetTaprootAsset().GetAmount(),
		AcknowledgeUnconfirmed: true,
		InputVtxoOutpoint:      input.GetOutpoint(),
	}

	// A partial send keeps the asset change on a minimal carrier; the
	// daemon defaults a zero value to the operator minimum and returns
	// the rest of the selected Bitcoin VTXO as plain change.
	if amount < intent.AssetAmount {
		intent.RecipientAssetAmount = amount
		intent.AssetChangeCarrierValueSat = changeCarrierSat
	}
	if amount > intent.AssetAmount {
		return invalidArgs(
			fmt.Errorf(
				"VTXO %s holds %d units, cannot send %d",
				input.GetOutpoint(), intent.AssetAmount, amount,
			),
		)
	}

	response, err := client.SendOOR(ctx, &waverpc.SendOORRequest{
		Recipients: []*waverpc.Output{{
			Destination: &waverpc.Output_Pubkey{
				Pubkey: recipientKey,
			},
			AmountSat: input.GetAmountSat(),
		}},
		IdempotencyKey: idempotencyKey,
		TaprootAsset:   intent,
	})
	if err != nil {
		return fmt.Errorf("SendOOR RPC failed: %w", err)
	}

	return printJSON(response)
}

// selectAssetSendInput resolves the asset VTXO to spend: the named
// outpoint, or the smallest live VTXO of the asset that covers the amount.
func selectAssetSendInput(ctx context.Context,
	client waverpc.DaemonServiceClient, assetRef string, amount uint64,
	outpoint string) (*waverpc.VTXO, error) {

	response, err := client.ListVTXOs(ctx, &waverpc.ListVTXOsRequest{
		StatusFilter:           waverpc.VTXOStatus_VTXO_STATUS_LIVE,
		AssetRef:               assetRef,
		ExcludeCheckpointPsbts: true,
	})
	if err != nil {
		return nil, fmt.Errorf("ListVTXOs RPC failed: %w", err)
	}

	var selected *waverpc.VTXO
	for _, v := range response.Vtxos {
		if outpoint != "" {
			if v.GetOutpoint() == outpoint {
				return v, nil
			}

			continue
		}
		if v.GetTaprootAsset().GetAmount() < amount {
			continue
		}
		if selected == nil || v.GetTaprootAsset().GetAmount() <
			selected.GetTaprootAsset().GetAmount() {

			selected = v
		}
	}
	if outpoint != "" {
		return nil, fmt.Errorf("no live VTXO %s carries asset %s",
			outpoint, assetRef)
	}
	if selected == nil {
		return nil, fmt.Errorf("no live asset VTXO covers %d "+
			"units of %s", amount, assetRef)
	}

	return selected, nil
}
