package router

import (
	"context"
	"testing"

	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"go-observatory/internal/middleware"
)

func TestGiveUpPanicsWithItsOwnType(t *testing.T) {
	t.Parallel()
	for kind, want := range map[failure]string{
		failureIndex:     "runtime.boundsError",
		failureNil:       "runtime.errorString",
		failureAssertion: "*runtime.TypeAssertionError",
		failureMap:       "runtime.plainError",
	} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			defer func() {
				panicked := &middleware.PanicError{Value: recover()}
				if got := panicked.ErrorType(); got != want {
					t.Errorf("giveUp(%q) panicked with %s, want %s", kind, got, want)
				}
			}()
			_ = giveUp(context.Background(), kind)
		})
	}
}

func TestGiveUpReturnsItsOwnType(t *testing.T) {
	t.Parallel()
	for kind, want := range map[failure]string{
		failureTimeout:    "context.deadlineExceededError",
		failurePermission: "*fs.PathError",
	} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			err := giveUp(context.Background(), kind)
			if got := semconv.ErrorType(err).Value.AsString(); got != want {
				t.Errorf("giveUp(%q) returned %s, want %s", kind, got, want)
			}
		})
	}
}

func TestCountPrimes(t *testing.T) {
	t.Parallel()
	if got := countPrimes(context.Background(), 30); got != 10 {
		t.Errorf("countPrimes(30) = %d, want 10", got)
	}
}
