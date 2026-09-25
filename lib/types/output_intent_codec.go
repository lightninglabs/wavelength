package types

// EncodeVTXORequests serializes output intents with the canonical join-request
// encoding. Actor checkpoints must preserve positional order and policy data
// so replay cannot change the outputs covered by the client's authorization.
func EncodeVTXORequests(requests []*VTXORequest) ([]byte, error) {
	return encodeJoinAuthVTXORequests(requests)
}

// DecodeVTXORequests restores positional output intents with the same bounds
// and validation used by the canonical join-request decoder.
func DecodeVTXORequests(raw []byte) ([]*VTXORequest, error) {
	return decodeJoinAuthVTXORequests(raw)
}

// EncodeLeaveRequests serializes on-chain output intents, including which
// output may receive change after fees are determined.
func EncodeLeaveRequests(requests []*LeaveRequest) ([]byte, error) {
	return encodeJoinAuthLeaveRequests(requests)
}

// DecodeLeaveRequests restores on-chain output intents with the canonical
// decoder's amount, count, and script validation.
func DecodeLeaveRequests(raw []byte) ([]*LeaveRequest, error) {
	return decodeJoinAuthLeaveRequests(raw)
}
