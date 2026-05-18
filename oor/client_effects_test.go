package oor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOORClientEffectWorkerRunOnceDoneAndRetry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := &fakeOORClientEffectStore{
		effects: []OORClientEffect{
			{
				ID:         "effect-ok",
				EffectType: OORClientEffectSendSubmitPackage,
				ClaimToken: "token-ok",
			},
			{
				ID:         "effect-retry",
				EffectType: OORClientEffectRequestArkSignatures,
				ClaimToken: "token-retry",
			},
		},
	}
	processor := &fakeOORClientEffectProcessor{
		failEffectID: "effect-retry",
	}

	worker := NewOORClientEffectWorker(OORClientEffectWorkerConfig{
		Store:      store,
		Processor:  processor,
		Owner:      "test-worker",
		BatchSize:  10,
		Lease:      time.Minute,
		RetryDelay: time.Second,
	})

	require.NoError(t, worker.RunOnce(ctx))
	require.True(t, store.releasedExpired)
	require.Equal(t, []string{"effect-ok", "effect-retry"}, processor.seen)
	require.Equal(t, []string{"effect-ok:token-ok"}, store.done)
	require.Len(t, store.retried, 1)
	require.Equal(t, "effect-retry:token-retry", store.retried[0])
}

func TestOORClientEffectWorkerLeavesExternalAckClaimOpen(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := &fakeOORClientEffectStore{
		effects: []OORClientEffect{{
			ID:         "effect-awaiting-ack",
			EffectType: OORClientEffectSendFinalizePackage,
			ClaimToken: "token-awaiting-ack",
		}},
	}
	processor := &fakeOORClientEffectProcessor{
		pendingEffectID: "effect-awaiting-ack",
	}

	worker := NewOORClientEffectWorker(OORClientEffectWorkerConfig{
		Store:      store,
		Processor:  processor,
		Owner:      "test-worker",
		BatchSize:  10,
		Lease:      time.Minute,
		RetryDelay: time.Second,
	})

	require.NoError(t, worker.RunOnce(ctx))
	require.Equal(t, []string{"effect-awaiting-ack"}, processor.seen)
	require.Empty(t, store.done)
	require.Empty(t, store.retried)
}

type fakeOORClientEffectStore struct {
	effects []OORClientEffect

	releasedExpired bool
	done            []string
	retried         []string
}

func (s *fakeOORClientEffectStore) ClaimDueOORClientEffects(_ context.Context,
	_ string, _ int, _ time.Duration) ([]OORClientEffect, error) {

	return append([]OORClientEffect(nil), s.effects...), nil
}

func (s *fakeOORClientEffectStore) MarkOORClientEffectDone(_ context.Context,
	id, claimToken string) error {

	s.done = append(s.done, id+":"+claimToken)

	return nil
}

func (s *fakeOORClientEffectStore) ReleaseOORClientEffectForRetry(
	_ context.Context, id, claimToken string, _ time.Duration,
	_ error) error {

	s.retried = append(s.retried, id+":"+claimToken)

	return nil
}

func (s *fakeOORClientEffectStore) ReleaseExpiredOORClientEffectClaims(
	context.Context) error {

	s.releasedExpired = true

	return nil
}

type fakeOORClientEffectProcessor struct {
	failEffectID    string
	pendingEffectID string
	seen            []string
}

func (p *fakeOORClientEffectProcessor) ProcessOORClientEffect(_ context.Context,
	effect OORClientEffect) error {

	p.seen = append(p.seen, effect.ID)
	if effect.ID == p.failEffectID {
		return errors.New("boom")
	}
	if effect.ID == p.pendingEffectID {
		return ErrOORClientEffectAwaitingExternalAck
	}

	return nil
}
