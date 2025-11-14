package lib

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/ark/lib/stores"
	"github.com/lightninglabs/ark/lib/types"
	"github.com/lightninglabs/ark/lib/wallets"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

type Chain interface {
	GetUTXO(op *wire.OutPoint) (*wire.TxOut, bool, int64, error)
}

type OperatorConfig struct {
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

	// RequiredBoardingInputConfs is the minimum number of confirmations
	// required for boarding input utxos.
	RequiredBoardingInputConfs int64

	MinBoardingAmount btcutil.Amount

	DustLimit  btcutil.Amount
	TargetConf uint32

	Wallet       wallets.OperatorWallet
	FeeEstimator chainfee.Estimator
	Chain        Chain
}

type Operator struct {
	cfg *OperatorConfig

	mainKey   *keychain.KeyDescriptor
	vtxoStore stores.VTXOStore

	// Batch management
	mu      sync.RWMutex
	batches map[string]BatchBuilder // key: batch UUID
}

// NewOperator creates a new ark operator instance.
func NewOperator(cfg *OperatorConfig) (*Operator, error) {
	pubKey, err := cfg.Wallet.MainOperatorKey()
	if err != nil {
		return nil, err
	}

	return &Operator{
		cfg:       cfg,
		mainKey:   pubKey,
		vtxoStore: stores.NewInMemoryVTXOStore(),
		batches:   make(map[string]BatchBuilder),
	}, nil
}

// Terms returns the various terms of the operator that the client must
// obey when making requests.
func (o *Operator) Terms() (*OperatorTerms, error) {
	return &OperatorTerms{
		PubKey:            o.mainKey.PubKey,
		BoardingExitDelay: o.cfg.BoardingExitDelay,
		VTXOExitDelay:     o.cfg.VTXOExitDelay,
	}, nil
}

// StartNewBatch creates and activates a new batch instance, returns the batch UUID.
func (o *Operator) StartNewBatch() (string, error) {
	sweepKey, err := o.cfg.Wallet.NewSweepKey()
	if err != nil {
		return "", err
	}

	batchSigningKey, err := o.cfg.Wallet.NewBatchSignerKey()
	if err != nil {
		return "", err
	}

	batch, err := NewBatch(&BatchConfig{
		BatchKey:           batchSigningKey,
		OperatorMainKey:    o.mainKey,
		SweepKey:           sweepKey,
		VTXOTreeRadix:      o.cfg.VTXOTreeRadix,
		ConnectorTreeRadix: o.cfg.ConnectorTreeRadix,
		BoardingExitDelay:  o.cfg.BoardingExitDelay,
		VTXOExitDelay:      o.cfg.VTXOExitDelay,
		SweepDelay:         o.cfg.SweepDelay,
		DustLimit:          o.cfg.DustLimit,
		TargetConf:         o.cfg.TargetConf,
		Wallet:             o.cfg.Wallet,
		FeeEstimator:       o.cfg.FeeEstimator,
	})
	if err != nil {
		return "", err
	}

	// Store the batch and return its UUID
	o.mu.Lock()
	defer o.mu.Unlock()

	batchUUID := batch.UUID()
	o.batches[batchUUID] = batch
	return batchUUID, nil
}

// getBatch retrieves a batch by UUID (internal helper)
func (o *Operator) getBatch(batchUUID string) (BatchBuilder, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	batch, exists := o.batches[batchUUID]
	if !exists {
		return nil, fmt.Errorf("batch not found: %s", batchUUID)
	}

	return batch, nil
}

// RegisterRequests delegates to the specified batch
func (o *Operator) RegisterRequests(batchUUID string,
	req *ParticipantRoundRequest) (string, error) {

	batch, err := o.getBatch(batchUUID)
	if err != nil {
		return "", err
	}

	// TODO: validate all requests before registering.

	// Create a boarding validator with operator configuration
	boardingReqValidator := NewBoardingValidator(
		o.mainKey,
		o.cfg.BoardingExitDelay,
		o.cfg.MinBoardingAmount,
		o.cfg.RequiredBoardingInputConfs,
		o.cfg.Chain.GetUTXO,
	)

	// Validate the boarding requests and then convert them to BoardingInputs
	boardingInputs := make([]*BoardingInput, len(req.BoardingReqs))
	for i, boardingReq := range req.BoardingReqs {
		// TODO: validate that the client key has signed this request.

		boardingInput, err := boardingReqValidator.ValidateRequest(
			boardingReq,
		)
		if err != nil {
			return "", fmt.Errorf("boarding request %d: %w", i, err)
		}
		boardingInputs[i] = boardingInput
	}

	return batch.RegisterRequests(
		boardingInputs,
		req.LeaveReqs,
		req.VTXOReqs,
		req.ForfeitReqs,
	)
}

// SealBatch delegates to the specified batch
func (o *Operator) SealBatch(batchUUID string) error {
	batch, err := o.getBatch(batchUUID)
	if err != nil {
		return err
	}

	return batch.SealBatch()
}

// GetClientBatchInfo delegates to the specified batch
func (o *Operator) GetClientBatchInfo(batchUUID string, clientRequestID string) (*ClientBatchInfo, error) {
	batch, err := o.getBatch(batchUUID)
	if err != nil {
		return nil, err
	}

	return batch.GetClientBatchInfo(clientRequestID)
}

// RegisterNonces delegates to the specified batch
func (o *Operator) RegisterNonces(batchUUID string, signer *btcec.PublicKey,
	nonces map[string]Musig2PubNonce) (bool, error) {

	batch, err := o.getBatch(batchUUID)
	if err != nil {
		return false, err
	}

	return batch.RegisterNonces(signer, nonces)
}

// GetAggNonce delegates to the specified batch
func (o *Operator) GetAggNonce(batchUUID string) (map[string][]Musig2PubNonce, error) {
	batch, err := o.getBatch(batchUUID)
	if err != nil {
		return nil, err
	}

	return batch.GetAggNonce()
}

// AddPartialSignatures delegates to the specified batch
func (o *Operator) AddPartialSignatures(batchUUID string, signer *btcec.PublicKey,
	sigs map[string]*musig2.PartialSignature) error {

	batch, err := o.getBatch(batchUUID)
	if err != nil {
		return err
	}

	return batch.AddPartialSignatures(signer, sigs)
}

// GetTreeSigs delegates to the specified batch
func (o *Operator) GetTreeSigs(batchUUID string) (map[string]*schnorr.Signature, error) {
	batch, err := o.getBatch(batchUUID)
	if err != nil {
		return nil, err
	}

	return batch.GetTreeSigs()
}

// AddBoardingSignatures delegates to the specified batch
func (o *Operator) AddBoardingSignatures(batchUUID string, sigs []*BoardingInputSignature) error {
	batch, err := o.getBatch(batchUUID)
	if err != nil {
		return err
	}

	return batch.AddBoardingSignatures(sigs)
}

// SubmitSignedForfeits delegates to the specified batch and handles VTXO removal
func (o *Operator) SubmitSignedForfeits(batchUUID string, clientID string, forfeitTxSigs []*ForfeitTxSig) error {
	batch, err := o.getBatch(batchUUID)
	if err != nil {
		return err
	}

	// First validate and process the forfeits with VTXO store access
	err = o.validateAndProcessForfeits(forfeitTxSigs)
	if err != nil {
		return err
	}

	// Store the forfeit transactions in the batch for later processing
	// The actual VTXO removal will happen after SignInputs completes
	err = batch.SubmitSignedForfeits(clientID, forfeitTxSigs)
	if err != nil {
		return err
	}

	// Note: VTXOs are NOT removed here anymore. They will be removed
	// after the batch is successfully signed in SignInputs

	return nil
}

// SignInputs delegates to the specified batch and handles VTXO addition
func (o *Operator) SignInputs(batchUUID string) (*wire.MsgTx, error) {
	batch, err := o.getBatch(batchUUID)
	if err != nil {
		return nil, err
	}

	// Sign the inputs
	signedTx, err := batch.SignInputs()
	if err != nil {
		return nil, err
	}

	// Extract newly created VTXOs from the batch tree structure and add them to vtxoStore
	vtxoReqs := batch.GetAllVTXORequests()
	if len(vtxoReqs) > 0 {
		// Get all batch outputs that contain VTXO trees
		batchOutputs := batch.GetBatchOutputs()

		var allVTXOOutpoints []*wire.OutPoint

		// Extract VTXO outpoints from each batch output's tree structure
		for i, batchOutput := range batchOutputs {
			if batchOutput.Tree == nil {
				continue
			}

			// Get all leaf nodes from the VTXO tree - each leaf represents a VTXO
			leafNodes, err := batchOutput.Tree.GetLeafNodes()
			if err != nil {
				return nil, fmt.Errorf("failed to get leaf nodes from VTXO tree %d: %w", i, err)
			}

			// Extract outpoint from each leaf node
			for j, leaf := range leafNodes {
				vtxoOutpoint, err := leaf.GetNonAnchorOutpoint()
				if err != nil {
					return nil, fmt.Errorf("failed to get VTXO outpoint from leaf node %d.%d: %w", i, j, err)
				}
				allVTXOOutpoints = append(allVTXOOutpoints, vtxoOutpoint)
			}
		}

		// Ensure we have the correct number of VTXO outpoints
		if len(allVTXOOutpoints) != len(vtxoReqs) {
			return nil, fmt.Errorf("VTXO count mismatch: found %d batch outputs with %d total outpoints, but have %d VTXO requests",
				len(batchOutputs), len(allVTXOOutpoints), len(vtxoReqs))
		}

		// Create ServerVTXO structs from VTXO requests and extracted outpoints
		if len(allVTXOOutpoints) > 0 {
			serverVTXOs, err := CreateServerVTXOs(vtxoReqs, allVTXOOutpoints, o.mainKey.PubKey)
			if err != nil {
				return nil, fmt.Errorf("failed to create server VTXOs: %w", err)
			}

			// Add VTXOs to the store
			err = o.vtxoStore.AddVTXOs(serverVTXOs)
			if err != nil {
				return nil, fmt.Errorf("failed to add VTXOs to store: %w", err)
			}
		}
	}

	// After successfully signing, remove any forfeited VTXOs from the store
	forfeitReqs := batch.GetAllForfeitRequests()
	if len(forfeitReqs) > 0 {
		var forfeitedOutpoints []*wire.OutPoint
		for _, forfeitReq := range forfeitReqs {
			forfeitedOutpoints = append(forfeitedOutpoints, forfeitReq.VTXOOutpoint)
		}

		// Remove forfeited VTXOs from operator store
		err = o.vtxoStore.RemoveVTXOs(forfeitedOutpoints)
		if err != nil {
			// Log error but don't fail the transaction - it's already signed
			// In production, this should be logged for monitoring
			fmt.Printf("Warning: failed to remove forfeited VTXOs from store: %v\n", err)
		}
	}

	return signedTx, nil
}

// GetOperatorWallet exposes the operator wallet for testing access
func (o *Operator) GetOperatorWallet() wallets.OperatorWallet {
	return o.cfg.Wallet
}

// ListVTXOs returns all VTXOs currently stored by the operator
func (o *Operator) ListVTXOs() ([]*types.ServerVTXO, error) {
	return o.vtxoStore.ListVTXOs()
}

// CreateServerVTXOs converts VTXO requests with their outpoints to ServerVTXO structs
func CreateServerVTXOs(vtxoReqs []*VTXORequest, outpoints []*wire.OutPoint, operatorKey *btcec.PublicKey) ([]*types.ServerVTXO, error) {
	if len(vtxoReqs) != len(outpoints) {
		return nil, fmt.Errorf("mismatch between VTXO requests (%d) and outpoints (%d)", len(vtxoReqs), len(outpoints))
	}

	serverVTXOs := make([]*types.ServerVTXO, len(vtxoReqs))

	for i, req := range vtxoReqs {
		serverVTXOs[i] = &types.ServerVTXO{
			Outpoint:        outpoints[i],
			Amount:          req.Amount,
			ClientKey:       req.ClientKey,
			OperatorKey:     operatorKey,
			Expiry:          req.Expiry,
			OriginalRequest: req,
		}
	}

	return serverVTXOs, nil
}

// validateAndProcessForfeits validates forfeit transactions using the server's VTXO store
func (o *Operator) validateAndProcessForfeits(forfeitTxSigs []*ForfeitTxSig) error {
	if len(forfeitTxSigs) == 0 {
		return nil
	}

	// Get server's forfeit address for validation
	forfeitAddr, err := o.cfg.Wallet.GetForfeitAddress()
	if err != nil {
		return fmt.Errorf("failed to get server forfeit address: %w", err)
	}

	forfeitScript, err := txscript.PayToAddrScript(forfeitAddr)
	if err != nil {
		return fmt.Errorf("failed to create forfeit script: %w", err)
	}

	// Validate each forfeit transaction and reconstruct witness components
	for i, forfeitTxSig := range forfeitTxSigs {
		if err := o.validateAndCompleteForfeitWithStore(forfeitTxSig, forfeitScript); err != nil {
			return fmt.Errorf("failed to validate forfeit tx %d: %w", i, err)
		}

		// TODO: let the server sign the connector input. Then store
		// the whole thing.
	}

	return nil
}

// validateAndCompleteForfeitWithStore validates a forfeit transaction and reconstructs witness components from VTXO store
func (o *Operator) validateAndCompleteForfeitWithStore(forfeitTxSig *ForfeitTxSig, expectedForfeitScript []byte) error {
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

	// Get the VTXO being forfeited from our store
	vtxoInput := tx.TxIn[0] // VTXO is first input
	serverVTXO, err := o.vtxoStore.GetVTXO(&vtxoInput.PreviousOutPoint)
	if err != nil {
		return fmt.Errorf("VTXO %s not found in store: %w", vtxoInput.PreviousOutPoint.String(), err)
	}

	// Reconstruct witness script and control block from the stored VTXO
	witnessScript, controlBlock, err := o.reconstructVTXOWitnessComponents(serverVTXO)
	if err != nil {
		return fmt.Errorf("failed to reconstruct witness components: %w", err)
	}

	// Complete the VTXO witness with reconstructed components
	err = o.completeVTXOWitnessFromStore(tx, 0, forfeitTxSig, witnessScript, controlBlock)
	if err != nil {
		return fmt.Errorf("failed to complete VTXO witness: %w", err)
	}

	return nil
}

// reconstructVTXOWitnessComponents recreates the witness script and control block from ServerVTXO
func (o *Operator) reconstructVTXOWitnessComponents(serverVTXO *types.ServerVTXO) ([]byte, []byte, error) {
	// Reconstruct the collaborative script from the VTXO request
	req, ok := serverVTXO.OriginalRequest.(*VTXORequest)
	if !ok || req == nil {
		return nil, nil, fmt.Errorf("missing or invalid original request in ServerVTXO")
	}

	// Recreate the tapscript for the VTXO
	tapscript, err := VTXOTapScript(req.ClientKey, req.OperatorKey, req.Expiry)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to recreate VTXO tapscript: %w", err)
	}

	// Assemble the taproot script tree
	tapTree := txscript.AssembleTaprootScriptTree(tapscript.Leaves...)
	if len(tapTree.LeafMerkleProofs) < 1 {
		return nil, nil, fmt.Errorf("expected at least 1 leaf in taproot tree, got %d", len(tapTree.LeafMerkleProofs))
	}

	// Get collaborative script (first leaf - index 0)
	collabLeafProof := tapTree.LeafMerkleProofs[0]
	collabScript := tapscript.Leaves[0].Script

	// Create control block for the collaborative script path
	controlBlock := collabLeafProof.ToControlBlock(&ARKNUMSKey)
	controlBlockBytes, err := controlBlock.ToBytes()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create control block: %w", err)
	}

	return collabScript, controlBlockBytes, nil
}

// completeVTXOWitnessFromStore constructs the VTXO witness using reconstructed components
func (o *Operator) completeVTXOWitnessFromStore(tx *wire.MsgTx, inputIndex int, forfeitTxSig *ForfeitTxSig, witnessScript []byte, controlBlock []byte) error {
	// Get the VTXO from the input
	vtxoOutpoint := &tx.TxIn[inputIndex].PreviousOutPoint
	serverVTXO, err := o.vtxoStore.GetVTXO(vtxoOutpoint)
	if err != nil {
		return fmt.Errorf("failed to get VTXO for signing: %w", err)
	}

	// Create the signature hash for the collaborative script path
	prevOuts := make(map[wire.OutPoint]*wire.TxOut)

	// Get the original request from ServerVTXO
	origReq, ok := serverVTXO.OriginalRequest.(*VTXORequest)
	if !ok || origReq == nil {
		return fmt.Errorf("invalid original request in ServerVTXO")
	}

	// Add the VTXO output being spent
	prevOuts[*vtxoOutpoint] = &wire.TxOut{
		Value:    int64(origReq.Amount),
		PkScript: origReq.PkScript,
	}

	// Add the connector output (second input)
	if len(tx.TxIn) > 1 {
		// For forfeit transactions, we need to include the connector output
		// In a complete implementation, the connector details would be retrieved
		// from the batch's connector tree. For now, we use the dust limit as value
		// since connectors are typically dust outputs.
		connectorOutpoint := &tx.TxIn[1].PreviousOutPoint

		// The connector script is typically a P2TR script controlled by the operator
		// For signature validation purposes, we can use a minimal output
		// Note: In production, this should retrieve the actual connector details
		connectorAddr, err := o.cfg.Wallet.NewTaprootAddress()
		if err != nil {
			return fmt.Errorf("failed to generate connector address: %w", err)
		}

		connectorScript, err := txscript.PayToAddrScript(connectorAddr)
		if err != nil {
			return fmt.Errorf("failed to create connector script: %w", err)
		}

		prevOuts[*connectorOutpoint] = &wire.TxOut{
			Value:    int64(o.cfg.DustLimit),
			PkScript: connectorScript,
		}
	}

	// Create signing context
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

	// Create sign descriptor for the collaborative script path
	signDesc := &input.SignDescriptor{
		WitnessScript:     witnessScript,
		Output:            prevOuts[*vtxoOutpoint],
		HashType:          txscript.SigHashDefault,
		InputIndex:        inputIndex,
		SignMethod:        input.TaprootScriptSpendSignMethod,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevOutFetcher,
		ControlBlock:      controlBlock,
	}

	// Sign the input using the operator's wallet
	serverSig, err := o.cfg.Wallet.SignOutputRaw(tx, signDesc)
	if err != nil {
		return fmt.Errorf("failed to sign VTXO input: %w", err)
	}

	// Ensure we have a schnorr signature
	schnorrSig, ok := serverSig.(*schnorr.Signature)
	if !ok {
		return fmt.Errorf("expected schnorr signature for VTXO input")
	}

	// Construct witness: [server_sig] [client_sig] [script] [control_block]
	// Note: The order is important - server signature first, then client signature
	witness := wire.TxWitness{
		schnorrSig.Serialize(),
		forfeitTxSig.ClientVTXOSig.Serialize(),
		witnessScript,
		controlBlock,
	}

	tx.TxIn[inputIndex].Witness = witness
	return nil
}

var _ ArkServer = (*Operator)(nil)
