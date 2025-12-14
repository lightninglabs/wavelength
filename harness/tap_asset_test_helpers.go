package harness

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
)

// NormalizeScriptKey normalizes a script key to 32-byte x-only format.
func NormalizeScriptKey(scriptKey []byte) ([]byte, error) {
	var xOnly [schnorr.PubKeyBytesLen]byte

	switch len(scriptKey) {
	case schnorr.PubKeyBytesLen:
		copy(xOnly[:], scriptKey)

	case schnorr.PubKeyBytesLen + 1:
		copy(xOnly[:], scriptKey[1:])

	default:
		return nil, fmt.Errorf("invalid script key length: %d",
			len(scriptKey))
	}

	return xOnly[:], nil
}

// LatestProofFromBlob extracts the latest proof from a proof file blob.
func LatestProofFromBlob(t *testing.T, blob []byte) *proof.Proof {
	t.Helper()

	pf, err := proof.DecodeFile(blob)
	require.NoError(t, err)
	require.NotZero(t, pf.NumProofs(), "proof file empty")

	proofIdx := uint32(pf.NumProofs() - 1)
	pr, err := pf.ProofAt(proofIdx)
	require.NoError(t, err)

	return pr
}

// FindAssetInputIndex finds the index of the asset input in the given PSBT.
func FindAssetInputIndex(t *testing.T, pkt *psbt.Packet, p *proof.Proof) int {
	t.Helper()

	anchorHash := p.AnchorTx.TxHash()
	assetOutpoint := wire.OutPoint{
		Hash:  anchorHash,
		Index: p.InclusionProof.OutputIndex,
	}

	for idx, txIn := range pkt.UnsignedTx.TxIn {
		if txIn.PreviousOutPoint == assetOutpoint {
			return idx
		}
	}

	t.Fatalf("asset input not found in anchor psbt")

	return -1
}

// MatchAssetTransfer creates a TransferMatcher that matches an asset transfer
// with the given parameters.
func MatchAssetTransfer(assetID []byte, scriptKey []byte, internalKey []byte,
	amount uint64, status taprpc.ProofDeliveryStatus) TransferMatcher {

	return func(t *testing.T, transfers []*taprpc.AssetTransfer) (
		*taprpc.AssetTransfer, int) {

		expectedScriptKey, err := NormalizeScriptKey(scriptKey)
		require.NoError(t, err)

		for _, transfer := range transfers {
			for idx, out := range transfer.Outputs {
				if !bytes.Equal(out.AssetId, assetID) {
					continue
				}

				if out.Amount != amount {
					continue
				}

				outKey, err := NormalizeScriptKey(out.ScriptKey)
				require.NoError(t, err)

				if !bytes.Equal(outKey, expectedScriptKey) {
					continue
				}

				if !bytes.Equal(
					out.Anchor.InternalKey, internalKey,
				) {

					continue
				}

				if out.ProofDeliveryStatus != status {
					continue
				}

				return transfer, idx
			}
		}

		return nil, -1
	}
}

// FundNode funds the given LND node with Bitcoin UTXOs.
func FundNode(h *Harness, lnd *lndclient.LndServices) {
	ctx, cancel := context.WithTimeout(h.T.Context(), defaultTimeout)
	defer cancel()

	addrResp, err := lnd.WalletKit.NextAddr(
		ctx, "", walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	require.NoError(h.T, err)

	const utxosToFund = 5
	const amountPerUtxo = btcutil.SatoshiPerBitcoin

	for range utxosToFund {
		h.Faucet(addrResp.String(), btcutil.Amount(amountPerUtxo))
	}

	h.Generate(3)

	require.Eventually(h.T, func() bool {
		ctxt, cancel := context.WithTimeout(
			h.T.Context(), defaultTimeout,
		)
		defer cancel()

		info, err := lnd.Client.GetInfo(ctxt)

		return err == nil && info.SyncedToChain
	}, defaultTimeout, time.Second)
}

// SendAssetHelper sends an asset from sender to receiver.
func SendAssetHelper(t *testing.T, h *Harness,
	sender, receiver *TapClientHarness, assetID [32]byte,
	amt uint64) {

	ctx, cancel := context.WithTimeout(t.Context(), defaultTimeout)
	defer cancel()

	addr, err := receiver.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId: assetID[:],
		Amt:     amt,
	})
	require.NoError(t, err)

	sender.SendAsset(addr.Encoded)
	h.GenerateAndWait(1)
	require.Eventually(t, func() bool {
		ctxt, cancel := context.WithTimeout(t.Context(), defaultTimeout)
		defer cancel()

		completed := taprpc.AddrEventStatus_ADDR_EVENT_STATUS_COMPLETED

		addrReceives, err := receiver.AddrReceives(
			ctxt, &taprpc.AddrReceivesRequest{
				FilterAddr:   addr.Encoded,
				FilterStatus: completed,
			},
		)
		require.NoError(t, err)

		return len(addrReceives.Events) == 1
	}, defaultTimeout, time.Second)
}

// MatchCompletedAddrTransfer creates a TransferMatcher for a completed address
// transfer.
func MatchCompletedAddrTransfer(
	addr *taprpc.Addr) TransferMatcher {

	return MatchAssetTransfer(
		addr.AssetId, addr.ScriptKey, addr.InternalKey, addr.Amount,
		taprpc.ProofDeliveryStatus_PROOF_DELIVERY_STATUS_COMPLETE,
	)
}

// SendAssetToAddr sends an asset to the given address and waits for the
// transfer to complete.
func SendAssetToAddr(h *Harness,
	sender *TapClientHarness, addr *taprpc.Addr) (
	*taprpc.AssetTransfer, int) {

	sender.SendAsset(addr.Encoded)
	h.GenerateAndWait(1)

	return sender.WaitForTransfer(MatchCompletedAddrTransfer(addr))
}
