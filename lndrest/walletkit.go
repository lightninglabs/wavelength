package lndrest

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// WalletKit REST paths, taken from lnd's walletrpc grpc-gateway pattern vars.
const (
	pathDeriveKey      = "/v2/wallet/key"
	pathDeriveNextKey  = "/v2/wallet/key/next"
	pathNextAddr       = "/v2/wallet/address/next"
	pathListUnspent    = "/v2/wallet/utxos"
	pathLeaseOutput    = "/v2/wallet/utxos/lease"
	pathReleaseOutput  = "/v2/wallet/utxos/release"
	pathImportTapscr   = "/v2/wallet/tapscript/import"
	pathPublishTx      = "/v2/wallet/tx"
	pathGetTransaction = "/v2/wallet/tx"
	pathEstimateFee    = "/v2/wallet/estimatefee"
)

// walletKitClient implements lndclient.WalletKitClient over lnd's REST gateway.
type walletKitClient struct {
	conn *conn
}

// A compile-time check that walletKitClient satisfies the lndclient interface.
var _ lndclient.WalletKitClient = (*walletKitClient)(nil)

// RawClientWithMacAuth is required by the ServiceClient interface but returns a
// nil raw client: the REST backend has no gRPC client to expose.
func (m *walletKitClient) RawClientWithMacAuth(parentCtx context.Context) (
	context.Context, time.Duration, walletrpc.WalletKitClient) {

	return parentCtx, m.conn.timeout, nil
}

// ListUnspent returns wallet UTXOs with a confirmation count in the range.
func (m *walletKitClient) ListUnspent(ctx context.Context, minConfs,
	maxConfs int32, opts ...lndclient.ListUnspentOption) ([]*lnwallet.Utxo,
	error) {

	req := &walletrpc.ListUnspentRequest{
		MinConfs: minConfs,
		MaxConfs: maxConfs,
	}
	for _, opt := range opts {
		opt(req)
	}

	resp := &walletrpc.ListUnspentResponse{}
	if err := m.conn.post(ctx, pathListUnspent, req, resp); err != nil {
		return nil, err
	}

	utxos := make([]*lnwallet.Utxo, 0, len(resp.Utxos))
	for _, utxo := range resp.Utxos {
		var addrType lnwallet.AddressType
		switch utxo.AddressType {
		case lnrpc.AddressType_WITNESS_PUBKEY_HASH:
			addrType = lnwallet.WitnessPubKey

		case lnrpc.AddressType_NESTED_PUBKEY_HASH:
			addrType = lnwallet.NestedWitnessPubKey

		case lnrpc.AddressType_TAPROOT_PUBKEY:
			addrType = lnwallet.TaprootPubkey

		default:
			return nil, fmt.Errorf("invalid utxo address type %v",
				utxo.AddressType)
		}

		pkScript, err := hex.DecodeString(utxo.PkScript)
		if err != nil {
			return nil, err
		}

		opHash, err := chainhash.NewHash(utxo.Outpoint.TxidBytes)
		if err != nil {
			return nil, err
		}

		utxos = append(utxos, &lnwallet.Utxo{
			AddressType:   addrType,
			Value:         btcAmount(utxo.AmountSat),
			Confirmations: utxo.Confirmations,
			PkScript:      pkScript,
			OutPoint: wire.OutPoint{
				Hash:  *opHash,
				Index: utxo.Outpoint.OutputIndex,
			},
		})
	}

	return utxos, nil
}

// LeaseOutput locks an output for the given lease duration.
func (m *walletKitClient) LeaseOutput(ctx context.Context, lockID wtxmgr.LockID,
	op wire.OutPoint, leaseTime time.Duration) (time.Time, error) {

	req := &walletrpc.LeaseOutputRequest{
		Id: lockID[:],
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   op.Hash[:],
			OutputIndex: op.Index,
		},
		ExpirationSeconds: uint64(leaseTime.Seconds()),
	}

	resp := &walletrpc.LeaseOutputResponse{}
	if err := m.conn.post(ctx, pathLeaseOutput, req, resp); err != nil {
		return time.Time{}, err
	}

	return time.Unix(int64(resp.Expiration), 0), nil
}

// ReleaseOutput unlocks a previously leased output.
func (m *walletKitClient) ReleaseOutput(ctx context.Context,
	lockID wtxmgr.LockID, op wire.OutPoint) error {

	req := &walletrpc.ReleaseOutputRequest{
		Id: lockID[:],
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   op.Hash[:],
			OutputIndex: op.Index,
		},
	}

	return m.conn.post(
		ctx, pathReleaseOutput, req, &walletrpc.ReleaseOutputResponse{},
	)
}

// DeriveNextKey derives the next unused key in the given family.
func (m *walletKitClient) DeriveNextKey(ctx context.Context, family int32) (
	*keychain.KeyDescriptor, error) {

	req := &walletrpc.KeyReq{
		KeyFamily: family,
	}

	resp := &signrpc.KeyDescriptor{}
	if err := m.conn.post(ctx, pathDeriveNextKey, req, resp); err != nil {
		return nil, err
	}

	key, err := btcec.ParsePubKey(resp.RawKeyBytes)
	if err != nil {
		return nil, err
	}

	return &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(resp.KeyLoc.KeyFamily),
			Index:  uint32(resp.KeyLoc.KeyIndex),
		},
		PubKey: key,
	}, nil
}

// DeriveKey derives the key at the given locator.
func (m *walletKitClient) DeriveKey(ctx context.Context,
	in *keychain.KeyLocator) (*keychain.KeyDescriptor, error) {

	req := &signrpc.KeyLocator{
		KeyFamily: int32(in.Family),
		KeyIndex:  int32(in.Index),
	}

	resp := &signrpc.KeyDescriptor{}
	if err := m.conn.post(ctx, pathDeriveKey, req, resp); err != nil {
		return nil, err
	}

	key, err := btcec.ParsePubKey(resp.RawKeyBytes)
	if err != nil {
		return nil, err
	}

	return &keychain.KeyDescriptor{
		KeyLocator: *in,
		PubKey:     key,
	}, nil
}

// NextAddr returns a fresh wallet address of the requested type.
func (m *walletKitClient) NextAddr(ctx context.Context, accountName string,
	addressType walletrpc.AddressType, change bool) (btcaddr.Address,
	error) {

	req := &walletrpc.AddrRequest{
		Account: accountName,
		Type:    addressType,
		Change:  change,
	}

	resp := &walletrpc.AddrResponse{}
	if err := m.conn.post(ctx, pathNextAddr, req, resp); err != nil {
		return nil, err
	}

	// Decode against the configured chain params so a network-mismatched
	// address is rejected, matching ImportTaprootScript below (bech32/
	// bech32m HRPs are self-describing, but pinning the network is
	// defense-in-depth and keeps the two decode sites consistent).
	return btcaddr.DecodeAddress(resp.Addr, m.conn.params)
}

// GetTransaction returns wallet details for the given txid.
func (m *walletKitClient) GetTransaction(ctx context.Context,
	txid chainhash.Hash) (lndclient.Transaction, error) {

	// GetTransaction is a GET endpoint that binds its txid from the query
	// string, so encode it there rather than in a body.
	query := url.Values{}
	query.Set("txid", txid.String())
	path := pathGetTransaction + "?" + query.Encode()

	resp := &lnrpc.Transaction{}
	if err := m.conn.get(ctx, path, resp); err != nil {
		return lndclient.Transaction{}, err
	}

	return unmarshalTransaction(resp)
}

// unmarshalTransaction converts an lnrpc.Transaction into lndclient's shape,
// mirroring lndclient's own conversion.
func unmarshalTransaction(rpcTx *lnrpc.Transaction) (lndclient.Transaction,
	error) {

	rawTx, err := hex.DecodeString(rpcTx.RawTxHex)
	if err != nil {
		return lndclient.Transaction{}, err
	}

	tx, err := decodeTx(rawTx)
	if err != nil {
		return lndclient.Transaction{}, err
	}

	return lndclient.Transaction{
		Tx:                tx,
		TxHash:            tx.TxHash().String(),
		Timestamp:         time.Unix(rpcTx.TimeStamp, 0),
		Amount:            btcAmount(rpcTx.Amount),
		Fee:               btcAmount(rpcTx.TotalFees),
		Confirmations:     rpcTx.NumConfirmations,
		Label:             rpcTx.Label,
		BlockHash:         rpcTx.BlockHash,
		BlockHeight:       rpcTx.BlockHeight,
		OutputDetails:     rpcTx.OutputDetails,
		PreviousOutpoints: rpcTx.PreviousOutpoints,
	}, nil
}

// PublishTransaction broadcasts the transaction with an optional label.
func (m *walletKitClient) PublishTransaction(ctx context.Context,
	tx *wire.MsgTx, label string) error {

	txRaw, err := encodeTx(tx)
	if err != nil {
		return err
	}

	req := &walletrpc.Transaction{
		TxHex: txRaw,
		Label: label,
	}

	return m.conn.post(
		ctx, pathPublishTx, req, &walletrpc.PublishResponse{},
	)
}

// EstimateFeeRate estimates the fee rate (sat/kw) for the confirmation target.
func (m *walletKitClient) EstimateFeeRate(ctx context.Context,
	confTarget int32) (chainfee.SatPerKWeight, error) {

	// The confirmation target is bound as a path parameter on this GET
	// endpoint, so it is appended to the path rather than sent as a body.
	path := pathEstimateFee + "/" + strconv.FormatInt(int64(confTarget), 10)

	resp := &walletrpc.EstimateFeeResponse{}
	if err := m.conn.get(ctx, path, resp); err != nil {
		return 0, err
	}

	return chainfee.SatPerKWeight(resp.SatPerKw), nil
}

// ImportTaprootScript imports a taproot script as a watch-only address.
func (m *walletKitClient) ImportTaprootScript(ctx context.Context,
	tapscript *waddrmgr.Tapscript) (btcaddr.Address, error) {

	if tapscript == nil {
		return nil, fmt.Errorf("invalid tapscript")
	}

	rpcReq, err := marshalTapscriptImport(tapscript)
	if err != nil {
		return nil, err
	}

	resp := &walletrpc.ImportTapscriptResponse{}
	if err := m.conn.post(ctx, pathImportTapscr, rpcReq, resp); err != nil {
		return nil, fmt.Errorf("import tapscript into lnd: %w", err)
	}

	addr, err := btcaddr.DecodeAddress(resp.P2TrAddress, m.conn.params)
	if err != nil {
		return nil, fmt.Errorf("parse imported p2tr addr: %w", err)
	}

	return addr, nil
}

// marshalTapscriptImport converts a waddrmgr tapscript into the walletrpc
// import request, mirroring lndclient's own marshalling across the four
// tapscript representations.
func marshalTapscriptImport(tapscript *waddrmgr.Tapscript) (
	*walletrpc.ImportTapscriptRequest, error) {

	var (
		rpcReq    = &walletrpc.ImportTapscriptRequest{}
		ctrlBlock = tapscript.ControlBlock
	)

	switch tapscript.Type {
	case waddrmgr.TapscriptTypeFullTree:
		rpcReq.InternalPublicKey = schnorr.SerializePubKey(
			ctrlBlock.InternalKey,
		)

		rpcLeaves := make([]*walletrpc.TapLeaf, len(tapscript.Leaves))
		for idx, leaf := range tapscript.Leaves {
			rpcLeaves[idx] = &walletrpc.TapLeaf{
				LeafVersion: uint32(leaf.LeafVersion),
				Script:      leaf.Script,
			}
		}
		rpcReq.Script = &walletrpc.ImportTapscriptRequest_FullTree{
			FullTree: &walletrpc.TapscriptFullTree{
				AllLeaves: rpcLeaves,
			},
		}

	case waddrmgr.TapscriptTypePartialReveal:
		rpcReq.InternalPublicKey = schnorr.SerializePubKey(
			ctrlBlock.InternalKey,
		)
		rpcReq.Script = &walletrpc.ImportTapscriptRequest_PartialReveal{
			PartialReveal: &walletrpc.TapscriptPartialReveal{
				RevealedLeaf: &walletrpc.TapLeaf{
					LeafVersion: uint32(
						ctrlBlock.LeafVersion,
					),
					Script: tapscript.RevealedScript,
				},
				FullInclusionProof: ctrlBlock.InclusionProof,
			},
		}

	case waddrmgr.TaprootKeySpendRootHash:
		rpcReq.InternalPublicKey = schnorr.SerializePubKey(
			ctrlBlock.InternalKey,
		)
		rpcReq.Script = &walletrpc.ImportTapscriptRequest_RootHashOnly{
			RootHashOnly: tapscript.RootHash,
		}

	case waddrmgr.TaprootFullKeyOnly:
		rpcReq.InternalPublicKey = schnorr.SerializePubKey(
			tapscript.FullOutputKey,
		)
		rpcReq.Script = &walletrpc.ImportTapscriptRequest_FullKeyOnly{
			FullKeyOnly: true,
		}

	default:
		return nil, fmt.Errorf("invalid tapscript type <%d>",
			tapscript.Type)
	}

	return rpcReq, nil
}

// The methods below are part of the lndclient.WalletKitClient interface but are
// not used by the waved wallet backend. They are stubbed to fail loudly if an
// unexpected caller reaches them over the REST transport.

// ListLeases is unsupported over REST.
func (m *walletKitClient) ListLeases(_ context.Context) (
	[]lndclient.LeaseDescriptor, error) {

	return nil, errUnsupportedOverREST
}

// SubmitPackage is unsupported over REST.
func (m *walletKitClient) SubmitPackage(_ context.Context, _ []*wire.MsgTx,
	_ *chainfee.SatPerVByte) (*lndclient.SubmitPackageResult, error) {

	return nil, errUnsupportedOverREST
}

// SendOutputs is unsupported over REST.
func (m *walletKitClient) SendOutputs(_ context.Context, _ []*wire.TxOut,
	_ chainfee.SatPerKWeight, _ string) (*wire.MsgTx, error) {

	return nil, errUnsupportedOverREST
}

// MinRelayFee is unsupported over REST.
func (m *walletKitClient) MinRelayFee(_ context.Context) (
	chainfee.SatPerKWeight, error) {

	return 0, errUnsupportedOverREST
}

// ListSweeps is unsupported over REST.
func (m *walletKitClient) ListSweeps(_ context.Context, _ int32) ([]string,
	error) {

	return nil, errUnsupportedOverREST
}

// ListSweepsVerbose is unsupported over REST.
func (m *walletKitClient) ListSweepsVerbose(_ context.Context, _ int32) (
	[]lnwallet.TransactionDetail, error) {

	return nil, errUnsupportedOverREST
}

// BumpFee is unsupported over REST.
func (m *walletKitClient) BumpFee(_ context.Context, _ wire.OutPoint,
	_ chainfee.SatPerKWeight, _ ...lndclient.BumpFeeOption) error {

	return errUnsupportedOverREST
}

// ListAccounts is unsupported over REST.
func (m *walletKitClient) ListAccounts(_ context.Context, _ string,
	_ walletrpc.AddressType) ([]*walletrpc.Account, error) {

	return nil, errUnsupportedOverREST
}

// FundPsbt is unsupported over REST.
func (m *walletKitClient) FundPsbt(_ context.Context,
	_ *walletrpc.FundPsbtRequest) (*psbt.Packet, int32,
	[]*walletrpc.UtxoLease, error) {

	return nil, 0, nil, errUnsupportedOverREST
}

// SignPsbt is unsupported over REST.
func (m *walletKitClient) SignPsbt(_ context.Context, _ *psbt.Packet) (
	*psbt.Packet, error) {

	return nil, errUnsupportedOverREST
}

// FinalizePsbt is unsupported over REST.
func (m *walletKitClient) FinalizePsbt(_ context.Context, _ *psbt.Packet,
	_ string) (*psbt.Packet, *wire.MsgTx, error) {

	return nil, nil, errUnsupportedOverREST
}

// ImportPublicKey is unsupported over REST.
func (m *walletKitClient) ImportPublicKey(_ context.Context, _ *btcec.PublicKey,
	_ lnwallet.AddressType) error {

	return errUnsupportedOverREST
}
