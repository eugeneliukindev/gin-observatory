package middleware

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Errors answers a request a handler gave up on with c.Error, with the status the error calls for.
// A handler answers what it expects itself — a 400, a 404 — and hands over only what it cannot.
func Errors() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if failure := c.Errors.Last(); failure != nil && !c.Writer.Written() {
			answer(c, statusOf(failure.Err))
		}
	}
}

// statusOf is the status for an error a handler could not handle: a deadline passed waiting on
// someone else is theirs, anything else is ours.
func statusOf(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}
