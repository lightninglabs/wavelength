//go:build swapruntime

package waved

// defaultMaxPaymentCLTV returns the automatic-maintenance payment reserve for
// swap-enabled builds. These builds can initiate Lightning payments, so they
// keep VTXOs fresh enough for a high-CLTV route by default.
func defaultMaxPaymentCLTV() int32 {
	return DefaultMaxPaymentCLTV
}
