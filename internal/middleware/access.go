// Package middleware gives every request a key and writes a line for every response.
package middleware

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"go-observatory/internal/observability"
)

// RequestIDHeader is the header a request's key arrives in, if the caller has one, and is
// returned in.
const RequestIDHeader = "X-Request-ID"

// Unhandled gives up on the request: the access line reports err with the stack of the caller,
// and the response is a 500 — the error a handler did not expect.
func Unhandled(c *gin.Context, err error) {
	_ = c.Error(unhandled{err: err, stack: string(debug.Stack())})
}

// unhandled is an error with the stack of the place that gave up on it: Go errors carry none.
type unhandled struct {
	err   error
	stack string
}

func (u unhandled) Error() string { return u.err.Error() }
func (u unhandled) Unwrap() error { return u.err }

// ErrorType names the error for error.type on the metrics and the span, as the line names it.
func (u unhandled) ErrorType() string { return errorType(u.err) }

// panicked is what a handler panicked with.
type panicked struct{ value any }

func (p panicked) Error() string     { return fmt.Sprint(p.value) }
func (p panicked) ErrorType() string { return fmt.Sprintf("%T", p.value) }

// errorType is the Go type of the error, looking through the wrappers that only add context —
// fmt.Errorf("…: %w") — but not into the error itself: *strconv.NumError stays, though it wraps
// strconv.ErrSyntax.
func errorType(err error) string {
	for strings.HasPrefix(fmt.Sprintf("%T", err), "*fmt.wrapError") {
		err = errors.Unwrap(err)
	}
	if named, ok := err.(interface{ ErrorType() string }); ok {
		return named.ErrorType()
	}
	return fmt.Sprintf("%T", err)
}

// Access gives the request a key, returns it in the response and writes a line for every
// response. A panic or an unhandled error becomes a 500 and a line with its stack. Paths in
// excludedPaths — liveness probes — get neither a key nor a line.
func Access(excludedPaths ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if slices.Contains(excludedPaths, c.Request.URL.Path) {
			c.Next()
			return
		}

		id := c.GetHeader(RequestIDHeader)
		if id == "" {
			id = uuid.NewString()
		}
		started := time.Now()
		c.Request = c.Request.WithContext(observability.WithRequestID(c.Request.Context(), id))
		// The request's key on its span, so a trace is found by the key a log line or a caller has.
		trace.SpanFromContext(c.Request.Context()).SetAttributes(
			attribute.StringSlice("http.response.header.x_request_id", []string{id}),
		)
		c.Header(RequestIDHeader, id)

		serve(c)

		failure := lastUnhandled(c)
		if failure == nil {
			write(c, "HTTP request handled", started, nil)
			return
		}
		// Answered here: the client gets a 500 whatever the handler had or had not written.
		if !c.Writer.Written() {
			c.String(http.StatusInternalServerError, "Internal Server Error")
		}
		trace.SpanFromContext(c.Request.Context()).AddEvent("exception", trace.WithAttributes(
			semconv.ExceptionTypeKey.String(errorType(failure)),
			semconv.ExceptionMessageKey.String(failure.Error()),
			semconv.ExceptionStacktraceKey.String(stackOf(failure)),
		))
		write(c, "HTTP request failed with an unhandled error", started, failure)
	}
}

// serve runs the handlers, a panic among them recorded as an unhandled error with its stack.
func serve(c *gin.Context) {
	defer func() {
		if value := recover(); value != nil {
			_ = c.Error(unhandled{err: panicked{value}, stack: string(debug.Stack())})
			c.Abort()
		}
	}()
	c.Next()
}

func lastUnhandled(c *gin.Context) error {
	for _, recorded := range slices.Backward(c.Errors) {
		if failure, ok := errors.AsType[unhandled](recorded.Err); ok {
			return failure
		}
	}
	return nil
}

func stackOf(err error) string {
	if failure, ok := errors.AsType[unhandled](err); ok {
		return failure.stack
	}
	return ""
}

func write(c *gin.Context, message string, started time.Time, failure error) {
	attrs := []slog.Attr{
		slog.String("method", c.Request.Method),
		slog.String("path", c.Request.URL.Path),
		// The template the metrics carry as http_route; none outside the API: a 404, the static files.
		slog.String("route", c.FullPath()),
		slog.String("query", c.Request.URL.RawQuery),
		slog.Int("status", c.Writer.Status()),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
	}
	if failure != nil {
		// Three fields rather than text: lines are filtered by the kind of failure.
		attrs = append(attrs,
			slog.String("error_type", errorType(failure)),
			slog.String("error_message", failure.Error()),
			slog.String("error_stack", stackOf(failure)),
		)
	}
	slog.LogAttrs(c.Request.Context(), severity(c.Writer.Status()), message, attrs...)
}

func severity(status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}
