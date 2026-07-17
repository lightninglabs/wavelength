package roundpb

const (
	// ServiceName is the fully-qualified protobuf service name used for
	// round protocol mailbox event routing.
	ServiceName = "round.v1.RoundService"

	// MethodJoinAck is the push event method name for
	// ClientSuccessResp. The server sends this to acknowledge a
	// client's JoinRoundRequest was accepted.
	MethodJoinAck = "ClientSuccessResp"

	// MethodBatchInfo is the push event method name for
	// ClientBatchInfo. The server sends this after building the
	// commitment transaction batch.
	MethodBatchInfo = "ClientBatchInfo"

	// MethodAwaitingInputSigs is the push event method name for
	// ClientAwaitingInputSigsResp. The server sends this when it
	// is ready to receive boarding input signatures from the
	// client.
	MethodAwaitingInputSigs = "ClientAwaitingInputSigsResp"

	// MethodAggNonces is the push event method name for
	// ClientVTXOAggNonces. The server sends this with aggregated
	// MuSig2 public nonces for transactions where the client is
	// a cosigner.
	MethodAggNonces = "ClientVTXOAggNonces"

	// MethodAggSigs is the push event method name for
	// ClientVTXOAggSigs. The server sends this with aggregated
	// schnorr signatures after combining all partial signatures.
	MethodAggSigs = "ClientVTXOAggSigs"

	// MethodRoundFailed is the push event method name for
	// ClientRoundFailedResp. The server sends this when a round
	// the client joined has failed.
	MethodRoundFailed = "ClientRoundFailedResp"

	// MethodError is the push event method name for
	// ClientErrorResp. The server sends this for general error
	// conditions.
	MethodError = "ClientErrorResp"

	// MethodJoinRoundQuote is the server→client push event method
	// name for JoinRoundQuote. The server sends this per-client
	// after the round seals under the #270 seal-time fee
	// handshake, carrying the server-decided VTXO / leave amounts
	// and the operator fee the client must accept (by signing) or
	// reject (via RejectQuote / timeout).
	MethodJoinRoundQuote = "JoinRoundQuote"

	// MethodJoinRound is the client→server method name for
	// JoinRoundRequest.
	MethodJoinRound = "JoinRound"

	// MethodAcceptQuote is the client→server method name for the
	// explicit JoinRoundAccept accept path (AcceptQuote RPC).
	// Servers advance from QuoteSentState to BatchBuildingState
	// only once every pending client has sent one of this method,
	// MethodRejectQuote, or timed out.
	MethodAcceptQuote = "AcceptQuote"

	// MethodRejectQuote is the client→server method name for the
	// explicit JoinRoundReject reject path (RejectQuote RPC).
	MethodRejectQuote = "RejectQuote"

	// MethodSubmitNonces is the client→server method name for
	// SubmitNoncesRequest.
	MethodSubmitNonces = "SubmitNonces"

	// MethodSubmitPartialSigs is the client→server method name for
	// SubmitPartialSigRequest.
	MethodSubmitPartialSigs = "SubmitPartialSigs"

	// MethodSubmitForfeitSigs is the client→server method name for
	// SubmitForfeitSigRequest (boarding input signatures).
	MethodSubmitForfeitSigs = "SubmitForfeitSigs"

	// MethodSubmitVTXOForfeitSigs is the client→server method name
	// for SubmitVTXOForfeitSigsToServer.
	MethodSubmitVTXOForfeitSigs = "SubmitVTXOForfeitSigs"

	// MethodQueryRoundStatus is the client→server method name for
	// QueryRoundStatusRequest. A client that has sent forfeit
	// signatures uses it to reconcile a round's fate before
	// releasing forfeit reservations.
	MethodQueryRoundStatus = "QueryRoundStatus"

	// MethodRoundStatusReport is the push event method name for
	// ClientRoundStatusReport. The server sends this in answer to
	// a QueryRoundStatus, carrying the authoritative lifecycle
	// status of the queried round.
	MethodRoundStatusReport = "ClientRoundStatusReport"
)
