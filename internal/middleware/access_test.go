package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
)

func TestErrorTypeLooksThroughWrappersButNotIntoTheError(t *testing.T) {
	t.Parallel()
	_, cause := strconv.Atoi("forty-two")
	wrapped := fmt.Errorf("read post: %w", fmt.Errorf("parse id: %w", cause))
	for name, err := range map[string]error{
		"the cause":     cause,
		"wrapped twice": wrapped,
		"a panic":       &PanicError{Value: cause},
	} {
		if got := errorType(err); got != "*strconv.NumError" {
			t.Errorf("errorType(%s) = %s, want *strconv.NumError", name, got)
		}
	}
}

func TestPanicErrorIsNamedByWhatPanicked(t *testing.T) {
	t.Parallel()
	if got := errorType(&PanicError{Value: "boom"}); got != "string" {
		t.Errorf("errorType(panic(\"boom\")) = %s, want string", got)
	}
}

func TestStatusOf(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]int{
		fmt.Errorf("wait: %w", context.DeadlineExceeded): http.StatusGatewayTimeout,
		errors.New("anything else"):                      http.StatusInternalServerError,
	} {
		if got := statusOf(err); got != want {
			t.Errorf("statusOf(%v) = %d, want %d", err, got, want)
		}
	}
}
