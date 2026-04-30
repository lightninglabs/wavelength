package rounds

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// measureRealCollabWitnessWeight returns the actual on-chain witness
// weight (in weight units) for a script-path spend of the given
// boarding input's collab leaf. It serializes a real wire.TxWitness
// stack — <cosigSig><ownerSig><script><controlBlock> — using dummy
// 64-byte schnorr signatures and the real control block derived from
// the boarding tap tree. This is the ground-truth measurement used to
// drive non-tautological assertions about witness weight estimates and
// fee deltas.
func measureRealCollabWitnessWeight(t *testing.T,
	bi *BoardingInput) lntypes.WeightUnit {

	t.Helper()

	require.NotEmpty(t, bi.Tapscript.Leaves,
		"boarding tapscript must have a collab leaf")

	tapTree := txscript.AssembleTaprootScriptTree(
		bi.Tapscript.Leaves...,
	)
	controlBlock := tapTree.LeafMerkleProofs[0].ToControlBlock(
		&arkscript.ARKNUMSKey,
	)
	cbBytes, err := controlBlock.ToBytes()
	require.NoError(t, err)

	const schnorrSigBytes = 64
	witness := wire.TxWitness{
		make([]byte, schnorrSigBytes),
		make([]byte, schnorrSigBytes),
		bi.Tapscript.Leaves[0].Script,
		cbBytes,
	}

	return lntypes.WeightUnit(witness.SerializeSize())
}

// expectedWitnessDeltaFee is the canonical expected change-output fee
// adjustment as a function of the real on-chain witness shape. It
// serializes the real collab witness, subtracts the per-input
// key-spend witness LND charged at FundPsbt time, multiplies by N,
// and applies a single FeeForWeight call (the truncation invariant
// the implementation must respect).
func expectedWitnessDeltaFee(t *testing.T, bis []*BoardingInput,
	feeRate chainfee.SatPerKWeight) int64 {

	t.Helper()

	var total lntypes.WeightUnit
	for _, bi := range bis {
		realW := measureRealCollabWitnessWeight(t, bi)
		total += realW - input.TaprootKeyPathWitnessSize
	}

	return int64(feeRate.FeeForWeight(total))
}

// fakeFeeEstimator is a minimal chainfee.Estimator returning a fixed rate.
type fakeFeeEstimator struct {
	rate chainfee.SatPerKWeight
}

func (f *fakeFeeEstimator) EstimateFeePerKW(uint32) (
	chainfee.SatPerKWeight, error) {

	return f.rate, nil
}
func (f *fakeFeeEstimator) Start() error                    { return nil }
func (f *fakeFeeEstimator) Stop() error                     { return nil }
func (f *fakeFeeEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	return chainfee.FeePerKwFloor
}

// fundCallback inspects the PSBT LND was handed and returns the
// (changeIdx, lockedOutpoints, err) tuple along with the post-fund packet
// modifications applied to it (wallet inputs added, change appended,
// reorder simulated, etc.).
type fundCallback func(*psbt.Packet) (int32, []wire.OutPoint, error)

// fakeWalletController is a callback-driven WalletController for tests
// that need to inspect the PSBT contents handed to FundPsbt and also
// drive specific post-fund packet shapes. It captures every call to
// FundPsbt and ReleaseInputs so tests can assert on them.
type fakeWalletController struct {
	input.Signer

	onFund    fundCallback
	onRelease func(lockID [32]byte, ops []wire.OutPoint) error

	fundCallCount    int
	releaseCallCount int
	releaseLockID    [32]byte
	releaseOutpoints []wire.OutPoint

	// inspectedAtFundTime is a deep snapshot of what the callback saw
	// when FundPsbt was invoked (used by tests that verify the metadata
	// LND sees, since the same packet is later mutated by the swap).
	inspectedAtFundTime []psbt.PInput
	inspectedTxIns      []wire.OutPoint
}

func (f *fakeWalletController) FundPsbt(_ context.Context,
	packet *psbt.Packet, _ int32, _ chainfee.SatPerKWeight, _ string,
	_ *FundingOpts) (int32, []wire.OutPoint, error) {

	f.fundCallCount++

	// Snapshot the inputs LND would see at FundPsbt time. Deep copy the
	// PInputs so later swaps don't mutate the snapshot.
	f.inspectedAtFundTime = make([]psbt.PInput, len(packet.Inputs))
	for i := range packet.Inputs {
		f.inspectedAtFundTime[i] = clonePInput(&packet.Inputs[i])
	}
	f.inspectedTxIns = make([]wire.OutPoint, len(packet.UnsignedTx.TxIn))
	for i, in := range packet.UnsignedTx.TxIn {
		f.inspectedTxIns[i] = in.PreviousOutPoint
	}

	return f.onFund(packet)
}

func (f *fakeWalletController) ReleaseInputs(_ context.Context,
	lockID [32]byte, ops []wire.OutPoint) error {

	f.releaseCallCount++
	f.releaseLockID = lockID
	f.releaseOutpoints = ops

	if f.onRelease != nil {
		return f.onRelease(lockID, ops)
	}

	return nil
}

func (f *fakeWalletController) FinalizePsbt(_ context.Context,
	_ *psbt.Packet) (*wire.MsgTx, error) {

	return nil, errors.New("FinalizePsbt should not be called from " +
		"buildCommitmentTx")
}

// clonePInput makes a deep-enough copy of a psbt.PInput for snapshot
// comparison: leaf hashes, control blocks, and pkScripts are copied so
// later mutations of the source PInput don't bleed into the snapshot.
func clonePInput(in *psbt.PInput) psbt.PInput {
	out := *in
	if in.WitnessUtxo != nil {
		w := *in.WitnessUtxo
		w.PkScript = append([]byte(nil), in.WitnessUtxo.PkScript...)
		out.WitnessUtxo = &w
	}
	if len(in.TaprootBip32Derivation) > 0 {
		copies := make(
			[]*psbt.TaprootBip32Derivation,
			len(in.TaprootBip32Derivation),
		)
		for i, d := range in.TaprootBip32Derivation {
			c := *d
			if len(d.LeafHashes) > 0 {
				c.LeafHashes = make([][]byte, len(d.LeafHashes))
				for j, h := range d.LeafHashes {
					c.LeafHashes[j] = append(
						[]byte(nil), h...,
					)
				}
			}
			copies[i] = &c
		}
		out.TaprootBip32Derivation = copies
	}
	if len(in.TaprootLeafScript) > 0 {
		copies := make(
			[]*psbt.TaprootTapLeafScript,
			len(in.TaprootLeafScript),
		)
		for i, ls := range in.TaprootLeafScript {
			c := *ls
			c.ControlBlock = append(
				[]byte(nil), ls.ControlBlock...,
			)
			c.Script = append([]byte(nil), ls.Script...)
			copies[i] = &c
		}
		out.TaprootLeafScript = copies
	}
	if len(in.TaprootMerkleRoot) > 0 {
		out.TaprootMerkleRoot = append(
			[]byte(nil), in.TaprootMerkleRoot...,
		)
	}
	if len(in.TaprootInternalKey) > 0 {
		out.TaprootInternalKey = append(
			[]byte(nil), in.TaprootInternalKey...,
		)
	}

	return out
}

// findInputIndexByOutpoint returns the position of the input in packet
// matching the given outpoint, or -1 if not found. We look up by
// outpoint because LND's PsbtCoinSelect path may reorder inputs
// during coin selection — index would be an unsafe stand-in. Used by
// the commitment-tx tests to locate boarding inputs after FundPsbt
// has run; production code uses buildInputIndexMap (O(M) map +
// duplicate-aware) for the same job.
func findInputIndexByOutpoint(packet *psbt.Packet,
	outpoint wire.OutPoint) int {

	for i, txIn := range packet.UnsignedTx.TxIn {
		if txIn.PreviousOutPoint == outpoint {
			return i
		}
	}

	return -1
}

// commitmentFixture wires the minimum environment for a buildCommitmentTx
// invocation: terms, fee estimator, wallet controller, and a synthetic
// boarding-input set with deterministic outpoints.
type commitmentFixture struct {
	t               *testing.T
	terms           *batch.Terms
	feeEstimator    *fakeFeeEstimator
	walletCtrl      *fakeWalletController
	operatorKey     *btcec.PublicKey
	boardingInputs  []*BoardingInput
	requiredOutputs []*wire.TxOut
	opts            *FundingOpts
}

// newCommitmentFixture creates a fixture with `numBoarding` boarding inputs
// each worth `boardingValue` sat, a single dust required output, and a
// fixed-rate fee estimator.
func newCommitmentFixture(t *testing.T, numBoarding int,
	boardingValue btcutil.Amount,
	feeRate chainfee.SatPerKWeight) *commitmentFixture {

	t.Helper()

	opPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	opKey := opPriv.PubKey()

	addr, err := btcutil.NewAddressTaproot(
		make([]byte, 32), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	terms := &batch.Terms{
		ConnectorDustAmount:  330,
		ConnectorAddress:     addr,
		MaxConnectorsPerTree: 32,
	}

	bis := make([]*BoardingInput, numBoarding)
	for i := 0; i < numBoarding; i++ {
		var h chainhash.Hash
		h[0] = byte(i + 1)
		op := &wire.OutPoint{Hash: h, Index: uint32(i)}
		bis[i] = buildTestBoardingInput(t, op, boardingValue, opKey)
	}

	// Single non-dust required output worth half the boarding-per-input
	// so total outputs ~= total boarding value (typical round shape).
	halfPerInput := boardingValue / 2
	totalRequired := int64(halfPerInput * btcutil.Amount(numBoarding))
	required := []*wire.TxOut{
		{
			Value:    totalRequired,
			PkScript: dummyP2TRScript(),
		},
	}

	return &commitmentFixture{
		t:               t,
		terms:           terms,
		feeEstimator:    &fakeFeeEstimator{rate: feeRate},
		walletCtrl:      &fakeWalletController{},
		operatorKey:     opKey,
		boardingInputs:  bis,
		requiredOutputs: required,
	}
}

// dummyP2TRScript returns a syntactically valid 34-byte P2TR pkScript with
// a fixed taproot output key. Never sent on-chain; only used as test data.
func dummyP2TRScript() []byte {
	return append([]byte{txscript.OP_1, 0x20}, make([]byte, 32)...)
}

// run invokes buildCommitmentTx with the fixture's parameters and returns
// the result.
func (f *commitmentFixture) run() (*psbt.Packet, int32, []wire.OutPoint,
	error) {

	pkt, changeIdx, _, _, _, locked, err := buildCommitmentTx(
		context.Background(), f.terms, f.feeEstimator, 6,
		f.walletCtrl, 1, "default", f.boardingInputs, nil,
		f.requiredOutputs, nil, f.opts,
	)

	return pkt, changeIdx, locked, err
}

// addWalletInputAndChange simulates LND adding a single wallet input to
// cover the residual plus a change output. The wallet input has
// `walletInputValue`, the change output is set to `changeValue`. Returns
// the change index. If `prepend` is true, the wallet input is inserted
// before any pre-existing boarding inputs (simulating LND reordering).
func addWalletInputAndChange(packet *psbt.Packet,
	walletInputValue, changeValue int64, prepend bool) int32 {

	walletIn := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0xff},
			Index: 0,
		},
		Sequence: wire.MaxTxInSequenceNum,
	}
	walletPin := psbt.PInput{
		WitnessUtxo: &wire.TxOut{
			Value:    walletInputValue,
			PkScript: dummyP2WKHScript(),
		},
	}

	if prepend {
		packet.UnsignedTx.TxIn = append(
			[]*wire.TxIn{walletIn}, packet.UnsignedTx.TxIn...,
		)
		packet.Inputs = append(
			[]psbt.PInput{walletPin}, packet.Inputs...,
		)
	} else {
		packet.UnsignedTx.TxIn = append(
			packet.UnsignedTx.TxIn, walletIn,
		)
		packet.Inputs = append(packet.Inputs, walletPin)
	}

	changeOut := &wire.TxOut{
		Value:    changeValue,
		PkScript: dummyP2WKHScript(),
	}
	packet.UnsignedTx.TxOut = append(
		packet.UnsignedTx.TxOut, changeOut,
	)
	packet.Outputs = append(packet.Outputs, psbt.POutput{})

	return int32(len(packet.UnsignedTx.TxOut) - 1)
}

// dummyP2WKHScript returns a syntactically valid P2WKH pkScript.
func dummyP2WKHScript() []byte {
	return append([]byte{txscript.OP_0, 0x14}, make([]byte, 20)...)
}

// TestBuildCommitmentTx_PreAddsBoardingAsKeySpend asserts that boarding
// inputs are pre-attached with key-spend metadata before FundPsbt is
// called, so LND's EstimateInputWeight accepts them.
func TestBuildCommitmentTx_PreAddsBoardingAsKeySpend(t *testing.T) {
	t.Parallel()

	fix := newCommitmentFixture(t, 3, 5_000_000, 1_000)
	wantOps := make(map[wire.OutPoint]btcutil.Amount, 3)
	for _, bi := range fix.boardingInputs {
		wantOps[*bi.Outpoint] = bi.Value
	}

	fix.walletCtrl.onFund = func(p *psbt.Packet) (
		int32, []wire.OutPoint, error) {

		// Every boarding outpoint must be present at FundPsbt time.
		seen := make(map[wire.OutPoint]bool, len(wantOps))
		for i, in := range p.UnsignedTx.TxIn {
			if _, ok := wantOps[in.PreviousOutPoint]; !ok {
				continue
			}
			seen[in.PreviousOutPoint] = true

			pin := p.Inputs[i]
			require.NotNil(t, pin.WitnessUtxo)
			require.Equal(
				t, int64(wantOps[in.PreviousOutPoint]),
				pin.WitnessUtxo.Value,
			)
			require.Empty(t, pin.TaprootLeafScript,
				"key-spend appearance must omit "+
					"TaprootLeafScript")
			require.Len(t, pin.TaprootBip32Derivation, 1)
			require.Empty(
				t, pin.TaprootBip32Derivation[0].LeafHashes,
				"key-spend appearance requires empty "+
					"LeafHashes",
			)
			require.NotEmpty(
				t, pin.TaprootInternalKey,
				"key-spend needs internal key",
			)
		}
		require.Len(t, seen, len(wantOps),
			"every boarding outpoint must appear at "+
				"FundPsbt time")

		// Simulate funding: add wallet input and change.
		changeIdx := addWalletInputAndChange(
			p, 100_000, 50_000, false,
		)

		return changeIdx, []wire.OutPoint{{
			Hash:  chainhash.Hash{0xff},
			Index: 0,
		}}, nil
	}

	_, _, _, err := fix.run()
	require.NoError(t, err)
	require.Equal(t, 1, fix.walletCtrl.fundCallCount)
}

// TestBuildCommitmentTx_SwapsToScriptSpendPostFund asserts that after
// FundPsbt, every boarding input has been swapped to the real script-spend
// metadata (TaprootLeafScript populated, LeafHashes non-empty).
func TestBuildCommitmentTx_SwapsToScriptSpendPostFund(t *testing.T) {
	t.Parallel()

	fix := newCommitmentFixture(t, 2, 4_000_000, 1_000)

	fix.walletCtrl.onFund = func(p *psbt.Packet) (
		int32, []wire.OutPoint, error) {

		idx := addWalletInputAndChange(p, 50_000, 30_000, false)
		return idx, nil, nil
	}

	pkt, _, _, err := fix.run()
	require.NoError(t, err)

	for _, bi := range fix.boardingInputs {
		idx := findInputIndexByOutpoint(pkt, *bi.Outpoint)
		require.GreaterOrEqual(t, idx, 0)

		pin := pkt.Inputs[idx]
		require.Len(t, pin.TaprootLeafScript, 1)

		collabLeaf := bi.Tapscript.Leaves[0]
		require.Equal(
			t, collabLeaf.Script,
			pin.TaprootLeafScript[0].Script,
		)
		require.Equal(
			t, collabLeaf.LeafVersion,
			pin.TaprootLeafScript[0].LeafVersion,
		)

		require.Len(t, pin.TaprootBip32Derivation, 1)
		require.Len(
			t, pin.TaprootBip32Derivation[0].LeafHashes, 1,
		)

		expectedHash := txscript.NewTapLeaf(
			collabLeaf.LeafVersion, collabLeaf.Script,
		).TapHash()
		require.Equal(
			t, expectedHash[:],
			pin.TaprootBip32Derivation[0].LeafHashes[0],
		)
		require.Equal(
			t, txscript.SigHashDefault, pin.SighashType,
		)
	}
}

// TestBuildCommitmentTx_ChangeAdjustedForWitnessDelta asserts that the
// change output is reduced by exactly the witness-weight-delta fee:
// feeRate.FeeForWeight((scriptW − keySpendW) × N).
func TestBuildCommitmentTx_ChangeAdjustedForWitnessDelta(t *testing.T) {
	t.Parallel()

	for _, n := range []int{1, 3, 8} {
		n := n

		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			t.Parallel()

			feeRate := chainfee.SatPerKWeight(2_500)
			fix := newCommitmentFixture(t, n, 1_000_000, feeRate)

			const startingChange int64 = 200_000
			fix.walletCtrl.onFund = func(p *psbt.Packet) (
				int32, []wire.OutPoint, error) {

				idx := addWalletInputAndChange(
					p, 100_000, startingChange, false,
				)

				return idx, nil, nil
			}

			pkt, changeIdx, _, err := fix.run()
			require.NoError(t, err)

			// Compute the expected delta by serializing a real
			// collab witness for each boarding input — the
			// ground-truth on-chain shape — rather than echoing
			// the implementation's TxWeightEstimator math. This
			// is the non-tautological check: the implementation
			// must reduce change by the actual miner fee the
			// real witness will incur, not by whatever its
			// estimator happens to compute.
			expectedDelta := expectedWitnessDeltaFee(
				t, fix.boardingInputs, feeRate,
			)
			expected := startingChange - expectedDelta
			require.Equal(
				t, expected,
				pkt.UnsignedTx.TxOut[changeIdx].Value,
			)
		})
	}
}

// TestBuildCommitmentTx_FundingAmountReducedByBoardingTotal proves the
// fix from issue #309: LND only needs to fund the residual, not the full
// output value, once boarding inputs are pre-added.
func TestBuildCommitmentTx_FundingAmountReducedByBoardingTotal(t *testing.T) {
	t.Parallel()

	fix := newCommitmentFixture(t, 1, 5_000_000, 1_000)

	var observedResidual int64
	fix.walletCtrl.onFund = func(p *psbt.Packet) (
		int32, []wire.OutPoint, error) {

		var inputSum int64
		for i, in := range p.UnsignedTx.TxIn {
			_ = in
			if p.Inputs[i].WitnessUtxo == nil {
				continue
			}
			inputSum += p.Inputs[i].WitnessUtxo.Value
		}
		var outputSum int64
		for _, out := range p.UnsignedTx.TxOut {
			outputSum += out.Value
		}
		observedResidual = outputSum - inputSum

		idx := addWalletInputAndChange(p, 50_000, 30_000, false)

		return idx, nil, nil
	}

	_, _, _, err := fix.run()
	require.NoError(t, err)

	// Σboarding = 5M, Σoutputs = 2.5M (single required at half), so
	// residual is negative — i.e. the wallet contributes nothing toward
	// the principal and only owes the fee. This is the operator
	// liquidity reduction the issue asked for.
	require.Less(t, observedResidual, int64(0),
		"expected residual to be negative once boarding inputs "+
			"are accounted for (issue #309)")
}

// TestBuildCommitmentTx_NoChangeFailsWithBoarding asserts that boarding
// inputs require a change output to absorb the witness-weight-delta fee.
func TestBuildCommitmentTx_NoChangeFailsWithBoarding(t *testing.T) {
	t.Parallel()

	fix := newCommitmentFixture(t, 2, 1_000_000, 1_000)
	fix.opts = &FundingOpts{LockID: [32]byte{0xab, 0xcd}}

	lockedOps := []wire.OutPoint{
		{Hash: chainhash.Hash{0xaa}, Index: 0},
	}
	fix.walletCtrl.onFund = func(p *psbt.Packet) (
		int32, []wire.OutPoint, error) {

		// Add a wallet input but no change output.
		walletIn := &wire.TxIn{
			PreviousOutPoint: lockedOps[0],
			Sequence:         wire.MaxTxInSequenceNum,
		}
		p.UnsignedTx.TxIn = append(p.UnsignedTx.TxIn, walletIn)
		p.Inputs = append(p.Inputs, psbt.PInput{
			WitnessUtxo: &wire.TxOut{
				Value:    50_000,
				PkScript: dummyP2WKHScript(),
			},
		})

		return -1, lockedOps, nil
	}

	_, _, _, err := fix.run()
	require.ErrorIs(t, err, ErrChangeRequiredForBoarding)
	require.Equal(t, 1, fix.walletCtrl.releaseCallCount,
		"locked outpoints must be released on this error path")
	require.Equal(t, lockedOps, fix.walletCtrl.releaseOutpoints)
}

// TestBuildCommitmentTx_NoBoardingNoChangeIsOK asserts that a refresh-
// only round (no boarding inputs) tolerates no change output.
func TestBuildCommitmentTx_NoBoardingNoChangeIsOK(t *testing.T) {
	t.Parallel()

	fix := newCommitmentFixture(t, 0, 0, 1_000)
	fix.walletCtrl.onFund = func(p *psbt.Packet) (
		int32, []wire.OutPoint, error) {

		// Add only a wallet input, no change.
		walletIn := &wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{0xff},
				Index: 1,
			},
			Sequence: wire.MaxTxInSequenceNum,
		}
		p.UnsignedTx.TxIn = append(p.UnsignedTx.TxIn, walletIn)
		p.Inputs = append(p.Inputs, psbt.PInput{
			WitnessUtxo: &wire.TxOut{
				Value:    100_000,
				PkScript: dummyP2WKHScript(),
			},
		})

		return -1, nil, nil
	}

	_, changeIdx, _, err := fix.run()
	require.NoError(t, err)
	require.Equal(t, int32(-1), changeIdx)
}

// TestBuildCommitmentTx_BoardingInputsReorderedByLnd asserts the swap
// looks up boarding inputs by PreviousOutPoint, not by index, so reorders
// from LND's coin selection don't corrupt the metadata mapping.
func TestBuildCommitmentTx_BoardingInputsReorderedByLnd(t *testing.T) {
	t.Parallel()

	fix := newCommitmentFixture(t, 3, 2_000_000, 1_000)

	fix.walletCtrl.onFund = func(p *psbt.Packet) (
		int32, []wire.OutPoint, error) {

		// Prepend the wallet input so boarding indices shift by 1.
		idx := addWalletInputAndChange(p, 50_000, 75_000, true)
		return idx, nil, nil
	}

	pkt, _, _, err := fix.run()
	require.NoError(t, err)

	for _, bi := range fix.boardingInputs {
		idx := findInputIndexByOutpoint(pkt, *bi.Outpoint)
		require.GreaterOrEqual(t, idx, 1,
			"boarding inputs must have shifted right by the "+
				"prepended wallet input")

		pin := pkt.Inputs[idx]
		require.Len(t, pin.TaprootLeafScript, 1,
			"swap must have applied script-spend metadata "+
				"to the right slot")
	}
}

// TestBuildCommitmentTx_PostSwapFeeExactness asserts that the
// witness-weight-delta charge against change applies a SINGLE
// FeeForWeight truncation, not two. Computing the delta as two
// independent FeeForWeight calls (one per estimator weight) can drift by
// 1 sat from the correct value due to integer truncation; the
// implementation must compute the weight delta first and call
// FeeForWeight once.
func TestBuildCommitmentTx_PostSwapFeeExactness(t *testing.T) {
	t.Parallel()

	for _, n := range []int{1, 2, 5, 8} {
		for _, rate := range []chainfee.SatPerKWeight{
			253, 500, 2_500, 10_000,
		} {
			n, rate := n, rate

			name := fmt.Sprintf(
				"N=%d_rate=%d", n, int64(rate),
			)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				assertSingleTruncation(t, n, rate)
			})
		}
	}
}

// assertSingleTruncation runs buildCommitmentTx and verifies the change
// drop equals feeRate.FeeForWeight(scriptW − keyW) — one call. We then
// also verify it does NOT equal feeRate.FeeForWeight(scriptW) −
// feeRate.FeeForWeight(keyW) when the two computations would differ.
func assertSingleTruncation(t *testing.T, n int,
	feeRate chainfee.SatPerKWeight) {

	t.Helper()

	const startingChange int64 = 500_000

	fix := newCommitmentFixture(t, n, 1_000_000, feeRate)
	fix.walletCtrl.onFund = func(p *psbt.Packet) (
		int32, []wire.OutPoint, error) {

		idx := addWalletInputAndChange(
			p, 100_000, startingChange, false,
		)

		return idx, nil, nil
	}

	pkt, _, _, err := fix.run()
	require.NoError(t, err)

	// Build the expected single- and double-truncation totals from a
	// ground-truth witness measurement instead of echoing the
	// implementation's estimator. The single-truncation form is
	// FeeForWeight(N · deltaW); the double-truncation form is
	// FeeForWeight(N · scriptW) − FeeForWeight(N · keySpendW). They
	// drift by ±1 sat at low fee rates; the contract is that the
	// implementation always uses single truncation.
	var (
		totalDelta  lntypes.WeightUnit
		totalScript lntypes.WeightUnit
		totalKey    lntypes.WeightUnit
	)
	for _, bi := range fix.boardingInputs {
		realW := measureRealCollabWitnessWeight(t, bi)
		totalScript += realW
		totalKey += input.TaprootKeyPathWitnessSize
		totalDelta += realW - input.TaprootKeyPathWitnessSize
	}
	singleTrunc := int64(feeRate.FeeForWeight(totalDelta))
	doubleTrunc := int64(feeRate.FeeForWeight(totalScript)) -
		int64(feeRate.FeeForWeight(totalKey))

	changeVal := pkt.UnsignedTx.TxOut[len(
		pkt.UnsignedTx.TxOut,
	)-1].Value
	gotDelta := startingChange - changeVal

	require.Equal(t, singleTrunc, gotDelta,
		"change must be reduced by FeeForWeight(deltaW), "+
			"computed once (rate=%d, N=%d, want=%d, got=%d)",
		int64(feeRate), n, singleTrunc, gotDelta,
	)

	// When single- and double-truncation diverge (off-by-one due to
	// rounding), the previous require.Equal already proves the
	// implementation picked single. Surface the divergence in the
	// log so a future regression is debuggable.
	if singleTrunc != doubleTrunc {
		t.Logf("single/double truncation diverged at rate=%d N=%d: "+
			"single=%d double=%d (impl correctly picked single)",
			int64(feeRate), n, singleTrunc, doubleTrunc,
		)
	}
}

// TestBuildCommitmentTx_FeeAccountingProperty is a property-based check
// that the witness-weight-delta accounting is exact across a wide range
// of boarding counts and fee rates.
func TestBuildCommitmentTx_FeeAccountingProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 8).Draw(rt, "n")
		valueSat := rapid.Int64Range(
			546, 10_000_000,
		).Draw(rt, "boardingValue")
		rate := chainfee.SatPerKWeight(rapid.Int64Range(
			253, 50_000,
		).Draw(rt, "feeRate"))

		fix := newCommitmentFixture(
			t, n, btcutil.Amount(valueSat), rate,
		)
		// Use a fixed required output so the math is predictable.
		fix.requiredOutputs = []*wire.TxOut{
			{Value: 10_000, PkScript: dummyP2TRScript()},
		}

		const startingChange = int64(500_000)
		fix.walletCtrl.onFund = func(p *psbt.Packet) (
			int32, []wire.OutPoint, error) {

			idx := addWalletInputAndChange(
				p, 1_000_000, startingChange, false,
			)

			return idx, nil, nil
		}

		pkt, _, _, err := fix.run()
		if err != nil {
			rt.Fatalf("buildCommitmentTx: %v", err)
		}

		// Compute the expected delta from the actual on-chain
		// witness shape (real signed wire.TxWitness serialization),
		// independent of the implementation's TxWeightEstimator
		// path. This binds the property test to economic ground
		// truth: the change reduction must equal the miner-fee
		// difference between what LND charged for a key-spend
		// witness and what the network will see at finalize.
		expectedDelta := expectedWitnessDeltaFee(
			t, fix.boardingInputs, rate,
		)

		changeVal := pkt.UnsignedTx.TxOut[len(
			pkt.UnsignedTx.TxOut,
		)-1].Value
		gotDelta := startingChange - changeVal
		if gotDelta != expectedDelta {
			rt.Fatalf("witness-weight delta mismatch: "+
				"want %d, got %d (n=%d, rate=%d)",
				expectedDelta, gotDelta, n, int64(rate))
		}
	})
}

// TestBuildCommitmentTx_EstimatorMatchesRealWitness is a regression
// guard that the per-input witness weight the implementation charges
// against change exactly matches the real on-chain witness weight a
// finalized collab spend produces. This is the load-bearing invariant
// for fee accounting after #309's fix: if the estimator under-charges,
// the operator silently underpays miners; if it over-charges, the
// operator over-pays. The test sweeps boarding counts to ensure the
// estimator scales linearly with N and never drifts from the real
// witness even at large N.
func TestBuildCommitmentTx_EstimatorMatchesRealWitness(t *testing.T) {
	t.Parallel()

	for _, n := range []int{1, 2, 8, 32} {
		n := n

		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			t.Parallel()

			fix := newCommitmentFixture(t, n, 1_000_000, 1_000)

			// Drive the implementation's estimator path: build a
			// script-spend estimator with AddTapscriptInput (the
			// same call buildCommitmentTx makes) alongside a
			// key-spend estimator with AddTaprootKeySpendInput
			// (what LND charged at FundPsbt time). Their
			// difference is the per-N witness-weight delta the
			// implementation bills against change.
			var (
				keyEst    input.TxWeightEstimator
				scriptEst input.TxWeightEstimator
			)
			for _, bi := range fix.boardingInputs {
				keyEst.AddTaprootKeySpendInput(
					txscript.SigHashDefault,
				)

				ts, err := boardingScriptSpendTapscript(bi)
				require.NoError(t, err)
				scriptEst.AddTapscriptInput(
					collabLeafWitnessSize, ts,
				)
			}
			estDelta := scriptEst.Weight() - keyEst.Weight()

			// Independent ground-truth: serialize a real
			// wire.TxWitness for each boarding input, sum the
			// per-input deltas vs the key-spend witness LND
			// charged. This computation never touches the
			// implementation's estimator code path.
			var realDelta lntypes.WeightUnit
			for _, bi := range fix.boardingInputs {
				realW := measureRealCollabWitnessWeight(
					t, bi,
				)
				realDelta += realW -
					input.TaprootKeyPathWitnessSize
			}

			require.Equal(t, realDelta, estDelta,
				"estimator delta must equal real "+
					"witness serialization delta "+
					"(N=%d)", n)
		})
	}
}

// TestBuildCommitmentTx_DuplicateBoardingInSlice asserts that two
// boarding inputs sharing the same outpoint in the boardingInputs
// slice are rejected with ErrDuplicateBoardingOutpoint and that the
// wallet lease is released. This is defense-in-depth against a
// validation-layer bug; the production path is gated upstream by the
// boarding-input locker.
func TestBuildCommitmentTx_DuplicateBoardingInSlice(t *testing.T) {
	t.Parallel()

	fix := newCommitmentFixture(t, 2, 1_000_000, 1_000)
	// Force the second boarding input to share the first's outpoint.
	fix.boardingInputs[1].Outpoint = fix.boardingInputs[0].Outpoint

	fix.opts = &FundingOpts{LockID: [32]byte{0xab}}
	fix.walletCtrl.onFund = func(p *psbt.Packet) (
		int32, []wire.OutPoint, error) {

		idx := addWalletInputAndChange(p, 100_000, 50_000, false)
		return idx, []wire.OutPoint{
			{Hash: chainhash.Hash{0xff}, Index: 0},
		}, nil
	}

	_, _, _, err := fix.run()
	require.ErrorIs(t, err, ErrDuplicateBoardingOutpoint)
	require.Equal(t, 1, fix.walletCtrl.releaseCallCount,
		"lease must be released when duplicate is detected")
}

// TestBuildCommitmentTx_DuplicateOutpointInPacket asserts that a
// funded PSBT returned by LND with the same outpoint at two different
// indices fails with ErrDuplicateBoardingOutpoint. This is a
// defense-in-depth check against a misbehaving wallet backend.
func TestBuildCommitmentTx_DuplicateOutpointInPacket(t *testing.T) {
	t.Parallel()

	fix := newCommitmentFixture(t, 1, 1_000_000, 1_000)
	fix.opts = &FundingOpts{LockID: [32]byte{0xcd}}

	dupOp := *fix.boardingInputs[0].Outpoint
	fix.walletCtrl.onFund = func(p *psbt.Packet) (
		int32, []wire.OutPoint, error) {

		// Inject a wallet input whose outpoint collides with the
		// boarding input's outpoint.
		walletIn := &wire.TxIn{
			PreviousOutPoint: dupOp,
			Sequence:         wire.MaxTxInSequenceNum,
		}
		walletPin := psbt.PInput{
			WitnessUtxo: &wire.TxOut{
				Value:    100_000,
				PkScript: dummyP2WKHScript(),
			},
		}
		p.UnsignedTx.TxIn = append(p.UnsignedTx.TxIn, walletIn)
		p.Inputs = append(p.Inputs, walletPin)

		changeOut := &wire.TxOut{
			Value:    50_000,
			PkScript: dummyP2WKHScript(),
		}
		p.UnsignedTx.TxOut = append(p.UnsignedTx.TxOut, changeOut)
		p.Outputs = append(p.Outputs, psbt.POutput{})

		return int32(len(p.UnsignedTx.TxOut) - 1),
			[]wire.OutPoint{dupOp}, nil
	}

	_, _, _, err := fix.run()
	require.ErrorIs(t, err, ErrDuplicateBoardingOutpoint)
	require.Equal(t, 1, fix.walletCtrl.releaseCallCount,
		"lease must be released when duplicate is detected")
}

// TestBuildInputIndexMap_HappyPath asserts the helper returns a
// 1-to-1 outpoint→index mapping for a packet with unique outpoints.
func TestBuildInputIndexMap_HappyPath(t *testing.T) {
	t.Parallel()

	pkt := &psbt.Packet{UnsignedTx: &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: wire.OutPoint{
				Hash: chainhash.Hash{0x01}, Index: 0,
			}},
			{PreviousOutPoint: wire.OutPoint{
				Hash: chainhash.Hash{0x02}, Index: 1,
			}},
			{PreviousOutPoint: wire.OutPoint{
				Hash: chainhash.Hash{0x01}, Index: 1,
			}},
		},
	}}

	got, err := buildInputIndexMap(pkt)
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, 0, got[wire.OutPoint{
		Hash: chainhash.Hash{0x01}, Index: 0,
	}])
	require.Equal(t, 1, got[wire.OutPoint{
		Hash: chainhash.Hash{0x02}, Index: 1,
	}])
	require.Equal(t, 2, got[wire.OutPoint{
		Hash: chainhash.Hash{0x01}, Index: 1,
	}])
}

// TestBuildCommitmentTx_RejectsLNDDecorationNonWitnessUtxo asserts the
// post-fund PInput swap fails fast with ErrBoardingPInputDecorated if
// LND ever populates a NonWitnessUtxo on the boarding entry. This
// guards against future LND changes that would silently invalidate
// the wholesale swap pattern.
func TestBuildCommitmentTx_RejectsLNDDecorationNonWitnessUtxo(t *testing.T) {
	t.Parallel()

	fix := newCommitmentFixture(t, 1, 1_000_000, 1_000)
	fix.opts = &FundingOpts{LockID: [32]byte{0x11}}
	fix.walletCtrl.onFund = func(p *psbt.Packet) (
		int32, []wire.OutPoint, error) {

		// Locate the boarding input we pre-added and decorate its
		// PInput as if LND had populated NonWitnessUtxo.
		idx := findInputIndexByOutpoint(
			p, *fix.boardingInputs[0].Outpoint,
		)
		require.GreaterOrEqual(t, idx, 0)
		p.Inputs[idx].NonWitnessUtxo = &wire.MsgTx{Version: 2}

		changeIdx := addWalletInputAndChange(p, 100_000, 50_000, false)

		return changeIdx, []wire.OutPoint{
			{Hash: chainhash.Hash{0xff}, Index: 0},
		}, nil
	}

	_, _, _, err := fix.run()
	require.ErrorIs(t, err, ErrBoardingPInputDecorated)
	require.Equal(t, 1, fix.walletCtrl.releaseCallCount,
		"lease must be released when decoration is detected")
}

// TestBuildCommitmentTx_RejectsLNDDecorationPartialSigs asserts the
// swap also rejects PartialSigs decoration (which would indicate LND
// pre-signed the input as wallet-owned).
func TestBuildCommitmentTx_RejectsLNDDecorationPartialSigs(t *testing.T) {
	t.Parallel()

	fix := newCommitmentFixture(t, 1, 1_000_000, 1_000)
	fix.opts = &FundingOpts{LockID: [32]byte{0x22}}
	fix.walletCtrl.onFund = func(p *psbt.Packet) (
		int32, []wire.OutPoint, error) {

		idx := findInputIndexByOutpoint(
			p, *fix.boardingInputs[0].Outpoint,
		)
		require.GreaterOrEqual(t, idx, 0)
		p.Inputs[idx].PartialSigs = []*psbt.PartialSig{
			{
				PubKey:    []byte{0x02, 0xaa},
				Signature: []byte{0x30},
			},
		}

		changeIdx := addWalletInputAndChange(p, 100_000, 50_000, false)

		return changeIdx, nil, nil
	}

	_, _, _, err := fix.run()
	require.ErrorIs(t, err, ErrBoardingPInputDecorated)
}

// TestBuildCommitmentTx_RejectsLNDDecorationBip32 asserts the swap
// rejects non-taproot Bip32Derivation decoration as well.
func TestBuildCommitmentTx_RejectsLNDDecorationBip32(t *testing.T) {
	t.Parallel()

	fix := newCommitmentFixture(t, 1, 1_000_000, 1_000)
	fix.opts = &FundingOpts{LockID: [32]byte{0x33}}
	fix.walletCtrl.onFund = func(p *psbt.Packet) (
		int32, []wire.OutPoint, error) {

		idx := findInputIndexByOutpoint(
			p, *fix.boardingInputs[0].Outpoint,
		)
		require.GreaterOrEqual(t, idx, 0)
		p.Inputs[idx].Bip32Derivation = []*psbt.Bip32Derivation{
			{PubKey: []byte{0x02, 0xbb}, Bip32Path: []uint32{0, 1}},
		}

		changeIdx := addWalletInputAndChange(p, 100_000, 50_000, false)

		return changeIdx, nil, nil
	}

	_, _, _, err := fix.run()
	require.ErrorIs(t, err, ErrBoardingPInputDecorated)
}

// TestAssertBoardingPInputUntouched_HappyPath asserts the helper
// accepts the canonical key-spend appearance built by
// boardingPInputKeySpend.
func TestAssertBoardingPInputUntouched_HappyPath(t *testing.T) {
	t.Parallel()

	fix := newCommitmentFixture(t, 1, 1_000_000, 1_000)
	pin, err := boardingPInputKeySpend(fix.boardingInputs[0])
	require.NoError(t, err)

	require.NoError(t, assertBoardingPInputUntouched(&pin))
}

// TestErrChangeRequiredForBoarding_GatesMetric asserts the load-bearing
// errors.Is gate the FSM transition uses to detect the no-change-with-
// boarding failure mode. The gate must match wrapped errors as well so
// the operator-alert counter increments even when a caller wraps the
// sentinel with additional context.
func TestErrChangeRequiredForBoarding_GatesMetric(t *testing.T) {
	t.Parallel()

	// Direct sentinel: matches.
	require.ErrorIs(t,
		ErrChangeRequiredForBoarding,
		ErrChangeRequiredForBoarding,
	)

	// Wrapped sentinel: matches via errors.Is unwrap.
	wrapped := fmt.Errorf(
		"build commitment tx: %w", ErrChangeRequiredForBoarding,
	)
	require.ErrorIs(t, wrapped, ErrChangeRequiredForBoarding,
		"wrapped sentinel must still trigger the metric gate")

	// Unrelated error: must NOT match.
	other := errors.New("unrelated failure")
	require.NotErrorIs(t, other, ErrChangeRequiredForBoarding)
}

// TestBuildInputIndexMap_DuplicateRejected asserts the helper surfaces
// duplicate outpoints as ErrDuplicateBoardingOutpoint.
func TestBuildInputIndexMap_DuplicateRejected(t *testing.T) {
	t.Parallel()

	op := wire.OutPoint{Hash: chainhash.Hash{0x01}, Index: 0}
	pkt := &psbt.Packet{UnsignedTx: &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op},
			{PreviousOutPoint: wire.OutPoint{
				Hash: chainhash.Hash{0x02},
			}},
			{PreviousOutPoint: op},
		},
	}}

	_, err := buildInputIndexMap(pkt)
	require.ErrorIs(t, err, ErrDuplicateBoardingOutpoint)
}
