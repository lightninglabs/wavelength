package unroll

import (
	"context"
	"fmt"
)

// PolicyResolvers dispatches durable non-standard exit policies by kind.
// Each child resolver remains responsible for validating its own reference.
type PolicyResolvers []ExitSpendPolicyResolver

// SupportsKind reports whether one child resolver advertises the policy kind.
func (r PolicyResolvers) SupportsKind(kind ExitPolicyKind) bool {
	hasLegacyResolver := false
	for _, resolver := range r {
		if resolver == nil {
			continue
		}
		support, ok := resolver.(ResolverKindSupport)
		if !ok {
			hasLegacyResolver = true

			continue
		}
		if support.SupportsKind(kind) {
			return true
		}
	}

	// A legacy resolver cannot advertise its coverage. Preserve its former
	// behavior by admitting the kind and consulting it at resolution time.
	return hasLegacyResolver
}

// ResolveExitSpendPolicy delegates to the one resolver advertising the kind.
func (r PolicyResolvers) ResolveExitSpendPolicy(ctx context.Context,
	req ExitSpendPolicyRequest) (ExitSpendPolicy, error) {

	for _, resolver := range r {
		if resolver == nil {
			continue
		}
		support, ok := resolver.(ResolverKindSupport)
		if !ok || !support.SupportsKind(req.Kind) {
			continue
		}

		return resolver.ResolveExitSpendPolicy(ctx, req)
	}

	// Prefer explicit kind routing above, then fall back to the first
	// legacy resolver exactly as a standalone pre-advertisement resolver
	// behaved.
	for _, resolver := range r {
		if resolver == nil {
			continue
		}
		if _, ok := resolver.(ResolverKindSupport); ok {
			continue
		}

		return resolver.ResolveExitSpendPolicy(ctx, req)
	}

	return nil, fmt.Errorf("no exit spend policy resolver for kind %q",
		req.Kind)
}

var _ ExitSpendPolicyResolver = PolicyResolvers{}
var _ ResolverKindSupport = PolicyResolvers{}
