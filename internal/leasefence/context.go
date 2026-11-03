// Package leasefence carries mailbox lease ownership through an existing
// operation without widening the durable store interface.
package leasefence

import "context"

// tokenKey scopes a lease token to the current store operation.
type tokenKey struct{}

// WithToken returns a context that requires a store mutation to validate the
// supplied mailbox lease token.
func WithToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, tokenKey{}, token)
}

// Token returns the mailbox lease token attached to the current store
// operation.
func Token(ctx context.Context) (string, bool) {
	token, ok := ctx.Value(tokenKey{}).(string)

	return token, ok && token != ""
}
