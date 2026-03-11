package main

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
	// Method is the dot-notation name (e.g. "list-rounds").
	Method string `json:"method"`

	// Description is a short description of the command.
	Description string `json:"description"`

	// Params lists the accepted parameters.
	Params []schemaParam `json:"params"`

	// RequestType is the proto request message name.
	RequestType string `json:"request_type"`

	// ResponseType is the proto response message name.
	ResponseType string `json:"response_type"`

	// DryRun indicates whether the command supports --dry-run.
	DryRun bool `json:"dry_run,omitempty"`

	// JSONInput indicates the command accepts --json for raw
	// proto payloads.
	JSONInput bool `json:"json_input"`
}

// methodRegistry returns the full schema for all arkcli commands.
// This is the single source of truth for both the schema command and
// MCP tool definitions.
func methodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:       "info",
			Description:  "Display operator server status",
			Params:       nil,
			RequestType:  "InfoRequest",
			ResponseType: "InfoResponse",
			JSONInput:    true,
		},
		{
			Method:       "trigger-batch",
			Description:  "Manually trigger a new batch round",
			Params:       nil,
			RequestType:  "TriggerBatchRequest",
			ResponseType: "TriggerBatchResponse",
			JSONInput:    true,
		},
		{
			Method:      "list-rounds",
			Description: "List past and active rounds",
			Params: []schemaParam{
				{
					Name:        "status",
					Type:        "enum",
					Description: "filter by round status",
					Values: []string{
						"open", "sealed",
						"signing", "broadcast",
						"confirmed", "failed",
					},
				},
				{
					Name:        "limit",
					Type:        "int64",
					Description: "max results to return",
				},
				{
					Name:        "offset",
					Type:        "int64",
					Description: "results to skip",
				},
				{
					Name: "fields",
					Type: "string",
					Description: "comma-separated " +
						"field names to include",
				},
				{
					Name: "ndjson",
					Type: "bool",
					Description: "one JSON object " +
						"per round",
				},
			},
			RequestType:  "ListRoundsRequest",
			ResponseType: "ListRoundsResponse",
			JSONInput:    true,
		},
		{
			Method:      "list-vtxos",
			Description: "List VTXOs with optional filters",
			Params: []schemaParam{
				{
					Name:        "status",
					Type:        "enum",
					Description: "filter by VTXO status",
					Values: []string{
						"pending", "live",
						"forfeited",
					},
				},
				{
					Name:        "limit",
					Type:        "int64",
					Description: "max results to return",
				},
				{
					Name: "fields",
					Type: "string",
					Description: "comma-separated " +
						"field names to include",
				},
				{
					Name: "ndjson",
					Type: "bool",
					Description: "one JSON object " +
						"per VTXO",
				},
			},
			RequestType:  "ListVTXOsRequest",
			ResponseType: "ListVTXOsResponse",
			JSONInput:    true,
		},
		{
			Method:       "vtxo-stats",
			Description:  "Display aggregate VTXO statistics",
			Params:       nil,
			RequestType:  "GetVTXOStatsRequest",
			ResponseType: "GetVTXOStatsResponse",
			JSONInput:    true,
		},
		{
			Method:      "list-clients",
			Description: "List registered mailbox clients",
			Params: []schemaParam{
				{
					Name: "fields",
					Type: "string",
					Description: "comma-separated " +
						"field names to include",
				},
				{
					Name: "ndjson",
					Type: "bool",
					Description: "one JSON object " +
						"per client",
				},
			},
			RequestType:  "ListClientsRequest",
			ResponseType: "ListClientsResponse",
			JSONInput:    true,
		},
	}
}
