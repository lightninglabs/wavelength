package waved

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/internal/indexerlimits"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// vtxoClaimValidityWindowBlocks mirrors the server's maximum claim
	// authorization window. The lower bound is the client's synchronized
	// tip.
	vtxoClaimValidityWindowBlocks uint32 = 144

	// redemptionReplacementPageSize bounds each indexer inventory page
	// while resolving an operator-reported replacement outpoint.
	redemptionReplacementPageSize uint32 = 128
)

// checkVTXORedeemability cross-checks only the locally supplied expired or
// redeeming descriptors. Requests are grouped by exact output script so each
// proof authorizes only the sources that script controls.
func (s *Server) checkVTXORedeemability(ctx context.Context,
	descriptors []*vtxo.Descriptor) ([]vtxo.RedemptionResult, error) {

	if s.indexer == nil {
		return nil, fmt.Errorf("indexer client is not initialized")
	}
	if s.proofKeyBackend == nil {
		return nil, fmt.Errorf("wallet key backend is not initialized")
	}

	type scriptGroup struct {
		pkScript       []byte
		participantKey keychain.KeyDescriptor
		outpoints      []wire.OutPoint
	}

	groups := make([]scriptGroup, 0, len(descriptors))
	groupIndex := make(map[string]int, len(descriptors))
	for i, descriptor := range descriptors {
		if descriptor == nil {
			return nil, fmt.Errorf("redemption descriptor "+
				"%d is nil", i)
		}
		if len(descriptor.PkScript) == 0 {
			return nil, fmt.Errorf("redemption descriptor %s has "+
				"empty pkScript", descriptor.Outpoint)
		}
		if descriptor.ClientKey.PubKey == nil {
			return nil, fmt.Errorf("redemption descriptor %s has "+
				"no participant key", descriptor.Outpoint)
		}

		// The same custom N-party script can be present under more than
		// one locally controlled participant key. Keep the proof groups
		// distinct so each scope is signed by the descriptor's actual
		// participant, never by the daemon identity fallback.
		key := string(descriptor.PkScript) + string(
			descriptor.ClientKey.PubKey.SerializeCompressed(),
		)
		idx, ok := groupIndex[key]
		if !ok {
			idx = len(groups)
			groupIndex[key] = idx
			groups = append(groups, scriptGroup{
				pkScript: bytes.Clone(
					descriptor.PkScript,
				),
				participantKey: descriptor.ClientKey,
			})
		}
		groups[idx].outpoints = append(
			groups[idx].outpoints, descriptor.Outpoint,
		)
	}

	results := make([]vtxo.RedemptionResult, 0, len(descriptors))
	for _, group := range groups {
		participantIndexer := s.indexer.WithSigner(
			s.proofKeyBackend.ProofSigner(group.participantKey),
		)
		for start := 0; start < len(group.outpoints); start +=
			indexer.MaxVTXORedeemabilityOutpoints {

			end := min(
				start+indexer.MaxVTXORedeemabilityOutpoints,
				len(group.outpoints),
			)
			resp, err := participantIndexer.
				CheckVTXORedeemabilityTaproot(
					ctx, group.outpoints[start:end],
					[]indexer.TaprootScriptScope{{
						PkScript: group.pkScript,
					}},
				)
			if err != nil {
				return nil, err
			}

			parsed, err := redemptionResultsFromRPC(resp)
			if err != nil {
				return nil, err
			}
			results = append(results, parsed...)
		}
	}

	return results, nil
}

// redemptionResultsFromRPC maps the sparse positive-only indexer response to
// the protocol-independent coordinator result.
func redemptionResultsFromRPC(resp *arkrpc.CheckVTXORedeemabilityResponse) (
	[]vtxo.RedemptionResult, error) {

	if resp == nil {
		return nil, fmt.Errorf("nil VTXO redeemability response")
	}

	claimRoundID := resp.GetClaimRoundId()
	if len(resp.GetRedeemableOutpoints()) > 0 {
		if claimRoundID == "" {
			return nil, fmt.Errorf("redeemable VTXOs have no " +
				"claim round ID")
		}
		parsedRoundID, err := round.ParseRoundID(claimRoundID)
		if err != nil {
			return nil, fmt.Errorf("invalid claim round ID %q: %w",
				claimRoundID, err)
		}
		claimRoundID = parsedRoundID.String()
	} else if claimRoundID != "" {
		return nil, fmt.Errorf("claim round ID is present without " +
			"redeemable VTXOs")
	}

	results := make(
		[]vtxo.RedemptionResult, 0,
		len(resp.GetRedeemableOutpoints())+len(resp.GetRedemptions()),
	)
	for i, protoOutpoint := range resp.GetRedeemableOutpoints() {
		outpoint, err := redemptionOutpointFromRPC(protoOutpoint)
		if err != nil {
			return nil, fmt.Errorf("redeemable outpoint %d: %w", i,
				err)
		}
		results = append(results, vtxo.RedemptionResult{
			Source:       outpoint,
			Redeemable:   true,
			ClaimRoundID: claimRoundID,
		})
	}

	for i, redemption := range resp.GetRedemptions() {
		if redemption == nil {
			return nil, fmt.Errorf("redemption %d is nil", i)
		}
		source, err := redemptionOutpointFromRPC(
			redemption.GetSourceOutpoint(),
		)
		if err != nil {
			return nil, fmt.Errorf("redemption %d source: %w", i,
				err)
		}
		replacement, err := redemptionOutpointFromRPC(
			redemption.GetReplacementOutpoint(),
		)
		if err != nil {
			return nil, fmt.Errorf("redemption %d replacement: %w",
				i, err)
		}
		results = append(results, vtxo.RedemptionResult{
			Source:      source,
			Replacement: &replacement,
		})
	}

	return results, nil
}

// redemptionOutpointFromRPC validates and converts an indexer outpoint.
func redemptionOutpointFromRPC(outpoint *arkrpc.OutPoint) (wire.OutPoint,
	error) {

	if outpoint == nil {
		return wire.OutPoint{}, fmt.Errorf("outpoint is missing")
	}
	if len(outpoint.GetTxid()) != chainhash.HashSize {
		return wire.OutPoint{}, fmt.Errorf("txid has length "+
			"%d, want %d", len(outpoint.GetTxid()),
			chainhash.HashSize)
	}

	var hash chainhash.Hash
	copy(hash[:], outpoint.GetTxid())

	return wire.OutPoint{
		Hash:  hash,
		Index: outpoint.GetVout(),
	}, nil
}

// submitVTXORedemptionClaims derives fresh tree-signing keys, independently
// authorizes each historical script claim, and hands the atomic claim bundle
// to the round actor.
func (s *Server) submitVTXORedemptionClaims(ctx context.Context,
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	], claimRoundID string, descriptors []*vtxo.Descriptor) error {

	if len(descriptors) == 0 {
		return nil
	}
	parsedRoundID, err := round.ParseRoundID(claimRoundID)
	if err != nil {
		return fmt.Errorf("invalid claim round ID %q: %w", claimRoundID,
			err)
	}
	claimRoundID = parsedRoundID.String()
	if s.actorSystem == nil {
		return fmt.Errorf("actor system is not initialized")
	}
	if s.proofKeyBackend == nil {
		return fmt.Errorf("wallet key backend is not initialized")
	}
	terms := s.operatorTerms.Load()
	if terms == nil || terms.PubKey == nil {
		return fmt.Errorf("operator claim key is not initialized")
	}

	bestHeight, err := redemptionBestHeight(ctx, chainSource)
	if err != nil {
		return err
	}
	validUntil := bestHeight + vtxoClaimValidityWindowBlocks
	if bestHeight > math.MaxUint32-vtxoClaimValidityWindowBlocks {
		validUntil = math.MaxUint32
	}

	claims := make([]round.VTXOClaimIntent, 0, len(descriptors))
	for i, descriptor := range descriptors {
		if descriptor == nil {
			return fmt.Errorf("redemption descriptor %d is nil", i)
		}
		if descriptor.ClientKey.PubKey == nil {
			return fmt.Errorf("redemption source %s has no "+
				"participant key", descriptor.Outpoint)
		}
		if descriptor.Amount <= 0 ||
			len(descriptor.PolicyTemplate) == 0 ||
			len(descriptor.PkScript) == 0 {
			return fmt.Errorf("redemption source %s is incomplete",
				descriptor.Outpoint)
		}

		replacementKey, err := s.proofKeyBackend.DeriveNextKey(
			ctx, types.VTXOSigningKeyFamily,
		)
		if err != nil {
			return fmt.Errorf("derive replacement signing key for "+
				"%s: %w", descriptor.Outpoint, err)
		}
		if replacementKey == nil || replacementKey.PubKey == nil {
			return fmt.Errorf("replacement signing key for %s "+
				"is missing", descriptor.Outpoint)
		}

		claim := types.VTXOClaimInput{
			SourceOutpoint:        descriptor.Outpoint,
			ParticipantPubKey:     descriptor.ClientKey.PubKey,
			ReplacementSigningKey: *replacementKey,
			ValidFrom:             bestHeight,
			ValidUntil:            validUntil,
		}
		if _, err := rand.Read(claim.Nonce[:]); err != nil {
			return fmt.Errorf("generate claim nonce for %s: %w",
				descriptor.Outpoint, err)
		}

		message, err := types.VTXOClaimAuthMessage(
			&claim, terms.PubKey, []byte(claimRoundID),
			descriptor.Amount, descriptor.PolicyTemplate,
			descriptor.PkScript,
		)
		if err != nil {
			return fmt.Errorf("build claim authorization for "+
				"%s: %w", descriptor.Outpoint, err)
		}
		signature, err := s.signTaggedSchnorrWithKey(
			ctx, descriptor.ClientKey, message,
			types.VTXOClaimAuthTag(),
			"VTXO redemption claim",
		)
		if err != nil {
			return fmt.Errorf("sign claim authorization for %s: %w",
				descriptor.Outpoint, err)
		}
		claim.Signature = signature.Serialize()

		claims = append(claims, round.VTXOClaimIntent{
			Input: claim,
			ExpectedOutput: types.VTXORequest{
				Amount:      descriptor.Amount,
				FixedAmount: true,
				PolicyTemplate: bytes.Clone(
					descriptor.PolicyTemplate,
				),
				PkScript:    bytes.Clone(descriptor.PkScript),
				Expiry:      descriptor.RelativeExpiry,
				ClientKey:   descriptor.ClientKey.PubKey,
				OwnerKey:    descriptor.ClientKey,
				OperatorKey: descriptor.OperatorKey,
				SigningKey:  *replacementKey,
				Origin:      types.VTXOOriginClaimReissue,
			},
		})
	}

	result := round.NewServiceKey().Ref(s.actorSystem).Ask(
		ctx, &round.RegisterVTXOClaimsRequest{
			RoundID:             claimRoundID,
			Claims:              claims,
			TriggerRegistration: true,
		},
	).Await(ctx)
	if _, err := result.Unpack(); err != nil {
		return fmt.Errorf("register VTXO redemption claims: %w", err)
	}

	return nil
}

// redemptionBestHeight returns the synchronized chain tip used to anchor the
// inclusive claim authorization window.
func redemptionBestHeight(ctx context.Context,
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg,
		chainsource.ChainSourceResp,
	]) (
	uint32, error) {

	respAny, err := chainSource.Ask(
		ctx, &chainsource.BestHeightRequest{},
	).Await(ctx).Unpack()
	if err != nil {
		return 0, fmt.Errorf("query redemption best height: %w", err)
	}
	resp, ok := respAny.(*chainsource.BestHeightResponse)
	if !ok {
		return 0, fmt.Errorf("query redemption best height: "+
			"unexpected response %T", respAny)
	}
	if resp.Height < 0 {
		return 0, fmt.Errorf("query redemption best height: negative "+
			"height %d", resp.Height)
	}

	return uint32(resp.Height), nil
}

// resolveVTXORedemptionReplacement fetches a complete replacement descriptor
// by exact historical script. A locally materialized round output wins; the
// indexer path repairs clients that crashed or whose co-participant submitted
// the claim.
func (s *Server) resolveVTXORedemptionReplacement(ctx context.Context,
	source *vtxo.Descriptor, replacement wire.OutPoint) (*vtxo.Descriptor,
	error) {

	if source == nil {
		return nil, fmt.Errorf("redemption source is required")
	}
	if s.vtxoStore != nil {
		descriptor, err := s.vtxoStore.GetVTXO(ctx, replacement)
		if err == nil {
			return descriptor, nil
		}
		if !errors.Is(err, vtxo.ErrVTXONotFound) {
			return nil, fmt.Errorf("get local replacement %s: %w",
				replacement, err)
		}
	}
	if s.indexer == nil {
		return nil, fmt.Errorf("indexer client is not initialized")
	}
	if s.proofKeyBackend == nil {
		return nil, fmt.Errorf("wallet key backend is not initialized")
	}
	if source.ClientKey.PubKey == nil {
		return nil, fmt.Errorf("redemption source has no participant " +
			"key")
	}
	participantIndexer := s.indexer.WithSigner(
		s.proofKeyBackend.ProofSigner(source.ClientKey),
	)

	var cursor []byte
	for {
		resp, err := participantIndexer.ListVTXOsByScriptsTaproot(
			ctx,
			[]indexer.TaprootScriptScope{{
				PkScript: bytes.Clone(source.PkScript),
			}},
			cursor, redemptionReplacementPageSize, nil,
		)
		if err != nil {
			return nil, fmt.Errorf("list redemption "+
				"replacements: %w", err)
		}

		page := vtxo.FlattenListVTXOsByScriptsResponse(resp)
		for i, indexed := range page {
			if indexed == nil {
				continue
			}
			outpoint, err := redemptionOutpointFromRPC(
				indexed.GetOutpoint(),
			)
			if err != nil {
				return nil, fmt.Errorf("replacement page "+
					"entry %d: %w", i, err)
			}
			if outpoint != replacement {
				continue
			}

			return redemptionDescriptorFromIndexer(source, indexed)
		}

		nextCursor := resp.GetNextCursor()
		if len(nextCursor) == 0 || len(page) == 0 {
			break
		}
		if bytes.Equal(nextCursor, cursor) {
			return nil, fmt.Errorf("redemption replacement " +
				"cursor did not advance")
		}
		if err := indexerlimits.ValidateVTXOsByScriptsCursor(
			nextCursor,
		); err != nil {
			return nil, fmt.Errorf("redemption replacement "+
				"cursor: %w", err)
		}
		cursor = bytes.Clone(nextCursor)
	}

	return nil, fmt.Errorf("replacement VTXO %s not found", replacement)
}

// redemptionDescriptorFromIndexer combines authoritative replacement lineage
// with the source's immutable historical policy and local participant key.
func redemptionDescriptorFromIndexer(source *vtxo.Descriptor,
	indexed *arkrpc.VTXO) (*vtxo.Descriptor, error) {

	if source == nil || indexed == nil {
		return nil, fmt.Errorf("source and indexed replacement are " +
			"required")
	}
	if source.OperatorKey == nil {
		return nil, fmt.Errorf("source historical operator key is " +
			"missing")
	}
	template, err := arkscript.DecodePolicyTemplate(
		source.PolicyTemplate,
	)
	if err != nil {
		return nil, fmt.Errorf("decode source policy: %w", err)
	}
	if !template.MatchesPkScript(source.PkScript) {
		return nil, fmt.Errorf("source policy does not match pkScript")
	}

	outpoint, err := redemptionOutpointFromRPC(indexed.GetOutpoint())
	if err != nil {
		return nil, err
	}
	if indexed.GetValueSat() > uint64(math.MaxInt64) ||
		indexed.GetValueSat() > uint64(btcutil.MaxSatoshi) {
		return nil, fmt.Errorf("replacement %s amount is out of range",
			outpoint)
	}
	if btcutil.Amount(indexed.GetValueSat()) != source.Amount {
		return nil, fmt.Errorf("replacement %s amount changed",
			outpoint)
	}
	if !bytes.Equal(indexed.GetPkScript(), source.PkScript) {
		return nil, fmt.Errorf("replacement %s pkScript changed",
			outpoint)
	}
	if indexed.GetRoundId() == "" {
		return nil, fmt.Errorf("replacement %s has no round id",
			outpoint)
	}
	if indexed.GetCreatedHeight() <= 0 ||
		indexed.GetBatchExpiryHeight() <= 0 {
		return nil, fmt.Errorf("replacement %s has invalid heights",
			outpoint)
	}
	if indexed.GetBatchExpiryHeight() <= source.BatchExpiry {
		return nil, fmt.Errorf("replacement %s batch expiry %d must "+
			"exceed source %d", outpoint,
			indexed.GetBatchExpiryHeight(), source.BatchExpiry)
	}

	// The indexer's relative_expiry for a round-created output is the
	// round's batch sweep delay. It is not a canonical per-policy delay for
	// custom scripts, which can contain several CSV branches. Preserve the
	// source descriptor's locally selected delay. Exact policy continuity
	// remains enforced by the source policy-to-pkScript binding above and
	// the replacement pkScript equality check.

	status, err := redemptionVTXOStatus(indexed.GetStatus())
	if err != nil {
		return nil, fmt.Errorf("replacement %s: %w", outpoint, err)
	}
	constructionVersion := arkrpc.ConstructionVersion(
		indexed.GetConstructionVersion(),
	)
	if err := arkrpc.ValidateConstructionVersion(
		constructionVersion,
	); err != nil {
		return nil, fmt.Errorf("replacement %s: %w", outpoint, err)
	}
	ancestry, err := vtxo.AncestryFromRPC(indexed.GetAncestryPaths())
	if err != nil {
		return nil, fmt.Errorf("replacement %s ancestry: %w", outpoint,
			err)
	}
	commitmentTxID, err := chainhash.NewHash(indexed.GetCommitmentTxid())
	if err != nil {
		return nil, fmt.Errorf("replacement %s commitment txid: %w",
			outpoint, err)
	}

	return &vtxo.Descriptor{
		Outpoint:            outpoint,
		Amount:              btcutil.Amount(indexed.GetValueSat()),
		PolicyTemplate:      bytes.Clone(source.PolicyTemplate),
		PkScript:            bytes.Clone(indexed.GetPkScript()),
		ClientKey:           source.ClientKey,
		OperatorKey:         source.OperatorKey,
		TapScript:           source.TapScript,
		Ancestry:            ancestry,
		RoundID:             indexed.GetRoundId(),
		CommitmentTxID:      *commitmentTxID,
		BatchExpiry:         indexed.GetBatchExpiryHeight(),
		RelativeExpiry:      source.RelativeExpiry,
		ChainDepth:          int(indexed.GetChainDepth()),
		CreatedHeight:       indexed.GetCreatedHeight(),
		Status:              status,
		ConstructionVersion: constructionVersion,
	}, nil
}

// redemptionVTXOStatus maps the authoritative indexer lifecycle onto the
// local descriptor. Replacements are normally Live when the claim journal is
// marked redeemed, but terminal mappings let a long-offline client reconcile
// without incorrectly restoring already-spent liquidity.
func redemptionVTXOStatus(status arkrpc.VTXOStatus) (vtxo.VTXOStatus, error) {
	switch status {
	case arkrpc.VTXOStatus_VTXO_STATUS_UNCONFIRMED,
		arkrpc.VTXOStatus_VTXO_STATUS_LIVE:
		return vtxo.VTXOStatusLive, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT:
		return vtxo.VTXOStatusPendingForfeit, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_FORFEITING:
		return vtxo.VTXOStatusForfeiting, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_FORFEITED:
		return vtxo.VTXOStatusForfeited, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_SPENT:
		return vtxo.VTXOStatusSpent, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT:
		return vtxo.VTXOStatusUnilateralExit, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_FAILED:
		return vtxo.VTXOStatusFailed, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_EXPIRED:
		return vtxo.VTXOStatusExpired, nil

	default:
		return 0, fmt.Errorf("unknown indexer VTXO status %v", status)
	}
}

// finalizeVTXORedemptionClaim loads the replacement that the round actor
// already persisted, then applies the coordinator's exact-policy validation
// and source-to-replacement compare-and-set.
func (s *Server) finalizeVTXORedemptionClaim(ctx context.Context,
	source, replacement wire.OutPoint) error {

	if s.redemptionCoordinator == nil {
		return fmt.Errorf("redemption coordinator is not initialized")
	}
	if s.vtxoStore == nil {
		return fmt.Errorf("VTXO store is not initialized")
	}

	descriptor, err := s.vtxoStore.GetVTXO(ctx, replacement)
	if err != nil {
		return fmt.Errorf("load redemption replacement %s: %w",
			replacement, err)
	}

	return s.redemptionCoordinator.Finalize(ctx, source, descriptor)
}

// observeVTXORedemption gives a newly recovered live replacement an actor.
// Terminal replacements only need the durable source link and must not be
// resurrected into spendable inventory.
func (s *Server) observeVTXORedemption(ctx context.Context,
	source wire.OutPoint, replacement *vtxo.Descriptor) error {

	if replacement == nil {
		return fmt.Errorf("redemption replacement is required")
	}
	roundID, err := round.ParseRoundID(replacement.RoundID)
	if err != nil {
		return fmt.Errorf("parse redemption round ID: %w", err)
	}
	var roundIDBytes [16]byte
	copy(roundIDBytes[:], roundID[:])

	if s.actorSystem == nil {
		return fmt.Errorf("actor system is not initialized")
	}
	if err := ledger.NewSink(s.actorSystem).Tell(
		context.WithoutCancel(ctx), &ledger.VTXOClaimReissuedMsg{
			Source:      source,
			Replacement: replacement.Outpoint,
			AmountSat:   int64(replacement.Amount),
			RoundID:     roundIDBytes,
		},
	); err != nil {
		return fmt.Errorf("record VTXO claim reissue: %w", err)
	}

	if replacement.Status != vtxo.VTXOStatusLive {
		return nil
	}
	if s.vtxoMgrRef.IsNone() {
		return fmt.Errorf("VTXO manager is not initialized")
	}

	result := s.vtxoMgrRef.UnsafeFromSome().Ask(
		ctx, &vtxo.VTXOsMaterializedNotification{
			VTXOs: []*vtxo.Descriptor{replacement},
		},
	).Await(ctx)
	_, err = result.Unpack()
	if err != nil {
		return fmt.Errorf("materialize redemption replacement: %w", err)
	}

	return nil
}
