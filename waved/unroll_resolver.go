package waved

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/unroll"
)

// compositeExitSpendPolicyResolver dispatches non-standard unroll exit
// policies to the resolver that advertises the requested kind.
type compositeExitSpendPolicyResolver struct {
	resolvers []unroll.ExitSpendPolicyResolver
}

// SupportsKind reports whether any child resolver can reconstruct kind.
func (r compositeExitSpendPolicyResolver) SupportsKind(
	kind unroll.ExitPolicyKind) bool {

	for _, resolver := range r.resolvers {
		support, ok := resolver.(unroll.ResolverKindSupport)
		if ok && support.SupportsKind(kind) {
			return true
		}
	}

	return false
}

// ResolveExitSpendPolicy resolves req with the matching child resolver.
func (r compositeExitSpendPolicyResolver) ResolveExitSpendPolicy(
	ctx context.Context, req unroll.ExitSpendPolicyRequest) (
	unroll.ExitSpendPolicy, error) {

	for _, resolver := range r.resolvers {
		support, ok := resolver.(unroll.ResolverKindSupport)
		if !ok || !support.SupportsKind(req.Kind) {
			continue
		}

		return resolver.ResolveExitSpendPolicy(ctx, req)
	}

	return nil, fmt.Errorf("no exit spend policy resolver for kind %s",
		req.Kind)
}

// Compile-time checks.
var _ unroll.ExitSpendPolicyResolver = compositeExitSpendPolicyResolver{}
var _ unroll.ResolverKindSupport = compositeExitSpendPolicyResolver{}
