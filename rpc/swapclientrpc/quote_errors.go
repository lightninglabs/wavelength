package swapclientrpc

const (
	// PayBelowMinimumHint identifies the local operator floor rejection in
	// an InvalidArgument response. Keep it stable for older SDK clients.
	PayBelowMinimumHint = "minimum for pay swaps (operator VTXO minimum)"

	// PayCreditCapHint identifies a quote whose required credit exceeds
	// the caller cap. The quote service wraps this as a gRPC Unknown error.
	PayCreditCapHint = "credit requirement exceeds max_credit_sat"
)
