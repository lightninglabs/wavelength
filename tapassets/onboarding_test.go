package tapassets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestOnboarderResumesWithoutRebuilding proves onboarding crosses its
// external side effects in order — commit, sign, publish — and that a
// restart mid-flight never rebuilds, resigns, or republishes any of them.
func TestOnboarderResumesWithoutRebuilding(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testOnboardingRequest(t)
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	var deriveCalls, signCalls int
	newOnboarder := func() *Onboarder {
		return &Onboarder{
			driver:    driver,
			inventory: inventory,
			keys:      inventory,
			store:     store,
			signer: func(_ context.Context, anchor []byte) ([]byte,
				error) {

				signCalls++
				packet, err := psbtutil.Parse(anchor)
				require.NoError(t, err)
				require.Len(t, packet.UnsignedTx.TxIn, 2)

				return append([]byte(nil), anchor...), nil
			},
			deriveOwnerKey: func(context.Context) (
				*keychain.KeyDescriptor, error) {

				deriveCalls++
				key := owner

				return &key, nil
			},
		}
	}

	result, err := newOnboarder().Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, 1, deriveCalls)
	require.Equal(t, 1, signCalls)
	require.Equal(t, 1, driver.commits)
	require.Equal(t, 1, driver.verifications)
	require.Equal(t, 1, driver.publishes)
	require.Equal(t, int64(1_000), result.ValueSat)
	require.Equal(t, uint64(250), result.ActualFeeSat)
	require.NotZero(t, result.TaprootAssetRoot)
	require.Equal(t, request.AssetRef, result.AssetRef)
	require.Equal(t, request.AssetAmount, result.AssetAmount)
	require.NotEmpty(t, result.PolicyTemplate)
	require.NotEmpty(t, result.PkScript)

	// A fully committed journal must not need tapd to resume.
	inventory.err = errors.New("tapd unavailable")
	result, err = newOnboarder().Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, uint64(250), result.ActualFeeSat)
	require.Equal(t, 1, deriveCalls)
	require.Equal(t, 1, signCalls)
	require.Equal(t, 1, driver.commits)
	require.Equal(t, 2, driver.verifications)
	require.Equal(t, 1, driver.publishes)

	// Another retry is a local read and validation only.
	result, err = newOnboarder().Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, uint64(250), result.ActualFeeSat)
	require.Equal(t, 1, deriveCalls)
	require.Equal(t, 1, signCalls)
	require.Equal(t, 1, driver.commits)
	require.Equal(t, 3, driver.verifications)
	require.Equal(t, 1, driver.publishes)

	dto := driver.requests[0]
	require.Equal(
		t, tapsdk.CustomAnchorFundingWalletFunded, dto.Funding.Mode,
	)
	require.NotNil(t, dto.Funding.WalletFunded)
	require.Equal(
		t, tapsdk.AnchorChangeOutputAdd,
		dto.Funding.WalletFunded.ChangeOutput.Mode,
	)
	require.Equal(
		t, tapsdk.AnchorFeeSatPerVByte,
		dto.Funding.WalletFunded.Fee.Mode,
	)
	require.Equal(
		t, request.FeeRateSatPerVByte,
		dto.Funding.WalletFunded.Fee.FeeRate.SatPerVByteFloor(),
	)
	require.Equal(t, request.MaxFeeSat, dto.Funding.WalletFunded.MaxFeeSat)
	require.Equal(
		t,
		onboardingCustomLockID(
			onboardingRequestDigest(request),
		),
		dto.Funding.WalletFunded.CustomLockID,
	)
	require.Len(t, dto.Funding.WalletFunded.CustomLockID, sha256.Size)
	require.Equal(
		t, tapsdk.CustomAnchorPassiveReject, dto.PassiveAssets.Policy,
	)
	require.Equal(t, tapsdk.CustomAnchorLossReject, dto.LossPolicy.Mode)
	require.Equal(
		t, tapsdk.CustomAssetWitnessBackendSigner,
		dto.Inputs[0].Witness.Mode,
	)
	// The boarding output is anyone-can-spend at the asset layer so the
	// round's operator can build the transition that consumes it.
	require.Equal(
		t, tapsdk.CustomAssetScriptOPTrue, dto.Outputs[0].Script.Mode,
	)
	require.NotNil(t, dto.Outputs[0].Script.OPTrue)
	require.Equal(t, uint64(1_000), dto.Outputs[0].AnchorValueSat)
	require.Len(t, dto.Outputs[0].Anchor.Tapscript.TapLeaves, 2)
	committed, err := psbtutil.Parse(driver.result.anchorPSBT)
	require.NoError(t, err)
	require.Len(t, committed.UnsignedTx.TxOut, 2)
	assetOutputIndex := driver.result.outputs[0].anchorOutputIndex
	require.Equal(
		t, int64(request.CarrierValueSat),
		committed.UnsignedTx.TxOut[assetOutputIndex].Value,
	)
}

// TestOnboardingFeeSelection binds the public whole-sat fee selectors to the
// tagged tap-sdk fee policy without silently preferring one selector.
func TestOnboardingFeeSelection(t *testing.T) {
	t.Parallel()

	request, _, _ := testOnboardingRequest(t)
	fee, err := onboardingAnchorFee(request)
	require.NoError(t, err)
	require.Equal(t, tapsdk.AnchorFeeSatPerVByte, fee.Mode)
	require.Equal(
		t, request.FeeRateSatPerVByte, fee.FeeRate.SatPerVByteFloor(),
	)

	request.FeeRateSatPerVByte = 0
	request.TargetConf = 6
	fee, err = onboardingAnchorFee(request)
	require.NoError(t, err)
	require.Equal(t, tapsdk.AnchorFeeTargetConf, fee.Mode)
	require.Equal(t, uint32(6), fee.TargetConf)
}

// TestOnboardingRejectsInvalidEconomics verifies malformed wallet-funding
// authority is rejected before the durable workflow derives or stores state.
func TestOnboardingRejectsInvalidEconomics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mutate      func(*OnboardingRequest)
		errContains string
	}{
		{
			name: "carrier missing",
			mutate: func(request *OnboardingRequest) {
				request.CarrierValueSat = 0
			},
			errContains: "carrier value is required",
		},
		{
			name: "carrier overflows signed output",
			mutate: func(request *OnboardingRequest) {
				request.CarrierValueSat = math.MaxUint64
			},
			errContains: "carrier value",
		},
		{
			name: "carrier below dust",
			mutate: func(request *OnboardingRequest) {
				request.CarrierValueSat =
					onboardingDustFloorSat - 1
			},
			errContains: "below the Taproot dust floor",
		},
		{
			name: "fee selector missing",
			mutate: func(request *OnboardingRequest) {
				request.FeeRateSatPerVByte = 0
			},
			errContains: "exactly one",
		},
		{
			name: "fee selectors conflict",
			mutate: func(request *OnboardingRequest) {
				request.TargetConf = 6
			},
			errContains: "exactly one",
		},
		{
			name: "fee rate overflows",
			mutate: func(request *OnboardingRequest) {
				request.FeeRateSatPerVByte = math.MaxUint64
			},
			errContains: "fee rate",
		},
		{
			name: "maximum fee missing",
			mutate: func(request *OnboardingRequest) {
				request.MaxFeeSat = 0
			},
			errContains: "maximum fee is required",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			request, _, _ := testOnboardingRequest(t)
			test.mutate(request)
			err := validateOnboardingRequest(request)
			require.ErrorContains(t, err, test.errContains)
		})
	}
}

// TestOnboardingCustomLockID proves retry identity is stable while every
// carrier-funding economic choice remains part of that identity.
func TestOnboardingCustomLockID(t *testing.T) {
	t.Parallel()

	request, _, _ := testOnboardingRequest(t)
	lockID := onboardingCustomLockID(onboardingRequestDigest(request))
	require.Len(t, lockID, sha256.Size)
	require.Equal(
		t, lockID,
		onboardingCustomLockID(
			onboardingRequestDigest(request),
		),
	)

	mutations := []func(*OnboardingRequest){
		func(request *OnboardingRequest) {
			request.CarrierValueSat++
		},
		func(request *OnboardingRequest) {
			request.FeeRateSatPerVByte++
		},
		func(request *OnboardingRequest) {
			request.FeeRateSatPerVByte = 0
			request.TargetConf = 6
		},
		func(request *OnboardingRequest) {
			request.MaxFeeSat++
		},
	}
	for _, mutate := range mutations {
		changed := *request
		mutate(&changed)
		require.NotEqual(
			t, lockID,
			onboardingCustomLockID(
				onboardingRequestDigest(&changed),
			),
		)
	}
}

// TestOnboarderRejectsInvalidFundingSummary ensures restored SDK packages
// cannot broaden the wallet-funding authority declared by the durable request.
func TestOnboarderRejectsInvalidFundingSummary(t *testing.T) {
	t.Parallel()

	callerFunded := tapsdk.CustomAnchorFundingCallerFundedExact
	wrongMaximum := uint64(999)
	tests := []struct {
		name        string
		configure   func(*fakeOnboardingDriver, *OnboardingRequest)
		errContains string
	}{
		{
			name: "wrong funding mode",
			configure: func(driver *fakeOnboardingDriver,
				_ *OnboardingRequest) {

				driver.fundingMode = &callerFunded
			},
			errContains: "not wallet funded",
		},
		{
			name: "different maximum",
			configure: func(driver *fakeOnboardingDriver,
				_ *OnboardingRequest) {

				driver.maxFeeSat = &wrongMaximum
			},
			errContains: "does not match request",
		},
		{
			name: "actual fee exceeds maximum",
			configure: func(driver *fakeOnboardingDriver,
				request *OnboardingRequest) {

				driver.actualFeeSat = request.MaxFeeSat + 1
			},
			errContains: "exceeds maximum",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			request, inventory, owner := testOnboardingRequest(t)
			driver := newFakeOnboardingDriver()
			test.configure(driver, request)
			store, err := NewFileStore(t.TempDir())
			require.NoError(t, err)
			onboarder := testOnboarder(
				driver, inventory, store, owner,
			)

			_, err = onboarder.Onboard(t.Context(), request)
			require.ErrorContains(t, err, test.errContains)
			require.Equal(t, 1, driver.commits)
			require.Zero(t, driver.verifications)
			require.Zero(t, driver.publishes)
		})
	}
}

// TestOnboarderRejectsIdempotencyRewrite binds the durable request identity
// before another asset transition can be attempted.
func TestOnboarderRejectsIdempotencyRewrite(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testOnboardingRequest(t)
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(
		driver, inventory, store, owner,
	)
	_, err = onboarder.Onboard(t.Context(), request)
	require.NoError(t, err)

	request.AssetAmount++
	_, err = onboarder.Onboard(t.Context(), request)
	require.ErrorContains(t, err, "idempotency key reused")
	require.Equal(t, 1, driver.commits)
}

// TestOnboarderRejectsPassiveAssets keeps the first PoC from silently moving
// another asset co-anchored beside the selected proof.
func TestOnboarderRejectsPassiveAssets(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testOnboardingRequest(t)
	anchor := inventory.onlyAnchor()
	passive := *anchor.Assets[0]
	passive.Genesis.IssuanceID[0] ^= 1
	anchor.Assets = append(anchor.Assets, &passive)
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(driver, inventory, store, owner)

	_, err = onboarder.Onboard(t.Context(), request)
	require.ErrorContains(t, err, "requires one isolated asset")
	require.Zero(t, driver.commits)
}

// TestOnboarderStopsAfterAmbiguousCommit ensures a transport failure that may
// have committed in tapd cannot create a competing transition on retry.
func TestOnboarderStopsAfterAmbiguousCommit(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testOnboardingRequest(t)
	driver := newFakeOnboardingDriver()
	driver.commitErr = &tapsdk.CustomAnchorCommitAttemptError{
		Err:            errors.New("transport lost"),
		OutcomeUnknown: true,
	}
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(driver, inventory, store, owner)

	_, err = onboarder.Onboard(t.Context(), request)
	require.ErrorIs(t, err, ErrReconciliationRequired)
	driver.commitErr = nil
	_, err = onboarder.Onboard(t.Context(), request)
	require.ErrorIs(t, err, ErrReconciliationRequired)
	require.Equal(t, 1, driver.commits)
}

// TestOnboarderFundsFromSeveralUtxos proves an onboarding that boards less
// than its funding anchors carry spends one asset input per anchor and
// returns the surplus on its own change output, owned by keys derived from
// the daemon's own tapd exactly once and pinned across every rebuild.
func TestOnboarderFundsFromSeveralUtxos(t *testing.T) {
	t.Parallel()

	const boarded = uint64(40)
	request, inventory, owner := testMultiUtxoOnboardingRequest(t, boarded)
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(driver, inventory, store, owner)

	result, err := onboarder.Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, boarded, result.AssetAmount)

	// One script key plus one anchor internal key, derived once and never
	// again on a replay.
	require.Equal(t, 2, inventory.derived)

	dto := driver.requests[0]
	require.Len(t, dto.Inputs, 2)
	require.Equal(
		t, "wavelength-onboarding-input-0", dto.Inputs[0].ID,
	)
	require.Equal(
		t, "wavelength-onboarding-input-1", dto.Inputs[1].ID,
	)
	require.Equal(
		t, testOnboardingFirstAmount, dto.Inputs[0].Amount,
	)
	require.Equal(
		t, testOnboardingSecondAmount, dto.Inputs[1].Amount,
	)
	require.Equal(t, request.ProofFiles[0], dto.Inputs[0].ProofFile)
	require.Equal(t, request.ProofFiles[1], dto.Inputs[1].ProofFile)

	// Every funding anchor is a tapd key spend under its own internal
	// key; wallet funding manages the inputs it adds after them.
	require.Len(t, dto.SigningPlans, 2)
	for idx := range dto.SigningPlans {
		plan := dto.SigningPlans[idx]
		require.EqualValues(t, idx, plan.InputIndex)
		require.NotNil(t, plan.KeyPath)
	}
	template, err := psbtutil.Parse(dto.AnchorPSBT)
	require.NoError(t, err)
	require.Len(t, template.UnsignedTx.TxIn, 2)
	require.Len(t, template.UnsignedTx.TxOut, 2)

	// The boarded output keeps index 0 and the change takes index 1, so
	// wallet funding can only append its Bitcoin change after both.
	require.Len(t, dto.Outputs, 2)
	require.Equal(t, onboardingOutputID, dto.Outputs[0].ID)
	require.Equal(t, boarded, dto.Outputs[0].Amount)
	require.EqualValues(t, 0, dto.Outputs[0].AnchorOutputIndex)
	require.Equal(
		t, tapsdk.CustomAssetScriptOPTrue, dto.Outputs[0].Script.Mode,
	)

	change := dto.Outputs[1]
	require.Equal(t, onboardingChangeID, change.ID)
	require.Equal(t, testOnboardingTotalAmount-boarded, change.Amount)
	require.Equal(
		t, onboardingChangeOutputIndex, change.AnchorOutputIndex,
	)
	require.EqualValues(
		t, onboardingChangeValueSat, change.AnchorValueSat,
	)
	require.Equal(t, scriptExternal, change.Script.Mode)
	require.NotNil(t, change.Script.External)
	require.Empty(t, change.Anchor.Tapscript.TapLeaves)

	// A replay must rebuild nothing and reuse the pinned change keys.
	replayed, err := onboarder.Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, result.Outpoint, replayed.Outpoint)
	require.Equal(t, 1, driver.commits)
	require.Equal(t, 2, inventory.derived)
}

// TestOnboarderExactFundingKeepsOneOutput proves funding that matches the
// boarded amount exactly still commits the single-output transition, so the
// change path never touches a full-value onboarding.
func TestOnboarderExactFundingKeepsOneOutput(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testMultiUtxoOnboardingRequest(
		t, testOnboardingTotalAmount,
	)
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(driver, inventory, store, owner)

	result, err := onboarder.Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, testOnboardingTotalAmount, result.AssetAmount)
	require.Zero(t, inventory.derived)

	dto := driver.requests[0]
	require.Len(t, dto.Inputs, 2)
	require.Len(t, dto.Outputs, 1)
	require.Equal(t, onboardingOutputID, dto.Outputs[0].ID)
	template, err := psbtutil.Parse(dto.AnchorPSBT)
	require.NoError(t, err)
	require.Len(t, template.UnsignedTx.TxOut, 1)
}

// TestOnboarderRejectsInsufficientFunding keeps an onboarding that its
// funding proofs cannot cover from ever reaching tapd.
func TestOnboarderRejectsInsufficientFunding(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testMultiUtxoOnboardingRequest(
		t, testOnboardingTotalAmount+1,
	)
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(driver, inventory, store, owner)

	_, err = onboarder.Onboard(t.Context(), request)
	require.ErrorContains(t, err, "carry 55 units, the request boards 56")
	require.Zero(t, driver.commits)
}

// TestOnboarderRejectsDuplicateFundingProof keeps two proofs selecting one
// anchor from double-counting the units it holds.
func TestOnboarderRejectsDuplicateFundingProof(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testMultiUtxoOnboardingRequest(t, 40)
	request.ProofFiles = [][]byte{
		request.ProofFiles[0], request.ProofFiles[0],
	}
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(driver, inventory, store, owner)

	_, err = onboarder.Onboard(t.Context(), request)
	require.ErrorContains(t, err, "selects anchor")
	require.Zero(t, driver.commits)
}

// TestOnboardingProofSetDigestIsOrderIndependent proves the retry identity
// depends on which funding proofs an onboarding selects, not on the order
// the selection or the durable replay slice hands them over in.
func TestOnboardingProofSetDigestIsOrderIndependent(t *testing.T) {
	t.Parallel()

	request, _, _ := testMultiUtxoOnboardingRequest(t, 40)
	digest := onboardingRequestDigest(request)

	swapped := *request
	swapped.ProofFiles = [][]byte{
		request.ProofFiles[1], request.ProofFiles[0],
	}
	require.Equal(t, digest, onboardingRequestDigest(&swapped))

	// A different funding set is a different request.
	dropped := *request
	dropped.ProofFiles = request.ProofFiles[:1]
	require.NotEqual(t, digest, onboardingRequestDigest(&dropped))

	// One proof reaches the same identity through either field.
	single, _, _ := testOnboardingRequest(t)
	folded := *single
	folded.ProofFile = nil
	folded.ProofFiles = [][]byte{single.ProofFile}
	require.Equal(
		t, onboardingRequestDigest(single),
		onboardingRequestDigest(&folded),
	)
}

// TestOnboardingChangeFailsClosed proves every divergence between the
// pinned asset change and what tapd committed is refused.
func TestOnboardingChangeFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mutate      func(*commitResult, *onboardingChange)
		errContains string
	}{{
		name: "missing change output",
		mutate: func(result *commitResult, _ *onboardingChange) {
			result.outputs = result.outputs[:1]
		},
		errContains: "misses output",
	}, {
		name: "wrong anchor position",
		mutate: func(result *commitResult, _ *onboardingChange) {
			result.outputs[1].anchorOutputIndex = 7
		},
		errContains: "output index 7",
	}, {
		name: "wrong amount",
		mutate: func(result *commitResult, _ *onboardingChange) {
			result.outputs[1].amount++
		},
		errContains: "amount 16, want 15",
	}, {
		name: "wrong carrier value",
		mutate: func(result *commitResult, _ *onboardingChange) {
			result.outputs[1].anchorValueSat--
		},
		errContains: "value 999, want 1000",
	}, {
		name: "wallet script mode substituted",
		mutate: func(result *commitResult, _ *onboardingChange) {
			result.outputs[1].scriptMode =
				tapsdk.CustomAssetScriptOPTrue
		},
		errContains: "is not the pinned external wallet key",
	}, {
		name: "another script key",
		mutate: func(result *commitResult, _ *onboardingChange) {
			result.outputs[1].scriptKey[1] ^= 1
		},
		errContains: "script key does not reproduce",
	}, {
		name: "another anchor internal key",
		mutate: func(_ *commitResult, change *onboardingChange) {
			change.AnchorInternalKey.PubKey =
				deterministicKey(
					tapsdk.Hash{1}, "onboarding-test",
				)
		},
		errContains: "does not reproduce the pinned wallet script",
	}, {
		name: "missing roots",
		mutate: func(result *commitResult, _ *onboardingChange) {
			result.outputs[1].taprootMerkleRoot = tapsdk.Hash{}
		},
		errContains: "root hints are missing",
	}}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			committed, change, anchorTx := onboardingChangeFixture(
				t,
			)
			test.mutate(committed, change)

			err := checkOnboardingChange(
				change, committed, anchorTx,
			)
			require.ErrorContains(t, err, test.errContains)
		})
	}
}

// onboardingChangeFixture commits one onboarding with change and returns the
// sealed material its verification runs over.
func onboardingChangeFixture(t *testing.T) (*commitResult, *onboardingChange,
	*wire.MsgTx) {

	t.Helper()
	request, inventory, owner := testMultiUtxoOnboardingRequest(t, 40)
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	onboarder := testOnboarder(driver, inventory, store, owner)

	_, err = onboarder.Onboard(t.Context(), request)
	require.NoError(t, err)

	committed := cloneCommitResult(driver.result)
	packet, err := psbtutil.Parse(committed.anchorPSBT)
	require.NoError(t, err)

	state, err := onboarder.loadState(
		t.Context(), request, onboardingRequestDigest(request),
	)
	require.NoError(t, err)
	require.NotNil(t, state.Change)
	require.EqualValues(t, 15, state.Change.Amount)

	// The unmutated material must pass, so every case below fails on its
	// own mutation alone.
	change := *state.Change
	require.NoError(
		t, checkOnboardingChange(
			&change, committed, packet.UnsignedTx,
		),
	)

	return committed, &change, packet.UnsignedTx
}

type fakeOnboardingDriver struct {
	mu            sync.Mutex
	base          *fakeDriver
	requests      []*tapsdk.CustomAnchorRequest
	result        *commitResult
	commitErr     error
	actualFeeSat  uint64
	appendChange  bool
	fundingMode   *tapsdk.CustomAnchorFundingMode
	maxFeeSat     *uint64
	commits       int
	verifications int
	publishes     int
}

func newFakeOnboardingDriver() *fakeOnboardingDriver {
	return &fakeOnboardingDriver{
		base:         newFakeDriver(),
		actualFeeSat: 250,
		appendChange: true,
	}
}

func (d *fakeOnboardingDriver) CommitOnboarding(ctx context.Context,
	request *tapsdk.CustomAnchorRequest,
	verifier tapsdk.ConfirmedProofVerifier) (*commitResult, error) {

	d.mu.Lock()
	d.commits++
	d.requests = append(d.requests, request.Clone())
	err := d.commitErr
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}

	result, err := d.base.Commit(ctx, request, verifier)
	if err != nil {
		return nil, err
	}

	// Onboarding spends one anchor input per selected funding UTXO, in
	// the template's own input order.
	template, err := psbtutil.Parse(result.anchorPSBT)
	if err != nil {
		return nil, err
	}
	result.inputs = make([]commitInput, len(request.Inputs))
	for idx := range request.Inputs {
		result.inputs[idx] = commitInput{
			logicalInputID:   request.Inputs[idx].ID,
			anchorInputIndex: uint32(idx),
			anchorOutpoint: sdkOutpoint(
				template.UnsignedTx.TxIn[idx].PreviousOutPoint,
			),
			assetRef: request.Inputs[idx].AssetRef,
			amount:   request.Inputs[idx].Amount,
		}
	}
	result.fundingMode = request.Funding.Mode
	result.actualFeeSat = d.actualFeeSat
	if request.Funding.WalletFunded != nil {
		result.maxFeeSat = request.Funding.WalletFunded.MaxFeeSat
	}
	if d.fundingMode != nil {
		result.fundingMode = *d.fundingMode
	}
	if d.maxFeeSat != nil {
		result.maxFeeSat = *d.maxFeeSat
	}
	if d.appendChange {
		packet, parseErr := psbtutil.Parse(result.anchorPSBT)
		if parseErr != nil {
			return nil, parseErr
		}
		walletInputValue := int64(2_000)
		for idx := range request.Inputs {
			packet.Inputs[idx].WitnessUtxo = &wire.TxOut{
				Value: int64(
					request.Outputs[0].AnchorValueSat,
				),
				PkScript: []byte{
					txscript.OP_TRUE,
				},
			}
		}
		walletInputHash := sha256Bytes(
			[]byte("onboarding-wallet-input"),
		)
		packet.UnsignedTx.AddTxIn(
			wire.NewTxIn(
				&wire.OutPoint{
					Hash:  walletInputHash,
					Index: 1,
				},
				nil,
				nil,
			),
		)
		packet.Inputs = append(packet.Inputs, psbt.PInput{
			WitnessUtxo: &wire.TxOut{
				Value:    walletInputValue,
				PkScript: []byte{txscript.OP_TRUE},
			},
		})
		changeValue := walletInputValue - int64(result.actualFeeSat)
		packet.UnsignedTx.AddTxOut(&wire.TxOut{
			Value:    changeValue,
			PkScript: []byte{txscript.OP_TRUE},
		})
		packet.Outputs = append(packet.Outputs, psbt.POutput{})
		result.anchorPSBT, err = psbtutil.Serialize(packet)
		if err != nil {
			return nil, err
		}
		anchorTxid := packet.UnsignedTx.TxHash()
		for idx := range result.outputs {
			result.outputs[idx].anchorOutpoint.Txid = anchorTxid
		}
	}
	d.mu.Lock()
	d.result = cloneCommitResult(result)
	d.mu.Unlock()

	return result, nil
}

func (d *fakeOnboardingDriver) DecodePackage(encoded []byte) (*commitResult,
	error) {

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.result == nil || !bytes.Equal(encoded, d.result.packageBytes) {
		return nil, errors.New("unknown fake onboarding package")
	}

	return cloneCommitResult(d.result), nil
}

func (d *fakeOnboardingDriver) VerifyFinalOnboarding(packageBytes,
	finalPSBT []byte) error {

	d.mu.Lock()
	defer d.mu.Unlock()
	d.verifications++
	if d.result == nil ||
		!bytes.Equal(packageBytes, d.result.packageBytes) ||
		!bytes.Equal(finalPSBT, d.result.anchorPSBT) {
		return errors.New("final onboarding artifacts mismatch")
	}

	return nil
}

func (d *fakeOnboardingDriver) PublishOnboarding(_ context.Context,
	packageBytes, finalPSBT []byte) error {

	d.mu.Lock()
	defer d.mu.Unlock()
	d.publishes++
	if d.result == nil ||
		!bytes.Equal(packageBytes, d.result.packageBytes) ||
		!bytes.Equal(finalPSBT, d.result.anchorPSBT) {
		return errors.New("published onboarding artifacts mismatch")
	}

	return nil
}

func testOnboardingRequest(t *testing.T) (*OnboardingRequest, *fakeInventory,
	keychain.KeyDescriptor) {

	t.Helper()
	preparation, inventory := testPreparationRequest(t)
	owner := testPrivateKey(t, 20)
	operator := testPrivateKey(t, 21)
	anchorKey := testPrivateKey(t, 22)
	anchor := inventory.onlyAnchor()
	anchor.AmtSat = 1_000
	anchor.InternalKey, _ = tapsdk.ParsePubKey(
		anchorKey.PubKey().SerializeCompressed(),
	)
	ownerDescriptor := keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: 91,
			Index:  7,
		},
		PubKey: owner.PubKey(),
	}

	return &OnboardingRequest{
		RequestID:   "asset-onboarding-request",
		AssetRef:    preparation.Intent.AssetRef,
		AssetAmount: preparation.Intent.AssetAmount,
		ProofFile: append(
			[]byte(nil), preparation.Intent.ProofFile...,
		),
		CarrierValueSat:    1_000,
		FeeRateSatPerVByte: 2,
		MaxFeeSat:          1_000,
		OperatorKey:        operator.PubKey(),
		ExitDelay:          144,
	}, inventory, ownerDescriptor
}

// Amounts of the two-anchor onboarding fixture: the base fixture's anchor
// holds 21 units and a second one holds 34.
const (
	testOnboardingFirstAmount  = uint64(21)
	testOnboardingSecondAmount = uint64(34)
	testOnboardingTotalAmount  = testOnboardingFirstAmount +
		testOnboardingSecondAmount
)

// testMultiUtxoOnboardingRequest funds one onboarding from two anchors of
// the same asset. Boarding less than they carry returns the surplus to the
// daemon's own tapd wallet as asset change.
func testMultiUtxoOnboardingRequest(t *testing.T, boarded uint64) (
	*OnboardingRequest, *fakeInventory, keychain.KeyDescriptor) {

	t.Helper()
	request, inventory, owner := testOnboardingRequest(t)
	first := inventory.onlyAnchor()
	require.Equal(t, testOnboardingFirstAmount, first.Assets[0].Amount)
	firstProof := append([]byte(nil), request.ProofFile...)

	scriptKey, err := tapsdk.ParsePubKey(
		testPrivateKey(t, 24).PubKey().SerializeCompressed(),
	)
	require.NoError(t, err)
	second := &tapsdk.ManagedUtxo{
		OutPoint: tapsdk.Outpoint{
			Txid:  sha256Bytes([]byte("second-onboarding-anchor")),
			Index: 3,
		},
		AmtSat:      first.AmtSat,
		InternalKey: first.InternalKey,
		TaprootAssetRoot: tapsdk.Hash(
			sha256Bytes(
				[]byte("second-onboarding-root"),
			),
		),
		Assets: []*tapsdk.AssetRecord{{
			AssetRef: first.Assets[0].AssetRef,
			Genesis:  first.Assets[0].Genesis,
			Amount:   testOnboardingSecondAmount,
			ScriptKey: tapsdk.ScriptKey{
				PubKey: scriptKey,
			},
		}},
	}
	inventory.utxos[second.OutPoint.String()] = second

	secondProof := []byte("second-confirmed-proof")
	inventory.verifications = map[string]*tapsdk.VerifyProofResponse{
		string(firstProof): inventory.verification,
		string(secondProof): {
			Valid: true,
			DecodedProof: &tapsdk.DecodedProof{
				AssetRef:   second.Assets[0].AssetRef,
				IssuanceID: second.Assets[0].Genesis.IssuanceID,
				ScriptKey:  scriptKey,
				Amount:     second.Assets[0].Amount,
				Outpoint:   second.OutPoint,
			},
		},
	}

	request.ProofFile = nil
	request.ProofFiles = [][]byte{firstProof, secondProof}
	request.AssetAmount = boarded

	return request, inventory, owner
}

func testOnboarder(driver onboardingDriver, inventory *fakeInventory,
	store Store, owner keychain.KeyDescriptor,
) *Onboarder {

	return &Onboarder{
		driver:    driver,
		inventory: inventory,
		keys:      inventory,
		store:     store,
		signer: func(_ context.Context, anchor []byte) ([]byte, error) {
			return append([]byte(nil), anchor...), nil
		},
		deriveOwnerKey: func(context.Context) (*keychain.KeyDescriptor,
			error) {

			key := owner

			return &key, nil
		},
	}
}
