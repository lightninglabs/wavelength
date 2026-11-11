package notification

import (
	"bytes"
	"context"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// SetFeedGrant delegates wake observation on an authenticated source
// connection. The wallet signs only after matching the challenge's owner and
// pinned source identity. No relay verification or mailbox ingress worker is
// needed. Retry an ambiguous response with a fresh challenge and the same grant
// ID.
func SetFeedGrant(ctx context.Context, rpc pb.WakeFeedServiceClient,
	owner, source *btcec.PublicKey, mutation *pb.FeedGrantMutation,
	sign Signer) (*pb.FeedGrantResponse, error) {

	if rpc == nil || owner == nil || source == nil || sign == nil {
		return nil, ErrInvalidAuthorization
	}
	if err := validateFeedMutationShape(mutation); err != nil {
		return nil, err
	}
	ownedMutation := new(pb.FeedGrantMutation)
	proto.Merge(ownedMutation, mutation)
	mutation = ownedMutation
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	challenge, err := rpc.GetFeedChallenge(
		callCtx, &pb.NotificationChallengeRequest{
			OwnerPubkey: owner.SerializeCompressed(),
		},
	)
	if status.Code(err) == codes.Unimplemented {
		return nil, ErrUnsupported
	}
	if err != nil {
		return nil, err
	}
	if challenge == nil ||
		!bytes.Equal(
			challenge.OwnerPubkey, owner.SerializeCompressed(),
		) ||
		!bytes.Equal(
			challenge.OperatorPubkey, source.SerializeCompressed(),
		) {
		return nil, ErrInvalidAuthorization
	}
	request := &pb.FeedGrantRequest{
		Challenge: challenge,
		Mutation:  mutation,
	}
	message, err := FeedSigningMessage(request)
	if err != nil {
		return nil, err
	}
	sig, err := sign(callCtx, message, []byte(FeedSignatureTag))
	if err != nil {
		return nil, err
	}
	if sig == nil {
		return nil, ErrInvalidAuthorization
	}
	request.Signature = sig.Serialize()
	response, err := rpc.SetFeedGrant(callCtx, request)
	if status.Code(err) == codes.Unimplemented {
		return nil, ErrUnsupported
	}
	if err != nil {
		return nil, err
	}
	if !validFeedResponse(response, mutation) {
		return nil, ErrInvalidAuthorization
	}

	return response, nil
}

// validFeedResponse rejects a response bound to another grant or an expiry
// greater than the wallet authorized. The token is a correlation identifier.
func validFeedResponse(response *pb.FeedGrantResponse,
	mutation *pb.FeedGrantMutation) bool {

	if response == nil || len(response.ProtoReflect().GetUnknown()) != 0 ||
		response.GrantId != mutation.GrantId {
		return false
	}
	if mutation.Action == pb.FeedGrantMutation_ACTION_REMOVE {
		return len(response.FeedToken) == 0 &&
			response.ExpiresAtUnix == 0
	}

	return len(response.FeedToken) == FeedTokenSize &&
		response.ExpiresAtUnix > 0 &&
		response.ExpiresAtUnix <= mutation.ExpiresAtUnix
}
