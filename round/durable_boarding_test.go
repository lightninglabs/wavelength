package round

import (
	"bytes"
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestDurableBoardingLocalMetadata keeps wallet ownership separate from the
// submitted request, including independently recorded chain and input points.
func TestDurableBoardingLocalMetadata(t *testing.T) {
	_, owner := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{3}, 32))
	_, operator := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{4}, 32))
	address, err := btcaddr.NewAddressTaproot(
		schnorr.SerializePubKey(owner), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	point := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 2}
	requestPoint := wire.OutPoint{Hash: chainhash.Hash{3}, Index: 4}
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&point, []byte{}, nil))
	tx.AddTxOut(wire.NewTxOut(5000, []byte{0x51}))
	script := &waddrmgr.Tapscript{
		Type:           waddrmgr.TaprootFullKeyOnly,
		FullOutputKey:  owner,
		RevealedScript: []byte{},
		RootHash:       []byte{},
	}
	original := BoardingIntent{
		BoardingIntent: wallet.BoardingIntent{
			Address: wallet.BoardingAddress{
				Address:   address,
				Tapscript: script,
				KeyDesc: keychain.KeyDescriptor{
					KeyLocator: keychain.KeyLocator{
						Family: 7,
						Index:  19,
					},
					PubKey: owner,
				},
				OperatorKey: operator, ExitDelay: 144,
			},
			Outpoint: point,
			ChainInfo: wallet.BoardingChainInfo{
				ConfHeight: -1, ConfHash: chainhash.Hash{
					5,
				}, ConfTx: tx,
				OutPoint: requestPoint, Amount: -7,
			},
			Status: wallet.BoardingStatus(2),
		},
		Request: types.BoardingRequest{
			Outpoint: &requestPoint, PolicyTemplate: []byte{
				1,
				2,
			},
			ClientKey: operator, OperatorKey: owner, ExitDelay: 288,
		},
	}
	raw, err := encodeDurableBoarding(original)
	require.NoError(t, err)
	restored, err := decodeDurableBoarding(
		raw, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.Equal(t, original, restored)
	again, err := encodeDurableBoarding(restored)
	require.NoError(t, err)
	require.Equal(t, raw, again)

	// Empty assembling fields must remain empty through persistence.
	raw, err = encodeDurableBoarding(BoardingIntent{})
	require.NoError(t, err)
	restored, err = decodeDurableBoarding(raw, nil)
	require.NoError(t, err)
	require.Equal(t, BoardingIntent{
		Request: types.BoardingRequest{PolicyTemplate: []byte{}},
	}, restored)
}

// TestDurableBoardingIndependentProofs retains both proof snapshots even when
// the request and wallet have observed different confirmation blocks.
func TestDurableBoardingIndependentProofs(t *testing.T) {
	_, key := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{8}, 32))
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 1}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(9000, []byte{0x51}))
	merkle, err := proof.NewTxMerkleProof([]*wire.MsgTx{tx}, 0)
	require.NoError(t, err)
	chainProof := proof.TxProof{
		MsgTx: *tx, InternalKey: *key,
		BlockHeader: wire.BlockHeader{
			Timestamp: time.Unix(1700000000, 0),
		},
		BlockHeight: 100, MerkleProof: *merkle,
		ClaimedOutPoint: wire.OutPoint{
			Hash: tx.TxHash(),
		},
	}
	requestProof := chainProof
	requestProof.BlockHeight = 101
	requestProof.BlockHeader.Nonce = 17
	original := BoardingIntent{
		BoardingIntent: wallet.BoardingIntent{
			ChainInfo: wallet.BoardingChainInfo{
				TxProof: fn.Some(chainProof),
			},
		},
		Request: types.BoardingRequest{
			TxProof: fn.Some(requestProof),
		},
	}
	raw, err := encodeDurableBoarding(original)
	require.NoError(t, err)
	restored, err := decodeDurableBoarding(raw, nil)
	require.NoError(t, err)
	require.True(t, restored.ChainInfo.TxProof.IsSome())
	require.True(t, restored.Request.TxProof.IsSome())
	chain := restored.ChainInfo.TxProof.UnwrapOr(proof.TxProof{})
	request := restored.Request.TxProof.UnwrapOr(proof.TxProof{})
	require.Equal(t, chainProof.BlockHeight, chain.BlockHeight)
	require.Equal(t, requestProof.BlockHeight, request.BlockHeight)
	require.Equal(
		t, chainProof.BlockHeader.BlockHash(),
		chain.BlockHeader.BlockHash(),
	)
	require.Equal(
		t, requestProof.BlockHeader.BlockHash(),
		request.BlockHeader.BlockHash(),
	)
	again, err := encodeDurableBoarding(restored)
	require.NoError(t, err)
	require.Equal(t, raw, again)
}
