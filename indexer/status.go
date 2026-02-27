package indexer

import "github.com/lightninglabs/darepo-client/arkrpc"

const (
	storeVTXOStatusPending   = "pending"
	storeVTXOStatusLive      = "live"
	storeVTXOStatusInFlight  = "in_flight"
	storeVTXOStatusForfeited = "forfeited"
	storeVTXOStatusSpent     = "spent"

	storeVTXOEventTypeCreated       = "created"
	storeVTXOEventTypeStatusChanged = "status_changed"
	storeVTXOEventTypeTerminated    = "terminated"
)

// VTXOStatusFromStore maps a stored VTXO status string into the indexer RPC
// status enum.
func VTXOStatusFromStore(statusStr string) arkrpc.VTXOStatus {
	switch statusStr {
	case storeVTXOStatusPending:
		return arkrpc.VTXOStatus_VTXO_STATUS_UNCONFIRMED

	case storeVTXOStatusLive:
		return arkrpc.VTXOStatus_VTXO_STATUS_LIVE

	case storeVTXOStatusInFlight:
		return arkrpc.VTXOStatus_VTXO_STATUS_FORFEITING

	case storeVTXOStatusForfeited:
		return arkrpc.VTXOStatus_VTXO_STATUS_FORFEITED

	case storeVTXOStatusSpent:
		return arkrpc.VTXOStatus_VTXO_STATUS_SPENT

	default:
		return arkrpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED
	}
}

func vtxoEventTypeToStore(eventType arkrpc.VTXOEventType) string {
	switch eventType {
	case arkrpc.VTXOEventType_VTXO_EVENT_TYPE_CREATED:
		return storeVTXOEventTypeCreated

	case arkrpc.VTXOEventType_VTXO_EVENT_TYPE_STATUS_CHANGED:
		return storeVTXOEventTypeStatusChanged

	case arkrpc.VTXOEventType_VTXO_EVENT_TYPE_TERMINATED:
		return storeVTXOEventTypeTerminated

	default:
		return storeVTXOEventTypeStatusChanged
	}
}

func vtxoEventTypeFromStore(eventType string) arkrpc.VTXOEventType {
	switch eventType {
	case storeVTXOEventTypeCreated:
		return arkrpc.VTXOEventType_VTXO_EVENT_TYPE_CREATED

	case storeVTXOEventTypeStatusChanged:
		return arkrpc.VTXOEventType_VTXO_EVENT_TYPE_STATUS_CHANGED

	case storeVTXOEventTypeTerminated:
		return arkrpc.VTXOEventType_VTXO_EVENT_TYPE_TERMINATED

	default:
		return arkrpc.VTXOEventType_VTXO_EVENT_TYPE_UNSPECIFIED
	}
}
