package lib

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

type mockUTXO struct {
	output *wire.TxOut
	spent  bool
	confs  int64
}

func TestBoardingValidator(t *testing.T) {
	operatorPriv, _ := btcec.NewPrivateKey()
	operatorPub := operatorPriv.PubKey()
	clientPriv, _ := btcec.NewPrivateKey()
	clientPub := clientPriv.PubKey()
	wrongPriv, _ := btcec.NewPrivateKey()
	wrongPub := wrongPriv.PubKey()

	outpoint := &wire.OutPoint{Hash: [32]byte{1, 2, 3, 4, 5}, Index: 0}

	// Build expected pkScript
	tapscript, _ := BoardingTapScript(clientPub, operatorPub, 144)
	outputKey, _ := tapscript.TaprootKey()
	expectedPkScript := make([]byte, 34)
	expectedPkScript[0] = 0x51
	expectedPkScript[1] = 0x20
	copy(expectedPkScript[2:], outputKey.SerializeCompressed()[1:33])

	tests := []struct {
		name      string
		req       *BoardingRequest
		utxo      *mockUTXO
		wantError string
	}{
		{
			name: "valid request",
			req: &BoardingRequest{
				Outpoint:    outpoint,
				ClientKey:   clientPub,
				OperatorKey: operatorPub,
				ExitDelay:   144,
			},
			utxo: &mockUTXO{
				output: &wire.TxOut{
					Value:    100000,
					PkScript: expectedPkScript,
				},
				spent: false,
				confs: 10,
			},
		},
		{
			name: "wrong operator key",
			req: &BoardingRequest{
				Outpoint:    outpoint,
				ClientKey:   clientPub,
				OperatorKey: wrongPub,
				ExitDelay:   144,
			},
			wantError: "invalid operator key",
		},
		{
			name: "exit delay too low",
			req: &BoardingRequest{
				Outpoint:    outpoint,
				ClientKey:   clientPub,
				OperatorKey: operatorPub,
				ExitDelay:   100,
			},
			wantError: "boarding request exit delay 100 is less than required minimum 144",
		},
		{
			name: "UTXO already spent",
			req: &BoardingRequest{
				Outpoint:    outpoint,
				ClientKey:   clientPub,
				OperatorKey: operatorPub,
				ExitDelay:   144,
			},
			utxo: &mockUTXO{
				output: &wire.TxOut{Value: 100000},
				spent:  true,
				confs:  10,
			},
			wantError: "already spent",
		},
		{
			name: "insufficient confirmations",
			req: &BoardingRequest{
				Outpoint:    outpoint,
				ClientKey:   clientPub,
				OperatorKey: operatorPub,
				ExitDelay:   144,
			},
			utxo: &mockUTXO{
				output: &wire.TxOut{Value: 100000},
				spent:  false,
				confs:  2,
			},
			wantError: "has only 2 confirmations, requires at least 6",
		},
		{
			name: "amount below minimum",
			req: &BoardingRequest{
				Outpoint:    outpoint,
				ClientKey:   clientPub,
				OperatorKey: operatorPub,
				ExitDelay:   144,
			},
			utxo: &mockUTXO{
				output: &wire.TxOut{Value: 30000}, // below 50000 minimum
				spent:  false,
				confs:  10,
			},
			wantError: "has value 30000, below minimum 50000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getUTXO := func(op *wire.OutPoint) (*wire.TxOut, bool,
				int64, error) {

				if tt.utxo == nil {
					return nil, false, 0, nil
				}

				return tt.utxo.output, tt.utxo.spent,
					tt.utxo.confs, nil
			}

			validator := NewBoardingValidator(
				&keychain.KeyDescriptor{
					PubKey: operatorPub,
				},
				144,
				btcutil.Amount(50000), // min amount
				6,
				getUTXO,
			)

			result, err := validator.ValidateRequest(tt.req)

			if tt.wantError != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantError)
				return
			}

			require.NoError(t, err)
			require.Equal(t, outpoint, result.Outpoint)
			require.Equal(t, clientPub, result.ClientKey)
			require.Equal(t, btcutil.Amount(100000), result.Value)
			require.True(t, bytes.Equal(expectedPkScript, result.PkScript))
		})
	}
}
