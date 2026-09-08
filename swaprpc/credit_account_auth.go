package swaprpc

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"google.golang.org/protobuf/proto"
)

const (
	// CreditAccountRequestTag domain-separates canonical credit-account
	// request digests from every other protocol message.
	//nolint:gosec // Public protocol tag, not a credential.
	CreditAccountRequestTag = "swap-credit-account-request-v1"

	// CreditAccountAuthTag domain-separates credit-account authorization
	// signatures from every other use of the wallet identity key.
	//nolint:gosec // Public protocol tag, not a credential.
	CreditAccountAuthTag = "swap-credit-account-auth-v1"

	// CreditAccountNonceSize is the required authorization nonce length.
	CreditAccountNonceSize = 32

	// CreditAccountMaxAuthTTL bounds signed request validity even when a
	// caller supplies a far-future expiry.
	CreditAccountMaxAuthTTL = 5 * time.Minute
)

const (
	requestChannelIDMethod  = "/swaprpc.SwapService/RequestChannelId"
	createInSwapMethod      = "/swaprpc.SwapService/CreateInSwap"
	createRefreshSwapMethod = "/swaprpc.SwapService/CreateRefreshSwap"
	quoteInSwapMethod       = "/swaprpc.SwapService/QuoteInSwap"
	accountCreateMethod     = "/swaprpc.SwapService/CreateCredit"
	accountRedeemMethod     = "/swaprpc.SwapService/RedeemCredit"
	accountListMethod       = "/swaprpc.SwapService/ListCredits"
)

// CreditAccountRequestDigest returns the canonical digest and account key for
// an account-scoped swap request. The authorization field is cleared before
// deterministic protobuf serialization so the signature never commits to
// itself.
func CreditAccountRequestDigest(req proto.Message) ([32]byte, []byte, error) {
	method, accountKey, unsigned, err := unsignedCreditAccountRequest(req)
	if err != nil {
		return [32]byte{}, nil, err
	}

	encoded, err := proto.MarshalOptions{
		Deterministic: true,
	}.Marshal(
		unsigned,
	)
	if err != nil {
		return [32]byte{}, nil, fmt.Errorf("marshal credit account "+
			"request: %w", err)
	}

	message := make([]byte, 0, len(method)+1+len(encoded))
	message = append(message, method...)
	message = append(message, 0)
	message = append(message, encoded...)
	digest := chainhash.TaggedHash([]byte(CreditAccountRequestTag), message)

	return *digest, append([]byte(nil), accountKey...), nil
}

// CreditAccountAuthMessage returns the canonical message signed by a credit
// account identity key for one request digest, expiry, and nonce.
func CreditAccountAuthMessage(accountKey []byte, requestDigest [32]byte,
	expiresAtUnix int64, nonce []byte) []byte {

	message := make(
		[]byte, 0, len(accountKey)+len(requestDigest)+8+len(nonce),
	)
	message = append(message, accountKey...)
	message = append(message, requestDigest[:]...)
	message = binary.BigEndian.AppendUint64(message, uint64(expiresAtUnix))
	message = append(message, nonce...)

	return message
}

// CreditAccountAuthDigest returns the BIP-340 tagged digest signed by a credit
// account identity key.
func CreditAccountAuthDigest(accountKey []byte, requestDigest [32]byte,
	expiresAtUnix int64, nonce []byte) [32]byte {

	message := CreditAccountAuthMessage(
		accountKey, requestDigest, expiresAtUnix, nonce,
	)
	digest := chainhash.TaggedHash([]byte(CreditAccountAuthTag), message)

	return *digest
}

// CreditAccountAuthorizationForRequest extracts an account authorization from
// one supported request.
func CreditAccountAuthorizationForRequest(req proto.Message) (
	*CreditAccountAuthorization, error) {

	switch typed := req.(type) {
	case *RequestChannelIdRequest:
		return typed.GetAccountAuthorization(), nil

	case *CreateInSwapRequest:
		return typed.GetAccountAuthorization(), nil

	case *CreateRefreshSwapRequest:
		return typed.GetAccountAuthorization(), nil

	case *QuoteInSwapRequest:
		return typed.GetAccountAuthorization(), nil

	case *CreateCreditRequest:
		return typed.GetAccountAuthorization(), nil

	case *RedeemCreditRequest:
		return typed.GetAccountAuthorization(), nil

	case *ListCreditsRequest:
		return typed.GetAccountAuthorization(), nil

	default:
		return nil, fmt.Errorf("unsupported credit account request %T",
			req)
	}
}

// SetCreditAccountAuthorization attaches an authorization to one supported
// request.
func SetCreditAccountAuthorization(req proto.Message,
	auth *CreditAccountAuthorization) error {

	switch typed := req.(type) {
	case *RequestChannelIdRequest:
		typed.AccountAuthorization = auth

	case *CreateInSwapRequest:
		typed.AccountAuthorization = auth

	case *CreateRefreshSwapRequest:
		typed.AccountAuthorization = auth

	case *QuoteInSwapRequest:
		typed.AccountAuthorization = auth

	case *CreateCreditRequest:
		typed.AccountAuthorization = auth

	case *RedeemCreditRequest:
		typed.AccountAuthorization = auth

	case *ListCreditsRequest:
		typed.AccountAuthorization = auth

	default:
		return fmt.Errorf("unsupported credit account request %T", req)
	}

	return nil
}

// unsignedCreditAccountRequest clones one supported request, clears its
// authorization, and returns the method and account identity committed by the
// signature.
func unsignedCreditAccountRequest(req proto.Message) (string, []byte,
	proto.Message, error) {

	var (
		method     string
		accountKey []byte
	)
	switch typed := req.(type) {
	case *RequestChannelIdRequest:
		method = requestChannelIDMethod
		accountKey = typed.GetClientVhtlcPubkey()

	case *CreateInSwapRequest:
		method = createInSwapMethod
		accountKey = typed.GetAccountPubkey()

	case *CreateRefreshSwapRequest:
		method = createRefreshSwapMethod
		accountKey = typed.GetClientVhtlcPubkey()

	case *QuoteInSwapRequest:
		method = quoteInSwapMethod
		accountKey = typed.GetAccountPubkey()

	case *CreateCreditRequest:
		method = accountCreateMethod
		accountKey = typed.GetAccountPubkey()

	case *RedeemCreditRequest:
		method = accountRedeemMethod
		accountKey = typed.GetAccountPubkey()

	case *ListCreditsRequest:
		method = accountListMethod
		accountKey = typed.GetAccountPubkey()

	default:
		return "", nil, nil, fmt.Errorf("unsupported credit account "+
			"request %T", req)
	}

	unsigned := proto.Clone(req)
	if err := SetCreditAccountAuthorization(unsigned, nil); err != nil {
		return "", nil, nil, fmt.Errorf("clear credit account "+
			"authorization: %w", err)
	}

	return method, accountKey, unsigned, nil
}
