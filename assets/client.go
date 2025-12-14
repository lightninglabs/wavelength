package assets

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/lndclient"
	tap "github.com/lightninglabs/taproot-assets"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/rpcutils"
	"github.com/lightninglabs/taproot-assets/tapcfg"
	"github.com/lightninglabs/taproot-assets/tapfreighter"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/assetwalletrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/mintrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/universerpc"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightninglabs/taproot-assets/universe"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

var (

	// maxMsgRecvSize is the largest message our client will receive. We
	// set this to 200MiB atm.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(200 * 1024 * 1024)
)

// TapdConfig is a struct that holds the configuration options to connect to a
// taproot assets daemon.
type TapdConfig struct {
	Activate     bool   `long:"activate" description:"Activate Tap daemon"`
	Host         string `long:"host" description:"Tap daemon host"`
	MacaroonPath string `long:"macaroonpath" description:"Macaroon path"`
	TLSPath      string `long:"tlspath" description:"TLS cert path"`
}

// DefaultTapdConfig returns a default configuration to connect to a taproot
// assets daemon.
func DefaultTapdConfig() *TapdConfig {
	defaultConf := tapcfg.DefaultConfig()
	return &TapdConfig{
		Activate:     false,
		Host:         "localhost:10029",
		MacaroonPath: defaultConf.RpcConf.MacaroonPath,
		TLSPath:      defaultConf.RpcConf.TLSCertPath,
	}
}

// TapdClient is a client for the Tap daemon.
type TapdClient struct {
	mintrpc.MintClient
	taprpc.TaprootAssetsClient
	universerpc.UniverseClient
	assetwalletrpc.AssetWalletClient

	cfg *TapdConfig
	cc  *grpc.ClientConn

	assetNameMutex sync.Mutex
	assetNameCache map[string]string
}

func getClientConn(config *TapdConfig) (*grpc.ClientConn, error) {
	// Load the specified TLS certificate and build transport credentials.
	creds, err := credentials.NewClientTLSFromFile(config.TLSPath, "")
	if err != nil {
		return nil, err
	}

	// Load the specified macaroon file.
	macBytes, err := os.ReadFile(config.MacaroonPath)
	if err != nil {
		return nil, err
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return nil, err
	}

	macaroon, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return nil, err
	}
	// Create the DialOptions with the macaroon credentials.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithPerRPCCredentials(macaroon),
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
	}

	// Dial the gRPC server.
	conn, err := grpc.Dial(config.Host, opts...)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// NewTapdClient returns a new taproot assets client.
func NewTapdClient(config *TapdConfig) (*TapdClient, error) {
	// Create the client connection to the server.
	conn, err := getClientConn(config)
	if err != nil {
		return nil, err
	}

	// Create the TapdClient.
	client := &TapdClient{
		assetNameCache:      make(map[string]string),
		cc:                  conn,
		cfg:                 config,
		MintClient:          mintrpc.NewMintClient(conn),
		TaprootAssetsClient: taprpc.NewTaprootAssetsClient(conn),
		UniverseClient:      universerpc.NewUniverseClient(conn),
		AssetWalletClient:   assetwalletrpc.NewAssetWalletClient(conn),
	}

	return client, nil
}

// Close closes the client connection to the server.
func (t *TapdClient) Close() {
	t.cc.Close()
}

// GetAssetName returns the human-readable name of the asset.
func (t *TapdClient) GetAssetName(ctx context.Context,
	assetID []byte) (string, error) {

	t.assetNameMutex.Lock()
	defer t.assetNameMutex.Unlock()

	assetIDStr := hex.EncodeToString(assetID)
	if name, ok := t.assetNameCache[assetIDStr]; ok {
		return name, nil
	}

	assetStats, err := t.UniverseClient.QueryAssetStats(
		ctx, &universerpc.AssetStatsQuery{
			AssetIdFilter: assetID,
		},
	)
	if err != nil {
		return "", err
	}

	if len(assetStats.AssetStats) == 0 {
		return "", fmt.Errorf("asset not found")
	}

	var assetName string

	// If the asset belongs to a group, return the group name.
	if assetStats.AssetStats[0].GroupAnchor != nil {
		assetName = assetStats.AssetStats[0].GroupAnchor.AssetName
	} else {
		assetName = assetStats.AssetStats[0].Asset.AssetName
	}

	t.assetNameCache[assetIDStr] = assetName

	return assetName, nil
}

// FundAndSignVpacket funds and signs a vpacket.
func (t *TapdClient) FundAndSignVpacket(ctx context.Context,
	vpkt *tappsbt.VPacket) (*tappsbt.VPacket, error) {

	// Fund the packet.
	var buf bytes.Buffer
	err := vpkt.Serialize(&buf)
	if err != nil {
		return nil, err
	}

	fundResp, err := t.FundVirtualPsbt(
		ctx, &assetwalletrpc.FundVirtualPsbtRequest{
			Template: &assetwalletrpc.FundVirtualPsbtRequest_Psbt{
				Psbt: buf.Bytes(),
			},
		},
	)
	if err != nil {
		return nil, err
	}

	// Sign the packet.
	signResp, err := t.SignVirtualPsbt(
		ctx, &assetwalletrpc.SignVirtualPsbtRequest{
			FundedPsbt: fundResp.FundedPsbt,
		},
	)
	if err != nil {
		return nil, err
	}

	return tappsbt.NewFromRawBytes(
		bytes.NewReader(signResp.SignedPsbt), false,
	)
}

// addP2WPKHOutputToPsbt adds a normal bitcoin P2WPKH output to a psbt for the
// given key and amount.
func addP2WPKHOutputToPsbt(packet *psbt.Packet, keyDesc keychain.KeyDescriptor,
	amount btcutil.Amount, params *chaincfg.Params) error {

	derivation, _, _ := btcwallet.Bip32DerivationFromKeyDesc(
		keyDesc, params.HDCoinType,
	)

	// Convert to Bitcoin address.
	pubKeyBytes := keyDesc.PubKey.SerializeCompressed()
	pubKeyHash := btcutil.Hash160(pubKeyBytes)
	address, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, params)
	if err != nil {
		return err
	}

	// Generate the P2WPKH scriptPubKey.
	scriptPubKey, err := txscript.PayToAddrScript(address)
	if err != nil {
		return err
	}

	// Add the output to the packet.
	packet.UnsignedTx.AddTxOut(
		wire.NewTxOut(int64(amount), scriptPubKey),
	)

	packet.Outputs = append(packet.Outputs, psbt.POutput{
		Bip32Derivation: []*psbt.Bip32Derivation{
			derivation,
		},
	})

	return nil
}

// PrepareAndCommitVirtualPsbts prepares and commits virtual psbt to a BTC
// template so that the underlying wallet can fund the transaction and add the
// necessary additional input to pay for fees as well as a change output if the
// change keydescriptor is not provided.
func (t *TapdClient) PrepareAndCommitVirtualPsbts(ctx context.Context,
	vpkt *tappsbt.VPacket, feeRateSatPerVByte chainfee.SatPerVByte,
	changeKeyDesc *keychain.KeyDescriptor, params *chaincfg.Params,
	sponsoringInputs []lndclient.LeaseDescriptor,
	customLockID *wtxmgr.LockID, lockExpiration time.Duration) (
	*psbt.Packet, []*tappsbt.VPacket, []*tappsbt.VPacket,
	*assetwalletrpc.CommitVirtualPsbtsResponse, error) {

	encodedVpkt, err := tappsbt.Encode(vpkt)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	btcPkt, err := tapsend.PrepareAnchoringTemplate(
		[]*tappsbt.VPacket{vpkt},
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	for _, lease := range sponsoringInputs {
		btcPkt.UnsignedTx.TxIn = append(
			btcPkt.UnsignedTx.TxIn, &wire.TxIn{
				PreviousOutPoint: lease.Outpoint,
			},
		)

		btcPkt.Inputs = append(btcPkt.Inputs, psbt.PInput{
			WitnessUtxo: wire.NewTxOut(
				int64(lease.Value),
				lease.PkScript,
			),
		})
	}

	commitRequest := &assetwalletrpc.CommitVirtualPsbtsRequest{
		Fees: &assetwalletrpc.CommitVirtualPsbtsRequest_SatPerVbyte{
			SatPerVbyte: uint64(feeRateSatPerVByte),
		},
		AnchorChangeOutput: &assetwalletrpc.
			CommitVirtualPsbtsRequest_Add{
			Add: true,
		},
		VirtualPsbts: [][]byte{
			encodedVpkt,
		},
		LockExpirationSeconds: uint64(lockExpiration.Seconds()),
	}

	if customLockID != nil {
		commitRequest.CustomLockId = (*customLockID)[:]
	}

	if feeRateSatPerVByte == 0 {
		commitRequest.SkipFunding = true
	}

	if changeKeyDesc != nil {
		err := addP2WPKHOutputToPsbt(
			btcPkt, *changeKeyDesc, btcutil.Amount(1), params,
		)
		if err != nil {
			return nil, nil, nil, nil, err
		}

		commitRequest.AnchorChangeOutput = &assetwalletrpc.
			CommitVirtualPsbtsRequest_ExistingOutputIndex{
			ExistingOutputIndex: 1,
		}
	} else {
		commitRequest.AnchorChangeOutput =
			&assetwalletrpc.CommitVirtualPsbtsRequest_Add{
				Add: true,
			}
	}
	var buf bytes.Buffer
	err = btcPkt.Serialize(&buf)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	commitRequest.AnchorPsbt = buf.Bytes()

	commitResponse, err := t.AssetWalletClient.CommitVirtualPsbts(
		ctx, commitRequest,
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	fundedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(commitResponse.AnchorPsbt), false,
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	activePackets := make(
		[]*tappsbt.VPacket, len(commitResponse.VirtualPsbts),
	)
	for idx := range commitResponse.VirtualPsbts {
		activePackets[idx], err = tappsbt.Decode(
			commitResponse.VirtualPsbts[idx],
		)
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}

	passivePackets := make(
		[]*tappsbt.VPacket, len(commitResponse.PassiveAssetPsbts),
	)
	for idx := range commitResponse.PassiveAssetPsbts {
		passivePackets[idx], err = tappsbt.Decode(
			commitResponse.PassiveAssetPsbts[idx],
		)
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}

	return fundedPacket, activePackets, passivePackets, commitResponse, nil
}

// LogAndPublish logs and publishes a psbt with the given active and passive
// assets.
func (t *TapdClient) LogAndPublish(ctx context.Context, btcPkt *psbt.Packet,
	activeAssets []*tappsbt.VPacket, passiveAssets []*tappsbt.VPacket,
	commitResp *assetwalletrpc.CommitVirtualPsbtsResponse,
	skipBoradcast bool, label string) (*taprpc.SendAssetResponse, error) {

	var buf bytes.Buffer
	err := btcPkt.Serialize(&buf)
	if err != nil {
		return nil, err
	}

	request := &assetwalletrpc.PublishAndLogRequest{
		AnchorPsbt:            buf.Bytes(),
		VirtualPsbts:          make([][]byte, len(activeAssets)),
		PassiveAssetPsbts:     make([][]byte, len(passiveAssets)),
		ChangeOutputIndex:     commitResp.ChangeOutputIndex,
		LndLockedUtxos:        commitResp.LndLockedUtxos,
		SkipAnchorTxBroadcast: skipBoradcast,
		Label:                 label,
	}

	for idx := range activeAssets {
		request.VirtualPsbts[idx], err = tappsbt.Encode(
			activeAssets[idx],
		)
		if err != nil {
			return nil, err
		}
	}
	for idx := range passiveAssets {
		request.PassiveAssetPsbts[idx], err = tappsbt.Encode(
			passiveAssets[idx],
		)
		if err != nil {
			return nil, err
		}
	}

	resp, err := t.PublishAndLogTransfer(ctx, request)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// GetAssetBalance checks the balance of an asset by its ID.
func (t *TapdClient) GetAssetBalance(ctx context.Context, assetID []byte) (
	uint64, error) {

	balanceResp, err := t.ListBalances(
		ctx, &taprpc.ListBalancesRequest{
			GroupBy: &taprpc.ListBalancesRequest_AssetId{
				AssetId: true,
			},
			AssetFilter: assetID,
		},
	)
	if err != nil {
		return 0, err
	}

	balance, ok := balanceResp.AssetBalances[hex.EncodeToString(assetID)]
	if !ok {
		return 0, nil
	}

	return balance.Balance, nil
}

// GetUnEncumberedAssetBalance returns the total balance of the given asset for
// which the given client owns the script keys.
func (t *TapdClient) GetUnEncumberedAssetBalance(ctx context.Context,
	assetID []byte) (uint64, error) {

	allAssets, err := t.ListAssets(ctx, &taprpc.ListAssetRequest{})
	if err != nil {
		return 0, err
	}

	var balance uint64
	for _, a := range allAssets.Assets {
		// Only count assets from the given asset ID.
		if !bytes.Equal(a.AssetGenesis.AssetId, assetID) {
			continue
		}

		// Non-local means we don't have the internal key to spend the
		// asset.
		if !a.ScriptKeyIsLocal {
			continue
		}

		// If the asset is not declared known or has a script path, we
		// can't spend it directly.
		if !a.ScriptKeyDeclaredKnown || a.ScriptKeyHasScriptPath {
			continue
		}

		balance += a.Amount
	}

	return balance, nil
}

// DeriveNewKeys derives a new internal and script key.
func (t *TapdClient) DeriveNewKeys(ctx context.Context) (asset.ScriptKey,
	keychain.KeyDescriptor, error) {

	scriptKeyDesc, err := t.NextScriptKey(
		ctx, &assetwalletrpc.NextScriptKeyRequest{
			KeyFamily: uint32(asset.TaprootAssetsKeyFamily),
		},
	)
	if err != nil {
		return asset.ScriptKey{}, keychain.KeyDescriptor{}, err
	}

	scriptKey, err := rpcutils.UnmarshalScriptKey(scriptKeyDesc.ScriptKey)
	if err != nil {
		return asset.ScriptKey{}, keychain.KeyDescriptor{}, err
	}

	internalKeyDesc, err := t.NextInternalKey(
		ctx, &assetwalletrpc.NextInternalKeyRequest{
			KeyFamily: uint32(asset.TaprootAssetsKeyFamily),
		},
	)
	if err != nil {
		return asset.ScriptKey{}, keychain.KeyDescriptor{}, err
	}
	internalKeyLnd, err := rpcutils.UnmarshalKeyDescriptor(
		internalKeyDesc.InternalKey,
	)
	if err != nil {
		return asset.ScriptKey{}, keychain.KeyDescriptor{}, err
	}

	return *scriptKey, internalKeyLnd, nil
}

// ImportProof inserts the given proof to the local tapd instance's database.
func (t *TapdClient) ImportProof(ctx context.Context, p *proof.Proof) error {
	var proofBytes bytes.Buffer
	err := p.Encode(&proofBytes)
	if err != nil {
		return err
	}

	asset := p.Asset

	proofType := universe.ProofTypeTransfer
	if asset.IsGenesisAsset() {
		proofType = universe.ProofTypeIssuance
	}

	uniID := universe.Identifier{
		AssetID:   asset.ID(),
		ProofType: proofType,
	}
	if asset.GroupKey != nil {
		uniID.GroupKey = &asset.GroupKey.GroupPubKey
	}

	rpcUniID, err := tap.MarshalUniID(uniID)
	if err != nil {
		return err
	}

	outpoint := &universerpc.Outpoint{
		HashStr: p.AnchorTx.TxHash().String(),
		Index:   int32(p.InclusionProof.OutputIndex),
	}

	scriptKey := p.Asset.ScriptKey.PubKey
	leafKey := &universerpc.AssetKey{
		Outpoint: &universerpc.AssetKey_Op{
			Op: outpoint,
		},
		ScriptKey: &universerpc.AssetKey_ScriptKeyBytes{
			ScriptKeyBytes: scriptKey.SerializeCompressed(),
		},
	}

	_, err = t.InsertProof(ctx, &universerpc.AssetProof{
		Key: &universerpc.UniverseKey{
			Id:      rpcUniID,
			LeafKey: leafKey,
		},
		AssetLeaf: &universerpc.AssetLeaf{
			Proof: proofBytes.Bytes(),
		},
	})

	return err
}

// ImportProofFile imports the proof file and returns the last proof.
func (t *TapdClient) ImportProofFile(ctx context.Context, rawProofFile []byte) (
	*proof.Proof, error) {

	proofFile, err := proof.DecodeFile(rawProofFile)
	if err != nil {
		return nil, err
	}

	var lastProof *proof.Proof

	for i := 0; i < proofFile.NumProofs(); i++ {
		lastProof, err = proofFile.ProofAt(uint32(i))
		if err != nil {
			return nil, err
		}

		err = t.ImportProof(ctx, lastProof)
		if err != nil {
			return nil, err
		}
	}

	return lastProof, nil
}

// TapReceiveEvent is a struct that holds the information about a receive event.
type TapReceiveEvent struct {
	// Outpoint is the anchor outpoint containing the confirmed asset.
	Outpoint wire.OutPoint

	// ConfirmationHeight is the height at which the asset transfer was
	// confirmed.
	ConfirmationHeight uint32
}

// WaitForReceiveComplete waits for a receive to complete returning a channel
// that will notify the caller when the receive is complete. The addr is the
// address to filter for, and startTS is the timestamp from which to start
// receiving events.
func (t *TapdClient) WaitForReceiveComplete(ctx context.Context, addr string,
	startTS time.Time) (<-chan TapReceiveEvent, <-chan error, error) {

	receiveEventsClient, err := t.SubscribeReceiveEvents(
		ctx, &taprpc.SubscribeReceiveEventsRequest{
			FilterAddr:     addr,
			StartTimestamp: startTS.UnixMicro(),
		},
	)
	if err != nil {
		return nil, nil, err
	}

	resChan := make(chan TapReceiveEvent)
	errChan := make(chan error, 1)

	go func() {
		for {
			select {
			case <-receiveEventsClient.Context().Done():
				errChan <- receiveEventsClient.Context().Err()

				return
			default:
			}
			event, err := receiveEventsClient.Recv()
			if err != nil {
				errChan <- err

				return
			}

			done, err := handleReceiveEvent(event, resChan)
			if err != nil {
				errChan <- err

				return
			}

			if done {
				return
			}
		}
	}()

	return resChan, errChan, err
}

func handleReceiveEvent(event *taprpc.ReceiveEvent,
	resChan chan<- TapReceiveEvent) (bool, error) {

	switch event.Status {
	case taprpc.AddrEventStatus_ADDR_EVENT_STATUS_TRANSACTION_DETECTED:

	case taprpc.AddrEventStatus_ADDR_EVENT_STATUS_TRANSACTION_CONFIRMED:

	case taprpc.AddrEventStatus_ADDR_EVENT_STATUS_COMPLETED:
		outpoint, err := wire.NewOutPointFromString(event.Outpoint)
		if err != nil {
			return false, err
		}

		resChan <- TapReceiveEvent{
			Outpoint:           *outpoint,
			ConfirmationHeight: event.ConfirmationHeight,
		}

		return true, nil

	default:
	}

	return false, nil
}

// TapSendEvent is a struct that holds the information about a send event.
type TapSendEvent struct {
	Transfer *taprpc.AssetTransfer
}

// WaitForSendComplete waits for a send to complete returning a channel that
// will notify the caller when the send is complete. The filterScriptKey is the
// script key of the asset to filter for, and the filterLabel is an optional
// label to filter the send events by.
func (t *TapdClient) WaitForSendComplete(ctx context.Context,
	filterScriptKey []byte, filterLabel string) (
	<-chan TapSendEvent, <-chan error, error) {

	sendEventsClient, err := t.SubscribeSendEvents(
		ctx, &taprpc.SubscribeSendEventsRequest{
			FilterScriptKey: filterScriptKey,
			FilterLabel:     filterLabel,
		},
	)
	if err != nil {
		return nil, nil, err
	}

	resChan := make(chan TapSendEvent)
	errChan := make(chan error, 1)

	isComplete := func(event *taprpc.SendEvent) bool {
		return event.SendState ==
			tapfreighter.SendStateComplete.String()
	}

	go func() {
		for {
			event, err := sendEventsClient.Recv()
			if err != nil {
				errChan <- err

				return
			}

			if isComplete(event) {
				resChan <- TapSendEvent{
					Transfer: event.Transfer,
				}

				return
			}
		}
	}()

	return resChan, errChan, nil
}
