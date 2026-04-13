package swaps

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

type testInSwapServerConn struct {
	cfg *InSwapConfig
}

// RequestChannelID is unused in these tests.
func (c *testInSwapServerConn) RequestChannelID(
	_ context.Context, _ *btcec.PublicKey,
	_ uint32) (*RouteHint, *VHTLCConfig, error) {

	return nil, nil, nil
}

// CreateInSwap returns the preconfigured in-swap config.
func (c *testInSwapServerConn) CreateInSwap(
	context.Context, string, uint64,
	*btcec.PublicKey) (*InSwapConfig, error) {

	return c.cfg, nil
}

// Close closes the server connection.
func (c *testInSwapServerConn) Close() error {
	return nil
}

// TestPayViaLightningReturnsClaimPreimage asserts the SDK recovers the
// preimage from the spending OOR package after the vHTLC is claimed.
func TestPayViaLightningReturnsClaimPreimage(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage, err := NewPreimage()
	require.NoError(t, err)

	serverConn := &testInSwapServerConn{
		cfg: &InSwapConfig{
			PaymentHash:  preimage.Hash(),
			AmountSat:    42_000,
			FeeSat:       123,
			ServerPubkey: serverPriv.PubKey(),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime:                       144,
				UnilateralClaimDelay:                 12,
				UnilateralRefundDelay:                24,
				UnilateralRefundWithoutReceiverDelay: 36,
			},
			Expiry: time.Now().Add(time.Minute),
		},
	}

	daemonConn := &testDaemonConn{
		identityKey:   clientPriv.PubKey(),
		operatorKey:   operatorPriv.PubKey(),
		sendSessionID: "funding-session",
		spentVTXO: &VTXOInfo{
			SpentByTxid: "0123456789abcdef0123456789abcdef" +
				"0123456789abcdef0123456789abcdef",
		},
		indexedPackage: &OORPackageInfo{
			FinalCheckpointPSBTs: [][]byte{
				testCheckpointPSBTWithPreimage(
					t, preimage[:],
				),
			},
		},
	}

	client := NewSwapClient(serverConn, daemonConn, nil, nil)
	client.waitPollInterval = time.Millisecond

	result, err := client.PayViaLightning(
		t.Context(), "lnrtest1invoice", 0,
	)
	require.NoError(t, err)
	require.Equal(t, preimage.Hash(), result.PaymentHash)
	require.Equal(t, preimage, result.Preimage)
	require.Equal(t, "funding-session", result.FundingSessionID)
	require.EqualValues(t, 123, result.FeeSat)
	require.NotEmpty(t, daemonConn.lastSendPolicy)
}

// TestWaitForSpentVTXOPreimageUsesSpendingSession asserts the SDK fetches the
// checkpoints of the OOR session that spent the funded vHTLC when the spent
// vHTLC's own package does not carry the preimage.
func TestWaitForSpentVTXOPreimageUsesSpendingSession(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		spentVTXO: &VTXOInfo{
			SpentByTxid: "0123456789abcdef0123456789abcdef" +
				"0123456789abcdef0123456789abcdef",
		},
		indexedPackage: &OORPackageInfo{
			FinalCheckpointPSBTs: [][]byte{
				testCheckpointPSBTWithPreimage(
					t, preimage[:],
				),
			},
		},
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	client.waitPollInterval = time.Millisecond

	result, err := client.waitForSpentVTXOPreimage(
		t.Context(), preimage.Hash(), []byte{0x51},
		time.Now().Add(time.Second),
	)
	require.NoError(t, err)
	require.Equal(t, preimage, *result)
}

// TestWaitForSpentVTXOPreimageUsesLocalSpentPackages asserts the SDK prefers
// locally persisted spent-VTXO checkpoints when they already carry the claim
// preimage.
func TestWaitForSpentVTXOPreimageUsesLocalSpentPackages(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		spentVTXOs: []VTXOInfo{{
			PkScript: []byte{0x51},
			FinalCheckpointPSBTs: [][]byte{
				testCheckpointPSBTWithPreimage(
					t, preimage[:],
				),
			},
		}},
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	client.waitPollInterval = time.Millisecond

	result, err := client.waitForSpentVTXOPreimage(
		t.Context(), preimage.Hash(), []byte{0x51},
		time.Now().Add(time.Second),
	)
	require.NoError(t, err)
	require.Equal(t, preimage, *result)
}

// TestWaitForSpentVTXOPreimageFallsBackToLivePackages asserts the SDK can
// recover the claim preimage from a received live VTXO package when the spent
// vHTLC itself is not exposed as a local spent VTXO.
func TestWaitForSpentVTXOPreimageFallsBackToLivePackages(t *testing.T) {
	t.Parallel()

	preimage, err := NewPreimage()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		liveVTXOs: []VTXOInfo{{
			FinalCheckpointPSBTs: [][]byte{
				testCheckpointPSBTWithPreimage(
					t, preimage[:],
				),
			},
		}},
	}

	client := NewSwapClient(nil, daemonConn, nil, nil)
	client.waitPollInterval = time.Millisecond

	result, err := client.waitForSpentVTXOPreimage(
		t.Context(), preimage.Hash(), []byte{0x51},
		time.Now().Add(time.Second),
	)
	require.NoError(t, err)
	require.Equal(t, preimage, *result)
}

// testCheckpointPSBTWithPreimage encodes one finalized checkpoint PSBT that
// carries preimage in a taproot script-spend signature slot.
func testCheckpointPSBTWithPreimage(t *testing.T, preimage []byte) []byte {
	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{})
	tx.AddTxOut(&wire.TxOut{
		Value:    1,
		PkScript: []byte{0x51},
	})

	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	var witness bytes.Buffer
	err = wire.WriteVarInt(&witness, 0, 1)
	require.NoError(t, err)

	err = wire.WriteVarBytes(&witness, 0, preimage)
	require.NoError(t, err)

	packet.Inputs[0].FinalScriptWitness = witness.Bytes()

	var buf bytes.Buffer
	err = packet.Serialize(&buf)
	require.NoError(t, err)

	return buf.Bytes()
}
