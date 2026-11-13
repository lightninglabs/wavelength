package waved

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/customanchor"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/tapassets"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeDepositTapd serves one wallet anchor, the transfer that funds the
// deposit output, and the chain tip.
type fakeDepositTapd struct {
	utxos     map[string]*tapsdk.ManagedUtxo
	transfers []*tapsdk.AssetTransfer
	height    uint32
	version   string
}

func (f *fakeDepositTapd) GetInfo(context.Context) (*tapsdk.Info, error) {
	return &tapsdk.Info{BlockHeight: f.height, Version: f.version}, nil
}

func (f *fakeDepositTapd) ListUtxos(context.Context, *tapsdk.ListUtxosRequest) (
	map[string]*tapsdk.ManagedUtxo, error) {

	return f.utxos, nil
}

func (f *fakeDepositTapd) ListTransfers(context.Context,
	*tapsdk.ListTransfersRequest) ([]*tapsdk.AssetTransfer, error) {

	return f.transfers, nil
}

func (f *fakeDepositTapd) ExportProof(_ context.Context, _ tapsdk.AssetRef,
	_ tapsdk.PubKey, outpoint *tapsdk.Outpoint) (*tapsdk.ProofFile, error) {

	return &tapsdk.ProofFile{RawProofFile: outpoint.Txid[:]}, nil
}

// fakeOnboarder records onboarding requests and returns a fixed result.
type fakeOnboarder struct {
	requests []*tapassets.OnboardingRequest
	result   *tapassets.OnboardingResult
	err      error
}

func (f *fakeOnboarder) Onboard(_ context.Context,
	req *tapassets.OnboardingRequest) (*tapassets.OnboardingResult, error) {

	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}

	return f.result, nil
}

// depositFixture wires a deposit service to fakes and a real store.
type depositFixture struct {
	service   *assetDepositService
	tapd      *fakeDepositTapd
	onboarder *fakeOnboarder
	terms     *types.OperatorTerms
	roundFee  btcutil.Amount
	assetRef  string
	output    wire.OutPoint

	// boardings records the deposits handed to the round actor, and
	// boardErr is returned for each.
	boardings []*round.RegisterAssetBoardingRequest
	boardErr  error
}

func newDepositFixture(t *testing.T) *depositFixture {
	t.Helper()

	issuance := tapsdk.AssetID{0x11}
	ref := tapsdk.AssetRefFromAssetID(issuance)
	_, operatorKey := btcec.PrivKeyFromBytes([]byte{0x05})
	_, ownerKey := btcec.PrivKeyFromBytes([]byte{0x06})
	scriptKey, err := tapsdk.ParsePubKey(ownerKey.SerializeCompressed())
	require.NoError(t, err)

	output := wire.OutPoint{Hash: chainhash.Hash{0x22}, Index: 0}
	leafHash := chainhash.Hash{0x44}
	template, err := arkscript.EncodeStandardVTXOTemplate(
		ownerKey, operatorKey, 144,
	)
	require.NoError(t, err)
	pkScript, _, _, err := arkscript.ComposedBoardingScript(
		template, leafHash,
	)
	require.NoError(t, err)
	tapd := &fakeDepositTapd{
		version: "0.9.0-alpha",
		utxos: map[string]*tapsdk.ManagedUtxo{
			"anchor": {
				OutPoint: tapsdk.Outpoint{
					Txid: tapsdk.Hash{
						1,
					},
				},
				Assets: []*tapsdk.AssetRecord{{
					AssetRef: ref,
					Genesis: tapsdk.IssuanceGenesis{
						IssuanceID: issuance,
					},
					Amount: 1_000,
				}},
			},
		},
		transfers: []*tapsdk.AssetTransfer{{
			AnchorTxid: output.Hash.String(),
			Outputs: []tapsdk.TransferOutput{{
				AnchorOutpoint: tapsdk.Outpoint{
					Txid:  tapsdk.Hash(output.Hash),
					Index: output.Index,
				},
				IssuanceID: issuance,
				ScriptKey:  scriptKey,
			}},
		}},
	}
	onboarder := &fakeOnboarder{
		result: &tapassets.OnboardingResult{
			Outpoint:       output,
			ValueSat:       10_000,
			ActualFeeSat:   250,
			PkScript:       pkScript,
			PolicyTemplate: template,
			OwnerKey: keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: 42, Index: 3,
				},
				PubKey: ownerKey,
			},
			Digest: tapsdk.Hash{
				0x33,
			},
			ScriptKey:        scriptKey,
			TaprootAssetRoot: leafHash,
			OPTrueWitness: [][]byte{
				{
					0x51,
				},
			},
		},
	}

	sqlDB := db.NewTestDB(t)
	store := db.NewAssetDepositStore(
		db.NewStore(
			sqlDB.DB, sqlDB.Queries, sqlDB.Backend(),
			btclog.Disabled,
		),
	)
	fixture := &depositFixture{
		tapd:      tapd,
		onboarder: onboarder,
		terms: &types.OperatorTerms{
			PubKey:            operatorKey,
			BoardingExitDelay: 144,
			MinVTXOAmount:     1_000,
			AssetRefs: []string{
				ref.String(),
			},
		},
		roundFee: 300,
		assetRef: ref.String(),
		output:   output,
	}
	fixture.service = newAssetDepositService(&assetDepositConfig{
		log:       btclog.Disabled,
		store:     store,
		claims:    store,
		tapd:      tapd,
		onboarder: onboarder,
		terms: func() *types.OperatorTerms {
			return fixture.terms
		},
		quoteBoardingFee: func(context.Context, btcutil.Amount) (
			btcutil.Amount, error) {

			return fixture.roundFee, nil
		},
		chainParams: &chaincfg.RegressionNetParams,
		registerBoarding: func(_ context.Context,
			req *round.RegisterAssetBoardingRequest) error {

			fixture.boardings = append(fixture.boardings, req)

			return fixture.boardErr
		},
	})

	return fixture
}

// request returns a valid deposit request for the fixture asset.
func (f *depositFixture) request(id string) *waverpc.DepositAssetRequest {
	return &waverpc.DepositAssetRequest{
		RequestId:   id,
		AssetRef:    f.assetRef,
		AssetAmount: 600,
		MaxFeeSat:   5_000,
	}
}

// TestAssetDepositFunding checks that a deposit funds once from the stored
// request, and that a retry under its request ID resumes it.
func TestAssetDepositFunding(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newDepositFixture(t)

	deposit, err := f.service.Deposit(ctx, f.request("alice-1"))
	require.NoError(t, err)
	require.Equal(t, db.AssetDepositPublished, deposit.Status)
	require.Equal(t, f.output, deposit.Output.Outpoint)

	require.Len(t, f.onboarder.requests, 1)
	onboarded := f.onboarder.requests[0]
	require.Equal(t, "alice-1", onboarded.RequestID)
	require.EqualValues(t, 600, onboarded.AssetAmount)
	require.EqualValues(t, 2*1_000+2*300, onboarded.CarrierValueSat)
	require.Len(t, onboarded.ProofFiles, 1)
	require.True(t, onboarded.OperatorKey.IsEqual(f.terms.PubKey))
	require.EqualValues(t, 144, onboarded.ExitDelay)
	require.EqualValues(
		t, defaultAssetDepositTargetConf, onboarded.TargetConf,
	)

	// A published deposit is not funded again, even once the fee quote
	// moves, and a different deposit cannot reuse its ID.
	f.roundFee = 900
	_, err = f.service.Deposit(ctx, f.request("alice-1"))
	require.NoError(t, err)
	require.Len(t, f.onboarder.requests, 1)

	changed := f.request("alice-1")
	changed.AssetAmount = 700
	_, err = f.service.Deposit(ctx, changed)
	require.Equal(t, codes.AlreadyExists, status.Code(err))
}

// TestAssetDepositUnsupportedTapd checks that a tapd that tap-sdk does not
// report as signing its own anchor inputs fails the deposit as a precondition.
func TestAssetDepositUnsupportedTapd(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newDepositFixture(t)

	f.onboarder.err = fmt.Errorf("build: %w",
		&customanchor.UnsupportedCapabilityError{
			Capability: customanchor.CapabilityBackendAnchorSigning,
		})
	_, err := f.service.Deposit(ctx, f.request("alice-1"))
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	deposits, err := f.service.List(ctx)
	require.NoError(t, err)
	require.Len(t, deposits, 1)
	require.Equal(t, db.AssetDepositFunding, deposits[0].Status)
}

// TestAssetDepositOldTapd checks that a deposit is refused before it is
// recorded when tapd cannot sign its own anchor inputs.
func TestAssetDepositOldTapd(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newDepositFixture(t)
	f.tapd.version = "0.8.5-alpha commit=v0.8.5"

	_, err := f.service.Deposit(ctx, f.request("alice-1"))
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "v0.9.0 or later")
	require.Empty(t, f.onboarder.requests)

	deposits, err := f.service.List(ctx)
	require.NoError(t, err)
	require.Empty(t, deposits)
}

// TestAssetDepositUnadmittedAsset checks that a deposit of an asset the
// operator does not admit into rounds is refused before it is recorded.
func TestAssetDepositUnadmittedAsset(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newDepositFixture(t)
	f.terms.AssetRefs = nil

	_, err := f.service.Deposit(ctx, f.request("alice-1"))
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "does not admit asset")
	require.Empty(t, f.onboarder.requests)

	deposits, err := f.service.List(ctx)
	require.NoError(t, err)
	require.Empty(t, deposits)
}

// TestAssetDepositRetry checks that a failed attempt stays funding with its
// error, and that a retry replays the stored proofs even once tapd no longer
// lists the anchor.
func TestAssetDepositRetry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newDepositFixture(t)

	f.onboarder.err = errors.Join(
		errors.New("publish outcome unknown"),
		tapassets.ErrReconciliationRequired,
	)
	_, err := f.service.Deposit(ctx, f.request("alice-1"))
	require.Equal(t, codes.Unavailable, status.Code(err))

	deposits, err := f.service.List(ctx)
	require.NoError(t, err)
	require.Len(t, deposits, 1)
	require.Equal(t, db.AssetDepositFunding, deposits[0].Status)
	require.Contains(t, deposits[0].LastError, "publish outcome unknown")

	f.onboarder.err = nil
	f.tapd.utxos = nil
	f.roundFee = 900
	deposit, err := f.service.Deposit(ctx, f.request("alice-1"))
	require.NoError(t, err)
	require.Equal(t, db.AssetDepositPublished, deposit.Status)
	require.Empty(t, deposit.LastError)
	require.Len(t, f.onboarder.requests, 2)
	require.Equal(
		t, f.onboarder.requests[0].ProofFiles,
		f.onboarder.requests[1].ProofFiles,
	)
	require.Equal(
		t, f.onboarder.requests[0].CarrierValueSat,
		f.onboarder.requests[1].CarrierValueSat,
	)
}

// TestAssetDepositFeePolicy checks that an explicit fee rate funds without a
// confirmation target, so a retry that drops the ignored target matches.
func TestAssetDepositFeePolicy(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newDepositFixture(t)

	req := f.request("alice-1")
	req.SatPerVbyte = 2
	req.TargetConf = 6
	_, err := f.service.Deposit(ctx, req)
	require.NoError(t, err)
	require.Len(t, f.onboarder.requests, 1)
	require.EqualValues(t, 2, f.onboarder.requests[0].FeeRateSatPerVByte)
	require.Zero(t, f.onboarder.requests[0].TargetConf)

	req.TargetConf = 0
	_, err = f.service.Deposit(ctx, req)
	require.NoError(t, err)
	require.Len(t, f.onboarder.requests, 1)
}

// TestAssetDepositRefusals checks the requests a deposit refuses before
// selecting any asset.
func TestAssetDepositRefusals(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newDepositFixture(t)

	noFee := f.request("a")
	noFee.MaxFeeSat = 0
	_, err := f.service.Deposit(ctx, noFee)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	badRef := f.request("a")
	badRef.AssetRef = "not-an-asset"
	_, err = f.service.Deposit(ctx, badRef)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	tooMuch := f.request("a")
	tooMuch.AssetAmount = 5_000
	_, err = f.service.Deposit(ctx, tooMuch)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	f.service.quoteBoardingFee = func(context.Context, btcutil.Amount) (
		btcutil.Amount, error) {

		return 0, errors.New("operator unreachable")
	}
	_, err = f.service.Deposit(ctx, f.request("a"))
	require.Equal(t, codes.Unavailable, status.Code(err))

	f.terms = nil
	_, err = f.service.Deposit(ctx, f.request("a"))
	require.Equal(t, codes.Unavailable, status.Code(err))

	require.Empty(t, f.onboarder.requests)
	deposits, err := f.service.List(ctx)
	require.NoError(t, err)
	require.Empty(t, deposits)
}

// TestDepositValue checks that a deposit covers two minimum VTXOs plus twice
// the round fee, within the operator's boarding and fee minimums.
func TestDepositValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		terms    types.OperatorTerms
		roundFee btcutil.Amount
		want     btcutil.Amount
	}{{
		name: "quoted fee",
		terms: types.OperatorTerms{
			MinVTXOAmount: 1_000,
		},
		roundFee: 300,
		want:     2_600,
	}, {
		name: "fee below operator minimum",
		terms: types.OperatorTerms{
			MinVTXOAmount:  1_000,
			MinOperatorFee: 500,
		},
		roundFee: 100,
		want:     3_000,
	}, {
		name: "dust above VTXO minimum",
		terms: types.OperatorTerms{
			DustLimit: 330,
		},
		want: 660,
	}, {
		name: "boarding minimum",
		terms: types.OperatorTerms{
			MinVTXOAmount:     1_000,
			MinBoardingAmount: 10_000,
		},
		roundFee: 300,
		want:     10_000,
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := depositValue(&test.terms, test.roundFee)
			require.Equal(t, test.want, got)
		})
	}
}

// TestAssetDepositConfirmation checks that a published deposit stores its
// proof only once its transfer reaches the operator's boarding depth.
func TestAssetDepositConfirmation(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newDepositFixture(t)
	f.terms.MinConfirmations = 3

	_, err := f.service.Deposit(ctx, f.request("alice-1"))
	require.NoError(t, err)

	f.tapd.height = 209
	f.service.confirmPublished(ctx)
	deposits, err := f.service.List(ctx)
	require.NoError(t, err)
	require.Equal(t, db.AssetDepositPublished, deposits[0].Status)

	// Two confirmations stay below the operator's three.
	f.tapd.transfers[0].AnchorTxBlockHash = [32]byte{1}
	f.tapd.transfers[0].AnchorTxBlockHeight = 210
	f.tapd.height = 211
	f.service.confirmPublished(ctx)
	deposits, err = f.service.List(ctx)
	require.NoError(t, err)
	require.Equal(t, db.AssetDepositPublished, deposits[0].Status)

	f.tapd.height = 212
	f.service.confirmPublished(ctx)

	deposits, err = f.service.List(ctx)
	require.NoError(t, err)
	require.Equal(t, db.AssetDepositConfirmed, deposits[0].Status)
	require.EqualValues(t, 210, deposits[0].ConfirmedHeight)
	require.Equal(t, f.output.Hash[:], deposits[0].ProofFile)

	rpc := assetDepositToRPC(deposits[0], "round-1")
	require.Equal(
		t, waverpc.AssetDepositStatus_ASSET_DEPOSIT_STATUS_CONFIRMED,
		rpc.Status,
	)
	require.Equal(t, f.output.String(), rpc.Outpoint)
	require.Equal(t, "round-1", rpc.RoundId)
}

// TestAssetDepositBoard checks that a confirmed deposit is handed to the round
// actor with an asset VTXO at the minimum VTXO amount and a BTC VTXO for the
// rest of its value after the quoted round fee.
func TestAssetDepositBoard(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newDepositFixture(t)

	_, err := f.service.Board(ctx, "alice-1")
	require.Equal(t, codes.NotFound, status.Code(err))

	_, err = f.service.Deposit(ctx, f.request("alice-1"))
	require.NoError(t, err)
	_, err = f.service.Board(ctx, "alice-1")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	f.tapd.transfers[0].AnchorTxBlockHash = [32]byte{1}
	f.tapd.transfers[0].AnchorTxBlockHeight = 210
	f.tapd.height = 210
	f.service.confirmPublished(ctx)

	_, err = f.service.Board(ctx, "alice-1")
	require.NoError(t, err)
	require.Len(t, f.boardings, 1)

	req := f.boardings[0]
	require.EqualValues(t, 1_000, req.AssetVTXOAmount)
	require.EqualValues(t, 10_000-1_000-300, req.ChangeAmount)
	require.Equal(t, f.output, req.Boarding.Outpoint)
	require.Equal(t, f.assetRef, req.Boarding.Request.AssetRef)
	require.EqualValues(t, 600, req.Boarding.Request.AssetAmount)
	require.NotNil(t, req.Boarding.AuthSpend)

	// A deposit already in a round is refused.
	f.boardErr = round.ErrBoardingInFlight
	_, err = f.service.Board(ctx, "alice-1")
	require.Equal(t, codes.AlreadyExists, status.Code(err))

	// A round fee that leaves less than a minimum VTXO is refused before
	// the round actor sees the deposit.
	f.boardErr = nil
	f.roundFee = 8_500
	_, err = f.service.Board(ctx, "alice-1")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Len(t, f.boardings, 2)
}

// fakeAssetClaimer returns a fixed claim and records the last request.
type fakeAssetClaimer struct {
	prepares int
	calls    int
	tx       *wire.MsgTx
	request  *tapassets.ClaimRequest

	// onClaim runs before a claim is recorded in tapd.
	onClaim func()
}

func (f *fakeAssetClaimer) Prepare(_ context.Context,
	request *tapassets.ClaimRequest) (*tapassets.ClaimResult, error) {

	f.prepares++
	f.request = request

	return &tapassets.ClaimResult{Tx: f.tx}, nil
}

func (f *fakeAssetClaimer) Claim(_ context.Context,
	request *tapassets.ClaimRequest) (*tapassets.ClaimResult, error) {

	f.calls++
	f.request = request
	if f.onClaim != nil {
		f.onClaim()
	}

	return &tapassets.ClaimResult{Tx: f.tx}, nil
}

// TestAssetDepositClaim checks that a confirmed deposit is claimed only once
// its exit leaf opens on a tapd that credits it, that the claim is recorded
// before tapd learns of it and handed to txconfirm again after a restart,
// and that a claim is final only at the finality depth.
func TestAssetDepositClaim(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newDepositFixture(t)

	claimTx := wire.NewMsgTx(3)
	claimTx.AddTxIn(&wire.TxIn{PreviousOutPoint: f.output})
	claimTx.AddTxOut(wire.NewTxOut(10_000, []byte{0x51}))
	claimer := &fakeAssetClaimer{tx: claimTx}
	var (
		broadcasts   int
		reservations []wire.OutPoint
		reserveErr   error
	)
	f.service.claimer = claimer
	f.service.broadcastClaim = func(context.Context, *wire.MsgTx,
		uint32) error {

		broadcasts++

		return nil
	}
	f.service.reserveClaim = func(_ context.Context,
		outpoint wire.OutPoint) error {

		reservations = append(reservations, outpoint)

		return reserveErr
	}
	claimer.onClaim = func() {
		claim, err := f.service.ClaimOf(ctx, "alice-1")
		require.NoError(t, err)
		require.NotNil(t, claim)
	}

	_, err := f.service.Claim(ctx, "alice-1")
	require.Equal(t, codes.NotFound, status.Code(err))

	_, err = f.service.Deposit(ctx, f.request("alice-1"))
	require.NoError(t, err)
	_, err = f.service.Claim(ctx, "alice-1")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	f.tapd.transfers[0].AnchorTxBlockHash = [32]byte{1}
	f.tapd.transfers[0].AnchorTxBlockHeight = 210
	f.tapd.height = 210
	f.service.confirmPublished(ctx)

	// The exit leaf opens 144 blocks after the deposit confirmed.
	f.tapd.height = 210 + 142
	_, err = f.service.Claim(ctx, "alice-1")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	// A tapd that cannot confirm a transfer spending an asset it does
	// not hold would never credit the claim.
	f.tapd.height = 210 + 143
	f.tapd.version = "0.8.5-alpha"
	_, err = f.service.Claim(ctx, "alice-1")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Zero(t, claimer.prepares)

	// A deposit a round holds is not claimed.
	f.tapd.version = "0.9.0-alpha"
	reserveErr = round.ErrBoardingInFlight
	_, err = f.service.Claim(ctx, "alice-1")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Zero(t, claimer.calls)
	claim, err := f.service.ClaimOf(ctx, "alice-1")
	require.NoError(t, err)
	require.Nil(t, claim)

	reserveErr = nil
	_, err = f.service.Claim(ctx, "alice-1")
	require.NoError(t, err)
	require.Equal(t, 1, claimer.calls)
	require.Equal(t, 1, broadcasts)
	require.Equal(t, []wire.OutPoint{f.output, f.output}, reservations)

	request := claimer.request
	require.Equal(t, "deposit/alice-1", request.RequestID)
	require.Equal(t, f.output, request.Outpoint)
	require.Len(t, request.Inputs, 1)
	require.EqualValues(t, 600, request.Inputs[0].Amount)
	require.EqualValues(t, 144, request.ExitSpend.RequiredSequence)
	require.NoError(
		t, request.ExitSpend.VerifyBindsToPkScript(
			request.Output.PkScript,
		),
	)

	claim, err = f.service.ClaimOf(ctx, "alice-1")
	require.NoError(t, err)
	require.Equal(t, claimTx.TxHash(), claim.ClaimTxid)
	require.Equal(t, db.AssetDepositClaiming, claim.Status)

	_, err = f.service.Board(ctx, "alice-1")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	// A tracked claim is not handed to txconfirm again, but after a
	// restart it is, from the claim journal.
	f.service.confirmClaims(ctx)
	require.Equal(t, 1, broadcasts)
	f.service.trackedClaims = make(map[chainhash.Hash]struct{})
	f.service.confirmClaims(ctx)
	require.Equal(t, 2, broadcasts)
	require.Equal(t, 2, claimer.calls)

	// A confirmed claim stays pending until it is buried at the finality
	// depth, and is handed to txconfirm again after a restart so a reorg
	// is still followed.
	claimHeight := f.tapd.height + 1
	f.tapd.transfers = append(f.tapd.transfers, &tapsdk.AssetTransfer{
		TransferTxid:        claimTx.TxHash(),
		AnchorTxBlockHash:   [32]byte{2},
		AnchorTxBlockHeight: claimHeight,
	})
	f.tapd.height = claimHeight + claimFinalityDepth - 2
	f.service.trackedClaims = make(map[chainhash.Hash]struct{})
	f.service.confirmClaims(ctx)
	claim, err = f.service.ClaimOf(ctx, "alice-1")
	require.NoError(t, err)
	require.Equal(t, db.AssetDepositClaiming, claim.Status)
	require.Equal(t, 3, broadcasts)

	f.tapd.height = claimHeight + claimFinalityDepth - 1
	f.service.confirmClaims(ctx)
	claim, err = f.service.ClaimOf(ctx, "alice-1")
	require.NoError(t, err)
	require.Equal(t, db.AssetDepositClaimed, claim.Status)

	_, err = f.service.Claim(ctx, "alice-1")
	require.NoError(t, err)
	require.Equal(t, 3, claimer.calls)
	require.Equal(t, 3, broadcasts)
	require.Equal(t, 2, claimer.prepares)
}
