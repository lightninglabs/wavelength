//go:build mobile && wavewalletrpc && swapruntime

package mobile

// This file holds the scalar "hot path" conveniences of the hybrid API. A host
// that only needs a single number or flag should not have to stand up a JSON
// decoder, so these return plain gomobile scalars instead of JSON bytes.

// ConfirmedBalanceSat returns the confirmed wallet balance in satoshis.
func ConfirmedBalanceSat() (int64, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return 0, err
	}

	bal, err := client.Balance(ctx)
	if err != nil {
		return 0, err
	}

	return bal.ConfirmedSat, nil
}

// PendingInboundSat returns the pending inbound balance in satoshis.
func PendingInboundSat() (int64, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return 0, err
	}

	bal, err := client.Balance(ctx)
	if err != nil {
		return 0, err
	}

	return bal.PendingInSat, nil
}

// WalletReady reports whether the daemon wallet is fully unlocked and ready to
// sign. It is the scalar form of GetInfo().WalletState == ready.
func WalletReady() (bool, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return false, err
	}

	info, err := client.GetInfo(ctx)
	if err != nil {
		return false, err
	}

	return info.WalletReady(), nil
}

// IsRunning reports whether a daemon lifecycle is active: true from the moment
// Start begins, across the whole boot window, until Stop completes. It never
// blocks on an RPC, so a host can poll it cheaply from the UI thread. Note that
// a true result during the boot window does not yet mean RPCs will succeed; use
// WalletReady / GetInfo for readiness.
func IsRunning() bool {
	return lifecycleActive()
}
