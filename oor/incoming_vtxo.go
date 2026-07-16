package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
)

// IncomingVTXOMetadata carries authoritative lineage/expiry metadata for an
// incoming OOR VTXO. The receive path must not invent synthetic placeholders
// for these fields because they drive expiry logic and unilateral-exit lineage.
type IncomingVTXOMetadata struct {
	// RoundID identifies the round lineage this VTXO belongs to. For
	// cross-commitment multi-input OOR VTXOs this is the round of the
	// first ancestry path; richer per-path round metadata travels in
	// Ancestry.
	RoundID string

	// CommitmentTxID is the commitment transaction anchoring the
	// primary VTXO lineage. For cross-commitment multi-input OOR VTXOs
	// this must match one entry in Ancestry; descriptor construction
	// normalizes that entry to Ancestry[0] for legacy consumers.
	CommitmentTxID chainhash.Hash

	// BatchExpiry is the absolute batch expiry height (most-restrictive
	// across all contributing rounds).
	BatchExpiry int32

	// ChainDepth is the number of OOR checkpoint hops between this
	// VTXO and the last on-chain commitment. This is distinct from
	// per-Ancestry TreeDepth, which tracks position within a single
	// commitment tree.
	ChainDepth int

	// CreatedHeight is the block height at which the VTXO was created.
	CreatedHeight int32

	// OperatorKey is the operator public key committed to the VTXO's
	// collaborative spend path. Older indexers may omit this, in which case
	// materialization falls back to the handler's configured operator key.
	OperatorKey *btcec.PublicKey

	// Ancestry is the set of rooted commitment-tree fragments required
	// to claim this VTXO unilaterally on-chain. Round-direct and
	// single-commitment OOR VTXOs have len(Ancestry) == 1; cross-round
	// multi-input OOR VTXOs have one entry per distinct contributing
	// commitment tx. Each entry carries its own TreePath, CommitmentTxID,
	// InputIndices, and TreeDepth.
	Ancestry []vtxo.Ancestry
}

// IncomingVTXOConfig describes how to materialize an Ark tx output into a
// spendable local VTXO descriptor.
//
// This is intentionally explicit: mapping Ark outputs to local keys requires
// wallet/application context that the FSM does not have.
type IncomingVTXOConfig struct {
	// OutputIndex is the Ark tx output index being materialized.
	OutputIndex uint32

	// ClientKey is the recipient key descriptor that controls this VTXO.
	ClientKey keychain.KeyDescriptor

	// OperatorKey is the operator public key used by the collaborative
	// spend path.
	OperatorKey *btcec.PublicKey

	// ExitDelay is the unilateral CSV delay used by the timeout spend
	// path.
	ExitDelay uint32

	// Metadata carries authoritative lineage and expiry attributes.
	Metadata IncomingVTXOMetadata

	// PolicyTemplate is the optional semantic policy template supplied with
	// the incoming recipient output. When omitted, descriptor construction
	// derives the standard VTXO policy from ClientKey, OperatorKey, and
	// ExitDelay for compatibility with older servers.
	PolicyTemplate []byte
}

// BuildIncomingVTXODescriptor constructs a VTXO descriptor for a recipient
// output of a canonical Ark PSBT.
//
// The returned VTXO descriptor uses an outpoint of (arkTxid, outputIndex) and
// derives the tapscript from (client key, operator key, exit delay). This
// allows later signing of collaborative and timeout spends.
func BuildIncomingVTXODescriptor(ark *psbt.Packet,
	cfg IncomingVTXOConfig) (*vtxo.Descriptor, error) {

	switch {
	case ark == nil || ark.UnsignedTx == nil:
		return nil, fmt.Errorf("ark psbt must be provided")

	case cfg.ClientKey.PubKey == nil:
		return nil, fmt.Errorf("client key must be provided")

	case cfg.OperatorKey == nil:
		return nil, fmt.Errorf("operator key must be provided")

	case cfg.Metadata.RoundID == "":
		return nil, fmt.Errorf("round id must be provided")

	case cfg.Metadata.ChainDepth < 0:
		return nil, fmt.Errorf("chain depth must be "+
			"non-negative, got %d", cfg.Metadata.ChainDepth)
	}

	if cfg.Metadata.CommitmentTxID == (chainhash.Hash{}) {
		return nil, fmt.Errorf("commitment tx id must be provided")
	}

	err := arktx.ValidateCanonicalPSBT(ark)
	if err != nil {
		return nil, err
	}

	tx := ark.UnsignedTx
	if int(cfg.OutputIndex) >= len(tx.TxOut) {
		return nil, fmt.Errorf("output index %d out of bounds",
			cfg.OutputIndex)
	}

	out := tx.TxOut[cfg.OutputIndex]
	if arktx.IsAnchorOutput(out) {
		return nil, fmt.Errorf("output index %d is anchor output",
			cfg.OutputIndex)
	}

	// Cross-check the operator-supplied ancestry against the metadata
	// and the Ark tx shape before we commit to a descriptor. Without
	// this gate, a malicious operator can return ancestry that decodes
	// cleanly, persists cleanly, and only fails at unroll time when
	// the user is racing a CSV — a fund-redirect surface, not just a
	// DoS. Failures here surface as *ErrInvalidAncestry so the receive
	// FSM can route them to a session-failure ack.
	err = validateIncomingAncestry(cfg.Metadata, uint32(len(tx.TxIn)))
	if err != nil {
		return nil, err
	}

	arkTxid := tx.TxHash()

	policyTemplate, tapscript, err := incomingOutputPolicy(
		out.PkScript, cfg,
	)
	if err != nil {
		return nil, err
	}

	ancestry := normalizeIncomingAncestry(cfg.Metadata)

	return &vtxo.Descriptor{
		Outpoint: wire.OutPoint{
			Hash:  arkTxid,
			Index: cfg.OutputIndex,
		},
		Amount:         btcutil.Amount(out.Value),
		PolicyTemplate: policyTemplate,
		PkScript:       out.PkScript,
		ClientKey:      cfg.ClientKey,
		OperatorKey:    cfg.OperatorKey,
		TapScript:      tapscript,
		Ancestry:       ancestry,
		RoundID:        cfg.Metadata.RoundID,
		CommitmentTxID: cfg.Metadata.CommitmentTxID,
		BatchExpiry:    cfg.Metadata.BatchExpiry,
		RelativeExpiry: cfg.ExitDelay,
		ChainDepth:     cfg.Metadata.ChainDepth,
		CreatedHeight:  cfg.Metadata.CreatedHeight,
		Status:         vtxo.VTXOStatusLive,
	}, nil
}

func incomingOutputPolicy(pkScript []byte, cfg IncomingVTXOConfig) ([]byte,
	*waddrmgr.Tapscript, error) {

	if len(cfg.PolicyTemplate) > 0 {
		template, err := arkscript.DecodePolicyTemplate(
			cfg.PolicyTemplate,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("decode incoming VTXO "+
				"policy: %w", err)
		}

		if !template.MatchesPkScript(pkScript) {
			return nil, nil, fmt.Errorf("incoming VTXO policy " +
				"does not match ark output pkscript")
		}

		tapscript, err := standardTapscriptIfApplicable(template)
		if err != nil {
			return nil, nil, err
		}

		return append(
			[]byte(nil), cfg.PolicyTemplate...,
		), tapscript, nil
	}

	tapscript, err := arkscript.VTXOTapScript(
		cfg.ClientKey.PubKey, cfg.OperatorKey, cfg.ExitDelay,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("derive vtxo tapscript: %w", err)
	}

	tapKey, err := arkscript.VTXOTapKey(
		cfg.ClientKey.PubKey, cfg.OperatorKey, cfg.ExitDelay,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("derive vtxo tapkey: %w", err)
	}

	expectedPkScript, err := txscript.PayToTaprootScript(tapKey)
	if err != nil {
		return nil, nil, fmt.Errorf("derive vtxo pkscript: %w", err)
	}

	if !bytes.Equal(expectedPkScript, pkScript) {
		return nil, nil, fmt.Errorf("ark output pkscript does not " +
			"match derived vtxo pkscript")
	}

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		cfg.ClientKey.PubKey, cfg.OperatorKey, cfg.ExitDelay,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("encode incoming VTXO policy: %w",
			err)
	}

	return policyTemplate, tapscript, nil
}

func standardTapscriptIfApplicable(template *arkscript.PolicyTemplate) (
	*waddrmgr.Tapscript, error) {

	params, decodeErr := arkscript.DecodeStandardVTXOParams(template)
	if decodeErr == nil {
		tapscript, err := arkscript.VTXOTapScript(
			params.OwnerKey, params.OperatorKey, params.ExitDelay,
		)
		if err != nil {
			return nil, fmt.Errorf("derive standard vtxo "+
				"tapscript: %w", err)
		}

		return tapscript, nil
	}

	return nil, nil
}

// validateIncomingAncestry runs the structural cross-checks that bind
// an operator-supplied IncomingVTXOMetadata.Ancestry to the metadata's
// claimed commitment and to the Ark tx's input shape. Failures return
// *ErrInvalidAncestry so the receive FSM can fail-fast with a typed
// reason rather than waiting for the bug to surface deep inside proof
// assembly.
//
// The checks are:
//
//   - At least one ancestry fragment is present. An empty Ancestry has
//     no unilateral-exit material; accepting it would leave the user's
//     funds dependent on the operator's continued cooperation.
//
//   - Some fragment's CommitmentTxID matches the metadata's claimed
//     CommitmentTxID. The metadata's CommitmentTxID is the anchor of
//     record for the produced VTXO; if no fragment anchors there, the
//     descriptor and its lineage disagree about which round funded this
//     VTXO. BuildIncomingVTXODescriptor normalizes that matching fragment
//     to Ancestry[0] after validation.
//
//   - No two fragments re-cover the same input under the same commitment
//     txid. A given input's lineage through a given commitment resolves
//     to exactly one leaf, so a repeated (commitment, input) pair is a
//     redundant or malformed fragment. Two fragments may still share a
//     commitment txid when they serve different inputs (an OOR VTXO whose
//     Ark tx spent two coins from the same commitment tree at different
//     leaves -- wavelength#969), and one input may appear under several
//     commitments (a chained receive), so uniqueness is enforced on the
//     (commitment, input) pair, not the commitment txid alone.
//
//   - Each fragment carries a non-nil TreePath. A nil path is unusable
//     for proof assembly and would be caught by the unroller; we surface
//     it here so the failure is bound to the receive boundary.
//
//   - Each fragment's TreePath.BatchOutpoint.Hash matches its claimed
//     CommitmentTxID. The commitment tx's batch output is the root the
//     fragment's path is supposed to extract; without this binding an
//     adversarial operator could supply a path that decodes cleanly,
//     persists cleanly, and only fails at unilateral-exit time when
//     the user is racing a CSV. Failure surfaces a fund-redirect
//     surface, not just a DoS.
//
//   - Each fragment's InputIndices is non-empty and every index is
//     within the Ark tx's input count. An empty slice violates the
//     Ancestry contract for incoming OOR VTXOs (those are always
//     produced by an OOR Ark tx); an out-of-range index points at a
//     non-existent input so the unroll proof would never resolve.
//
//   - No input index repeats within a fragment, and the union of all
//     fragments' InputIndices covers every Ark tx input. Distinct
//     fragments may name the same input when that OOR Ark input itself
//     carries multi-fragment ancestry from an earlier chained receive.
//     A missing index means at least one Ark tx input has no rooted-path
//     material attached: fraud-watch plans (BuildWatchPlan) would lack
//     a watch for the uncovered input's lineage and unilateral-exit
//     proof assembly would have no fragment to broadcast for that
//     input. The descriptor would persist cleanly and the gap would
//     only surface if the operator later refuses cooperation, at which
//     point the user is racing a CSV with no way to recover. We reject
//     at the receive boundary so the bad indexer response fails before
//     any funds are credited.
//
// commitInputKey identifies one (commitment tx, ark input index) pair. It is
// the uniqueness key for ancestry fragments: a given input descends from a
// given commitment through exactly one leaf, so the same pair may not be
// covered by more than one fragment. See validateIncomingAncestry.
type commitInputKey struct {
	commitment chainhash.Hash
	input      uint32
}

func validateIncomingAncestry(meta IncomingVTXOMetadata,
	arkTxInputCount uint32) error {

	if len(meta.Ancestry) == 0 {
		return &ErrInvalidAncestry{
			Reason: "empty ancestry",
		}
	}

	// Track whether at least one ancestry fragment serves each Ark tx
	// input so we can verify coverage at the end. Sized to the Ark tx's
	// declared input count; we only set entries after the per-fragment
	// range check has passed, so out-of-range writes are impossible.
	covered := make([]bool, arkTxInputCount)

	// seenCommitInputs tracks the (commitment txid, ark input index) pairs
	// already claimed by a fragment. A given Ark tx input descends from a
	// given commitment tx through exactly one leaf, so the same pair
	// appearing under two fragments is a redundant or malformed fragment.
	// We deliberately key on the pair, not the commitment txid alone: two
	// fragments legitimately share a commitment txid when they serve
	// different inputs -- e.g. an OOR change VTXO whose Ark tx spent two
	// input VTXOs that both descend from the same commitment tx but sit at
	// different leaves, so each needs its own root->leaf path
	// (wavelength#969) -- and a single input may likewise appear under
	// several commitments for a chained receive.
	seenCommitInputs := make(map[commitInputKey]struct{})
	hasPrimary := false
	for i, frag := range meta.Ancestry {
		if frag.CommitmentTxID == meta.CommitmentTxID {
			hasPrimary = true
		}

		if frag.TreePath == nil {
			return &ErrInvalidAncestry{
				Reason: fmt.Sprintf(
					"fragment %d has nil tree path", i,
				),
			}
		}

		// Validate the per-fragment tree depth here as a
		// defense-in-depth check. The RPC ingress
		// (arkrpc.ValidateAncestryPathDepth) is the primary trust
		// boundary, but any code path that constructs Ancestry
		// in-process (tests, future internal materializers) must
		// also enforce that the stored TreeDepth matches the actual
		// path. Without this, an under-reported depth could survive
		// to drive expiry-monitoring decisions and silently delay a
		// unilateral exit past its safe deadline.
		if err := arkrpc.ValidateAncestryPathDepth(
			frag.TreeDepth, frag.TreePath,
		); err != nil {
			return &ErrInvalidAncestry{
				Reason: fmt.Sprintf(
					"fragment %d depth: %v", i, err,
				),
			}
		}

		// Bind the supplied tree path to its claimed commitment.
		// The TreePath.BatchOutpoint is the batch output of the
		// commitment tx the path extracts from; if the operator
		// substitutes a path rooted in some other commitment, the
		// fragment cannot drive a valid unilateral exit and we
		// have no way to detect the mismatch later when the user
		// is already racing the exit CSV.
		if frag.TreePath.BatchOutpoint.Hash != frag.CommitmentTxID {
			return &ErrInvalidAncestry{
				Reason: fmt.Sprintf(
					"fragment %d tree path batch "+
						"outpoint hash %s does not "+
						"match claimed commitment "+
						"txid %s", i,
					frag.TreePath.BatchOutpoint.Hash,
					frag.CommitmentTxID,
				),
			}
		}

		if len(frag.InputIndices) == 0 {
			return &ErrInvalidAncestry{
				Reason: fmt.Sprintf(
					"fragment %d has empty input indices "+
						"(incoming OOR fragments must "+
						"name at least one input)", i,
				),
			}
		}

		seenInFragment := make(
			map[uint32]struct{}, len(frag.InputIndices),
		)
		for j, idx := range frag.InputIndices {
			if idx >= arkTxInputCount {
				return &ErrInvalidAncestry{
					Reason: fmt.Sprintf(
						"fragment %d input index "+
							"[%d]=%d out of range "+
							"(ark tx has %d "+
							"inputs)", i, j, idx,
						arkTxInputCount,
					),
				}
			}

			// Reject only duplicate indices within this
			// fragment. Cross-fragment reuse is valid for a
			// chained OOR receive where one Ark input is backed
			// by multiple earlier commitment roots.
			if _, ok := seenInFragment[idx]; ok {
				return &ErrInvalidAncestry{
					Reason: fmt.Sprintf(
						"fragment %d input index "+
							"[%d]=%d duplicates "+
							"an earlier index in "+
							"this fragment",
						i, j, idx,
					),
				}
			}
			seenInFragment[idx] = struct{}{}
			covered[idx] = true

			// A given input's lineage through a given commitment
			// resolves to exactly one leaf, so the same
			// (commitment, input) pair under two fragments is a
			// redundant or malformed fragment. Fragments sharing
			// only the commitment (different inputs) or only the
			// input (different commitments) are both legitimate;
			// see seenCommitInputs above.
			pair := commitInputKey{
				commitment: frag.CommitmentTxID,
				input:      idx,
			}
			if _, ok := seenCommitInputs[pair]; ok {
				return &ErrInvalidAncestry{
					Reason: fmt.Sprintf(
						"fragment %d re-covers "+
							"input %d already "+
							"served under "+
							"commitment %s", i,
						idx, frag.CommitmentTxID,
					),
				}
			}
			seenCommitInputs[pair] = struct{}{}
		}
	}

	if !hasPrimary {
		return &ErrInvalidAncestry{
			Reason: fmt.Sprintf(
				"no ancestry fragment matches metadata "+
					"commitment txid %s",
				meta.CommitmentTxID,
			),
		}
	}

	// Every Ark tx input must be covered by at least one fragment.
	// Accepting a missing input would leave the uncovered input with no
	// rooted-path material for unilateral exit, stranding the received
	// VTXO if the operator later refuses cooperation.
	for idx, ok := range covered {
		if !ok {
			return &ErrInvalidAncestry{
				Reason: fmt.Sprintf(
					"ark tx input %d is not covered by "+
						"any ancestry fragment "+
						"(incoming ancestry must "+
						"cover every input)", idx,
				),
			}
		}
	}

	return nil
}

// normalizeIncomingAncestry copies the incoming ancestry slice and moves the
// metadata commitment fragment to the front. Indexer responses may order
// cross-round multi-input fragments by input path, but older descriptor
// consumers treat Ancestry[0] as the primary lineage.
func normalizeIncomingAncestry(meta IncomingVTXOMetadata) []vtxo.Ancestry {
	ancestry := make([]vtxo.Ancestry, len(meta.Ancestry))
	copy(ancestry, meta.Ancestry)

	for i := range ancestry {
		if ancestry[i].CommitmentTxID != meta.CommitmentTxID {
			continue
		}

		if i == 0 {
			return ancestry
		}

		primary := ancestry[i]
		copy(ancestry[1:i+1], ancestry[0:i])
		ancestry[0] = primary

		return ancestry
	}

	return ancestry
}
