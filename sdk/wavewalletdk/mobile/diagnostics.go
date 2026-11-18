//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// DiagnosticRead exposes a finite read-only inspection surface, not arbitrary
// RPC dispatch. Requests select View, Outpoint, RoundID, History and PageToken.
// Calls inherit the wallet lifecycle and a ten-second read deadline.
func DiagnosticRead(reqJSON []byte) ([]byte, error) {
	client, parent, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := readContext(parent)
	defer cancel()
	var req struct {
		View      string
		Outpoint  string
		RoundID   string
		History   bool
		PageToken string
	}
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}
	rpc := client.ArkRPC()
	var response proto.Message
	var oorPage *diagnosticOORPage
	switch req.View {
	case "vtxos":
		statuses := []waverpc.VTXOStatus{
			waverpc.VTXOStatus_VTXO_STATUS_LIVE,
			waverpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT,
			waverpc.VTXOStatus_VTXO_STATUS_FORFEITING,
			waverpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT,
			waverpc.VTXOStatus_VTXO_STATUS_FAILED,
			waverpc.VTXOStatus_VTXO_STATUS_SPENDING,
			waverpc.VTXOStatus_VTXO_STATUS_PENDING_ROUND,
			waverpc.VTXOStatus_VTXO_STATUS_EXPIRED,
		}
		if req.History {
			statuses = append(
				statuses, waverpc.VTXOStatus_VTXO_STATUS_SPENT,
				waverpc.VTXOStatus_VTXO_STATUS_FORFEITED,
			)
		}
		response, err = diagnosticVTXOs(ctx, rpc, statuses)

	case "vtxo":
		target := &waverpc.GetVTXOExpiryInfoRequest_Outpoint{
			Outpoint: req.Outpoint,
		}
		response, err = rpc.GetVTXOExpiryInfo(
			ctx, &waverpc.GetVTXOExpiryInfoRequest{
				Target: target,
			},
		)

	case "anchors":
		anchors, readErr := client.VTXOAnchors(ctx, req.Outpoint)
		if readErr != nil {
			return nil, readErr
		}

		return marshal(anchors)

	case "rounds":
		response, err = rpc.ListRounds(ctx, &waverpc.ListRoundsRequest{
			PageSize: 50, PageToken: req.PageToken,
		})

	case "round":
		response, err = rpc.GetRound(ctx, &waverpc.GetRoundRequest{
			RoundId: req.RoundID,
		})

	case "info":
		response, err = rpc.GetInfo(ctx, &waverpc.GetInfoRequest{})

	case "balance":
		response, err = rpc.GetBalance(
			ctx, &waverpc.GetBalanceRequest{},
		)

	case "oor":
		sessions, readErr := rpc.ListOORSessions(
			ctx, &waverpc.ListOORSessionsRequest{
				PageToken: req.PageToken,
			},
		)
		if readErr != nil {
			return nil, readErr
		}
		oorPage = diagnosticOORRecords(ctx, client, sessions)
		response = sessions

	case "swaps":
		swaps, readErr := client.SwapRPC().ListSwaps(
			ctx, &swapclientrpc.ListSwapsRequest{},
		)
		if readErr != nil {
			return nil, readErr
		}
		for i, swap := range swaps.GetSwaps() {
			swaps.Swaps[i] = diagnosticSwap(swap)
		}
		response = swaps

	case "credits":
		response, err = client.SwapRPC().ListCredits(
			ctx, &swapclientrpc.ListCreditsRequest{Limit: 50},
		)

	default:
		return nil, fmt.Errorf("unknown diagnostic view")
	}
	if err != nil {
		return nil, err
	}

	return marshalDiagnostic(response, oorPage)
}

// marshalDiagnostic preserves protobuf integer strings and optional OOR values.
func marshalDiagnostic(response proto.Message,
	oorPage *diagnosticOORPage) ([]byte, error) {

	data, err := (protojson.MarshalOptions{
		UseProtoNames: true, EmitUnpopulated: true,
	}).Marshal(response)
	if err != nil {
		return nil, err
	}
	// Normalize whitespace for a stable copyable raw-response presentation.
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}

	if oorPage != nil {
		if oorPage.PackageEvidenceUnavailable ||
			oorPage.SwapEvidenceUnavailable {

			pageInfo, err := json.Marshal(oorPage)
			if err != nil {
				return nil, err
			}
			object["diagnostic"] = pageInfo
		}
		var records []map[string]json.RawMessage
		if err := json.Unmarshal(
			object["sessions"], &records,
		); err != nil {
			return nil, err
		}
		for _, record := range records {
			var id string
			if err := json.Unmarshal(
				record["session_id"], &id,
			); err != nil {
				return nil, err
			}
			if item, ok := oorPage.records[id]; ok {
				if item.amount != nil {
					record["amount_sat"], err = json.Marshal(
						fmt.Sprint(*item.amount),
					)
					if err != nil {
						return nil, err
					}
				}
				if item.details != nil {
					record["diagnostic"], err = json.Marshal(
						item.details,
					)
					if err != nil {
						return nil, err
					}
				}
			}
		}
		object["sessions"], err = json.Marshal(records)
		if err != nil {
			return nil, err
		}
	}

	return json.Marshal(object)
}

// diagnosticVTXOs enriches lightweight inventory with authoritative expiry.
// ListVTXOs omits ancestry and cannot supply the policy's tree-depth threshold.
// Each detail read stays within the caller's one shared lifecycle deadline.
func diagnosticVTXOs(ctx context.Context, rpc waverpc.DaemonServiceClient,
	statuses []waverpc.VTXOStatus) (*waverpc.ListVTXOsResponse, error) {

	inventory, err := rpc.ListVTXOs(ctx, &waverpc.ListVTXOsRequest{
		Statuses: statuses, ExcludeCheckpointPsbts: true,
	})
	if err != nil {
		return nil, err
	}
	var height int32
	for _, item := range inventory.GetVtxos() {
		if item.GetOutpoint() == "" {
			continue
		}
		target := &waverpc.GetVTXOExpiryInfoRequest_Outpoint{
			Outpoint: item.GetOutpoint(),
		}
		detail, err := rpc.GetVTXOExpiryInfo(
			ctx, &waverpc.GetVTXOExpiryInfoRequest{
				Target:        target,
				CurrentHeight: height,
			},
		)
		if err != nil {
			return nil, err
		}
		if detail.GetFound() {
			item.ExpiryInfo = detail.GetExpiryInfo()
			if item.GetExpiryInfo().GetCurrentHeight() > 0 {
				height = item.GetExpiryInfo().GetCurrentHeight()
			}
		}
	}

	return inventory, nil
}
