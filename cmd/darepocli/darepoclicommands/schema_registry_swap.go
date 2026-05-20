package darepoclicommands

// swapMethodRegistry returns the schema entries for the `swap.*`
// advanced subtree. Each entry carries mcp_tool=true; the MCP
// registration itself is build-tag gated on `swapruntime`, so a
// stub-build agent that consults the schema sees the verbs but the
// actual MCP server only exposes them when the daemon was built with
// the matching tag.
func swapMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:      "swap.list",
			Description: "List persisted Lightning swap sessions",
			Params: []schemaParam{
				{
					Name: "pending",
					Type: "bool",
					Description: "only non-terminal " +
						"resumable swaps",
				},
			},
			RequestType:  "ListSwapsRequest",
			ResponseType: "ListSwapsResponse",
			JSONInput:    true,
			MCPTool:      true,
		},
		{
			Method: "swap.show",
			Description: "Show one persisted Lightning swap " +
				"session by payment hash",
			Params: []schemaParam{
				{
					Name:     "payment_hash",
					Type:     "string",
					Required: true,
					Description: "32-byte payment hash " +
						"(64 hex chars)",
				},
			},
			RequestType:  "GetSwapRequest",
			ResponseType: "GetSwapResponse",
			JSONInput:    true,
			MCPTool:      true,
		},
		{
			Method:      "swap.receive",
			Description: "Receive BTC via Lightning into Ark",
			Params: []schemaParam{
				{
					Name:        "amount",
					Type:        "int64",
					Required:    true,
					Description: "amount in satoshis",
				},
			},
			RequestType:  "StartReceiveRequest",
			ResponseType: "StartReceiveResponse",
			JSONInput:    true,
			MCPTool:      true,
		},
		{
			Method:      "swap.pay",
			Description: "Pay a Lightning invoice from Ark funds",
			Params: []schemaParam{
				{
					Name:        "invoice",
					Type:        "string",
					Required:    true,
					Description: "BOLT-11 invoice to pay",
				},
				{
					Name: "maxfee",
					Type: "uint64",
					Description: "maximum fee in " +
						"satoshis; zero means no limit",
				},
			},
			RequestType:  "StartPayRequest",
			ResponseType: "StartPayResponse",
			JSONInput:    true,
			MCPTool:      true,
		},
		{
			Method: "swap.resume",
			Description: "Resume a persisted Lightning swap " +
				"session",
			Params: []schemaParam{
				{
					Name:     "payment_hash",
					Type:     "string",
					Required: true,
					Description: "32-byte payment hash " +
						"(64 hex chars)",
				},
				{
					Name:        "direction",
					Type:        "enum",
					Required:    true,
					Description: "swap direction to resume",
					Values: []string{
						"pay",
						"receive",
					},
				},
			},
			RequestType:  "ResumeSwapRequest",
			ResponseType: "ResumeSwapResponse",
			JSONInput:    true,
			MCPTool:      true,
		},
		{
			Method:      "swap.watch",
			Description: "Stream swap session updates",
			Params: []schemaParam{
				{
					Name: "pending",
					Type: "bool",
					Description: "stream only pending " +
						"swap updates",
				},
				{
					Name: "include-existing",
					Type: "bool",
					Description: "emit existing swaps " +
						"before live updates " +
						"(default true)",
				},
			},
			RequestType:  "SubscribeSwapsRequest",
			ResponseType: "SwapSummary (stream)",
			JSONInput:    true,
			MCPTool:      false,
		},
	}
}
