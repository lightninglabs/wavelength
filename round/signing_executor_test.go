package round

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestSigningExecutorSharedBound verifies that simultaneous batches share the
// same executor-wide concurrency limit.
func TestSigningExecutorSharedBound(t *testing.T) {
	t.Parallel()

	executor := newTestSigningExecutor(t, 2)
	started := make(chan struct{}, 8)
	release := make(chan struct{})

	var active atomic.Int32
	var maxActive atomic.Int32
	executor.createSession = func(job CreateSignerSessionJob) (
		*tree.SignerSession, error) {

		current := active.Add(1)
		for {
			oldMax := maxActive.Load()
			if current <= oldMax || maxActive.CompareAndSwap(
				oldMax, current,
			) {

				break
			}
		}

		started <- struct{}{}
		<-release
		active.Add(-1)

		return &tree.SignerSession{}, nil
	}
	executor.getNonces = func(
		*tree.SignerSession) map[tree.TxID]tree.Musig2PubNonce {

		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := executor.CreateSessions(
				ctx, makeSignerSessionJobs(4),
			)
			results <- err
		}()
	}

	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			require.NoError(t, ctx.Err())
		}
	}
	close(release)

	for range 2 {
		require.NoError(t, <-results)
	}
	require.Equal(t, int32(2), maxActive.Load())
}

// TestSigningExecutorSerialOrder verifies that one worker retains the input
// execution order used by the old sequential loops.
func TestSigningExecutorSerialOrder(t *testing.T) {
	t.Parallel()

	executor := newTestSigningExecutor(t, 1)
	jobs := makeSignerSessionJobs(5)
	sessions := make([]*tree.SignerSession, len(jobs))
	for index := range sessions {
		sessions[index] = &tree.SignerSession{}
	}

	var order []byte
	executor.createSession = func(job CreateSignerSessionJob) (
		*tree.SignerSession, error) {

		order = append(order, job.SignerKey[0])

		return sessions[int(job.SignerKey[0])], nil
	}
	executor.getNonces = func(
		*tree.SignerSession) map[tree.TxID]tree.Musig2PubNonce {

		return nil
	}

	results, err := executor.CreateSessions(context.Background(), jobs)
	require.NoError(t, err)
	require.Equal(t, []byte{0, 1, 2, 3, 4}, order)
	require.Len(t, results, len(jobs))
	for index, result := range results {
		require.Equal(t, jobs[index].SignerKey, result.SignerKey)
		require.Same(t, sessions[index], result.Session)
	}
}

// TestSigningExecutorIndexedResults verifies that completion order cannot
// reorder protocol results.
func TestSigningExecutorIndexedResults(t *testing.T) {
	t.Parallel()

	executor := newTestSigningExecutor(t, 3)
	jobs := makeSignerSessionJobs(3)
	sessions := []*tree.SignerSession{
		{}, {}, {},
	}
	started := make(chan byte, len(jobs))
	completed := make(chan byte, len(jobs))
	releases := []chan struct{}{
		make(chan struct{}),
		make(chan struct{}),
		make(chan struct{}),
	}
	executor.createSession = func(job CreateSignerSessionJob) (
		*tree.SignerSession, error) {

		index := int(job.SignerKey[0])
		started <- job.SignerKey[0]
		<-releases[index]
		completed <- job.SignerKey[0]

		return sessions[index], nil
	}
	executor.getNonces = func(
		*tree.SignerSession) map[tree.TxID]tree.Musig2PubNonce {

		return nil
	}

	type createResult struct {
		results []SignerSessionResult
		err     error
	}
	resultChan := make(chan createResult, 1)
	go func() {
		results, err := executor.CreateSessions(
			context.Background(), jobs,
		)
		resultChan <- createResult{results: results, err: err}
	}()

	for range jobs {
		<-started
	}
	for index := len(releases) - 1; index >= 0; index-- {
		close(releases[index])
		require.Equal(t, byte(index), <-completed)
	}

	result := <-resultChan
	require.NoError(t, result.err)
	for index := range jobs {
		require.Equal(
			t, jobs[index].SignerKey,
			result.results[index].SignerKey,
		)
		require.Same(t, sessions[index], result.results[index].Session)
	}
}

// TestSigningExecutorCreateFailure verifies all-or-nothing results and
// cleanup of sessions created before failure.
func TestSigningExecutorCreateFailure(t *testing.T) {
	t.Parallel()

	executor := newTestSigningExecutor(t, 1)
	jobs := makeSignerSessionJobs(3)
	sessions := []*tree.SignerSession{{}, {}, {}}
	errOne := errors.New("one failed")
	var createCalls atomic.Int32
	executor.createSession = func(job CreateSignerSessionJob) (
		*tree.SignerSession, error) {

		createCalls.Add(1)
		index := int(job.SignerKey[0])
		switch index {
		case 0:
			return sessions[index], nil

		case 1:
			return nil, errOne

		default:
			return sessions[index], nil
		}
	}
	executor.getNonces = func(
		*tree.SignerSession) map[tree.TxID]tree.Musig2PubNonce {

		return nil
	}

	var cleanupCalls atomic.Int32
	cleanedSessions := make(chan *tree.SignerSession, 1)
	executor.cleanupSession = func(session *tree.SignerSession) error {
		cleanedSessions <- session
		cleanupCalls.Add(1)

		return nil
	}

	results, err := executor.CreateSessions(context.Background(), jobs)
	require.Nil(t, results)
	require.ErrorIs(t, err, errOne)
	require.Equal(t, int32(2), createCalls.Load())
	require.Equal(t, int32(1), cleanupCalls.Load())
	require.Same(t, sessions[0], <-cleanedSessions)
}

// TestSigningExecutorConcurrentFailures verifies that failures from jobs
// already running at cancellation remain available in deterministic order.
func TestSigningExecutorConcurrentFailures(t *testing.T) {
	t.Parallel()

	executor := newTestSigningExecutor(t, 2)
	errZero := errors.New("zero failed")
	errOne := errors.New("one failed")
	jobErrors := []error{errZero, errOne}

	var started sync.WaitGroup
	started.Add(len(jobErrors))
	err := executor.run(t.Context(), len(jobErrors), func(index int) error {
		started.Done()
		started.Wait()

		return jobErrors[index]
	})

	require.ErrorIs(t, err, errZero)
	require.ErrorIs(t, err, errOne)
	require.EqualError(t, err, "zero failed\none failed")
}

// TestSigningExecutorCancellation verifies cancellation stops new jobs and
// cleans a session returned by an already-running signer call.
func TestSigningExecutorCancellation(t *testing.T) {
	t.Parallel()

	executor := newTestSigningExecutor(t, 1)
	jobs := makeSignerSessionJobs(3)
	session := &tree.SignerSession{}
	started := make(chan struct{})
	release := make(chan struct{})

	var createCalls atomic.Int32
	executor.createSession = func(CreateSignerSessionJob) (
		*tree.SignerSession, error) {

		createCalls.Add(1)
		close(started)
		<-release

		return session, nil
	}
	executor.getNonces = func(
		*tree.SignerSession) map[tree.TxID]tree.Musig2PubNonce {

		return nil
	}

	var cleanupCalls atomic.Int32
	cleanedSessions := make(chan *tree.SignerSession, 1)
	executor.cleanupSession = func(session *tree.SignerSession) error {
		cleanedSessions <- session
		cleanupCalls.Add(1)

		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	type createResult struct {
		results []SignerSessionResult
		err     error
	}
	resultChan := make(chan createResult, 1)
	go func() {
		results, err := executor.CreateSessions(ctx, jobs)
		resultChan <- createResult{results: results, err: err}
	}()
	<-started
	cancel()
	close(release)

	result := <-resultChan
	require.Nil(t, result.results)
	require.ErrorIs(t, result.err, context.Canceled)
	require.Equal(t, int32(1), createCalls.Load())
	require.Equal(t, int32(1), cleanupCalls.Load())
	require.Same(t, session, <-cleanedSessions)
}

// TestSigningExecutorSign verifies parallel signing remains index-stable and
// cleans every distinct session after all signing calls finish.
func TestSigningExecutorSign(t *testing.T) {
	t.Parallel()

	executor := newTestSigningExecutor(t, 2)
	sessions := makeSignerSessionResults(3)

	var mu sync.Mutex
	signed := make(map[*tree.SignerSession]struct{})
	executor.signSession = func(session *tree.SignerSession) (
		map[tree.TxID]*musig2.PartialSignature, error) {

		mu.Lock()
		signed[session] = struct{}{}
		mu.Unlock()

		return nil, nil
	}

	cleaned := make(map[*tree.SignerSession]int)
	executor.cleanupSession = func(session *tree.SignerSession) error {
		mu.Lock()
		defer mu.Unlock()

		require.Contains(t, signed, session)
		cleaned[session]++

		return nil
	}

	results, err := executor.Sign(context.Background(), sessions)
	require.NoError(t, err)
	require.Len(t, results, len(sessions))
	for index, result := range results {
		require.Equal(t, sessions[index].SignerKey, result.SignerKey)
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, cleaned, len(sessions))
	for _, count := range cleaned {
		require.Equal(t, 1, count)
	}
}

// TestSigningExecutorSignFailure verifies signing and cleanup failures never
// expose a partial result.
func TestSigningExecutorSignFailure(t *testing.T) {
	t.Parallel()

	executor := newTestSigningExecutor(t, 1)
	sessions := makeSignerSessionResults(3)
	signErr := errors.New("sign failed")
	cleanupErr := errors.New("cleanup failed")

	var signCalls atomic.Int32
	executor.signSession = func(*tree.SignerSession) (
		map[tree.TxID]*musig2.PartialSignature, error) {

		call := signCalls.Add(1)
		if call == 2 {
			return nil, signErr
		}

		return nil, nil
	}

	var cleanupCalls atomic.Int32
	executor.cleanupSession = func(session *tree.SignerSession) error {
		call := cleanupCalls.Add(1)
		if call == 3 {
			return cleanupErr
		}

		return nil
	}

	results, err := executor.Sign(context.Background(), sessions)
	require.Nil(t, results)
	require.ErrorIs(t, err, signErr)
	require.ErrorIs(t, err, cleanupErr)
	require.Equal(t, int32(2), signCalls.Load())
	require.Equal(t, int32(3), cleanupCalls.Load())
}

// TestSigningExecutorRealMuSigManager exercises concurrent session creation
// through the same LND manager embedded by lwwallet and btcwallet. The gated
// key fetcher proves four real manager calls overlap without relying on sleeps.
func TestSigningExecutorRealMuSigManager(t *testing.T) {
	t.Parallel()

	const workerCount = 4

	h := newTestHarness(t)
	started := make(chan struct{}, workerCount)
	release := make(chan struct{})
	var fetchCalls atomic.Int32
	signer := input.NewMusigSessionManager(func(*keychain.KeyDescriptor) (
		*btcec.PrivateKey, error) {

		call := fetchCalls.Add(1)
		if call <= workerCount {
			started <- struct{}{}
			<-release
		}

		return h.clientPrivKey, nil
	})

	vtxoTree, _ := h.newTestVTXOTree(8)
	prevOuts, err := vtxoTree.Root.PrevOutputFetcher(
		vtxoTree.BatchOutput,
	)
	require.NoError(t, err)

	jobs := make([]CreateSignerSessionJob, 8)
	for index := range jobs {
		jobs[index] = CreateSignerSessionJob{
			SignerKey: SignerKey{
				byte(index),
			},
			Signer: signer,
			SigningKey: keychain.KeyDescriptor{
				PubKey: h.clientPubKey,
			},
			SweepTapscriptRoot: vtxoTree.SweepTapscriptRoot,
			PrevOuts:           prevOuts,
			Root:               vtxoTree.Root,
		}
	}

	type createResult struct {
		results []SignerSessionResult
		err     error
	}
	resultChan := make(chan createResult, 1)
	go func() {
		results, err := NewSigningExecutor(workerCount).CreateSessions(
			t.Context(), jobs,
		)
		resultChan <- createResult{results: results, err: err}
	}()

	for range workerCount {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("real MuSig2 session creation did not overlap")
		}
	}
	close(release)

	result := <-resultChan
	require.NoError(t, result.err)
	require.Len(t, result.results, len(jobs))
	for _, session := range result.results {
		require.NoError(t, session.Session.Cleanup())
	}
}

// makeSignerSessionJobs returns jobs whose first signer-key byte is their
// input index.
func makeSignerSessionJobs(count int) []CreateSignerSessionJob {
	jobs := make([]CreateSignerSessionJob, count)
	for index := range jobs {
		jobs[index].SignerKey[0] = byte(index)
	}

	return jobs
}

// makeSignerSessionResults returns indexed results with distinct sessions.
func makeSignerSessionResults(count int) []SignerSessionResult {
	results := make([]SignerSessionResult, count)
	for index := range results {
		results[index] = SignerSessionResult{
			Session: &tree.SignerSession{},
		}
		results[index].SignerKey[0] = byte(index)
	}

	return results
}

// newTestSigningExecutor returns the concrete executor so tests can replace
// its operation hooks without unchecked type assertions at each call site.
func newTestSigningExecutor(t *testing.T, workers int) *boundedSigningExecutor {
	t.Helper()

	executor, ok := NewSigningExecutor(workers).(*boundedSigningExecutor)
	require.True(t, ok)

	return executor
}
