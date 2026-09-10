//go:build !swapruntime

package waved

// defaultMaxPaymentCLTV disables the payment-specific lifetime reserve in
// builds that cannot initiate Lightning swaps. Such clients retain the
// ordinary VTXO exit and cooperative-retry refresh policy.
func defaultMaxPaymentCLTV() int32 {
	return 0
}
