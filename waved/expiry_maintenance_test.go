package waved

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestMaintenanceWindowPumpsWithoutInvoice proves ordinary wakes do not need
// source enrollment and do not authorize receipts. Deferred expiry still pumps.
func TestMaintenanceWindowPumpsWithoutInvoice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ingress := &windowTestIngress{
			pump: func(context.Context) (
				serverconn.IngressPumpResult, error) {

				return serverconn.IngressPumpResult{}, nil
			},
		}
		controller := newWindowTestController(t, ingress)
		controller.read = func(context.Context,
			LightningReceiptRequest) ([]LightningReceipt, error) {

			t.Fatal("maintenance read a receipt source")

			return nil, nil
		}
		controller.resume = func(context.Context,
			LightningReceiptReference) error {

			t.Fatal("maintenance resumed an invoice")

			return nil
		}
		controller.maintain = func(context.Context) (uint32, bool,
			bool) {

			return 3, true, true
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		result, err := controller.runWindow(
			ctx, ReceiptWindowRequest{
				WindowID: "maintenance",
			},
			false,
		)
		require.NoError(t, err)
		require.True(t, result.Interrupted)
		require.Empty(t, result.Receipts)
		require.EqualValues(t, 3, result.MaintenanceChecked)
		require.True(t, result.MaintenanceDeferred)
		require.True(t, result.MaintenanceMore)
		require.GreaterOrEqual(
			t, ingress.pumps, 2, "empty mailbox returned early",
		)
	})
}

// TestMaintenanceCursorResumesActualProgress proves interruption does not skip
// an unprocessed owner and an exhausted finite sweep starts a fresh range.
func TestMaintenanceCursorResumesActualProgress(t *testing.T) {
	first := wire.OutPoint{Index: 1}
	last := wire.OutPoint{Index: 2}
	pass := 0
	m := &expiryMaintenance{
		tip: func(context.Context) (int32, chainhash.Hash, error) {
			return 100, chainhash.Hash{}, nil
		},
		page: func(_ context.Context, after, through *wire.OutPoint,
			limit int32) (db.VTXOMaintenancePage, error) {

			require.Equal(t, db.MaxVTXOMaintenancePage, limit)
			if pass == 0 {
				require.Nil(t, after)
				require.Nil(t, through)

				return db.VTXOMaintenancePage{
					Outpoints: []wire.OutPoint{
						first,
						last,
					},
					Through: &last,
					Next:    &last,
				}, nil
			}
			require.Equal(t, &first, after)
			require.Equal(t, &last, through)

			return db.VTXOMaintenancePage{
				Outpoints: []wire.OutPoint{
					last,
				},
				Through: &last,
				Next:    &last,
			}, nil
		},
		drive: func(_ context.Context, points []wire.OutPoint,
			epoch vtxo.BlockEpochEvent) (
			vtxo.ExpiryMaintenanceResult, error) {

			require.EqualValues(t, 100, epoch.Height)
			pass++
			if pass == 1 {
				return vtxo.ExpiryMaintenanceResult{
					Next:    1,
					Checked: 1,
				}, context.Canceled
			}
			require.Equal(t, []wire.OutPoint{last}, points)

			return vtxo.ExpiryMaintenanceResult{
				Next:    1,
				Checked: 1,
			}, nil
		},
	}
	checked, deferred, more := m.run(t.Context())
	require.EqualValues(t, 1, checked)
	require.True(t, deferred)
	require.True(t, more)
	_, deferred, more = m.run(t.Context())
	require.False(t, deferred)
	require.False(t, more)
	require.Nil(t, m.after)
	require.Nil(t, m.through)
	m.tip = func(context.Context) (int32, chainhash.Hash, error) {
		return 0, chainhash.Hash{}, errors.New("offline")
	}
	checked, deferred, more = m.run(t.Context())
	require.Zero(t, checked)
	require.True(t, deferred)
	require.True(t, more)
}
