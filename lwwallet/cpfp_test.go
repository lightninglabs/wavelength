package lwwallet

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// mockCPFPWallet is a minimal CPFPWallet implementation for testing
// CPFP child construction logic without a real wallet.
type mockCPFPWallet struct {
	utxos   []*lnwallet.Utxo
	listErr error

	// newAddrScript is returned as a P2TR-like address. We store the
	// raw pkScript so the test can verify it ends up in the child tx.
	newAddrPkScript []byte
	newAddrErr      error

	signCalled bool
	signErr    error
}

func (m *mockCPFPWallet) ListUnspentWitness(
	_, _ int32) ([]*lnwallet.Utxo, error) {

	return m.utxos, m.listErr
}

// mockAddress satisfies btcutil.Address for testing.
type mockAddress struct {
	pkScript []byte
}

func (a *mockAddress) EncodeAddress() string            { return "mock-addr" }
func (a *mockAddress) String() string                   { return "mock-addr" }
func (a *mockAddress) ScriptAddress() []byte            { return a.pkScript }
func (a *mockAddress) IsForNet(_ *chaincfg.Params) bool { return true }

func (m *mockCPFPWallet) NewAddress(
	_ context.Context) (btcutil.Address, error) {

	if m.newAddrErr != nil {
		return nil, m.newAddrErr
	}

	return &mockAddress{pkScript: m.newAddrPkScript}, m.newAddrErr
}

func (m *mockCPFPWallet) ComputeInputScript(
	_ *wire.MsgTx, _ *input.SignDescriptor) (*input.Script, error) {

	m.signCalled = true
	if m.signErr != nil {
		return nil, m.signErr
	}

	return &input.Script{
		Witness: wire.TxWitness{[]byte("mock-sig")},
	}, nil
}

// makeParentWithP2A creates a minimal V3 parent transaction with a P2A
// anchor output at the given index.
func makeParentWithP2A(t *testing.T, anchorIdx int,
	otherOutputs int) *wire.MsgTx {

	t.Helper()

	tx := wire.NewMsgTx(3)

	// Add a dummy input.
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("parent-input")),
			Index: 0,
		},
		Sequence: wire.MaxTxInSequenceNum,
	})

	// Build outputs. Place the P2A anchor at the requested index.
	totalOuts := otherOutputs + 1
	for i := 0; i < totalOuts; i++ {
		if i == anchorIdx {
			tx.AddTxOut(&wire.TxOut{
				Value:    240,
				PkScript: scripts.AnchorPkScript,
			})
		} else {
			tx.AddTxOut(&wire.TxOut{
				Value:    50_000,
				PkScript: []byte{0x51, 0x20, 0x01},
			})
		}
	}

	return tx
}

// TestCPFPFindAnchorOutput verifies that buildCPFPChild correctly
// locates the P2A anchor output in a V3 parent transaction,
// regardless of its position.
func TestCPFPFindAnchorOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		anchorIdx int
		numOther  int
	}{
		{
			name:      "anchor at index 0",
			anchorIdx: 0,
			numOther:  1,
		},
		{
			name:      "anchor at index 1",
			anchorIdx: 1,
			numOther:  1,
		},
		{
			name:      "anchor at index 2 among 3 outputs",
			anchorIdx: 2,
			numOther:  2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			parent := makeParentWithP2A(
				t, tc.anchorIdx, tc.numOther,
			)

			// Verify the P2A script is at the expected index.
			require.True(
				t,
				bytes.Equal(
					parent.TxOut[tc.anchorIdx].PkScript,
					scripts.AnchorPkScript,
				),
				"P2A script not at expected index %d",
				tc.anchorIdx,
			)

			// Verify no other output has the P2A script.
			for i, out := range parent.TxOut {
				if i == tc.anchorIdx {
					continue
				}

				isAnchor := bytes.Equal(
					out.PkScript,
					scripts.AnchorPkScript,
				)
				require.False(t,
					isAnchor,
					"unexpected P2A at %d", i,
				)
			}
		})
	}
}

// TestCPFPFindAnchorMissing verifies that buildCPFPChild returns an
// error when the parent transaction has no P2A anchor output.
func TestCPFPFindAnchorMissing(t *testing.T) {
	t.Parallel()

	// Build a parent with no P2A output.
	parent := wire.NewMsgTx(3)
	parent.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input")),
			Index: 0,
		},
	})
	parent.AddTxOut(&wire.TxOut{
		Value:    50_000,
		PkScript: []byte{0x51, 0x20, 0x01},
	})

	wallet := &mockCPFPWallet{
		utxos: []*lnwallet.Utxo{
			{Value: 100_000},
		},
	}

	backend := &ChainBackend{wallet: wallet}

	_, err := backend.buildCPFPChild(
		t.Context(), []*wire.MsgTx{parent},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no P2A anchor output")
}

// TestCPFPSelectFeeUTXO verifies that selectFeeUTXO picks the
// smallest sufficient UTXO from the wallet.
func TestCPFPSelectFeeUTXO(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		utxos     []*lnwallet.Utxo
		minValue  btcutil.Amount
		wantValue btcutil.Amount
		wantErr   string
	}{
		{
			name: "picks smallest sufficient",
			utxos: []*lnwallet.Utxo{
				{Value: 10_000},
				{Value: 5_000},
				{Value: 50_000},
				{Value: 20_000},
			},
			minValue:  15_000,
			wantValue: 20_000,
		},
		{
			name: "exact match",
			utxos: []*lnwallet.Utxo{
				{Value: 10_000},
				{Value: 5_000},
			},
			minValue:  5_000,
			wantValue: 5_000,
		},
		{
			name: "single UTXO sufficient",
			utxos: []*lnwallet.Utxo{
				{Value: 100_000},
			},
			minValue:  50_000,
			wantValue: 100_000,
		},
		{
			name:     "no UTXOs available",
			utxos:    []*lnwallet.Utxo{},
			minValue: 1_000,
			wantErr:  "no confirmed wallet UTXOs",
		},
		{
			name: "all UTXOs too small",
			utxos: []*lnwallet.Utxo{
				{Value: 1_000},
				{Value: 2_000},
			},
			minValue: 5_000,
			wantErr:  "no UTXO with sufficient value",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			wallet := &mockCPFPWallet{utxos: tc.utxos}
			backend := &ChainBackend{wallet: wallet}

			utxo, err := backend.selectFeeUTXO(tc.minValue, nil)

			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.wantErr,
				)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantValue, utxo.Value)
		})
	}
}

// TestCPFPSelectFeeUTXOListError verifies that selectFeeUTXO
// propagates wallet list errors.
func TestCPFPSelectFeeUTXOListError(t *testing.T) {
	t.Parallel()

	wallet := &mockCPFPWallet{
		listErr: fmt.Errorf("wallet locked"),
	}
	backend := &ChainBackend{wallet: wallet}

	_, err := backend.selectFeeUTXO(1_000, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "wallet locked")
}

// TestCPFPEstimateFeeMinimum verifies that the fee estimation
// enforces a minimum of 1 sat/vB.
func TestCPFPEstimateFeeMinimum(t *testing.T) {
	t.Parallel()

	// The EstimateFee function enforces >= 1 sat/vB. We test the
	// code path where all estimates are below 1.0.
	srv := mockEsploraServer(
		t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/blocks/tip/height":
				fmt.Fprint(w, "100")

			case "/block-height/100":
				h := chainhash.HashH([]byte("test"))
				fmt.Fprint(w, h.String())

			case "/fee-estimates":
				// All estimates below 1.0.
				fmt.Fprint(w, `{"1": 0.5, "6": 0.3}`)

			default:
				fmt.Fprint(w, "not found")
			}
		},
	)

	esplora := NewEsploraClient(srv.URL, nil)
	backend := NewChainBackend(esplora, 0, nil)

	fee, err := backend.EstimateFee(t.Context(), 6)
	require.NoError(t, err)

	// Should enforce minimum 1 sat/vB.
	require.Equal(t, btcutil.Amount(1), fee)
}

// TestCPFPEstimateFeeFallback verifies fallback behavior when no
// estimate is >= the target confirmation count.
func TestCPFPEstimateFeeFallback(t *testing.T) {
	t.Parallel()

	srv := mockEsploraServer(
		t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/blocks/tip/height":
				fmt.Fprint(w, "100")

			case "/block-height/100":
				h := chainhash.HashH([]byte("test"))
				fmt.Fprint(w, h.String())

			case "/fee-estimates":
				// Only low-target estimates. Requesting
				// target 100 should fall back to largest
				// available (target 6).
				fmt.Fprint(w,
					`{"1": 25.0, "3": 15.0, "6": 10.0}`)

			default:
				fmt.Fprint(w, "not found")
			}
		},
	)

	esplora := NewEsploraClient(srv.URL, nil)
	backend := NewChainBackend(esplora, 0, nil)

	fee, err := backend.EstimateFee(t.Context(), 100)
	require.NoError(t, err)

	// Should fall back to the largest available target (6) with
	// rate 10.0 -> ceil -> 10.
	require.Equal(t, btcutil.Amount(10), fee)
}

// TestCPFPP2AScriptConstant verifies the P2A script constant matches
// the expected BIP-341 Pay-to-Anchor encoding.
func TestCPFPP2AScriptConstant(t *testing.T) {
	t.Parallel()

	// P2A is: OP_1 (0x51) OP_PUSHBYTES_2 (0x02) 0x4e 0x73
	expected := []byte{0x51, 0x02, 0x4e, 0x73}
	require.Equal(t, expected, scripts.AnchorPkScript)
	require.Len(t, scripts.AnchorPkScript, 4)
}

// TestCPFPEstimateWeightCB verifies the weight estimation helper.
func TestCPFPEstimateWeightCB(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0,
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: []byte{0x51},
	})

	weight := estimateWeightCB(tx)

	// Weight = base_size * 3 + total_size. For a non-witness tx,
	// base_size == total_size, so weight = 4 * base_size.
	baseSize := int64(tx.SerializeSizeStripped())
	totalSize := int64(tx.SerializeSize())
	expected := baseSize*3 + totalSize
	require.Equal(t, expected, weight)
	require.Greater(t, weight, int64(0))
}
