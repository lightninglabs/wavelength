package wavewalletdk

import (
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

// TestDecodeInvoice distinguishes validated amountless requests from invalid,
// expired, wrong-network and fixed-amount invoices without a running wallet.
func TestDecodeInvoice(t *testing.T) {
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	makeInvoice := func(amount *lnwire.MilliSatoshi,
		timestamp time.Time) string {

		var hash [32]byte
		hash[0] = 42
		inv, err := zpay32.NewInvoice(
			&chaincfg.RegressionNetParams, hash, timestamp,
			zpay32.Description("Coffee with Alex"),
		)
		require.NoError(t, err)
		inv.MilliSat = amount
		encoded, err := inv.Encode(
			zpay32.MessageSigner{
				SignCompact: func(msg []byte) ([]byte, error) {
					digest := sha256.Sum256(msg)

					return ecdsa.SignCompact(
						key, digest[:], true,
					), nil
				},
			},
		)
		require.NoError(t, err)

		return encoded
	}
	for _, network := range []struct {
		name   string
		params *chaincfg.Params
	}{
		{
			"mainnet",
			&chaincfg.MainNetParams,
		}, {
			"testnet",
			&chaincfg.TestNet3Params,
		},
		{
			"testnet3",
			&chaincfg.TestNet3Params,
		}, {
			"testnet4",
			&chaincfg.TestNet4Params,
		},
		{
			"signet",
			&chaincfg.SigNetParams,
		}, {
			"regtest",
			&chaincfg.RegressionNetParams,
		},
	} {
		t.Run(network.name, func(t *testing.T) {
			var hash [32]byte
			hash[0] = 42
			inv, err := zpay32.NewInvoice(
				network.params, hash, time.Now(),
				zpay32.Description("network"),
			)
			require.NoError(t, err)
			encoded, err := inv.Encode(
				zpay32.MessageSigner{
					SignCompact: func(msg []byte) ([]byte,
						error) {

						digest := sha256.Sum256(msg)

						return ecdsa.SignCompact(
							key, digest[:], true,
						), nil
					},
				},
			)
			require.NoError(t, err)
			details, err := DecodeInvoice(encoded, network.name)
			require.NoError(t, err)
			require.Nil(t, details.AmountSat)
		})
	}

	amountless := makeInvoice(nil, time.Now())
	for _, invoice := range []string{
		amountless,
		strings.ToUpper(amountless),
	} {
		details, err := DecodeInvoice(invoice, "regtest")
		require.NoError(t, err)
		require.Nil(t, details.AmountSat)
		require.Equal(t, "Coffee with Alex", details.Description)
	}
	fixed := lnwire.MilliSatoshi(25000000)
	details, err := DecodeInvoice(
		makeInvoice(
			&fixed, time.Now(),
		),
		"regtest",
	)
	require.NoError(t, err)
	require.Equal(t, uint64(25000), *details.AmountSat)
	_, err = DecodeInvoice(amountless, "mainnet")
	require.Error(t, err)
	_, err = DecodeInvoice(
		makeInvoice(
			nil, time.Now().Add(-2*time.Hour),
		),
		"regtest",
	)
	require.ErrorContains(t, err, "expired")
	// Change one checksum symbol without leaving the Bech32 alphabet.
	suffix := "q"
	if strings.HasSuffix(amountless, suffix) {
		suffix = "p"
	}
	_, err = DecodeInvoice(amountless[:len(amountless)-1]+suffix, "regtest")
	require.ErrorContains(t, err, "checksum")
	_, err = DecodeInvoice("lnbcrt1invalid", "regtest")
	require.Error(t, err)
}
