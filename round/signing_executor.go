package round

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// CreateSignerSessionJob contains the inputs needed to create all MuSig2
// sessions for one VTXO signing key.
type CreateSignerSessionJob struct {
	// SignerKey identifies the result in round protocol messages.
	SignerKey SignerKey

	// Signer creates and operates the MuSig2 sessions.
	Signer input.MuSig2Signer

	// SigningKey is the private-key locator for this VTXO path.
	SigningKey keychain.KeyDescriptor

	// SweepTapscriptRoot tweaks the VTXO tree key-spend path.
	SweepTapscriptRoot []byte

	// PrevOuts provides the outputs spent by transactions in the path.
	PrevOuts txscript.PrevOutputFetcher

	// Root is the root of the VTXO tree containing the signer path.
	Root *tree.Node
}

// SignerSessionResult is an index-stable signer session and its public
// nonces.
type SignerSessionResult struct {
	// SignerKey identifies the session in round protocol messages.
	SignerKey SignerKey

	// Session owns all transaction-level MuSig2 sessions for this path.
	Session *tree.SignerSession

	// Nonces contains one public nonce for every transaction in the path.
	Nonces map[tree.TxID]tree.Musig2PubNonce
}

// SignerSessionSignatures contains all partial signatures for one VTXO
// signing key.
type SignerSessionSignatures struct {
	// SignerKey identifies the signatures in round protocol messages.
	SignerKey SignerKey

	// Signatures contains one partial signature for every path transaction.
	Signatures map[tree.TxID]*musig2.PartialSignature
}

// SigningExecutor runs independent VTXO signing sessions with bounded
// concurrency. Each method returns results in the same order as its input.
type SigningExecutor interface {
	// CreateSessions creates the MuSig2 sessions and public nonces for a
	// batch of VTXO signing keys.
	CreateSessions(context.Context,
		[]CreateSignerSessionJob) ([]SignerSessionResult, error)

	// Sign generates partial signatures and cleans every input session.
	// An error returns no partial results.
	Sign(context.Context,
		[]SignerSessionResult) ([]SignerSessionSignatures, error)
}

// boundedSigningExecutor shares one concurrency bound across all batches.
type boundedSigningExecutor struct {
	maxWorkers  int
	workerSlots chan struct{}

	createSession func(CreateSignerSessionJob) (*tree.SignerSession, error)
	getNonces     func(
		*tree.SignerSession) map[tree.TxID]tree.Musig2PubNonce
	signSession func(*tree.SignerSession) (
		map[tree.TxID]*musig2.PartialSignature, error)
	cleanupSession func(*tree.SignerSession) error
}

// NewSigningExecutor creates a signing executor. Values smaller than one
// select the safe, serial behavior.
func NewSigningExecutor(maxWorkers int) SigningExecutor {
	if maxWorkers < 1 {
		maxWorkers = 1
	}

	return &boundedSigningExecutor{
		maxWorkers:  maxWorkers,
		workerSlots: make(chan struct{}, maxWorkers),
		createSession: func(job CreateSignerSessionJob) (
			*tree.SignerSession, error) {

			return tree.NewSignerSession(
				job.Signer, &job.SigningKey,
				job.SweepTapscriptRoot, job.PrevOuts, job.Root,
			)
		},
		getNonces: func(
			session *tree.SignerSession,
		) map[tree.TxID]tree.Musig2PubNonce {

			return session.GetNonces()
		},
		signSession: func(session *tree.SignerSession) (
			map[tree.TxID]*musig2.PartialSignature, error) {

			// Successful calls retain the original signer behavior
			// and clean each transaction session as its signature
			// is produced. The batch cleanup below then handles
			// only failed or unstarted sessions, avoiding an extra
			// remote cleanup RPC per signature.
			return session.Signatures(true)
		},
		cleanupSession: func(session *tree.SignerSession) error {
			return session.Cleanup()
		},
	}
}

// CreateSessions creates signer sessions in parallel while retaining the
// input order in the returned slice.
func (e *boundedSigningExecutor) CreateSessions(ctx context.Context,
	jobs []CreateSignerSessionJob) ([]SignerSessionResult, error) {

	results := make([]SignerSessionResult, len(jobs))
	err := e.run(ctx, len(jobs), func(index int) error {
		job := jobs[index]
		session, err := e.createSession(job)
		if err != nil {
			return fmt.Errorf("create session %d for signer %x: %w",
				index, job.SignerKey[:], err)
		}
		if session == nil {
			return fmt.Errorf("create session %d for signer %x: "+
				"signer returned a nil session", index,
				job.SignerKey[:])
		}

		results[index] = SignerSessionResult{
			SignerKey: job.SignerKey,
			Session:   session,
			Nonces:    e.getNonces(session),
		}

		return nil
	})
	if err == nil {
		return results, nil
	}

	cleanupErr := e.cleanup(results)

	return nil, errors.Join(err, cleanupErr)
}

// Sign generates partial signatures in parallel. Explicit recovery cleanup
// starts only after every in-flight signing call has returned; successful
// signer calls clean their own transaction session inline.
func (e *boundedSigningExecutor) Sign(ctx context.Context,
	sessions []SignerSessionResult) ([]SignerSessionSignatures, error) {

	results := make([]SignerSessionSignatures, len(sessions))
	err := e.run(ctx, len(sessions), func(index int) error {
		session := sessions[index]
		if session.Session == nil {
			return fmt.Errorf("sign session %d for signer %x: "+
				"session is nil", index, session.SignerKey[:])
		}

		partialSigs, err := e.signSession(session.Session)
		if err != nil {
			return fmt.Errorf("sign session %d for signer %x: %w",
				index, session.SignerKey[:], err)
		}

		results[index] = SignerSessionSignatures{
			SignerKey:  session.SignerKey,
			Signatures: partialSigs,
		}

		return nil
	})

	cleanupErr := e.cleanup(sessions)
	if err != nil || cleanupErr != nil {
		return nil, errors.Join(err, cleanupErr)
	}

	return results, nil
}

// run executes indexed jobs using a batch-local worker pool and an
// executor-wide concurrency limiter.
func (e *boundedSigningExecutor) run(ctx context.Context, numJobs int,
	job func(int) error) error {

	if err := ctx.Err(); err != nil {
		return err
	}

	if numJobs == 0 {
		return nil
	}

	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan int)
	jobErrors := make([]error, numJobs)
	numWorkers := min(e.maxWorkers, numJobs)

	var workers sync.WaitGroup
	workers.Add(numWorkers)
	for range numWorkers {
		go func() {
			defer workers.Done()

			for index := range jobs {
				if !e.acquire(batchCtx) {
					return
				}

				if err := batchCtx.Err(); err != nil {
					e.release()

					return
				}

				err := job(index)
				e.release()
				if err == nil {
					continue
				}

				jobErrors[index] = err
				cancel()

				return
			}
		}()
	}

sendJobs:
	for index := range numJobs {
		select {
		case jobs <- index:
		case <-batchCtx.Done():
			break sendJobs
		}
	}
	close(jobs)
	workers.Wait()

	// Prefer concrete job errors over the cancellation they caused. Join
	// them by index so concurrent completion does not make diagnostics
	// unstable.
	var errs []error
	for _, err := range jobErrors {
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return ctx.Err()
}

// acquire reserves one executor-wide worker slot.
func (e *boundedSigningExecutor) acquire(ctx context.Context) bool {
	select {
	case e.workerSlots <- struct{}{}:
		return true

	case <-ctx.Done():
		return false
	}
}

// release returns one executor-wide worker slot.
func (e *boundedSigningExecutor) release() {
	<-e.workerSlots
}

// cleanup releases each distinct session once, preserving input order for
// deterministic error reporting.
func (e *boundedSigningExecutor) cleanup(sessions []SignerSessionResult) error {
	seen := make(map[*tree.SignerSession]struct{}, len(sessions))
	var cleanupErrors []error
	for index, result := range sessions {
		if result.Session == nil {
			continue
		}

		if _, ok := seen[result.Session]; ok {
			continue
		}
		seen[result.Session] = struct{}{}

		err := e.cleanupSession(result.Session)
		if err != nil {
			cleanupErrors = append(
				cleanupErrors, fmt.Errorf("cleanup session "+
					"%d for signer %x: %w", index,
					result.SignerKey[:], err),
			)
		}
	}

	return errors.Join(cleanupErrors...)
}
