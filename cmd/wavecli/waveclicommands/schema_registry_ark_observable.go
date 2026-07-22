package waveclicommands

// arkObservableMethodRegistry returns schema entries for the `ark.*`
// observability and operator-runbook surface that earlier registry
// blocks omitted: board, fees.{estimate,history}, rounds.{get,list,
// watch}, oor.{get,list}. The CLI itself already exposes these, but
// without schema entries an agent inspecting `wavecli schema --all`
// couldn't discover them.
//
// The list-shaped entries (rounds.list, oor.list, fees.history) carry
// the standard --fields / --ndjson output modifiers via
// listOutputParams so the schema reflects the agent-CLI surface
// addListOutputFlags installs.
func arkObservableMethodRegistry() []schemaMethod {
	out := arkObservableMutateMethodRegistry()
	out = append(out, arkObservableListMethodRegistry()...)
	out = append(out, arkObservableSingletonMethodRegistry()...)

	return out
}

// arkObservableMutateMethodRegistry registers the boarding-mutate and
// boarding-sweep surface.
func arkObservableMutateMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:      "ark.board",
			Description: "Board confirmed UTXOs into VTXOs",
			Params: []schemaParam{
				{
					Name: "target-vtxo-count",
					Type: "uint32",
					Description: "fan the boarded " +
						"balance into N VTXOs; " +
						"zero boards the whole " +
						"balance as one VTXO",
				},
				{
					Name: "no-persist",
					Type: "bool",
					Description: "skip restart-safe " +
						"Board replay (daemon " +
						"restart between admission " +
						"and round seal silently " +
						"drops the intent)",
				},
			},
			RequestType:  "BoardRequest",
			ResponseType: "BoardResponse",
			JSONInput:    true,
		},
		{
			Method: "ark.rounds.join",
			Description: "Commit queued intents and join the " +
				"next " +
				"round",
			Params:       nil,
			RequestType:  "JoinNextRoundRequest",
			ResponseType: "JoinNextRoundResponse",
			JSONInput:    false,
		},
	}
}

// arkObservableListMethodRegistry registers the list-shaped
// observability verbs that gained --fields / --ndjson output
// modifiers in this audit pass.
func arkObservableListMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:      "ark.rounds.list",
			Description: "List current round FSM states",
			Params: append([]schemaParam{
				{
					Name: "persisted-only",
					Type: "bool",
					Description: "skip in-memory pending " +
						"rounds",
				},
				{
					Name: "page-size",
					Type: "int32",
					Description: "max persisted rounds " +
						"to return; zero uses default",
				},
				{
					Name: "page-token",
					Type: "string",
					Description: "cursor from a previous " +
						"response for pagination",
				},
				{
					Name: "state",
					Type: "string",
					Description: "optional state filter " +
						"(confirmed, failed, ...)",
				},
				{
					Name: "created-after",
					Type: "int64",
					Description: "only rounds created at " +
						"or after this Unix time",
				},
				{
					Name: "created-before",
					Type: "int64",
					Description: "only rounds created " +
						"before this Unix time",
				},
			}, listOutputParams()...),
			RequestType:  "ListRoundsRequest",
			ResponseType: "ListRoundsResponse",
			JSONInput:    true,
		},
		{
			Method:      "ark.oor.list",
			Description: "List OOR session statuses",
			Params: append([]schemaParam{
				{
					Name:        "direction",
					Type:        "enum",
					Description: "direction filter",
					Values: []string{
						"all", "outgoing", "incoming",
					},
				},
				{
					Name:        "status",
					Type:        "enum",
					Description: "status filter",
					Values: []string{
						"all", "pending", "completed",
						"failed",
					},
				},
				{
					Name:        "page-size",
					Type:        "int32",
					Description: "max sessions to return",
				},
				{
					Name: "page-token",
					Type: "string",
					Description: "cursor from a previous " +
						"response for pagination",
				},
			}, listOutputParams()...),
			RequestType:  "ListOORSessionsRequest",
			ResponseType: "ListOORSessionsResponse",
			JSONInput:    true,
		},
		{
			Method:      "ark.fees.history",
			Description: "Paginated fee payment history",
			Params: []schemaParam{
				{
					Name:        "limit",
					Type:        "uint32",
					Description: "max entries to return",
				},
				{
					Name:        "offset",
					Type:        "uint32",
					Description: "entries to skip",
				},
			},
			RequestType:  "GetFeeHistoryRequest",
			ResponseType: "GetFeeHistoryResponse",
			JSONInput:    true,
		},
	}
}

// arkObservableSingletonMethodRegistry registers the get-one /
// estimate verbs that fetch a single record rather than a list.
func arkObservableSingletonMethodRegistry() []schemaMethod {
	return []schemaMethod{
		{
			Method:      "ark.rounds.get",
			Description: "Get one round status by id",
			Params: []schemaParam{
				{
					Name:     "round-id",
					Type:     "string",
					Required: true,
					Description: "server-assigned round " +
						"id to fetch",
				},
			},
			RequestType:  "GetRoundRequest",
			ResponseType: "GetRoundResponse",
			JSONInput:    true,
		},
		{
			Method:      "ark.rounds.watch",
			Description: "Stream round state updates",
			Params:      nil,
			RequestType: "WatchRoundsRequest",
			ResponseType: "RoundStateUpdate (server " +
				"stream)",
			JSONInput: false,
		},
		{
			Method:      "ark.oor.get",
			Description: "Get one OOR session status by id",
			Params: []schemaParam{
				{
					Name:        "session-id",
					Type:        "string",
					Required:    true,
					Description: "OOR session id to fetch",
				},
			},
			RequestType:  "GetOORSessionRequest",
			ResponseType: "GetOORSessionResponse",
			JSONInput:    true,
		},
		{
			Method:      "ark.fees.estimate",
			Description: "Estimate fee for a VTXO operation",
			Params: []schemaParam{
				{
					Name:        "amount",
					Type:        "int64",
					Required:    true,
					Description: "VTXO amount in sats",
				},
				{
					Name: "boarding",
					Type: "bool",
					Description: "estimate for boarding " +
						"(default true) or refresh",
				},
				{
					Name: "remaining-blocks",
					Type: "uint32",
					Description: "remaining VTXO " +
						"lifetime in blocks " +
						"(refresh only, required " +
						"when boarding=false)",
				},
			},
			RequestType:  "EstimateFeeRequest",
			ResponseType: "EstimateFeeResponse",
			JSONInput:    true,
		},
	}
}
