package tapassets

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/stretchr/testify/require"
)

type onboardingPublicationClient struct {
	tapsdk.Client
	transfers []*tapsdk.AssetTransfer
	err       error
}

// ListTransfers models tapd's durable publication log.
func (c *onboardingPublicationClient) ListTransfers(context.Context,
	*tapsdk.ListTransfersRequest) ([]*tapsdk.AssetTransfer, error) {

	return c.transfers, c.err
}

// TestOnboardingPublicationEvidence requires the exact signed transaction,
// including its witness, before recovering an uncertain publication.
func TestOnboardingPublicationEvidence(t *testing.T) {
	t.Parallel()
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 1}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(1000, []byte{0x51}))
	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	packet.Inputs[0].FinalScriptWitness = []byte{1, 1, 0x51}
	final, err := psbtutil.Serialize(packet)
	require.NoError(t, err)
	signed, err := psbt.Extract(packet)
	require.NoError(t, err)
	saved := &tapsdk.AssetTransfer{
		TransferTxid: signed.TxHash(),
		AnchorTx:     serializeTx(signed),
	}
	client := &onboardingPublicationClient{}
	driver := &sdkDriver{
		wallet: tapsdk.NewWallet(client, tapsdk.NetworkRegtest),
	}
	require.ErrorIs(
		t,
		driver.ReconcileOnboarding(
			t.Context(), final,
		),
		ErrReconciliationRequired,
	)
	client.transfers = []*tapsdk.AssetTransfer{saved}
	require.NoError(t, driver.ReconcileOnboarding(t.Context(), final))
	different := signed.Copy()
	different.TxIn[0].Witness[0][0] ^= 1
	saved.AnchorTx = serializeTx(different)
	require.ErrorIs(
		t,
		driver.ReconcileOnboarding(
			t.Context(), final,
		),
		ErrReconciliationRequired,
	)
	client.err = errors.New("tapd unavailable")
	require.ErrorIs(
		t,
		driver.ReconcileOnboarding(
			t.Context(), final,
		),
		ErrReconciliationRequired,
	)
}
