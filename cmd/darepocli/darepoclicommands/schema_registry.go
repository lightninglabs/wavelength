package darepoclicommands

// schemaParam describes a single parameter for a CLI command / RPC
// method.
type schemaParam struct {
	// Name is the flag name or proto field name.
	Name string `json:"name"`

	// Type is the parameter type (string, int64, bool, enum, etc.).
	Type string `json:"type"`

	// Description explains what the parameter does.
	Description string `json:"description,omitempty"`

	// Required indicates whether the parameter must be provided.
	Required bool `json:"required,omitempty"`

	// Values lists valid values for enum-typed parameters.
	Values []string `json:"values,omitempty"`
}

// schemaMethod describes a single CLI command / RPC method.
type schemaMethod struct {
	// Method is the dot-notation name (e.g. "vtxos.list").
	Method string `json:"method"`

	// Description is a short description of the command.
	Description string `json:"description"`

	// Params lists the accepted parameters.
	Params []schemaParam `json:"params"`

	// RequestType is the proto request message name.
	RequestType string `json:"request_type"`

	// ResponseType is the proto response message name.
	ResponseType string `json:"response_type"`

	// DryRun indicates whether the command supports --dry_run.
	DryRun bool `json:"dry_run,omitempty"`

	// JSONInput indicates the command accepts --json for raw
	// proto payloads.
	JSONInput bool `json:"json_input"`
}

// methodRegistry returns the full schema for all darepocli commands.
// This is the single source of truth for both the schema command and
// MCP tool definitions. The body is split across helper functions to
// stay under the funlen linter cap.
func methodRegistry() []schemaMethod {
	out := baseMethodRegistry()
	out = append(out, vtxoLifecycleMethodRegistry()...)
	out = append(out, sendMethodRegistry()...)
	out = append(out, receiveMethodRegistry()...)
	out = append(out, transferMethodRegistry()...)

	return out
}

// baseMethodRegistry returns the schema entries for the base
// daemon-level commands (info / wallet / boarding / list-* / fees /
// unroll). Split out of methodRegistry to keep the function under the
// funlen cap.
func baseMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:       "getinfo",
			Description:  "Display daemon status information",
			Params:       nil,
			RequestType:  "GetInfoRequest",
			ResponseType: "GetInfoResponse",
			JSONInput:    true,
		},
		{
			Method:      "wallet.create",
			Description: "Create a new wallet from a fresh seed",
			Params: []schemaParam{
				{
					Name: "wallet_password_file",
					Type: "string",
					Description: "path to file " +
						"containing wallet password",
				},
				{
					Name: "seed_passphrase",
					Type: "string",
					Description: "optional aezeed " +
						"passphrase",
				},
			},
			RequestType:  "InitWalletRequest",
			ResponseType: "InitWalletResponse",
			JSONInput:    true,
		},
		{
			Method:      "wallet.unlock",
			Description: "Unlock an existing wallet",
			Params: []schemaParam{
				{
					Name: "wallet_password_file",
					Type: "string",
					Description: "path to file " +
						"containing wallet password",
				},
			},
			RequestType:  "UnlockWalletRequest",
			ResponseType: "UnlockWalletResponse",
			JSONInput:    true,
		},
		{
			Method:       "wallet.balance",
			Description:  "Display wallet balance",
			Params:       nil,
			RequestType:  "GetBalanceRequest",
			ResponseType: "GetBalanceResponse",
			JSONInput:    true,
		},
		{
			Method:       "wallet.newaddress",
			Description:  "Generate a new boarding address",
			Params:       nil,
			RequestType:  "NewAddressRequest",
			ResponseType: "NewAddressResponse",
			JSONInput:    true,
		},
		{
			Method:      "sweep",
			Description: "Sweep expired boarding UTXOs",
			Params: []schemaParam{
				{
					Name: "outpoint",
					Type: "string[]",
					Description: "boarding UTXO " +
						"outpoint(s) to sweep " +
						"(txid:index)",
				},
				{
					Name: "broadcast",
					Type: "bool",
					Description: "broadcast aggregate " +
						"sweep and track confirmation",
				},
				{
					Name: "fee_rate_sat_per_vbyte",
					Type: "int64",
					Description: "fee rate override; " +
						"zero estimates by target",
				},
				{
					Name: "conf_target",
					Type: "uint32",
					Description: "confirmation target; " +
						"zero uses default",
				},
				{
					Name: "sweep_address",
					Type: "string",
					Description: "optional destination; " +
						"empty uses wallet address",
				},
			},
			RequestType:  "SweepBoardingUTXOsRequest",
			ResponseType: "SweepBoardingUTXOsResponse",
			JSONInput:    true,
		},
		{
			Method:      "sweep.list",
			Description: "List tracked boarding sweeps",
			Params: []schemaParam{
				{
					Name: "status",
					Type: "string",
					Description: "status filter: " +
						"pending, published, " +
						"confirmed, " +
						"external_resolved, or failed",
				},
				{
					Name: "page_size",
					Type: "uint32",
					Description: "maximum sweeps to " +
						"return; zero uses default",
				},
				{
					Name: "page_token",
					Type: "string",
					Description: "token from a previous " +
						"sweep list response",
				},
			},
			RequestType:  "ListBoardingSweepsRequest",
			ResponseType: "ListBoardingSweepsResponse",
			JSONInput:    true,
		},
		{
			Method:      "listtransactions",
			Description: "List local transaction history",
			Params: []schemaParam{
				{
					Name: "from",
					Type: "string",
					Description: "ISO 8601 lower " +
						"timestamp bound",
				},
				{
					Name: "to",
					Type: "string",
					Description: "ISO 8601 upper " +
						"timestamp bound",
				},
				{
					Name: "limit",
					Type: "uint32",
					Description: "maximum entries to " +
						"return; zero uses default",
				},
				{
					Name: "offset",
					Type: "uint32",
					Description: "number of filtered " +
						"entries to skip",
				},
				{
					Name:        "type",
					Type:        "enum",
					Description: "transaction type filter",
					Values: []string{
						"boarding",
						"round",
						"oor",
						"sweep",
					},
				},
			},
			RequestType:  "ListTransactionsRequest",
			ResponseType: "ListTransactionsResponse",
			JSONInput:    true,
		},
		{
			Method:      "oor.receive",
			Description: "Allocate a fresh receive script",
			Params: []schemaParam{
				{
					Name: "label",
					Type: "string",
					Description: "optional indexer " +
						"registration label",
				},
			},
			RequestType:  "NewReceiveScriptRequest",
			ResponseType: "NewReceiveScriptResponse",
			JSONInput:    true,
		},
	}
}

// receiveMethodRegistry returns the generic receive-target schema entries.
func receiveMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:      "receive",
			Description: "Allocate a fresh Ark receive target",
			Params: []schemaParam{
				{
					Name: "label",
					Type: "string",
					Description: "optional indexer " +
						"registration label",
				},
			},
			RequestType:  "NewReceiveScriptRequest",
			ResponseType: "NewReceiveScriptResponse",
			JSONInput:    true,
		},
		{
			Method:       "receive.list",
			Description:  "List registered Ark receive targets",
			Params:       nil,
			RequestType:  "ListReceiveScriptsRequest",
			ResponseType: "ListReceiveScriptsResponse",
			JSONInput:    true,
		},
	}
}

// transferMethodRegistry returns the transfer status schema entries.
func transferMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:      "transfers.list",
			Description: "List in-round and OOR transfer statuses",
			Params: []schemaParam{
				{
					Name: "mode",
					Type: "enum",
					Description: "mode filter: all, " +
						"inround, or oor",
					Values: []string{
						"all",
						"inround",
						"oor",
					},
				},
				{
					Name: "direction",
					Type: "enum",
					Description: "direction filter: all, " +
						"outgoing, or incoming; " +
						"unknown pending rows " +
						"are always shown",
					Values: []string{
						"all",
						"outgoing",
						"incoming",
					},
				},
				{
					Name: "status",
					Type: "enum",
					Description: "status filter: all, " +
						"pending, completed, or failed",
					Values: []string{
						"all",
						"pending",
						"completed",
						"failed",
					},
				},
				{
					Name: "limit",
					Type: "uint32",
					Description: "max rows to return; " +
						"zero uses default",
				},
				{
					Name: "offset",
					Type: "uint32",
					Description: "rows to skip after " +
						"filtering and sorting",
				},
			},
			RequestType:  "ListTransfersRequest",
			ResponseType: "ListTransfersResponse",
			JSONInput:    true,
		},
	}
}

// vtxoLifecycleMethodRegistry returns the VTXO lifecycle commands
// (list / refresh / leave). Split out of methodRegistry so the parent
// stays under the funlen cap as new entries land.
func vtxoLifecycleMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:      "vtxos.list",
			Description: "List VTXOs with optional filters",
			Params: []schemaParam{
				{
					Name:        "status",
					Type:        "enum",
					Description: "filter by VTXO status",
					Values: []string{
						"live",
						"refresh_requested",
						"forfeiting",
						"forfeited",
						"spent",
						"expiring",
						"failed",
					},
				},
				{
					Name:        "min_amount",
					Type:        "int64",
					Description: "minimum amount in sats",
				},
				{
					Name: "fields",
					Type: "string",
					Description: "comma-separated field " +
						"names to include",
				},
				{
					Name: "ndjson",
					Type: "bool",
					Description: "emit one JSON object " +
						"per VTXO (newline-delimited)",
				},
			},
			RequestType:  "ListVTXOsRequest",
			ResponseType: "ListVTXOsResponse",
			JSONInput:    true,
		},
		{
			Method:      "vtxos.refresh",
			Description: "Queue VTXOs for refresh in next round",
			Params: []schemaParam{
				{
					Name: "outpoint",
					Type: "string[]",
					Description: "VTXO outpoint(s) to " +
						"refresh (txid:index)",
				},
				{
					Name:        "all",
					Type:        "bool",
					Description: "refresh all live VTXOs",
				},
			},
			RequestType:  "RefreshVTXOsRequest",
			ResponseType: "RefreshVTXOsResponse",
			DryRun:       true,
			JSONInput:    true,
		},
		{
			Method: "vtxos.leave",
			Description: "Queue VTXOs for cooperative leave " +
				"(offboard)",
			Params: []schemaParam{
				{
					Name: "outpoint",
					Type: "string[]",
					Description: "VTXO outpoint(s) to " +
						"leave (txid:index)",
				},
				{
					Name:        "all",
					Type:        "bool",
					Description: "leave all live VTXOs",
				},
				{
					Name: "address",
					Type: "string",
					Description: "default on-chain " +
						"destination address",
				},
				{
					Name: "pk_script",
					Type: "string",
					Description: "default destination " +
						"pk_script (hex)",
				},
				{
					Name: "destination",
					Type: "map[string]string",
					Description: "per-outpoint override: " +
						"outpoint=addr or " +
						"outpoint=script:<hex>",
				},
				{
					Name: "yes",
					Type: "bool",
					Description: "skip --all " +
						"interactive confirmation",
				},
			},
			RequestType:  "LeaveVTXOsRequest",
			ResponseType: "LeaveVTXOsResponse",
			DryRun:       true,
			JSONInput:    true,
		},
	}
}

// sendMethodRegistry returns the send.* schema entries (in-round and
// OOR). Split out of methodRegistry so the parent stays under the
// funlen cap as new entries land.
func sendMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:      "send.inround",
			Description: "Send via in-round refresh",
			Params: []schemaParam{
				{
					Name:        "to",
					Type:        "string[]",
					Required:    true,
					Description: "recipient address(es)",
				},
				{
					Name:     "amount",
					Type:     "int64[]",
					Required: true,
					Description: "amount(s) in sats " +
						"(one per --to)",
				},
			},
			RequestType:  "SendVTXORequest",
			ResponseType: "SendVTXOResponse",
			DryRun:       true,
			JSONInput:    true,
		},
		{
			Method:      "send.oor",
			Description: "Send via out-of-round transfer",
			Params: []schemaParam{
				{
					Name: "to",
					Type: "string",
					Description: "recipient address " +
						"(exactly one of to or pubkey)",
				},
				{
					Name: "pubkey",
					Type: "string",
					Description: "recipient 32-byte " +
						"x-only pubkey hex (exactly " +
						"one of to or pubkey)",
				},
				{
					Name:        "amount",
					Type:        "int64",
					Required:    true,
					Description: "amount in sats",
				},
				{
					Name: "idempotency_key",
					Type: "string",
					Description: "stable caller intent " +
						"key for retry-safe OOR sends",
				},
			},
			RequestType:  "SendOORRequest",
			ResponseType: "SendOORResponse",
			DryRun:       true,
			JSONInput:    true,
		},
	}
}
