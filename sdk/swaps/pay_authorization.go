package swaps

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/btcsuite/btcd/btcec/v2"
	swapsqlc "github.com/lightninglabs/wavelength/sdk/swaps/sqlc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrPayAuthorizationMismatch means a payment hash already belongs to a
// different durable request. No new server operation is admitted by this error.
var ErrPayAuthorizationMismatch = status.Error(
	codes.AlreadyExists, "pay authorization differs from original request",
)

// authorizePay persists the first caller's exact request before a server can
// reserve credit. The payment hash binds retries, including a lost create
// response, to the original invoice, account and caps. Concurrent requests with
// different terms cannot replace the winning authorization.
func (c *SwapClient) authorizePay(ctx context.Context, invoice string,
	maxFeeSat, maxCreditSat uint64, account *btcec.PublicKey) (lntypes.Hash,
	error) {

	decoded, err := decodePayInvoice(invoice, c.chainParams)
	if err != nil {
		return lntypes.Hash{}, err
	}
	if decoded.PaymentHash == nil {
		return lntypes.Hash{}, fmt.Errorf("invoice payment hash is " +
			"required")
	}
	hash := lntypes.Hash(*decoded.PaymentHash)
	if c.store == nil {
		return hash, nil
	}

	// Historical swap rows have no recoverable original credit cap. Resume
	// can reconcile them, but a new Start must not invent authority for
	// them.
	_, err = c.store.queries.GetPayAuthorization(ctx, hash[:])
	if errors.Is(err, sql.ErrNoRows) {
		_, rowErr := c.store.queries.GetPaySwap(ctx, hash[:])
		if rowErr == nil {
			return hash, fmt.Errorf("historical pay " +
				"authorization unknown; resume the existing " +
				"swap")
		}
		if !errors.Is(rowErr, sql.ErrNoRows) {
			return hash, rowErr
		}
	} else if err != nil {
		return hash, err
	}

	terms := swapsqlc.AdmitPayAuthorizationParams{
		PaymentHash: hash[:], Invoice: invoice,
		AccountPubkey: account.SerializeCompressed(),
		MaxFeeSat:     strconv.FormatUint(maxFeeSat, 10),
		MaxCreditSat:  strconv.FormatUint(maxCreditSat, 10),
	}
	if err := c.store.queries.AdmitPayAuthorization(
		ctx, terms,
	); err != nil {
		return hash, fmt.Errorf("persist pay authorization: %w", err)
	}
	stored, err := c.store.queries.GetPayAuthorization(ctx, hash[:])
	if err != nil {
		return hash, fmt.Errorf("load pay authorization: %w", err)
	}
	if stored.Invoice != terms.Invoice ||
		stored.MaxFeeSat != terms.MaxFeeSat ||
		stored.MaxCreditSat != terms.MaxCreditSat ||
		!bytes.Equal(stored.AccountPubkey, terms.AccountPubkey) {
		return hash, ErrPayAuthorizationMismatch
	}

	return hash, nil
}

// ValidatePayAuthorization checks a replay against an existing durable request.
// It never admits a payment or assigns a cap to historical state. Bridges call
// it before returning an existing swap so local deduplication cannot bypass
// immutable authorization.
func (c *SwapClient) ValidatePayAuthorization(ctx context.Context,
	invoice string, maxFeeSat, maxCreditSat uint64) error {

	decoded, err := decodePayInvoice(invoice, c.chainParams)
	if err != nil {
		return err
	}
	if decoded.PaymentHash == nil || c.store == nil {
		return fmt.Errorf("durable pay authorization is required")
	}
	key, err := c.creditAccountKey(ctx)
	if err != nil {
		return err
	}
	stored, err := c.store.queries.GetPayAuthorization(
		ctx, decoded.PaymentHash[:],
	)
	if errors.Is(err, sql.ErrNoRows) {
		// Migration cannot recover historical caps. Reattaching to the
		// exact local invoice and account creates no server operation
		// and writes no new authority. Its existing worker retains
		// recovery ownership.
		row, err := c.store.queries.GetPaySwap(
			ctx, decoded.PaymentHash[:],
		)
		if err != nil {
			return fmt.Errorf("load historical pay: %w", err)
		}
		if row.Invoice != invoice ||
			!bytes.Equal(row.ClientPubkey, key) {
			return ErrPayAuthorizationMismatch
		}

		return nil
	}
	if err != nil {
		return fmt.Errorf("load original pay authorization: %w", err)
	}
	if stored.Invoice != invoice ||
		stored.MaxFeeSat != strconv.FormatUint(maxFeeSat, 10) ||
		stored.MaxCreditSat != strconv.FormatUint(maxCreditSat, 10) ||
		!bytes.Equal(stored.AccountPubkey, key) {
		return ErrPayAuthorizationMismatch
	}

	return nil
}
