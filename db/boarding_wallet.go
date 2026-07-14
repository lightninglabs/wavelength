package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

type (
	BoardingAddrRow   = sqlc.BoardingAddress
	BoardingIntentRow = sqlc.BoardingIntent
	NewAddrParams     = sqlc.InsertBoardingAddressParams
	NewIntentParams   = sqlc.InsertBoardingIntentParams
	BoardingIntentKey = sqlc.GetBoardingIntentParams
	OutpointRow       = sqlc.ListBoardingIntentOutpointsRow
)

// IntentHeightFilter filters boarding intents by status and min conf height.
type IntentHeightFilter = sqlc.ListBoardingIntentsByStatusAndMinHeightParams

// BoardingSweepRow is the persisted aggregate boarding sweep transaction row.
type BoardingSweepRow = sqlc.BoardingSweep

// BoardingSweepInputRow is the persisted per-input boarding sweep row.
type BoardingSweepInputRow = sqlc.BoardingSweepInput

// BoardingStore is the interface that groups all boarding-related database
// queries. This is a subset of sqlc.Querier focused on boarding operations.
//
// focused on a single store capability.
//
//nolint:interfacebloat // Grouping all boarding queries keeps ExecTx closures
type BoardingStore interface {
	// InternalKeyQuerier lets the boarding store register and hydrate the
	// client wallet key via the shared internal_keys registry within its
	// own transaction.
	InternalKeyQuerier

	InsertBoardingAddress(ctx context.Context, arg NewAddrParams) error

	GetBoardingAddress(ctx context.Context,
		pkScript []byte) (BoardingAddrRow, error)

	ListAllBoardingAddresses(ctx context.Context) ([]BoardingAddrRow, error)

	InsertBoardingIntent(ctx context.Context, arg NewIntentParams) error

	GetBoardingIntent(ctx context.Context,
		arg BoardingIntentKey) (BoardingIntentRow, error)

	ListBoardingIntentsByStatus(ctx context.Context,
		status string) ([]BoardingIntentRow, error)

	ListBoardingIntentsBySweepableStatuses(ctx context.Context,
		arg sqlc.ListBoardingIntentsBySweepableStatusesParams) (
		[]BoardingIntentRow, error)

	ListAllBoardingIntents(ctx context.Context) ([]BoardingIntentRow, error)

	ListBoardingIntentsByPkScript(ctx context.Context,
		pkScript []byte) ([]BoardingIntentRow, error)

	ListBoardingIntentOutpoints(ctx context.Context) ([]OutpointRow, error)

	ListBoardingIntentsByStatusAndMinHeight(ctx context.Context,
		arg IntentHeightFilter) ([]BoardingIntentRow, error)

	UpdateBoardingIntentStatus(ctx context.Context,
		arg sqlc.UpdateBoardingIntentStatusParams) error

	InsertBoardingSweep(
		ctx context.Context, arg sqlc.InsertBoardingSweepParams,
	) error

	InsertBoardingSweepInput(ctx context.Context,
		arg sqlc.InsertBoardingSweepInputParams) error

	GetBoardingSweep(ctx context.Context,
		txid []byte) (BoardingSweepRow, error)

	GetBoardingSweepByInput(ctx context.Context,
		arg sqlc.GetBoardingSweepByInputParams) (
		BoardingSweepRow,
		error,
	)

	ListBoardingSweepInputs(ctx context.Context,
		txid []byte) ([]BoardingSweepInputRow, error)

	ListBoardingSweeps(ctx context.Context,
		arg sqlc.ListBoardingSweepsParams) ([]BoardingSweepRow, error)

	ListPendingBoardingSweeps(ctx context.Context) (
		[]BoardingSweepRow,
		error,
	)

	ListPendingBoardingSweepInputs(ctx context.Context) (
		[]BoardingSweepInputRow, error)

	MarkBoardingSweepStatus(ctx context.Context,
		arg sqlc.MarkBoardingSweepStatusParams) error

	MarkBoardingSweepInputStatus(
		ctx context.Context,
		arg sqlc.MarkBoardingSweepInputStatusParams,
	) error

	MarkBoardingSweepInputsStatus(
		ctx context.Context,
		arg sqlc.MarkBoardingSweepInputsStatusParams,
	) error

	MarkBoardingSweepInputSpentByOutpoint(ctx context.Context,
		arg sqlc.MarkBoardingSweepInputSpentByOutpointParams) (
		int64,
		error,
	)

	CountUnresolvedBoardingSweepInputs(ctx context.Context,
		txid []byte) (int64, error)
}

// BatchedBoardingStore combines BoardingStore with transaction support via the
// BatchedTx generic interface. This enables atomic operations across multiple
// queries.
type BatchedBoardingStore interface {
	BoardingStore
	BatchedTx[BoardingStore]
}

// BoardingWalletStore implements the wallet.BoardingStore interface using the
// BatchedTx pattern for transaction-safe operations. All methods execute within
// database transactions with automatic retry on serialization errors.
//
// The embedded PendingIntentPersistenceStore contributes the generic
// pending-intent outbox surface (UpsertPendingIntent and friends) that
// wallet.BoardingStore embeds via wallet.PendingIntentStore.
type BoardingWalletStore struct {
	*PendingIntentPersistenceStore

	db          BatchedBoardingStore
	chainParams *chaincfg.Params
	clock       clock.Clock

	// Log is an optional logger for this persistence store. If None,
	// the store falls back to extracting a logger from context via
	// build.LoggerFromContext, or uses btclog.Disabled if no logger
	// is found. Matches the fn.Option[btclog.Logger] pattern used by
	// VTXOPersistenceStore and other subsystems.
	Log fn.Option[btclog.Logger]
}

// NewBoardingWalletStore creates a new boarding wallet store using the
// transaction executor pattern. The pending-intent executor shares the same
// underlying database; it is a separate argument only because the generic
// transaction executor is typed per query-interface.
func NewBoardingWalletStore(db BatchedBoardingStore,
	intentDB BatchedPendingIntentStore, chainParams *chaincfg.Params,
	clock clock.Clock) *BoardingWalletStore {

	return &BoardingWalletStore{
		PendingIntentPersistenceStore: NewPendingIntentPersistenceStore(
			intentDB,
		),
		db:          db,
		chainParams: chainParams,
		clock:       clock,
	}
}

// logger returns the configured logger, falling back to extracting a logger
// from context. If neither is available, returns btclog.Disabled.
func (b *BoardingWalletStore) logger(ctx context.Context) btclog.Logger {
	return b.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// InsertBoardingAddress persists a boarding address when it is first created.
// This method is idempotent - inserting the same address multiple times is
// safe due to ON CONFLICT DO NOTHING in the SQL.
func (b *BoardingWalletStore) InsertBoardingAddress(ctx context.Context,
	addr *wallet.BoardingAddress) error {

	writeTxOpts := WriteTxOption()

	return b.db.ExecTx(ctx, writeTxOpts, func(q BoardingStore) error {
		operatorPubkeyBytes := addr.OperatorKey.SerializeCompressed()

		pkScript, err := txscript.PayToAddrScript(addr.Address)
		if err != nil {
			return fmt.Errorf("create pk script: %w", err)
		}

		// Register the client wallet key in the shared internal_keys
		// registry and reference it by id, rather than inlining the
		// (pubkey, family, index) triple on the row.
		clientKeyID, err := RegisterInternalKeyTx(
			ctx, q, b.clock.Now().Unix(), addr.KeyDesc,
		)
		if err != nil {
			return fmt.Errorf("register client key: %w", err)
		}

		params := NewAddrParams{
			PkScript:      pkScript,
			AddressString: addr.Address.String(),
			ClientKeyID: sql.NullInt64{
				Int64: clientKeyID,
				Valid: true,
			},
			OperatorPubkey: operatorPubkeyBytes,
			ExitDelay:      int32(addr.ExitDelay),
			CreationTime:   b.clock.Now().Unix(),
		}

		return q.InsertBoardingAddress(ctx, params)
	})
}

// LookupBoardingAddress retrieves a boarding address by its pkScript. Returns
// an error if the address is not found.
func (b *BoardingWalletStore) LookupBoardingAddress(ctx context.Context,
	pkScript []byte) (*wallet.BoardingAddress, error) {

	readTxOpts := ReadTxOption()

	var result *wallet.BoardingAddress

	err := b.db.ExecTx(ctx, readTxOpts, func(q BoardingStore) error {
		dbAddr, err := q.GetBoardingAddress(ctx, pkScript)
		if err != nil {
			return fmt.Errorf("get boarding address: %w", err)
		}

		addr, err := dbAddrToDomainAddr(ctx, q, b.chainParams, dbAddr)
		if err != nil {
			return err
		}

		result = addr

		return nil
	})

	return result, err
}

// ListAllBoardingAddresses returns all persisted boarding addresses. This is
// used during actor startup to re-register confirmation monitoring for all
// addresses.
func (b *BoardingWalletStore) ListAllBoardingAddresses(ctx context.Context) (
	[]*wallet.BoardingAddress, error) {

	readTxOpts := ReadTxOption()

	var result []*wallet.BoardingAddress

	err := b.db.ExecTx(ctx, readTxOpts, func(q BoardingStore) error {
		dbAddrs, err := q.ListAllBoardingAddresses(ctx)
		if err != nil {
			return fmt.Errorf("list all boarding addresses: %w",
				err)
		}

		addrs := make([]*wallet.BoardingAddress, 0, len(dbAddrs))
		for _, dbAddr := range dbAddrs {
			addr, err := dbAddrToDomainAddr(
				ctx, q, b.chainParams, dbAddr,
			)
			if err != nil {
				return fmt.Errorf("convert address: %w", err)
			}

			addrs = append(addrs, addr)
		}

		result = addrs

		return nil
	})

	return result, err
}

// InsertBoardingIntents persists one or more boarding intents. This operation
// is idempotent, allowing the same intent to be saved multiple times as it
// progresses through different states.
func (b *BoardingWalletStore) InsertBoardingIntents(ctx context.Context,
	intents ...wallet.BoardingIntent) error {

	writeTxOpts := WriteTxOption()

	return b.db.ExecTx(ctx, writeTxOpts, func(q BoardingStore) error {
		for _, intent := range intents {
			params, err := domainIntentToInsertParams(
				intent, b.clock,
			)
			if err != nil {
				return fmt.Errorf("convert intent to "+
					"params: %w", err)
			}

			err = q.InsertBoardingIntent(ctx, params)
			if err != nil {
				return fmt.Errorf("insert boarding intent: %w",
					err)
			}
		}

		return nil
	})
}

// FetchBoardingIntents returns all boarding intents that are currently in
// progress (not yet completed). This is used during actor startup to resume
// monitoring pending boarding flows.
func (b *BoardingWalletStore) FetchBoardingIntents(ctx context.Context) (
	[]wallet.BoardingIntent, error) {

	readTxOpts := ReadTxOption()

	var result []wallet.BoardingIntent

	err := b.db.ExecTx(ctx, readTxOpts, func(q BoardingStore) error {
		dbIntents, err := q.ListAllBoardingIntents(ctx)
		if err != nil {
			return fmt.Errorf("list all boarding intents: %w", err)
		}

		intents := make([]wallet.BoardingIntent, 0, len(dbIntents))
		for _, dbIntent := range dbIntents {
			intent, err := b.dbIntentToDomainIntent(
				ctx, q, dbIntent,
			)
			if err != nil {
				return fmt.Errorf("convert intent: %w", err)
			}

			intents = append(intents, *intent)
		}

		result = intents

		return nil
	})

	return result, err
}

// FetchBoardingIntentsByStatus returns all boarding intents matching the given
// status. This is used during startup to filter for Confirmed-but-not-Adopted
// intents that need to be resumed.
func (b *BoardingWalletStore) FetchBoardingIntentsByStatus(ctx context.Context,
	status wallet.BoardingStatus) ([]wallet.BoardingIntent, error) {

	statusStr, err := statusToString(status)
	if err != nil {
		return nil, err
	}

	readTxOpts := ReadTxOption()

	var result []wallet.BoardingIntent

	err = b.db.ExecTx(ctx, readTxOpts, func(q BoardingStore) error {
		dbIntents, err := q.ListBoardingIntentsByStatus(
			ctx, statusStr,
		)
		if err != nil {
			return fmt.Errorf("list boarding intents by status: %w",
				err)
		}

		intents := make([]wallet.BoardingIntent, 0, len(dbIntents))
		for _, dbIntent := range dbIntents {
			intent, err := b.dbIntentToDomainIntent(
				ctx, q, dbIntent,
			)
			if err != nil {
				return fmt.Errorf("convert intent: %w", err)
			}

			intents = append(intents, *intent)
		}

		result = intents

		return nil
	})

	return result, err
}

// FetchBoardingIntentsBySweepableStatuses returns all boarding intents in the
// lifecycle states that can still represent a timeout-path sweep candidate.
//
// The current sqlc query accepts exactly three status filters; the wallet
// actor always supplies confirmed/failed/expired so this guards the input
// length explicitly rather than padding with an inert value.
func (b *BoardingWalletStore) FetchBoardingIntentsBySweepableStatuses(
	ctx context.Context, statuses []wallet.BoardingStatus) (
	[]wallet.BoardingIntent, error) {

	if len(statuses) != 3 {
		return nil, fmt.Errorf("expected 3 sweepable statuses, got %d",
			len(statuses))
	}

	status0, err := statusToString(statuses[0])
	if err != nil {
		return nil, err
	}

	status1, err := statusToString(statuses[1])
	if err != nil {
		return nil, err
	}

	status2, err := statusToString(statuses[2])
	if err != nil {
		return nil, err
	}

	readTxOpts := ReadTxOption()

	var result []wallet.BoardingIntent

	err = b.db.ExecTx(ctx, readTxOpts, func(q BoardingStore) error {
		dbIntents, err := q.ListBoardingIntentsBySweepableStatuses(
			ctx, sqlc.ListBoardingIntentsBySweepableStatusesParams{
				Status:   status0,
				Status_2: status1,
				Status_3: status2,
			},
		)
		if err != nil {
			return fmt.Errorf("list boarding intents by "+
				"statuses: %w", err)
		}

		intents := make([]wallet.BoardingIntent, 0, len(dbIntents))
		for _, dbIntent := range dbIntents {
			intent, err := b.dbIntentToDomainIntent(
				ctx, q, dbIntent,
			)
			if err != nil {
				return fmt.Errorf("convert intent: %w", err)
			}

			intents = append(intents, *intent)
		}

		result = intents

		return nil
	})

	return result, err
}

// FetchBoardingIntentOutpoints returns just the outpoints of all boarding
// intents. This is more efficient than FetchBoardingIntents when only the
// outpoints are needed (e.g., for seenUtxos initialization).
func (b *BoardingWalletStore) FetchBoardingIntentOutpoints(
	ctx context.Context) ([]wire.OutPoint, error) {

	readTxOpts := ReadTxOption()

	var result []wire.OutPoint

	err := b.db.ExecTx(ctx, readTxOpts, func(q BoardingStore) error {
		rows, err := q.ListBoardingIntentOutpoints(ctx)
		if err != nil {
			return fmt.Errorf("list boarding intent outpoints: %w",
				err)
		}

		outpoints := make([]wire.OutPoint, 0, len(rows))
		for _, row := range rows {
			var hash chainhash.Hash
			copy(hash[:], row.OutpointHash)

			outpoints = append(outpoints, wire.OutPoint{
				Hash:  hash,
				Index: uint32(row.OutpointIndex),
			})
		}

		result = outpoints

		return nil
	})

	return result, err
}

// FetchBoardingIntentsByStatusAndMinHeight returns all boarding intents
// matching the given status with confirmation height >= minHeight. This is
// used for efficient backlog delivery to newly registered notifiers.
func (b *BoardingWalletStore) FetchBoardingIntentsByStatusAndMinHeight(
	ctx context.Context, status wallet.BoardingStatus, minHeight int32) (
	[]wallet.BoardingIntent, error) {

	statusStr, err := statusToString(status)
	if err != nil {
		return nil, err
	}

	readTxOpts := ReadTxOption()

	var result []wallet.BoardingIntent

	err = b.db.ExecTx(ctx, readTxOpts, func(q BoardingStore) error {
		params := IntentHeightFilter{
			Status:     statusStr,
			ConfHeight: minHeight,
		}
		dbIntents, err := q.ListBoardingIntentsByStatusAndMinHeight(
			ctx, params,
		)
		if err != nil {
			return fmt.Errorf("list intents by status and "+
				"height: %w", err)
		}

		intents := make([]wallet.BoardingIntent, 0, len(dbIntents))
		for _, dbIntent := range dbIntents {
			intent, err := b.dbIntentToDomainIntent(
				ctx, q, dbIntent,
			)
			if err != nil {
				return fmt.Errorf("convert intent: %w", err)
			}

			intents = append(intents, *intent)
		}

		result = intents

		return nil
	})

	return result, err
}

// UpdateBoardingIntentStatus updates one boarding intent's lifecycle status.
func (b *BoardingWalletStore) UpdateBoardingIntentStatus(ctx context.Context,
	outpoint wire.OutPoint, status wallet.BoardingStatus) error {

	statusStr, err := statusToString(status)
	if err != nil {
		return err
	}

	writeTxOpts := WriteTxOption()

	return b.db.ExecTx(ctx, writeTxOpts, func(q BoardingStore) error {
		params := sqlc.UpdateBoardingIntentStatusParams{
			OutpointHash:   outpoint.Hash[:],
			OutpointIndex:  int32(outpoint.Index),
			Status:         statusStr,
			LastUpdateTime: b.clock.Now().Unix(),
		}

		err := q.UpdateBoardingIntentStatus(ctx, params)
		if err != nil {
			return fmt.Errorf("update boarding intent status: %w",
				err)
		}

		return nil
	})
}

// GetIntent retrieves a boarding intent by its outpoint (primary key). Returns
// an error if the intent is not found.
func (b *BoardingWalletStore) GetIntent(ctx context.Context,
	outpoint wire.OutPoint) (*wallet.BoardingIntent, error) {

	readTxOpts := ReadTxOption()

	var result *wallet.BoardingIntent

	err := b.db.ExecTx(ctx, readTxOpts, func(q BoardingStore) error {
		params := BoardingIntentKey{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
		}

		dbIntent, err := q.GetBoardingIntent(ctx, params)
		if err != nil {
			return fmt.Errorf("get boarding intent: %w", err)
		}

		intent, err := b.dbIntentToDomainIntent(ctx, q, dbIntent)
		if err != nil {
			return err
		}

		result = intent

		return nil
	})

	return result, err
}

// LookupIntentByScript returns the stored intent associated with a boarding
// pkScript. Returns an error if none exists.
func (b *BoardingWalletStore) LookupIntentByScript(ctx context.Context,
	pkScript []byte) (*wallet.BoardingIntent, error) {

	readTxOpts := ReadTxOption()

	var result *wallet.BoardingIntent

	err := b.db.ExecTx(ctx, readTxOpts, func(q BoardingStore) error {
		dbIntents, err := q.ListBoardingIntentsByPkScript(
			ctx, pkScript,
		)
		if err != nil {
			return fmt.Errorf("list boarding intents by pk "+
				"script: %w", err)
		}

		if len(dbIntents) == 0 {
			return sql.ErrNoRows
		}

		// Return the first (most recent) intent for this pkScript.
		intent, err := b.dbIntentToDomainIntent(ctx, q, dbIntents[0])
		if err != nil {
			return err
		}

		result = intent

		return nil
	})

	return result, err
}

// dbAddrToDomainAddr converts a sqlc BoardingAddress to a
// wallet.BoardingAddress. The client KeyDescriptor is hydrated from the
// internal_keys registry via the client_key_id FK; the tapscript is
// reconstructed from the client pubkey, operator pubkey, and exit delay using
// scripts.VTXOTapScript().
func dbAddrToDomainAddr(ctx context.Context, q InternalKeyQuerier,
	chainParams *chaincfg.Params,
	dbAddr BoardingAddrRow) (*wallet.BoardingAddress, error) {

	if !dbAddr.ClientKeyID.Valid {
		return nil, fmt.Errorf("boarding address %x missing "+
			"client key id", dbAddr.PkScript)
	}

	clientKeyDesc, err := InternalKeyDescByIDTx(
		ctx, q, dbAddr.ClientKeyID.Int64,
	)
	if err != nil {
		return nil, fmt.Errorf("hydrate client key: %w", err)
	}

	operatorPubkey, err := btcec.ParsePubKey(dbAddr.OperatorPubkey)
	if err != nil {
		return nil, fmt.Errorf("parse operator pubkey: %w", err)
	}
	tapscript, err := arkscript.VTXOTapScript(
		clientKeyDesc.PubKey, operatorPubkey, uint32(dbAddr.ExitDelay),
	)
	if err != nil {
		return nil, fmt.Errorf("reconstruct tapscript: %w", err)
	}

	address, err := btcaddr.DecodeAddress(
		dbAddr.AddressString, chainParams,
	)
	if err != nil {
		return nil, fmt.Errorf("decode address: %w", err)
	}

	return &wallet.BoardingAddress{
		Address:     address,
		Tapscript:   tapscript,
		KeyDesc:     clientKeyDesc,
		OperatorKey: operatorPubkey,
		ExitDelay:   uint32(dbAddr.ExitDelay),
	}, nil
}

// dbIntentToDomainIntent converts a sqlc BoardingIntent to a
// wallet.BoardingIntent. The BoardingStore parameter is used to fetch the
// associated boarding address within the same transaction.
func (b *BoardingWalletStore) dbIntentToDomainIntent(ctx context.Context,
	q BoardingStore, dbIntent BoardingIntentRow) (*wallet.BoardingIntent,
	error) {

	// Look up the boarding address within the same transaction.
	dbAddr, err := q.GetBoardingAddress(ctx, dbIntent.PkScript)
	if err != nil {
		return nil, fmt.Errorf("lookup boarding address: %w", err)
	}
	addr, err := dbAddrToDomainAddr(ctx, q, b.chainParams, dbAddr)
	if err != nil {
		return nil, fmt.Errorf("convert address: %w", err)
	}

	var outpointHash chainhash.Hash
	copy(outpointHash[:], dbIntent.OutpointHash)
	outpoint := wire.OutPoint{
		Hash:  outpointHash,
		Index: uint32(dbIntent.OutpointIndex),
	}

	var confHash chainhash.Hash
	copy(confHash[:], dbIntent.ConfHash)

	var confTx *wire.MsgTx
	if len(dbIntent.ConfTx) > 0 {
		confTx = &wire.MsgTx{}
		reader := bytes.NewReader(dbIntent.ConfTx)
		err = confTx.Deserialize(reader)
		if err != nil {
			return nil, fmt.Errorf("deserialize conf tx: %w", err)
		}
	}

	// Hydrate the SPV TxProof when present. NULL columns and decode
	// failures both fall through to None: the rebuild-fallback in
	// wallet.maybeRebuildBoardingProof will reconstruct the proof from
	// ConfTx/ConfHash via the chain backend, so a corrupt blob is
	// recoverable rather than fatal. NULL is the legacy / pre-migration
	// contract; a decode error is unexpected, so we log it as Warn so
	// operators see the corruption signal even though we recover.
	//
	// This intentionally diverges from db/round_store.go which fails
	// hard on the same decoder: round-state load has no rebuild
	// fallback, so the strict policy is appropriate there.
	txProofOpt := fn.None[proof.TxProof]()
	if len(dbIntent.TxProof) > 0 {
		decoded, err := types.DeserializeTxProof(dbIntent.TxProof)
		switch {
		case err != nil:
			b.logger(ctx).WarnS(
				ctx, "Corrupt boarding TxProof blob; "+
					"will rebuild on next backlog "+
					"or board RPC", err,
				btclog.Fmt("outpoint", "%v", outpoint),
			)

		case decoded != nil:
			txProofOpt = fn.Some(*decoded)
		}
	}

	chainInfo := wallet.BoardingChainInfo{
		ConfHeight: dbIntent.ConfHeight,
		ConfHash:   confHash,
		ConfTx:     confTx,
		OutPoint:   outpoint,
		Amount:     btcutil.Amount(dbIntent.Amount),
		TxProof:    txProofOpt,
	}

	status, err := stringToStatus(dbIntent.Status)
	if err != nil {
		return nil, fmt.Errorf("parse status: %w", err)
	}

	return &wallet.BoardingIntent{
		Address:   *addr,
		Outpoint:  outpoint,
		ChainInfo: chainInfo,
		Status:    status,
	}, nil
}

// domainIntentToInsertParams converts a wallet.BoardingIntent to sqlc insert
// parameters.
func domainIntentToInsertParams(intent wallet.BoardingIntent,
	c clock.Clock) (NewIntentParams, error) {

	pkScript, err := txscript.PayToAddrScript(intent.Address.Address)
	if err != nil {
		return NewIntentParams{}, fmt.Errorf("create pk script: %w",
			err)
	}

	var confTxBytes []byte
	if intent.ChainInfo.ConfTx != nil {
		var buf bytes.Buffer
		err := intent.ChainInfo.ConfTx.Serialize(&buf)
		if err != nil {
			return NewIntentParams{}, fmt.Errorf("serialize "+
				"conf tx: %w", err)
		}
		confTxBytes = buf.Bytes()
	}

	// Serialize the SPV TxProof using the same TLV encoding as
	// round_boarding_intents.tx_proof so the column is wire-format
	// compatible across the two tables. None proofs persist as a NULL
	// column; the row stays valid and decodes back to None on read.
	var (
		txProofBytes  []byte
		txProofSerErr error
	)
	intent.ChainInfo.TxProof.WhenSome(func(p proof.TxProof) {
		data, err := types.SerializeTxProof(&p)
		if err != nil {
			txProofSerErr = err

			return
		}
		txProofBytes = data
	})
	if txProofSerErr != nil {
		return NewIntentParams{}, fmt.Errorf("serialize tx proof: %w",
			txProofSerErr)
	}
	// Normalise a zero-length proof slice to nil so it lands as SQL
	// NULL via the COALESCE upsert and never overwrites a previously
	// persisted proof with an empty BLOB. This is the Go-side mirror
	// of the dialect-portable plain COALESCE in boarding.sql: writing
	// a NULLIF(..., x'') guard there would work on SQLite but fail on
	// Postgres BYTEA (x'' is parsed as a bit-string), so the empty-
	// slice defense lives here instead.
	if len(txProofBytes) == 0 {
		txProofBytes = nil
	}

	statusStr, err := statusToString(intent.Status)
	if err != nil {
		return NewIntentParams{}, err
	}

	nowUnix := c.Now().Unix()

	return NewIntentParams{
		OutpointHash:   intent.Outpoint.Hash[:],
		OutpointIndex:  int32(intent.Outpoint.Index),
		PkScript:       pkScript,
		Amount:         int64(intent.ChainInfo.Amount),
		ConfHeight:     intent.ChainInfo.ConfHeight,
		ConfHash:       intent.ChainInfo.ConfHash[:],
		ConfTx:         confTxBytes,
		TxProof:        txProofBytes,
		Status:         statusStr,
		CreationTime:   nowUnix,
		LastUpdateTime: nowUnix,
	}, nil
}

// statusToString converts a wallet.BoardingStatus to its string representation.
// Returns an error if an unknown status is provided.
func statusToString(status wallet.BoardingStatus) (string, error) {
	switch status {
	case wallet.BoardingStatusConfirmed:
		return "confirmed", nil

	case wallet.BoardingStatusAdopted:
		return "adopted", nil

	case wallet.BoardingStatusFailed:
		return "failed", nil

	case wallet.BoardingStatusExpired:
		return "expired", nil

	case wallet.BoardingStatusSwept:
		return "swept", nil

	case wallet.BoardingStatusSweepPending:
		return "sweep_pending", nil

	default:
		return "", fmt.Errorf("unknown boarding status: %d", status)
	}
}

// stringToStatus converts a string to a wallet.BoardingStatus. Returns an error
// if an unknown status string is provided.
func stringToStatus(status string) (wallet.BoardingStatus, error) {
	switch status {
	case "confirmed":
		return wallet.BoardingStatusConfirmed, nil

	case "adopted":
		return wallet.BoardingStatusAdopted, nil

	case "failed":
		return wallet.BoardingStatusFailed, nil

	case "expired":
		return wallet.BoardingStatusExpired, nil

	case "swept":
		return wallet.BoardingStatusSwept, nil

	case "sweep_pending":
		return wallet.BoardingStatusSweepPending, nil

	default:
		return 0, fmt.Errorf("unknown boarding status: %q", status)
	}
}

// Compile-time check that BoardingWalletStore implements BoardingStore.
var _ wallet.BoardingStore = (*BoardingWalletStore)(nil)
