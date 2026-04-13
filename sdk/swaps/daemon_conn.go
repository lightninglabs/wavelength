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
	daemon daemonrpc.DaemonServiceClient
	ark    arkrpc.ArkServiceClient
}

// NewRPCDaemonConn creates a new DaemonConn backed by gRPC clients for the
// daemon and ark services.
func NewRPCDaemonConn(daemon daemonrpc.DaemonServiceClient,
	ark arkrpc.ArkServiceClient) *RPCDaemonConn {

	return &RPCDaemonConn{
		daemon: daemon,
		ark:    ark,
	}
}

// SendOORWithPolicy sends an OOR transfer to the given semantic policy
// template.
//
// NOTE: This is part of the DaemonConn interface.
func (r *RPCDaemonConn) SendOORWithPolicy(ctx context.Context,
	amountSat int64, recipientPolicyTemplate []byte) (string, error) {

	resp, err := r.daemon.SendOOR(
		ctx, &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_PolicyTemplate{
					PolicyTemplate: recipientPolicyTemplate,
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
	recipientPubKey []byte, amountSat int64,
	inputs []CustomInput) (string, error) {

	rpcInputs := make(
		[]*daemonrpc.CustomOORInput, len(inputs),
	)
	for i, in := range inputs {
		rpcInputs[i] = &daemonrpc.CustomOORInput{
			Outpoint:           in.Outpoint,
			VtxoPolicyTemplate: in.VTXOPolicyTemplate,
			SpendPath:          in.SpendPath,
			AmountSat:          in.AmountSat,
			PkScript:           in.PkScript,
		}
	}

	resp, err := r.daemon.SendOOR(
		ctx, &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: recipientPubKey,
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

	return r.listVTXOsByStatus(
		ctx, daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
	)
}

// ListSpentVTXOs returns all VTXOs with SPENT status from the local daemon.
//
// NOTE: This is part of the DaemonConn interface.
func (r *RPCDaemonConn) ListSpentVTXOs(
	ctx context.Context) ([]VTXOInfo, error) {

	return r.listVTXOsByStatus(
		ctx, daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)
}

// listVTXOsByStatus queries one daemon VTXO status and translates the result
// into the swap SDK's local VTXO shape.
func (r *RPCDaemonConn) listVTXOsByStatus(ctx context.Context,
	status daemonrpc.VTXOStatus) ([]VTXOInfo, error) {

	resp, err := r.daemon.ListVTXOs(
		ctx, &daemonrpc.ListVTXOsRequest{
			StatusFilter: status,
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

		checkpoints := make(
			[][]byte, 0, len(v.GetOorFinalCheckpointPsbts()),
		)
		for j := range v.GetOorFinalCheckpointPsbts() {
			checkpoints = append(checkpoints, append(
				[]byte(nil),
				v.GetOorFinalCheckpointPsbts()[j]...,
			))
		}

		vtxos[i] = VTXOInfo{
			Outpoint:             v.GetOutpoint(),
			AmountSat:            v.GetAmountSat(),
			PkScript:             pkScript,
			FinalCheckpointPSBTs: checkpoints,
			SpentByTxid:          v.GetSpentByTxid(),
		}
	}

	return vtxos, nil
}

// FindLiveVTXOByPkScript queries the authoritative indexer for a live VTXO
// matching the given pkScript.
func (r *RPCDaemonConn) FindLiveVTXOByPkScript(ctx context.Context,
	pkScript []byte) (*VTXOInfo, error) {

	resp, err := r.daemon.GetIndexedVTXOByPkScript(
		ctx, &daemonrpc.GetIndexedVTXOByPkScriptRequest{
			PkScript: pkScript,
			StatusFilter: []daemonrpc.VTXOStatus{
				daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"daemon GetIndexedVTXOByPkScript: %w", err,
		)
	}

	vtxo := resp.GetVtxo()
	if vtxo == nil {
		return nil, nil
	}

	pkScriptBytes, err := hex.DecodeString(vtxo.GetPkScript())
	if err != nil {
		return nil, fmt.Errorf("decode vtxo pkScript: %w", err)
	}

	return &VTXOInfo{
		Outpoint:    vtxo.GetOutpoint(),
		AmountSat:   vtxo.GetAmountSat(),
		PkScript:    pkScriptBytes,
		SpentByTxid: vtxo.GetSpentByTxid(),
	}, nil
}

// FindSpentVTXOByPkScript queries the authoritative indexer for a spent VTXO
// matching the given pkScript.
func (r *RPCDaemonConn) FindSpentVTXOByPkScript(ctx context.Context,
	pkScript []byte) (*VTXOInfo, error) {

	resp, err := r.daemon.GetIndexedVTXOByPkScript(
		ctx, &daemonrpc.GetIndexedVTXOByPkScriptRequest{
			PkScript: pkScript,
			StatusFilter: []daemonrpc.VTXOStatus{
				daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"daemon GetIndexedVTXOByPkScript: %w", err,
		)
	}

	vtxo := resp.GetVtxo()
	if vtxo == nil {
		return nil, nil
	}

	checkpoints := make(
		[][]byte, 0, len(vtxo.GetOorFinalCheckpointPsbts()),
	)
	for j := range vtxo.GetOorFinalCheckpointPsbts() {
		checkpoints = append(checkpoints, append(
			[]byte(nil), vtxo.GetOorFinalCheckpointPsbts()[j]...,
		))
	}

	pkScriptBytes, err := hex.DecodeString(vtxo.GetPkScript())
	if err != nil {
		return nil, fmt.Errorf("decode vtxo pkScript: %w", err)
	}

	return &VTXOInfo{
		Outpoint:             vtxo.GetOutpoint(),
		AmountSat:            vtxo.GetAmountSat(),
		PkScript:             pkScriptBytes,
		FinalCheckpointPSBTs: checkpoints,
		SpentByTxid:          vtxo.GetSpentByTxid(),
	}, nil
}

// GetIndexedOORSessionByTxid queries the authoritative indexer for one OOR
// session's Ark package and finalized checkpoints.
func (r *RPCDaemonConn) GetIndexedOORSessionByTxid(ctx context.Context,
	pkScript []byte, sessionTxid string) (*OORPackageInfo, error) {

	sessionHash, err := parseSessionTxid(sessionTxid)
	if err != nil {
		return nil, fmt.Errorf("decode session txid: %w", err)
	}

	resp, err := r.daemon.GetIndexedOORSessionByTxid(
		ctx, &daemonrpc.GetIndexedOORSessionByTxidRequest{
			PkScript:    pkScript,
			SessionTxid: sessionHash,
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"daemon GetIndexedOORSessionByTxid: %w", err,
		)
	}

	checkpoints := make(
		[][]byte, 0, len(resp.GetCheckpointPsbts()),
	)
	for i := range resp.GetCheckpointPsbts() {
		checkpoints = append(checkpoints, append(
			[]byte(nil), resp.GetCheckpointPsbts()[i]...,
		))
	}

	return &OORPackageInfo{
		ArkPSBT:              append([]byte(nil), resp.GetArkPsbt()...),
		FinalCheckpointPSBTs: checkpoints,
	}, nil
}

// parseSessionTxid converts a human-readable txid string into the raw
// chainhash byte order expected by the daemon/indexer RPC boundary.
func parseSessionTxid(sessionTxid string) ([]byte, error) {
	hash, err := chainhash.NewHashFromStr(sessionTxid)
	if err != nil {
		return nil, err
	}

	return append([]byte(nil), hash[:]...), nil
}

// NewOORReceiveScript allocates a fresh OOR receive script from the
// daemon.
func (r *RPCDaemonConn) NewOORReceiveScript(
	ctx context.Context) (*OORReceiveInfo, error) {

	resp, err := r.daemon.NewOORReceiveScript(
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

	pubKey, err := hex.DecodeString(resp.GetPubkeyXonlyHex())
	if err != nil {
		return nil, fmt.Errorf(
			"decode pubkey_xonly_hex: %w", err,
		)
	}

	return &OORReceiveInfo{
		PkScript: pkScript,
		PubKey:   pubKey,
	}, nil
}

// Compile-time assertion that RPCDaemonConn implements DaemonConn.
var _ DaemonConn = (*RPCDaemonConn)(nil)
