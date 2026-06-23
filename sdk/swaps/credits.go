package swaps

import (
	"context"
	"fmt"
)

type creditServerConn interface {
	CreateCredit(context.Context, []byte,
		CreateCreditRequest) (*CreditOperation, error)

	RedeemCredit(context.Context, []byte,
		RedeemCreditRequest) (*CreditRedemption, error)

	ListCredits(context.Context, []byte, uint32) (*CreditSnapshot, error)
}

// CreateCredit starts one server-owned credit funding operation for this
// wallet's identity account.
func (c *SwapClient) CreateCredit(ctx context.Context,
	req CreateCreditRequest) (*CreditOperation, error) {

	server, err := c.creditServer()
	if err != nil {
		return nil, err
	}

	accountKey, err := c.creditAccountKey(ctx)
	if err != nil {
		return nil, err
	}

	return server.CreateCredit(ctx, accountKey, req)
}

// RedeemCredit asks the swap server to materialize available credits back into
// the supplied Ark destination for this wallet's identity account.
func (c *SwapClient) RedeemCredit(ctx context.Context,
	req RedeemCreditRequest) (*CreditRedemption, error) {

	server, err := c.creditServer()
	if err != nil {
		return nil, err
	}

	accountKey, err := c.creditAccountKey(ctx)
	if err != nil {
		return nil, err
	}

	return server.RedeemCredit(ctx, accountKey, req)
}

// ListCredits returns the server-authoritative credit account snapshot for the
// wallet identity account.
func (c *SwapClient) ListCredits(ctx context.Context, limit uint32) (
	*CreditSnapshot, error) {

	server, err := c.creditServer()
	if err != nil {
		return nil, err
	}

	accountKey, err := c.creditAccountKey(ctx)
	if err != nil {
		return nil, err
	}

	return server.ListCredits(ctx, accountKey, limit)
}

func (c *SwapClient) creditServer() (creditServerConn, error) {
	if c == nil || c.server == nil {
		return nil, fmt.Errorf("swap server is not configured")
	}

	server, ok := c.server.(creditServerConn)
	if !ok {
		return nil, fmt.Errorf("swap server does not support credits")
	}

	return server, nil
}

func (c *SwapClient) creditAccountKey(ctx context.Context) ([]byte, error) {
	if c == nil || c.daemon == nil {
		return nil, fmt.Errorf("daemon is not configured")
	}

	accountKey, err := c.daemon.IdentityPubKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("get credit account pubkey: %w", err)
	}
	if accountKey == nil {
		return nil, fmt.Errorf("credit account pubkey is required")
	}

	return accountKey.SerializeCompressed(), nil
}
