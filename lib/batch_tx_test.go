package lib_test

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/ark/lib"
	"github.com/stretchr/testify/require"
)

// TestForfeitFlow tests the forfeit flow where a client creates a request
// to forfeit a VTXO (usually this will be in exchange to either a new VTXO
// or a leave-UTXO). The flow is as follows:
//   - client creates a forfeit request which points to the VTXO they want to
//     spend.
//   - server creates connector tree (after collecting all forfeit requests) and
//     assigns a single connector output to each forfeit request. It then sends
//     client: the connector output that it should use in the forfeit and also the
//     pk script to use in the dust output of the forfeit.
//   - client builds the forfeit transaction using the provided connector output
//     and the VTXO it is forfeiting. It signs this VTXO input and sends the
//     partly signed forfeit transaction to the server.
//   - If the client unrolls the forfeited VTXO, then the server can use this
//     forfeit tx to claim the funds.
func TestForfeitFlow(t *testing.T) {
	t.Parallel()

	// Setup test environment with an operator.
	env := newEnvironment(t)
	server := env.operator.ArkServer
	terms, err := server.Terms()
	require.NoError(t, err)

	// Fund the operator wallet with enough for two batches.
	env.fundWallet(env.operator.wallet, oneBtc*2)

	// Create two clients so that we can test that the connector outputs get
	// assigned correctly.
	client1 := env.newClient()
	client2 := env.newClient()

	// We'll need to run through 2 batches. In the first one, we let both
	// clients board the ark and create two new VTXOs.
	batch1ID, err := server.StartNewBatch()
	require.NoError(t, err)

	const (
		amt1 = oneBtc / 10 // 0.1 BTC
		amt2 = oneBtc / 20 // 0.05 BTC
	)
	// Prepare boarding requests for both clients.
	boardingReq1 := client1.prepBoarding(amt1)
	boardingReq2 := client2.prepBoarding(amt2)

	// Prepare VTXO requests for both clients for the same amounts.
	vtxoReq1, err := client1.CreateVTXORequest(terms, amt1)
	require.NoError(t, err)
	vtxoReq2, err := client2.CreateVTXORequest(terms, amt2)
	require.NoError(t, err)

	// Let both client's register their boarding and vtxo requests with the
	// batch.
	client1RequestID, err := server.RegisterRequests(batch1ID,
		&lib.ParticipantRoundRequest{
			BoardingReqs: []*lib.BoardingRequest{boardingReq1},
			VTXOReqs:     []*lib.VTXORequest{vtxoReq1},
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, client1RequestID)
	err = client1.BatchJoined(
		batch1ID,
		[]*lib.BoardingRequest{boardingReq1},
		[]*lib.VTXORequest{vtxoReq1}, nil, nil,
	)
	require.NoError(t, err)

	client2RequestID, err := server.RegisterRequests(batch1ID,
		&lib.ParticipantRoundRequest{
			BoardingReqs: []*lib.BoardingRequest{boardingReq2},
			VTXOReqs:     []*lib.VTXORequest{vtxoReq2},
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, client2RequestID)
	err = client2.BatchJoined(
		batch1ID,
		[]*lib.BoardingRequest{boardingReq2},
		[]*lib.VTXORequest{vtxoReq2}, nil, nil,
	)
	require.NoError(t, err)

	// Seal the batch.
	err = server.SealBatch(batch1ID)
	require.NoError(t, err)

	// Get client-specific info to verify the batch was built correctly
	client1Info, err := server.GetClientBatchInfo(batch1ID, client1RequestID)
	require.NoError(t, err)
	require.NotNil(t, client1Info.Transaction)
	require.NotNil(t, client1Info.VTXOInfo)
	require.Len(t, client1Info.VTXOInfo.VTXOPaths, 1)

	tx := client1Info.Transaction

	// Submit the client batch info to both clients.
	require.NoError(t, client1.BatchCreated(batch1ID, client1Info))

	client2Info, err := server.GetClientBatchInfo(batch1ID, client2RequestID)
	require.NoError(t, err)
	require.NoError(t, client2.BatchCreated(batch1ID, client2Info))

	// Both the clients need to sign the vtxo tree & get the server's
	// signatures for it before signing boarding inputs.
	client1Nonces, err := client1.GetNoncesForSigner(batch1ID, vtxoReq1.SigningKey)
	require.NoError(t, err)
	_, err = server.RegisterNonces(batch1ID, vtxoReq1.SigningKey, client1Nonces)
	require.NoError(t, err)
	client2Nonces, err := client2.GetNoncesForSigner(batch1ID, vtxoReq2.SigningKey)
	require.NoError(t, err)
	_, err = server.RegisterNonces(batch1ID, vtxoReq2.SigningKey, client2Nonces)
	require.NoError(t, err)

	// Get the aggregated nonces from the server and submit to both clients
	// and get their partial signatures for the vtxo tree.
	allTreeNonces, err := server.GetAggNonce(batch1ID)
	require.NoError(t, err)

	sigs1, err := client1.SubmitNonces(batch1ID, vtxoReq1.SigningKey, allTreeNonces)
	require.NoError(t, err)
	sigs2, err := client2.SubmitNonces(batch1ID, vtxoReq2.SigningKey, allTreeNonces)
	require.NoError(t, err)

	// Submit the partial signatures to the server.
	require.NoError(t, server.AddPartialSignatures(batch1ID, vtxoReq1.SigningKey, sigs1))
	require.NoError(t, server.AddPartialSignatures(batch1ID, vtxoReq2.SigningKey, sigs2))
	// Get the full signatures from the server and submit to both clients.
	sigs, err := server.GetTreeSigs(batch1ID)
	require.NoError(t, err)
	require.NoError(t, client1.SubmitTreeSigs(batch1ID, vtxoReq1.SigningKey, sigs))
	require.NoError(t, client2.SubmitTreeSigs(batch1ID, vtxoReq2.SigningKey, sigs))

	// At this point, the clients can sign their boarding inputs.
	sigsBoarding1, err := client1.SignBoardingInputs(batch1ID, tx)
	require.NoError(t, err)
	require.Len(t, sigsBoarding1, 1)
	sigsBoarding2, err := client2.SignBoardingInputs(batch1ID, tx)
	require.NoError(t, err)
	require.Len(t, sigsBoarding2, 1)

	// Submit these signatures to the server for verification.
	require.NoError(t, server.AddBoardingSignatures(batch1ID, sigsBoarding1))
	require.NoError(t, server.AddBoardingSignatures(batch1ID, sigsBoarding2))

	// The server can now sign the inputs.
	finalTx, err := server.SignInputs(batch1ID)
	require.NoError(t, err)

	// Broadcast the finalized transaction.
	env.broadcastTx(finalTx, 6)

	vtxos, err := client1.ListVTXOs()
	require.NoError(t, err)
	require.Len(t, vtxos, 1)
	client1VTXO := vtxos[0]
	vtxos, err = client2.ListVTXOs()
	require.NoError(t, err)
	require.Len(t, vtxos, 1)
	client2VTXO := vtxos[0]

	// Verify that the server now has the VTXOs in its store
	operatorImpl := server.(*lib.Operator)
	serverVTXOs, err := operatorImpl.ListVTXOs()
	require.NoError(t, err)
	require.Len(t, serverVTXOs, 2, "server should have 2 VTXOs after first batch")

	// Verify the server has the specific VTXOs
	var foundClient1VTXO, foundClient2VTXO bool
	for _, vtxo := range serverVTXOs {
		if vtxo.Outpoint.String() == client1VTXO.Outpoint.String() {
			foundClient1VTXO = true
			require.Equal(t, int64(amt1), int64(vtxo.Amount), "server VTXO amount should match client1")
		}
		if vtxo.Outpoint.String() == client2VTXO.Outpoint.String() {
			foundClient2VTXO = true
			require.Equal(t, int64(amt2), int64(vtxo.Amount), "server VTXO amount should match client2")
		}
	}
	require.True(t, foundClient1VTXO, "server should have client1's VTXO")
	require.True(t, foundClient2VTXO, "server should have client2's VTXO")

	// At this point, both clients now have valid VTXOs.
	// So we can now start batch 2 where both clients will create
	// forfeit requests to forfeit their VTXOs.
	batch2ID, err := server.StartNewBatch()
	require.NoError(t, err)

	// Let both clients create forfeit requests which point to the VTXO
	// they each created in the previous batch.
	forfeitReq1, err := client1.CreateForfeitRequest(client1VTXO.Outpoint)
	require.NoError(t, err)
	client1ForfeitRequestID, err := server.RegisterRequests(
		batch2ID, &lib.ParticipantRoundRequest{
			ForfeitReqs: []*lib.ForfeitRequest{forfeitReq1},
		},
	)
	require.NoError(t, err)
	err = client1.BatchJoined(
		batch2ID,
		nil, nil, nil, []*lib.ForfeitRequest{forfeitReq1},
	)
	require.NoError(t, err)

	forfeitReq2, err := client2.CreateForfeitRequest(client2VTXO.Outpoint)
	require.NoError(t, err)

	// Have client2 also add a leave request to provide funding for the batch
	leaveReq2, err := client2.CreateLeaveRequest(amt2 / 2) // Leave half of client2's funds
	require.NoError(t, err)

	// Register both forfeit and leave requests together for client2
	client2ForfeitRequestID, err := server.RegisterRequests(
		batch2ID, &lib.ParticipantRoundRequest{
			LeaveReqs:   []*lib.LeaveRequest{leaveReq2},
			ForfeitReqs: []*lib.ForfeitRequest{forfeitReq2},
		},
	)
	require.NoError(t, err)
	err = client2.BatchJoined(
		batch2ID,
		nil, nil, []*lib.LeaveRequest{leaveReq2}, []*lib.ForfeitRequest{forfeitReq2},
	)
	require.NoError(t, err)

	// Seal the batch.
	err = server.SealBatch(batch2ID)
	require.NoError(t, err)

	// Get client-specific info for forfeit requests
	client1ForfeitInfo, err := server.GetClientBatchInfo(batch2ID, client1ForfeitRequestID)
	require.NoError(t, err)
	// Submit the forfeit info to client1.
	require.NoError(t, client1.BatchCreated(batch2ID, client1ForfeitInfo))

	client2ForfeitInfo, err := server.GetClientBatchInfo(batch2ID, client2ForfeitRequestID)
	require.NoError(t, err)
	require.NoError(t, client2.BatchCreated(batch2ID, client2ForfeitInfo))

	// Test the new GetSignedForfeits functionality
	// Get forfeit address from the operator's wallet
	forfeitAddr, err := operatorImpl.GetOperatorWallet().GetForfeitAddress()
	require.NoError(t, err)

	forfeitScript, err := txscript.PayToAddrScript(forfeitAddr)
	require.NoError(t, err)

	// Get signed forfeit transactions from client1
	client1ForfeitTxs, err := client1.GetSignedForfeits(batch2ID, forfeitScript)
	require.NoError(t, err)
	require.Len(t, client1ForfeitTxs, 1) // Should have one forfeit tx

	// Get signed forfeit transactions from client2
	client2ForfeitTxs, err := client2.GetSignedForfeits(batch2ID, forfeitScript)
	require.NoError(t, err)
	require.Len(t, client2ForfeitTxs, 1) // Should have one forfeit tx

	// Submit signed forfeit transactions from client1 separately
	err = server.SubmitSignedForfeits(batch2ID, client1ForfeitRequestID, client1ForfeitTxs)
	require.NoError(t, err)

	// Submit signed forfeit transactions from client2 separately
	err = server.SubmitSignedForfeits(batch2ID, client2ForfeitRequestID, client2ForfeitTxs)
	require.NoError(t, err)

	// No boarding signatures needed since no clients are boarding in batch2
	// Just proceed to sign inputs

	// Sign and complete batch2 to verify VTXO removal
	tx2, err := server.SignInputs(batch2ID)
	require.NoError(t, err)
	env.broadcastTx(tx2, 6)

	// Verify that the forfeited VTXOs have been removed from the store
	serverVTXOsFinal, err := operatorImpl.ListVTXOs()
	require.NoError(t, err)
	require.Len(t, serverVTXOsFinal, 0)

	t.Log("Successfully verified VTXO lifecycle: VTXOs created, stored, then removed after forfeit")
}

// TestVTXOCreation tests that the client can negotiate the creation of a VTXO
// with the server.
func TestVTXOCreation(t *testing.T) {
	t.Parallel()

	// Setup test environment with an operator.
	env := newEnvironment(t)
	server := env.operator.ArkServer
	terms, err := server.Terms()
	require.NoError(t, err)

	// Fund the operator wallet.
	env.fundWallet(env.operator.wallet, oneBtc)

	// Create four clients.
	client1 := env.newClient()
	client2 := env.newClient()
	client3 := env.newClient()
	client4 := env.newClient()

	// Let the server start a new batch.
	batchID, err := server.StartNewBatch()
	require.NoError(t, err)

	// Let all the clients create and register VTXO requests.
	const (
		amt1 = 1000000
		amt2 = 2000000
		amt3 = 3000000
		amt4 = 4000000
	)
	vtxoReq1, err := client1.CreateVTXORequest(terms, amt1)
	require.NoError(t, err)
	client1RequestID, err := server.RegisterRequests(batchID, &lib.ParticipantRoundRequest{
		VTXOReqs: []*lib.VTXORequest{vtxoReq1},
	})
	require.NoError(t, err)
	require.NotEmpty(t, client1RequestID)
	err = client1.BatchJoined(
		batchID, nil, []*lib.VTXORequest{vtxoReq1}, nil, nil,
	)
	require.NoError(t, err)

	vtxoReq2, err := client2.CreateVTXORequest(terms, amt2)
	require.NoError(t, err)
	client2RequestID, err := server.RegisterRequests(batchID, &lib.ParticipantRoundRequest{
		VTXOReqs: []*lib.VTXORequest{vtxoReq2},
	})
	require.NoError(t, err)
	require.NotEmpty(t, client2RequestID)
	err = client2.BatchJoined(
		batchID, nil, []*lib.VTXORequest{vtxoReq2}, nil, nil,
	)
	require.NoError(t, err)

	vtxoReq3, err := client3.CreateVTXORequest(terms, amt3)
	require.NoError(t, err)
	client3RequestID, err := server.RegisterRequests(batchID, &lib.ParticipantRoundRequest{
		VTXOReqs: []*lib.VTXORequest{vtxoReq3},
	})
	require.NoError(t, err)
	require.NotEmpty(t, client3RequestID)
	err = client3.BatchJoined(
		batchID, nil, []*lib.VTXORequest{vtxoReq3}, nil, nil,
	)
	require.NoError(t, err)

	vtxoReq4, err := client4.CreateVTXORequest(terms, amt4)
	require.NoError(t, err)
	client4RequestID, err := server.RegisterRequests(batchID, &lib.ParticipantRoundRequest{
		VTXOReqs: []*lib.VTXORequest{vtxoReq4},
	})
	require.NoError(t, err)
	require.NotEmpty(t, client4RequestID)
	err = client4.BatchJoined(
		batchID, nil, []*lib.VTXORequest{vtxoReq4}, nil, nil,
	)
	require.NoError(t, err)

	// Build the commitment transaction. This should include a batch
	// output that includes the new VTXO.
	err = server.SealBatch(batchID)
	require.NoError(t, err)

	// Get client-specific info to verify the batch was built correctly
	client1Info, err := server.GetClientBatchInfo(batchID, client1RequestID)
	require.NoError(t, err)
	require.NotNil(t, client1Info.Transaction)
	require.NotNil(t, client1Info.VTXOInfo)
	require.Len(t, client1Info.VTXOInfo.VTXOPaths, 1)

	client2Info, err := server.GetClientBatchInfo(batchID, client2RequestID)
	require.NoError(t, err)
	require.NotNil(t, client2Info.VTXOInfo)
	require.Len(t, client2Info.VTXOInfo.VTXOPaths, 1)

	// Use the transaction from client1's info (they should be the same)
	tx := client1Info.Transaction
	batchOutput := client1Info.VTXOInfo.BatchOutput

	// Assert that the batch output has the expected index
	require.EqualValues(t, uint32(0), batchOutput.Idx)

	// Recompute the expected batch pk script and assert that it matches the
	// one in the transaction.
	output, err := lib.BuildBatchOutput(
		lib.VTXOLeavesFromRequests([]*lib.VTXORequest{
			vtxoReq1, vtxoReq2, vtxoReq3, vtxoReq4,
		}),
		batchOutput.SignerKey,
		batchOutput.Tree.SweepKey,
		batchOutput.Tree.SweepDelay,
	)
	require.NoError(t, err)
	require.EqualValues(t, output.PkScript,
		tx.TxOut[batchOutput.Idx].PkScript)

	// Assert that the batch output amount matches the sum of the VTXO
	expectedAmount := amt1 + amt2 + amt3 + amt4
	require.EqualValues(t, expectedAmount, tx.TxOut[batchOutput.Idx].Value)

	// Since we have 4 leaves and used a radix of 2, the tree should have a
	// depth of 3. And the total number of transactions should be 7.
	tree := batchOutput.Tree
	require.EqualValues(t, 3, tree.Root.Depth())
	require.EqualValues(t, 7, tree.Root.NumTx())
	require.NoError(t, tree.Verify())

	// The number of cosigners of the root node should be 5: one for each
	// client plus one for the operator
	require.EqualValues(t, 5, len(tree.Root.CoSigners))

	// Make sure that the correct keys are present in the cosigner list.
	cosigners := tree.Root.CoSigners
	require.True(t, lib.ContainsCosigner(cosigners, vtxoReq1.SigningKey))
	require.True(t, lib.ContainsCosigner(cosigners, vtxoReq2.SigningKey))
	require.True(t, lib.ContainsCosigner(cosigners, vtxoReq3.SigningKey))
	require.True(t, lib.ContainsCosigner(cosigners, vtxoReq4.SigningKey))
	require.True(t, lib.ContainsCosigner(cosigners, batchOutput.SignerKey))

	// Extract the Tree path that is relevant for client1 1 and verify it.
	tree1 := tree.ExtractPathForCosigner(vtxoReq1.SigningKey)
	require.NoError(t, tree1.Verify())

	// The depth of this tree should also be 3, but the number of
	// transactions should be 3 this time.
	require.EqualValues(t, 3, tree1.Root.Depth())
	require.EqualValues(t, 3, tree1.Root.NumTx())

	// Pretty print the full tree and client1's path for debugging
	t.Logf("Full VTXO Tree:\n%s", tree.PrettyPrint())
	t.Logf("Client1's Path:\n%s", tree1.PrettyPrint())

	submitTreeAndRegisterNonces := func(client lib.ArkClient, clientRequestID string, vtxoReq *lib.VTXORequest) {
		// Get the client's batch info and submit it to the client
		clientInfo, err := server.GetClientBatchInfo(batchID, clientRequestID)
		require.NoError(t, err)
		require.NoError(t, client.BatchCreated(batchID, clientInfo))

		clientNonces, err := client.GetNoncesForSigner(batchID, vtxoReq.SigningKey)
		require.NoError(t, err)
		require.Len(t, clientNonces, 3)
		// Register the nonces with the server.
		_, err = server.RegisterNonces(batchID, vtxoReq.SigningKey, clientNonces)
		require.NoError(t, err)
	}

	submitTreeAndRegisterNonces(client1, client1RequestID, vtxoReq1)
	submitTreeAndRegisterNonces(client2, client2RequestID, vtxoReq2)
	submitTreeAndRegisterNonces(client3, client3RequestID, vtxoReq3)
	submitTreeAndRegisterNonces(client4, client4RequestID, vtxoReq4)

	allTreeNonces, err := server.GetAggNonce(batchID)
	require.NoError(t, err)

	// There are 7 txs so there should be 7 sets of nonces.
	require.Len(t, allTreeNonces, 7)
	// all txs should have at least 2 nonces.
	for txid, nonces := range allTreeNonces {
		require.GreaterOrEqual(t, len(nonces), 2,
			"tx %s has less than 2 nonces", txid)
	}

	// submit the nonces to all the clients.
	sigs1, err := client1.SubmitNonces(batchID, vtxoReq1.SigningKey, allTreeNonces)
	require.NoError(t, err)
	sigs2, err := client2.SubmitNonces(batchID, vtxoReq2.SigningKey, allTreeNonces)
	require.NoError(t, err)
	sigs3, err := client3.SubmitNonces(batchID, vtxoReq3.SigningKey, allTreeNonces)
	require.NoError(t, err)
	sigs4, err := client4.SubmitNonces(batchID, vtxoReq4.SigningKey, allTreeNonces)
	require.NoError(t, err)
	fmt.Println(sigs1, sigs2, sigs3, sigs4)

	// Add all the partial sigs.
	require.NoError(t, server.AddPartialSignatures(batchID, vtxoReq1.SigningKey, sigs1))
	require.NoError(t, server.AddPartialSignatures(batchID, vtxoReq2.SigningKey, sigs2))
	require.NoError(t, server.AddPartialSignatures(batchID, vtxoReq3.SigningKey, sigs3))
	require.NoError(t, server.AddPartialSignatures(batchID, vtxoReq4.SigningKey, sigs4))

	// Get all the full sigs from the server.
	sigs, err := server.GetTreeSigs(batchID)
	require.NoError(t, err)

	// There should be 7 full sigs, one for each transaction.
	require.Len(t, sigs, 7)

	// Now, pass the full sigs to all clients for storage and verification.
	require.NoError(t, client1.SubmitTreeSigs(batchID, vtxoReq1.SigningKey, sigs))
	require.NoError(t, client2.SubmitTreeSigs(batchID, vtxoReq2.SigningKey, sigs))
	require.NoError(t, client3.SubmitTreeSigs(batchID, vtxoReq3.SigningKey, sigs))
	require.NoError(t, client4.SubmitTreeSigs(batchID, vtxoReq4.SigningKey, sigs))

	// The server can now sign the inputs.
	tx, err = server.SignInputs(batchID)
	require.NoError(t, err)

	// Broadcast the finalized transaction.
	env.broadcastTx(tx, 6)

	// Confirm that the clients have the VTXOs in their store.
	assertVTXO := func(client lib.ArkClient, expectedPkScript []byte) {
		vtxos, err := client.ListVTXOs()
		require.NoError(t, err)
		require.Len(t, vtxos, 1)
		require.Equal(t, expectedPkScript, vtxos[0].PkScript)
	}

	assertVTXO(client1, vtxoReq1.PkScript)
	assertVTXO(client2, vtxoReq2.PkScript)
	assertVTXO(client3, vtxoReq3.PkScript)
	assertVTXO(client4, vtxoReq4.PkScript)

	// TODO(elle): now, check that the client can unroll the VTXO
	//  unilaterally.
}

// TestLeaveRequest tests that the client can create a leave request, that the
// server can register the leave request, build a batch transaction including
// the leave output, sign the inputs, and broadcast the finalized transaction.
func TestLeaveRequest(t *testing.T) {
	t.Parallel()

	// Setup test environment with an operator.
	env := newEnvironment(t)
	server := env.operator.ArkServer

	// Fund the operator wallet.
	env.fundWallet(env.operator.wallet, oneBtc)

	// Create a client.
	client := env.newClient()

	// Let the server start a new batch.
	batchID, err := server.StartNewBatch()
	require.NoError(t, err)

	// Create a leave request.
	leaveReq, err := client.CreateLeaveRequest(oneBtc / 20)
	require.NoError(t, err)

	// Register the leave request with the operator.
	clientRequestID, err := server.RegisterRequests(batchID,
		&lib.ParticipantRoundRequest{
			LeaveReqs: []*lib.LeaveRequest{leaveReq},
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, clientRequestID)

	err = server.SealBatch(batchID)
	require.NoError(t, err)

	// Get client-specific info
	clientInfo, err := server.GetClientBatchInfo(batchID, clientRequestID)
	require.NoError(t, err)
	require.NotNil(t, clientInfo.Transaction)
	require.Nil(t, clientInfo.VTXOInfo)      // No VTXO requests
	require.Nil(t, clientInfo.ConnectorInfo) // No forfeit requests

	tx := clientInfo.Transaction

	// Verify that the transaction contains the leave output.
	found := false
	for _, txOut := range tx.TxOut {
		if txOut.Value == leaveReq.Output.Value &&
			string(txOut.PkScript) == string(leaveReq.Output.PkScript) {
			found = true
			break
		}
	}
	require.True(t, found, "leave output not found in transaction")

	// The server can now sign the inputs.
	tx, err = server.SignInputs(batchID)
	require.NoError(t, err)

	// Broadcast the finalized transaction.
	env.broadcastTx(tx, 6)
}

// TestBoardingInputSigning tests that the client can register a boarding
// request with the server, that the server can then create a batch transaction
// using the boarding input, and that the client can sign the input.
func TestBoardingInputSigning(t *testing.T) {
	t.Parallel()

	// Setup test environment with an operator.
	env := newEnvironment(t)
	server := env.operator.ArkServer

	// Create a client.
	client := env.newClient()

	// Let the client prepare a boarding UTXO.
	boardingReq := client.prepBoarding(oneBtc / 10)

	// Let the server start a new batch.
	batchID, err := server.StartNewBatch()
	require.NoError(t, err)

	// Register the boarding request with the operator.
	clientRequestID, err := server.RegisterRequests(batchID,
		&lib.ParticipantRoundRequest{
			BoardingReqs: []*lib.BoardingRequest{boardingReq},
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, clientRequestID)

	err = client.BatchJoined(batchID, []*lib.BoardingRequest{boardingReq}, nil, nil, nil)
	require.NoError(t, err)

	// Build the batch transaction.
	err = server.SealBatch(batchID)
	require.NoError(t, err)

	// Get client-specific info
	clientInfo, err := server.GetClientBatchInfo(batchID, clientRequestID)
	require.NoError(t, err)
	require.NotNil(t, clientInfo.Transaction)
	require.Nil(t, clientInfo.VTXOInfo)      // No VTXO requests
	require.Nil(t, clientInfo.ConnectorInfo) // No forfeit requests

	tx := clientInfo.Transaction

	// Let the client sign the boarding input.
	sigs, err := client.SignBoardingInputs(batchID, tx)
	require.NoError(t, err)
	require.Len(t, sigs, 1)

	// Submit these signatures to the server for verification.
	require.NoError(t, server.AddBoardingSignatures(batchID, sigs))

	// The server can now add its own signatures to each of the inputs.
	tx, err = server.SignInputs(batchID)
	require.NoError(t, err)

	// Broadcast the finalized transaction.
	env.broadcastTx(tx, 6)
}

// TestBoardingAndLeave tests a scenario where multiple clients perform both
// boarding and leave operations in the same batch.
func TestBoardingAndLeave(t *testing.T) {
	t.Parallel()

	// Setup test environment with an operator.
	env := newEnvironment(t)
	server := env.operator.ArkServer

	// Fund the operator wallet generously.
	env.fundWallet(env.operator.wallet, oneBtc*10)

	// Create multiple clients.
	boardingClient := env.newClient()
	leaveClient := env.newClient()
	mixedClient := env.newClient()

	// Boarding client: Prepare boarding requests
	boardingReq1 := boardingClient.prepBoarding(oneBtc / 10) // 0.1 BTC
	boardingReq2 := boardingClient.prepBoarding(oneBtc / 20) // 0.05 BTC

	// Let the server start a new batch.
	batchID, err := server.StartNewBatch()
	require.NoError(t, err)

	// Leave client: Create leave requests
	leaveReq1, err := leaveClient.CreateLeaveRequest(oneBtc / 16) // 0.0625 BTC
	require.NoError(t, err)
	leaveReq2, err := leaveClient.CreateLeaveRequest(oneBtc / 25) // 0.04 BTC
	require.NoError(t, err)

	// Mixed client: Both boarding and leave.
	mixedBoardingReq := mixedClient.prepBoarding(oneBtc / 8)          // 0.125 BTC
	mixedLeaveReq, err := mixedClient.CreateLeaveRequest(oneBtc / 16) // 0.0625 BTC
	require.NoError(t, err)

	// Register requests per client (as required by new API)
	// Boarding client requests
	boardingClientRequestID, err := server.RegisterRequests(batchID,
		&lib.ParticipantRoundRequest{
			BoardingReqs: []*lib.BoardingRequest{boardingReq1, boardingReq2},
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, boardingClientRequestID)

	// Leave client requests
	leaveClientRequestID, err := server.RegisterRequests(batchID,
		&lib.ParticipantRoundRequest{
			LeaveReqs: []*lib.LeaveRequest{leaveReq1, leaveReq2},
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, leaveClientRequestID)

	// Mixed client requests
	mixedClientRequestID, err := server.RegisterRequests(batchID,
		&lib.ParticipantRoundRequest{
			BoardingReqs: []*lib.BoardingRequest{mixedBoardingReq},
			LeaveReqs:    []*lib.LeaveRequest{mixedLeaveReq},
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, mixedClientRequestID)

	// Notify each client that they have joined the batch.
	err = boardingClient.BatchJoined(
		batchID,
		[]*lib.BoardingRequest{boardingReq1, boardingReq2}, nil, nil,
		nil,
	)
	require.NoError(t, err)
	err = leaveClient.BatchJoined(
		batchID, nil, nil,
		[]*lib.LeaveRequest{leaveReq1, leaveReq2}, nil,
	)
	require.NoError(t, err)
	err = mixedClient.BatchJoined(
		batchID, []*lib.BoardingRequest{mixedBoardingReq}, nil,
		[]*lib.LeaveRequest{mixedLeaveReq}, nil,
	)
	require.NoError(t, err)

	// Build the batch transaction.
	err = server.SealBatch(batchID)
	require.NoError(t, err)

	// Get client-specific info from one of the clients (they share the same transaction)
	clientInfo, err := server.GetClientBatchInfo(batchID, boardingClientRequestID)
	require.NoError(t, err)
	require.NotNil(t, clientInfo.Transaction)
	require.Nil(t, clientInfo.VTXOInfo)      // No VTXO requests
	require.Nil(t, clientInfo.ConnectorInfo) // No forfeit requests

	tx := clientInfo.Transaction

	// Verify that all boarding inputs are in the transaction (at least 3).
	require.GreaterOrEqual(t, len(tx.TxIn), 3, "Expected at least 3 boarding inputs in transaction")

	// Recreate arrays for validation
	allBoardingReqs := []*lib.BoardingRequest{
		boardingReq1, boardingReq2, mixedBoardingReq,
	}
	allLeaveReqs := []*lib.LeaveRequest{
		leaveReq1, leaveReq2, mixedLeaveReq,
	}

	// Verify that all the boarding inputs are in the transaction.
	foundBoardingInputs := 0
	for _, txIn := range tx.TxIn {
		for _, boardingReq := range allBoardingReqs {
			if txIn.PreviousOutPoint == *boardingReq.Outpoint {
				foundBoardingInputs++
				break
			}
		}
	}

	// Verify that all leave outputs are in the transaction.
	foundLeaveOutputs := 0
	for _, txOut := range tx.TxOut {
		for _, leaveReq := range allLeaveReqs {
			if txOut.Value == leaveReq.Output.Value &&
				string(txOut.PkScript) == string(leaveReq.Output.PkScript) {
				foundLeaveOutputs++
				break
			}
		}
	}
	require.Equal(t, 3, foundLeaveOutputs, "Not all leave outputs found in transaction")

	// Each client signs their own boarding inputs.
	boardingClientSigs, err := boardingClient.SignBoardingInputs(batchID, tx)
	require.NoError(t, err)
	require.Len(t, boardingClientSigs, 2)

	// Submit these to the operator.
	require.NoError(t, server.AddBoardingSignatures(batchID, boardingClientSigs))

	mixedClientSigs, err := mixedClient.SignBoardingInputs(batchID, tx)
	require.NoError(t, err)
	require.Len(t, mixedClientSigs, 1)

	// Submit these to the operator.
	require.NoError(t, server.AddBoardingSignatures(batchID, mixedClientSigs))

	// The server can now sign the inputs to complete the transaction.
	tx, err = server.SignInputs(batchID)
	require.NoError(t, err)

	// Broadcast the finalized transaction.
	env.broadcastTx(tx, 6)
}

// TestSweepExpiredBoardingUTXOs tests that the client can sweep
// expired boarding UTXOs.
func TestSweepExpiredBoardingUTXOs(t *testing.T) {
	t.Parallel()

	// Setup test environment with an operator.
	env := newEnvironment(t)

	// Create a new client.
	client := env.newClient()

	// Fund the client wallet.
	env.fundWallet(client.wallet, oneBtc)

	// Request the operator's terms.
	terms, err := env.operator.Terms()
	require.NoError(t, err)

	// Using the terms, let the client derive a new boarding address.
	addr, err := client.NewBoardingAddress(terms)
	require.NoError(t, err)

	// Fund the boarding address.
	env.fundAddr(addr, oneBtc/10)

	// Wait for the client to detect the boarding UTXO.
	client.waitForBoardingUTXO(addr)

	// Now, mine blocks to exceed the boarding exit delay.
	blocksToMine := terms.BoardingExitDelay + 1
	_, err = env.generateBlocks(blocksToMine)
	require.NoError(t, err)

	// Wait for the client to see the boarding utxos as expired.
	waitFor(t, func() bool {
		utxos, err := client.ListBoardingUTXOs()
		require.NoError(t, err)
		require.Len(t, utxos, 1)

		return utxos[0].Expired()
	})

	sweepTx, err := client.SweepExpiredBoardingUTXOs()
	require.NoError(t, err)

	// Submit the transaction to the network.
	env.broadcastTx(sweepTx, 6)

	// Wait for the client to no longer see any boarding UTXOs.
	waitFor(t, func() bool {
		utxos, err := client.ListBoardingUTXOs()
		require.NoError(t, err)

		return len(utxos) == 0
	})
}
