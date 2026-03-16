package arkscript

// SpendPath bundles everything needed to spend a custom VTXO leaf
// through OOR. It replaces the separate SpendInfo + ConditionWitness
// fields that callers previously had to manage independently.
type SpendPath struct {
	// SpendInfo is the compiled leaf script + control block.
	*SpendInfo

	// Conditions holds extra witness elements needed by the spend
	// script beyond signatures (e.g., preimage for hashlock).
	Conditions [][]byte
}
