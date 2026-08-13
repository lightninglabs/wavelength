package waved

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/indexer"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/waverpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultOORReceiveScriptLabel = "oor receive"
	defaultOORChangeScriptLabel  = "oor change"
)

// receiveScriptLock serializes callers for one receive-script idempotency key.
type receiveScriptLock struct {
	token chan struct{}
	refs  int
}

// receiveScriptRegistrationCompleter persists acknowledged remote
// registration for one idempotent allocation.
type receiveScriptRegistrationCompleter interface {
	// MarkOwnedReceiveScriptRegistered records acknowledged
	// remote registration for one allocation and stable RPC key.
	MarkOwnedReceiveScriptRegistered(ctx context.Context, idempotencyKey,
		registrationRPCKey string) error
}

// NewReceiveScript allocates and registers a taproot receive script, or returns
// the exact existing allocation for an idempotent retry.
func (r *RPCServer) NewReceiveScript(ctx context.Context,
	req *waverpc.NewReceiveScriptRequest) (
	*waverpc.NewReceiveScriptResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if req == nil {
		req = &waverpc.NewReceiveScriptRequest{}
	}

	label := req.Label
	if label == "" {
		label = defaultOORReceiveScriptLabel
	}

	if req.IdempotencyKey != "" {
		if len(req.IdempotencyKey) >
			db.MaxOwnedReceiveScriptIdempotencyKeyBytes {
			return nil, status.Errorf(codes.InvalidArgument,
				"idempotency_key exceeds %d bytes",
				db.MaxOwnedReceiveScriptIdempotencyKeyBytes)
		}
		hasControl := strings.IndexFunc(
			req.IdempotencyKey, unicode.IsControl,
		) >= 0
		if hasControl {
			return nil, status.Error(
				codes.InvalidArgument,
				"idempotency_key contains control characters",
			)
		}

		if r.server.db == nil {
			return nil, status.Errorf(codes.Internal, "database "+
				"not initialized")
		}

		release, err := r.acquireReceiveScriptLock(
			ctx, req.IdempotencyKey,
		)
		if err != nil {
			return nil, status.FromContextError(err).Err()
		}
		defer release()

		return r.newIdempotentReceiveScript(
			ctx, req.IdempotencyKey, label,
		)
	}

	if r.server.db == nil {
		return nil, status.Errorf(codes.Internal, "database not "+
			"initialized")
	}

	if r.server.indexer == nil {
		return nil, status.Errorf(codes.Internal, "indexer client "+
			"not initialized")
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

	expiresAt := r.server.clk.Now().Add(
		defaultOORRegistrationTTL,
	)
	keyDesc, pkScript, err := CreateOORReceiveScriptWithExpiry(
		ctx, r.server.indexer, store, deriveNextKey, signerFactory,
		terms.PubKey, terms.VTXOExitDelay, label, expiresAt,
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
		KeyFamily:      uint32(keyDesc.KeyLocator.Family),
		KeyIndex:       keyDesc.KeyLocator.Index,
		Label:          label,
		ExpiresAtUnixS: uint64(expiresAt.Unix()),
	}, nil
}

// newIdempotentReceiveScript admits or resumes one retry-safe allocation.
func (r *RPCServer) newIdempotentReceiveScript(ctx context.Context,
	idempotencyKey, label string) (*waverpc.NewReceiveScriptResponse,
	error) {

	store, err := r.newOORReceiveScriptStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to "+
			"initialize OOR receive-script store: %v", err)
	}

	rec, err := store.LookupOwnedReceiveScriptByIdempotencyKey(
		ctx, idempotencyKey,
	)
	switch {
	case err == nil:
		if rec.RegistrationLabel != label {
			return nil, status.Error(
				codes.InvalidArgument, "idempotency_key "+
					"was already used with a different "+
					"label",
			)
		}

	case !errors.Is(err, sql.ErrNoRows):
		return nil, status.Errorf(codes.Internal, "unable to look up "+
			"OOR receive script: %v", err)

	default:
		rec, err = r.admitIdempotentReceiveScript(
			ctx, store, idempotencyKey, label,
		)
		if err != nil {
			return nil, err
		}
	}

	now := r.server.clk.Now()
	if !rec.RegistrationExpiresAt.After(now) {
		oldExpiresAt := rec.RegistrationExpiresAt
		nextRPCKey, err := mailboxrpc.NewIdempotencyKey()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "unable to "+
				"allocate renewal request key: %v", err)
		}

		rec, err = store.RenewOwnedReceiveScriptRegistration(
			ctx, *rec, now.Add(defaultOORRegistrationTTL),
			nextRPCKey,
		)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "unable to "+
				"renew OOR receive script registration: %v",
				err)
		}

		r.server.log.DebugS(ctx, "Renewed expired OOR receive-script "+
			"registration window",
			slog.String("idempotency_key", idempotencyKey),
			slog.Int64("old_expires_at_unix_s", oldExpiresAt.Unix()),
			slog.Int64(
				"new_expires_at_unix_s",
				rec.RegistrationExpiresAt.Unix(),
			),
		)
	}

	if rec.RegistrationCompletedAt.IsSome() {
		return newReceiveScriptResponse(rec), nil
	}

	if r.server.indexer == nil {
		return nil, status.Errorf(codes.Internal, "indexer client "+
			"not initialized")
	}

	_, signerFactory, err := r.oorReceiveKeyOps()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to "+
			"initialize OOR receive key ops: %v", err)
	}

	registerClient := r.server.indexer.WithSigner(
		signerFactory(rec.ClientKey),
	)
	err = registerIdempotentReceiveScript(
		ctx, registerClient, store, rec,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to register "+
			"OOR receive script: %v", err)
	}

	completedAt := r.server.clk.Now()
	rec.RegistrationCompletedAt = fn.Some(completedAt)

	return newReceiveScriptResponse(rec), nil
}

// registerIdempotentReceiveScript resumes a pending remote registration with
// its durable mailbox request key, then records acknowledged completion.
func registerIdempotentReceiveScript(ctx context.Context,
	registerClient *indexer.Client,
	store receiveScriptRegistrationCompleter,
	rec *db.OwnedReceiveScriptRecord) error {

	if rec.RegistrationCompletedAt.IsSome() {
		return nil
	}

	if registerClient == nil {
		return fmt.Errorf("indexer client must be provided")
	}

	if store == nil {
		return fmt.Errorf("registration store must be provided")
	}

	if rec.RegistrationRPCKey == "" {
		return fmt.Errorf("registration RPC key must be provided")
	}

	_, err := registerClient.RegisterReceiveScriptTaproot(
		ctx, rec.PkScript, rec.RegistrationExpiresAt,
		rec.RegistrationLabel, mailboxrpc.RPCOptions{
			IdempotencyKey: rec.RegistrationRPCKey,
		},
	)
	if err != nil {
		return err
	}

	return store.MarkOwnedReceiveScriptRegistered(
		ctx, rec.IdempotencyKey, rec.RegistrationRPCKey,
	)
}

// admitIdempotentReceiveScript derives a candidate only after lookup proves no
// durable allocation exists, then lets the serializable store select a winner.
func (r *RPCServer) admitIdempotentReceiveScript(ctx context.Context,
	store *db.OORArtifactPersistenceStore, idempotencyKey, label string) (
	*db.OwnedReceiveScriptRecord, error) {

	if r.server.indexer == nil {
		return nil, status.Errorf(codes.Internal, "indexer client "+
			"not initialized")
	}

	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to fetch "+
			"operator terms: %v", err)
	}

	deriveNextKey, _, err := r.oorReceiveKeyOps()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to "+
			"initialize OOR receive key ops: %v", err)
	}

	keyDesc, err := deriveNextKey(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to derive "+
			"OOR receive key: %v", err)
	}

	if keyDesc == nil || keyDesc.PubKey == nil {
		return nil, status.Error(
			codes.Internal, "missing receive key descriptor",
		)
	}

	pkScript, err := BuildPubKeyVTXOReceiveScript(
		keyDesc.PubKey, terms.PubKey, terms.VTXOExitDelay,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to build "+
			"OOR receive script: %v", err)
	}

	registrationRPCKey, err := mailboxrpc.NewIdempotencyKey()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to allocate "+
			"registration request key: %v", err)
	}

	now := r.server.clk.Now()
	candidate := db.OwnedReceiveScriptRecord{
		PkScript:          pkScript,
		ClientKey:         *keyDesc,
		OperatorPubKey:    terms.PubKey,
		ExitDelay:         int64(terms.VTXOExitDelay),
		Source:            db.OwnedReceiveScriptSourceWallet,
		CreatedAt:         now,
		LastUsedAt:        fn.None[time.Time](),
		IdempotencyKey:    idempotencyKey,
		RegistrationLabel: label,
		RegistrationExpiresAt: now.Add(
			defaultOORRegistrationTTL,
		),
		RegistrationRPCKey: registrationRPCKey,
	}

	rec, _, err := store.AdmitIdempotentOwnedReceiveScript(
		ctx, candidate,
	)
	if errors.Is(err, db.ErrOwnedReceiveScriptReplayMismatch) {
		return nil, status.Error(
			codes.InvalidArgument, "idempotency_key was "+
				"already used with a different label",
		)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to admit "+
			"OOR receive script: %v", err)
	}

	return rec, nil
}

// newReceiveScriptResponse reconstructs the public response from durable
// allocation data.
func newReceiveScriptResponse(
	rec *db.OwnedReceiveScriptRecord) *waverpc.NewReceiveScriptResponse {

	return &waverpc.NewReceiveScriptResponse{
		PkScriptHex: hex.EncodeToString(rec.PkScript),
		PubkeyXonlyHex: hex.EncodeToString(
			schnorr.SerializePubKey(rec.ClientKey.PubKey),
		),
		KeyFamily: uint32(rec.ClientKey.KeyLocator.Family),
		KeyIndex:  rec.ClientKey.KeyLocator.Index,
		Label:     rec.RegistrationLabel,
		ExpiresAtUnixS: uint64(
			rec.RegistrationExpiresAt.Unix(),
		),
	}
}

// acquireReceiveScriptLock locks one idempotency key and returns its release
// function. A canceled waiter leaves promptly, and the registry entry is
// removed after the final holder or waiter exits.
func (r *RPCServer) acquireReceiveScriptLock(ctx context.Context, key string) (
	func(), error) {

	r.receiveScriptLocksMu.Lock()
	if r.receiveScriptLocks == nil {
		r.receiveScriptLocks = make(map[string]*receiveScriptLock)
	}

	entry, ok := r.receiveScriptLocks[key]
	if !ok {
		entry = &receiveScriptLock{
			token: make(chan struct{}, 1),
		}
		entry.token <- struct{}{}
		r.receiveScriptLocks[key] = entry
	}
	entry.refs++
	r.receiveScriptLocksMu.Unlock()

	releaseRef := func() {
		r.receiveScriptLocksMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(r.receiveScriptLocks, key)
		}
		r.receiveScriptLocksMu.Unlock()
	}

	select {
	case <-ctx.Done():
		releaseRef()

		return nil, ctx.Err()

	case <-entry.token:
	}

	return func() {
		entry.token <- struct{}{}
		releaseRef()
	}, nil
}

// newOORReceiveScriptStore returns the artifact store used to persist owned
// receive-script metadata for later proof lookup and incoming resolution.
func (r *RPCServer) newOORReceiveScriptStore() (*db.OORArtifactPersistenceStore,
	error) {

	if r.server.oorArtifactStore == nil {
		return nil, fmt.Errorf("artifact store not initialized")
	}

	dbStore := db.NewStore(
		r.server.db.DB, r.server.db.Queries, r.server.db.Backend(),
		r.server.subLogger(db.Subsystem),
	)

	if r.server.clk == nil {
		return nil, fmt.Errorf("server clock not initialized")
	}

	return dbStore.NewOORArtifactStore(r.server.clk), nil
}

// oorReceiveKeyOps returns the fresh-key derivation function and signer
// factory for the active wallet backend.
func (r *RPCServer) oorReceiveKeyOps() (DeriveDefaultOORReceiveKeyFunc,
	OORReceiveScriptSignerFactory, error) {

	return r.server.indexerProofNextKeyOps()
}
