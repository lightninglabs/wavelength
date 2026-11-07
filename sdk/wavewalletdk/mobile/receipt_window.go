//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
)

// SetHostForeground changes ingress mode without stopping the retained daemon.
// Foreground ingress outlives this call; initialization has a 10-second budget.
func SetHostForeground(foreground bool) error {
	client, parent, err := activeClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	return client.SetHostForeground(ctx, foreground)
}

// RunReceiptWindow checks one enrolled invoice under a cooperative host budget.
// The maximum is 20 seconds; OS expiry cancels the same unique window by ID.
func RunReceiptWindow(reqJSON []byte, timeoutSeconds int64) ([]byte, error) {
	if timeoutSeconds < 1 || timeoutSeconds > 20 {
		return nil, fmt.Errorf("receipt window budget must be 1..20 " +
			"seconds")
	}
	client, parent, err := activeClient()
	if err != nil {
		return nil, err
	}
	var request wavewalletdk.ReceiptWindowRequest
	if err := decode(reqJSON, &request); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(
		parent, time.Duration(timeoutSeconds)*time.Second,
	)
	defer cancel()
	result, err := client.RunReceiptWindow(ctx, request)
	if err != nil {
		return nil, err
	}

	return marshal(result)
}

// CancelReceiptWindow signals expiry while the original invocation still joins.
func CancelReceiptWindow(windowID string) error {
	client, parent, err := activeClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	return client.CancelReceiptWindow(ctx, windowID)
}

// RunMaintenanceWindow attempts due expiry and ordinary native round work.
// The host cancels expiry with CancelReceiptWindow using the same unique ID.
func RunMaintenanceWindow(windowID string,
	timeoutSeconds int64) ([]byte, error) {

	if timeoutSeconds < 1 || timeoutSeconds > 20 {
		return nil, fmt.Errorf("maintenance window budget must be " +
			"1..20 seconds")
	}
	client, parent, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(
		parent, time.Duration(timeoutSeconds)*time.Second,
	)
	defer cancel()
	result, err := client.RunMaintenanceWindow(ctx, windowID)
	if err != nil {
		return nil, err
	}

	return marshal(result)
}
