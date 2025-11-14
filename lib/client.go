package lib

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/ark/lib/stores"
	"github.com/lightninglabs/ark/lib/types"
	"github.com/lightninglabs/ark/lib/wallets"
	"github.com/lightningnetwork/lnd/keychain"
)

// Client is the main ark client type.
type Client struct {
	wallet   wallets.ClientWallet
	explorer Explorer

	boardingStore stores.BoardingStore
	vtxoStore     map[string]*ClientVTXO

	pendingVtxoReqs map[string]*clientVTXOInfo
	batchSession    map[string]*ClientBatchSession
}

// NewClient creates a new ark client.
func NewClient(chainParams *chaincfg.Params,
	wallet wallets.ClientWallet, explorer Explorer) ArkClient {

	return &Client{
		wallet:          wallet,
		explorer:        explorer,
		boardingStore:   stores.NewInMemoryBoardingStore(chainParams),
		vtxoStore:       make(map[string]*ClientVTXO),
		batchSession:    make(map[string]*ClientBatchSession),
		pendingVtxoReqs: make(map[string]*clientVTXOInfo),
	}
}

type ClientBatchSession struct {
	uuid string

	boardingReqs []*BoardingRequest
	vtxoReqs     map[string]*clientVTXOInfo
	leaveReqs    []*LeaveRequest
	forfeitReqs  []*ForfeitRequest
	batchTx      *wire.MsgTx

	// Connector info for forfeit transactions
	connectorPaths []*Tree // Paths to connector outputs for each forfeit
}

type clientVTXOInfo struct {
	req           *VTXORequest
	clientKeyDesc *keychain.KeyDescriptor
	signDesc      *keychain.KeyDescriptor
	signSess      *TreeSignerSession
	treePath      *Tree // The client's extracted path in the VTXO tree
}

func (c *Client) ListVTXOs() ([]*ClientVTXO, error) {
	vtxos := make([]*ClientVTXO, 0, len(c.vtxoStore))
	for _, vtxo := range c.vtxoStore {
		vtxos = append(vtxos, vtxo)
	}
	return vtxos, nil
}

func (c *Client) CreateForfeitRequest(vtxo *wire.OutPoint) (*ForfeitRequest,
	error) {

	// Check that we know about this vtxo.
	vtxoKey := vtxo.String()
	_, ok := c.vtxoStore[vtxoKey]
	if !ok {
		return nil, fmt.Errorf("unknown vtxo %s", vtxoKey)
	}

	return &ForfeitRequest{
		VTXOOutpoint: vtxo,
	}, nil
}

// NewBoardingAddress derives a new boarding address. It uses the operator
// information to construct the proper boarding script. It registers the
// script with the backing wallet so that it can properly track and spend
// outputs sent to the address.
func (c *Client) NewBoardingAddress(terms *OperatorTerms) (btcutil.Address,
	error) {

	// To do this, we need to first derive a new key from the wallet to use
	// as our key for the boarding script.
	keyDesc, err := c.wallet.NextBoardingKey()
	if err != nil {
		return nil, err
	}

	// Build the boarding taproot script.
	boardingTapScript, err := BoardingTapScript(
		keyDesc.PubKey, terms.PubKey, terms.BoardingExitDelay,
	)
	if err != nil {
		return nil, err
	}

	// Register the new script with the wallet.
	addr, err := c.wallet.WatchTaprootScript(boardingTapScript)
	if err != nil {
		return nil, err
	}

	// Finally, register the address with the boarding store.
	err = c.boardingStore.Register(&types.BoardingAddress{
		Address:     addr,
		Tapscript:   boardingTapScript,
		KeyDesc:     keyDesc,
		OperatorKey: terms.PubKey,
		ExitDelay:   terms.BoardingExitDelay,
	})
	if err != nil {
		return nil, err
	}

	return addr, nil
}

// refreshBoardingUTXOs refreshes the UTXOs for all known boarding addresses.
func (c *Client) refreshBoardingUTXOs() error {
	boardingAddrs, err := c.boardingStore.ListAddresses()
	if err != nil {
		return err
	}

	for _, addr := range boardingAddrs {
		utxos, err := c.wallet.GetUTXOsForAddress(addr)
		if err != nil {
			return err
		}

		err = c.boardingStore.RegisterUTXOs(addr, utxos)
		if err != nil {
			return err
		}
	}

	return nil
}

// ListBoardingUTXOs lists all known boarding UTXOs in the wallet.
func (c *Client) ListBoardingUTXOs() ([]*types.BoardingUTXO, error) {
	// First, refresh the boarding UTXOs.
	err := c.refreshBoardingUTXOs()
	if err != nil {
		return nil, err
	}

	return c.boardingStore.ListUTXOs()
}

// CreateBoardingRequest creates a boarding request using a UTXO that funds
// the given boarding address.
func (c *Client) CreateBoardingRequest(boardingAddress *types.BoardingUTXO) (
	*BoardingRequest, error) {

	return &BoardingRequest{
		Outpoint:    &boardingAddress.UTXO.OutPoint,
		ClientKey:   boardingAddress.Address.KeyDesc.PubKey,
		OperatorKey: boardingAddress.Address.OperatorKey,
		ExitDelay:   boardingAddress.Address.ExitDelay,
	}, nil
}

// CreateLeaveRequest creates a leave request for the given amount.
func (c *Client) CreateLeaveRequest(amt btcutil.Amount) (*LeaveRequest, error) {
	// Derive a new address to send the leave funds to.
	leaveAddr, err := c.wallet.NextAddress()
	if err != nil {
		return nil, err
	}

	clientScript, err := txscript.PayToAddrScript(leaveAddr)
	if err != nil {
		return nil, err
	}

	return &LeaveRequest{
		Output: &wire.TxOut{
			Value:    int64(amt),
			PkScript: clientScript,
		},
	}, nil
}

// CreateVTXORequest creates a VTXO request that locks the given amount in a
// VTXO output. It uses the provided operator terms to construct the proper
// VTXO script.
func (c *Client) CreateVTXORequest(terms *OperatorTerms,
	amt btcutil.Amount) (*VTXORequest, error) {

	// A VTXO request involves 2 client keys. One is used in the output
	// pk script of the VTXO itself. The other is used in the musig2
	// signing sessions of the VTX tree that contains the VTXO.
	vtxoKey, err := c.wallet.NextVTXOKey()
	if err != nil {
		return nil, err
	}

	// Compute the key that will be used in the musig2 signing sessions for
	// the VTXO tree.
	musigKey, err := c.wallet.NextMusig2SigningKey()
	if err != nil {
		return nil, err
	}

	req, err := NewVTXORequest(
		terms.PubKey, vtxoKey.PubKey, musigKey.PubKey,
		terms.VTXOExitDelay, amt,
	)
	if err != nil {
		return nil, err
	}

	// remember this vtxo.
	c.pendingVtxoReqs[toHexKey(musigKey.PubKey)] = &clientVTXOInfo{
		req:           req,
		clientKeyDesc: &vtxoKey,
		signDesc:      &musigKey,
	}

	return req, nil
}

func (c *Client) BatchJoined(uuid string, boardReqs []*BoardingRequest,
	vtxoReqs []*VTXORequest, leaveReqs []*LeaveRequest,
	forfeitReqs []*ForfeitRequest) error {

	vtxos := make(map[string]*clientVTXOInfo)
	for _, req := range vtxoReqs {
		info, ok := c.pendingVtxoReqs[toHexKey(req.SigningKey)]
		if !ok {
			return fmt.Errorf("no pending vtxo req for key %x",
				req.SigningKey.SerializeCompressed())
		}

		vtxos[toHexKey(req.SigningKey)] = info

		delete(c.pendingVtxoReqs, toHexKey(req.SigningKey))
	}

	session := &ClientBatchSession{
		uuid:         uuid,
		vtxoReqs:     vtxos,
		boardingReqs: boardReqs,
		leaveReqs:    leaveReqs,
		forfeitReqs:  forfeitReqs,
	}

	c.batchSession[uuid] = session

	return nil
}

func (c *Client) BatchCreated(uuid string, clientInfo *ClientBatchInfo) error {
	session, ok := c.batchSession[uuid]
	if !ok {
		return fmt.Errorf("no batch session found for uuid %s", uuid)
	}

	// Extract batch transaction from client info
	batchTx := clientInfo.Transaction

	// If client has VTXO info, process the tree
	if clientInfo.VTXOInfo != nil {
		tree := clientInfo.VTXOInfo.BatchOutput.Tree

		// For each cosigner, see if we have a vtxo req where that is our
		// signing key.
		for _, cosigner := range tree.Root.CoSigners {
			req, ok := session.vtxoReqs[toHexKey(cosigner)]
			if !ok {
				// TODO: error if we expected a vtxo in this tree.
				continue
			}

			clientTree := tree.ExtractPathForCosigner(cosigner)
			if clientTree == nil {
				return fmt.Errorf("no tree path found for signing key %x",
					cosigner.SerializeCompressed())
			}

			err := clientTree.Verify()
			if err != nil {
				return fmt.Errorf("tree verification failed: %w", err)
			}

			// TODO: Verify that this tree is an output of the given batch tx.

			// Store the tree path for later use in signature submission
			req.treePath = clientTree

			// TODO: Check that this tree has a single path leading to the vtxo
			// identified by the give signing key. Ie, validate it against the
			// stored req.

			// Initialise a signing session for this tree using the new simplified method.
			signSess, err := clientTree.NewTreeSignerSession(c.wallet, req.signDesc)
			if err != nil {
				return fmt.Errorf("failed to create tree signer "+
					"session: %w", err)
			}

			req.signSess = signSess
		}
	}

	// If client has connector info, store the connector paths
	if clientInfo.ConnectorInfo != nil {
		session.connectorPaths = clientInfo.ConnectorInfo.ConnectorPaths
	}

	// Store the batch tx for later use.
	session.batchTx = batchTx

	return nil
}

// GetSignedForfeits creates forfeit transactions and returns unsigned transactions with client VTXO signatures
func (c *Client) GetSignedForfeits(uuid string, serverForfeitScript []byte) ([]*ForfeitTxSig, error) {
	session, ok := c.batchSession[uuid]
	if !ok {
		return nil, fmt.Errorf("no batch session found for uuid %s", uuid)
	}

	// Return empty slice if no forfeit requests
	if len(session.forfeitReqs) == 0 {
		return []*ForfeitTxSig{}, nil
	}

	// Ensure we have connector paths for forfeits
	if len(session.connectorPaths) != len(session.forfeitReqs) {
		return nil, fmt.Errorf("mismatch between forfeit requests (%d) and connector paths (%d)",
			len(session.forfeitReqs), len(session.connectorPaths))
	}

	var forfeitTxSigs []*ForfeitTxSig

	// Create a forfeit transaction for each forfeit request
	for i, forfeitReq := range session.forfeitReqs {
		// Get the VTXO being forfeited
		vtxoKey := forfeitReq.VTXOOutpoint.String()
		vtxo, ok := c.vtxoStore[vtxoKey]
		if !ok {
			return nil, fmt.Errorf("VTXO not found for forfeit: %s", vtxoKey)
		}

		// Get the connector path for this forfeit
		connectorPath := session.connectorPaths[i]

		// Get the connector output (leaf of the connector tree)
		connectorOutput, err := c.getConnectorOutput(connectorPath)
		if err != nil {
			return nil, fmt.Errorf("failed to get connector output: %w", err)
		}

		// Create the unsigned forfeit transaction
		unsignedTx, err := BuildForfeitTx(
			vtxo.Outpoint, vtxo.Amount,
			connectorOutput.Outpoint,
			serverForfeitScript,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create forfeit tx: %w", err)
		}

		// Get the client's VTXO signature (witness components no longer needed by server)
		vtxoSig, err := c.signVTXOForForfeit(
			unsignedTx, vtxo, connectorOutput,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to sign VTXO input: %w", err)
		}

		// Create the ForfeitTxSig struct
		forfeitTxSig := &ForfeitTxSig{
			UnsignedTx:    unsignedTx,
			ClientVTXOSig: vtxoSig,
		}

		forfeitTxSigs = append(forfeitTxSigs, forfeitTxSig)
	}

	return forfeitTxSigs, nil
}

// Connector represents a connector output with its outpoint and value info
type Connector struct {
	Outpoint *wire.OutPoint
	Output   *wire.TxOut
}

// getConnectorOutput extracts the connector output details from the connector tree path
func (c *Client) getConnectorOutput(connectorPath *Tree) (*Connector, error) {
	// Find the leaf node in the connector tree
	leafNodes, err := connectorPath.GetLeafNodes()
	if err != nil {
		return nil, fmt.Errorf("failed to get leaf nodes: %w", err)
	}
	if len(leafNodes) != 1 {
		return nil, fmt.Errorf("expected exactly one leaf in connector path, got %d", len(leafNodes))
	}

	leaf := leafNodes[0]

	// Get the non-anchor outpoint (connector outpoint)
	outpoint, err := leaf.GetNonAnchorOutpoint()
	if err != nil {
		return nil, fmt.Errorf("failed to get connector outpoint: %w", err)
	}

	// Get the connector output details from the leaf's outputs
	// The connector output should be the first non-anchor output
	var connectorTxOut *wire.TxOut
	for _, output := range leaf.Outputs {
		if bytes.Equal(output.PkScript, ANCHOR_PKSCRIPT) {
			continue
		}

		connectorTxOut = output
		break
	}

	if connectorTxOut == nil {
		return nil, fmt.Errorf("no connector output found in leaf")
	}

	return &Connector{
		Outpoint: outpoint,
		Output:   connectorTxOut,
	}, nil
}

// signVTXOForForfeit creates a signature for the VTXO input and returns the signature components
func (c *Client) signVTXOForForfeit(tx *wire.MsgTx, vtxo *ClientVTXO,
	connector *Connector) (*schnorr.Signature, error) {

	// Create VTXO tapscript
	tapscript, err := VTXOTapScript(vtxo.ClientKey.PubKey, vtxo.OperatorKey, vtxo.Expiry)
	if err != nil {
		return nil, fmt.Errorf("failed to create VTXO tapscript: %w", err)
	}

	signDesc, err := ForfeitTxVTXOSignDescriptor(
		vtxo.ClientKey,
		tx, connector, &VTXOSpendContext{
			Outpoint: vtxo.Outpoint,
			Output: &wire.TxOut{
				Value:    int64(vtxo.Amount),
				PkScript: vtxo.PkScript,
			},
			TapScript: tapscript,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create VTXO sign "+
			"descriptor: %w", err)
	}

	// Sign the input.
	sig, err := c.wallet.SignOutputRaw(tx, signDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to sign VTXO input: %w", err)
	}

	schnorrSig, ok := sig.(*schnorr.Signature)
	if !ok {
		return nil, fmt.Errorf("expected schnorr signature for VTXO input")
	}

	// Return the signature components
	return schnorrSig, nil
}

// Temporary.
func (c *Client) GetNoncesForSigner(uuid string, key *btcec.PublicKey) (
	map[string]Musig2PubNonce, error) {

	session, ok := c.batchSession[uuid]
	if !ok {
		return nil, fmt.Errorf("no batch session found for uuid %s", uuid)
	}

	req, ok := session.vtxoReqs[toHexKey(key)]
	if !ok {
		return nil, fmt.Errorf("no vtxo request found for "+
			"signing key %x", key.SerializeCompressed())
	}

	return req.signSess.GetNonces()
}

func (c *Client) SubmitNonces(uuid string, signingKey *btcec.PublicKey,
	nonces map[string][]Musig2PubNonce) (
	map[string]*musig2.PartialSignature, error) {

	session, ok := c.batchSession[uuid]
	if !ok {
		return nil, fmt.Errorf("no batch session found for uuid %s", uuid)
	}

	req, ok := session.vtxoReqs[toHexKey(signingKey)]
	if !ok {
		return nil, fmt.Errorf("no vtxo request found for signing key %x",
			signingKey.SerializeCompressed())
	}

	err := req.signSess.RegisterNonces(nonces)
	if err != nil {
		return nil, fmt.Errorf("failed to register nonces: %w", err)
	}

	return req.signSess.Signatures()
}

func (c *Client) SubmitTreeSigs(uuid string, signingKey *btcec.PublicKey,
	sigs map[string]*schnorr.Signature) error {

	session, ok := c.batchSession[uuid]
	if !ok {
		return fmt.Errorf("no batch session found for uuid %s", uuid)
	}

	req, ok := session.vtxoReqs[toHexKey(signingKey)]
	if !ok {
		return fmt.Errorf("no vtxo request found for signing key %x",
			signingKey.SerializeCompressed())
	}

	if req.signSess == nil {
		return fmt.Errorf("no signing session found for key %x",
			signingKey.SerializeCompressed())
	}

	if req.treePath == nil {
		return fmt.Errorf("no tree path found for signing key %x - call BatchCreated first",
			signingKey.SerializeCompressed())
	}

	// Store the signatures in the client's tree path
	err := req.treePath.SubmitTreeSigs(sigs)
	if err != nil {
		return fmt.Errorf("failed to submit tree signatures: %w", err)
	}

	// Verify all signatures in the tree using the simplified method
	err = req.treePath.VerifySigned()
	if err != nil {
		return fmt.Errorf("signature verification failed: %w", err)
	}

	vtxoLeaf, err := req.treePath.GetLeafForCosigner(signingKey)
	if err != nil {
		return fmt.Errorf("failed to get leaf for cosigner: %w", err)
	}

	// Extract the VTXO info and store it in our VTXO set. VTXOs are
	// identified by their outpoint.
	// TODO: in reality, it only becomes "live" after the batch tx is fully
	// signed, broadcast & confirmed.
	outpoint, err := vtxoLeaf.GetNonAnchorOutpoint()
	if err != nil {
		return fmt.Errorf("failed to get outpoint from leaf: %w", err)
	}

	c.vtxoStore[outpoint.String()] = &ClientVTXO{
		Outpoint:    outpoint,
		Amount:      req.req.Amount,
		PkScript:    req.req.PkScript,
		Expiry:      req.req.Expiry,
		ClientKey:   *req.clientKeyDesc,
		OperatorKey: req.req.OperatorKey,
	}

	return nil
}

type ClientVTXO struct {
	// Outpoint is the outpoint of the VTXO.
	Outpoint *wire.OutPoint

	// Amount is the amount of satoshis to lock in the VTXO.
	Amount btcutil.Amount

	// PkScript is the output script of the VTXO. This will have
	// both a collaborative and unilateral spend path.
	PkScript []byte

	// Expiry is the CSV delay used in the unilateral timeout script path
	// of the VTXO.
	Expiry uint32

	// ClientKey is the public key of the client used in the construction
	// of the collaborative spend path of the VTXO.
	ClientKey keychain.KeyDescriptor

	// OperatorKey is the public key of the operator used in the
	// construction of the collaborative spend path of the VTXO.
	OperatorKey *btcec.PublicKey
}

// SweepExpiredBoardingUTXOs creates a transaction that sweeps all expired
// boarding UTXOs back to the wallet.
func (c *Client) SweepExpiredBoardingUTXOs() (*wire.MsgTx, error) {
	err := c.refreshBoardingUTXOs()
	if err != nil {
		return nil, err
	}

	// Get all expired boarding UTXOs.
	utxos, err := c.boardingStore.ListExpiredUTXOs()
	if err != nil {
		return nil, err
	}

	// Derive a sweep address.
	sweepAddr, err := c.wallet.NextAddress()
	if err != nil {
		return nil, err
	}

	clientScript, err := txscript.PayToAddrScript(sweepAddr)
	if err != nil {
		return nil, err
	}

	// Create the sweep transaction.
	tx := wire.NewMsgTx(2)

	// Add all expired UTXOs as inputs.
	var totalInputAmt btcutil.Amount
	for _, utxo := range utxos {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: utxo.UTXO.OutPoint,
			Sequence:         utxo.Address.ExitDelay,
		})
		totalInputAmt += utxo.UTXO.Value
	}

	// TODO(elle): estimate fee properly.
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(totalInputAmt) - 1000, // Subtract fee
		PkScript: clientScript,
	})

	// Now, we need to sign each input.
	// First, create prev output fetcher.
	prevOuts := make(map[wire.OutPoint]*wire.TxOut)
	utxoLookup := make(map[wire.OutPoint]*types.BoardingUTXO)
	for _, utxo := range utxos {
		prevOuts[utxo.UTXO.OutPoint] = &wire.TxOut{
			Value:    int64(utxo.UTXO.Value),
			PkScript: utxo.UTXO.PkScript,
		}
		utxoLookup[utxo.UTXO.OutPoint] = utxo
	}

	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

	for i, in := range tx.TxIn {
		utxo, ok := utxoLookup[in.PreviousOutPoint]
		if !ok {
			return nil, fmt.Errorf("utxo for input %d not found", i)
		}

		scriptInfo := utxo.Address
		boardingInput := utxo.UTXO

		timeoutInfo, err := NewBoardingTimeoutSpendInfo(
			scriptInfo.Tapscript,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to build timeout "+
				"spend info: %w", err)
		}

		signDesc, err := BoardingTimeoutSignDescriptor(
			&scriptInfo.KeyDesc,
			boardingInput.Value,
			boardingInput.PkScript,
			i,
			sigHashes,
			prevOutFetcher,
			timeoutInfo,
		)
		if err != nil {
			return nil, err
		}

		witness, err := BoardingTimoutSpendWitness(
			c.wallet, signDesc, tx,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create timeout "+
				"witness: %w", err)
		}

		tx.TxIn[i].Witness = witness
	}

	return tx, nil
}

func (c *Client) SignBoardingInputs(uuid string,
	tx *wire.MsgTx) ([]*BoardingInputSignature, error) {

	session, ok := c.batchSession[uuid]
	if !ok {
		return nil, fmt.Errorf("no batch session found for uuid %s",
			uuid)
	}

	// Use Explorer API to get all previous outputs for the transaction
	allPrevOuts, err := c.explorer.GetPrevOutputs(tx)
	if err != nil {
		return nil, err
	}

	// Create prev output fetcher for all transaction inputs
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(allPrevOuts)

	// Create signature hashes.
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

	// Find and sign each client boarding input.
	var signatures []*BoardingInputSignature
	for inputIdx, txIn := range tx.TxIn {
		// Find if this input belongs to the client
		req := findBoardReq(session.boardingReqs, txIn.PreviousOutPoint)
		if req == nil {
			continue // Not our input
		}

		// Get the boarding UTXO from the store to access additional info.
		boardUTXO, err := c.boardingStore.FindBoardingUTXO(req.Outpoint)
		if err != nil {
			return nil, fmt.Errorf("failed to find boarding UTXO "+
				"for outpoint %v: %w", req.Outpoint, err)
		}

		boardInput := boardUTXO.Address
		utxo := boardUTXO.UTXO

		spendInfo, err := NewBoardingCollabSpendInfo(
			boardInput.Tapscript,
		)
		if err != nil {
			return nil, err
		}

		signature, err := SignBoardingCollabInput(
			c.wallet, tx, inputIdx, spendInfo,
			&boardInput.KeyDesc, utxo.Value, utxo.PkScript,
			sigHashes, prevOutFetcher,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to sign boarding "+
				"input %d: %w", inputIdx, err)
		}

		signatures = append(signatures, &BoardingInputSignature{
			InputIndex:      inputIdx,
			Outpoint:        txIn.PreviousOutPoint,
			ClientSignature: signature,
		})
	}

	return signatures, nil
}

// findClientInput finds the boarding request that matches the given
// outpoint.
func findBoardReq(reqs []*BoardingRequest,
	outpoint wire.OutPoint) *BoardingRequest {

	for _, input := range reqs {
		if !input.Outpoint.Hash.IsEqual(&outpoint.Hash) {
			continue
		}

		if input.Outpoint.Index == outpoint.Index {
			return input
		}
	}

	return nil
}
