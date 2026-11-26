//go:build systest

package systest

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/customanchor"
	tapgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/harness"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/lib/types"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/rpc/oorpb"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/tapassets"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	// assetOORExitDelay is the short CSV exit delay used by the OOR
	// unilateral-exit and claim systest so regtest mining stays fast while
	// satisfying the minimum OOR checkpoint CSV delay (10 blocks).
	assetOORExitDelay = uint32(10)

	// assetOORBatchSweepDelay is the operator sweep delay committed into
	// the test commitment transaction's batch outputs.
	assetOORBatchSweepDelay = uint32(500)

	// assetOORMinVTXOSat is the minimum VTXO value and per-leaf carrier
	// budget for the test operator.
	assetOORMinVTXOSat = int64(1_000)

	// assetOORFloatValueSat is the value of the operator carrier float
	// VTXO leased to fund new asset leaves in the OOR transfer.
	assetOORFloatValueSat = int64(10_000)

	// assetOORLeaf0Units and assetOORLeaf1Units are Alice's two input
	// asset VTXOs merged by SendAssetOOR.
	assetOORLeaf0Units = uint64(400)
	assetOORLeaf1Units = uint64(300)

	// assetOORSendUnits is the amount Alice sends to Bob, forcing a merge
	// of both leaves (400 + 300 -> 550 to Bob + 150 asset change).
	assetOORSendUnits = uint64(550)
)

// TestAssetOORMergeAndClaim exercises the real embedded Taproot Assets OOR
// lifecycle end to end on regtest: Alice merges two confirmed-round asset
// VTXOs (400 + 300 units) via SendAssetOOR using an operator-leased carrier
// float, Bob receives and verifies the merged 550-unit VTXO (persisting its
// sealed Ark asset package) while Alice receives her 150-unit asset change
// and 2,000-sat carrier return, and Bob finally unrolls and claims all 550
// units unilaterally on-chain into his real embedded asset wallet.
func TestAssetOORMergeAndClaim(t *testing.T) {
	ParallelN(t)

	h := NewSysTestHarness(t, func(opts *harness.Options) {
		opts.StartTapd = true
	})
	h.Harness.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	issuer := newAssetTestTapd(t, h)
	ref := mintV1AssetTestAsset(
		t, h, issuer, assetOORLeaf0Units+assetOORLeaf1Units,
	)

	op := newAssetOORTestOperator(t, ref)

	aliceDir := t.TempDir()
	bobDir := t.TempDir()
	alice := startEmbeddedAssetDaemonAt(t, h, aliceDir, op.mailboxAddr)
	bob := startEmbeddedAssetDaemonAt(t, h, bobDir, op.mailboxAddr)

	// Give Bob extra confirmed wallet UTXOs so parallel CPFP bumps across
	// his multi-input unroll DAG do not wait on change from a single UTXO.
	fundDaemonWalletExtraUTXOs(t, h, bob, 3)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	// Allocate two receive scripts on Alice (so her wallet owns both real
	// leaf keys) and one receive script on Bob (also registering Bob's
	// routing script with the fake operator).
	aliceRecv0, err := alice.client.NewReceiveScript(
		ctx, &waverpc.NewReceiveScriptRequest{
			Label: "alice-leaf-0",
		},
	)
	require.NoError(t, err)
	aliceRecv1, err := alice.client.NewReceiveScript(
		ctx, &waverpc.NewReceiveScriptRequest{
			Label: "alice-leaf-1",
		},
	)
	require.NoError(t, err)
	bobRecv, err := bob.client.NewReceiveScript(
		ctx, &waverpc.NewReceiveScriptRequest{
			Label: "bob-recv",
		},
	)
	require.NoError(t, err)

	// Stop Alice briefly so we can inspect her persisted receive-key
	// descriptors and seed two real asset VTXOs backed by a real confirmed
	// commitment transaction and signed asset tree.
	alice.stop()

	aliceKey0 := lookupOwnedReceiveKey(
		t, alice.cfg, aliceRecv0.PkScriptHex,
	)
	aliceKey1 := lookupOwnedReceiveKey(
		t, alice.cfg, aliceRecv1.PkScriptHex,
	)

	roundFixture := buildRealAssetOORRoundFixture(
		t, h, issuer, ref, op, aliceKey0, aliceKey1,
	)
	op.installRoundFixture(roundFixture)

	seedRealAssetVTXOs(t, alice.cfg, op, roundFixture)

	// Restart Alice so her VTXO manager loads both real live asset VTXOs.
	alice = startEmbeddedAssetDaemonAt(t, h, aliceDir, op.mailboxAddr)

	require.Eventually(t, func() bool {
		return len(listAllVTXOs(t, alice.client)) == 2
	}, 30*time.Second, 200*time.Millisecond,
		"Alice never loaded seeded asset VTXOs")

	// Alice merges both asset VTXOs (400 + 300) and sends 550 units to Bob.
	bobPubkeyBytes, err := hex.DecodeString(bobRecv.PubkeyXonlyHex)
	require.NoError(t, err)

	sendResp, err := alice.client.SendAssetOOR(
		ctx, &waverpc.SendAssetOORRequest{
			Destination: &waverpc.SendAssetOORRequest_Pubkey{
				Pubkey: bobPubkeyBytes,
			},
			AssetRef: ref.String(),
			Amount:   assetOORSendUnits,
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", sendResp.Status)
	require.EqualValues(t, 2, sendResp.SelectedInputCount)
	require.EqualValues(
		t, assetOORLeaf0Units+assetOORLeaf1Units-assetOORSendUnits,
		sendResp.AssetChangeAmount,
	)
	require.EqualValues(
		t, 2*assetOORMinVTXOSat, sendResp.SenderBtcChangeSat,
	)
	require.False(t, sendResp.FailedBeforePonr)
	require.NotEmpty(t, sendResp.RecipientOutpoint)

	bobOutpoint, err := wire.NewOutPointFromString(
		sendResp.RecipientOutpoint,
	)
	require.NoError(t, err)

	// Wait for Bob to receive, authenticate, verify on his real embedded
	// engine, and materialize the merged 550-unit asset VTXO.
	var bobVTXO *waverpc.VTXO
	require.Eventually(t, func() bool {
		v := findVTXOByOutpoint(
			listAllVTXOs(t, bob.client), *bobOutpoint,
		)
		if v == nil || v.Status != waverpc.VTXOStatus_VTXO_STATUS_LIVE {
			return false
		}
		bobVTXO = v

		return true
	}, time.Minute, 250*time.Millisecond,
		"Bob never materialized incoming merged asset VTXO %s",
		bobOutpoint)

	require.EqualValues(t, assetOORMinVTXOSat, bobVTXO.AmountSat)
	require.EqualValues(t, assetOORSendUnits, bobVTXO.AssetAmount)
	require.Equal(t, ref.String(), bobVTXO.AssetRef)
	require.NotEmpty(t, bobVTXO.AssetRoot)

	// Also verify Alice's two old asset VTXOs are spent and she received
	// both her 150-unit asset change VTXO and her 2,000-sat BTC carrier
	// return VTXO.
	require.Eventually(t, func() bool {
		listCtx, listCancel := context.WithTimeout(
			t.Context(), 5*time.Second,
		)
		defer listCancel()

		resp, err := alice.client.ListVTXOs(
			listCtx, &waverpc.ListVTXOsRequest{
				Statuses: []waverpc.VTXOStatus{
					waverpc.VTXOStatus_VTXO_STATUS_LIVE,
					waverpc.VTXOStatus_VTXO_STATUS_SPENT,
				},
			},
		)
		if err != nil {
			return false
		}

		var (
			spentOld     int
			liveAssetChg bool
			liveBTCChg   bool
		)
		leaf0OP := roundFixture.leaf0.Outpoint.String()
		leaf1OP := roundFixture.leaf1.Outpoint.String()
		for _, v := range resp.Vtxos {
			switch {
			case v.Outpoint == leaf0OP || v.Outpoint == leaf1OP:
				if v.Status ==
					waverpc.VTXOStatus_VTXO_STATUS_SPENT {

					spentOld++
				}

			case v.Status == waverpc.VTXOStatus_VTXO_STATUS_LIVE &&
				v.AssetAmount == 150 &&
				v.AmountSat == assetOORMinVTXOSat:

				liveAssetChg = true

			case v.Status == waverpc.VTXOStatus_VTXO_STATUS_LIVE &&
				v.AssetAmount == 0 &&
				v.AmountSat == 2*assetOORMinVTXOSat:

				liveBTCChg = true
			}
		}

		return spentOld == 2 && liveAssetChg && liveBTCChg
	}, time.Minute, 250*time.Millisecond,
		"Alice did not settle spent inputs and change VTXOs")

	// Trigger a real unilateral exit on Bob's merged OOR asset VTXO. Bob's
	// unroll subsystem broadcasts the asset tree, float tree, all three
	// checkpoints, and the Ark tx, waits for CSV maturity, and invokes
	// assetVTXOClaimer on Bob's real embedded asset engine.
	unrollResp, err := bob.client.Unroll(ctx, &waverpc.UnrollRequest{
		Outpoint: bobOutpoint.String(),
	})
	require.NoError(t, err)
	require.NotEmpty(t, unrollResp.ActorId)

	// Drive block production on regtest as Bob's unroll broadcasts each
	// tier of the DAG, matures the CSV exit delay, and finalizes the claim.
	require.Eventually(t, func() bool {
		h.Harness.Generate(1)

		return embeddedAssetUnits(t, bob.client, ref) ==
			assetOORSendUnits
	}, 3*time.Minute, 2*time.Second,
		"Bob never unilaterally claimed 550 merged asset units")
}

// assetOORLeafSpec holds one real materialized asset leaf ready to seed into
// Alice's database and index on the fake operator.
type assetOORLeafSpec struct {
	Outpoint       wire.OutPoint
	Amount         btcutil.Amount
	PolicyTemplate []byte
	PkScript       []byte
	ClientKey      keychain.KeyDescriptor
	TapScript      *vtxo.Descriptor
	AssetRoot      chainhash.Hash
	AssetAmount    uint64
	SealedPackage  []byte
	TreePath       *tree.Tree
}

// assetOORRoundFixture holds the real confirmed commitment tx, signed asset
// tree, and signed carrier float tree for the systest.
type assetOORRoundFixture struct {
	roundID        round.RoundID
	ref            tapsdk.AssetRef
	commitTx       *wire.MsgTx
	commitTxID     chainhash.Hash
	commitHeight   int32
	batchExpiry    int32
	sweepLeaf      txscript.TapLeaf
	sweepRoot      chainhash.Hash
	assetTree      *tree.Tree
	floatTree      *tree.Tree
	leaf0          assetOORLeafSpec
	leaf1          assetOORLeafSpec
	floatOutpoint  wire.OutPoint
	floatPolicy    []byte
	floatPkScript  []byte
	floatValueSat  int64
	floatTreePath  *tree.Tree
	assetTreePath0 *tree.Tree
	assetTreePath1 *tree.Tree
}

// indexedOOROutput records one finalized OOR recipient output indexed by the
// fake operator for ListVTXOsByScripts and ListOORRecipientEventsByScript.
type indexedOOROutput struct {
	sessionID        chainhash.Hash
	eventID          uint64
	outputIndex      uint32
	value            uint64
	composedPkScript []byte
	routingPkScript  []byte
	policyTemplate   []byte
	assetRoot        []byte
	assetRef         string
	assetAmount      uint64
	arkPSBT          []byte
	checkpointPSBTs  [][]byte
	assetTransfer    []byte
	ancestryPaths    []*arkrpc.AncestryPath
}

// submittedOORSession stores the submit request and cosigned Ark PSBT between
// SubmitPackage and FinalizePackage.
type submittedOORSession struct {
	req         *oorpb.SubmitPackageRequest
	cosignedArk []byte
}

// assetOORTestOperator wraps fakeMailboxServer with real Taproot OOR
// cosigning, carrier float leasing, receive-script registration, and indexer
// queries so two real waved instances can execute an end-to-end asset OOR
// transfer without mocking any daemon subsystem.
type assetOORTestOperator struct {
	t           *testing.T
	ref         tapsdk.AssetRef
	operatorKey *btcec.PrivateKey
	carrierKey  *btcec.PrivateKey
	mailboxAddr string
	mailbox     *fakeMailboxServer

	mu              sync.Mutex
	fixture         *assetOORRoundFixture
	registeredByHex map[string]string
	submittedByID   map[chainhash.Hash]*submittedOORSession
	indexedOutputs  []indexedOOROutput
	nextEventID     uint64
}

// newAssetOORTestOperator starts a fake operator configured for asset OOR
// transfers with a short exit delay and a carrier float key.
func newAssetOORTestOperator(t *testing.T,
	ref tapsdk.AssetRef) *assetOORTestOperator {

	t.Helper()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	carrierPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorInfo := &arkrpc.GetInfoResponse{
		Pubkey:             operatorPriv.PubKey().SerializeCompressed(),
		BoardingExitDelay:  assetOORExitDelay,
		VtxoExitDelay:      assetOORExitDelay,
		DustLimit:          testDustLimitSat,
		MinVtxoAmountSat:   assetOORMinVTXOSat,
		MinOperatorFee:     testOperatorFeeSat,
		MinConfirmations:   1,
		SelectedArkVersion: arkrpc.ArkProtocolVersionV1,
		ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{{
			Version: arkrpc.ArkProtocolVersionV1,
			State:   arkrpc.ArkVersionPolicy_STATE_ACTIVE,
		}},
		Assets: []*arkrpc.AssetPolicy{{
			AssetRef: ref.String(),
		}},
		OorCarrierPubkey: carrierPriv.PubKey().SerializeCompressed(),
	}

	mailboxAddr, mailbox, stopMailbox := startFakeMailboxServer(
		t,
		serverconn.PubKeyMailboxID(
			operatorPriv.PubKey(),
		),
		operatorInfo,
	)
	t.Cleanup(stopMailbox)
	mailbox.setFeeQuote(&arkrpc.EstimateFeeResponse{
		TotalFeeSat: testOperatorFeeSat,
	})

	op := &assetOORTestOperator{
		t:               t,
		ref:             ref,
		operatorKey:     operatorPriv,
		carrierKey:      carrierPriv,
		mailboxAddr:     mailboxAddr,
		mailbox:         mailbox,
		registeredByHex: make(map[string]string),
		submittedByID:   make(map[chainhash.Hash]*submittedOORSession),
	}

	mailbox.mu.Lock()
	mailbox.envelopeHandler = op.handleEnvelope
	mailbox.mu.Unlock()

	return op
}

// installRoundFixture attaches the confirmed commitment and trees to the fake
// operator so LeaseOORCarrier and ListVTXOsByScripts return real ancestry.
func (o *assetOORTestOperator) installRoundFixture(f *assetOORRoundFixture) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.fixture = f
}

// handleEnvelope intercepts operator and indexer mailbox RPCs required for an
// end-to-end asset OOR send and receive.
func (o *assetOORTestOperator) handleEnvelope(ctx context.Context,
	env *mailboxpb.Envelope) (bool, error) {

	_ = ctx
	if env.Rpc == nil {
		return false, nil
	}

	switch {
	case env.Rpc.Service == "arkrpc.ArkService" &&
		env.Rpc.Method == "LeaseOORCarrier":
		return true, o.handleLeaseOORCarrier(env)

	case env.Rpc.Service == "arkrpc.IndexerService" &&
		env.Rpc.Method == "RegisterReceiveScript":
		return true, o.handleRegisterReceiveScript(env)

	case env.Rpc.Service == "arkrpc.IndexerService" &&
		env.Rpc.Method == "ListOORRecipientEventsByScript":
		return true, o.handleListOORRecipientEventsByScript(env)

	case env.Rpc.Service == "arkrpc.IndexerService" &&
		env.Rpc.Method == "ListVTXOsByScripts":
		return true, o.handleListVTXOsByScripts(env)

	case env.Rpc.Service == oorpb.ServiceName &&
		env.Rpc.Method == oorpb.MethodSubmitPackage:
		return true, o.handleSubmitPackage(env)

	case env.Rpc.Service == oorpb.ServiceName &&
		env.Rpc.Method == oorpb.MethodFinalizePackage:
		return true, o.handleFinalizePackage(env)

	case env.Rpc.Service == oorpb.ServiceName &&
		env.Rpc.Method == oorpb.MethodIncomingAck:
		return true, nil

	default:
		return false, nil
	}
}

// replyUnary enqueues a KIND_RESPONSE mailbox envelope back to the caller.
func (o *assetOORTestOperator) replyUnary(env *mailboxpb.Envelope,
	sender string, msg proto.Message) error {

	body, err := anypb.New(msg)
	if err != nil {
		return fmt.Errorf("wrap response %T: %w", msg, err)
	}

	recipient := env.Rpc.ReplyTo
	if recipient == "" {
		recipient = env.Sender
	}

	o.mailbox.enqueueEnvelope(&mailboxpb.Envelope{
		ProtocolVersion:    env.ProtocolVersion,
		ArkProtocolVersion: env.ArkProtocolVersion,
		Sender:             sender,
		Recipient:          recipient,
		CreatedAtUnixMs:    time.Now().UnixMilli(),
		Body:               body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
			Service:       env.Rpc.Service,
			Method:        env.Rpc.Method,
			CorrelationId: env.Rpc.CorrelationId,
		},
	})

	return nil
}

// handleRegisterReceiveScript records the daemon mailbox owning each
// registered uncomposed receive script so IncomingOOREvent can be pushed.
func (o *assetOORTestOperator) handleRegisterReceiveScript(
	env *mailboxpb.Envelope) error {

	var req arkrpc.RegisterReceiveScriptRequest
	if err := env.Body.UnmarshalTo(&req); err != nil {
		return fmt.Errorf("decode RegisterReceiveScript: %w", err)
	}

	o.mu.Lock()
	o.registeredByHex[hex.EncodeToString(req.PkScript)] = env.Sender
	o.mu.Unlock()

	return o.replyUnary(
		env, serverconn.IndexerMailboxID,
		&arkrpc.RegisterReceiveScriptResponse{},
	)
}

// handleLeaseOORCarrier leases the fixture's confirmed carrier float VTXO.
func (o *assetOORTestOperator) handleLeaseOORCarrier(
	env *mailboxpb.Envelope) error {

	var req arkrpc.LeaseOORCarrierRequest
	if err := env.Body.UnmarshalTo(&req); err != nil {
		return fmt.Errorf("decode LeaseOORCarrier: %w", err)
	}

	o.mu.Lock()
	f := o.fixture
	o.mu.Unlock()
	if f == nil {
		return fmt.Errorf("round fixture not installed")
	}

	resp := &arkrpc.LeaseOORCarrierResponse{
		Outpoint:           f.floatOutpoint.String(),
		ValueSat:           f.floatValueSat,
		VtxoPolicyTemplate: bytes.Clone(f.floatPolicy),
		PkScript:           bytes.Clone(f.floatPkScript),
		ExpiresAtUnix:      time.Now().Add(5 * time.Minute).Unix(),
	}

	return o.replyUnary(env, o.mailbox.operatorMailbox, resp)
}

// handleSubmitPackage cosigns every checkpoint and Ark input with the operator
// key, signs the leased carrier float checkpoint and Ark input with the
// carrier key, and returns SubmitPackageResponse.
func (o *assetOORTestOperator) handleSubmitPackage(
	env *mailboxpb.Envelope) error {

	var req oorpb.SubmitPackageRequest
	if err := env.Body.UnmarshalTo(&req); err != nil {
		return fmt.Errorf("decode SubmitPackage: %w", err)
	}

	arkPkt, err := psbtutil.Parse(req.ArkPsbt)
	if err != nil {
		return fmt.Errorf("parse submit ark psbt: %w", err)
	}
	checkpoints := make([]*psbt.Packet, len(req.CheckpointPsbts))
	for i, raw := range req.CheckpointPsbts {
		cp, err := psbtutil.Parse(raw)
		if err != nil {
			return fmt.Errorf("parse submit checkpoint %d: %w", i,
				err)
		}
		checkpoints[i] = cp
	}

	if _, err := oortx.ValidateSubmitPackage(
		arkPkt, checkpoints,
	); err != nil {
		return fmt.Errorf("validate submit package: %w", err)
	}

	o.mu.Lock()
	f := o.fixture
	o.mu.Unlock()
	if f == nil {
		return fmt.Errorf("round fixture not installed")
	}

	// Attach the spent VTXO's collaborative tapleaf (composed when asset-
	// bearing) and sign every checkpoint with the operator key; sign the
	// carrier float checkpoint with both carrier and operator keys.
	if len(req.SigningDescriptors) != len(checkpoints) {
		return fmt.Errorf("signing descriptor count %d != checkpoint "+
			"count %d", len(req.SigningDescriptors),
			len(checkpoints))
	}

	var floatCheckpointTxID chainhash.Hash
	for i, cp := range checkpoints {
		if err := attachCheckpointCollabLeaf(
			cp, req.SigningDescriptors[i],
		); err != nil {
			return fmt.Errorf("attach checkpoint %d leaf: %w", i,
				err)
		}

		isFloat := cp.UnsignedTx.TxIn[0].PreviousOutPoint ==
			f.floatOutpoint
		if err := signTaprootScriptInput(
			cp, 0, o.operatorKey,
		); err != nil {
			return fmt.Errorf("operator sign checkpoint %d: %w", i,
				err)
		}
		if isFloat {
			if err := signTaprootScriptInput(
				cp, 0, o.carrierKey,
			); err != nil {
				return fmt.Errorf("carrier sign float "+
					"checkpoint %d: %w", i, err)
			}
			floatCheckpointTxID = cp.UnsignedTx.TxHash()
		}
	}

	// Sign every Ark input with the operator key; also sign the carrier
	// float Ark input with the carrier key.
	for i := range arkPkt.UnsignedTx.TxIn {
		if err := signTaprootScriptInput(
			arkPkt, i, o.operatorKey,
		); err != nil {
			return fmt.Errorf("operator sign ark input %d: %w", i,
				err)
		}
		if arkPkt.UnsignedTx.TxIn[i].PreviousOutPoint.Hash ==
			floatCheckpointTxID {

			if err := signTaprootScriptInput(
				arkPkt, i, o.carrierKey,
			); err != nil {
				return fmt.Errorf("carrier sign float ark "+
					"input %d: %w", i, err)
			}
		}
	}

	arkRaw, err := psbtutil.Serialize(arkPkt)
	if err != nil {
		return fmt.Errorf("serialize cosigned ark: %w", err)
	}
	cpRaws := make([][]byte, len(checkpoints))
	for i, cp := range checkpoints {
		raw, err := psbtutil.Serialize(cp)
		if err != nil {
			return fmt.Errorf("serialize cosigned cp %d: %w", i,
				err)
		}
		cpRaws[i] = raw
	}

	sessionID := arkPkt.UnsignedTx.TxHash()
	clonedReq, ok := proto.Clone(&req).(*oorpb.SubmitPackageRequest)
	if !ok {
		return fmt.Errorf("clone submit request failed")
	}
	o.mu.Lock()
	o.submittedByID[sessionID] = &submittedOORSession{
		req:         clonedReq,
		cosignedArk: bytes.Clone(arkRaw),
	}
	o.mu.Unlock()

	resp := &oorpb.SubmitPackageResponse{
		Result: &oorpb.SubmitPackageResponse_Success{
			Success: &oorpb.SubmitPackageSuccess{
				SessionId:               sessionID[:],
				CoSignedArkPsbt:         arkRaw,
				CoSignedCheckpointPsbts: cpRaws,
			},
		},
	}

	return o.replyUnary(env, o.mailbox.operatorMailbox, resp)
}

// handleFinalizePackage validates the client-finalized checkpoints and Ark
// PSBT, indexes each recipient output, replies FinalizePackageResponse, and
// pushes IncomingOOREvent notifications to every registered recipient daemon.
func (o *assetOORTestOperator) handleFinalizePackage(
	env *mailboxpb.Envelope) error {

	var req oorpb.FinalizePackageRequest
	if err := env.Body.UnmarshalTo(&req); err != nil {
		return fmt.Errorf("decode FinalizePackage: %w", err)
	}
	if len(req.SessionId) != chainhash.HashSize {
		return fmt.Errorf("invalid finalize session id length %d",
			len(req.SessionId))
	}
	var sessionID chainhash.Hash
	copy(sessionID[:], req.SessionId)

	o.mu.Lock()
	submitted := o.submittedByID[sessionID]
	f := o.fixture
	o.mu.Unlock()
	if submitted == nil || f == nil {
		return fmt.Errorf("unknown OOR session %s", sessionID)
	}

	arkPkt, err := psbtutil.Parse(submitted.cosignedArk)
	if err != nil {
		return fmt.Errorf("parse cosigned ark psbt: %w", err)
	}
	checkpoints := make([]*psbt.Packet, len(req.FinalCheckpointPsbts))
	for i, raw := range req.FinalCheckpointPsbts {
		cp, err := psbtutil.Parse(raw)
		if err != nil {
			return fmt.Errorf("parse finalize checkpoint %d: %w", i,
				err)
		}
		checkpoints[i] = cp
	}

	if err := oortx.ValidateFinalizePackageSigned(
		arkPkt, checkpoints,
	); err != nil {
		return fmt.Errorf("validate finalize package signed: %w", err)
	}

	submitReq := submitted.req

	// Build ancestry paths spanning both the asset tree (with
	// BatchAssetRoot) and the operator carrier float tree.
	assetAncestry, err := buildProtoAncestryPath(
		f.assetTree, f.commitTxID, f.commitHeight,
		o.operatorKey.PubKey(), assetOORBatchSweepDelay, []uint32{0, 1},
	)
	if err != nil {
		return fmt.Errorf("build asset ancestry path: %w", err)
	}
	floatAncestry, err := buildProtoAncestryPath(
		f.floatTree, f.commitTxID, f.commitHeight,
		o.operatorKey.PubKey(), assetOORBatchSweepDelay, []uint32{2},
	)
	if err != nil {
		return fmt.Errorf("build float ancestry path: %w", err)
	}
	ancestryPaths := []*arkrpc.AncestryPath{assetAncestry, floatAncestry}

	type pendingPush struct {
		mailboxID string
		event     *arkrpc.IncomingOOREvent
	}
	var pushes []pendingPush

	o.mu.Lock()
	for outIdx, rec := range submitReq.RecipientOutputs {
		if outIdx >= len(arkPkt.UnsignedTx.TxOut) {
			o.mu.Unlock()

			return fmt.Errorf("recipient output %d out of range",
				outIdx)
		}

		// Derive the uncomposed routing script from the policy
		// template so registered receive scripts match both asset and
		// plain Bitcoin outputs.
		routingScript := bytes.Clone(rec.PkScript)
		if len(rec.VtxoPolicyTemplate) > 0 {
			tmpl, err := arkscript.DecodePolicyTemplate(
				rec.VtxoPolicyTemplate,
			)
			if err != nil {
				o.mu.Unlock()

				return fmt.Errorf("decode recipient %d "+
					"policy: %w", outIdx, err)
			}
			routingScript, err = tmpl.PkScript()
			if err != nil {
				o.mu.Unlock()

				return fmt.Errorf("derive recipient %d "+
					"routing script: %w", outIdx, err)
			}
		}

		o.nextEventID++
		eventID := o.nextEventID

		var assetTransfer []byte
		if len(rec.TaprootAssetRoot) > 0 {
			assetTransfer = bytes.Clone(
				submitReq.TaprootAssetTransfer,
			)
		}

		out := indexedOOROutput{
			sessionID:        sessionID,
			eventID:          eventID,
			outputIndex:      uint32(outIdx),
			value:            uint64(rec.ValueSat),
			composedPkScript: bytes.Clone(rec.PkScript),
			routingPkScript:  routingScript,
			policyTemplate:   bytes.Clone(rec.VtxoPolicyTemplate),
			assetRoot:        bytes.Clone(rec.TaprootAssetRoot),
			assetRef:         rec.TaprootAssetRef,
			assetAmount:      rec.TaprootAssetAmount,
			arkPSBT:          bytes.Clone(submitted.cosignedArk),
			checkpointPSBTs: cloneByteSlices(
				req.FinalCheckpointPsbts,
			),
			assetTransfer: assetTransfer,
			ancestryPaths: ancestryPaths,
		}
		o.indexedOutputs = append(o.indexedOutputs, out)

		if mbID, ok := o.registeredByHex[hex.EncodeToString(
			routingScript,
		)]; ok {

			pushes = append(pushes, pendingPush{
				mailboxID: mbID,
				event: &arkrpc.IncomingOOREvent{
					RecipientPkScript: routingScript,
					RecipientEventId:  eventID,
					SessionId:         sessionID[:],
					OutputIndex:       uint32(outIdx),
					Value:             uint64(rec.ValueSat),
				},
			})
		}
	}
	o.mu.Unlock()

	// Acknowledge finalization to Alice first, then deliver push hints to
	// each registered recipient daemon.
	if err := o.replyUnary(
		env, o.mailbox.operatorMailbox,
		&oorpb.FinalizePackageResponse{
			SessionId: sessionID[:],
		},
	); err != nil {
		return err
	}

	for _, push := range pushes {
		body, err := anypb.New(push.event)
		if err != nil {
			return fmt.Errorf("wrap IncomingOOREvent: %w", err)
		}
		o.mailbox.enqueueEnvelope(&mailboxpb.Envelope{
			ProtocolVersion:    env.ProtocolVersion,
			ArkProtocolVersion: env.ArkProtocolVersion,
			Sender:             serverconn.IndexerMailboxID,
			Recipient:          push.mailboxID,
			CreatedAtUnixMs:    time.Now().UnixMilli(),
			Body:               body,
			Rpc: &mailboxpb.RpcMeta{
				Kind:    mailboxpb.RpcMeta_KIND_EVENT,
				Service: "arkrpc.ArkService",
				Method:  waved.MethodIncomingOOR,
				ReplyTo: serverconn.IndexerMailboxID,
			},
		})
	}

	return nil
}

// handleListOORRecipientEventsByScript returns indexed recipient events
// matching the requested uncomposed routing script.
func (o *assetOORTestOperator) handleListOORRecipientEventsByScript(
	env *mailboxpb.Envelope) error {

	var req arkrpc.ListOORRecipientEventsByScriptRequest
	if err := env.Body.UnmarshalTo(&req); err != nil {
		return fmt.Errorf("decode ListOORRecipientEventsByScript: %w",
			err)
	}

	o.mu.Lock()
	var events []*arkrpc.OORRecipientEvent
	for _, out := range o.indexedOutputs {
		if !bytes.Equal(out.routingPkScript, req.PkScript) {
			continue
		}
		if out.eventID <= req.AfterEventId {
			continue
		}
		events = append(events, &arkrpc.OORRecipientEvent{
			RecipientPkScript: bytes.Clone(out.composedPkScript),
			EventId:           out.eventID,
			SessionId:         bytes.Clone(out.sessionID[:]),
			OutputIndex:       out.outputIndex,
			Value:             out.value,
			ArkPsbt:           bytes.Clone(out.arkPSBT),
			CheckpointPsbts: cloneByteSlices(
				out.checkpointPSBTs,
			),
			VtxoPolicyTemplate:   bytes.Clone(out.policyTemplate),
			TaprootAssetRoot:     bytes.Clone(out.assetRoot),
			TaprootAssetRef:      out.assetRef,
			TaprootAssetAmount:   out.assetAmount,
			TaprootAssetTransfer: bytes.Clone(out.assetTransfer),
		})
	}
	o.mu.Unlock()

	return o.replyUnary(
		env, serverconn.IndexerMailboxID,
		&arkrpc.ListOORRecipientEventsByScriptResponse{
			Events: events,
		},
	)
}

// handleListVTXOsByScripts returns indexed VTXO records with full ancestry for
// every requested script.
func (o *assetOORTestOperator) handleListVTXOsByScripts(
	env *mailboxpb.Envelope) error {

	var req arkrpc.ListVTXOsByScriptsRequest
	if err := env.Body.UnmarshalTo(&req); err != nil {
		return fmt.Errorf("decode ListVTXOsByScripts: %w", err)
	}

	o.mu.Lock()
	f := o.fixture
	outputs := append([]indexedOOROutput(nil), o.indexedOutputs...)
	o.mu.Unlock()
	if f == nil {
		return fmt.Errorf("round fixture not installed")
	}

	var matched []*arkrpc.VTXO
	for _, scope := range req.Scripts {
		wantScript := scope.GetPkScript()
		for _, out := range outputs {
			if !bytes.Equal(out.composedPkScript, wantScript) {
				continue
			}
			matched = append(matched, &arkrpc.VTXO{
				Outpoint: &arkrpc.OutPoint{
					Txid: bytes.Clone(out.sessionID[:]),
					Vout: out.outputIndex,
				},
				ValueSat: out.value,
				PkScript: bytes.Clone(out.composedPkScript),
				Status:   arkrpc.VTXOStatus_VTXO_STATUS_LIVE,
				RoundId:  f.roundID.String(),
				CommitmentTxid: bytes.Clone(
					f.commitTxID[:],
				),
				CreatedHeight:     f.commitHeight,
				BatchExpiryHeight: f.batchExpiry,
				RelativeExpiry:    assetOORExitDelay,
				ChainDepth:        1,
				OperatorPubkey: o.operatorKey.PubKey().
					SerializeCompressed(),
				ConstructionVersion: 1,
				AncestryPaths:       out.ancestryPaths,
				AncestryPackages: []*arkrpc.OORSessionPackage{{
					SessionId: bytes.Clone(
						out.sessionID[:],
					),
					ArkPsbt: bytes.Clone(out.arkPSBT),
					CheckpointPsbts: cloneByteSlices(
						out.checkpointPSBTs,
					),
				}},
				TaprootAssetRoot:   bytes.Clone(out.assetRoot),
				TaprootAssetRef:    out.assetRef,
				TaprootAssetAmount: out.assetAmount,
			})
		}
	}

	return o.replyUnary(
		env, serverconn.IndexerMailboxID,
		&arkrpc.ListVTXOsByScriptsResponse{
			Vtxos: matched,
		},
	)
}

// buildProtoAncestryPath encodes a signed tree with sweep policy and
// confirmation height into an arkrpc.AncestryPath.
func buildProtoAncestryPath(t *tree.Tree, commitTxID chainhash.Hash,
	commitHeight int32, sweepKey *btcec.PublicKey, sweepDelay uint32,
	inputIndices []uint32) (*arkrpc.AncestryPath, error) {

	path, err := arkrpc.AncestryPathFromTree(t, commitTxID, inputIndices)
	if err != nil {
		return nil, err
	}
	path.CommitmentHeight = commitHeight
	path.CommitmentSweepDelay = sweepDelay
	path.CommitmentSweepKey = sweepKey.SerializeCompressed()

	return path, nil
}

// signTaprootScriptInput appends a BIP-341 Tapscript Schnorr signature for
// privKey on the first TaprootLeafScript of pkt.Inputs[inputIdx].
func signTaprootScriptInput(pkt *psbt.Packet, inputIdx int,
	privKey *btcec.PrivateKey) error {

	if inputIdx < 0 || inputIdx >= len(pkt.Inputs) {
		return fmt.Errorf("input index %d out of range", inputIdx)
	}
	in := &pkt.Inputs[inputIdx]
	if len(in.TaprootLeafScript) == 0 {
		return fmt.Errorf("input %d missing taproot leaf script",
			inputIdx)
	}

	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(pkt.Inputs))
	for i, pin := range pkt.Inputs {
		if pin.WitnessUtxo == nil {
			return fmt.Errorf("input %d missing witness utxo", i)
		}
		prevOuts[pkt.UnsignedTx.TxIn[i].PreviousOutPoint] =
			pin.WitnessUtxo
	}
	fetcher := txscript.NewMultiPrevOutFetcher(prevOuts)

	leafScript := in.TaprootLeafScript[0]
	tapLeaf := txscript.NewTapLeaf(
		leafScript.LeafVersion, leafScript.Script,
	)
	leafHash := tapLeaf.TapHash()

	sigHashes := txscript.NewTxSigHashes(pkt.UnsignedTx, fetcher)
	sigHash, err := txscript.CalcTapscriptSignaturehash(
		sigHashes, txscript.SigHashDefault, pkt.UnsignedTx, inputIdx,
		fetcher, tapLeaf,
	)
	if err != nil {
		return fmt.Errorf("calc tapscript sighash: %w", err)
	}
	sig, err := schnorr.Sign(privKey, sigHash)
	if err != nil {
		return fmt.Errorf("schnorr sign: %w", err)
	}

	in.TaprootScriptSpendSig = append(
		in.TaprootScriptSpendSig,
		&psbt.TaprootScriptSpendSig{
			XOnlyPubKey: schnorr.SerializePubKey(privKey.PubKey()),
			LeafHash:    leafHash[:],
			Signature:   sig.Serialize(),
			SigHash:     txscript.SigHashDefault,
		},
	)

	return nil
}

// attachCheckpointCollabLeaf derives the collaborative spend leaf and control
// block for the spent VTXO (composed with TaprootAssetRoot when asset-bearing)
// and attaches it to checkpoint input 0 if not already present.
func attachCheckpointCollabLeaf(cp *psbt.Packet,
	desc *oorpb.OORSigningDescriptor) error {

	if desc == nil || len(desc.VtxoPolicyTemplate) == 0 {
		return fmt.Errorf("missing vtxo policy template")
	}

	template, err := arkscript.DecodePolicyTemplate(desc.VtxoPolicyTemplate)
	if err != nil {
		return fmt.Errorf("decode vtxo policy: %w", err)
	}
	params, err := arkscript.DecodeStandardVTXOParams(template)
	if err != nil {
		return fmt.Errorf("decode standard vtxo params: %w", err)
	}
	policy, err := arkscript.NewVTXOPolicy(
		params.OwnerKey, params.OperatorKey, params.ExitDelay,
	)
	if err != nil {
		return fmt.Errorf("build vtxo policy: %w", err)
	}
	info, err := policy.CollabSpendInfo()
	if err != nil {
		return fmt.Errorf("collab spend info: %w", err)
	}
	if len(desc.TaprootAssetRoot) > 0 {
		root, err := chainhash.NewHash(desc.TaprootAssetRoot)
		if err != nil {
			return fmt.Errorf("parse asset root: %w", err)
		}
		leafIdx := policy.ScriptIndex(info.WitnessScript)
		if leafIdx < 0 {
			return fmt.Errorf("collab leaf index not found")
		}
		composed, err := arkscript.ComposeWithSiblingRoot(
			policy.CompiledPolicy, *root,
		)
		if err != nil {
			return fmt.Errorf("compose asset root: %w", err)
		}
		info, err = composed.SpendInfo(leafIdx)
		if err != nil {
			return fmt.Errorf("composed spend info: %w", err)
		}
	}

	leaf := &psbt.TaprootTapLeafScript{
		ControlBlock: bytes.Clone(info.ControlBlock),
		Script:       bytes.Clone(info.WitnessScript),
		LeafVersion:  txscript.BaseLeafVersion,
	}
	in := &cp.Inputs[0]
	for _, existing := range in.TaprootLeafScript {
		if existing != nil &&
			bytes.Equal(existing.Script, leaf.Script) &&
			bytes.Equal(existing.ControlBlock, leaf.ControlBlock) {
			return nil
		}
	}
	in.TaprootLeafScript = append(in.TaprootLeafScript, leaf)

	return nil
}

// embeddedAssetOORDaemon is a running waved instance plus its config for
// direct test DB inspection and seeding.
type embeddedAssetOORDaemon struct {
	client  waverpc.DaemonServiceClient
	server  *waved.Server
	cfg     *waved.Config
	dataDir string
	stop    func()
}

// startEmbeddedAssetDaemonAt runs waved with the real embedded asset engine
// against an externally managed operator mailbox address.
func startEmbeddedAssetDaemonAt(t *testing.T, h *SysTestHarness, dataDir,
	mailboxAddr string) *embeddedAssetOORDaemon {

	t.Helper()

	cfg := waved.DefaultConfig()
	cfg.DataDir = dataDir
	cfg.Network = "regtest"
	cfg.DebugLevel = "info"
	cfg.Wallet.Type = waved.WalletTypeLwwallet
	cfg.Wallet.EsploraURL = h.Harness.EsploraURL
	cfg.Server.Host = mailboxAddr
	cfg.Server.Insecure = true
	cfg.RPC.ListenAddr = "127.0.0.1:0"
	cfg.RPC.Gateway.ListenAddr = "127.0.0.1:0"
	cfg.RPC.NoTLS = true
	cfg.RPC.NoMacaroons = true

	courierHost := h.Harness.TapdUniverseHost()
	cfg.Assets = &waved.AssetsConfig{
		Enabled: true,
		Courier: waved.AssetCourierConfig{
			Addr: "authmailbox+universerpc://" + courierHost,
			Endpoints: []string{
				courierHost + "=" + net.JoinHostPort(
					"127.0.0.1", h.Harness.TapdGRPCPort,
				) + "=" + h.Harness.TapdTLSCertPath(),
			},
		},
	}

	server, err := waved.NewServer(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(h.Context())
	errChan := make(chan error, 1)
	go func() {
		errChan <- server.RunWithContext(ctx)
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true

		cancel()
		select {
		case err := <-errChan:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Logf("waved stopped: %v", err)
			}

		case <-time.After(30 * time.Second):
			t.Errorf("waved did not stop")
		}
	}
	t.Cleanup(stop)

	require.Eventually(t, func() bool {
		return server.RPCAddr() != nil
	}, time.Minute, 50*time.Millisecond, "waved RPC never listened")

	conn, err := grpc.NewClient(
		server.RPCAddr().String(),
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := waverpc.NewDaemonServiceClient(conn)

	rpcCtx, rpcCancel := context.WithTimeout(ctx, time.Minute)
	defer rpcCancel()

	var info *waverpc.GetInfoResponse
	require.Eventually(t, func() bool {
		info, err = client.GetInfo(rpcCtx, &waverpc.GetInfoRequest{})

		return err == nil
	}, time.Minute, 100*time.Millisecond, "waved RPC never answered")

	fresh := info.GetWalletState() == waverpc.WalletState_WALLET_STATE_NONE
	if fresh {
		seed, err := client.GenSeed(rpcCtx, &waverpc.GenSeedRequest{})
		require.NoError(t, err)
		_, err = client.InitWallet(rpcCtx, &waverpc.InitWalletRequest{
			Mnemonic:       seed.Mnemonic,
			WalletPassword: embeddedAssetTestPassword,
		})
		require.NoError(t, err)
	} else {
		_, err = client.UnlockWallet(
			rpcCtx, &waverpc.UnlockWalletRequest{
				WalletPassword: embeddedAssetTestPassword,
			},
		)
		require.NoError(t, err)
	}

	require.NoError(t, server.WaitForWalletServicesReady(rpcCtx))
	waitForDaemonReady(t, client)

	if fresh {
		funding, err := client.NewAssetFundingAddress(
			rpcCtx, &waverpc.NewAssetFundingAddressRequest{},
		)
		require.NoError(t, err)
		h.Harness.Faucet(funding.Address, assetTestWalletFunds)
		h.Harness.GenerateAndWait(1)
		require.Eventually(t, func() bool {
			utxos, err := server.ListWalletUnspent(
				rpcCtx, 1, 9_999_999,
			)

			return err == nil && len(utxos) >= 1
		}, time.Minute, time.Second, "daemon wallet never funded")
	}

	return &embeddedAssetOORDaemon{
		client:  client,
		server:  server,
		cfg:     cfg,
		dataDir: dataDir,
		stop:    stop,
	}
}

// fundDaemonWalletExtraUTXOs sends count additional confirmed P2TR UTXOs to
// the daemon wallet so parallel CPFP bumps have independent fee inputs.
func fundDaemonWalletExtraUTXOs(t *testing.T, h *SysTestHarness,
	d *embeddedAssetOORDaemon, count int) {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	for range count {
		funding, err := d.client.NewAssetFundingAddress(
			ctx, &waverpc.NewAssetFundingAddressRequest{},
		)
		require.NoError(t, err)
		h.Harness.Faucet(funding.Address, assetTestWalletFunds)
	}
	h.Harness.GenerateAndWait(1)

	require.Eventually(t, func() bool {
		utxos, err := d.server.ListWalletUnspent(ctx, 1, 9_999_999)

		return err == nil && len(utxos) >= count+1
	}, time.Minute, time.Second,
		"extra daemon wallet UTXOs never confirmed")
}

// lookupOwnedReceiveKey reads a persisted owned receive script's full key
// descriptor (including compressed 33-byte pubkey and key locator) from the
// daemon's SQLite database while the daemon is stopped.
func lookupOwnedReceiveKey(t *testing.T, cfg *waved.Config,
	pkScriptHex string) keychain.KeyDescriptor {

	t.Helper()

	pkScript, err := hex.DecodeString(pkScriptHex)
	require.NoError(t, err)

	sqliteStore, err := db.NewSqliteStore(
		db.DefaultSqliteConfig(
			cfg.NetworkDir(),
		),
		btclog.Disabled,
	)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, sqliteStore.Close())
	}()

	store := db.NewStore(
		sqliteStore.DB, sqliteStore.Queries, sqliteStore.Backend(),
		btclog.Disabled,
	)
	oorStore := store.NewOORArtifactStore(clock.NewDefaultClock())

	rec, err := oorStore.LookupOwnedReceiveScript(
		t.Context(), pkScript,
	)
	require.NoError(t, err)
	require.NotNil(t, rec, "receive script %s not found", pkScriptHex)
	require.NotNil(t, rec.ClientKey.PubKey)

	return rec.ClientKey
}

// confirmedOnlyProofVerifier adapts tapassets.ProofChainVerifier to
// customanchor.ConfirmedProofVerifier for confirmed-only batch sources.
type confirmedOnlyProofVerifier struct {
	chain tapassets.ProofChainVerifier
}

func (v *confirmedOnlyProofVerifier) VerifyConfirmedProof(ctx context.Context,
	proofFile []byte) (*customanchor.ConfirmedProofVerification, error) {

	if err := v.chain.VerifyProofChain(ctx, proofFile); err != nil {
		return nil, err
	}

	return customanchor.DeriveConfirmedProofInventory(proofFile)
}

// memTransitionStore is an in-memory tapassets.Store for building the test
// commitment anchor and asset tree.
type memTransitionStore struct {
	mu      sync.Mutex
	records map[string][]byte
}

func (m *memTransitionStore) Load(_ context.Context, key string) ([]byte,
	error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	val, ok := m.records[key]
	if !ok {
		return nil, tapassets.ErrStoreNotFound
	}

	return bytes.Clone(val), nil
}

func (m *memTransitionStore) Store(_ context.Context, key string,
	data []byte) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.records == nil {
		m.records = make(map[string][]byte)
	}
	m.records[key] = bytes.Clone(data)

	return nil
}

// mintV1AssetTestAsset mints an ungrouped v1 fungible asset in the harness
// tapd (required for unconfirmed proof paths) and waits until tapd holds it.
func mintV1AssetTestAsset(t *testing.T, h *SysTestHarness, tapd *tapgrpc.Client,
	amount uint64) tapsdk.AssetRef {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	_, err := tapd.MintAsset(ctx, &tapsdk.MintAssetRequest{
		Asset: &tapsdk.MintAsset{
			AssetVersion:  tapsdk.AssetVersionV1,
			AssetType:     tapsdk.AssetTypeNormal,
			Name:          "oor-test-v1",
			InitialSupply: amount,
			AssetMeta: &tapsdk.AssetMeta{
				Data: []byte("oor test v1 asset"),
			},
		},
		ShortResponse: true,
	})
	require.NoError(t, err)
	batch, err := tapd.FinalizeBatch(ctx, &tapsdk.FinalizeBatchRequest{
		ShortResponse: true,
	})
	require.NoError(t, err)
	h.Harness.WaitMempoolTx(batch.BatchTxid)
	h.Harness.Generate(1)

	var ref tapsdk.AssetRef
	require.Eventually(t, func() bool {
		utxos, err := tapd.ListUtxos(ctx, &tapsdk.ListUtxosRequest{})
		if err != nil {
			return false
		}
		for _, utxo := range utxos {
			for _, asset := range utxo.Assets {
				if asset != nil && asset.Amount == amount {
					ref = asset.AssetRef
				}
			}
		}

		return ref != ""
	}, time.Minute, time.Second, "minted v1 asset never appeared")

	return ref
}

// buildRealAssetOORRoundFixture mints a real confirmed commitment tx anchored
// on regtest bitcoind containing both a 2-leaf asset batch output (400 + 300
// units for Alice) and a 1-leaf 10,000-sat carrier float batch output, and
// signs every tree transaction with real MuSig2 signatures.
func buildRealAssetOORRoundFixture(t *testing.T, h *SysTestHarness,
	issuer *tapgrpc.Client, ref tapsdk.AssetRef, op *assetOORTestOperator,
	aliceKey0, aliceKey1 keychain.KeyDescriptor) *assetOORRoundFixture {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// Locate the minted 700-unit asset UTXO held by the harness tapd.
	utxos, err := issuer.ListUtxos(ctx, &tapsdk.ListUtxosRequest{})
	require.NoError(t, err)

	var (
		assetUtxo   *tapsdk.ManagedUtxo
		mintedAsset *tapsdk.AssetRecord
	)
	for _, u := range utxos {
		for _, a := range u.Assets {
			if a != nil && a.AssetRef.Equivalent(ref) &&
				a.Amount == assetOORLeaf0Units+
					assetOORLeaf1Units {

				assetUtxo = u
				mintedAsset = a
				break
			}
		}
		if assetUtxo != nil {
			break
		}
	}
	require.NotNil(t, assetUtxo, "minted asset UTXO not found")

	exportedProof, err := issuer.ExportProof(
		ctx, ref, mintedAsset.ScriptKey.PubKey, &assetUtxo.OutPoint,
	)
	require.NoError(t, err)
	require.NotEmpty(t, exportedProof.RawProofFile)

	assetOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash(assetUtxo.OutPoint.Txid),
		Index: assetUtxo.OutPoint.Index,
	}
	assetPrevTx := h.Harness.GetRawTransaction(assetOutpoint.Hash.String())
	require.Greater(t, len(assetPrevTx.TxOut), int(assetOutpoint.Index))
	assetTxOut := assetPrevTx.TxOut[assetOutpoint.Index]

	assetInternalKey, err := btcec.ParsePubKey(assetUtxo.InternalKey[:])
	require.NoError(t, err)

	// Generate ephemeral cosigner keys for the two asset tree leaves so
	// the test holds every MuSig2 private key needed to sign the tree.
	cosignerPriv0, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	cosignerPriv1, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	sweepLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		op.operatorKey.PubKey(), assetOORBatchSweepDelay,
	)
	require.NoError(t, err)
	sweepRoot := sweepLeaf.TapHash()

	assetCosigners := []*btcec.PublicKey{
		op.operatorKey.PubKey(),
		cosignerPriv0.PubKey(),
		cosignerPriv1.PubKey(),
	}
	floatCosigners := []*btcec.PublicKey{
		op.operatorKey.PubKey(),
		op.carrierKey.PubKey(),
	}
	floatInternalKey, err := tree.ComputeInternalKey(floatCosigners)
	require.NoError(t, err)
	floatBatchKey, err := tree.ComputeFinalKey(floatCosigners, sweepRoot[:])
	require.NoError(t, err)
	floatBatchScript, err := txscript.PayToTaprootScript(floatBatchKey)
	require.NoError(t, err)

	// Encode Output 1's single-leaf BIP-371 TapTree so Taproot Assets can
	// construct a valid exclusion proof for the non-asset P2TR float batch
	// output on the commitment transaction.
	var floatTapTreeBuf bytes.Buffer
	floatTapTreeBuf.WriteByte(0)
	floatTapTreeBuf.WriteByte(byte(sweepLeaf.LeafVersion))
	require.NoError(
		t, wire.WriteVarBytes(&floatTapTreeBuf, 0, sweepLeaf.Script),
	)
	floatTapTree := floatTapTreeBuf.Bytes()

	issuerWallet := tapsdk.NewWallet(issuer, tapsdk.Network("regtest"))
	proofChainVerifier, err := tapassets.NewTapdProofChainVerifier(issuer)
	require.NoError(t, err)
	confirmedVerifier := &confirmedOnlyProofVerifier{
		chain: proofChainVerifier,
	}

	store := &memTransitionStore{}
	committer, err := tapassets.NewBatchAnchorCommitter(
		tapassets.BatchAnchorCommitterConfig{
			Wallet: issuerWallet,
			Store:  store,
		},
	)
	require.NoError(t, err)

	batchDigest := tapsdk.Hash(
		chainhash.HashH(
			[]byte("asset-oor-systest-batch"),
		),
	)
	totalAssetUnits := assetOORLeaf0Units + assetOORLeaf1Units
	batchReq := &tapassets.BatchAnchorRequest{
		AssetRef: ref,
		Amount:   totalAssetUnits,
		Sources: []tapassets.BatchAnchorSource{{
			ProofFile:         exportedProof.RawProofFile,
			Amount:            totalAssetUnits,
			Verifier:          confirmedVerifier,
			AnchorOutpoint:    assetOutpoint,
			AnchorInternalKey: assetInternalKey,
		}},
		Cosigners:      assetCosigners,
		SweepLeaf:      sweepLeaf,
		Digest:         batchDigest,
		OutputIndex:    0,
		OutputValueSat: 2 * assetOORMinVTXOSat,
	}

	// Derive the composed asset batch script before Bitcoin funding.
	placeholderTx := wire.NewMsgTx(2)
	placeholderTx.AddTxIn(wire.NewTxIn(&assetOutpoint, nil, nil))
	placeholderTx.AddTxOut(
		wire.NewTxOut(
			2*assetOORMinVTXOSat, floatBatchScript,
		),
	)
	placeholderTx.AddTxOut(
		wire.NewTxOut(
			assetOORFloatValueSat, floatBatchScript,
		),
	)
	preFunding, err := psbt.NewFromUnsignedTx(placeholderTx)
	require.NoError(t, err)
	preFunding.Inputs[0].WitnessUtxo = assetTxOut
	preFunding.Outputs[1].TaprootInternalKey = schnorr.SerializePubKey(
		floatInternalKey,
	)
	preFunding.Outputs[1].TaprootTapTree = bytes.Clone(floatTapTree)

	derived, err := committer.DeriveScript(ctx, batchReq, preFunding)
	require.NoError(t, err)

	// Fund the commitment transaction via OperatorLND so it supplies a
	// confirmed fee input and change output with full derivation info.
	outputsOnlyTx := wire.NewMsgTx(2)
	outputsOnlyTx.AddTxOut(
		wire.NewTxOut(
			2*assetOORMinVTXOSat, derived.PkScript,
		),
	)
	outputsOnlyTx.AddTxOut(
		wire.NewTxOut(
			assetOORFloatValueSat, floatBatchScript,
		),
	)
	outputsOnlyPkt, err := psbt.NewFromUnsignedTx(outputsOnlyTx)
	require.NoError(t, err)
	outputsOnlyRaw, err := psbtutil.Serialize(outputsOnlyPkt)
	require.NoError(t, err)

	funded, changeIdx, _, err := h.Harness.LND.WalletKit.
		FundPsbt(ctx, &walletrpc.FundPsbtRequest{
			Template: &walletrpc.FundPsbtRequest_Psbt{
				Psbt: outputsOnlyRaw,
			},
			Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
				SatPerVbyte: 2,
			},
			MinConfs: 1,
			ChangeType: walletrpc.
				ChangeAddressType_CHANGE_ADDRESS_TYPE_P2TR,
		})
	require.NoError(t, err)
	require.GreaterOrEqual(t, changeIdx, int32(0))

	// Assemble the final commitment PSBT with: Input 0: minted asset UTXO
	// (signed by tapd via BatchAnchorCommitter) Inputs 1..N: LND funding
	// inputs (signed by OperatorLND) Output 0: asset batch output (2,000
	// sats) Output 1: float batch output (10,000 sats) Output 2: LND change
	// (increased by assetTxOut.Value)
	changeOut := funded.UnsignedTx.TxOut[changeIdx]
	changePOut := funded.Outputs[changeIdx]

	commitUnsigned := wire.NewMsgTx(2)
	commitUnsigned.AddTxIn(wire.NewTxIn(&assetOutpoint, nil, nil))
	for _, in := range funded.UnsignedTx.TxIn {
		commitUnsigned.AddTxIn(in)
	}
	commitUnsigned.AddTxOut(
		wire.NewTxOut(
			2*assetOORMinVTXOSat, derived.PkScript,
		),
	)
	commitUnsigned.AddTxOut(
		wire.NewTxOut(
			assetOORFloatValueSat, floatBatchScript,
		),
	)
	commitUnsigned.AddTxOut(
		wire.NewTxOut(
			changeOut.Value+assetTxOut.Value, changeOut.PkScript,
		),
	)

	commitPkt, err := psbt.NewFromUnsignedTx(commitUnsigned)
	require.NoError(t, err)
	commitPkt.Inputs[0].WitnessUtxo = assetTxOut
	for i := range funded.Inputs {
		commitPkt.Inputs[i+1] = funded.Inputs[i]
	}
	commitPkt.Outputs[1].TaprootInternalKey = schnorr.SerializePubKey(
		floatInternalKey,
	)
	commitPkt.Outputs[1].TaprootTapTree = bytes.Clone(floatTapTree)
	commitPkt.Outputs[2] = changePOut

	batchCommit, err := committer.Commit(ctx, batchReq, commitPkt, derived)
	require.NoError(t, err)

	// Restore LND's derivation metadata on the BTC funding inputs
	// (stripped during tapd's custom-anchor commit), sign via OperatorLND,
	// finalize every input (including input 0 signed by tapd), record the
	// transfer via committer.Publish, and broadcast on regtest bitcoind.
	committedPkt, err := psbtutil.Parse(batchCommit.AnchorPSBT)
	require.NoError(t, err)
	for i := range funded.Inputs {
		committedPkt.Inputs[i+1].Bip32Derivation =
			funded.Inputs[i].Bip32Derivation
		committedPkt.Inputs[i+1].TaprootBip32Derivation =
			funded.Inputs[i].TaprootBip32Derivation
	}
	signedCommitPkt, err := h.Harness.LND.WalletKit.SignPsbt(
		ctx, committedPkt,
	)
	require.NoError(t, err)
	for i := range signedCommitPkt.Inputs {
		if len(signedCommitPkt.Inputs[i].FinalScriptWitness) != 0 {
			continue
		}
		finalized, err := psbt.MaybeFinalize(signedCommitPkt, i)
		require.NoError(t, err)
		require.True(
			t, finalized, "commitment input %d failed to finalize",
			i,
		)
	}
	commitTx, err := psbt.Extract(signedCommitPkt)
	require.NoError(t, err)

	finalCommitPSBT, err := psbtutil.Serialize(signedCommitPkt)
	require.NoError(t, err)
	require.NoError(
		t, committer.Publish(
			ctx, batchCommit.PackageBytes, finalCommitPSBT,
		),
	)

	btcClient, err := h.Harness.BitcoinRPCClient()
	require.NoError(t, err)
	commitTxIDPtr, err := btcClient.SendRawTransaction(commitTx, true)
	require.NoError(t, err)
	commitTxID := *commitTxIDPtr

	h.Harness.GenerateAndWait(1)
	commitHeight := int32(h.Harness.BlockCount())

	// Build and MuSig2-sign the 2-leaf asset VTXO tree (400 + 300 units).
	aliceAnchor0, err := tapassets.StandardVTXOLeafAnchor(
		aliceKey0.PubKey, op.operatorKey.PubKey(), assetOORExitDelay,
	)
	require.NoError(t, err)
	aliceAnchor1, err := tapassets.StandardVTXOLeafAnchor(
		aliceKey1.PubKey, op.operatorKey.PubKey(), assetOORExitDelay,
	)
	require.NoError(t, err)

	cosigner0Key := string(cosignerPriv0.PubKey().SerializeCompressed())
	cosigner1Key := string(cosignerPriv1.PubKey().SerializeCompressed())
	anchorByCosigner := map[string]tapassets.TreeLeafAnchor{
		cosigner0Key: aliceAnchor0,
		cosigner1Key: aliceAnchor1,
	}

	assetTree, err := tapassets.BuildAssetTree(
		ctx,
		tapassets.TreeMaterializerConfig{
			Wallet:    issuerWallet,
			Store:     store,
			AssetRef:  ref,
			SweepLeaf: sweepLeaf,
			LeafAnchor: func(node *tree.Node) (
				tapassets.TreeLeafAnchor, error) {

				for _, k := range node.CoSigners {
					if a, ok := anchorByCosigner[string(
						k.SerializeCompressed(),
					)]; ok {
						return a, nil
					}
				}

				return tapassets.TreeLeafAnchor{}, fmt.Errorf(
					"unknown leaf " +
						"cosigners")
			},
			Root:   batchCommit.RootSource,
			Digest: batchDigest,
		},
		tapassets.AssetTreeRequest{
			Leaves: []tree.LeafDescriptor{
				{
					PkScript: aliceAnchor0.
						UncomposedPkScript,
					Amount: btcutil.Amount(
						assetOORMinVTXOSat,
					),
					CoSignerKey: cosignerPriv0.PubKey(),
					AssetAmount: assetOORLeaf0Units,
				},
				{
					PkScript: aliceAnchor1.
						UncomposedPkScript,
					Amount: btcutil.Amount(
						assetOORMinVTXOSat,
					),
					CoSignerKey: cosignerPriv1.PubKey(),
					AssetAmount: assetOORLeaf1Units,
				},
			},
			OperatorKey: op.operatorKey.PubKey(),
			Radix:       2,
			BatchOutpoint: wire.OutPoint{
				Hash:  commitTxID,
				Index: 0,
			},
			BatchOutput: commitTx.TxOut[0],
			AssetAmount: assetOORLeaf0Units + assetOORLeaf1Units,
		},
	)
	require.NoError(t, err)
	require.Equal(t, derived.AssetRoot[:], assetTree.BatchAssetRoot)

	opPubKey := string(op.operatorKey.PubKey().SerializeCompressed())
	carrierPubKey := string(op.carrierKey.PubKey().SerializeCompressed())
	signTreeWithLocalKeys(t, assetTree, map[string]*btcec.PrivateKey{
		opPubKey:     op.operatorKey,
		cosigner0Key: cosignerPriv0,
		cosigner1Key: cosignerPriv1,
	})
	require.NoError(t, assetTree.VerifySigned())

	// Build and MuSig2-sign the 1-leaf Bitcoin carrier float tree.
	floatDesc, err := tree.NewVTXODescriptor(
		btcutil.Amount(assetOORFloatValueSat), op.carrierKey.PubKey(),
		op.operatorKey.PubKey(), assetOORExitDelay,
	)
	require.NoError(t, err)

	floatTree, err := tree.NewTree(
		wire.OutPoint{Hash: commitTxID, Index: 1},
		commitTx.TxOut[1],
		[]tree.LeafDescriptor{{
			PkScript:    floatDesc.PkScript,
			Amount:      btcutil.Amount(assetOORFloatValueSat),
			CoSignerKey: op.carrierKey.PubKey(),
		}},
		op.operatorKey.PubKey(),
		sweepRoot[:],
		2,
	)
	require.NoError(t, err)

	signTreeWithLocalKeys(t, floatTree, map[string]*btcec.PrivateKey{
		opPubKey:      op.operatorKey,
		carrierPubKey: op.carrierKey,
	})
	require.NoError(t, floatTree.VerifySigned())

	floatLeafTx, err := floatTree.Root.ToSignedTx()
	require.NoError(t, err)
	floatOutpoint := wire.OutPoint{
		Hash:  floatLeafTx.TxHash(),
		Index: 0,
	}

	// Extract Alice's two individual leaf specs from the signed asset tree.
	leaf0 := extractSeededAssetLeaf(
		t, assetTree, cosignerPriv0.PubKey(), aliceKey0,
		op.operatorKey.PubKey(), assetOORLeaf0Units,
	)
	leaf1 := extractSeededAssetLeaf(
		t, assetTree, cosignerPriv1.PubKey(), aliceKey1,
		op.operatorKey.PubKey(), assetOORLeaf1Units,
	)

	roundID, err := round.NewRoundID()
	require.NoError(t, err)

	return &assetOORRoundFixture{
		roundID:        roundID,
		ref:            ref,
		commitTx:       commitTx,
		commitTxID:     commitTxID,
		commitHeight:   commitHeight,
		batchExpiry:    commitHeight + int32(assetOORBatchSweepDelay),
		sweepLeaf:      sweepLeaf,
		sweepRoot:      sweepRoot,
		assetTree:      assetTree,
		floatTree:      floatTree,
		leaf0:          leaf0,
		leaf1:          leaf1,
		floatOutpoint:  floatOutpoint,
		floatPolicy:    floatDesc.PolicyTemplate,
		floatPkScript:  floatDesc.PkScript,
		floatValueSat:  assetOORFloatValueSat,
		floatTreePath:  floatTree,
		assetTreePath0: leaf0.TreePath,
		assetTreePath1: leaf1.TreePath,
	}
}

// extractSeededAssetLeaf extracts one signed leaf descriptor and sealed asset
// package from the materialized asset tree for Alice's key.
func extractSeededAssetLeaf(t *testing.T, fullTree *tree.Tree,
	cosignerPub *btcec.PublicKey, clientKey keychain.KeyDescriptor,
	operatorPub *btcec.PublicKey, assetAmount uint64) assetOORLeafSpec {

	t.Helper()

	extracted, err := fullTree.ExtractPathForCoSigners(cosignerPub)
	require.NoError(t, err)
	require.NotNil(t, extracted)

	leaves := extracted.Root.GetLeafNodes()
	require.Len(t, leaves, 1)
	leafNode := leaves[0]

	leafTx, err := leafNode.ToSignedTx()
	require.NoError(t, err)

	rootBytes := extracted.AssetContext.LeafAssetRoot(leafNode.Input)
	require.Len(t, rootBytes, chainhash.HashSize)
	var assetRoot chainhash.Hash
	copy(assetRoot[:], rootBytes)

	sealedPkg := extracted.AssetContext.SealedPackage(leafNode.Input)
	require.NotEmpty(t, sealedPkg)

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		clientKey.PubKey, operatorPub, assetOORExitDelay,
	)
	require.NoError(t, err)

	return assetOORLeafSpec{
		Outpoint: wire.OutPoint{
			Hash:  leafTx.TxHash(),
			Index: 0,
		},
		Amount:         btcutil.Amount(leafNode.Outputs[0].Value),
		PolicyTemplate: policyTemplate,
		PkScript:       bytes.Clone(leafNode.Outputs[0].PkScript),
		ClientKey:      clientKey,
		AssetRoot:      assetRoot,
		AssetAmount:    assetAmount,
		SealedPackage:  bytes.Clone(sealedPkg),
		TreePath:       extracted,
	}
}

// signTreeWithLocalKeys computes real MuSig2 key-spend signatures for every
// node in a VTXO tree using the supplied cosigner private keys.
func signTreeWithLocalKeys(t *testing.T, vtxoTree *tree.Tree,
	privByPub map[string]*btcec.PrivateKey) {

	t.Helper()

	prevOutFetcher, err := vtxoTree.Root.PrevOutputFetcher(
		vtxoTree.BatchOutput,
	)
	require.NoError(t, err)

	for node := range vtxoTree.Root.NodesIter() {
		sigHash, err := node.SigHash(prevOutFetcher)
		require.NoError(t, err)

		tweak := vtxoTree.SweepTapscriptRoot
		if vtxoTree.AssetContext != nil {
			tweak = vtxoTree.AssetContext.SigningTweak(node.Input)
		}

		// Sort cosigners identically to MuSig2 key aggregation.
		signers := append([]*btcec.PublicKey(nil), node.CoSigners...)
		sort.Slice(signers, func(i, j int) bool {
			return bytes.Compare(
				signers[i].SerializeCompressed(),
				signers[j].SerializeCompressed(),
			) < 0
		})

		sessions := make([]*musig2.Session, len(signers))
		pubNonces := make([][musig2.PubNonceSize]byte, len(signers))
		for i, pub := range signers {
			priv, ok := privByPub[string(pub.SerializeCompressed())]
			require.True(t, ok, "missing private key for cosigner")

			ctx, err := musig2.NewContext(
				priv, true, musig2.WithKnownSigners(signers),
				musig2.WithTaprootTweakCtx(tweak),
			)
			require.NoError(t, err)

			sess, err := ctx.NewSession()
			require.NoError(t, err)
			sessions[i] = sess
			pubNonces[i] = sess.PublicNonce()
		}

		for i, sess := range sessions {
			for j, nonce := range pubNonces {
				if i == j {
					continue
				}
				_, err := sess.RegisterPubNonce(nonce)
				require.NoError(t, err)
			}
		}

		partialSigs := make([]*musig2.PartialSignature, len(sessions))
		for i, sess := range sessions {
			psig, err := sess.Sign([32]byte(sigHash))
			require.NoError(t, err)
			partialSigs[i] = psig
		}

		combiner := sessions[0]
		for i := 1; i < len(partialSigs); i++ {
			_, err := combiner.CombineSig(partialSigs[i])
			require.NoError(t, err)
		}
		node.Signature = combiner.FinalSig()
	}
}

// seedRealAssetVTXOs inserts Alice's two real confirmed-round asset VTXOs into
// her SQLite database while her daemon is stopped.
func seedRealAssetVTXOs(t *testing.T, cfg *waved.Config,
	op *assetOORTestOperator, f *assetOORRoundFixture) {

	t.Helper()

	networkDir := cfg.NetworkDir()
	require.NoError(t, os.MkdirAll(networkDir, 0o700))

	sqliteStore, err := db.NewSqliteStore(
		db.DefaultSqliteConfig(networkDir), btclog.Disabled,
	)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, sqliteStore.Close())
	}()

	store := db.NewStore(
		sqliteStore.DB, sqliteStore.Queries, sqliteStore.Backend(),
		btclog.Disabled,
	)
	roundStore := store.NewRoundStore(
		&chaincfg.RegressionNetParams, clock.NewDefaultClock(),
	)
	vtxoStore := store.NewVTXOStore(clock.NewDefaultClock())

	unsignedCommitTx := f.commitTx.Copy()
	for _, in := range unsignedCommitTx.TxIn {
		in.SignatureScript = nil
		in.Witness = nil
	}
	commitPSBT, err := psbt.NewFromUnsignedTx(unsignedCommitTx)
	require.NoError(t, err)

	err = roundStore.CommitState(t.Context(), &round.Round{
		RoundID:      f.roundID,
		StartHeight:  uint32(f.commitHeight),
		CommitmentTx: fn.Some(commitPSBT),
		VTXOTreePaths: fn.Some(map[int]*tree.Tree{
			0: f.assetTree,
		}),
	}, &round.InputSigSentState{
		RoundID:     f.roundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	})
	require.NoError(t, err)

	err = roundStore.FinalizeRound(
		t.Context(),
		f.roundID,
		f.commitTxID,
		round.ConfInfo{
			Height: f.commitHeight,
		},
		nil,
	)
	require.NoError(t, err)

	for _, leaf := range []assetOORLeafSpec{f.leaf0, f.leaf1} {
		tapScript, err := arkscript.VTXOTapScript(
			leaf.ClientKey.PubKey, op.operatorKey.PubKey(),
			assetOORExitDelay,
		)
		require.NoError(t, err)

		rootCopy := leaf.AssetRoot
		err = vtxoStore.SaveVTXO(t.Context(), &vtxo.Descriptor{
			Outpoint:       leaf.Outpoint,
			Amount:         leaf.Amount,
			PolicyTemplate: leaf.PolicyTemplate,
			PkScript:       leaf.PkScript,
			ClientKey:      leaf.ClientKey,
			OperatorKey:    op.operatorKey.PubKey(),
			TapScript:      tapScript,
			Ancestry: []types.Ancestry{{
				TreePath:             leaf.TreePath,
				CommitmentTxID:       f.commitTxID,
				CommitmentHeight:     f.commitHeight,
				CommitmentSweepDelay: assetOORBatchSweepDelay,
				CommitmentSweepKey:   op.operatorKey.PubKey(),
				TreeDepth:            2,
			}},
			RoundID:                   f.roundID.String(),
			CommitmentTxID:            f.commitTxID,
			BatchExpiry:               f.batchExpiry,
			RelativeExpiry:            assetOORExitDelay,
			CreatedHeight:             f.commitHeight,
			Status:                    vtxo.VTXOStatusLive,
			TaprootAssetRoot:          &rootCopy,
			TaprootAssetRef:           f.ref.String(),
			TaprootAssetAmount:        leaf.AssetAmount,
			TaprootAssetSealedPackage: leaf.SealedPackage,
		})
		require.NoError(t, err)
	}
}

// cloneByteSlices returns a deep copy of a slice of byte slices.
func cloneByteSlices(in [][]byte) [][]byte {
	if len(in) == 0 {
		return nil
	}
	out := make([][]byte, len(in))
	for i := range in {
		out[i] = bytes.Clone(in[i])
	}

	return out
}
