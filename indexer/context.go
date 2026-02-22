package indexer

import "context"

type principalKey struct{}

// Principal identifies the mailbox principal associated with an inbound
// mailbox request.
type Principal struct {
	// MailboxID is the mailbox identifier the operator should treat as the
	// request principal.
	MailboxID string
}

// ContextWithPrincipal attaches principal to ctx.
func ContextWithPrincipal(ctx context.Context,
	principal Principal) context.Context {

	return context.WithValue(ctx, principalKey{}, principal)
}

// PrincipalFromContext extracts a Principal from ctx.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	v := ctx.Value(principalKey{})
	if v == nil {
		return Principal{}, false
	}

	principal, ok := v.(Principal)

	return principal, ok
}
