package vtxo

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// redemptionMemoryStore is a deterministic fake for coordinator tests.
type redemptionMemoryStore struct {
	mu      sync.Mutex
	rows    map[wire.OutPoint]*Descriptor
	pending map[wire.OutPoint]PendingRedemption
	lists   []VTXOStatus
}

// newRedemptionMemoryStore constructs a fake populated with descriptors.
func newRedemptionMemoryStore(descs ...*Descriptor) *redemptionMemoryStore {
	store := &redemptionMemoryStore{
		rows:    make(map[wire.OutPoint]*Descriptor, len(descs)),
		pending: make(map[wire.OutPoint]PendingRedemption),
	}
	for _, desc := range descs {
		store.rows[desc.Outpoint] = cloneRedemptionDescriptor(desc)
	}

	return store
}

// ListVTXOsByStatus returns only rows with the requested local status.
func (s *redemptionMemoryStore) ListVTXOsByStatus(_ context.Context,
	status VTXOStatus) ([]*Descriptor, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.lists = append(s.lists, status)
	result := make([]*Descriptor, 0)
	for _, desc := range s.rows {
		if desc.Status == status {
			result = append(result, cloneRedemptionDescriptor(desc))
		}
	}

	return result, nil
}

// GetVTXO returns one cloned descriptor.
func (s *redemptionMemoryStore) GetVTXO(_ context.Context,
	outpoint wire.OutPoint) (*Descriptor, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	desc, ok := s.rows[outpoint]
	if !ok {
		return nil, ErrVTXONotFound
	}

	return cloneRedemptionDescriptor(desc), nil
}

// MarkVTXORedeeming advances an expired row to Redeeming.
func (s *redemptionMemoryStore) MarkVTXORedeeming(_ context.Context,
	roundID string, outpoint wire.OutPoint) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	desc, ok := s.rows[outpoint]
	if !ok {
		return ErrVTXONotFound
	}
	if desc.Status == VTXOStatusRedeeming {
		if desc.RedemptionRoundID.IsSome() &&
			desc.RedemptionRoundID.UnsafeFromSome() == roundID {
			return nil
		}

		return fmt.Errorf("owned by another round")
	}
	if desc.Status != VTXOStatusExpired {
		return fmt.Errorf("cannot mark %s redeeming", desc.Status)
	}
	desc.Status = VTXOStatusRedeeming
	desc.RedemptionRoundID = fn.Some(roundID)

	return nil
}

// RevertVTXORedeeming returns Redeeming or already-Expired rows to Expired.
func (s *redemptionMemoryStore) RevertVTXORedeeming(_ context.Context,
	roundID string, outpoint wire.OutPoint) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	desc, ok := s.rows[outpoint]
	if !ok {
		return ErrVTXONotFound
	}
	if desc.Status == VTXOStatusExpired && desc.RedemptionRoundID.IsNone() {
		return nil
	}
	if desc.Status != VTXOStatusRedeeming ||
		desc.RedemptionRoundID.IsNone() ||
		desc.RedemptionRoundID.UnsafeFromSome() != roundID {
		return fmt.Errorf("cannot revert %s", desc.Status)
	}
	desc.Status = VTXOStatusExpired
	desc.RedemptionRoundID = fn.None[string]()

	return nil
}

// FinalizeVTXORedemption saves the replacement and links the old row.
func (s *redemptionMemoryStore) FinalizeVTXORedemption(_ context.Context,
	source wire.OutPoint, replacement *Descriptor) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	desc, ok := s.rows[source]
	if !ok {
		return ErrVTXONotFound
	}
	if desc.Status != VTXOStatusExpired &&
		desc.Status != VTXOStatusRedeeming &&
		desc.Status != VTXOStatusRedeemed {
		return fmt.Errorf("cannot finalize %s", desc.Status)
	}
	if desc.Status == VTXOStatusRedeemed &&
		(desc.RedemptionRoundID.IsNone() ||
			desc.RedemptionRoundID.UnsafeFromSome() !=
				replacement.RoundID) {
		return fmt.Errorf("redemption finalized by another round")
	}
	if desc.ReplacedBy.IsSome() &&
		desc.ReplacedBy.UnsafeFromSome() != replacement.Outpoint {
		return fmt.Errorf("conflicting replacement")
	}

	s.rows[replacement.Outpoint] = cloneRedemptionDescriptor(replacement)
	desc.Status = VTXOStatusRedeemed
	desc.ReplacedBy = fn.Some(replacement.Outpoint)
	desc.RedemptionRoundID = fn.Some(replacement.RoundID)
	pending := PendingRedemption{
		Source:      source,
		Replacement: replacement.Outpoint,
		RoundID:     replacement.RoundID,
	}
	if existing, ok := s.pending[source]; ok && existing != pending {
		return fmt.Errorf("conflicting redemption outbox")
	}
	s.pending[source] = pending

	return nil
}

// ListPendingVTXORedemptions returns the durable observer replay queue.
func (s *redemptionMemoryStore) ListPendingVTXORedemptions(_ context.Context) (
	[]PendingRedemption, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]PendingRedemption, 0, len(s.pending))
	for _, pending := range s.pending {
		result = append(result, pending)
	}

	return result, nil
}

// AcknowledgeVTXORedemption deletes one exact observer replay tuple.
func (s *redemptionMemoryStore) AcknowledgeVTXORedemption(_ context.Context,
	pending PendingRedemption) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.pending[pending.Source]
	if !ok {
		return nil
	}
	if existing != pending {
		return fmt.Errorf("conflicting redemption acknowledgement")
	}
	delete(s.pending, pending.Source)

	return nil
}

// TestRedemptionCoordinatorReplaysFinalizedObserverAfterCrash verifies the
// terminal source link cannot hide observer work after an error or restart.
func TestRedemptionCoordinatorReplaysFinalizedObserverAfterCrash(t *testing.T) {
	t.Parallel()

	source := testRedemptionDescriptor(31, VTXOStatusExpired)
	replacement := cloneRedemptionDescriptor(source)
	replacement.Outpoint = testRedemptionDescriptor(
		32, VTXOStatusLive,
	).Outpoint
	replacement.RoundID = "replacement-round"
	replacement.BatchExpiry = source.BatchExpiry + 144
	replacement.Status = VTXOStatusLive
	store := newRedemptionMemoryStore(source)

	firstObserverCalls := 0
	first := NewRedemptionCoordinator(RedemptionCoordinatorConfig{
		Store: store,
		FinalizedObserver: func(context.Context, wire.OutPoint,
			*Descriptor) error {

			firstObserverCalls++

			return fmt.Errorf("observer unavailable")
		},
	})
	require.Error(
		t,
		first.Finalize(
			t.Context(), source.Outpoint, replacement,
		),
	)
	require.Equal(t, 1, firstObserverCalls)
	persisted, err := store.GetVTXO(t.Context(), source.Outpoint)
	require.NoError(t, err)
	require.Equal(t, VTXOStatusRedeemed, persisted.Status)
	require.Len(t, store.pending, 1)

	// A fresh coordinator simulates daemon restart. It drains the local
	// outbox before considering an operator check, then acknowledges the
	// exact tuple. Replaying another pass is a no-op.
	replayed := 0
	restarted := NewRedemptionCoordinator(RedemptionCoordinatorConfig{
		Store: store,
		Checker: func(context.Context, []*Descriptor) (
			[]RedemptionResult, error) {

			t.Fatal(
				"Redeemed observer replay must not query " +
					"operator",
			)

			return nil, nil
		},
		FinalizedObserver: func(_ context.Context,
			gotSource wire.OutPoint,
			gotReplacement *Descriptor) error {

			require.Equal(t, source.Outpoint, gotSource)
			require.Equal(
				t, replacement.Outpoint,
				gotReplacement.Outpoint,
			)
			replayed++

			return nil
		},
	})
	require.NoError(t, restarted.CheckNow(t.Context()))
	require.Equal(t, 1, replayed)
	require.Empty(t, store.pending)
	require.NoError(t, restarted.CheckNow(t.Context()))
	require.Equal(t, 1, replayed)
}

// TestRedemptionCoordinatorQueriesOnlyLocalClaims verifies server state is
// never mirrored or queried for live VTXOs. Redeeming is included so a
// checkpointed claim can converge after a crash or stale server lock cleanup.
func TestRedemptionCoordinatorQueriesOnlyLocalClaims(t *testing.T) {
	t.Parallel()

	expired := testRedemptionDescriptor(1, VTXOStatusExpired)
	redeeming := testRedemptionDescriptor(2, VTXOStatusRedeeming)
	redeeming.RedemptionRoundID = fn.Some("recovering-round")
	live := testRedemptionDescriptor(3, VTXOStatusLive)
	store := newRedemptionMemoryStore(expired, redeeming, live)

	var checked []*Descriptor
	coordinator := NewRedemptionCoordinator(RedemptionCoordinatorConfig{
		Store: store,
		Checker: func(_ context.Context, descs []*Descriptor) (
			[]RedemptionResult, error) {

			checked = descs

			return nil, nil
		},
		Submitter: func(context.Context, string, []*Descriptor) error {
			t.Fatal("omitted result must not submit")

			return nil
		},
	})

	require.NoError(t, coordinator.CheckNow(t.Context()))
	require.Len(t, checked, 2)
	checkedOutpoints := fn.NewSet[wire.OutPoint]()
	for _, descriptor := range checked {
		checkedOutpoints.Add(descriptor.Outpoint)
	}
	require.True(t, checkedOutpoints.Contains(expired.Outpoint))
	require.True(t, checkedOutpoints.Contains(redeeming.Outpoint))
	require.False(t, checkedOutpoints.Contains(live.Outpoint))
	require.Equal(t, []VTXOStatus{
		VTXOStatusExpired, VTXOStatusRedeeming,
	}, store.lists)
}

// TestRedemptionCoordinatorDeduplicatesSubmission verifies repeated polls do
// not submit a second local round while the first submission is unresolved.
func TestRedemptionCoordinatorDeduplicatesSubmission(t *testing.T) {
	t.Parallel()

	expired := testRedemptionDescriptor(3, VTXOStatusExpired)
	store := newRedemptionMemoryStore(expired)
	var submissions int
	coordinator := NewRedemptionCoordinator(RedemptionCoordinatorConfig{
		Store: store,
		Checker: func(_ context.Context, _ []*Descriptor) (
			[]RedemptionResult, error) {

			return []RedemptionResult{{
				Source:       expired.Outpoint,
				Redeemable:   true,
				ClaimRoundID: "dedup-round",
			}}, nil
		},
		Submitter: func(_ context.Context, claimRoundID string,
			descs []*Descriptor) error {

			require.Equal(t, "dedup-round", claimRoundID)
			require.Len(t, descs, 1)
			submissions++

			return nil
		},
	})

	require.NoError(t, coordinator.CheckNow(t.Context()))
	require.NoError(t, coordinator.CheckNow(t.Context()))
	require.Equal(t, 1, submissions)

	// A pre-checkpoint round failure releases the in-memory submission and
	// leaves/reverts the durable source to Expired for a later retry.
	require.NoError(t, coordinator.RevertRedeeming(
		t.Context(), "", []wire.OutPoint{expired.Outpoint},
	))
	require.NoError(t, coordinator.CheckNow(t.Context()))
	require.Equal(t, 2, submissions)
}

// TestRedemptionCoordinatorBoundsClaimBundles verifies a recovery set larger
// than the server's per-client claim limit is drained over successive rounds
// instead of being submitted as one permanently rejected bundle.
func TestRedemptionCoordinatorBoundsClaimBundles(t *testing.T) {
	t.Parallel()

	const claimCount = maxRedemptionClaimsPerRound + 1
	descriptors := make([]*Descriptor, 0, claimCount)
	for i := 0; i < claimCount; i++ {
		desc := testRedemptionDescriptor(byte(i), VTXOStatusExpired)
		desc.Outpoint.Index = uint32(i)
		descriptors = append(descriptors, desc)
	}
	store := newRedemptionMemoryStore(descriptors...)

	var submissions [][]wire.OutPoint
	coordinator := NewRedemptionCoordinator(RedemptionCoordinatorConfig{
		Store: store,
		Checker: func(_ context.Context, checked []*Descriptor) (
			[]RedemptionResult, error) {

			results := make([]RedemptionResult, 0, len(checked))
			for _, desc := range checked {
				results = append(results, RedemptionResult{
					Source:       desc.Outpoint,
					Redeemable:   true,
					ClaimRoundID: "bounded-claim-round",
				})
			}

			return results, nil
		},
		Submitter: func(_ context.Context, claimRoundID string,
			claimed []*Descriptor) error {

			require.Equal(t, "bounded-claim-round", claimRoundID)
			outpoints := make([]wire.OutPoint, len(claimed))
			for i, desc := range claimed {
				outpoints[i] = desc.Outpoint
			}
			submissions = append(submissions, outpoints)

			return nil
		},
	})

	require.NoError(t, coordinator.CheckNow(t.Context()))
	require.NoError(t, coordinator.CheckNow(t.Context()))
	require.Len(t, submissions, 2)
	require.Len(t, submissions[0], maxRedemptionClaimsPerRound)
	require.Len(t, submissions[1], 1)

	claimed := fn.NewSet[wire.OutPoint]()
	for _, submission := range submissions {
		for _, outpoint := range submission {
			require.False(t, claimed.Contains(outpoint))
			claimed.Add(outpoint)
		}
	}
	require.Equal(t, uint(claimCount), claimed.Size())
}

// TestRedemptionCoordinatorReleasesOrphanBeforeResubmit verifies a restart
// cannot carry an obsolete round lease into a newly submitted claim. The
// server's redeemable answer is the proof that the prior round abandoned it.
func TestRedemptionCoordinatorReleasesOrphanBeforeResubmit(t *testing.T) {
	t.Parallel()

	redeeming := testRedemptionDescriptor(8, VTXOStatusRedeeming)
	redeeming.RedemptionRoundID = fn.Some("abandoned-round")
	store := newRedemptionMemoryStore(redeeming)

	var submitted *Descriptor
	coordinator := NewRedemptionCoordinator(RedemptionCoordinatorConfig{
		Store: store,
		Checker: func(_ context.Context, descs []*Descriptor) (
			[]RedemptionResult, error) {

			require.Len(t, descs, 1)

			return []RedemptionResult{{
				Source:       redeeming.Outpoint,
				Redeemable:   true,
				ClaimRoundID: "replacement-round",
			}}, nil
		},
		Submitter: func(ctx context.Context, claimRoundID string,
			descs []*Descriptor) error {

			require.Equal(t, "replacement-round", claimRoundID)
			require.Len(t, descs, 1)
			submitted = cloneRedemptionDescriptor(descs[0])

			persisted, err := store.GetVTXO(
				ctx, redeeming.Outpoint,
			)
			require.NoError(t, err)
			require.Equal(t, VTXOStatusExpired, persisted.Status)
			require.True(t, persisted.RedemptionRoundID.IsNone())

			return nil
		},
	})

	require.NoError(t, coordinator.CheckNow(t.Context()))
	require.NotNil(t, submitted)
	require.Equal(t, VTXOStatusExpired, submitted.Status)
	require.True(t, submitted.RedemptionRoundID.IsNone())
}

// TestRedemptionCoordinatorBackoffAndTriggerReset verifies sparse operator
// answers back off 30s, 60s, 120s while an explicit block/reconnect trigger
// retries immediately and resets the next interval to the base delay.
func TestRedemptionCoordinatorBackoffAndTriggerReset(t *testing.T) {
	t.Parallel()

	start := time.Unix(1_700_000_000, 0)
	ticks := make(chan time.Duration, 16)
	testClock := clock.NewTestClockWithTickSignal(start, ticks)
	expired := testRedemptionDescriptor(9, VTXOStatusExpired)
	store := newRedemptionMemoryStore(expired)
	checks := make(chan struct{}, 16)

	coordinator := NewRedemptionCoordinator(RedemptionCoordinatorConfig{
		Store: store,
		Checker: func(context.Context, []*Descriptor) (
			[]RedemptionResult, error) {

			checks <- struct{}{}

			return nil, nil
		},
		Clock:           testClock,
		PollInterval:    30 * time.Second,
		MaxPollInterval: 5 * time.Minute,
		Jitter: func(delay time.Duration) time.Duration {
			return delay
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	coordinator.Start(ctx)

	require.Equal(
		t, 30*time.Second, receiveRedemptionTestValue(t, ticks),
	)
	receiveRedemptionTestValue(t, checks) // Immediate startup Trigger.
	require.Equal(
		t, 30*time.Second, receiveRedemptionTestValue(t, ticks),
	)

	testClock.SetTime(start.Add(30 * time.Second))
	receiveRedemptionTestValue(t, checks)
	require.Equal(
		t, 60*time.Second, receiveRedemptionTestValue(t, ticks),
	)

	testClock.SetTime(start.Add(90 * time.Second))
	receiveRedemptionTestValue(t, checks)
	require.Equal(
		t, 120*time.Second, receiveRedemptionTestValue(t, ticks),
	)

	testClock.SetTime(start.Add(210 * time.Second))
	receiveRedemptionTestValue(t, checks)
	require.Equal(
		t, 240*time.Second, receiveRedemptionTestValue(t, ticks),
	)

	testClock.SetTime(start.Add(450 * time.Second))
	receiveRedemptionTestValue(t, checks)
	require.Equal(
		t, 5*time.Minute, receiveRedemptionTestValue(t, ticks),
	)

	testClock.SetTime(start.Add(750 * time.Second))
	receiveRedemptionTestValue(t, checks)
	require.Equal(
		t, 5*time.Minute, receiveRedemptionTestValue(t, ticks),
	)

	// A block/reconnect edge is immediate and resets exponential retry.
	coordinator.Trigger()
	receiveRedemptionTestValue(t, checks)
	require.Equal(
		t, 30*time.Second, receiveRedemptionTestValue(t, ticks),
	)
}

// TestJitterRedemptionDelayBounds verifies the production jitter never leaves
// the documented symmetric twenty-percent window.
func TestJitterRedemptionDelayBounds(t *testing.T) {
	t.Parallel()

	const delay = 100 * time.Second
	for range 100 {
		jittered := jitterRedemptionDelay(delay)
		require.GreaterOrEqual(t, jittered, 80*time.Second)
		require.LessOrEqual(t, jittered, 120*time.Second)
	}
	require.Zero(t, jitterRedemptionDelay(0))
}

// TestRedemptionCoordinatorUnimplementedIsRetryable locks in mixed-version
// rollout behavior: an old operator lacking the checker RPC cannot mutate or
// consume the local recovery queue, and a later reconciliation pass retries.
func TestRedemptionCoordinatorUnimplementedIsRetryable(t *testing.T) {
	t.Parallel()

	for _, localStatus := range []VTXOStatus{
		VTXOStatusExpired, VTXOStatusRedeeming,
	} {
		localStatus := localStatus
		t.Run(localStatus.String(), func(t *testing.T) {
			t.Parallel()

			desc := testRedemptionDescriptor(10, localStatus)
			if localStatus == VTXOStatusRedeeming {
				desc.RedemptionRoundID = fn.Some(
					"inflight-round",
				)
			}
			store := newRedemptionMemoryStore(desc)
			var checks, submissions, resolutions int
			coordinator := NewRedemptionCoordinator(
				RedemptionCoordinatorConfig{
					Store: store,
					Checker: func(context.Context,
						[]*Descriptor) (
						[]RedemptionResult, error) {

						checks++

						return nil, status.Error(
							codes.Unimplemented,
							"old operator",
						)
					},
					Submitter: func(context.Context, string,
						[]*Descriptor) error {

						submissions++

						return nil
					},
					ReplacementResolver: func(
						context.Context, *Descriptor,
						wire.OutPoint) (*Descriptor,
						error) {

						resolutions++

						return nil, nil
					},
				},
			)

			for range 2 {
				err := coordinator.CheckNow(t.Context())
				require.Error(t, err)
				require.Equal(
					t, codes.Unimplemented,
					status.Code(err),
				)
			}
			require.Equal(t, 2, checks)
			require.Zero(t, submissions)
			require.Zero(t, resolutions)

			persisted, err := store.GetVTXO(
				t.Context(), desc.Outpoint,
			)
			require.NoError(t, err)
			require.Equal(t, localStatus, persisted.Status)
			require.Equal(
				t, desc.RedemptionRoundID,
				persisted.RedemptionRoundID,
			)
			require.True(t, persisted.ReplacedBy.IsNone())
		})
	}
}

// TestRedemptionCoordinatorRecoversExactReplacement verifies another
// participant's completed reissue is repaired locally using the canonical
// replacement link.
func TestRedemptionCoordinatorRecoversExactReplacement(t *testing.T) {
	t.Parallel()

	expired := testRedemptionDescriptor(4, VTXOStatusExpired)
	replacement := testRedemptionDescriptor(5, VTXOStatusLive)
	replacement.Amount = expired.Amount
	replacement.PolicyTemplate = append(
		[]byte(nil), expired.PolicyTemplate...,
	)
	replacement.PkScript = append([]byte(nil), expired.PkScript...)
	store := newRedemptionMemoryStore(expired)

	coordinator := NewRedemptionCoordinator(RedemptionCoordinatorConfig{
		Store: store,
		Checker: func(_ context.Context, _ []*Descriptor) (
			[]RedemptionResult, error) {

			return []RedemptionResult{{
				Source:      expired.Outpoint,
				Replacement: &replacement.Outpoint,
			}}, nil
		},
		ReplacementResolver: func(_ context.Context, _ *Descriptor,
			outpoint wire.OutPoint) (*Descriptor, error) {

			require.Equal(t, replacement.Outpoint, outpoint)

			return replacement, nil
		},
	})

	require.NoError(t, coordinator.CheckNow(t.Context()))
	old, err := store.GetVTXO(t.Context(), expired.Outpoint)
	require.NoError(t, err)
	require.Equal(t, VTXOStatusRedeemed, old.Status)
	require.Equal(t, replacement.Outpoint, old.ReplacedBy.UnsafeFromSome())
	newDesc, err := store.GetVTXO(t.Context(), replacement.Outpoint)
	require.NoError(t, err)
	require.Equal(t, VTXOStatusLive, newDesc.Status)
}

// TestRedemptionCoordinatorChainsExpiredReplacement verifies a client that
// stayed offline through multiple expiry windows does not resurrect the first
// replacement as live. Finalizing the old source preserves the replacement's
// Expired status, and the next selective pass asks only about that new claim.
func TestRedemptionCoordinatorChainsExpiredReplacement(t *testing.T) {
	t.Parallel()

	source := testRedemptionDescriptor(41, VTXOStatusExpired)
	replacement := cloneRedemptionDescriptor(source)
	replacement.Outpoint = testRedemptionDescriptor(
		42, VTXOStatusExpired,
	).Outpoint
	replacement.RoundID = "expired-replacement-round"
	replacement.BatchExpiry = source.BatchExpiry + 144
	replacement.Status = VTXOStatusExpired
	store := newRedemptionMemoryStore(source)

	var checked [][]wire.OutPoint
	observed := 0
	coordinator := NewRedemptionCoordinator(RedemptionCoordinatorConfig{
		Store: store,
		Checker: func(_ context.Context, descriptors []*Descriptor) (
			[]RedemptionResult, error) {

			outpoints := make([]wire.OutPoint, len(descriptors))
			for i, descriptor := range descriptors {
				outpoints[i] = descriptor.Outpoint
			}
			checked = append(checked, outpoints)
			if len(checked) == 1 {
				return []RedemptionResult{{
					Source:      source.Outpoint,
					Replacement: &replacement.Outpoint,
				}}, nil
			}

			return nil, nil
		},
		ReplacementResolver: func(_ context.Context, _ *Descriptor,
			outpoint wire.OutPoint) (*Descriptor, error) {

			require.Equal(t, replacement.Outpoint, outpoint)

			return replacement, nil
		},
		FinalizedObserver: func(_ context.Context,
			gotSource wire.OutPoint,
			gotReplacement *Descriptor) error {

			require.Equal(t, source.Outpoint, gotSource)
			require.Equal(
				t, VTXOStatusExpired, gotReplacement.Status,
			)
			observed++

			return nil
		},
	})

	require.NoError(t, coordinator.CheckNow(t.Context()))
	require.NoError(t, coordinator.CheckNow(t.Context()))
	require.Equal(t, 1, observed)
	require.Equal(t, [][]wire.OutPoint{
		{source.Outpoint},
		{replacement.Outpoint},
	}, checked)

	old, err := store.GetVTXO(t.Context(), source.Outpoint)
	require.NoError(t, err)
	require.Equal(t, VTXOStatusRedeemed, old.Status)
	newDesc, err := store.GetVTXO(t.Context(), replacement.Outpoint)
	require.NoError(t, err)
	require.Equal(t, VTXOStatusExpired, newDesc.Status)
	require.Empty(t, store.pending)
}

// TestRedemptionCoordinatorRejectsChangedReplacement locks in the full-value,
// exact-script reissue policy.
func TestRedemptionCoordinatorRejectsChangedReplacement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Descriptor)
	}{
		{
			name: "amount",
			mutate: func(desc *Descriptor) {
				desc.Amount--
			},
		},
		{
			name: "policy",
			mutate: func(desc *Descriptor) {
				desc.PolicyTemplate[0] ^= 1
			},
		},
		{
			name: "pkScript",
			mutate: func(desc *Descriptor) {
				desc.PkScript[0] ^= 1
			},
		},
		{
			name: "batch expiry",
			mutate: func(desc *Descriptor) {
				desc.BatchExpiry = 0
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			expired := testRedemptionDescriptor(
				6, VTXOStatusExpired,
			)
			replacement := testRedemptionDescriptor(
				7, VTXOStatusLive,
			)
			replacement.Amount = expired.Amount
			replacement.PolicyTemplate = append(
				[]byte(nil), expired.PolicyTemplate...,
			)
			replacement.PkScript = append(
				[]byte(nil), expired.PkScript...,
			)
			test.mutate(replacement)
			store := newRedemptionMemoryStore(expired)
			source := expired.Outpoint
			replacementOutpoint := replacement.Outpoint
			checkerResults := []RedemptionResult{{
				Source:      source,
				Replacement: &replacementOutpoint,
			}}
			coordinator := NewRedemptionCoordinator(
				RedemptionCoordinatorConfig{
					Store: store,
					Checker: func(_ context.Context,
						_ []*Descriptor) (
						[]RedemptionResult, error) {

						return checkerResults, nil
					},
					ReplacementResolver: func(
						context.Context, *Descriptor,
						wire.OutPoint) (*Descriptor,
						error) {

						return replacement, nil
					},
				},
			)

			require.Error(t, coordinator.CheckNow(t.Context()))
			old, err := store.GetVTXO(t.Context(), expired.Outpoint)
			require.NoError(t, err)
			require.Equal(t, VTXOStatusExpired, old.Status)
		})
	}
}

// testRedemptionDescriptor returns a minimal deterministic descriptor.
func testRedemptionDescriptor(index byte, status VTXOStatus) *Descriptor {
	var hash chainhash.Hash
	hash[0] = index

	return &Descriptor{
		Outpoint: wire.OutPoint{
			Hash:  hash,
			Index: uint32(index),
		},
		Amount: btcutil.Amount(10_000 + int(index)),
		PolicyTemplate: []byte{
			0xaa,
			index,
		},
		PkScript: []byte{
			0x51,
			0x20,
			index,
		},
		RoundID:     fmt.Sprintf("round-%d", index),
		BatchExpiry: 1_000 + int32(index),
		Status:      status,
	}
}

// cloneRedemptionDescriptor clones mutable fields used by these tests.
func cloneRedemptionDescriptor(desc *Descriptor) *Descriptor {
	clone := *desc
	clone.PolicyTemplate = append([]byte(nil), desc.PolicyTemplate...)
	clone.PkScript = append([]byte(nil), desc.PkScript...)

	return &clone
}

// receiveRedemptionTestValue prevents a broken background-loop test from
// hanging the package until the global go test timeout.
func receiveRedemptionTestValue[T any](t *testing.T, values <-chan T) T {
	t.Helper()

	select {
	case value := <-values:
		return value

	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for redemption coordinator")

		var zero T

		return zero
	}
}

var _ RedemptionStore = (*redemptionMemoryStore)(nil)
