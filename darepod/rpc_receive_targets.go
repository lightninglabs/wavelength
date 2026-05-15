package darepod

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ListReceiveScripts returns active receive targets registered to this
// wallet's mailbox principal.
func (r *RPCServer) ListReceiveScripts(ctx context.Context,
	req *daemonrpc.ListReceiveScriptsRequest) (
	*daemonrpc.ListReceiveScriptsResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if r.server.indexer == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer client not initialized",
		)
	}

	resp, err := r.server.indexer.ListMyReceiveScripts(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "unable to list "+
			"receive scripts: %v", err)
	}

	// Receive-script registrations are expected to remain small for the
	// local-wallet CLI flows this RPC currently serves.
	// TODO(ListReceiveScripts pagination): Add limit/cursor support if
	// these registrations become long-lived enough to grow without bound.
	targets := make([]*daemonrpc.ReceiveTarget, 0, len(resp.GetScripts()))
	for _, script := range resp.GetScripts() {
		target, err := receiveTargetFromRegisteredScript(
			script, r.server.chainParams,
		)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "unable to "+
				"format receive target: %v", err)
		}

		targets = append(targets, target)
	}

	return &daemonrpc.ListReceiveScriptsResponse{
		Targets: targets,
	}, nil
}

// receiveTargetFromRegisteredScript converts an indexer receive-script
// registration into the daemon's user-facing receive target type.
func receiveTargetFromRegisteredScript(script *arkrpc.RegisteredReceiveScript,
	params *chaincfg.Params) (*daemonrpc.ReceiveTarget, error) {

	if script == nil {
		return nil, fmt.Errorf("receive script is nil")
	}

	address, err := addressFromTaprootPkScript(script.GetPkScript(), params)
	if err != nil {
		return nil, err
	}

	return &daemonrpc.ReceiveTarget{
		Address:        address,
		PkScriptHex:    hex.EncodeToString(script.GetPkScript()),
		Label:          script.GetLabel(),
		ExpiresAtUnixS: script.GetExpiresAtUnixS(),
	}, nil
}

// addressFromTaprootPkScript returns the bech32m address committed to by a
// native v1 taproot pkScript.
func addressFromTaprootPkScript(pkScript []byte,
	params *chaincfg.Params) (string, error) {

	if params == nil {
		return "", fmt.Errorf("chain params are nil")
	}

	if !txscript.IsPayToTaproot(pkScript) {
		return "", fmt.Errorf("pk_script is not a native taproot " +
			"output script")
	}

	parsedScript, err := txscript.ParsePkScript(pkScript)
	if err != nil {
		return "", fmt.Errorf("parse taproot pkScript: %w", err)
	}

	addr, err := parsedScript.Address(params)
	if err != nil {
		return "", fmt.Errorf("derive taproot address: %w", err)
	}

	return addr.EncodeAddress(), nil
}
