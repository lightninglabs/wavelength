package darepoclicommands

import (
	"testing"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/stretchr/testify/require"
)

// TestParseTransferFilters verifies public CLI filter strings map to the
// daemon RPC enums.
func TestParseTransferFilters(t *testing.T) {
	t.Parallel()

	unspecifiedMode := daemonrpc.TransferMode_TRANSFER_MODE_UNSPECIFIED
	unspecifiedDirection := daemonrpc.
		TransferDirection_TRANSFER_DIRECTION_UNSPECIFIED
	unspecifiedStatus := daemonrpc.
		TransferStatus_TRANSFER_STATUS_UNSPECIFIED

	mode, err := parseTransferModeFilter("")
	require.NoError(t, err)
	require.Equal(t, unspecifiedMode, mode)

	mode, err = parseTransferModeFilter("all")
	require.NoError(t, err)
	require.Equal(t, unspecifiedMode, mode)

	direction, err := parseTransferDirectionFilter("")
	require.NoError(t, err)
	require.Equal(t, unspecifiedDirection, direction)

	direction, err = parseTransferDirectionFilter("all")
	require.NoError(t, err)
	require.Equal(t, unspecifiedDirection, direction)

	status, err := parseTransferStatusFilter("")
	require.NoError(t, err)
	require.Equal(t, unspecifiedStatus, status)

	status, err = parseTransferStatusFilter("all")
	require.NoError(t, err)
	require.Equal(t, unspecifiedStatus, status)

	mode, err = parseTransferModeFilter("inround")
	require.NoError(t, err)
	require.Equal(t, daemonrpc.TransferMode_TRANSFER_MODE_INROUND, mode)

	mode, err = parseTransferModeFilter("oor")
	require.NoError(t, err)
	require.Equal(t, daemonrpc.TransferMode_TRANSFER_MODE_OOR, mode)

	direction, err = parseTransferDirectionFilter("incoming")
	require.NoError(t, err)
	require.Equal(
		t, daemonrpc.TransferDirection_TRANSFER_DIRECTION_INCOMING,
		direction,
	)

	direction, err = parseTransferDirectionFilter("outgoing")
	require.NoError(t, err)
	require.Equal(
		t, daemonrpc.TransferDirection_TRANSFER_DIRECTION_OUTGOING,
		direction,
	)

	status, err = parseTransferStatusFilter("completed")
	require.NoError(t, err)
	require.Equal(
		t, daemonrpc.TransferStatus_TRANSFER_STATUS_COMPLETED, status,
	)

	status, err = parseTransferStatusFilter("pending")
	require.NoError(t, err)
	require.Equal(
		t, daemonrpc.TransferStatus_TRANSFER_STATUS_PENDING, status,
	)

	status, err = parseTransferStatusFilter("failed")
	require.NoError(t, err)
	require.Equal(
		t, daemonrpc.TransferStatus_TRANSFER_STATUS_FAILED, status,
	)
}

// TestParseTransferFiltersRejectUnknown verifies invalid filter values are
// rejected before the CLI reaches the daemon.
func TestParseTransferFiltersRejectUnknown(t *testing.T) {
	t.Parallel()

	_, err := parseTransferModeFilter("boarding")
	require.ErrorContains(t, err, "unknown transfer mode")

	_, err = parseTransferDirectionFilter("sideways")
	require.ErrorContains(t, err, "unknown transfer direction")

	_, err = parseTransferStatusFilter("stuck")
	require.ErrorContains(t, err, "unknown transfer status")
}

// TestMethodRegistryTransfersSchema verifies the schema advertises the new
// transfer list filters.
func TestMethodRegistryTransfersSchema(t *testing.T) {
	t.Parallel()

	method := findSchemaMethod(t, "transfers.list")
	require.Equal(t, "ListTransfersRequest", method.RequestType)
	require.Equal(t, "ListTransfersResponse", method.ResponseType)

	params := make(map[string]schemaParam, len(method.Params))
	for _, param := range method.Params {
		params[param.Name] = param
	}

	require.Equal(
		t, []string{"all", "inround", "oor"}, params["mode"].Values,
	)
	require.Equal(
		t, "mode filter: all, inround, or oor",
		params["mode"].Description,
	)
	require.Equal(
		t, []string{"all", "outgoing", "incoming"},
		params["direction"].Values,
	)
	require.Equal(
		t, "direction filter: all, outgoing, or incoming; unknown "+
			"pending rows are always shown",
		params["direction"].Description,
	)
	require.Equal(
		t, []string{"all", "pending", "completed", "failed"},
		params["status"].Values,
	)
	require.Equal(
		t, "status filter: all, pending, completed, or failed",
		params["status"].Description,
	)
	require.Equal(t, "uint32", params["limit"].Type)
	require.Equal(
		t, "max rows to return; zero uses default",
		params["limit"].Description,
	)
	require.Equal(t, "uint32", params["offset"].Type)
	require.Equal(
		t, "rows to skip after filtering and sorting",
		params["offset"].Description,
	)
}

// TestMethodRegistryReceiveSchema verifies the schema advertises generic
// receive allocation and listing.
func TestMethodRegistryReceiveSchema(t *testing.T) {
	t.Parallel()

	receive := findSchemaMethod(t, "receive")
	require.Equal(t, "NewReceiveScriptRequest", receive.RequestType)
	require.Equal(t, "NewReceiveScriptResponse", receive.ResponseType)

	list := findSchemaMethod(t, "receive.list")
	require.Equal(t, "ListReceiveScriptsRequest", list.RequestType)
	require.Equal(t, "ListReceiveScriptsResponse", list.ResponseType)
}
