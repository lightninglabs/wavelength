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
	treeCommitStateVersion  = uint16(0)
	maxTreeCommitStateSize  = 128 * 1024 * 1024
	treeJournalWriteTimeout = 5 * time.Second
)

type treeCommitState struct {
	Version       uint16      `json:"version"`
	RequestDigest tapsdk.Hash `json:"request_digest"`
	Package       []byte      `json:"package,omitempty"`
}

func (m *treeMaterializer) commitDurably(ctx context.Context,
	input wire.OutPoint, request *tapsdk.CustomAnchorRequest,
	verifier tapsdk.ConfirmedProofVerifier) (*commitResult, error) {

	if request == nil {
		return nil, fmt.Errorf("asset tree request is required")
	}

	digest, err := treeCommitRequestDigest(request)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("asset-tree/%s/%s", m.cfg.Digest, input)

	state, err := m.loadTreeCommitState(ctx, key, digest)
	if err != nil {
		return nil, err
	}
	if len(state.Package) != 0 {
		committed, err := m.driver.DecodePackage(state.Package)
		if err != nil {
			return nil, fmt.Errorf("decode journaled asset tree "+
				"package: %w", err)
		}

		return committed, nil
	}
	committed, err := m.driver.Commit(ctx, request, verifier)
	if err != nil {
		return nil, err
	}
	if committed == nil || len(committed.packageBytes) == 0 {
		return nil, fmt.Errorf("committed asset tree has no sealed " +
			"package")
	}

	state.Package = append([]byte(nil), committed.packageBytes...)
	if err := m.storeTreeCommitStateAfterCommit(
		ctx, key, state,
	); err != nil {
		return nil, fmt.Errorf("record committed asset tree "+
			"package: %w", err)
	}

	return committed, nil
}

func (m *treeMaterializer) storeTreeCommitStateAfterCommit(
	requestCtx context.Context, key string, state *treeCommitState) error {

	journalCtx, cancel := context.WithTimeout(
		context.WithoutCancel(requestCtx), treeJournalWriteTimeout,
	)
	defer cancel()

	return m.storeTreeCommitState(journalCtx, key, state)
}

func (m *treeMaterializer) loadTreeCommitState(ctx context.Context, key string,
	digest tapsdk.Hash) (*treeCommitState, error) {

	encoded, err := m.cfg.Store.Load(ctx, key)
	if errors.Is(err, ErrStoreNotFound) {
		return &treeCommitState{
			Version:       treeCommitStateVersion,
			RequestDigest: digest,
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load asset tree commit state: %w", err)
	}
	if len(encoded) == 0 || len(encoded) > maxTreeCommitStateSize {
		return nil, fmt.Errorf("asset tree commit state size %d "+
			"is invalid", len(encoded))
	}

	var state treeCommitState
	if err := json.Unmarshal(encoded, &state); err != nil {
		return nil, fmt.Errorf("decode asset tree commit state: %w",
			err)
	}
	if state.Version != treeCommitStateVersion {
		return nil, fmt.Errorf("asset tree commit state version %d is "+
			"unsupported", state.Version)
	}
	if state.RequestDigest != digest {
		return nil, fmt.Errorf("asset tree journal key reused with a " +
			"different request")
	}

	return &state, nil
}

func (m *treeMaterializer) storeTreeCommitState(ctx context.Context, key string,
	state *treeCommitState) error {

	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode asset tree commit state: %w", err)
	}
	if len(encoded) > maxTreeCommitStateSize {
		return fmt.Errorf("asset tree commit state exceeds %d bytes",
			maxTreeCommitStateSize)
	}

	return m.cfg.Store.Store(ctx, key, encoded)
}

func treeCommitRequestDigest(request *tapsdk.CustomAnchorRequest) (tapsdk.Hash,
	error) {

	encoded, err := json.Marshal(request)
	if err != nil {
		return tapsdk.Hash{}, fmt.Errorf("encode asset tree "+
			"request: %w", err)
	}

	digest := sha256.New()
	_, _ = digest.Write([]byte("wavelength/asset-tree-request/v0"))
	_, _ = digest.Write(encoded)

	var result tapsdk.Hash
	copy(result[:], digest.Sum(nil))

	return result, nil
}
