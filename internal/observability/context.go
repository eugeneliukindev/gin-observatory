package observability

import "context"

type requestIDKey struct{}

// WithRequestID returns a context whose every record carries the request key.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID is the request key every record of the request carries; empty outside a request.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
