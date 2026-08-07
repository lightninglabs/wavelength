package swaps

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/wavelength/swaprpc"
	"google.golang.org/protobuf/proto"
)

const (
	creditAccountAuthorizationTTL = time.Minute
	creditAccountNonceSize        = swaprpc.CreditAccountNonceSize
)

// CreditAccountAuthorizationSigner signs one canonical account request with
// the wallet identity key. The signer must reject accountKey unless it names
// that identity key.
type CreditAccountAuthorizationSigner func(context.Context, []byte, [32]byte,
	int64, [creditAccountNonceSize]byte) (*schnorr.Signature, error)

// authorizeCreditAccountRequest attaches a fresh, short-lived authorization
// to one account-scoped request.
func (g *GRPCSwapServerConn) authorizeCreditAccountRequest(ctx context.Context,
	req proto.Message) error {

	if g.creditAccountSigner == nil {
		return fmt.Errorf("credit account authorization signer is " +
			"required")
	}

	digest, accountKey, err := swaprpc.CreditAccountRequestDigest(req)
	if err != nil {
		return err
	}

	var nonce [creditAccountNonceSize]byte
	if _, err := io.ReadFull(g.authRand, nonce[:]); err != nil {
		return fmt.Errorf("generate credit account authorization "+
			"nonce: %w", err)
	}

	expiresAt := g.authNow().Add(creditAccountAuthorizationTTL).Unix()
	sig, err := g.creditAccountSigner(
		ctx, accountKey, digest, expiresAt, nonce,
	)
	if err != nil {
		return fmt.Errorf("sign credit account authorization: %w", err)
	}
	if sig == nil {
		return fmt.Errorf("credit account authorization signature is " +
			"required")
	}

	return swaprpc.SetCreditAccountAuthorization(
		req, &swaprpc.CreditAccountAuthorization{
			ExpiresAtUnix: expiresAt,
			Nonce:         nonce[:],
			Signature:     sig.Serialize(),
		},
	)
}

// defaultCreditAccountAuthRand returns the cryptographic random source used
// for authorization nonces.
func defaultCreditAccountAuthRand() io.Reader {
	return rand.Reader
}
