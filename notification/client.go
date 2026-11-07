package notification

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ErrUnsupported reports an older edge with no notification control service.
// Wallet receipt and foreground recovery remain available without this service.
var ErrUnsupported = errors.New("notification subscriptions unsupported")

// Signer signs the raw message with the supplied BIP-340 tag under the mailbox
// identity key. It must not hash the tagged digest again or expose private
// keys.
type Signer func(context.Context, []byte, []byte) (*schnorr.Signature, error)

// SetSubscription obtains a fresh challenge and signs its exact mutation for
// the already-trusted operator identity. Use its authenticated TLS connection,
// not a URL supplied by a notification. The direct RPC requires no mailbox
// ingress worker. An ambiguous response needs another invocation; it never
// replays a consumed proof. The caller owns ordering of simultaneous mutations.
func SetSubscription(ctx context.Context,
	rpc mailboxpb.NotificationServiceClient,
	owner, operator *btcec.PublicKey,
	mutation *mailboxpb.NotificationSubscriptionMutation, sign Signer) (
	*mailboxpb.NotificationSubscriptionResponse, error) {

	if rpc == nil || owner == nil || operator == nil || sign == nil {
		return nil, ErrInvalidAuthorization
	}
	if err := validateMutationShape(mutation); err != nil {
		return nil, err
	}
	// Own request bytes independently of the caller across the two RPCs.
	ownedMutation := new(mailboxpb.NotificationSubscriptionMutation)
	proto.Merge(ownedMutation, mutation)
	mutation = ownedMutation
	ownerBytes := owner.SerializeCompressed()
	operatorBytes := operator.SerializeCompressed()
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	challenge, err := rpc.GetNotificationChallenge(callCtx,
		&mailboxpb.NotificationChallengeRequest{
			OwnerPubkey: ownerBytes,
		})
	if status.Code(err) == codes.Unimplemented {
		return nil, ErrUnsupported
	}
	if err != nil {
		return nil, err
	}
	if challenge == nil ||
		!bytes.Equal(challenge.OwnerPubkey, ownerBytes) ||
		!bytes.Equal(
			challenge.OperatorPubkey,
			operatorBytes,
		) {
		return nil, ErrInvalidAuthorization
	}
	request := &mailboxpb.NotificationSubscriptionRequest{
		Challenge: challenge, Mutation: mutation,
	}
	message, err := SigningMessage(request)
	if err != nil {
		return nil, err
	}
	sig, err := sign(callCtx, message, []byte(SignatureTag))
	if err != nil {
		return nil, err
	}
	if sig == nil {
		return nil, ErrInvalidAuthorization
	}
	request.Signature = sig.Serialize()

	return rpc.SetNotificationSubscription(callCtx, request)
}
