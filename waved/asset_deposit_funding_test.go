package waved

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/walletcore"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// fakeDepositWallet lists a fixed coin set, records leases, and fails the
// lease of one outpoint when asked to.
type fakeDepositWallet struct {
	utxos    []*wallet.Utxo
	leases   map[wire.OutPoint]walletcore.LockID
	failOn   *wire.OutPoint
	funded   map[wire.OutPoint]walletcore.LockID
	released []wire.OutPoint
}

func (w *fakeDepositWallet) ListUnspent(context.Context, int32, int32) (
	[]*wallet.Utxo, error) {

	return w.utxos, nil
}

func (w *fakeDepositWallet) FundTemplate(_ context.Context, _ *psbt.Packet,
	_ chainfee.SatPerKWeight, _ wallet.LockID, _ time.Duration) (int32,
	error) {

	w.funded = make(map[wire.OutPoint]walletcore.LockID, len(w.leases))
	for op, id := range w.leases {
		w.funded[op] = id
	}

	return -1, nil
}

func (w *fakeDepositWallet) SignInputs(context.Context, *psbt.Packet) error {
	return errors.New("not used")
}

func (w *fakeDepositWallet) LeaseOutput(_ context.Context, id walletcore.LockID,
	op wire.OutPoint, expiry time.Duration) (time.Time, error) {

	if w.failOn != nil && *w.failOn == op {
		return time.Time{}, errors.New("output already leased")
	}
	w.leases[op] = id

	return time.Now().Add(expiry), nil
}

func (w *fakeDepositWallet) ReleaseOutput(_ context.Context,
	id walletcore.LockID, op wire.OutPoint) error {

	if w.leases[op] != id {
		return errors.New("lease mismatch")
	}
	delete(w.leases, op)
	w.released = append(w.released, op)

	return nil
}

// testCoin returns a confirmed coin paying pkScript.
func testCoin(index uint32, pkScript []byte) *wallet.Utxo {
	return &wallet.Utxo{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x51,
			},
			Index: index,
		},
		PkScript:      pkScript,
		Amount:        btcutil.Amount(10_000),
		Confirmations: 6,
	}
}

// TestDepositFunderExcludesNestedCoins keeps nested SegWit coins out of a
// deposit's coin selection and frees them afterwards.
func TestDepositFunderExcludesNestedCoins(t *testing.T) {
	t.Parallel()

	taproot := testCoin(
		0,
		append(
			[]byte{txscript.OP_1, txscript.OP_DATA_32},
			bytes.Repeat([]byte{0x01}, 32)...,
		),
	)
	nested := testCoin(
		1,
		append(
			[]byte{txscript.OP_HASH160, txscript.OP_DATA_20},
			append(
				bytes.Repeat(
					[]byte{0x02}, 20,
				),
				txscript.OP_EQUAL,
			)...,
		),
	)
	w := &fakeDepositWallet{
		utxos: []*wallet.Utxo{
			taproot,
			nested,
		},
		leases: make(map[wire.OutPoint]walletcore.LockID),
	}
	funder := &depositFunder{
		wallet: func() (depositWallet, error) {
			return w, nil
		},
	}

	_, err := funder.FundTemplate(
		t.Context(), &psbt.Packet{}, 500, walletcore.LockID{1},
		time.Hour,
	)
	require.NoError(t, err)
	require.Equal(
		t, map[wire.OutPoint]walletcore.LockID{
			nested.Outpoint: depositExclusionLockID,
		}, w.funded,
	)
	require.Empty(t, w.leases)
	require.Equal(t, []wire.OutPoint{nested.Outpoint}, w.released)
}

// TestLeaseFundedInputsRollsBack releases the inputs a funding leased before
// one of its leases failed.
func TestLeaseFundedInputsRollsBack(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	for idx := range uint32(3) {
		tx.AddTxIn(
			wire.NewTxIn(
				&wire.OutPoint{
					Hash:  chainhash.Hash{0x52},
					Index: idx,
				},
				nil,
				nil,
			),
		)
	}
	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	failing := tx.TxIn[2].PreviousOutPoint
	w := &fakeDepositWallet{
		leases: make(map[wire.OutPoint]walletcore.LockID),
		failOn: &failing,
	}
	err = leaseFundedInputs(
		t.Context(), w, packet, walletcore.LockID{2}, time.Hour,
	)
	require.ErrorContains(t, err, "lease funded input")
	require.Empty(t, w.leases)
	require.ElementsMatch(
		t, []wire.OutPoint{
			tx.TxIn[0].PreviousOutPoint,
			tx.TxIn[1].PreviousOutPoint,
		}, w.released,
	)
}

// fakeFeeBackend estimates in whole sat/vB only.
type fakeFeeBackend struct {
	chainsource.ChainBackend

	satPerVByte btcutil.Amount
}

func (b *fakeFeeBackend) EstimateFee(context.Context, uint32) (btcutil.Amount,
	error) {

	return b.satPerVByte, nil
}

// fakePerKWBackend adds a full-precision estimate to fakeFeeBackend.
type fakePerKWBackend struct {
	fakeFeeBackend

	perKW chainfee.SatPerKWeight
}

func (b *fakePerKWBackend) EstimateFeePerKW(context.Context, uint32) (
	chainfee.SatPerKWeight, error) {

	return b.perKW, nil
}

// TestEstimateDepositFee keeps a backend's sub-sat/vB precision and converts
// a whole sat/vB estimate otherwise.
func TestEstimateDepositFee(t *testing.T) {
	t.Parallel()

	server := &Server{chainBackend: &fakePerKWBackend{
		fakeFeeBackend: fakeFeeBackend{
			satPerVByte: 1,
		},
		perKW: 499,
	}}
	rate, err := server.estimateDepositFee(t.Context(), 6)
	require.NoError(t, err)
	require.Equal(t, chainfee.SatPerKWeight(499), rate)

	server.chainBackend = &fakeFeeBackend{satPerVByte: 3}
	rate, err = server.estimateDepositFee(t.Context(), 6)
	require.NoError(t, err)
	require.Equal(t, chainfee.SatPerKWeight(750), rate)
}
