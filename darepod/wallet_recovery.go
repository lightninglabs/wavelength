package darepod

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	libtypes "github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo-client/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	recoveryVTXOPageSize = 128
	recoveryOORPageSize  = 128

	recoveryIndexerRetryAttempts = 8
	recoveryIndexerRetryDelay    = 150 * time.Millisecond
)

type walletRecoveryResult struct {
	BoardingAddresses uint32
	BoardingUTXOs     uint32
	VTXOs             uint32
	OORReceiveScripts uint32
	OOREvents         uint32
}

// retryRecoveryIndexerRPC retries recovery-local indexer calls that hit the
// operator's per-client query limiter.
func retryRecoveryIndexerRPC(ctx context.Context, call func() error) error {
	var err error
	for attempt := 0; attempt < recoveryIndexerRetryAttempts; attempt++ {
		err = call()
		if status.Code(err) != codes.ResourceExhausted {
			return err
		}

		if attempt == recoveryIndexerRetryAttempts-1 {
			break
		}

		timer := time.NewTimer(recoveryIndexerRetryDelay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}

			return fmt.Errorf("wait for indexer retry: %w",
				ctx.Err())
		}
	}

	return err
}

func (r walletRecoveryResult) apply(resp *daemonrpc.InitWalletResponse) {
	resp.RecoveryRan = true
	resp.RecoveredBoardingAddresses = r.BoardingAddresses
	resp.RecoveredBoardingUtxos = r.BoardingUTXOs
	resp.RecoveredVtxos = r.VTXOs
	resp.RecoveredOorReceiveScripts = r.OORReceiveScripts
	resp.RecoveredOorEvents = r.OOREvents
}

func (r *RPCServer) recoverWalletState(ctx context.Context, window uint32) (
	*walletRecoveryResult, error) {

	if window == 0 {
		window = r.server.cfg.Wallet.RecoveryWindow
	}
	if window == 0 {
		window = DefaultRecoveryWindow
	}

	if r.server.proofKeyBackend == nil {
		return nil, fmt.Errorf("wallet backend not initialized")
	}

	// InitWallet stores the seed before this point, which lets the daemon
	// start wallet-backed services on a separate goroutine. Recovery needs
	// those services, so wait for that async startup before scanning.
	select {
	case <-r.server.DaemonReady():
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for wallet-ready services: %w",
			ctx.Err())
	}

	if r.server.indexer == nil {
		return nil, fmt.Errorf("indexer client not initialized")
	}

	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, err
	}

	var result walletRecoveryResult
	if err := r.recoverBoardingKeys(
		ctx, terms, window, &result,
	); err != nil {
		return nil, fmt.Errorf("recover boarding keys: %w", err)
	}
	if err := r.recoverIndexedVTXOs(
		ctx, terms, window, &result,
	); err != nil {
		return nil, fmt.Errorf("recover indexed VTXOs: %w", err)
	}
	if err := r.recoverOORReceiveScripts(
		ctx, terms, window, &result,
	); err != nil {
		return nil, fmt.Errorf("recover OOR receive scripts: %w", err)
	}

	return &result, nil
}

func (r *RPCServer) recoverBoardingKeys(ctx context.Context,
	terms *libtypes.OperatorTerms, window uint32,
	result *walletRecoveryResult) error {

	backend, err := r.server.recoveryBoardingBackend()
	if err != nil {
		return err
	}

	store := r.server.newBoardingStore()
	recoveredScripts := make(map[string]struct{}, window)

	for i := uint32(0); i < window; i++ {
		keyDesc, err := r.server.proofKeyBackend.DeriveKey(
			ctx, keychain.KeyLocator{
				Family: wallet.BoardingKeyFamily,
				Index:  i,
			},
		)
		if err != nil {
			return fmt.Errorf("derive boarding key %d: %w", i, err)
		}

		tapscript, err := arkscript.VTXOTapScript(
			keyDesc.PubKey, terms.PubKey, terms.BoardingExitDelay,
		)
		if err != nil {
			return fmt.Errorf("build boarding tapscript %d: %w", i,
				err)
		}

		addr, err := backend.ImportTaprootScript(ctx, tapscript)
		if err != nil {
			return fmt.Errorf("import boarding script %d: %w", i,
				err)
		}

		boardingAddr := &wallet.BoardingAddress{
			Address:     addr,
			Tapscript:   tapscript,
			KeyDesc:     *keyDesc,
			OperatorKey: terms.PubKey,
			ExitDelay:   terms.BoardingExitDelay,
		}
		if err := store.InsertBoardingAddress(
			ctx, boardingAddr,
		); err != nil {
			return fmt.Errorf("persist boarding address %d: %w", i,
				err)
		}

		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return fmt.Errorf("derive boarding pk script %d: %w", i,
				err)
		}
		recoveredScripts[string(pkScript)] = struct{}{}
		result.BoardingAddresses++
	}

	utxos, err := backend.ListUnspent(
		ctx, wallet.MinBoardingConfs, wallet.MaxConfsForListUnspent,
	)
	if err != nil {
		return fmt.Errorf("list boarding UTXOs: %w", err)
	}

	for _, utxo := range utxos {
		if _, ok := recoveredScripts[string(utxo.PkScript)]; ok {
			result.BoardingUTXOs++
		}
	}

	return nil
}

func (s *Server) recoveryBoardingBackend() (wallet.BoardingBackend, error) {
	switch s.cfg.Wallet.Type {
	case WalletTypeLwwallet:
		if !s.lwWallet.IsSome() {
			return nil, fmt.Errorf("lwwallet not initialized")
		}

		return s.lwWallet.UnsafeFromSome().BoardingBackend(), nil

	case WalletTypeBtcwallet:
		if !s.btcwWallet.IsSome() {
			return nil, fmt.Errorf("btcwallet not initialized")
		}

		return s.btcwWallet.UnsafeFromSome().BoardingBackend(), nil

	default:
		return nil, fmt.Errorf("wallet type %s does not support seed "+
			"recovery", s.cfg.Wallet.Type)
	}
}

func (r *RPCServer) recoverIndexedVTXOs(ctx context.Context,
	terms *libtypes.OperatorTerms, window uint32,
	result *walletRecoveryResult) error {

	for i := uint32(0); i < window; i++ {
		keyDesc, err := r.server.proofKeyBackend.DeriveKey(
			ctx, keychain.KeyLocator{
				Family: libtypes.VTXOOwnerKeyFamily,
				Index:  i,
			},
		)
		if err != nil {
			return fmt.Errorf("derive VTXO key %d: %w", i, err)
		}

		pkScript, err := BuildPubKeyVTXOReceiveScript(
			keyDesc.PubKey, terms.PubKey, terms.VTXOExitDelay,
		)
		if err != nil {
			return fmt.Errorf("build VTXO script %d: %w", i, err)
		}

		idx := r.server.indexer.WithSigner(
			r.server.proofKeyBackend.ProofSigner(*keyDesc),
		)
		expiresAt := r.server.clk.Now().Add(
			defaultOORReceiveScriptRegistrationTTL,
		)
		err = retryRecoveryIndexerRPC(ctx, func() error {
			_, err := idx.RegisterReceiveScriptTaproot(
				ctx, pkScript, expiresAt, "seed-recovery-vtxo",
			)

			return err
		})
		if err != nil && status.Code(err) != codes.AlreadyExists {
			return fmt.Errorf("register VTXO script %d: %w", i, err)
		}

		cursor := []byte(nil)
		for {
			var resp *arkrpc.ListVTXOsByScriptsResponse
			err := retryRecoveryIndexerRPC(ctx, func() error {
				var err error
				resp, err = idx.ListVTXOsByScriptsTaproot(
					ctx,
					[]indexer.TaprootScriptScope{{
						PkScript: pkScript,
					}},
					cursor, recoveryVTXOPageSize,
					nil,
				)

				return err
			})
			if err != nil {
				return fmt.Errorf("list VTXOs for key %d: %w",
					i, err)
			}

			vtxos := vtxo.FlattenListVTXOsByScriptsResponse(
				resp,
			)
			for _, indexed := range vtxos {
				indexedScript := indexed.GetPkScript()
				if !bytes.Equal(indexedScript, pkScript) {
					continue
				}

				desc, ok, err := recoveryDescriptorFromIndexer(
					indexed, *keyDesc, terms,
				)
				if err != nil {
					return fmt.Errorf("convert VTXO for "+
						"key %d: %w", i, err)
				}
				if !ok {
					continue
				}

				saved, err := r.saveRecoveredVTXO(ctx, desc)
				if err != nil {
					return err
				}
				if saved {
					result.VTXOs++
				}
			}

			cursor = resp.GetNextCursor()
			if len(cursor) == 0 {
				break
			}
		}
	}

	return nil
}

func recoveryDescriptorFromIndexer(indexed *arkrpc.VTXO,
	keyDesc keychain.KeyDescriptor, terms *libtypes.OperatorTerms) (
	*vtxo.Descriptor, bool, error) {

	status, ok := recoveryVTXOStatus(indexed.GetStatus())
	if !ok {
		return nil, false, nil
	}

	outpoint, err := recoveryOutpoint(indexed.GetOutpoint())
	if err != nil {
		return nil, false, err
	}

	operatorKey := terms.PubKey
	if len(indexed.GetOperatorPubkey()) > 0 {
		operatorKey, err = btcec.ParsePubKey(
			indexed.GetOperatorPubkey(),
		)
		if err != nil {
			return nil, false, fmt.Errorf("parse operator key: %w",
				err)
		}
	}

	exitDelay := indexed.GetRelativeExpiry()
	if exitDelay == 0 {
		exitDelay = terms.VTXOExitDelay
	}

	tapscript, err := arkscript.VTXOTapScript(
		keyDesc.PubKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, false, fmt.Errorf("build tapscript: %w", err)
	}

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		keyDesc.PubKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, false, fmt.Errorf("encode policy: %w", err)
	}

	ancestry, err := vtxo.AncestryFromRPC(indexed.GetAncestryPaths())
	if err != nil {
		return nil, false, fmt.Errorf("convert ancestry: %w", err)
	}

	commitmentTxID, err := chainhash.NewHash(
		indexed.GetCommitmentTxid(),
	)
	if err != nil {
		return nil, false, fmt.Errorf("parse commitment txid: %w", err)
	}

	return &vtxo.Descriptor{
		Outpoint:       outpoint,
		Amount:         btcutil.Amount(indexed.GetValueSat()),
		PolicyTemplate: policyTemplate,
		PkScript:       append([]byte(nil), indexed.GetPkScript()...),
		ClientKey:      keyDesc,
		OperatorKey:    operatorKey,
		TapScript:      tapscript,
		Ancestry:       ancestry,
		RoundID:        indexed.GetRoundId(),
		CommitmentTxID: *commitmentTxID,
		BatchExpiry:    indexed.GetBatchExpiryHeight(),
		RelativeExpiry: exitDelay,
		ChainDepth:     int(indexed.GetChainDepth()),
		CreatedHeight:  indexed.GetCreatedHeight(),
		Status:         status,
	}, true, nil
}

func recoveryOutpoint(op *arkrpc.OutPoint) (wire.OutPoint, error) {
	if op == nil {
		return wire.OutPoint{}, fmt.Errorf("outpoint missing")
	}

	txid, err := chainhash.NewHash(op.GetTxid())
	if err != nil {
		return wire.OutPoint{}, fmt.Errorf("parse outpoint txid: %w",
			err)
	}

	return wire.OutPoint{
		Hash:  *txid,
		Index: op.GetVout(),
	}, nil
}

func recoveryVTXOStatus(status arkrpc.VTXOStatus) (vtxo.VTXOStatus, bool) {
	switch status {
	case arkrpc.VTXOStatus_VTXO_STATUS_UNCONFIRMED,
		arkrpc.VTXOStatus_VTXO_STATUS_LIVE:
		return vtxo.VTXOStatusLive, true

	case arkrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT:
		return vtxo.VTXOStatusPendingForfeit, true

	case arkrpc.VTXOStatus_VTXO_STATUS_FORFEITING:
		return vtxo.VTXOStatusForfeiting, true

	case arkrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT:
		return vtxo.VTXOStatusUnilateralExit, true

	default:
		return vtxo.VTXOStatusLive, false
	}
}

func (r *RPCServer) saveRecoveredVTXO(ctx context.Context,
	desc *vtxo.Descriptor) (bool, error) {

	if r.server.vtxoStore == nil {
		return false, fmt.Errorf("VTXO store not initialized")
	}

	err := r.server.vtxoStore.SaveVTXO(ctx, desc)
	if err == nil {
		if err := r.notifyRecoveredVTXOs(
			ctx, []*vtxo.Descriptor{desc},
		); err != nil {
			return false, err
		}

		return true, nil
	}

	existing, getErr := r.server.vtxoStore.GetVTXO(ctx, desc.Outpoint)
	if getErr != nil {
		if !errorsIsNoRows(getErr) {
			return false, fmt.Errorf("check existing recovered "+
				"VTXO: %w", getErr)
		}

		return false, err
	}
	if existing == nil || existing.Amount != desc.Amount ||
		!bytes.Equal(existing.PkScript, desc.PkScript) {
		return false, err
	}

	return false, nil
}

func (r *RPCServer) notifyRecoveredVTXOs(ctx context.Context,
	descs []*vtxo.Descriptor) error {

	if len(descs) == 0 || !r.server.vtxoMgrRef.IsSome() {
		return nil
	}

	var notifyErr error
	r.server.vtxoMgrRef.WhenSome(func(ref actor.ActorRef[
		vtxo.ManagerMsg, vtxo.ManagerResp,
	]) {

		notifyErr = ref.Tell(
			ctx, &vtxo.VTXOsMaterializedNotification{
				VTXOs: descs,
			},
		)
	})
	if notifyErr != nil {
		return fmt.Errorf("notify VTXO manager: %w", notifyErr)
	}

	return nil
}

func (r *RPCServer) recoverOORReceiveScripts(ctx context.Context,
	terms *libtypes.OperatorTerms, window uint32,
	result *walletRecoveryResult) error {

	var registered *arkrpc.ListMyReceiveScriptsResponse
	err := retryRecoveryIndexerRPC(ctx, func() error {
		var err error
		registered, err = r.server.indexer.ListMyReceiveScripts(ctx)

		return err
	})
	if err != nil {
		return fmt.Errorf("list registered receive scripts: %w", err)
	}

	registeredScripts := make(
		map[string]struct{},
		len(
			registered.GetScripts(),
		),
	)
	for _, script := range registered.GetScripts() {
		registeredScripts[string(script.GetPkScript())] = struct{}{}
	}

	packageStore := r.newLocalOORArtifactStore()
	handler := r.recoveryOORHandler(terms, packageStore)

	for i := uint32(0); i < window; i++ {
		keyDesc, err := r.server.proofKeyBackend.DeriveKey(
			ctx, keychain.KeyLocator{
				Family: oorReceiveKeyFamily,
				Index:  i,
			},
		)
		if err != nil {
			return fmt.Errorf("derive OOR key %d: %w", i, err)
		}

		pkScript, err := BuildPubKeyVTXOReceiveScript(
			keyDesc.PubKey, terms.PubKey, terms.VTXOExitDelay,
		)
		if err != nil {
			return fmt.Errorf("build OOR script %d: %w", i, err)
		}

		scriptPersisted := false
		persistScript := func() error {
			if scriptPersisted {
				return nil
			}

			source := db.OwnedReceiveScriptSourceSync
			err := packageStore.UpsertOwnedReceiveScript(
				ctx, db.OwnedReceiveScriptRecord{
					PkScript:       pkScript,
					ClientKey:      *keyDesc,
					OperatorPubKey: terms.PubKey,
					ExitDelay: int64(
						terms.VTXOExitDelay,
					),
					Source:     source,
					CreatedAt:  r.server.clk.Now(),
					LastUsedAt: fn.None[time.Time](),
				},
			)
			if err != nil {
				return fmt.Errorf("persist OOR script %d: %w",
					i, err)
			}

			scriptPersisted = true
			result.OORReceiveScripts++

			return nil
		}

		if _, ok := registeredScripts[string(pkScript)]; ok {
			if err := persistScript(); err != nil {
				return err
			}
		}

		idx := r.server.indexer.WithSigner(
			r.server.proofKeyBackend.ProofSigner(*keyDesc),
		)
		if err := r.recoverOOREventsForScript(
			ctx, idx, pkScript, persistScript, handler, result,
		); err != nil {
			return fmt.Errorf("recover OOR events for key %d: %w",
				i, err)
		}
	}

	return nil
}

func (r *RPCServer) recoveryOORHandler(
	terms *libtypes.OperatorTerms,
	packageStore *db.OORArtifactPersistenceStore,
) *oor.LocalPersistenceOutboxHandler {

	return &oor.LocalPersistenceOutboxHandler{
		Store:        r.server.vtxoStore,
		PackageStore: packageStore,
		OperatorKey:  terms.PubKey,
		ExitDelay:    terms.VTXOExitDelay,
		NotifyIncomingVTXOs: func(ctx context.Context,
			descs []*vtxo.Descriptor) error {

			return r.notifyRecoveredVTXOs(ctx, descs)
		},
		ResolveIncomingClientKey: func(ctx context.Context,
			recipient oor.ArkRecipientOutput) (
			keychain.KeyDescriptor, error) {

			return ResolveOwnedReceiveScriptKey(
				ctx, packageStore, recipient,
			)
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID oor.SessionID,
			recipient oor.ArkRecipientOutput, ark *psbt.Packet,
			finalCheckpoints []*psbt.Packet) (
			oor.IncomingVTXOMetadata, error) {

			_ = ctx
			_ = sessionID
			_ = recipient
			_ = ark
			_ = finalCheckpoints

			return oor.IncomingVTXOMetadata{}, fmt.Errorf(
				"unexpected generic resolver call")
		},
	}
}

func (r *RPCServer) recoverOOREventsForScript(ctx context.Context,
	idx *indexer.Client, pkScript []byte, ensureScript func() error,
	handler *oor.LocalPersistenceOutboxHandler,
	result *walletRecoveryResult) error {

	var afterEventID uint64
	for {
		var resp *arkrpc.ListOORRecipientEventsByScriptResponse
		err := retryRecoveryIndexerRPC(ctx, func() error {
			var err error
			resp, err = idx.ListOORRecipientEventsByScriptTaproot(
				ctx, pkScript, afterEventID,
				recoveryOORPageSize,
			)

			return err
		})
		if err != nil {
			return err
		}

		for _, event := range resp.GetEvents() {
			if event == nil {
				continue
			}

			if err := ensureScript(); err != nil {
				return err
			}

			err := r.materializeRecoveredOOREvent(
				ctx, idx, event, handler,
			)
			if err != nil {
				return err
			}
			result.OOREvents++
		}

		nextCursor := resp.GetNextCursor()
		if nextCursor == 0 || nextCursor == afterEventID {
			break
		}

		afterEventID = nextCursor
	}

	return nil
}

func (r *RPCServer) materializeRecoveredOOREvent(ctx context.Context,
	idx *indexer.Client, event *arkrpc.OORRecipientEvent,
	handler *oor.LocalPersistenceOutboxHandler) error {

	sessionHash, err := chainhash.NewHash(event.GetSessionId())
	if err != nil {
		return fmt.Errorf("parse OOR session id: %w", err)
	}
	sessionID := oor.SessionID(*sessionHash)

	incoming, err := oor.IncomingTransferEventFromResponse(
		sessionID, event.GetEventId(),
		&arkrpc.ListOORRecipientEventsByScriptResponse{
			Events: []*arkrpc.OORRecipientEvent{event},
		},
	)
	if err != nil {
		return fmt.Errorf("decode incoming event: %w", err)
	}

	recipients, err := oor.ExtractArkRecipients(incoming.ArkPSBT)
	if err != nil {
		return fmt.Errorf("extract recipients: %w", err)
	}

	metadataMatches := make(
		[]oor.IncomingMetadataMatch, 0, len(recipients),
	)
	for _, recipient := range recipients {
		if !bytes.Equal(
			recipient.PkScript, event.GetRecipientPkScript(),
		) ||
			recipient.OutputIndex != event.GetOutputIndex() {

			continue
		}

		metadata, err := ResolveIncomingMetadataFromIndexerWithLimits(
			build.ContextWithLogger(
				ctx, r.server.subLogger(Subsystem),
			),
			idx,
			sessionID,
			recipient,
			r.server.cfg.OORReceiveLimits(),
		)
		if err != nil {
			return err
		}

		metadataMatches = append(metadataMatches,
			oor.IncomingMetadataMatch{
				OutputIndex: recipient.OutputIndex,
				Metadata:    metadata,
			},
		)
	}

	_, err = handler.Handle(
		ctx, sessionID, &oor.MaterializeIncomingVTXOsRequest{
			SessionID:            sessionID,
			ArkPSBT:              incoming.ArkPSBT,
			FinalCheckpointPSBTs: incoming.FinalCheckpointPSBTs,
			Recipients:           recipients,
			MetadataMatches:      metadataMatches,
			AncestorPackages:     incoming.AncestorPackages,
		},
	)
	if err != nil {
		return fmt.Errorf("materialize incoming event: %w", err)
	}

	return nil
}

func errorsIsNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
