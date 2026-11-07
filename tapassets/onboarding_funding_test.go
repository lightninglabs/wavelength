package tapassets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/walletcore"
	"github.com/stretchr/testify/require"
)

// fakeOnboardingWalletInput is the coin the fake funder spends.
var fakeOnboardingWalletInput = wire.OutPoint{
	Hash:  sha256Bytes([]byte("onboarding-wallet-input")),
	Index: 1,
}

// fakeOnboardingWalletScript marks the fake wallet's coin and change.
var fakeOnboardingWalletScript = append(
	[]byte{txscript.OP_1, txscript.OP_DATA_32},
	bytes.Repeat([]byte{0x77}, 32)...,
)

// isFakeOnboardingWalletInput reports whether an input spends the fake
// wallet's coin.
func isFakeOnboardingWalletInput(input *psbt.PInput) bool {
	return input.WitnessUtxo != nil && bytes.Equal(
		input.WitnessUtxo.PkScript, fakeOnboardingWalletScript,
	)
}

// fakeOnboardingFunder funds a template with one coin and change, putting the
// change first as a BIP 69 sort may.
type fakeOnboardingFunder struct {
	mu sync.Mutex

	inputValue   int64
	walletFeeSat int64
	estimate     uint64
	fundErr      error

	estimated   []uint32
	funds       int
	finalizes   int
	leaseCalls  int
	lastFeeRate int64
	templates   []*psbt.Packet
	leases      map[wire.OutPoint]walletcore.LockID
}

func newFakeOnboardingFunder() *fakeOnboardingFunder {
	return &fakeOnboardingFunder{
		inputValue:   10_000,
		walletFeeSat: 300,
		leases:       make(map[wire.OutPoint]walletcore.LockID),
	}
}

func (f *fakeOnboardingFunder) FeeRate(_ context.Context, targetConf uint32) (
	uint64, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.estimated = append(f.estimated, targetConf)

	return f.estimate, nil
}

func (f *fakeOnboardingFunder) FundTemplate(_ context.Context,
	packet *psbt.Packet, feeRate int64, lockID walletcore.LockID,
	_ time.Duration) (int32, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.funds++
	f.lastFeeRate = feeRate
	raw, err := psbtutil.Serialize(packet)
	if err != nil {
		return 0, err
	}
	template, err := psbtutil.Parse(raw)
	if err != nil {
		return 0, err
	}
	f.templates = append(f.templates, template)
	if f.fundErr != nil {
		return 0, f.fundErr
	}

	var outputs int64
	for _, txOut := range packet.UnsignedTx.TxOut {
		outputs += txOut.Value
	}
	change := f.inputValue - outputs - f.walletFeeSat
	if change < 0 {
		return 0, errors.New("insufficient wallet funds")
	}

	packet.UnsignedTx.AddTxIn(
		wire.NewTxIn(&fakeOnboardingWalletInput, nil, nil),
	)
	packet.Inputs = append(packet.Inputs, psbt.PInput{
		WitnessUtxo: &wire.TxOut{
			Value:    f.inputValue,
			PkScript: fakeOnboardingWalletScript,
		},
	})
	packet.UnsignedTx.TxOut = append([]*wire.TxOut{{
		Value:    change,
		PkScript: fakeOnboardingWalletScript,
	}}, packet.UnsignedTx.TxOut...)
	packet.Outputs = append([]psbt.POutput{{}}, packet.Outputs...)
	f.leases[fakeOnboardingWalletInput] = lockID

	return 0, nil
}

func (f *fakeOnboardingFunder) FinalizePsbt(_ context.Context, raw []byte) (
	*wire.MsgTx, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalizes++
	packet, err := psbtutil.Parse(raw)
	if err != nil {
		return nil, err
	}
	for idx := range packet.Inputs {
		input := &packet.Inputs[idx]
		switch {
		case isFakeOnboardingWalletInput(input):
			input.FinalScriptWitness = testWalletWitness

		case len(input.FinalScriptWitness) == 0:
			return nil, fmt.Errorf("input %d is not final", idx)
		}
	}

	return psbt.Extract(packet)
}

func (f *fakeOnboardingFunder) LeaseOutput(_ context.Context,
	id walletcore.LockID, op wire.OutPoint, expiry time.Duration) (
	time.Time, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaseCalls++
	f.leases[op] = id

	return time.Now().Add(expiry), nil
}

func (f *fakeOnboardingFunder) ReleaseOutput(_ context.Context,
	id walletcore.LockID, op wire.OutPoint) error {

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leases[op] != id {
		return fmt.Errorf("%s is not leased under this lock", op)
	}
	delete(f.leases, op)

	return nil
}

// testFunder returns the fake funder of an onboarder built by testOnboarder.
func testFunder(t *testing.T, onboarder *Onboarder) *fakeOnboardingFunder {
	t.Helper()

	funder, ok := onboarder.funder.(*fakeOnboardingFunder)
	require.True(t, ok)

	return funder
}

// TestOnboardingFundedTemplateLayout checks what the wallet funds and how the
// funded transaction is laid out for tapd.
func TestOnboardingFundedTemplateLayout(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testMultiUtxoOnboardingRequest(t, 40)
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(driver, inventory, store, owner)
	funder := testFunder(t, onboarder)

	_, err = onboarder.Onboard(t.Context(), request)
	require.NoError(t, err)

	// The two 1,000 sat anchors cover all but the fee for their own 460
	// weight units of the boarding output, so the wallet funds the rest.
	require.Len(t, funder.templates, 1)
	template := funder.templates[0].UnsignedTx
	require.Empty(t, template.TxIn)
	require.Len(t, template.TxOut, 2)
	require.EqualValues(t, 4_000-2_000+230, template.TxOut[0].Value)
	require.Equal(t, onboardingChangeValueSat, template.TxOut[1].Value)

	funded, err := psbtutil.Parse(driver.requests[0].AnchorPSBT)
	require.NoError(t, err)
	tx := funded.UnsignedTx
	require.Len(t, tx.TxIn, 3)
	for idx, proof := range sortedProofFiles(request) {
		tip := inventory.verifications[string(proof)].DecodedProof
		require.Equal(
			t, tip.Outpoint, sdkOutpoint(
				tx.TxIn[idx].PreviousOutPoint,
			),
		)
	}
	require.Equal(t, fakeOnboardingWalletInput, tx.TxIn[2].PreviousOutPoint)

	require.Len(t, tx.TxOut, 3)
	require.EqualValues(t, request.CarrierValueSat, tx.TxOut[0].Value)
	require.Equal(t, onboardingChangeValueSat, tx.TxOut[1].Value)
	require.Equal(t, fakeOnboardingWalletScript, tx.TxOut[2].PkScript)

	// The wallet's own fee plus the anchors' share.
	fee := funder.inputValue + 2_000
	for _, txOut := range tx.TxOut {
		fee -= txOut.Value
	}
	require.EqualValues(t, funder.walletFeeSat+230, fee)
}

// TestOnboarderReleasesRefusedFunding frees the wallet's coins whenever a
// funding is refused before tapd sees it.
func TestOnboarderReleasesRefusedFunding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		configure   func(*OnboardingRequest, *fakeInventory)
		funds       int
		errContains string
	}{{
		name: "fee above maximum",
		configure: func(request *OnboardingRequest, _ *fakeInventory) {
			request.MaxFeeSat = 100
		},
		funds:       1,
		errContains: "onboarding fee 415 exceeds maximum 100",
	}, {
		name: "anchors exceed the output",
		configure: func(_ *OnboardingRequest,
			inventory *fakeInventory) {

			inventory.onlyAnchor().AmtSat = 10_000
		},
		errContains: "more than the onboarding output needs",
	}}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			request, inventory, owner := testOnboardingRequest(t)
			test.configure(request, inventory)
			driver := newFakeOnboardingDriver()
			store, err := NewFileStore(t.TempDir())
			require.NoError(t, err)
			onboarder := testOnboarder(
				driver, inventory, store, owner,
			)
			funder := testFunder(t, onboarder)

			_, err = onboarder.Onboard(t.Context(), request)
			require.ErrorContains(t, err, test.errContains)
			require.Equal(t, test.funds, funder.funds)
			require.Empty(t, funder.leases)
			require.Zero(t, driver.commits)

			state, err := onboarder.loadState(
				t.Context(), request,
				onboardingRequestDigest(request),
			)
			require.NoError(t, err)
			require.Empty(t, state.FundedPSBT)
		})
	}
}

// TestOnboarderReusesFundedTemplate spends the journaled coins again after a
// retry, renewing their leases instead of funding twice.
func TestOnboarderReusesFundedTemplate(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testOnboardingRequest(t)
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(
		newFakeOnboardingDriver(), inventory, store, owner,
	)
	funder := testFunder(t, onboarder)
	state, err := onboarder.loadState(
		t.Context(), request, onboardingRequestDigest(request),
	)
	require.NoError(t, err)
	anchors := []*tapsdk.ManagedUtxo{inventory.onlyAnchor()}
	values := []int64{int64(request.CarrierValueSat)}

	first, err := onboarder.fundedTemplate(
		t.Context(), request, state, anchors, values,
	)
	require.NoError(t, err)
	stored, err := onboarder.loadState(
		t.Context(), request, onboardingRequestDigest(request),
	)
	require.NoError(t, err)
	require.NotEmpty(t, stored.FundedPSBT)

	second, err := onboarder.fundedTemplate(
		t.Context(), request, stored, anchors, values,
	)
	require.NoError(t, err)
	require.Equal(t, first.UnsignedTx.TxHash(), second.UnsignedTx.TxHash())
	require.Equal(t, 1, funder.funds)
	require.Equal(t, 1, funder.leaseCalls)

	// A journaled template only serves the anchors it was funded for.
	other := *anchors[0]
	other.OutPoint.Index++
	_, err = onboarder.fundedTemplate(
		t.Context(), request, stored, []*tapsdk.ManagedUtxo{&other},
		values,
	)
	require.ErrorContains(t, err, "is not anchor")
}

// TestOnboarderFundingFailure keeps a wallet that cannot fund from reaching
// tapd or the journal.
func TestOnboarderFundingFailure(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testOnboardingRequest(t)
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(driver, inventory, store, owner)
	funder := testFunder(t, onboarder)
	funder.fundErr = errors.New("insufficient funds")

	_, err = onboarder.Onboard(t.Context(), request)
	require.ErrorContains(t, err, "fund onboarding: insufficient funds")
	require.Zero(t, driver.commits)

	funder.fundErr = nil
	_, err = onboarder.Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, 2, funder.funds)
}
