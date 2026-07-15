package tapassets

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
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

		return &OnboardingRegistrationResult{
			Outpoint:           driver.result.outputs[0].wireOutpoint(),
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
	require.Equal(t, int64(4_750), result.ValueSat)
	require.NotZero(t, result.TaprootAssetRoot)
	require.NotEmpty(t, result.PolicyTemplate)
	require.NotEmpty(t, result.PkScript)

	// A fully committed journal must not need tapd to resume.
	inventory.err = errors.New("tapd unavailable")
	result, err = newOnboarder().Onboard(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, OnboardingStatusReady, result.Status)
	require.Equal(t, int32(321), result.ConfirmationHeight)
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
	require.Equal(t, 1, deriveCalls)
	require.Equal(t, 1, signCalls)
	require.Equal(t, 1, driver.commits)
	require.Equal(t, 3, driver.verifications)
	require.Equal(t, 1, driver.publishes)
	require.Len(t, registrations, 2)

	dto := driver.requests[0]
	require.Equal(
		t, tapsdk.CustomAnchorFundingCallerFundedExact,
		dto.Funding.Mode,
	)
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
	require.Equal(t, uint64(4_750), dto.Outputs[0].AnchorValueSat)
	require.Len(t, dto.Outputs[0].Anchor.Tapscript.TapLeaves, 2)
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
	commits       int
	verifications int
	publishes     int
}

func newFakeOnboardingDriver() *fakeOnboardingDriver {
	return &fakeOnboardingDriver{base: newFakeDriver()}
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
	d.mu.Lock()
	d.result = cloneCommitResult(result)
	d.mu.Unlock()

	return result, nil
}

func (d *fakeOnboardingDriver) DecodePackage(encoded []byte) (*commitResult,
	error) {

	return d.base.DecodePackage(encoded)
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
	anchor.AmtSat = 5_000
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
		AnchorFeeSat: 250,
		OperatorKey:  operator.PubKey(),
		ExitDelay:    144,
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
