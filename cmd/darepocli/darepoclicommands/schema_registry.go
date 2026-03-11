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
// MCP tool definitions.
func methodRegistry() []schemaMethod {
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
					Name:        "to",
					Type:        "string",
					Required:    true,
					Description: "recipient address",
				},
				{
					Name:        "amount",
					Type:        "int64",
					Required:    true,
					Description: "amount in sats",
				},
			},
			RequestType:  "SendOORRequest",
			ResponseType: "SendOORResponse",
			DryRun:       true,
			JSONInput:    true,
		},
	}
}
