package darepod

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/unroll"
	"github.com/lightninglabs/darepo-client/vhtlcrecovery"
	"github.com/lightninglabs/darepo-client/vhtlcrecovery/coordinator"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestArmVHTLCRecoveryRequiresRequestID verifies that the RPC boundary rejects
// a missing idempotency key before the request can reach durable storage.
func TestArmVHTLCRecoveryRequiresRequestID(t *testing.T) {
	t.Parallel()

	walletReady := make(chan struct{})
	close(walletReady)

	rpcServer := &RPCServer{
		server: &Server{
			walletReady:   walletReady,
			vhtlcRecovery: &coordinator.Service{},
		},
	}

	_, err := rpcServer.ArmVHTLCRecovery(
		context.Background(), &daemonrpc.ArmVHTLCRecoveryRequest{},
	)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "request_id")
}

// TestCancelVHTLCRecoveryMissingIsIdempotent verifies stale swap metadata can
// cancel a recovery row that disappeared with a local database reset without
// wedging the swap FSM behind a permanent NotFound.
func TestCancelVHTLCRecoveryMissingIsIdempotent(t *testing.T) {
	t.Parallel()

	walletReady := make(chan struct{})
	close(walletReady)

	recoveryService, err := coordinator.NewService(
		coordinator.ServiceConfig{
			Store:  missingRecoveryStore{},
			Unroll: noopUnrollRegistry{},
		},
	)
	require.NoError(t, err)

	rpcServer := &RPCServer{
		server: &Server{
			walletReady:   walletReady,
			vhtlcRecovery: recoveryService,
		},
	}

	resp, err := rpcServer.CancelVHTLCRecovery(
		context.Background(), &daemonrpc.CancelVHTLCRecoveryRequest{
			RecoveryId: "missing-recovery",
			Reason:     "cooperative settlement observed",
		},
	)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Nil(t, resp.GetStatus())
}

// TestRecoveryPolicyTemplateRoundTrip verifies that recovery target
// materialization can reconstruct the semantic vHTLC policy needed by custom
// refresh from the durable recovery tuple.
func TestRecoveryPolicyTemplateRoundTrip(t *testing.T) {
	t.Parallel()

	policy, preimage, senderPriv, receiverPriv, serverPriv :=
		testVHTLCPolicyFixture(t)
	expected, err := policy.Template.Encode()
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	preimageHash := preimage.Hash()
	got, err := recoveryPolicyTemplate(vhtlcrecovery.RecoveryJob{
		SenderPubkey: cloneRPCBytes(
			senderPriv.PubKey().SerializeCompressed(),
		),
		ReceiverPubkey: cloneRPCBytes(
			receiverPriv.PubKey().SerializeCompressed(),
		),
		ServerPubkey: cloneRPCBytes(
			serverPriv.PubKey().SerializeCompressed(),
		),
		RefundLocktime:                       144,
		UnilateralClaimDelay:                 10,
		UnilateralRefundDelay:                20,
		UnilateralRefundWithoutReceiverDelay: 30,
		PreimageHash:                         preimageHash[:],
	}, pkScript)
	require.NoError(t, err)
	require.Equal(t, expected, got)
}

type missingRecoveryStore struct{}

func (missingRecoveryStore) ArmRecovery(context.Context,
	vhtlcrecovery.RecoveryJob) (*vhtlcrecovery.RecoveryJob, bool, error) {

	return nil, false, errors.New("unexpected arm recovery")
}

func (missingRecoveryStore) GetRecovery(context.Context, string) (
	*vhtlcrecovery.RecoveryJob, error) {

	return nil, db.ErrVHTLCRecoveryJobNotFound
}

func (missingRecoveryStore) ListNonTerminalRecoveries(context.Context) (
	[]vhtlcrecovery.RecoveryJob, error) {

	return nil, errors.New("unexpected list non-terminal recoveries")
}

func (missingRecoveryStore) ListRecoveries(context.Context) (
	[]vhtlcrecovery.RecoveryJob, error) {

	return nil, errors.New("unexpected list recoveries")
}

func (missingRecoveryStore) EscalateRecovery(context.Context, string,
	[]byte) error {

	return errors.New("unexpected escalate recovery")
}

func (missingRecoveryStore) CancelRecovery(context.Context, string, string,
	[]byte) error {

	return errors.New("unexpected cancel recovery")
}

func (missingRecoveryStore) CompleteRecovery(context.Context, string) error {
	return errors.New("unexpected complete recovery")
}

func (missingRecoveryStore) FailRecovery(context.Context, string, error) error {
	return errors.New("unexpected fail recovery")
}

type noopUnrollRegistry struct{}

func (noopUnrollRegistry) EnsureUnroll(context.Context,
	unroll.EnsureUnrollRequest) (*unroll.EnsureUnrollResp, error) {

	return nil, errors.New("unexpected ensure unroll")
}

func (noopUnrollRegistry) GetStatus(context.Context, wire.OutPoint) (
	*unroll.GetStatusResp, error) {

	return nil, errors.New("unexpected get unroll status")
}
