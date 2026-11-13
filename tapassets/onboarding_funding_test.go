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
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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

// fakeOnboardingWalletKey is the derivation key of the fake wallet's coin.
var fakeOnboardingWalletKey = append(
	[]byte{0x02}, bytes.Repeat([]byte{0x77}, 32)...,
)

// isFakeOnboardingWalletInput reports whether an input spends the fake
// wallet's coin.
func isFakeOnboardingWalletInput(input *psbt.PInput) bool {
	return input.WitnessUtxo != nil && bytes.Equal(
		input.WitnessUtxo.PkScript, fakeOnboardingWalletScript,
	)
}

// fakeOnboardingFunder funds a template with one coin and change, putting the
// change first as a BIP 69 sort may. It signs every input carrying derivation
// info, as a wallet sharing tapd's keys would. Like a wallet RPC, its calls
// fail once the caller's context ends.
type fakeOnboardingFunder struct {
	mu sync.Mutex

	inputValue   int64
	inputScript  []byte
	walletFeeSat int64
	estimate     chainfee.SatPerKWeight
	fundErr      error
	releaseErr   error
	signErr      error
	skipSign     bool
	afterFund    func()
	afterSign    func()

	estimated   []uint32
	funds       int
	signs       int
	leaseCalls  int
	lastFeeRate chainfee.SatPerKWeight
	templates   []*psbt.Packet
	leases      map[wire.OutPoint]walletcore.LockID
}

func newFakeOnboardingFunder() *fakeOnboardingFunder {
	return &fakeOnboardingFunder{
		inputValue:   10_000,
		inputScript:  fakeOnboardingWalletScript,
		walletFeeSat: 300,
		leases:       make(map[wire.OutPoint]walletcore.LockID),
	}
}

func (f *fakeOnboardingFunder) FeeRate(_ context.Context, targetConf uint32) (
	chainfee.SatPerKWeight, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.estimated = append(f.estimated, targetConf)

	return f.estimate, nil
}

func (f *fakeOnboardingFunder) FundTemplate(ctx context.Context,
	packet *psbt.Packet, feeRate chainfee.SatPerKWeight,
	lockID walletcore.LockID, _ time.Duration) (int32, error) {

	if f.afterFund != nil {
		defer f.afterFund()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

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
			PkScript: f.inputScript,
		},
		Bip32Derivation: []*psbt.Bip32Derivation{{
			PubKey: fakeOnboardingWalletKey,
		}},
	})
	packet.UnsignedTx.TxOut = append([]*wire.TxOut{{
		Value:    change,
		PkScript: fakeOnboardingWalletScript,
	}}, packet.UnsignedTx.TxOut...)
	packet.Outputs = append([]psbt.POutput{{}}, packet.Outputs...)
	f.leases[fakeOnboardingWalletInput] = lockID

	return 0, nil
}

func (f *fakeOnboardingFunder) SignInputs(ctx context.Context,
	packet *psbt.Packet) error {

	if f.afterSign != nil {
		defer f.afterSign()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.signs++
	if f.signErr != nil || f.skipSign {
		return f.signErr
	}
	for idx := range packet.Inputs {
		input := &packet.Inputs[idx]
		if len(input.FinalScriptWitness) == 0 &&
			len(input.Bip32Derivation) != 0 {

			input.FinalScriptWitness = testWalletWitness
		}
	}

	return nil
}

func (f *fakeOnboardingFunder) LeaseOutput(ctx context.Context,
	id walletcore.LockID, op wire.OutPoint, expiry time.Duration) (
	time.Time, error) {

	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaseCalls++
	f.leases[op] = id

	return time.Now().Add(expiry), nil
}

func (f *fakeOnboardingFunder) ReleaseOutput(ctx context.Context,
	id walletcore.LockID, op wire.OutPoint) error {

	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.releaseErr != nil {
		return f.releaseErr
	}
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
		configure   func(*OnboardingRequest, *fakeOnboardingFunder)
		errContains string
	}{{
		name: "fee above maximum",
		configure: func(request *OnboardingRequest,
			_ *fakeOnboardingFunder) {

			request.MaxFeeSat = 100
		},
		errContains: "onboarding fee 415 exceeds maximum 100",
	}, {
		name: "nested SegWit input",
		configure: func(_ *OnboardingRequest,
			funder *fakeOnboardingFunder) {

			funder.inputScript = append(
				[]byte{
					txscript.OP_HASH160,
					txscript.OP_DATA_20,
				},
				append(
					bytes.Repeat(
						[]byte{0x55}, 20,
					),
					txscript.OP_EQUAL,
				)...,
			)
		},
		errContains: "funder input 0 is not native SegWit",
	}}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			request, inventory, owner := testOnboardingRequest(t)
			driver := newFakeOnboardingDriver()
			store, err := NewFileStore(t.TempDir())
			require.NoError(t, err)
			onboarder := testOnboarder(
				driver, inventory, store, owner,
			)
			funder := testFunder(t, onboarder)
			test.configure(request, funder)

			_, err = onboarder.Onboard(t.Context(), request)
			require.ErrorContains(t, err, test.errContains)
			require.Equal(t, 1, funder.funds)
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

// TestOnboardingSpreadsAnchorCredit takes anchors worth more than the boarding
// output off both asset outputs, and returns what is left through the wallet's
// change.
func TestOnboardingSpreadsAnchorCredit(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testMultiUtxoOnboardingRequest(t, 40)
	for _, utxo := range inventory.utxos {
		utxo.AmtSat = 2_500
	}
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(driver, inventory, store, owner)
	funder := testFunder(t, onboarder)

	_, err = onboarder.Onboard(t.Context(), request)
	require.NoError(t, err)

	// 5,000 sat of anchors less their 230 sat fee bring both outputs to
	// dust, leaving 430 sat over.
	template := funder.templates[0].UnsignedTx
	require.EqualValues(t, 330, template.TxOut[0].Value)
	require.EqualValues(t, 330, template.TxOut[1].Value)

	funded, err := psbtutil.Parse(driver.requests[0].AnchorPSBT)
	require.NoError(t, err)
	tx := funded.UnsignedTx
	require.Len(t, tx.TxOut, 3)
	require.EqualValues(t, request.CarrierValueSat, tx.TxOut[0].Value)
	require.Equal(t, onboardingChangeValueSat, tx.TxOut[1].Value)
	require.EqualValues(
		t, funder.inputValue-660-funder.walletFeeSat+430,
		tx.TxOut[2].Value,
	)

	fee := funder.inputValue + 5_000
	for _, txOut := range tx.TxOut {
		fee -= txOut.Value
	}
	require.EqualValues(t, funder.walletFeeSat+230, fee)
}

// TestOnboarderRenewsSealedLeases keeps the wallet's coins reserved for a
// sealed package until it publishes.
func TestOnboarderRenewsSealedLeases(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testOnboardingRequest(t)
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(driver, inventory, store, owner)
	funder := testFunder(t, onboarder)
	funder.signErr = errors.New("wallet locked")

	_, err = onboarder.Onboard(t.Context(), request)
	require.ErrorContains(t, err, "wallet locked")
	require.Equal(t, 1, driver.commits)
	require.Zero(t, funder.leaseCalls)

	funder.signErr = nil
	_, err = onboarder.Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, 1, funder.funds)
	require.Equal(t, 1, funder.leaseCalls)
	require.Equal(
		t,
		onboardingLockID(
			onboardingRequestDigest(request),
		),
		funder.leases[fakeOnboardingWalletInput],
	)

	// A published package spent the coins, so nothing is renewed.
	_, err = onboarder.Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, 1, funder.leaseCalls)
}

// TestOnboarderSignsOnlyFunderInputs has the funder sign its own inputs from
// their journaled derivation info, while tapd's anchors reach publication
// unsigned even though the funder could derive their keys.
func TestOnboarderSignsOnlyFunderInputs(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testMultiUtxoOnboardingRequest(t, 40)
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(driver, inventory, store, owner)
	funder := testFunder(t, onboarder)

	_, err = onboarder.Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, 1, funder.signs)

	state, err := onboarder.loadState(
		t.Context(), request, onboardingRequestDigest(request),
	)
	require.NoError(t, err)
	final, err := psbtutil.Parse(state.FinalAnchorPSBT)
	require.NoError(t, err)
	require.Len(t, final.Inputs, 3)
	for idx := range 2 {
		require.Empty(t, final.Inputs[idx].FinalScriptWitness)
	}
	require.Equal(t, testWalletWitness, final.Inputs[2].FinalScriptWitness)
}

// TestOnboarderKeepsFundingAfterCancel journals the funded coins even when the
// caller gives up right after the wallet funds, so a retry spends them.
func TestOnboarderKeepsFundingAfterCancel(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testOnboardingRequest(t)
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(driver, inventory, store, owner)
	funder := testFunder(t, onboarder)
	ctx, cancel := context.WithCancel(t.Context())
	funder.afterFund = cancel

	_, err = onboarder.Onboard(ctx, request)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, driver.commits)
	require.Len(t, funder.leases, 1)

	state, err := onboarder.loadState(
		t.Context(), request, onboardingRequestDigest(request),
	)
	require.NoError(t, err)
	require.NotEmpty(t, state.FundedPSBT)

	funder.afterFund = nil
	_, err = onboarder.Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, 1, funder.funds)
	require.Equal(t, 1, funder.leaseCalls)
}

// TestOnboarderKeepsCoinsWhenReleaseFails reuses the journaled coins after a
// refused commit whose release failed, rather than funding a second set.
func TestOnboarderKeepsCoinsWhenReleaseFails(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testOnboardingRequest(t)
	driver := newFakeOnboardingDriver()
	driver.commitErr = errors.New("commit rejected")
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(driver, inventory, store, owner)
	funder := testFunder(t, onboarder)
	funder.releaseErr = errors.New("wallet unavailable")

	_, err = onboarder.Onboard(t.Context(), request)
	require.ErrorContains(t, err, "commit rejected")
	require.ErrorContains(t, err, "wallet unavailable")

	state, err := onboarder.loadState(
		t.Context(), request, onboardingRequestDigest(request),
	)
	require.NoError(t, err)
	require.NotEmpty(t, state.FundedPSBT)

	driver.commitErr = nil
	funder.releaseErr = nil
	_, err = onboarder.Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, 1, funder.funds)
	require.Equal(t, 2, driver.commits)
}
