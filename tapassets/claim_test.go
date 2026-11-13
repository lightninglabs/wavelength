package tapassets

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/tap-sdk/customanchor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// fakeClaimDriver seals whatever template it is given and counts tapd calls.
type fakeClaimDriver struct {
	commits    int
	publishes  int
	reconciles int

	publishErr   error
	reconcileErr error
	anchorPSBT   []byte
	inputProofs  [][]byte
	request      *customanchor.Request

	// outputs are the asset outputs tapd seals: one per input, in the
	// claim's first output, unless a test sets them.
	outputs []commitOutput
}

func (d *fakeClaimDriver) Commit(_ context.Context,
	request *customanchor.Request, _ customanchor.ConfirmedProofVerifier) (
	*commitResult, error) {

	d.commits++
	d.request = request
	d.anchorPSBT = request.AnchorPSBT
	if d.outputs == nil {
		for _, input := range request.Inputs {
			d.outputs = append(d.outputs, commitOutput{
				amount: input.Amount,
			})
		}
	}

	return d.DecodePackage([]byte("claim-package"))
}

func (d *fakeClaimDriver) DecodePackage(encoded []byte) (*commitResult, error) {
	return &commitResult{
		packageBytes: encoded,
		anchorPSBT:   d.anchorPSBT,
		fundingMode:  customanchor.FundingCallerFundedExact,
		outputs:      d.outputs,
	}, nil
}

func (d *fakeClaimDriver) PublishWithInputProofs(_ context.Context, _, _ []byte,
	inputProofs [][]byte) error {

	d.publishes++
	d.inputProofs = inputProofs

	return d.publishErr
}

func (d *fakeClaimDriver) ReconcileOnboarding(context.Context, []byte) error {
	d.reconciles++

	return d.reconcileErr
}

// fakeClaimSigner returns a fixed Schnorr signature.
type fakeClaimSigner struct {
	sig *schnorr.Signature
}

func (s fakeClaimSigner) SignOutputRaw(*wire.MsgTx, *input.SignDescriptor) (
	input.Signature, error) {

	return s.sig, nil
}

// newClaimTest returns a claimer over fakes and a claim of a composed deposit
// output with a 144-block exit delay.
func newClaimTest(t *testing.T) (*Claimer, *fakeClaimDriver, *fakeInventory,
	*ClaimRequest) {

	t.Helper()

	owner, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	root := chainhash.HashH([]byte("asset root"))

	policy, err := arkscript.NewVTXOPolicy(
		owner.PubKey(), operator.PubKey(), 144,
	)
	require.NoError(t, err)
	template, err := policy.Template.Encode()
	require.NoError(t, err)
	pkScript, _, _, err := arkscript.ComposedBoardingScript(template, root)
	require.NoError(t, err)
	exitSpend, err := arkscript.ComposedBoardingExitSpend(
		owner.PubKey(), operator.PubKey(), 144, root,
	)
	require.NoError(t, err)

	digest := chainhash.HashH([]byte("sighash"))
	sig, err := schnorr.Sign(owner, digest[:])
	require.NoError(t, err)

	driver := &fakeClaimDriver{}
	keys := &fakeInventory{}
	claimer := &Claimer{
		driver: driver,
		keys:   keys,
		store:  newMemoryStore(),
		signer: fakeClaimSigner{
			sig: sig,
		},
	}
	request := &ClaimRequest{
		RequestID: "deposit/alice-1",
		Outpoint: wire.OutPoint{
			Hash: chainhash.HashH([]byte("deposit")),
		},
		Output:    wire.NewTxOut(4_000, pkScript),
		ExitSpend: exitSpend,
		OwnerKey: keychain.KeyDescriptor{
			PubKey: owner.PubKey(),
		},
		AssetRef: "asset",
		Inputs: []ClaimInput{{
			Amount:    600,
			ProofFile: []byte("proof"),
			OPTrueWitness: [][]byte{
				{
					0x51,
				},
				{
					0xc0,
				},
			},
		}},
	}

	return claimer, driver, keys, request
}

// TestClaimerClaimsOnce checks that a claim spends the exit leaf into a
// zero-fee transaction with a P2A anchor, and that a retry returns the same
// transaction without calling tapd again.
func TestClaimerClaimsOnce(t *testing.T) {
	t.Parallel()

	claimer, driver, keys, request := newClaimTest(t)
	ctx := t.Context()

	result, err := claimer.Claim(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 1, driver.commits)
	require.Equal(t, 1, driver.publishes)
	require.Equal(t, 2, keys.derived)

	tx := result.Tx
	require.EqualValues(t, arktx.TxVersion, tx.Version)
	require.Len(t, tx.TxIn, 1)
	require.Equal(t, request.Outpoint, tx.TxIn[0].PreviousOutPoint)
	require.EqualValues(t, 144, tx.TxIn[0].Sequence)
	require.Len(t, tx.TxIn[0].Witness, 3)
	require.Equal(
		t, request.ExitSpend.WitnessScript, tx.TxIn[0].Witness[1],
	)
	require.Len(t, tx.TxOut, 2)
	require.EqualValues(t, 4_000, tx.TxOut[0].Value)
	require.Equal(t, arkscript.AnchorOutput(), tx.TxOut[1])
	require.Equal(t, wire.OutPoint{Hash: tx.TxHash()}, result.AssetOutpoint)

	// The asset moves under the caller's OP_TRUE witness to keys tapd
	// derived, and waved signs the exit leaf itself.
	committed := driver.request
	require.Equal(
		t, customanchor.FundingCallerFundedExact,
		committed.Funding.Mode,
	)
	require.Len(t, committed.SigningPlans, 1)
	require.NotNil(t, committed.SigningPlans[0].CallerSigned)
	require.Equal(
		t, customanchor.AssetWitnessCallerProvided,
		committed.Inputs[0].Witness.Mode,
	)
	require.Equal(
		t, request.Inputs[0].OPTrueWitness,
		committed.Inputs[0].Witness.Stack,
	)
	require.Equal(t, scriptExternal, committed.Outputs[0].Script.Mode)

	// tapd gets the claimed output's proof file with the claim, since it
	// may never have seen its history.
	require.Equal(
		t, [][]byte{request.Inputs[0].ProofFile}, driver.inputProofs,
	)

	again, err := claimer.Claim(ctx, request)
	require.NoError(t, err)
	require.Equal(t, tx.TxHash(), again.Tx.TxHash())
	require.Equal(t, 1, driver.commits)
	require.Equal(t, 1, driver.publishes)
	require.Equal(t, 2, keys.derived)
}

// TestClaimerReconcilesAmbiguousPublish checks that a publish with an unknown
// outcome is reconciled on retry rather than published again.
func TestClaimerReconcilesAmbiguousPublish(t *testing.T) {
	t.Parallel()

	claimer, driver, _, request := newClaimTest(t)
	ctx := t.Context()

	driver.publishErr = &customanchor.PublishAttemptError{
		OutcomeUnknown: true,
	}
	_, err := claimer.Claim(ctx, request)
	require.ErrorIs(t, err, ErrReconciliationRequired)

	driver.publishErr = nil
	_, err = claimer.Claim(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 1, driver.publishes)
	require.Equal(t, 1, driver.reconciles)
	require.Equal(t, 1, driver.commits)
}

// TestClaimerRepublishesUnrecordedClaim checks that a claim whose publish
// attempt left no record in tapd, such as one interrupted before its RPC, is
// published again.
func TestClaimerRepublishesUnrecordedClaim(t *testing.T) {
	t.Parallel()

	claimer, driver, _, request := newClaimTest(t)
	ctx := t.Context()

	driver.publishErr = &customanchor.PublishAttemptError{
		OutcomeUnknown: true,
	}
	_, err := claimer.Claim(ctx, request)
	require.ErrorIs(t, err, ErrReconciliationRequired)

	// A reconcile that fails for any other reason does not publish.
	driver.publishErr = nil
	driver.reconcileErr = ErrReconciliationRequired
	_, err = claimer.Claim(ctx, request)
	require.ErrorIs(t, err, ErrReconciliationRequired)
	require.Equal(t, 1, driver.publishes)

	driver.reconcileErr = errors.Join(
		ErrReconciliationRequired, errTransferNotRecorded,
	)
	_, err = claimer.Claim(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 2, driver.publishes)
	require.Equal(t, 2, driver.reconciles)

	_, err = claimer.Claim(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 2, driver.publishes)
	require.Equal(t, 2, driver.reconciles)
}

// TestClaimerPrepareDoesNotPublish checks that a prepared claim is the one a
// later claim publishes.
func TestClaimerPrepareDoesNotPublish(t *testing.T) {
	t.Parallel()

	claimer, driver, _, request := newClaimTest(t)
	ctx := t.Context()

	prepared, err := claimer.Prepare(ctx, request)
	require.NoError(t, err)
	require.Zero(t, driver.publishes)

	claimed, err := claimer.Claim(ctx, request)
	require.NoError(t, err)
	require.Equal(t, prepared.Tx.TxHash(), claimed.Tx.TxHash())
	require.Equal(t, 1, driver.commits)
	require.Equal(t, 1, driver.publishes)
}

// TestTapdCanClaim checks the tapd versions that credit a claimed asset.
func TestTapdCanClaim(t *testing.T) {
	t.Parallel()

	for version, want := range map[string]bool{
		"0.8.5-alpha commit=v0.8.5": false,
		"0.8.99-alpha":              true,
		"0.9.0-alpha.rc1":           true,
		"0.10.0":                    true,
		"1.0.0":                     true,
	} {
		got, err := TapdCanClaim(version)
		require.NoError(t, err, version)
		require.Equal(t, want, got, version)
	}

	_, err := TapdCanClaim("unknown")
	require.Error(t, err)
}

// TestTapdSignsAnchorInputs checks the tapd versions that sign their own
// anchor inputs at publish.
func TestTapdSignsAnchorInputs(t *testing.T) {
	t.Parallel()

	for version, want := range map[string]bool{
		"0.8.5-alpha commit=v0.8.5": false,
		"0.8.99-alpha":              true,
		"0.9.0-alpha.rc1":           true,
	} {
		got, err := TapdSignsAnchorInputs(version)
		require.NoError(t, err, version)
		require.Equal(t, want, got, version)
	}
}

// TestClaimerRejectsChangedRequest checks that a journal entry serves only
// the request it was written for.
func TestClaimerRejectsChangedRequest(t *testing.T) {
	t.Parallel()

	claimer, _, _, request := newClaimTest(t)
	ctx := t.Context()

	_, err := claimer.Claim(ctx, request)
	require.NoError(t, err)

	changed := *request
	changed.Inputs = []ClaimInput{request.Inputs[0]}
	changed.Inputs[0].Amount = 500
	_, err = claimer.Claim(ctx, &changed)
	require.ErrorContains(t, err, "different request")
}

// TestClaimerClaimsEveryIssuance checks that a claim of an output holding two
// issuances spends both in one asset output and supplies both proof files.
func TestClaimerClaimsEveryIssuance(t *testing.T) {
	t.Parallel()

	claimer, driver, _, request := newClaimTest(t)
	reissued := ClaimInput{
		Amount:        150,
		ProofFile:     []byte("reissued proof"),
		OPTrueWitness: request.Inputs[0].OPTrueWitness,
	}
	request.Inputs = append(request.Inputs, reissued)

	_, err := claimer.Claim(t.Context(), request)
	require.NoError(t, err)

	committed := driver.request
	require.Len(t, committed.Inputs, 2)
	require.NotEqual(t, committed.Inputs[0].ID, committed.Inputs[1].ID)
	for idx, input := range committed.Inputs {
		require.Equal(t, request.Inputs[idx].Amount, input.Amount)
		require.Equal(t, request.Inputs[idx].ProofFile, input.ProofFile)
	}
	require.Len(t, committed.Outputs, 1)
	require.EqualValues(t, 750, committed.Outputs[0].Amount)
	require.Equal(
		t, [][]byte{[]byte("proof"), []byte("reissued proof")},
		driver.inputProofs,
	)
}

// TestClaimerRefusesShortCommit checks that a sealed claim moving fewer units
// than the output holds is refused before it is signed.
func TestClaimerRefusesShortCommit(t *testing.T) {
	t.Parallel()

	claimer, driver, _, request := newClaimTest(t)
	driver.outputs = []commitOutput{{
		amount: 599,
	}}

	_, err := claimer.Claim(t.Context(), request)
	require.ErrorContains(t, err, "moves 599 units, want 600")
	require.Zero(t, driver.publishes)
}

// TestClaimerKeepsJournalAcrossProofs checks that a journaled claim serves a
// request whose proof file was rebuilt, as after a reorganization confirms
// the same history in other blocks, and publishes the rebuilt proof.
func TestClaimerKeepsJournalAcrossProofs(t *testing.T) {
	t.Parallel()

	claimer, driver, _, request := newClaimTest(t)
	ctx := t.Context()

	prepared, err := claimer.Prepare(ctx, request)
	require.NoError(t, err)

	rebuilt := *request
	rebuilt.Inputs = []ClaimInput{request.Inputs[0]}
	rebuilt.Inputs[0].ProofFile = []byte("reconfirmed proof")
	claimed, err := claimer.Claim(ctx, &rebuilt)
	require.NoError(t, err)
	require.Equal(t, prepared.Tx.TxHash(), claimed.Tx.TxHash())
	require.Equal(t, 1, driver.commits)
	require.Equal(
		t, [][]byte{[]byte("reconfirmed proof")}, driver.inputProofs,
	)
}

// TestClaimRequestBindsExitSpend checks that a claim is refused when the exit
// leaf does not spend the claimed output.
func TestClaimRequestBindsExitSpend(t *testing.T) {
	t.Parallel()

	claimer, driver, _, request := newClaimTest(t)

	other, _, _, otherRequest := newClaimTest(t)
	require.NotNil(t, other)
	request.ExitSpend = otherRequest.ExitSpend

	_, err := claimer.Claim(t.Context(), request)
	require.Error(t, err)
	require.Zero(t, driver.commits)
}
