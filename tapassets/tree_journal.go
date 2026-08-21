package tapassets

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
)

const (
	customAnchorCommitStateVersion = uint16(0)
	maxCustomAnchorCommitStateSize = 128 * 1024 * 1024
	commitJournalWriteTimeout      = 5 * time.Second
	treeCommitDigestDomain         = "wavelength/asset-tree-request/v0"
)

type customAnchorCommitState struct {
	Version       uint16      `json:"version"`
	RequestDigest tapsdk.Hash `json:"request_digest"`
	Package       []byte      `json:"package,omitempty"`
}

type customAnchorCommitJournal struct {
	store        Store
	driver       assetTreeDriver
	operation    string
	digestDomain string
}

func (m *treeMaterializer) commitDurably(ctx context.Context,
	input wire.OutPoint, request *tapsdk.CustomAnchorRequest,
	verifier tapsdk.ConfirmedProofVerifier) (*commitResult, error) {

	key := fmt.Sprintf("asset-tree/%s/%s", m.cfg.Digest, input)
	journal := customAnchorCommitJournal{
		store:        m.cfg.Store,
		driver:       m.driver,
		operation:    "asset tree",
		digestDomain: treeCommitDigestDomain,
	}

	return journal.commitDurably(ctx, key, request, verifier)
}

func (j *customAnchorCommitJournal) commitDurably(ctx context.Context,
	key string, request *tapsdk.CustomAnchorRequest,
	verifier tapsdk.ConfirmedProofVerifier) (*commitResult, error) {

	if request == nil {
		return nil, fmt.Errorf("%s request is required", j.operation)
	}
	if j.store == nil {
		return nil, fmt.Errorf("%s store is required", j.operation)
	}
	if j.driver == nil {
		return nil, fmt.Errorf("%s driver is required", j.operation)
	}
	if key == "" || j.digestDomain == "" || j.operation == "" {
		return nil, fmt.Errorf("commit journal configuration is " +
			"invalid")
	}

	digest, err := customAnchorCommitRequestDigest(
		request, j.digestDomain,
	)
	if err != nil {
		return nil, err
	}

	state, err := j.load(ctx, key, digest)
	if err != nil {
		return nil, err
	}
	if len(state.Package) != 0 {
		committed, err := j.driver.DecodePackage(state.Package)
		if err != nil {
			return nil, fmt.Errorf("decode journaled %s "+
				"package: %w", j.operation, err)
		}

		return committed, nil
	}

	committed, err := j.driver.Commit(ctx, request, verifier)
	if err != nil {
		return nil, err
	}
	if committed == nil || len(committed.packageBytes) == 0 {
		return nil, fmt.Errorf("committed %s has no sealed package",
			j.operation)
	}

	state.Package = append([]byte(nil), committed.packageBytes...)
	if err := j.storeStateAfterCommit(
		ctx, key, state,
	); err != nil {
		return nil, fmt.Errorf("record committed %s package: %w",
			j.operation, err)
	}

	return committed, nil
}

func (j *customAnchorCommitJournal) storeStateAfterCommit(
	requestCtx context.Context, key string,
	state *customAnchorCommitState) error {

	journalCtx, cancel := context.WithTimeout(
		context.WithoutCancel(requestCtx), commitJournalWriteTimeout,
	)
	defer cancel()

	return j.storeState(journalCtx, key, state)
}

func (j *customAnchorCommitJournal) load(ctx context.Context, key string,
	digest tapsdk.Hash) (*customAnchorCommitState, error) {

	encoded, err := j.store.Load(ctx, key)
	if errors.Is(err, ErrStoreNotFound) {
		return &customAnchorCommitState{
			Version:       customAnchorCommitStateVersion,
			RequestDigest: digest,
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load %s commit state: %w", j.operation,
			err)
	}
	if len(encoded) == 0 || len(encoded) > maxCustomAnchorCommitStateSize {
		return nil, fmt.Errorf("%s commit state size %d is invalid",
			j.operation, len(encoded))
	}

	var state customAnchorCommitState
	if err := json.Unmarshal(encoded, &state); err != nil {
		return nil, fmt.Errorf("decode %s commit state: %w",
			j.operation, err)
	}
	if state.Version != customAnchorCommitStateVersion {
		return nil, fmt.Errorf("%s commit state version %d is "+
			"unsupported", j.operation, state.Version)
	}
	if state.RequestDigest != digest {
		return nil, fmt.Errorf("%s journal key reused with a "+
			"different request", j.operation)
	}

	return &state, nil
}

func (j *customAnchorCommitJournal) storeState(ctx context.Context, key string,
	state *customAnchorCommitState) error {

	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode %s commit state: %w", j.operation,
			err)
	}
	if len(encoded) > maxCustomAnchorCommitStateSize {
		return fmt.Errorf("%s commit state exceeds %d bytes",
			j.operation, maxCustomAnchorCommitStateSize)
	}

	return j.store.Store(ctx, key, encoded)
}

func customAnchorCommitRequestDigest(request *tapsdk.CustomAnchorRequest,
	domain string) (tapsdk.Hash, error) {

	encoded, err := json.Marshal(request)
	if err != nil {
		return tapsdk.Hash{}, fmt.Errorf("encode custom anchor "+
			"request: %w", err)
	}

	digest := sha256.New()
	_, _ = digest.Write([]byte(domain))
	_, _ = digest.Write(encoded)

	var result tapsdk.Hash
	copy(result[:], digest.Sum(nil))

	return result, nil
}
