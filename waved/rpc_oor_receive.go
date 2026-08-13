package waved

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultOORReceiveScriptLabel = "oor receive"
	defaultOORChangeScriptLabel  = "oor change"
)

// NewReceiveScript registers a wallet-owned taproot receive script and returns
// the details needed to hand the destination to a sender. Ordinary callers get
// a fresh key; protocol runtimes can explicitly register the durable identity
// key when the destination must survive independent process restarts.
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

	var keyDesc *keychain.KeyDescriptor
	var pkScript []byte
	if req.GetIdentityKey() {
		identityKey := r.server.clientKeyDesc
		if identityKey.PubKey == nil {
			return nil, status.Errorf(codes.Internal, "missing "+
				"daemon identity key")
		}

		pkScript, err = RegisterOwnedOORReceiveScript(
			ctx, r.server.indexer, store, identityKey,
			signerFactory, terms.PubKey, terms.VTXOExitDelay, label,
		)
		keyDesc = &identityKey
	} else {
		keyDesc, pkScript, err = CreateOORReceiveScript(
			ctx, r.server.indexer, store, deriveNextKey,
			signerFactory, terms.PubKey, terms.VTXOExitDelay, label,
		)
	}
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
