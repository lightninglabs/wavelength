package indexer

const (
	// defaultRecipientEventLimit is used when a client omits a limit.
	defaultRecipientEventLimit = 50

	// defaultVTXOLimit is used when a client omits a VTXO list limit.
	defaultVTXOLimit = 100

	// defaultVTXOEventLimit is used when a client omits an event list
	// limit.
	defaultVTXOEventLimit = 200

	// defaultOperatorProtocolVersion is used for outbound EVENT
	// envelopes when the operator does not override protocol version.
	defaultOperatorProtocolVersion = 1

	// defaultOperatorPullMaxEnvelopes is the pull batch size used
	// by the draft operator loop.
	defaultOperatorPullMaxEnvelopes = 50

	// indexerServiceName is the protobuf service name used in mailbox meta.
	indexerServiceName = "arkrpc.IndexerService"

	// responseMsgIDPrefix prefixes mailbox response envelopes.
	responseMsgIDPrefix = "resp-"

	// eventMsgIDPrefix prefixes mailbox event envelopes.
	eventMsgIDPrefix = "evt-"

	// purposeListVTXOsByScripts is the canonical purpose string used in
	// script-scope proofs for ListVTXOsByScripts.
	purposeListVTXOsByScripts = "list_vtxos_by_scripts"

	// purposeGetSubtreeByScripts is the canonical purpose string used in
	// script-scope proofs for GetSubtreeByScripts.
	purposeGetSubtreeByScripts = "get_subtree_by_scripts"

	// purposeListVTXOEventsByScripts is the canonical purpose string used
	// in script-scope proofs for ListVTXOEventsByScripts.
	purposeListVTXOEventsByScripts = "list_vtxo_events_by_scripts"
)
