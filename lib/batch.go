package lib

import (
	"bytes"
	"fmt"
	"sort"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/google/uuid"
	"github.com/lightninglabs/ark/lib/wallets"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

type batchState int

const (
	// batchStateRegistration is the initial state of a batch where
	// requests can be registered.
	batchStateRegistration batchState = iota

	batchSealed
	batchStateNonceCollection
	batchStateSigCollection
	batchStateInputSigCollection
	batchStateSignInputs
	batchStateComplete
)

// BoardingInput holds all the info about a boarding UTXO to be included in
// the batch that the Batch needs in order to add it to the batch transaction
// and then sign the input.
type BoardingInput struct {
	// Outpoint represents the UTXO that will be used as input to the batch
	// transaction.
	Outpoint *wire.OutPoint

	// Tapscript contains the boarding tapscript for spending via script
	// path.
	Tapscript *waddrmgr.Tapscript

	// Value is the amount of satoshis in this UTXO.
	Value btcutil.Amount

	// PkScript is the script of the UTXO (taproot script).
	PkScript []byte

	// ClientKey is the public key of the client who owns this boarding
	// input.
	ClientKey *btcec.PublicKey

	// OperatorKeyDesc is the key descriptor of the operator's key
	// that corresponds to the operator key in the tapscript.
	OperatorKeyDesc *keychain.KeyDescriptor
}

// ClientRequestInfo tracks all requests from a single client
type ClientRequestInfo struct {
	ClientID     string
	BoardingReqs []*BoardingInput
	LeaveReqs    []*LeaveRequest
	VTXOReqs     []*VTXORequest
	ForfeitReqs  []*ForfeitRequest
}

type BatchConfig struct {
	OperatorMainKey *keychain.KeyDescriptor
	BatchKey        *keychain.KeyDescriptor
	SweepKey        *keychain.KeyDescriptor

	VTXOTreeRadix      int
	ConnectorTreeRadix int

	// BoardingExitDelay is the minimum CSV delay to use for boarding
	// outputs that the operator expects.
	BoardingExitDelay uint32

	// VTXOExitDelay is the minimum CSV delay to use for VTXO outputs. This
	// delay will give the server time to respond to unilateral spends of
	// a VTXO that has been forfeit or spent.
	VTXOExitDelay uint32

	// SweepDelay is the CSV delay to use for the sweep path of VTXs in the
	// VTXT. This defines the expiry of a batch.
	SweepDelay uint32

	DustLimit  btcutil.Amount
	TargetConf uint32

	Wallet       wallets.OperatorWallet
	FeeEstimator chainfee.Estimator
}

// Batch contains all state and logic for a single batch transaction
type Batch struct {
	uuid string

	cfg *BatchConfig

	state batchState

	// Client-centric tracking
	clientRequests map[string]*ClientRequestInfo

	// Sealed batch results (populated after SealBatch)
	batchOutputs     []*BatchOutputInfo
	connectorOutputs []*ConnectorOutputInfo

	boardingSigs map[wire.OutPoint]*BoardingInputSignature
	tx           *wire.MsgTx
	prevOuts     map[wire.OutPoint]*wire.TxOut
	sigHashes    *txscript.TxSigHashes

	batchSigningCoordinator *TreeSignerCoordinator
	treeSigningSession      *TreeSignerSession

	mu sync.RWMutex
}

var _ BatchBuilder = (*Batch)(nil)

// Helper methods to collect requests from all clients

// getAllBoardingInputs collects all boarding requests from all clients
func (b *Batch) getAllBoardingInputs() []*BoardingInput {
	var boardingReqs []*BoardingInput
	for _, clientInfo := range b.clientRequests {
		boardingReqs = append(boardingReqs, clientInfo.BoardingReqs...)
	}
	return boardingReqs
}

// getAllLeaveRequests collects all leave requests from all clients
func (b *Batch) getAllLeaveRequests() []*LeaveRequest {
	var leaveReqs []*LeaveRequest
	for _, clientInfo := range b.clientRequests {
		leaveReqs = append(leaveReqs, clientInfo.LeaveReqs...)
	}
	return leaveReqs
}

// getAllVTXORequests collects all VTXO requests from all clients
func (b *Batch) getAllVTXORequests() []*VTXORequest {
	var vtxoReqs []*VTXORequest
	for _, clientInfo := range b.clientRequests {
		vtxoReqs = append(vtxoReqs, clientInfo.VTXOReqs...)
	}
	return vtxoReqs
}

// getAllForfeitRequests collects all forfeit requests from all clients
func (b *Batch) getAllForfeitRequests() []*ForfeitRequest {
	var forfeitReqs []*ForfeitRequest
	for _, clientInfo := range b.clientRequests {
		forfeitReqs = append(forfeitReqs, clientInfo.ForfeitReqs...)
	}
	return forfeitReqs
}

func NewBatch(cfg *BatchConfig) (*Batch, error) {
	uuid, err := uuid.NewUUID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate batch UUID: %w", err)
	}

	return &Batch{
		uuid:           uuid.String(),
		cfg:            cfg,
		state:          batchStateRegistration,
		clientRequests: make(map[string]*ClientRequestInfo),
		prevOuts:       make(map[wire.OutPoint]*wire.TxOut),
		boardingSigs:   make(map[wire.OutPoint]*BoardingInputSignature),
	}, nil
}

func (b *Batch) UUID() string {
	return b.uuid
}

// RegisterRequests adds new requests to the batch and returns a client request ID.
func (b *Batch) RegisterRequests(boardingReqs []*BoardingInput,
	leaveReqs []*LeaveRequest, vtxoReqs []*VTXORequest,
	forfeitReqs []*ForfeitRequest) (string, error) {

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state != batchStateRegistration {
		return "", fmt.Errorf("batch not in registration state")
	}

	// Generate a unique client request ID
	clientUUID, err := uuid.NewUUID()
	if err != nil {
		return "", fmt.Errorf("failed to generate client request UUID: %w", err)
	}
	clientRequestID := clientUUID.String()

	// Store client requests
	clientInfo := &ClientRequestInfo{
		ClientID:     clientRequestID,
		BoardingReqs: boardingReqs,
		LeaveReqs:    leaveReqs,
		VTXOReqs:     vtxoReqs,
		ForfeitReqs:  forfeitReqs,
	}
	b.clientRequests[clientRequestID] = clientInfo

	return clientRequestID, nil
}

// SealBatch seals the batch for further request registration. At this point,
// the commitment transaction can be built.
func (b *Batch) SealBatch() error {

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state != batchStateRegistration {
		return fmt.Errorf("batch not in registration state")
	}

	tx, batchOutputs, connectors, err := b.buildCommitmentTx()
	if err != nil {
		return fmt.Errorf("failed to build commitment tx: %w", err)
	}

	// Store results for later client info retrieval
	b.tx = tx
	b.batchOutputs = batchOutputs
	b.connectorOutputs = connectors

	if len(batchOutputs) > 0 {
		if len(batchOutputs) != 1 {
			return fmt.Errorf("expected exactly one "+
				"batch output, got %d", len(batchOutputs))
		}
		vtxoTree := batchOutputs[0].Tree

		// Create the tree signer coordinator.
		coordinator, err := vtxoTree.NewTreeSignerCoordinator()
		if err != nil {
			return fmt.Errorf("failed to create tree "+
				"signer coordinator: %w", err)
		}

		// Use the new simplified Tree method for creating the signing
		// session.
		signSess, err := vtxoTree.NewTreeSignerSession(
			b.cfg.Wallet, b.cfg.BatchKey,
		)
		if err != nil {
			return fmt.Errorf("failed to create tree "+
				"signing session: %w", err)
		}

		// We can immediately register our own nonces.
		nonces, err := signSess.GetNonces()
		if err != nil {
			return fmt.Errorf("failed to get own "+
				"nonces: %w", err)
		}

		err = coordinator.AddNonces(b.cfg.BatchKey.PubKey, nonces)
		if err != nil {
			return fmt.Errorf("failed to add own "+
				"nonces: %w", err)
		}

		b.batchSigningCoordinator = coordinator
		b.treeSigningSession = signSess
		b.state = batchStateNonceCollection
	} else if len(b.getAllBoardingInputs()) > 0 {
		b.state = batchStateInputSigCollection
	} else {
		b.state = batchStateSignInputs
	}

	return nil
}

// GetClientBatchInfo returns client-specific batch information
func (b *Batch) GetClientBatchInfo(clientRequestID string) (*ClientBatchInfo, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	// Verify client exists
	clientInfo, exists := b.clientRequests[clientRequestID]
	if !exists {
		return nil, fmt.Errorf("client request ID not found: %s", clientRequestID)
	}

	// Verify batch has been sealed
	if b.tx == nil {
		return nil, fmt.Errorf("batch has not been sealed yet")
	}

	info := &ClientBatchInfo{
		Transaction: b.tx,
	}

	// Add VTXO info if client has VTXO requests
	if len(clientInfo.VTXOReqs) > 0 {
		vtxoInfo, err := b.buildClientVTXOInfo(clientInfo)
		if err != nil {
			return nil, fmt.Errorf("failed to build VTXO info: %w", err)
		}
		info.VTXOInfo = vtxoInfo
	}

	// Add connector info if client has forfeit requests
	if len(clientInfo.ForfeitReqs) > 0 {
		connectorInfo, err := b.buildClientConnectorInfo(clientInfo)
		if err != nil {
			return nil, fmt.Errorf("failed to build connector info: %w", err)
		}
		info.ConnectorInfo = connectorInfo
	}

	return info, nil
}

// GetOperatorWallet returns the operator wallet for testing purposes
func (b *Batch) GetOperatorWallet() wallets.OperatorWallet {
	return b.cfg.Wallet
}

// buildClientVTXOInfo builds VTXO-specific information for a client
func (b *Batch) buildClientVTXOInfo(clientInfo *ClientRequestInfo) (*ClientVTXOInfo, error) {
	if len(b.batchOutputs) == 0 {
		return nil, fmt.Errorf("no batch outputs available")
	}

	if len(b.batchOutputs) != 1 {
		return nil, fmt.Errorf("expected exactly one batch output, got %d", len(b.batchOutputs))
	}

	batchOutput := b.batchOutputs[0]
	vtxoPaths := make([]*Tree, 0, len(clientInfo.VTXOReqs))

	// Extract path for each VTXO request using GetLeafForCosigner
	for _, vtxoReq := range clientInfo.VTXOReqs {
		path := batchOutput.Tree.ExtractPathForCosigner(vtxoReq.SigningKey)
		if path == nil {
			return nil, fmt.Errorf("failed to find path for VTXO signing key")
		}
		vtxoPaths = append(vtxoPaths, path)
	}

	return &ClientVTXOInfo{
		BatchOutput: batchOutput,
		VTXOPaths:   vtxoPaths,
	}, nil
}

// buildClientConnectorInfo builds connector-specific information for a client
func (b *Batch) buildClientConnectorInfo(clientInfo *ClientRequestInfo) (*ClientConnectorInfo, error) {
	if len(b.connectorOutputs) == 0 {
		return nil, fmt.Errorf("no connector outputs available")
	}

	if len(b.connectorOutputs) != 1 {
		return nil, fmt.Errorf("expected exactly one connector output, got %d", len(b.connectorOutputs))
	}

	connectorOutput := b.connectorOutputs[0]
	connectorPaths := make([]*Tree, 0, len(clientInfo.ForfeitReqs))

	// Find the index of each forfeit request and extract path by index
	// We need to find which index this client's forfeit requests are at
	// Use deterministic ordering to ensure consistent connector assignment
	globalForfeitIndex := 0
	orderedClientIDs := b.getOrderedClientIDs()

	for _, clientID := range orderedClientIDs {
		clientReq := b.clientRequests[clientID]
		if clientReq.ClientID == clientInfo.ClientID {
			// Found our client, extract paths for their forfeit requests
			for range clientInfo.ForfeitReqs {
				path, err := connectorOutput.Tree.ExtractPathForIndex(globalForfeitIndex)
				if err != nil {
					return nil, fmt.Errorf("failed to extract path for forfeit index %d: %w", globalForfeitIndex, err)
				}
				if path == nil {
					return nil, fmt.Errorf("no path found for forfeit index %d", globalForfeitIndex)
				}
				connectorPaths = append(connectorPaths, path)
				globalForfeitIndex++
			}
			break
		} else {
			// Count forfeit requests from other clients
			globalForfeitIndex += len(clientReq.ForfeitReqs)
		}
	}

	return &ClientConnectorInfo{
		ConnectorOutput: connectorOutput,
		ConnectorPaths:  connectorPaths,
	}, nil
}

// getOrderedClientIDs returns client IDs in deterministic sorted order
func (b *Batch) getOrderedClientIDs() []string {
	clientIDs := make([]string, 0, len(b.clientRequests))
	for clientID := range b.clientRequests {
		clientIDs = append(clientIDs, clientID)
	}

	// Sort to ensure deterministic ordering
	sort.Strings(clientIDs)

	return clientIDs
}

func (b *Batch) RegisterNonces(signer *btcec.PublicKey,
	nonces map[string]Musig2PubNonce) (bool, error) {

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state != batchStateNonceCollection {
		return false, fmt.Errorf("batch not in nonce collection state")
	}

	err := b.batchSigningCoordinator.AddNonces(signer, nonces)
	if err != nil {
		return false, fmt.Errorf("failed to add nonces: %w", err)
	}

	if !b.batchSigningCoordinator.HasAllNonces() {
		return false, nil
	}

	allNonces, err := b.batchSigningCoordinator.GetAllNonces()
	if err != nil {
		return false, err
	}

	err = b.treeSigningSession.RegisterNonces(allNonces)
	if err != nil {
		return false, fmt.Errorf("failed to register nonces: %w", err)
	}

	b.state = batchStateSigCollection

	return true, nil
}

func (b *Batch) GetAggNonce() (map[string][]Musig2PubNonce, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state != batchStateSigCollection {
		return nil, fmt.Errorf("batch not in sig collection phase")
	}

	// The operator can add its signatures for the tree.
	sigs, err := b.treeSigningSession.Signatures()
	if err != nil {
		return nil, fmt.Errorf("batch tree signing failed: %w", err)
	}
	err = b.batchSigningCoordinator.AddPartialSignatures(
		b.cfg.BatchKey.PubKey, sigs,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to add batch partial "+
			"sigs: %w", err)
	}

	return b.batchSigningCoordinator.GetAllNonces()
}

func (b *Batch) AddPartialSignatures(signer *btcec.PublicKey,
	sigs map[string]*musig2.PartialSignature) error {

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state != batchStateSigCollection {
		return fmt.Errorf("batch not in signature collection state")
	}

	err := b.batchSigningCoordinator.AddPartialSignatures(signer, sigs)
	if err != nil {
		return fmt.Errorf("failed to add partial signatures: %w", err)
	}

	if !b.batchSigningCoordinator.FullySigned() {
		return nil
	}

	if len(b.getAllBoardingInputs()) != 0 {
		b.state = batchStateInputSigCollection
	} else {
		b.state = batchStateSignInputs
	}

	return nil
}

func (b *Batch) GetTreeSigs() (map[string]*schnorr.Signature, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !(b.state == batchStateSignInputs ||
		b.state == batchStateInputSigCollection) {

		return nil, fmt.Errorf("batch not in input signing or " +
			"input sig collection phase")
	}

	return b.batchSigningCoordinator.Signatures()
}

func (b *Batch) AddBoardingSignatures(sigs []*BoardingInputSignature) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state != batchStateInputSigCollection {
		return fmt.Errorf("batch not in sealed state")
	}

	for _, sig := range sigs {
		// Find the corresponding boarding input
		boardingInput := b.findBoardingInput(sig.Outpoint)
		if boardingInput == nil {
			return fmt.Errorf("boarding input not found for "+
				"outpoint %v", sig.Outpoint)
		}

		// Verify the signature
		err := b.verifyBoardingSignature(
			sig, boardingInput,
		)
		if err != nil {
			return fmt.Errorf("signature verification failed for "+
				"input %d: %w", sig.InputIndex, err)
		}

		// Collect the boarding signature.
		b.boardingSigs[sig.Outpoint] = sig
	}

	if len(b.boardingSigs) == len(b.getAllBoardingInputs()) {
		b.state = batchStateSignInputs
	}

	return nil
}

// SubmitSignedForfeits receives client-signed forfeit transactions and validates/completes them
func (b *Batch) SubmitSignedForfeits(clientID string, forfeitTxSigs []*ForfeitTxSig) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(forfeitTxSigs) == 0 {
		return nil
	}

	// Get server's forfeit address for validation
	forfeitAddr, err := b.cfg.Wallet.GetForfeitAddress()
	if err != nil {
		return fmt.Errorf("failed to get server forfeit address: %w", err)
	}

	forfeitScript, err := txscript.PayToAddrScript(forfeitAddr)
	if err != nil {
		return fmt.Errorf("failed to create forfeit script: %w", err)
	}

	// Find the client's request info to validate connector assignments
	clientInfo, exists := b.clientRequests[clientID]
	if !exists {
		return fmt.Errorf("client ID %s not found in batch", clientID)
	}

	// Get the client's allocated connector paths for validation
	clientConnectorInfo, err := b.buildClientConnectorInfo(clientInfo)
	if err != nil {
		return fmt.Errorf("failed to build client connector info: %w", err)
	}

	// Validate and complete each forfeit transaction
	for i, forfeitTxSig := range forfeitTxSigs {
		if err := b.validateAndCompleteForfeitTxSig(forfeitTxSig, forfeitScript, clientConnectorInfo, i); err != nil {
			return fmt.Errorf("failed to validate forfeit tx %d from client %s: %w", i, clientID, err)
		}
	}

	// TODO: Store completed forfeit transactions for potential broadcast

	return nil
}

// validateAndCompleteForfeitTxSig validates a forfeit tx sig and constructs the complete transaction
func (b *Batch) validateAndCompleteForfeitTxSig(forfeitTxSig *ForfeitTxSig, expectedForfeitScript []byte, clientConnectorInfo *ClientConnectorInfo, forfeitIndex int) error {
	tx := forfeitTxSig.UnsignedTx

	// Validate basic structure
	if len(tx.TxIn) != 2 {
		return fmt.Errorf("forfeit tx must have exactly 2 inputs, got %d", len(tx.TxIn))
	}
	if len(tx.TxOut) != 2 {
		return fmt.Errorf("forfeit tx must have exactly 2 outputs, got %d", len(tx.TxOut))
	}
	if tx.Version != 3 {
		return fmt.Errorf("forfeit tx must be version 3, got %d", tx.Version)
	}

	// Validate the output goes to server's forfeit address
	if !bytes.Equal(tx.TxOut[0].PkScript, expectedForfeitScript) {
		return fmt.Errorf("forfeit output does not pay to server's forfeit address")
	}

	// Find the VTXO being forfeited (should match one of our forfeit requests)
	vtxoInput := tx.TxIn[0] // VTXO is first input
	vtxoKey := vtxoInput.PreviousOutPoint.String()

	var vtxoFound bool
	for _, clientReq := range b.clientRequests {
		for _, forfeitReq := range clientReq.ForfeitReqs {
			if forfeitReq.VTXOOutpoint.String() == vtxoKey {
				vtxoFound = true
				break
			}
		}
		if vtxoFound {
			break
		}
	}

	if !vtxoFound {
		return fmt.Errorf("VTXO %s not found in any forfeit requests", vtxoKey)
	}

	// Validate the connector input matches this client's allocated connector for this forfeit index
	connectorInput := tx.TxIn[1] // Connector is second input
	connectorKey := connectorInput.PreviousOutPoint.String()

	if forfeitIndex >= len(clientConnectorInfo.ConnectorPaths) {
		return fmt.Errorf("forfeit index %d exceeds client's allocated connectors (%d)", forfeitIndex, len(clientConnectorInfo.ConnectorPaths))
	}

	// Get the specific connector path allocated to this client for this forfeit
	clientConnectorPath := clientConnectorInfo.ConnectorPaths[forfeitIndex]

	// Validate that the connector input matches the client's allocated connector
	leafNodes, err := clientConnectorPath.GetLeafNodes()
	if err != nil {
		return fmt.Errorf("failed to get leaf nodes from client connector path: %w", err)
	}

	var connectorFound bool
	for _, leaf := range leafNodes {
		if outpoint, err := leaf.GetNonAnchorOutpoint(); err == nil {
			if outpoint.String() == connectorKey {
				connectorFound = true
				break
			}
		}
	}

	if !connectorFound {
		return fmt.Errorf("connector %s not found in client's allocated connector paths (forfeit index %d)", connectorKey, forfeitIndex)
	}

	// Construct the VTXO witness with both client and server signatures
	// The witness format is: [server_sig] [client_sig] [script] [control_block]
	err = b.completeVTXOWitness(tx, 0, forfeitTxSig)
	if err != nil {
		return fmt.Errorf("failed to complete VTXO witness: %w", err)
	}

	// Sign the connector input
	err = b.signConnectorInput(tx, 1) // Connector is second input
	if err != nil {
		return fmt.Errorf("failed to sign connector input: %w", err)
	}

	// TODO: Store the completed transaction for potential broadcast

	return nil
}

// completeVTXOWitness constructs the witness for the VTXO input with both client and server signatures
func (b *Batch) completeVTXOWitness(tx *wire.MsgTx, inputIndex int, forfeitTxSig *ForfeitTxSig) error {
	// TODO: In a full implementation, this would:
	// 1. Create server signature using proper prevout information
	// 2. Construct witness with both signatures
	// For now, we'll create a placeholder witness to satisfy the structure

	// Create a dummy server signature for testing
	// In production, this would be a real signature using the server's key
	dummyServerSig := make([]byte, 64) // schnorr signature is 64 bytes

	// Note: This method is deprecated. Witness reconstruction is now handled
	// by the server using the VTXO store in completeVTXOWitnessFromStore.
	// This is kept as a placeholder for backward compatibility.
	witness := wire.TxWitness{
		dummyServerSig,
		forfeitTxSig.ClientVTXOSig.Serialize(),
		[]byte{}, // placeholder - actual script reconstructed by server
		[]byte{}, // placeholder - actual control block reconstructed by server
	}

	tx.TxIn[inputIndex].Witness = witness
	return nil
}

// signConnectorInput signs the connector input with the server key
func (b *Batch) signConnectorInput(tx *wire.MsgTx, inputIndex int) error {
	// TODO: Implement connector input signing
	// For now, this is a placeholder
	return nil
}

// GetAllVTXORequests returns all VTXO requests in this batch
func (b *Batch) GetAllVTXORequests() []*VTXORequest {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.getAllVTXORequests()
}

// GetBatchOutputs returns all batch outputs containing VTXO trees
func (b *Batch) GetBatchOutputs() []*BatchOutputInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.batchOutputs
}

// GetAllForfeitRequests returns all forfeit requests in this batch
func (b *Batch) GetAllForfeitRequests() []*ForfeitRequest {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.getAllForfeitRequests()
}

func (b *Batch) SignInputs() (*wire.MsgTx, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state != batchStateSignInputs {
		return nil, fmt.Errorf("batch not in input signing state")
	}

	// Track which inputs are boarding inputs
	boardingInputIndices := make(map[int]bool)

	// Sign boarding inputs first
	boardingReqs := b.getAllBoardingInputs()
	for _, boardingInput := range boardingReqs {
		clientSig, ok := b.boardingSigs[*boardingInput.Outpoint]
		if !ok {
			return nil, fmt.Errorf("missing client signature "+
				"for boarding input %v", boardingInput.Outpoint)
		}

		boardingInputIndices[clientSig.InputIndex] = true

		spendInfo, err := NewBoardingCollabSpendInfo(
			boardingInput.Tapscript,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to build collab spend "+
				"info: %w", err)
		}

		serverSig, err := SignBoardingCollabInput(
			b.cfg.Wallet,
			b.tx,
			clientSig.InputIndex,
			spendInfo,
			boardingInput.OperatorKeyDesc,
			boardingInput.Value,
			boardingInput.PkScript,
			b.sigHashes,
			txscript.NewMultiPrevOutFetcher(b.prevOuts),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create server "+
				"tapscript signature: %w", err)
		}

		// Build the witness stack for collaborative spend.
		witness, err := BoardingCollabWitness(
			serverSig, clientSig.ClientSignature, spendInfo,
		)
		if err != nil {
			return nil, err
		}

		// Set the witness.
		b.tx.TxIn[clientSig.InputIndex].Witness = witness
	}

	// Now sign any additional wallet inputs (non-boarding inputs)
	for i, txIn := range b.tx.TxIn {
		// Skip boarding inputs, they're already signed
		if boardingInputIndices[i] {
			continue
		}

		prevOut, ok := b.prevOuts[txIn.PreviousOutPoint]
		if !ok {
			return nil, fmt.Errorf("missing prev output for input %d: %v",
				i, txIn.PreviousOutPoint)
		}

		// Create prev output fetcher for this input
		prevOutFetcher := txscript.NewCannedPrevOutputFetcher(
			prevOut.PkScript, int64(prevOut.Value),
		)

		// Create sign descriptor for wallet input
		signDesc := &input.SignDescriptor{
			Output:            prevOut,
			InputIndex:        i,
			SigHashes:         b.sigHashes,
			PrevOutputFetcher: prevOutFetcher,
			HashType:          txscript.SigHashAll,
		}

		// Adjust hash type for taproot inputs
		if txscript.IsPayToTaproot(prevOut.PkScript) {
			signDesc.HashType = txscript.SigHashDefault
		}

		// Sign using the signer's ComputeInputScript method
		inputScript, err := b.cfg.Wallet.ComputeInputScript(
			b.tx, signDesc,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to sign wallet input %d: %w",
				i, err)
		}

		// Set the signature and witness
		txIn.SignatureScript = inputScript.SigScript
		txIn.Witness = inputScript.Witness
	}

	b.state = batchStateComplete

	return b.tx, nil
}

// verifyBoardingSignature verifies a single boarding input signature
// Note: This is called from AddBoardingSignatures which already holds the lock
func (b *Batch) verifyBoardingSignature(sig *BoardingInputSignature,
	boardingInput *BoardingInput) error {

	// Get the collaborative script (first leaf).
	if len(boardingInput.Tapscript.Leaves) == 0 {
		return fmt.Errorf("no tap leaves found in boarding script")
	}

	collabScriptTapLeaf := boardingInput.Tapscript.Leaves[0]

	// Create prev output fetcher for this input
	prevOut := &wire.TxOut{
		Value:    int64(boardingInput.Value),
		PkScript: boardingInput.PkScript,
	}
	prevOutFetcher := txscript.NewCannedPrevOutputFetcher(prevOut.PkScript, prevOut.Value)

	// Create the signature hash for verification
	sigHash, err := txscript.CalcTapscriptSignaturehash(
		b.sigHashes,
		txscript.SigHashDefault,
		b.tx,
		sig.InputIndex,
		prevOutFetcher,
		collabScriptTapLeaf,
	)
	if err != nil {
		return fmt.Errorf("failed to calculate tapscript signature hash: %w", err)
	}

	// Verify the schnorr signature
	if !sig.ClientSignature.Verify(sigHash, boardingInput.ClientKey) {
		return fmt.Errorf("invalid schnorr signature")
	}

	return nil
}

// findBoardingInput finds a boarding input by outpoint
// Note: This is called from AddBoardingSignatures which already holds the lock
func (b *Batch) findBoardingInput(
	outpoint wire.OutPoint) *BoardingInput {

	boardingReqs := b.getAllBoardingInputs()
	for _, boardingInput := range boardingReqs {
		if !boardingInput.Outpoint.Hash.IsEqual(&outpoint.Hash) {
			continue
		}

		if boardingInput.Outpoint.Index == outpoint.Index {
			return boardingInput
		}
	}
	return nil
}

func (b *Batch) buildCommitmentTx() (*wire.MsgTx, []*BatchOutputInfo,
	[]*ConnectorOutputInfo, error) {

	// Compile our list of required outputs based on the leave requests
	// and possible batch outputs.
	var (
		baseWeightEstimator input.TxWeightEstimator
		leaveReqs           = b.getAllLeaveRequests()
		requiredOutputs     = make(
			[]*wire.TxOut, 0, len(leaveReqs),
		)
		requiredOutputTotal btcutil.Amount
	)
	for _, leaveReq := range leaveReqs {
		requiredOutputs = append(requiredOutputs, leaveReq.Output)
		requiredOutputTotal += btcutil.Amount(leaveReq.Output.Value)

		// Add output weight to base estimator
		addOutputWeight(&baseWeightEstimator, leaveReq.Output.PkScript)
	}

	// If there are vtxos, we add a batch output. For now, we just add
	// a single batch output. In the future, we may want to consider
	// supporting multiple batch outputs.
	var batchOutputs []*BatchOutputInfo
	vtxoReqs := b.getAllVTXORequests()
	if len(vtxoReqs) > 0 {
		// Using our vtxo requests, we can compute the batch output
		// script.
		batchOutput, err := BuildBatchOutput(
			VTXOLeavesFromRequests(vtxoReqs),
			b.cfg.BatchKey.PubKey,
			b.cfg.SweepKey.PubKey,
			b.cfg.SweepDelay,
		)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to build VTXT: %w",
				err)
		}

		// Add the batch output to the required set of outputs and
		// update the weight estimator.
		batchIndex := len(requiredOutputs)
		requiredOutputs = append(requiredOutputs, batchOutput)
		requiredOutputTotal += btcutil.Amount(batchOutput.Value)
		addOutputWeight(&baseWeightEstimator, batchOutput.PkScript)

		batchOutputs = append(batchOutputs, &BatchOutputInfo{
			Idx:       batchIndex,
			SignerKey: b.cfg.BatchKey.PubKey,
		})
	}

	// If there are forfeit requests, we build a connector output.
	// For now we just build one.
	var connectorOutputs []*ConnectorOutputInfo
	forfeitReqs := b.getAllForfeitRequests()
	if len(forfeitReqs) > 0 {
		connectorAddr, err := b.cfg.Wallet.NewTaprootAddress()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to get new "+
				"connector address: %w", err)
		}

		connectorOutput, err := BuildConnectorOutput(
			len(forfeitReqs), b.cfg.DustLimit, connectorAddr,
		)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to build "+
				"connector output: %w", err)
		}

		connectorIndex := len(requiredOutputs)
		requiredOutputs = append(requiredOutputs, connectorOutput)
		requiredOutputTotal += btcutil.Amount(connectorOutput.Value)
		addOutputWeight(&baseWeightEstimator, connectorOutput.PkScript)

		taprootKey, err := schnorr.ParsePubKey(
			connectorAddr.ScriptAddress(),
		)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to parse "+
				"taproot key: %w", err)
		}

		connectorOutputs = append(connectorOutputs, &ConnectorOutputInfo{
			Idx:          connectorIndex,
			ConnectorKey: taprootKey,
			NumLeaves:    len(forfeitReqs),
		})
	}

	// Perform iterative coin selection with taproot script fee estimation.
	boardingReqs := b.getAllBoardingInputs()
	tx, selectedUTXOs, _, err := b.buildTransactionWithFees(
		boardingReqs,
		requiredOutputs,
		requiredOutputTotal,
		baseWeightEstimator,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("transaction building failed: %w",
			err)
	}

	// Store the transaction in our state.
	b.tx = tx

	// Now that we have the commitment transaction, we can construct the
	// VTX tree.
	for _, batchOutput := range batchOutputs {
		initialOutpoint := &wire.OutPoint{
			Hash:  tx.TxHash(),
			Index: uint32(batchOutput.Idx),
		}

		// Get the prevOut from the transaction output
		prevOut := tx.TxOut[batchOutput.Idx]

		vtxoTree, err := BuildVTXOTree(
			initialOutpoint,
			LeavesFromVTXOReqs(vtxoReqs),
			b.cfg.SweepDelay,
			b.cfg.SweepKey.PubKey,
			b.cfg.BatchKey.PubKey,
			b.cfg.VTXOTreeRadix,
			prevOut,
		)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to build vtxo "+
				"tree: %w", err)
		}

		batchOutput.Tree = vtxoTree
	}

	// We can also now build the connector tree(s).
	for _, connectorOutput := range connectorOutputs {
		// Get the prevOut from the transaction output
		prevOut := tx.TxOut[connectorOutput.Idx]

		connectorTree, err := BuildConnectorTree(
			&wire.OutPoint{
				Hash:  tx.TxHash(),
				Index: uint32(connectorOutput.Idx),
			},
			connectorOutput.NumLeaves,
			b.cfg.DustLimit,
			connectorOutput.ConnectorKey,
			b.cfg.ConnectorTreeRadix,
			prevOut,
		)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to build "+
				"connector tree: %w", err)
		}

		connectorOutput.Tree = connectorTree
	}

	// Add boarding inputs to prevOuts
	for _, boardingInput := range boardingReqs {
		b.prevOuts[*boardingInput.Outpoint] = &wire.TxOut{
			Value:    int64(boardingInput.Value),
			PkScript: boardingInput.PkScript,
		}
	}

	// Add selected wallet UTXOs to prevOuts
	for _, utxo := range selectedUTXOs {
		b.prevOuts[utxo.OutPoint] = &wire.TxOut{
			Value:    int64(utxo.Value),
			PkScript: utxo.PkScript,
		}
	}

	prevOutFetcher := txscript.NewMultiPrevOutFetcher(b.prevOuts)

	// Create signature hashes.
	b.sigHashes = txscript.NewTxSigHashes(tx, prevOutFetcher)

	return tx, batchOutputs, connectorOutputs, nil
}

// buildTransactionWithFees builds the transaction with iterative fee estimation
// This method orchestrates the process using the focused helper functions
// Note: This is called from BuildCommitmentTx which already holds the lock
func (b *Batch) buildTransactionWithFees(
	boardingInputs []*BoardingInput,
	requiredOutputs []*wire.TxOut,
	requiredOutputTotal btcutil.Amount,
	baseWeightEstimator input.TxWeightEstimator,
) (*wire.MsgTx, []*lnwallet.Utxo, btcutil.Amount, error) {

	// Get fee rate estimate.
	feeRate, err := b.cfg.FeeEstimator.EstimateFeePerKW(
		b.cfg.TargetConf,
	)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("fee estimation failed: %w", err)
	}

	amountNeeded := requiredOutputTotal
	var finalSelectedUTXOs []*lnwallet.Utxo

	// 1. Build basic transaction template (pure function, no wallet calls).
	tx, boardingInputTotal := buildTransactionTemplate(
		boardingInputs, requiredOutputs,
	)

	for {
		// 2. Populate additional wallet inputs if needed (isolated
		// wallet calls).
		additionalInputs, totalInputAmt, err := b.populateWalletInputs(
			tx, boardingInputTotal, amountNeeded,
		)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("failed to populate "+
				"wallet inputs: %w", err)
		}

		// Store the selected UTXOs for this iteration.
		finalSelectedUTXOs = additionalInputs

		// 3. Calculate transaction weights and fees.
		feeNoChange, feeWithChange, err := calculateTaprootFees(
			boardingInputs, additionalInputs, feeRate,
			baseWeightEstimator,
		)
		if err != nil {
			return nil, nil, 0, err
		}

		// 4. Determine if we need change and calculate amounts.
		changeAmount, newAmountNeeded := calculateChangeAmount(
			b.cfg.DustLimit, totalInputAmt, requiredOutputTotal,
			feeNoChange, feeWithChange,
		)

		// 5. Check if we need another iteration.
		if newAmountNeeded > 0 {
			amountNeeded = newAmountNeeded
			continue
		}

		// 6. Add change output if needed (isolated wallet call).
		if changeAmount > b.cfg.DustLimit {
			changeOutput, err := b.createChangeOutput(changeAmount)
			if err != nil {
				return nil, nil, 0, fmt.Errorf("failed to "+
					"create change output: %w", err)

			}
			tx.AddTxOut(changeOutput)
		}

		// 7. Calculate final fee.
		finalFee := totalInputAmt - requiredOutputTotal - changeAmount

		return tx, finalSelectedUTXOs, finalFee, nil
	}
}

// createChangeOutput creates a taproot change output
// Note: This is called from buildTransactionWithFees which is called from BuildCommitmentTx which holds the lock
func (b *Batch) createChangeOutput(amount btcutil.Amount) (*wire.TxOut, error) {
	changeAddr, err := b.cfg.Wallet.NewAddress()
	if err != nil {
		return nil, err
	}

	changeScript, err := txscript.PayToAddrScript(changeAddr)
	if err != nil {
		return nil, err
	}

	return &wire.TxOut{
		Value:    int64(amount),
		PkScript: changeScript,
	}, nil
}

// populateWalletInputs adds additional wallet UTXOs if needed to meet the amount requirement
// Note: This is called from buildTransactionWithFees which is called from BuildCommitmentTx which holds the lock
func (b *Batch) populateWalletInputs(
	tx *wire.MsgTx,
	boardingInputTotal btcutil.Amount,
	amountNeeded btcutil.Amount,
) ([]*lnwallet.Utxo, btcutil.Amount, error) {

	var additionalInputs []*lnwallet.Utxo
	totalInputAmount := boardingInputTotal

	// Check if we need additional UTXOs beyond boarding inputs
	if amountNeeded > boardingInputTotal {
		shortfall := amountNeeded - boardingInputTotal

		// Get additional UTXOs from wallet
		availableUTXOs, err := b.cfg.Wallet.ListAvailableUTXOs()
		if err != nil {
			return nil, 0, fmt.Errorf("failed to list UTXOs: %w", err)
		}

		selectedUTXOs, _, err := selectAdditionalInputs(shortfall, availableUTXOs)
		if err != nil {
			return nil, 0, fmt.Errorf("insufficient funds: %w", err)
		}

		// Add selected UTXOs as inputs to the transaction
		for _, utxo := range selectedUTXOs {
			tx.AddTxIn(&wire.TxIn{
				PreviousOutPoint: utxo.OutPoint,
				Sequence:         wire.MaxTxInSequenceNum,
			})
		}

		additionalInputs = selectedUTXOs
		for _, utxo := range additionalInputs {
			totalInputAmount += utxo.Value
		}
	}

	return additionalInputs, totalInputAmount, nil
}

// calculateTaprootFees estimates fees for boarding inputs (taproot script) + additional inputs
// This is a standalone function with no operator dependencies
func calculateTaprootFees(
	boardingInputs []*BoardingInput,
	additionalInputs []*lnwallet.Utxo,
	feeRate chainfee.SatPerKWeight,
	baseEstimator input.TxWeightEstimator,
) (btcutil.Amount, btcutil.Amount, error) {

	estimator := baseEstimator

	// Add boarding input weights (taproot script spend)
	for _, boardingInput := range boardingInputs {
		addBoardingCollabInputWeight(&estimator, boardingInput)
	}

	// Add additional UTXO input weights
	for _, utxo := range additionalInputs {
		addInputWeight(&estimator, utxo.PkScript)
	}

	// Calculate fee without change
	weightNoChange := estimator.Weight()
	feeNoChange := feeRate.FeeForWeight(weightNoChange)

	// Add change output weight and calculate fee with change.
	estimator.AddP2TROutput()
	weightWithChange := estimator.Weight()
	feeWithChange := feeRate.FeeForWeight(weightWithChange)

	return feeNoChange, feeWithChange, nil
}

// buildTransactionTemplate creates the basic transaction structure with boarding inputs and required outputs
// This is a pure function that doesn't make wallet calls
func buildTransactionTemplate(
	boardingInputs []*BoardingInput,
	requiredOutputs []*wire.TxOut,
) (*wire.MsgTx, btcutil.Amount) {

	tx := wire.NewMsgTx(2)

	// Add boarding inputs (taproot script spend)
	var boardingInputTotal btcutil.Amount
	for _, boardingInput := range boardingInputs {
		boardingInputTotal += boardingInput.Value
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *boardingInput.Outpoint,
			SignatureScript:  nil, // Empty for taproot
			Sequence:         wire.MaxTxInSequenceNum,
		})
	}

	// Add required outputs
	for _, output := range requiredOutputs {
		tx.AddTxOut(output)
	}

	return tx, boardingInputTotal
}

// selectAdditionalInputs selects UTXOs to cover the shortfall (simple largest-first).
func selectAdditionalInputs(target btcutil.Amount,
	utxos []*lnwallet.Utxo) ([]*lnwallet.Utxo, btcutil.Amount, error) {

	// Sort UTXOs by value (largest first)
	sortedUTXOs := make([]*lnwallet.Utxo, len(utxos))
	copy(sortedUTXOs, utxos)

	// Simple sort by value descending
	for i := 0; i < len(sortedUTXOs)-1; i++ {
		for j := i + 1; j < len(sortedUTXOs); j++ {
			if sortedUTXOs[i].Value > sortedUTXOs[j].Value {
				continue
			}

			sortedUTXOs[i], sortedUTXOs[j] = sortedUTXOs[j],
				sortedUTXOs[i]
		}
	}

	var selected []*lnwallet.Utxo
	var total btcutil.Amount

	for _, utxo := range sortedUTXOs {
		selected = append(selected, utxo)
		total += utxo.Value

		if total >= target {
			break
		}
	}

	if total < target {
		return nil, 0, fmt.Errorf("insufficient funds: need %v, "+
			"have %v", target, total)
	}

	return selected, total, nil
}

// addBoardingCollabInputWeight adds weight for boarding input (taproot
// script spend).
func addBoardingCollabInputWeight(estimator *input.TxWeightEstimator,
	boardingInput *BoardingInput) {

	estimator.AddTapscriptInput(
		CollabMultisigLeafWitnessSize, boardingInput.Tapscript,
	)
}

// addInputWeight adds weight for regular wallet UTXO inputs
func addInputWeight(estimator *input.TxWeightEstimator, pkScript []byte) {
	switch {
	case txscript.IsPayToWitnessPubKeyHash(pkScript):
		estimator.AddP2WKHInput()
	case txscript.IsPayToScriptHash(pkScript):
		estimator.AddNestedP2WKHInput()
	case txscript.IsPayToTaproot(pkScript):
		estimator.AddTaprootKeySpendInput(txscript.SigHashDefault)
	default:
		// Fallback to P2WPKH
		estimator.AddP2WKHInput()
	}
}

// addOutputWeight adds weight for outputs based on script type
func addOutputWeight(estimator *input.TxWeightEstimator, pkScript []byte) {
	switch {
	case txscript.IsPayToWitnessPubKeyHash(pkScript):
		estimator.AddP2WKHOutput()
	case txscript.IsPayToWitnessScriptHash(pkScript):
		estimator.AddP2WSHOutput()
	case txscript.IsPayToTaproot(pkScript):
		estimator.AddP2TROutput()
	default:
		estimator.AddP2WKHOutput()
	}
}

// calculateChangeAmount determines change amount and if more inputs are needed
func calculateChangeAmount(dustLimit, totalInputValue, requiredAmount,
	feeNoChange, feeWithChange btcutil.Amount) (
	changeAmount btcutil.Amount, newAmountNeeded btcutil.Amount) {

	overshoot := totalInputValue - requiredAmount

	switch {
	// Not enough to cover fees without change - need more inputs
	case overshoot < feeNoChange:
		return 0, requiredAmount + feeNoChange

	// Can afford change output and it's above dust limit
	case overshoot > feeWithChange && (overshoot-feeWithChange) > dustLimit:
		return overshoot - feeWithChange, 0

	// Use fee without change (no change output)
	default:
		return 0, 0
	}
}
