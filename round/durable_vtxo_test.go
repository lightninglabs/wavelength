package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestDurableVTXOLocalMetadata checks the fields deliberately excluded from
// authenticated wire requests, including an unassigned signing key and a
// foreign output with no local owner. Persisting must not change either case.
func TestDurableVTXOLocalMetadata(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		key := func(name string) *btcec.PublicKey {
			if rapid.Bool().Draw(t, name+"Absent") {
				return nil
			}
			_, pub := btcec.PrivKeyFromBytes([]byte{
				rapid.ByteRange(1, 255).Draw(t, name),
			})

			return pub
		}
		descriptor := func(name string) keychain.KeyDescriptor {
			family := rapid.Int32().Draw(t, name+"Family")
			index := rapid.Uint32().Draw(t, name+"Index")

			return keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: keychain.KeyFamily(family),
					Index:  index,
				},
				PubKey: key(name),
			}
		}
		req := types.VTXORequest{
			Amount: btcutil.Amount(
				rapid.Int64().Draw(t, "amount"),
			),
			IsChange:    rapid.Bool().Draw(t, "change"),
			FixedAmount: rapid.Bool().Draw(t, "fixed"),
			AssetRef:    rapid.String().Draw(t, "asset"),
			AssetAmount: rapid.Uint64().Draw(t, "units"),
			PolicyTemplate: []byte{
				1,
				2,
				3,
			}, PkScript: []byte{
				4,
				5,
			},
			Expiry:    rapid.Uint32().Draw(t, "expiry"),
			ClientKey: key("client"), OperatorKey: key("operator"),
			OwnerKey: descriptor("owner"), SigningKey: descriptor(
				"signer",
			),
			Origin: types.VTXOOrigin(
				rapid.Byte().Draw(
					t, "origin",
				),
			),
		}
		if rapid.Bool().Draw(t, "refresh") {
			point := wire.OutPoint{
				Index: rapid.Uint32().Draw(t, "index"),
			}
			point.Hash[0] = rapid.Byte().Draw(t, "hash")
			req.RefreshSourceOutpoint = &point
		}
		raw, err := encodeDurableVTXO(req)
		require.NoError(t, err)
		restored, err := decodeDurableVTXO(raw)
		require.NoError(t, err)
		require.Equal(t, req, restored)
		reencoded, err := encodeDurableVTXO(restored)
		require.NoError(t, err)
		require.Equal(t, raw, reencoded)
	})
}
