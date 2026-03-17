package swaps

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
)

// RPCDaemonConn implements DaemonConn by wrapping the daemon and
// ark gRPC client stubs. It translates between the domain types in
// this package and the generated protobuf types.
type RPCDaemonConn struct {
	daemon  daemonrpc.DaemonServiceClient
	ark     arkrpc.ArkServiceClient
	indexer arkrpc.IndexerServiceClient
}

// NewRPCDaemonConn creates a new DaemonConn backed by gRPC clients for the
// daemon, ark, and indexer services.
func NewRPCDaemonConn(daemon daemonrpc.DaemonServiceClient,
	ark arkrpc.ArkServiceClient,
	indexer arkrpc.IndexerServiceClient) *RPCDaemonConn {

	return &RPCDaemonConn{
		daemon:  daemon,
		ark:     ark,
		indexer: indexer,
	}
}

// SendOOR sends an OOR transfer to the given pkScript by calling
// the daemon's SendOOR RPC. Returns the session ID of the
// initiated transfer.
//
// NOTE: This is part of the DaemonConn interface.
func (r *RPCDaemonConn) SendOOR(ctx context.Context,
	pkScript []byte, amountSat int64) (string, error) {

	resp, err := r.daemon.SendOOR(
		ctx, &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_PkScript{
					PkScript: pkScript,
				},
				AmountSat: amountSat,
			},
		},
	)
	if err != nil {
		return "", fmt.Errorf("daemon SendOOR: %w", err)
	}

	return resp.GetSessionId(), nil
}

// SendOORWithCustomInputs sends an OOR with caller-specified
// inputs, enabling non-standard spend paths such as vHTLC claims.
//
// NOTE: This is part of the DaemonConn interface.
func (r *RPCDaemonConn) SendOORWithCustomInputs(ctx context.Context,
	recipientPkScript []byte, amountSat int64,
	inputs []CustomInput) (string, error) {

	rpcInputs := make(
		[]*daemonrpc.CustomOORInput, len(inputs),
	)
	for i, in := range inputs {
		rpcInputs[i] = &daemonrpc.CustomOORInput{
			Outpoint:           in.Outpoint,
			SpendWitnessScript: in.SpendWitnessScript,
			SpendControlBlock:  in.SpendControlBlock,
			ConditionWitness:   in.ConditionWitness,
			AmountSat:          in.AmountSat,
			PkScript:           in.PkScript,
		}
	}

	resp, err := r.daemon.SendOOR(
		ctx, &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_PkScript{
					PkScript: recipientPkScript,
				},
				AmountSat: amountSat,
			},
			CustomInputs: rpcInputs,
		},
	)
	if err != nil {
		return "", fmt.Errorf(
			"daemon SendOOR (custom): %w", err,
		)
	}

	return resp.GetSessionId(), nil
}

// GetIdentityPubkey returns the client's identity public key by
// calling the daemon's GetInfo RPC and parsing the identity_pubkey
// field.
//
// NOTE: This is part of the DaemonConn interface.
func (r *RPCDaemonConn) GetIdentityPubkey(
	ctx context.Context) (*btcec.PublicKey, error) {

	info, err := r.daemon.GetInfo(
		ctx, &daemonrpc.GetInfoRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("daemon GetInfo: %w", err)
	}

	pubBytes, err := hex.DecodeString(info.GetIdentityPubkey())
	if err != nil {
		return nil, fmt.Errorf(
			"decode identity pubkey hex: %w", err,
		)
	}

	key, err := btcec.ParsePubKey(pubBytes)
	if err != nil {
		return nil, fmt.Errorf(
			"parse identity pubkey: %w", err,
		)
	}

	return key, nil
}

// GetOperatorPubkey returns the Ark operator's public key by
// calling the ark service's GetInfo RPC.
//
// NOTE: This is part of the DaemonConn interface.
func (r *RPCDaemonConn) GetOperatorPubkey(
	ctx context.Context) (*btcec.PublicKey, error) {

	info, err := r.ark.GetInfo(
		ctx, &arkrpc.GetInfoRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("ark GetInfo: %w", err)
	}

	key, err := btcec.ParsePubKey(info.GetPubkey())
	if err != nil {
		return nil, fmt.Errorf(
			"parse operator pubkey: %w", err,
		)
	}

	return key, nil
}

// ListLiveVTXOs returns all VTXOs with LIVE status by calling the
// daemon's ListVTXOs RPC with the appropriate status filter.
//
// NOTE: This is part of the DaemonConn interface.
func (r *RPCDaemonConn) ListLiveVTXOs(
	ctx context.Context) ([]VTXOInfo, error) {

	resp, err := r.daemon.ListVTXOs(
		ctx, &daemonrpc.ListVTXOsRequest{
			StatusFilter: daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"daemon ListVTXOs: %w", err,
		)
	}

	vtxos := make([]VTXOInfo, len(resp.GetVtxos()))
	for i, v := range resp.GetVtxos() {
		pkScript, err := hex.DecodeString(v.GetPkScript())
		if err != nil {
			return nil, fmt.Errorf(
				"decode vtxo pkscript: %w", err,
			)
		}

		vtxos[i] = VTXOInfo{
			Outpoint:  v.GetOutpoint(),
			AmountSat: v.GetAmountSat(),
			PkScript:  pkScript,
		}
	}

	return vtxos, nil
}

// FindLiveVTXOByPkScript queries the authoritative indexer for a live VTXO
// matching the given pkScript.
func (r *RPCDaemonConn) FindLiveVTXOByPkScript(ctx context.Context,
	pkScript []byte) (*VTXOInfo, error) {

	resp, err := r.indexer.ListVTXOsByScripts(
		ctx, &arkrpc.ListVTXOsByScriptsRequest{
			Scripts: []*arkrpc.ScriptScope{{
				PkScript: pkScript,
			}},
			StatusFilter: []arkrpc.VTXOStatus{
				arkrpc.VTXOStatus_VTXO_STATUS_LIVE,
			},
			Limit: 1,
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"indexer ListVTXOsByScripts: %w", err,
		)
	}

	if len(resp.GetVtxos()) == 0 {
		return nil, nil
	}

	vtxo := resp.GetVtxos()[0]
	outpoint := vtxo.GetOutpoint()
	if outpoint == nil {
		return nil, fmt.Errorf("indexer vtxo missing outpoint")
	}

	txid, err := chainhash.NewHash(outpoint.GetTxid())
	if err != nil {
		return nil, fmt.Errorf("parse vtxo txid: %w", err)
	}

	return &VTXOInfo{
		Outpoint:  fmt.Sprintf("%s:%d", txid, outpoint.GetVout()),
		AmountSat: int64(vtxo.GetValueSat()),
		PkScript:  append([]byte(nil), vtxo.GetPkScript()...),
	}, nil
}

// NewOORReceiveScript allocates a fresh OOR receive script from the
// daemon.
func (c *RPCDaemonConn) NewOORReceiveScript(
	ctx context.Context) ([]byte, error) {

	resp, err := c.daemon.NewOORReceiveScript(
		ctx, &daemonrpc.NewOORReceiveScriptRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"NewOORReceiveScript RPC: %w", err,
		)
	}

	pkScript, err := hex.DecodeString(resp.GetPkScriptHex())
	if err != nil {
		return nil, fmt.Errorf(
			"decode pk_script hex: %w", err,
		)
	}

	return pkScript, nil
}

// Compile-time assertion that RPCDaemonConn implements DaemonConn.
var _ DaemonConn = (*RPCDaemonConn)(nil)
