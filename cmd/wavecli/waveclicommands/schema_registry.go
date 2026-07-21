package waveclicommands

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

	// DryRunResponseType is the proto response message name emitted in
	// dry-run mode when it differs from ResponseType.
	DryRunResponseType string `json:"dry_run_response_type,omitempty"`

	// DryRun indicates whether the command supports --dry-run.
	// Top-level wallet verbs use --dry-run (CLI-only validation,
	// exits 0); ark.* commands use --dry-run that reaches the
	// daemon's dry_run RPC field.
	DryRun bool `json:"dry_run,omitempty"`

	// JSONInput indicates the command accepts --request-json for raw proto
	// payloads.
	JSONInput bool `json:"request_json_input"`

	// MCPTool indicates the method is exposed as an MCP tool with
	// the same name. False means it is CLI-only (create / unlock
	// are intentionally CLI-only because they handle secret
	// material).
	MCPTool bool `json:"mcp_tool,omitempty"`
}

// methodRegistry returns the full schema for all wavecli commands.
// The body is split across helper functions to stay under the funlen
// linter cap. The top-level wallet verbs (create, unlock, send, recv,
// activity, balance, exit) are the day-to-day surface; advanced commands
// live under the `ark.*` namespace.
func methodRegistry() []schemaMethod {
	out := walletAdminMethodRegistry()
	out = append(out, walletPaymentMethodRegistry()...)
	out = append(out, walletQueryMethodRegistry()...)
	out = append(out, arkBaseMethodRegistry()...)
	out = append(out, arkVTXOMethodRegistry()...)
	out = append(out, arkSendMethodRegistry()...)
	out = append(out, arkObservableMethodRegistry()...)

	return out
}

// listOutputParams returns the standard agent-CLI output-shape
// modifier params (--fields, --ndjson) for list-shaped commands. The
// helper keeps every list entry's schema description in sync with
// addListOutputFlags so an agent inspecting the schema sees exactly
// the flags the CLI exposes.
func listOutputParams() []schemaParam {
	return []schemaParam{
		{
			Name: "fields",
			Type: "string",
			Description: "comma-separated field names to " +
				"include (response filtered before " +
				"printing)",
		},
		{
			Name: "ndjson",
			Type: "bool",
			Description: "emit one JSON object per row " +
				"(newline-delimited); pairs with " +
				"--fields to shrink each line",
		},
	}
}

// walletAdminMethodRegistry returns the admin-shape wallet verbs
// (create, unlock) plus daemon introspection (getinfo).
func walletAdminMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:      "create",
			Description: "Create a new wallet from a fresh seed",
			Params: []schemaParam{
				{
					Name: "wallet-password-file",
					Type: "string",
					Description: "path to file " +
						"containing wallet password",
				},
				{
					Name: "seed-passphrase-file",
					Type: "string",
					Description: "path to file " +
						"containing optional aezeed " +
						"passphrase",
				},
				{
					Name: "print-mnemonic-json",
					Type: "bool",
					Description: "include the mnemonic " +
						"in the JSON response on " +
						"stdout (default: stderr only)",
				},
			},
			RequestType:  "CreateRequest",
			ResponseType: "CreateResponse",
			JSONInput:    false,
		},
		{
			Method:      "unlock",
			Description: "Unlock an existing wallet",
			Params: []schemaParam{
				{
					Name: "wallet-password-file",
					Type: "string",
					Description: "path to file " +
						"containing wallet password",
				},
			},
			RequestType:  "UnlockRequest",
			ResponseType: "UnlockResponse",
			JSONInput:    false,
		},
		{
			Method:       "getinfo",
			Description:  "Display daemon status information",
			Params:       nil,
			RequestType:  "GetInfoRequest",
			ResponseType: "GetInfoResponse",
			JSONInput:    true,
		},
	}
}

// walletPaymentMethodRegistry returns the payment-shape wallet verbs
// (send, recv).
func walletPaymentMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:      "send",
			Description: "Send a payment (offchain or onchain)",
			Params: []schemaParam{
				{
					Name: "offchain",
					Type: "bool",
					Description: "force offchain " +
						"(BOLT-11 invoice) " +
						"dispatch (default)",
				},
				{
					Name: "onchain",
					Type: "bool",
					Description: "force onchain " +
						"(cooperative leave) dispatch",
				},
				{
					Name: "amt",
					Type: "uint64",
					Description: "amount in satoshis " +
						"(required for onchain " +
						"unless --sweep-all)",
				},
				{
					Name: "max-fee",
					Type: "uint64",
					Description: "max fee in satoshis; " +
						"zero uses daemon defaults",
				},
				{
					Name:        "note",
					Type:        "string",
					Description: "caller-supplied label",
				},
				{
					Name: "sweep-all",
					Type: "bool",
					Description: "onchain only: drain " +
						"every live VTXO",
				},
				{
					Name: "dry-run",
					Type: "bool",
					Description: "prepare and print the " +
						"proto-JSON preview without " +
						"dispatching",
				},
				{
					Name: "force",
					Type: "bool",
					Description: "skip interactive " +
						"confirmation",
				},
				{
					Name:        "yes",
					Type:        "bool",
					Description: "alias for force",
				},
			},
			RequestType:        "PrepareSendRequest",
			ResponseType:       "SendResponse",
			DryRunResponseType: "PrepareSendResponse",
			DryRun:             true,
			JSONInput:          false,
			MCPTool:            true,
		},
		{
			Method:      "recv",
			Description: "Receive a payment (offchain or onchain)",
			Params: []schemaParam{
				{
					Name: "offchain",
					Type: "bool",
					Description: "force offchain " +
						"(invoice) recv (default)",
				},
				{
					Name: "onchain",
					Type: "bool",
					Description: "force onchain " +
						"(boarding address) recv",
				},
				{
					Name: "amt",
					Type: "uint64",
					Description: "amount in satoshis " +
						"(required for --offchain)",
				},
				{
					Name:        "memo",
					Type:        "string",
					Description: "optional invoice memo",
				},
				{
					Name: "amt-hint",
					Type: "uint64",
					Description: "optional expected " +
						"deposit amount (--onchain)",
				},
			},
			RequestType:  "RecvRequest",
			ResponseType: "RecvResponse",
			JSONInput:    false,
			MCPTool:      true,
		},
	}
}

// walletQueryMethodRegistry returns the wallet query verbs (activity,
// balance, exit, exit.status, activity.inspect).
func walletQueryMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:      "activity",
			Description: "Show wallet activity",
			Params: []schemaParam{
				{
					Name:        "pending",
					Type:        "bool",
					Description: "only in-flight entries",
				},
				{
					Name:        "kind",
					Type:        "string[]",
					Description: "kind filter",
				},
				{
					Name: "limit",
					Type: "uint32",
					Description: "page size; 0 uses " +
						"daemon default",
				},
				{
					Name:        "offset",
					Type:        "uint32",
					Description: "pagination offset",
				},
				{
					Name:        "format",
					Type:        "enum",
					Description: "format; default table",
					Values: []string{
						"table", "expanded",
						"x", "json",
					},
				},
			},
			RequestType:  "ListRequest",
			ResponseType: "ListResponse",
			JSONInput:    false,
			MCPTool:      true,
		},
		{
			Method:       "balance",
			Description:  "Display wallet balance",
			Params:       nil,
			RequestType:  "BalanceRequest",
			ResponseType: "BalanceResponse",
			JSONInput:    false,
			MCPTool:      true,
		},
		{
			Method:      "exit",
			Description: "Trigger a unilateral exit for a VTXO",
			Params: []schemaParam{
				{
					Name:     "outpoint",
					Type:     "string",
					Required: true,
					Description: "VTXO outpoint " +
						"(txid:vout)",
				},
				{
					Name: "dry-run",
					Type: "bool",
					Description: "CLI-side validation " +
						"only; print the proto-JSON " +
						"preview and exit 0 without " +
						"dispatching",
				},
			},
			RequestType:  "ExitRequest",
			ResponseType: "ExitResponse",
			DryRun:       true,
			JSONInput:    false,
			MCPTool:      true,
		},
		{
			Method:      "exit.status",
			Description: "Query the status of an exit job",
			Params: []schemaParam{
				{
					Name:     "outpoint",
					Type:     "string",
					Required: true,
					Description: "VTXO outpoint " +
						"(txid:vout)",
				},
			},
			RequestType:  "ExitStatusRequest",
			ResponseType: "ExitStatusResponse",
			JSONInput:    false,
			MCPTool:      true,
		},
		{
			Method: "exit.summary",
			Description: "Summarize all in-progress exits and " +
				"their totals",
			Params:       nil,
			RequestType:  "ExitSummaryRequest",
			ResponseType: "ExitSummaryResponse",
			JSONInput:    false,
			MCPTool:      true,
		},
		{
			Method: "exit.plan",
			Description: "Preview backing-wallet funding " +
				"readiness for an exit",
			Params: []schemaParam{
				{
					Name:     "outpoint",
					Type:     "string[]",
					Required: true,
					Description: "VTXO outpoint " +
						"(txid:vout); repeatable",
				},
			},
			RequestType:  "GetExitPlanRequest",
			ResponseType: "GetExitPlanResponse",
			JSONInput:    false,
			MCPTool:      true,
		},
		{
			Method: "wallet-sweep",
			Description: "Preview or broadcast a sweep of the " +
				"backing wallet to a destination address",
			Params: []schemaParam{
				{
					Name:     "destination",
					Type:     "string",
					Required: true,
					Description: "on-chain destination " +
						"address",
				},
				{
					Name: "broadcast",
					Type: "bool",
					Description: "publish the sweep; " +
						"omitted means preview only",
				},
				{
					Name: "fee-rate",
					Type: "int64",
					Description: "explicit fee rate in " +
						"sat/vByte; 0 estimates from " +
						"the chain backend",
				},
				{
					Name: "conf-target",
					Type: "uint32",
					Description: "confirmation target " +
						"for fee estimation",
				},
			},
			RequestType:  "SweepWalletRequest",
			ResponseType: "SweepWalletResponse",
			JSONInput:    false,
			MCPTool:      true,
		},
		{
			Method: "activity.inspect",
			Description: "Inspect technical swap, VTXO, and " +
				"ledger details for one activity entry",
			Params: []schemaParam{
				{
					Name:        "id",
					Type:        "string",
					Required:    true,
					Description: "WalletEntry id",
				},
				{
					Name: "ledger-limit",
					Type: "uint32",
					Description: "maximum ledger rows to " +
						"scan; 0 uses daemon maximum",
				},
				{
					Name:        "format",
					Type:        "enum",
					Description: "output format",
					Values: []string{
						"expanded", "x", "json",
					},
				},
			},
			RequestType:  "InspectActivityRequest",
			ResponseType: "InspectActivityResponse",
			JSONInput:    false,
		},
	}
}

// arkBaseMethodRegistry returns the schema for the `ark.*` advanced
// subtree's non-VTXO, non-send entries: sweep, listtransactions, oor
// receive, board, fees.
func arkBaseMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:      "ark.sweep",
			Description: "Sweep expired boarding UTXOs (advanced)",
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
			Method:      "ark.sweep.list",
			Description: "List tracked boarding sweeps (advanced)",
			Params: append([]schemaParam{
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
			}, listOutputParams()...),
			RequestType:  "ListBoardingSweepsRequest",
			ResponseType: "ListBoardingSweepsResponse",
			JSONInput:    true,
		},
		{
			Method:      "ark.listtransactions",
			Description: "Raw paginated transaction history",
			Params: append([]schemaParam{
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
			}, listOutputParams()...),
			RequestType:  "ListTransactionsRequest",
			ResponseType: "ListTransactionsResponse",
			JSONInput:    true,
		},
		{
			Method:      "ark.oor.receive",
			Description: "Allocate a receive script (advanced)",
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

// arkVTXOMethodRegistry returns the VTXO lifecycle commands (list /
// refresh / leave) under the `ark.*` namespace. Split out of
// methodRegistry so the parent stays under the funlen cap as new
// entries land.
func arkVTXOMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:      "ark.vtxos.list",
			Description: "List VTXOs with optional filters",
			Params: append([]schemaParam{
				{
					Name:        "status",
					Type:        "enum",
					Description: "filter by VTXO status",
					Values: []string{
						"live",
						"pending_forfeit",
						"forfeiting",
						"forfeited",
						"spent",
						"unilateral_exit",
						"failed",
						"spending",
					},
				},
				{
					Name:        "min-amount",
					Type:        "int64",
					Description: "minimum amount in sats",
				},
			}, listOutputParams()...),
			RequestType:  "ListVTXOsRequest",
			ResponseType: "ListVTXOsResponse",
			JSONInput:    true,
		},
		{
			Method: "ark.vtxos.refresh",
			Description: "Queue VTXOs for refresh and join the " +
				"next round (auto-joins unless --no-join). " +
				"Charged an operator fee at seal time; " +
				"--dry-run previews an itemized estimate, " +
				"and a real refresh requires --yes on " +
				"non-interactive stdin",
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
				{
					Name: "yes",
					Type: "bool",
					Description: "skip the interactive " +
						"fee confirmation",
				},
				{
					Name: "no-join",
					Type: "bool",
					Description: "skip the implicit " +
						"`ark rounds join` follow-up",
				},
			},
			RequestType:  "RefreshVTXOsRequest",
			ResponseType: "RefreshVTXOsResponse",
			DryRun:       true,
			JSONInput:    true,
		},
		{
			Method: "ark.vtxos.leave",
			Description: "Queue VTXOs for cooperative leave " +
				"(offboard) and join the next round " +
				"(auto-joins unless --no-join)",
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
					Name: "pk-script",
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
				{
					Name: "no-join",
					Type: "bool",
					Description: "skip the implicit " +
						"`ark rounds join` follow-up",
				},
			},
			RequestType:  "LeaveVTXOsRequest",
			ResponseType: "LeaveVTXOsResponse",
			DryRun:       true,
			JSONInput:    true,
		},
	}
}

// arkSendMethodRegistry returns the `ark.send.*` schema entries
// (in-round and OOR raw waverpc paths).
func arkSendMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:      "ark.send.inround",
			Description: "Send via in-round refresh (advanced)",
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
			Method:      "ark.send.oor",
			Description: "Send via out-of-round transfer (adv.)",
			Params: []schemaParam{
				{
					Name: "to",
					Type: "string",
					Description: "recipient address " +
						"(exactly one of to, pubkey, " +
						"or pk_script)",
				},
				{
					Name: "pubkey",
					Type: "string",
					Description: "recipient 32-byte " +
						"x-only pubkey hex (exactly " +
						"one of to, pubkey, or " +
						"pk_script)",
				},
				{
					Name: "pk-script",
					Type: "string",
					Description: "recipient raw " +
						"pk_script hex (exactly one " +
						"of to, pubkey, or " +
						"pk_script)",
				},
				{
					Name:        "amount",
					Type:        "int64",
					Required:    true,
					Description: "amount in sats",
				},
				{
					Name: "idempotency-key",
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
