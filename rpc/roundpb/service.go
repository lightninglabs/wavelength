package roundpb

const (
	// ServiceName is the fully-qualified protobuf service name used for
	// round protocol mailbox event routing.
	ServiceName = "round.v1.RoundService"

	// MethodBatchInfo is the push event method name for
	// ClientBatchInfo. The server sends this after building the
	// commitment transaction batch.
	MethodBatchInfo = "BatchInfo"

	// MethodAwaitingInputSigs is the push event method name for
	// ClientAwaitingInputSigsResp. The server sends this when it is
	// ready to receive boarding input signatures from the client.
	MethodAwaitingInputSigs = "AwaitingInputSigs"

	// MethodAggNonces is the push event method name for
	// ClientVTXOAggNonces. The server sends this with aggregated
	// MuSig2 public nonces for transactions where the client is a
	// cosigner.
	MethodAggNonces = "AggNonces"

	// MethodAggSigs is the push event method name for
	// ClientVTXOAggSigs. The server sends this with aggregated
	// schnorr signatures after combining all partial signatures.
	MethodAggSigs = "AggSigs"

	// MethodRoundFailed is the push event method name for
	// ClientRoundFailedResp. The server sends this when a round
	// the client joined has failed.
	MethodRoundFailed = "RoundFailed"

	// MethodError is the push event method name for
	// ClientErrorResp. The server sends this for general error
	// conditions.
	MethodError = "Error"
)
