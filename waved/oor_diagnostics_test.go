package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/db/sqlc"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/stretchr/testify/require"
)

// TestDiagnosticOORAmountsExcludeChangeAndUnknown pins public amount semantics.
func TestDiagnosticOORAmountsExcludeChangeAndUnknown(t *testing.T) {
	requested := []oortx.RecipientOutput{
		{
			Value: 999,
			PkScript: []byte{
				0x51,
			},
		},
	}
	all := append(
		[]oortx.RecipientOutput{
			{Value: 400000, PkScript: []byte{0x52}},
		},
		requested...,
	)
	raw, err := oor.NewOutgoingReplayData(all, requested)
	require.NoError(t, err)
	amount, err := diagnosticRequestedAmount(raw)
	require.NoError(t, err)
	require.NotNil(t, amount)
	require.EqualValues(t, 999, *amount)
	amount, err = diagnosticRequestedAmount(nil)
	require.NoError(t, err)
	require.Nil(t, amount)
	_, err = diagnosticRequestedAmount([]byte{0xff})
	require.Error(t, err)
	rows := []sqlc.ListOORVTXOBindingsBySessionRow{
		{
			LinkKind: int32(types.OORPackageLinkKindConsumedInput),
			ValueSat: 500000,
		},
		{
			LinkKind: int32(types.OORPackageLinkKindCreatedOutput),
			ValueSat: 999,
		},
		{
			LinkKind: int32(types.OORPackageLinkKindCreatedOutput),
			ValueSat: 42,
		},
	}
	require.EqualValues(t, 1041, *diagnosticReceivedAmount(rows))
	rows[2].ValueSat = 0
	require.Nil(t, diagnosticReceivedAmount(rows))
	require.Nil(t, diagnosticReceivedAmount(rows[:1]))
}
