//go:build systest

package systest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/bip322"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	clienttypes "github.com/lightninglabs/darepo-client/lib/types"
	clientround "github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestJoinAuthE2EForgedBoardingInputRejected verifies that a client cannot
// forge a valid join request for another participant's boarding input.
func TestJoinAuthE2EForgedBoardingInputRejected(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := t.Context()

	h.FundServerWallet(btcutil.SatoshiPerBitcoin)

	victim := NewTestClient(h)
	attacker := NewTestClient(h)

	prepareClientWithBoardingIntent(t, h, victim, 200_000)

	// Buffer the victim's outbound join so we can capture a canonical
	// boarding request payload without registering it server-side.
	victim.serverConn.SetBuffered(true)
	err := victim.TriggerRegistration(ctx)
	require.NoError(t, err, "victim should emit join request")

	err = h.Transcript().WaitForEntryCount(1, 10*time.Second)
	require.NoError(t, err, "victim join request should be recorded")
	require.Equal(t, 1, victim.serverConn.PendingCount(),
		"victim join request should remain buffered")

	victimJoinReq := latestClientJoinRoundRequest(
		t, h.Transcript(), victim.ClientID(),
	)

	forgedReq := buildForgedJoinRoundRequest(
		ctx, t, h, attacker, victimJoinReq,
	)

	sendResp := attacker.serverConn.Receive(
		ctx, &serverconn.SendClientEventRequest{
			Message: forgedReq,
		},
	)
	resp, sendErr := sendResp.Unpack()
	require.NoError(t, sendErr, "bridge should route forged join request")

	sendAck, ok := resp.(*serverconn.SendClientEventResponse)
	require.True(t, ok, "response should be SendClientEventResponse")
	require.True(t, sendAck.Success, "bridge send should succeed")

	failed := waitForBoardingFailedEvent(
		t, h, attacker.ClientID(), 20*time.Second,
	)
	require.Contains(t, failed.Reason, "join request invalid")
	require.Contains(t, failed.Reason, "join request auth invalid")

	h.Transcript().AssertContainsMessage(
		t, C2SFrom("JoinRoundRequest", attacker.ClientID()),
	)
	h.Transcript().AssertContainsMessage(
		t, S2CTo("ClientErrorResp", attacker.ClientID()),
	)
	h.Transcript().AssertNotContainsMessage(
		t, S2CTo("ClientSuccessResp", attacker.ClientID()),
	)
}

// TestJoinAuthE2EJoinRequestExpiresInBuffer verifies that a previously valid
// join request is rejected when delivered after its block window expires.
func TestJoinAuthE2EJoinRequestExpiresInBuffer(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := t.Context()

	h.FundServerWallet(btcutil.SatoshiPerBitcoin)

	client := NewTestClient(h)

	// Use a larger boarding exit delay so the test can mine
	// enough blocks to expire join auth without tripping
	// boarding delay-path safety checks.
	terms := h.Terms()
	boardingExitDelay := terms.BoardingExitDelay + 500
	prepareClientWithBoardingIntentWithExitDelay(
		t, h, client, 200_000, boardingExitDelay,
	)

	// Hold the outbound join request so we can advance chain height past
	// the request's embedded valid-until block.
	client.serverConn.SetBuffered(true)
	err := client.TriggerRegistration(ctx)
	require.NoError(t, err, "client should emit join request")

	err = h.Transcript().WaitForEntryCount(1, 10*time.Second)
	require.NoError(t, err, "join request should be recorded")
	require.Equal(t, 1, client.serverConn.PendingCount(),
		"join request should remain buffered")

	joinReq := latestClientJoinRoundRequest(
		t, h.Transcript(), client.ClientID(),
	)
	require.NotNil(t, joinReq.Auth,
		"join request should include auth payload")

	validUntil := joinReq.Auth.ValidUntil
	require.NotZero(t, validUntil,
		"join auth should include valid-until block")

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err, "should get bitcoin RPC client")

	currentHeight, err := rpcClient.GetBlockCount()
	require.NoError(t, err, "should fetch current block height")

	// Mine well past valid-until to account for small notification lag
	// between bitcoind height and the server's observed chain height.
	targetHeight := validUntil + 20
	blocksToMine := int(targetHeight - uint32(currentHeight))
	if blocksToMine < 1 {
		blocksToMine = 1
	}

	h.MineBlocks(blocksToMine)

	client.serverConn.SetBuffered(false)
	err = client.serverConn.FlushAll()
	require.NoError(t, err, "should flush buffered messages")

	failed := waitForBoardingFailedEvent(
		t, h, client.ClientID(), 20*time.Second,
	)
	require.Contains(t, failed.Reason, "join request auth invalid")
	require.Contains(t, failed.Reason, "signature expired")

	h.Transcript().AssertContainsMessage(
		t, C2SFrom("JoinRoundRequest", client.ClientID()),
	)
	h.Transcript().AssertContainsMessage(
		t, S2CTo("ClientErrorResp", client.ClientID()),
	)
	h.Transcript().AssertNotContainsMessage(
		t, S2CTo("ClientSuccessResp", client.ClientID()),
	)
}

// prepareClientWithBoardingIntent funds one boarding input, waits for
// confirmation, and registers one VTXO request so TriggerRegistration emits a
// join request.
func prepareClientWithBoardingIntent(t *testing.T, h *E2EHarness,
	client *TestClient, amount btcutil.Amount) {

	terms := h.Terms()
	prepareClientWithBoardingIntentWithExitDelay(
		t, h, client, amount, terms.BoardingExitDelay,
	)
}

// prepareClientWithBoardingIntentWithExitDelay funds one boarding input with
// an explicit exit delay, waits for confirmation, and registers one VTXO
// request so TriggerRegistration emits a join request.
func prepareClientWithBoardingIntentWithExitDelay(t *testing.T, h *E2EHarness,
	client *TestClient, amount btcutil.Amount, exitDelay uint32) {

	ctx := t.Context()

	boardingResp, err := client.CreateBoardingAddress(exitDelay)
	require.NoError(t, err, "should create boarding address")

	h.Harness.Faucet(boardingResp.Address.String(), amount)
	h.MineBlocks(int(h.Terms().MinBoardingConfirmations))

	err = client.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(t, err, "client should detect boarding confirmation")

	vtxoAmount := amount - 5000
	err = client.RegisterVTXORequests(ctx, []btcutil.Amount{vtxoAmount})
	require.NoError(t, err, "client should register VTXO request")
}

// latestClientJoinRoundRequest returns the latest JoinRoundRequest emitted by
// the given client.
func latestClientJoinRoundRequest(t *testing.T, transcript *MessageTranscript,
	clientID clientconn.ClientID) *clientround.JoinRoundRequest {

	entries := transcript.Entries()
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry.Direction != ClientToServer {
			continue
		}
		if entry.ClientID != clientID {
			continue
		}
		if entry.MsgType != "JoinRoundRequest" {
			continue
		}

		joinReq, ok := entry.Msg.(*clientround.JoinRoundRequest)
		require.True(t, ok,
			"transcript entry should be JoinRoundRequest")

		return joinReq
	}

	require.FailNowf(t, "missing JoinRoundRequest",
		"client %s has no JoinRoundRequest in transcript", clientID)

	return nil
}

// waitForBoardingFailedEvent waits for a BoardingFailed event for the target
// client and returns it.
func waitForBoardingFailedEvent(t *testing.T, h *E2EHarness,
	clientID clientconn.ClientID,
	timeout time.Duration) *clientround.BoardingFailed {

	event, err := h.Bridge().WaitForEvent(
		clientID, func(e clientround.ClientEvent) bool {
			_, ok := e.(*clientround.BoardingFailed)
			return ok
		}, timeout,
	)
	require.NoError(t, err, "should receive BoardingFailed event")

	failed, ok := event.(*clientround.BoardingFailed)
	require.True(t, ok, "event should be BoardingFailed")

	return failed
}

// buildForgedJoinRoundRequest constructs an attacker-authenticated join
// request that reuses another participant's boarding input data.
func buildForgedJoinRoundRequest(ctx context.Context, t *testing.T,
	h *E2EHarness, attacker *TestClient,
	source *clientround.JoinRoundRequest) *clientround.JoinRoundRequest {

	require.NotNil(t, source, "source join request must be provided")
	require.NotEmpty(t, source.BoardingRequests,
		"source join request must include a boarding input")

	forgedReq := cloneClientJoinRoundRequest(source)

	walletSigner := attacker.Backend().ClientWallet()

	identifierKey, err := walletSigner.DeriveNextKey(
		ctx, keychain.KeyFamilyNodeKey,
	)
	require.NoError(t, err, "should derive attacker identifier key")
	require.NotNil(t, identifierKey,
		"identifier key descriptor should exist")

	forgedProofKey, err := walletSigner.DeriveNextKey(
		ctx, keychain.KeyFamilyNodeKey,
	)
	require.NoError(t, err, "should derive attacker proof key")
	require.NotNil(t, forgedProofKey, "proof key descriptor should exist")

	boardingReq := forgedReq.BoardingRequests[0]
	require.NotNil(t, boardingReq.Outpoint, "boarding outpoint must be set")

	prevOut := fetchChainPrevOut(t, h, *boardingReq.Outpoint)

	forgedAuth := buildForgedJoinRoundAuth(
		t, walletSigner, forgedReq, boardingReq,
		prevOut, *identifierKey, *forgedProofKey,
	)

	forgedReq.Identifier = identifierKey.PubKey
	forgedReq.Auth = forgedAuth

	return forgedReq
}

// buildForgedJoinRoundAuth produces a BIP-322 payload where input 0 is signed
// by the attacker and the proof input witness is also signed by attacker keys.
// This intentionally mismatches the boarding script's owner key.
func buildForgedJoinRoundAuth(t *testing.T,
	signer clientround.ClientWallet,
	req *clientround.JoinRoundRequest,
	boardingReq clienttypes.BoardingRequest,
	boardingPrevOut *wire.TxOut,
	identifierKey keychain.KeyDescriptor,
	forgedProofKey keychain.KeyDescriptor,
) *clienttypes.JoinRoundAuth {

	sharedReq := toSharedJoinRoundRequest(req, identifierKey.PubKey, nil)

	message, err := clienttypes.JoinRoundAuthMessage(sharedReq)
	require.NoError(t, err, "should build canonical join auth message")

	intent := &bip322.Intent{
		Payload:    message,
		ValidFrom:  0,
		ValidUntil: 0,
	}

	intentMessage, err := intent.SigningMessage()
	require.NoError(t, err, "should build intent message")

	challengeScript, err := bip322.JoinRoundMessageChallenge(
		identifierKey.PubKey,
	)
	require.NoError(t, err, "should derive join auth challenge")

	require.NotNil(t, boardingReq.Outpoint, "boarding outpoint must be set")
	tapScript, err := scripts.VTXOTapScript(
		boardingReq.ClientKey,
		boardingReq.OperatorKey,
		boardingReq.ExitDelay,
	)
	require.NoError(t, err, "should rebuild boarding tapscript")

	spendInfo, err := scripts.NewVTXOSpendInfo(
		tapScript, scripts.VTXOTimeoutPathLeaf,
	)
	require.NoError(t, err, "should derive boarding timeout spend info")

	forgedSig, err := bip322.BuildAndSignFullTx(
		intentMessage,
		challengeScript,
		&forgedJoinAuthSigner{
			wallet:         signer,
			identifierKey:  identifierKey,
			proofKey:       forgedProofKey,
			proofPrevOut:   cloneWireTxOut(boardingPrevOut),
			proofSpendInfo: spendInfo,
		},
		bip322.WithToSignVersion(2),
		bip322.WithToSignAdditionalInputs(bip322.AdditionalInput{
			PreviousOutPoint: *boardingReq.Outpoint,
			Sequence:         boardingReq.ExitDelay,
			WitnessUtxo:      cloneWireTxOut(boardingPrevOut),
		}),
	)
	require.NoError(t, err, "should build forged BIP-322 signature")

	rawSig, err := forgedSig.Encode()
	require.NoError(t, err, "should encode forged signature")

	return &clienttypes.JoinRoundAuth{
		Message:    message,
		ValidFrom:  intent.ValidFrom,
		ValidUntil: intent.ValidUntil,
		Signature:  rawSig,
	}
}

// toSharedJoinRoundRequest converts the client outbox join request shape into
// the shared lib/types join request used for canonical auth message encoding.
func toSharedJoinRoundRequest(req *clientround.JoinRoundRequest,
	identifier *btcec.PublicKey,
	auth *clienttypes.JoinRoundAuth) *clienttypes.JoinRoundRequest {

	nBoarding := len(req.BoardingRequests)
	nVTXO := len(req.VTXORequests)
	nForfeit := len(req.ForfeitRequests)
	nLeave := len(req.LeaveRequests)

	shared := &clienttypes.JoinRoundRequest{
		Identifier: identifier,
		BoardingReqs: make(
			[]*clienttypes.BoardingRequest, 0, nBoarding,
		),
		VTXOReqs: make(
			[]*clienttypes.VTXORequest, 0, nVTXO,
		),
		ForfeitReqs: make(
			[]*clienttypes.ForfeitRequest, 0, nForfeit,
		),
		LeaveReqs: make(
			[]*clienttypes.LeaveRequest, 0, nLeave,
		),
		Auth: auth,
	}

	for i := 0; i < len(req.BoardingRequests); i++ {
		boardingReq := req.BoardingRequests[i]
		shared.BoardingReqs = append(shared.BoardingReqs, &boardingReq)
	}

	for i := 0; i < len(req.VTXORequests); i++ {
		vtxoReq := req.VTXORequests[i]
		shared.VTXOReqs = append(shared.VTXOReqs, &vtxoReq)
	}

	for i := 0; i < len(req.ForfeitRequests); i++ {
		forfeitReq := req.ForfeitRequests[i]
		clone := &clienttypes.ForfeitRequest{
			VTXOOutpoint: &forfeitReq.VTXOOutpoint,
		}
		shared.ForfeitReqs = append(
			shared.ForfeitReqs, clone,
		)
	}

	for i := 0; i < len(req.LeaveRequests); i++ {
		leaveReq := req.LeaveRequests[i]
		leaveClone := &clienttypes.LeaveRequest{
			Output: cloneWireTxOut(leaveReq.Output),
		}
		shared.LeaveReqs = append(
			shared.LeaveReqs, leaveClone,
		)
	}

	return shared
}

// cloneClientJoinRoundRequest deep-copies a client round JoinRoundRequest for
// safe local mutation in tests.
func cloneClientJoinRoundRequest(
	src *clientround.JoinRoundRequest) *clientround.JoinRoundRequest {

	if src == nil {
		return nil
	}

	nBoarding := len(src.BoardingRequests)
	nVTXO := len(src.VTXORequests)
	nForfeit := len(src.ForfeitRequests)
	nLeave := len(src.LeaveRequests)

	dst := &clientround.JoinRoundRequest{
		BoardingRequests: make(
			[]clienttypes.BoardingRequest, nBoarding,
		),
		VTXORequests: make(
			[]clienttypes.VTXORequest, nVTXO,
		),
		ForfeitRequests: make(
			[]*clientround.ForfeitRequest, nForfeit,
		),
		LeaveRequests: make(
			[]*clientround.LeaveRequest, nLeave,
		),
		RoundID:    src.RoundID,
		Identifier: src.Identifier,
		Auth:       src.Auth,
	}

	copy(dst.BoardingRequests, src.BoardingRequests)
	copy(dst.VTXORequests, src.VTXORequests)

	for i := 0; i < len(src.ForfeitRequests); i++ {
		if src.ForfeitRequests[i] == nil {
			continue
		}

		dst.ForfeitRequests[i] = &clientround.ForfeitRequest{
			VTXOOutpoint: src.ForfeitRequests[i].VTXOOutpoint,
		}
	}

	for i := 0; i < len(src.LeaveRequests); i++ {
		if src.LeaveRequests[i] == nil {
			continue
		}

		dst.LeaveRequests[i] = &clientround.LeaveRequest{
			Output: cloneWireTxOut(src.LeaveRequests[i].Output),
		}
	}

	if src.Auth != nil {
		dst.Auth = &clienttypes.JoinRoundAuth{
			Message:    append([]byte(nil), src.Auth.Message...),
			ValidFrom:  src.Auth.ValidFrom,
			ValidUntil: src.Auth.ValidUntil,
			Signature:  append([]byte(nil), src.Auth.Signature...),
		}
	}

	return dst
}

// fetchChainPrevOut loads the referenced transaction output from bitcoind for
// the provided outpoint.
func fetchChainPrevOut(t *testing.T, h *E2EHarness,
	outpoint wire.OutPoint) *wire.TxOut {

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err, "should get bitcoin RPC client")

	tx, err := rpcClient.GetRawTransaction(&outpoint.Hash)
	require.NoError(t, err, "should fetch transaction %s", outpoint.Hash)

	txOutIndex := int(outpoint.Index)
	require.Less(t, txOutIndex, len(tx.MsgTx().TxOut),
		"outpoint %s index should exist", outpoint)

	return cloneWireTxOut(tx.MsgTx().TxOut[txOutIndex])
}

// cloneWireTxOut returns a deep copy of the provided tx output.
func cloneWireTxOut(src *wire.TxOut) *wire.TxOut {
	if src == nil {
		return nil
	}

	return &wire.TxOut{
		Value:    src.Value,
		PkScript: append([]byte(nil), src.PkScript...),
	}
}

// forgedJoinAuthSigner signs a BIP-322 to_sign transaction with attacker keys.
// Input 0 signs the identifier challenge, and input 1 signs the boarding proof
// path with a key that does not match the boarding script.
type forgedJoinAuthSigner struct {
	wallet         input.Signer
	identifierKey  keychain.KeyDescriptor
	proofKey       keychain.KeyDescriptor
	proofPrevOut   *wire.TxOut
	proofSpendInfo *scripts.VTXOSpendData
}

var _ bip322.TxSigner = (*forgedJoinAuthSigner)(nil)

// SignBIP322 signs the message input and forged proof input in-place.
func (s *forgedJoinAuthSigner) SignBIP322(toSpend *wire.MsgTx,
	toSign *wire.MsgTx, prevFetcher txscript.PrevOutputFetcher,
	hashCache *txscript.TxSigHashes) error {

	if s.wallet == nil {
		return fmt.Errorf("wallet signer must be provided")
	}

	if len(toSign.TxIn) != 2 {
		return fmt.Errorf("expected 2 to_sign inputs, got %d",
			len(toSign.TxIn))
	}

	err := signJoinAuthMessageInputWithKey(
		s.wallet, toSign, toSpend, s.identifierKey, hashCache,
		prevFetcher,
	)
	if err != nil {
		return err
	}

	signDesc := scripts.VTXOSignDesc(
		s.proofKey, s.proofPrevOut, hashCache, prevFetcher, 1,
		s.proofSpendInfo,
	)
	witness, err := scripts.VTXOTimeoutSpendWitness(
		s.wallet, signDesc, toSign,
	)
	if err != nil {
		return fmt.Errorf("sign forged proof input: %w", err)
	}

	toSign.TxIn[1].Witness = witness

	return nil
}

// signJoinAuthMessageInputWithKey signs to_sign input 0 for the message
// challenge output using the provided identifier key.
func signJoinAuthMessageInputWithKey(signer input.Signer, toSign *wire.MsgTx,
	toSpend *wire.MsgTx, identifierKey keychain.KeyDescriptor,
	hashCache *txscript.TxSigHashes,
	prevFetcher txscript.PrevOutputFetcher) error {

	if signer == nil {
		return fmt.Errorf("wallet signer must be provided")
	}

	if len(toSpend.TxOut) == 0 {
		return fmt.Errorf("to_spend output must be provided")
	}

	signDesc := &input.SignDescriptor{
		KeyDesc:           identifierKey,
		Output:            toSpend.TxOut[0],
		HashType:          txscript.SigHashDefault,
		InputIndex:        0,
		SignMethod:        input.TaprootKeySpendBIP0086SignMethod,
		SigHashes:         hashCache,
		PrevOutputFetcher: prevFetcher,
		TapTweak:          []byte{},
	}

	sig, err := signer.SignOutputRaw(toSign, signDesc)
	if err != nil {
		return fmt.Errorf("sign message input: %w", err)
	}

	toSign.TxIn[0].Witness = wire.TxWitness{
		sig.Serialize(),
	}

	return nil
}
