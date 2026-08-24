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
	for _, resolver := range r {
		if resolver == nil {
			continue
		}
		support, ok := resolver.(ResolverKindSupport)
		if ok && support.SupportsKind(kind) {
			return true
		}
	}

	return false
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

	return nil, fmt.Errorf("no exit spend policy resolver for kind %q",
		req.Kind)
}

var _ ExitSpendPolicyResolver = PolicyResolvers{}
var _ ResolverKindSupport = PolicyResolvers{}
