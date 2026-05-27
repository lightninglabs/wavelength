package darepod

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// SendWalletOnchain pays a bech32 destination from the backing Bitcoin wallet
// rather than from Ark VTXOs. It returns the broadcast transaction id.
func (s *Server) SendWalletOnchain(ctx context.Context, address string,
	amtSat uint64, label string) (string, error) {

	if !s.isWalletReady() {
		return "", fmt.Errorf("wallet is not ready")
	}
	if amtSat == 0 {
		return "", fmt.Errorf("amount must be positive")
	}
	if amtSat > math.MaxInt64 {
		return "", fmt.Errorf("amount exceeds int64 range")
	}

	addr, err := btcutil.DecodeAddress(
		strings.TrimSpace(address), s.chainParams,
	)
	if err != nil {
		return "", fmt.Errorf("decode destination address: %w", err)
	}
	if !addr.IsForNet(s.chainParams) {
		return "", fmt.Errorf("destination address is for wrong "+
			"network: got %s, want %s", addrNetName(addr),
			s.chainParams.Name)
	}

	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return "", fmt.Errorf("destination script: %w", err)
	}
	outputs := []*wire.TxOut{{
		Value:    int64(amtSat),
		PkScript: pkScript,
	}}

	label = strings.TrimSpace(label)
	if label == "" {
		label = "walletdk onchain send"
	}

	var tx *wire.MsgTx
	switch {
	case s.lnd.IsSome():
		lndSvc := s.lnd.UnsafeFromSome()
		tx, err = lndSvc.WalletKit.SendOutputs(
			ctx, outputs, chainfee.FeePerKwFloor, label,
		)

	case s.lwWallet.IsSome():
		tx, err = s.lwWallet.UnsafeFromSome().BtcWallet.SendOutputs(
			nil,
			outputs,
			chainfee.FeePerKwFloor,
			1,
			label,
			base.CoinSelectionLargest,
		)

	case s.btcwWallet.IsSome():
		tx, err = s.btcwWallet.UnsafeFromSome().BtcWallet.SendOutputs(
			nil,
			outputs,
			chainfee.FeePerKwFloor,
			1,
			label,
			base.CoinSelectionLargest,
		)

	default:
		return "", fmt.Errorf("wallet backend is not initialized")
	}
	if err != nil {
		return "", fmt.Errorf("send outputs: %w", err)
	}

	txid := tx.TxHash()

	return txid.String(), nil
}
