package indexer

const (
	// defaultRecipientEventLimit is used when a client omits a limit.
	defaultRecipientEventLimit = 50

	// defaultVTXOLimit is used when a client omits a VTXO list limit.
	defaultVTXOLimit = 100

	// defaultVTXOEventLimit is used when a client omits an event list
	// limit.
	defaultVTXOEventLimit = 200

	// maxQueryLimit is the hard upper bound on client-supplied page
	// sizes across all paginated query RPCs. This prevents a single
	// request from materializing an unbounded result set.
	maxQueryLimit = 1000

	// maxScriptsPerRequest is the hard upper bound on the number
	// of script scopes a single request can contain. Each script
	// requires TLV parsing, schnorr verification, and DB queries.
	maxScriptsPerRequest = 100

	// defaultOperatorProtocolVersion is used for outbound EVENT
	// envelopes when the operator does not override protocol version.
	defaultOperatorProtocolVersion = 1

	// indexerServiceName is the protobuf service name used in mailbox meta.
	indexerServiceName = "arkrpc.IndexerService"

	// responseMsgIDPrefix prefixes mailbox response envelopes.
	responseMsgIDPrefix = "resp-"

	// purposeListVTXOsByScripts is the canonical purpose string used in
	// script-scope proofs for ListVTXOsByScripts.
	purposeListVTXOsByScripts = "list_vtxos_by_scripts"

	// purposeGetSubtreeByScripts is the canonical purpose string used in
	// script-scope proofs for GetSubtreeByScripts.
	purposeGetSubtreeByScripts = "get_subtree_by_scripts"

	// purposeListVTXOEventsByScripts is the canonical purpose string used
	// in script-scope proofs for ListVTXOEventsByScripts.
	purposeListVTXOEventsByScripts = "list_vtxo_events_by_scripts"

	// purposeOORRecipientEvents is the canonical purpose string used in
	// script-scope proofs for ListOORRecipientEventsByScript.
	purposeOORRecipientEvents = "list_oor_recipient_events_by_script"

	// purposeRegisterReceiveScript is the canonical purpose string used in
	// receive-script proofs for RegisterReceiveScript.
	purposeRegisterReceiveScript = "register_receive_script"

	// purposeUnregisterReceiveScript is the canonical purpose
	// string used in receive-script proofs for
	// UnregisterReceiveScript.
	purposeUnregisterReceiveScript = "unregister_receive_script"
)
