package waved

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/clock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultOORReceiveScriptLabel = "oor receive"
	defaultOORChangeScriptLabel  = "oor change"
)

// NewReceiveScript allocates a fresh wallet key, registers the matching
// taproot receive script with the indexer, and returns the script details
// needed to hand this one-shot destination to a sender.
func (r *RPCServer) NewReceiveScript(ctx context.Context,
	req *waverpc.NewReceiveScriptRequest) (
	*waverpc.NewReceiveScriptResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if req == nil {
		req = &waverpc.NewReceiveScriptRequest{}
	}

	if r.server.indexer == nil {
		return nil, status.Errorf(codes.Internal, "indexer client "+
			"not initialized")
	}

	if r.server.db == nil {
		return nil, status.Errorf(codes.Internal, "database not "+
			"initialized")
	}

	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to fetch "+
			"operator terms: %v", err)
	}

	store, err := r.newOORReceiveScriptStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to "+
			"initialize OOR receive-script store: %v", err) //nolint:ll
	}

	deriveNextKey, signerFactory, err := r.oorReceiveKeyOps()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to "+
			"initialize OOR receive key ops: %v", err)
	}

	label := req.Label
	if label == "" {
		label = defaultOORReceiveScriptLabel
	}

	keyDesc, pkScript, err := CreateOORReceiveScript(
		ctx, r.server.indexer, store, deriveNextKey, signerFactory,
		terms.PubKey, terms.VTXOExitDelay, label,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to create "+
			"OOR receive script: %v", err)
	}

	if keyDesc == nil || keyDesc.PubKey == nil {
		return nil, status.Errorf(codes.Internal, "missing receive "+
			"key descriptor")
	}

	return &waverpc.NewReceiveScriptResponse{
		PkScriptHex: hex.EncodeToString(pkScript),
		PubkeyXonlyHex: hex.EncodeToString(
			schnorr.SerializePubKey(keyDesc.PubKey),
		),
		KeyFamily: uint32(keyDesc.KeyLocator.Family),
		KeyIndex:  keyDesc.KeyLocator.Index,
		Label:     label,
	}, nil
}

// newOORReceiveScriptStore returns the artifact store used to persist owned
// receive-script metadata for later proof lookup and incoming resolution.
func (r *RPCServer) newOORReceiveScriptStore() (*db.OORArtifactPersistenceStore,
	error) {

	if r.server.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	dbStore := db.NewStore(
		r.server.db.DB, r.server.db.Queries, r.server.db.Backend(),
		r.server.subLogger(db.Subsystem),
	)

	return dbStore.NewOORArtifactStore(clock.NewDefaultClock()), nil
}

// oorReceiveKeyOps returns the fresh-key derivation function and signer
// factory for the active wallet backend.
func (r *RPCServer) oorReceiveKeyOps() (DeriveDefaultOORReceiveKeyFunc,
	OORReceiveScriptSignerFactory, error) {

	return r.server.indexerProofNextKeyOps()
}
