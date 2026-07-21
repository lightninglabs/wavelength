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

// TestOnboarderResumesPendingConfirmation proves the PoC crosses its three
// external side effects in order and never rebuilds, resigns, or republishes
// after a restart. The operator always receives byte-identical artifacts.
func TestOnboarderResumesPendingConfirmation(t *testing.T) {
	t.Parallel()

	request, inventory, owner := testOnboardingRequest(t)
	driver := newFakeOnboardingDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	var deriveCalls, signCalls int
	var registrations []OnboardingRegistration
	registrar := func(_ context.Context,
		registration OnboardingRegistration) (
		*OnboardingRegistrationResult, error) {

		registrations = append(registrations, registration)
		if len(registrations) == 1 {
			return nil, ErrOnboardingPendingConfirmation
		}
		outpoint := driver.result.outputs[0].wireOutpoint()

		return &OnboardingRegistrationResult{
			Outpoint:           outpoint,
			ConfirmationHeight: 321,
		}, nil
	}
	newOnboarder := func() *Onboarder {
		return &Onboarder{
			driver:    driver,
			inventory: inventory,
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
			registrar: registrar,
		}
	}

	result, err := newOnboarder().Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, OnboardingStatusPendingConfirmation, result.Status)
	require.Equal(t, 1, deriveCalls)
	require.Equal(t, 1, signCalls)
	require.Equal(t, 1, driver.commits)
	require.Equal(t, 1, driver.verifications)
	require.Equal(t, 1, driver.publishes)
	require.Len(t, registrations, 1)
	require.Equal(t, int64(1_000), result.ValueSat)
	require.Equal(t, uint64(250), result.ActualFeeSat)
	require.NotZero(t, result.TaprootAssetRoot)
	require.Equal(t, request.AssetRef, result.AssetRef)
	require.Equal(t, request.AssetAmount, result.AssetAmount)
	require.Equal(t, request.AssetRef, registrations[0].TaprootAssetRef)
	require.Equal(
		t, request.AssetAmount, registrations[0].TaprootAssetAmount,
	)
	require.NotEmpty(t, result.PolicyTemplate)
	require.NotEmpty(t, result.PkScript)

	// A fully committed journal must not need tapd to resume.
	inventory.err = errors.New("tapd unavailable")
	result, err = newOnboarder().Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, OnboardingStatusReady, result.Status)
	require.Equal(t, int32(321), result.ConfirmationHeight)
	require.Equal(t, uint64(250), result.ActualFeeSat)
	require.Equal(t, 1, deriveCalls)
	require.Equal(t, 1, signCalls)
	require.Equal(t, 1, driver.commits)
	require.Equal(t, 2, driver.verifications)
	require.Equal(t, 1, driver.publishes)
	require.Len(t, registrations, 2)
	require.Equal(t, registrations[0], registrations[1])

	// Once admitted, another retry is a local read and validation only.
	result, err = newOnboarder().Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, OnboardingStatusReady, result.Status)
	require.Equal(t, uint64(250), result.ActualFeeSat)
	require.Equal(t, 1, deriveCalls)
	require.Equal(t, 1, signCalls)
	require.Equal(t, 1, driver.commits)
	require.Equal(t, 3, driver.verifications)
	require.Equal(t, 1, driver.publishes)
	require.Len(t, registrations, 2)

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
	require.Equal(
		t, tapsdk.CustomAssetScriptWallet, dto.Outputs[0].Script.Mode,
	)
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
				func(context.Context, OnboardingRegistration) (
					*OnboardingRegistrationResult, error) {

					return nil,
						ErrOnboardingPendingConfirmation
				},
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
		func(_ context.Context, _ OnboardingRegistration) (
			*OnboardingRegistrationResult, error) {

			return nil, ErrOnboardingPendingConfirmation
		},
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
	onboarder := testOnboarder(
		driver, inventory, store, owner,
		func(context.Context, OnboardingRegistration) (
			*OnboardingRegistrationResult, error) {

			return nil, nil
		},
	)

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
	onboarder := testOnboarder(
		driver, inventory, store, owner,
		func(context.Context, OnboardingRegistration) (
			*OnboardingRegistrationResult, error) {

			return nil, nil
		},
	)

	_, err = onboarder.Onboard(t.Context(), request)
	require.ErrorIs(t, err, ErrReconciliationRequired)
	driver.commitErr = nil
	_, err = onboarder.Onboard(t.Context(), request)
	require.ErrorIs(t, err, ErrReconciliationRequired)
	require.Equal(t, 1, driver.commits)
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
		packet.Inputs[0].WitnessUtxo = &wire.TxOut{
			Value: int64(request.Outputs[0].AnchorValueSat),
			PkScript: []byte{
				txscript.OP_TRUE,
			},
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

func testOnboarder(driver onboardingDriver, inventory proofInventoryClient,
	store Store, owner keychain.KeyDescriptor,
	registrar OnboardingRegistrar) *Onboarder {

	return &Onboarder{
		driver:    driver,
		inventory: inventory,
		store:     store,
		signer: func(_ context.Context, anchor []byte) ([]byte, error) {
			return append([]byte(nil), anchor...), nil
		},
		deriveOwnerKey: func(context.Context) (*keychain.KeyDescriptor,
			error) {

			key := owner

			return &key, nil
		},
		registrar: registrar,
	}
}

func (o commitOutput) wireOutpoint() wire.OutPoint {
	return wire.OutPoint{
		Hash:  o.anchorOutpoint.Txid,
		Index: o.anchorOutpoint.Index,
	}
}
