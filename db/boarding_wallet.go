package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
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

// BoardingStore is the interface that groups all boarding-related database
// queries. This is a subset of sqlc.Querier focused on boarding operations.
type BoardingStore interface {
	InsertBoardingAddress(ctx context.Context, arg NewAddrParams) error

	GetBoardingAddress(
		ctx context.Context, pkScript []byte) (BoardingAddrRow, error)

	ListAllBoardingAddresses(ctx context.Context) ([]BoardingAddrRow, error)

	InsertBoardingIntent(ctx context.Context, arg NewIntentParams) error

	GetBoardingIntent(
		ctx context.Context, arg BoardingIntentKey,
	) (BoardingIntentRow, error)

	ListBoardingIntentsByStatus(
		ctx context.Context, status string) ([]BoardingIntentRow, error)

	ListAllBoardingIntents(ctx context.Context) ([]BoardingIntentRow, error)

	ListBoardingIntentsByPkScript(
		ctx context.Context, pkScript []byte,
	) ([]BoardingIntentRow, error)

	ListBoardingIntentOutpoints(ctx context.Context) ([]OutpointRow, error)

	ListBoardingIntentsByStatusAndMinHeight(
		ctx context.Context, arg IntentHeightFilter,
	) ([]BoardingIntentRow, error)
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
type BoardingWalletStore struct {
	db          BatchedBoardingStore
	chainParams *chaincfg.Params
	clock       clock.Clock
}

// NewBoardingWalletStore creates a new boarding wallet store using the
// transaction executor pattern.
func NewBoardingWalletStore(db BatchedBoardingStore,
	chainParams *chaincfg.Params, clock clock.Clock) *BoardingWalletStore {

	return &BoardingWalletStore{
		db:          db,
		chainParams: chainParams,
		clock:       clock,
	}
}

// InsertBoardingAddress persists a boarding address when it is first created.
// This method is idempotent - inserting the same address multiple times is
// safe due to ON CONFLICT DO NOTHING in the SQL.
func (b *BoardingWalletStore) InsertBoardingAddress(ctx context.Context,
	addr *wallet.BoardingAddress) error {

	writeTxOpts := WriteTxOption()

	return b.db.ExecTx(ctx, writeTxOpts, func(q BoardingStore) error {
		clientPubkeyBytes := addr.KeyDesc.PubKey.SerializeCompressed()
		operatorPubkeyBytes := addr.OperatorKey.SerializeCompressed()

		pkScript, err := txscript.PayToAddrScript(addr.Address)
		if err != nil {
			return fmt.Errorf("create pk script: %w", err)
		}

		params := NewAddrParams{
			PkScript:        pkScript,
			AddressString:   addr.Address.String(),
			ClientPubkey:    clientPubkeyBytes,
			ClientKeyFamily: int32(addr.KeyDesc.Family),
			ClientKeyIndex:  int32(addr.KeyDesc.Index),
			OperatorPubkey:  operatorPubkeyBytes,
			ExitDelay:       int32(addr.ExitDelay),
			CreationTime:    b.clock.Now().Unix(),
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

		addr, err := dbAddrToDomainAddr(b.chainParams, dbAddr)
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
			addr, err := dbAddrToDomainAddr(b.chainParams, dbAddr)
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
				return fmt.Errorf(
					"convert intent to params: %w", err,
				)
			}

			err = q.InsertBoardingIntent(ctx, params)
			if err != nil {
				return fmt.Errorf(
					"insert boarding intent: %w", err,
				)
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
			return fmt.Errorf("list all boarding intents: %w",
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
			return fmt.Errorf(
				"list boarding intents by status: %w", err,
			)
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
	ctx context.Context,
) ([]wire.OutPoint, error) {

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
	ctx context.Context, status wallet.BoardingStatus, minHeight int32,
) ([]wallet.BoardingIntent, error) {

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
			return fmt.Errorf(
				"list intents by status and height: %w", err,
			)
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
			return fmt.Errorf(
				"list boarding intents by pk script: %w", err,
			)
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
// wallet.BoardingAddress. The tapscript is reconstructed from the stored
// component fields (client pubkey, operator pubkey, exit delay) using
// scripts.VTXOTapScript().
func dbAddrToDomainAddr(chainParams *chaincfg.Params,
	dbAddr BoardingAddrRow) (*wallet.BoardingAddress, error) {

	clientPubkey, err := btcec.ParsePubKey(dbAddr.ClientPubkey)
	if err != nil {
		return nil, fmt.Errorf("parse client pubkey: %w", err)
	}
	operatorPubkey, err := btcec.ParsePubKey(dbAddr.OperatorPubkey)
	if err != nil {
		return nil, fmt.Errorf("parse operator pubkey: %w", err)
	}
	tapscript, err := arkscript.VTXOTapScript(
		clientPubkey, operatorPubkey, uint32(dbAddr.ExitDelay),
	)
	if err != nil {
		return nil, fmt.Errorf("reconstruct tapscript: %w", err)
	}

	address, err := btcutil.DecodeAddress(
		dbAddr.AddressString, chainParams,
	)
	if err != nil {
		return nil, fmt.Errorf("decode address: %w", err)
	}

	return &wallet.BoardingAddress{
		Address:   address,
		Tapscript: tapscript,
		KeyDesc: keychain.KeyDescriptor{
			PubKey: clientPubkey,
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(
					dbAddr.ClientKeyFamily,
				),
				Index: uint32(dbAddr.ClientKeyIndex),
			},
		},
		OperatorKey: operatorPubkey,
		ExitDelay:   uint32(dbAddr.ExitDelay),
	}, nil
}

// dbIntentToDomainIntent converts a sqlc BoardingIntent to a
// wallet.BoardingIntent. The BoardingStore parameter is used to fetch the
// associated boarding address within the same transaction.
func (b *BoardingWalletStore) dbIntentToDomainIntent(ctx context.Context,
	q BoardingStore, dbIntent BoardingIntentRow) (
	*wallet.BoardingIntent, error) {

	// Look up the boarding address within the same transaction.
	dbAddr, err := q.GetBoardingAddress(ctx, dbIntent.PkScript)
	if err != nil {
		return nil, fmt.Errorf("lookup boarding address: %w", err)
	}
	addr, err := dbAddrToDomainAddr(b.chainParams, dbAddr)
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

	chainInfo := wallet.BoardingChainInfo{
		ConfHeight: dbIntent.ConfHeight,
		ConfHash:   confHash,
		ConfTx:     confTx,
		OutPoint:   outpoint,
		Amount:     btcutil.Amount(dbIntent.Amount),
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
		return NewIntentParams{}, fmt.Errorf(
			"create pk script: %w", err,
		)
	}

	var confTxBytes []byte
	if intent.ChainInfo.ConfTx != nil {
		var buf bytes.Buffer
		err := intent.ChainInfo.ConfTx.Serialize(&buf)
		if err != nil {
			return NewIntentParams{}, fmt.Errorf(
				"serialize conf tx: %w", err,
			)
		}
		confTxBytes = buf.Bytes()
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
	default:
		return 0, fmt.Errorf("unknown boarding status: %q", status)
	}
}

// Compile-time check that BoardingWalletStore implements BoardingStore.
var _ wallet.BoardingStore = (*BoardingWalletStore)(nil)
