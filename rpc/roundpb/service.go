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

	// MethodJoinRound is the client→server method name for
	// JoinRoundRequest.
	MethodJoinRound = "JoinRound"

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
)
